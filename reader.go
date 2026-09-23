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
	if index < 0 || index >= len(rd.footer.RowGroups) {
		return nil, fmt.Errorf("keine: row group %d out of range (have %d)", index, len(rd.footer.RowGroups))
	}
	rg := rd.footer.RowGroups[index]

	starts := make([]int64, len(rg.Columns))
	offset := rg.ByteOffset
	for i, col := range rg.Columns {
		starts[i] = offset
		offset += col.ByteLength
	}

	out := make([][]any, len(colIndexes))
	for k, ci := range colIndexes {
		if ci < 0 || ci >= len(rg.Columns) {
			return nil, fmt.Errorf("keine: column index %d out of range (have %d)", ci, len(rg.Columns))
		}
		meta := rg.Columns[ci]
		schema := rd.footer.Schema[ci]

		if _, err := rd.r.Seek(starts[ci], io.SeekStart); err != nil {
			return nil, fmt.Errorf("keine: cannot seek to column %d: %w", ci, err)
		}
		chunk, err := ReadChunk(bufio.NewReader(rd.r))
		if err != nil {
			return nil, fmt.Errorf("keine: reading column %d (%s): %w", ci, schema.Name, err)
		}

		data, err := Decompress(chunk.Data, chunk.Compress)
		if err != nil {
			return nil, fmt.Errorf("keine: decompressing column %d (%s): %w", ci, schema.Name, err)
		}

		// Only the non-null values were encoded.
		numValues := int(rg.NumRows)
		if len(chunk.NullBitmap) > 0 {
			numValues -= int(meta.NullCount)
		}
		vals, err := decodeWith(chunk.Encoding, data, numValues, schema.Type)
		if err != nil {
			return nil, fmt.Errorf("keine: decoding column %d (%s): %w", ci, schema.Name, err)
		}

		if len(chunk.NullBitmap) > 0 {
			nulls := DecodeBitmap(chunk.NullBitmap, int(rg.NumRows))
			expanded := make([]any, rg.NumRows)
			j := 0
			for i := 0; i < int(rg.NumRows); i++ {
				if nulls[i] {
					expanded[i] = nil
					continue
				}
				expanded[i] = vals[j]
				j++
			}
			vals = expanded
		}

		out[k] = vals
	}

	return out, nil
}

func readUint32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}
