package keine

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// Reader reads a keine file written by Writer.
type Reader struct {
	r      io.ReadSeeker
	footer Footer
}

// NewReader reads the footer of a file written by Writer. The file must end
// with the footer, its length as a little-endian uint32, and the magic.
func NewReader(r io.ReadSeeker) (*Reader, error) {
	rd := &Reader{r: r}

	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("keine: cannot seek to end of file: %w", err)
	}
	if end < 8 {
		return nil, fmt.Errorf("keine: file is %d bytes, too small to contain a footer", end)
	}
	if _, err := r.Seek(end-8, io.SeekStart); err != nil {
		return nil, fmt.Errorf("keine: cannot seek to footer: %w", err)
	}

	footerLen, err := readUint32(r)
	if err != nil {
		return nil, fmt.Errorf("keine: cannot read footer length: %w", err)
	}
	trailer := make([]byte, 4)
	if _, err := io.ReadFull(r, trailer); err != nil {
		return nil, fmt.Errorf("keine: cannot read trailing magic: %w", err)
	}
	if string(trailer) != magic {
		return nil, fmt.Errorf("keine: trailing magic %q is not %q", trailer, magic)
	}

	if int64(footerLen) > end-8 {
		return nil, fmt.Errorf("keine: footer length %d exceeds file size", footerLen)
	}
	if _, err := r.Seek(end-8-int64(footerLen), io.SeekStart); err != nil {
		return nil, fmt.Errorf("keine: cannot seek to footer: %w", err)
	}
	footerBytes := make([]byte, footerLen)
	if _, err := io.ReadFull(r, footerBytes); err != nil {
		return nil, fmt.Errorf("keine: cannot read footer: %w", err)
	}

	footer, err := decodeFooter(footerBytes)
	if err != nil {
		return nil, fmt.Errorf("keine: cannot decode footer: %w", err)
	}
	rd.footer = footer
	return rd, nil
}

// Schema returns the schema the file was written with.
func (rd *Reader) Schema() []ColumnSchema {
	return rd.footer.Schema
}

// RowGroupCount returns the number of row groups in the file.
func (rd *Reader) RowGroupCount() int {
	return len(rd.footer.RowGroups)
}

// RowGroupMeta returns metadata for one row group: per-column byte length, null
// count, value stats and the encoding and codec each chunk was written with. It
// reads only the footer, so it is cheap to call before deciding which columns
// to read.
func (rd *Reader) RowGroupMeta(index int) (RowGroupMeta, error) {
	if index < 0 || index >= len(rd.footer.RowGroups) {
		return RowGroupMeta{}, fmt.Errorf("keine: row group %d out of range (have %d)", index, len(rd.footer.RowGroups))
	}
	return rd.footer.RowGroups[index], nil
}

// ReadRowGroup reads the requested columns of one row group. Columns not in
// colIndexes are skipped using their recorded on-disk length. Values come back
// typed according to the schema; rows that were null are nil.
func (rd *Reader) ReadRowGroup(index int, colIndexes []int) ([][]any, error) {
	rg, err := rd.RowGroupMeta(index)
	if err != nil {
		return nil, err
	}

	out := make([][]any, len(colIndexes))
	for k, ci := range colIndexes {
		if ci < 0 || ci >= len(rg.Columns) {
			return nil, fmt.Errorf("keine: column index %d out of range (have %d)", ci, len(rg.Columns))
		}

		typed, nulls, err := rd.readColumn(startOf(rg, ci), rg, ci)
		if err != nil {
			return nil, err
		}

		vals, err := boxColumn(typed, rg.Columns[ci].Encoding, rd.footer.Schema[ci].Type)
		if err != nil {
			return nil, err
		}
		out[k] = expandNulls(vals, nulls, int(rg.NumRows))
	}

	return out, nil
}

// ReadColumn reads one column of one row group as a typed slice, so a TypeInt64
// column comes back as []int64 and a TypeString column as []string rather than
// []any, and no value is boxed in an interface. T must be the Go type the column
// decodes to. The column must hold no nulls; ReadRowGroup covers those, since a
// nil marker has nowhere to go in a []T of values.
func ReadColumn[T any](rd *Reader, index, col int) ([]T, error) {
	rg, err := rd.RowGroupMeta(index)
	if err != nil {
		return nil, err
	}
	if col < 0 || col >= len(rg.Columns) {
		return nil, fmt.Errorf("keine: column index %d out of range (have %d)", col, len(rg.Columns))
	}
	if rg.Columns[col].NullCount > 0 {
		return nil, fmt.Errorf("keine: column %d (%s) has %d null values; ReadRowGroup reads those",
			col, rd.footer.Schema[col].Name, rg.Columns[col].NullCount)
	}

	typed, _, err := rd.readColumn(startOf(rg, col), rg, col)
	if err != nil {
		return nil, err
	}

	enc := rg.Columns[col].Encoding
	typ := rd.footer.Schema[col].Type
	if encProducesType(enc, typ) {
		out, ok := asValues[T](typed)
		if !ok {
			return nil, fmt.Errorf("keine: column %d (%s) decodes to %T, not %T",
				col, rd.footer.Schema[col].Name, typed, *new(T))
		}
		return out, nil
	}

	// The encoding produces a wider type than the column declares — Delta keeps
	// int64 diffs for every integer width — so narrow to the declared type
	// before converting.
	vals, err := canonicalColumn(boxValues(typed), typ)
	if err != nil {
		return nil, err
	}
	out := make([]T, len(vals))
	for i, v := range vals {
		t, ok := v.(T)
		if !ok {
			return nil, fmt.Errorf("keine: column %d (%s) narrows to %T, not %T",
				col, rd.footer.Schema[col].Name, v, *new(T))
		}
		out[i] = t
	}
	return out, nil
}

// startOf is the byte offset of column col within its row group.
func startOf(rg RowGroupMeta, col int) int64 {
	offset := rg.ByteOffset
	for _, c := range rg.Columns[:col] {
		offset += c.ByteLength
	}
	return offset
}

// readColumn seeks to one column chunk, reads and decompresses it, and decodes
// it into a slice of its declared Go type. nulls is the column's null bitmap,
// nil when the column was written dense.
func (rd *Reader) readColumn(start int64, rg RowGroupMeta, ci int) (typed any, nulls []bool, err error) {
	schema := rd.footer.Schema[ci]
	meta := rg.Columns[ci]

	if _, err := rd.r.Seek(start, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("keine: cannot seek to column %d: %w", ci, err)
	}
	chunk, err := ReadChunk(bufio.NewReader(rd.r))
	if err != nil {
		return nil, nil, fmt.Errorf("keine: reading column %d (%s): %w", ci, schema.Name, err)
	}

	data, err := Decompress(chunk.Data, chunk.Compress)
	if err != nil {
		return nil, nil, fmt.Errorf("keine: decompressing column %d (%s): %w", ci, schema.Name, err)
	}

	// Only the non-null values were encoded.
	numValues := int(rg.NumRows)
	if len(chunk.NullBitmap) > 0 {
		numValues -= int(meta.NullCount)
	}
	typed, err = decodeTyped(chunk.Encoding, data, numValues, schema.Type)
	if err != nil {
		return nil, nil, fmt.Errorf("keine: decoding column %d (%s): %w", ci, schema.Name, err)
	}

	if len(chunk.NullBitmap) > 0 {
		nulls = DecodeBitmap(chunk.NullBitmap, int(rg.NumRows))
	}
	return typed, nulls, nil
}

// boxColumn converts a decoded column to []any, narrowing it to the declared
// type when the encoding does not already produce it.
func boxColumn(typed any, enc, typ uint8) ([]any, error) {
	if !encProducesType(enc, typ) {
		return canonicalColumn(boxValues(typed), typ)
	}
	return boxValues(typed), nil
}

// expandNulls re-inserts nil at every null position. Only the non-null values
// were encoded, so vals is dense and nulls says where to spread it.
func expandNulls(vals []any, nulls []bool, numRows int) []any {
	if len(nulls) == 0 {
		return vals
	}
	expanded := make([]any, numRows)
	j := 0
	for i := 0; i < numRows; i++ {
		if nulls[i] {
			continue
		}
		expanded[i] = vals[j]
		j++
	}
	return expanded
}

func readUint32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}
