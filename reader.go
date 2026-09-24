package keine

import (
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"sync"
)

// Reader reads a keine file written by Writer.
type Reader struct {
	r      io.ReadSeeker
	footer Footer

	// rgbuf holds the chunks of the row group being read, one region per column.
	// raw is the decompressed bytes of the column being decoded on the typed
	// path. Nothing a caller gets back points into either once decoding finishes,
	// so both are kept for the next read instead of being collected. A Reader
	// holds the position of its ReadSeeker and is not safe to use from multiple
	// goroutines, so they need no synchronisation.
	rgbuf    []byte
	raw      []byte
	chunkbuf []byte
	buffers  sync.Pool
}

// NewReader reads the footer of a file written by Writer. The file must end
// with the footer, its length as a little-endian uint32, and the magic.
func NewReader(r io.ReadSeeker) (*Reader, error) {
	rd := &Reader{r: r}
	rd.buffers.New = func() any { return []byte(nil) }

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
	for _, ci := range colIndexes {
		if ci < 0 || ci >= len(rg.Columns) {
			return nil, fmt.Errorf("keine: column index %d out of range (have %d)", ci, len(rg.Columns))
		}
	}

	// One buffer holds every requested chunk, each in its own region, so a chunk
	// handed to a decode goroutine needs no copy: nothing a later column reads
	// overlaps it. The buffer stays with the Reader for the next row group.
	total := int64(0)
	for _, ci := range colIndexes {
		total += rg.Columns[ci].ByteLength
	}
	if int64(cap(rd.rgbuf)) < total {
		rd.rgbuf = make([]byte, total)
	} else {
		rd.rgbuf = rd.rgbuf[:total]
	}

	jobs := make([]readJob, len(colIndexes))
	off := 0
	for k, ci := range colIndexes {
		region := rd.rgbuf[off : off+int(rg.Columns[ci].ByteLength)]
		chunk, err := rd.readChunk(startOf(rg, ci), region)
		if err != nil {
			return nil, err
		}
		jobs[k] = readJob{chunk: chunk, rg: rg, ci: ci}
		off += int(rg.Columns[ci].ByteLength)
	}

	results := make([]readResult, len(jobs))
	rd.decodeColumns(jobs, results)
	for k, res := range results {
		if res.err != nil {
			return nil, res.err
		}
		out[k] = res.vals
	}

	return out, nil
}

// readJob is one column's chunk once it is in memory, with everything its
// decoder needs from the footer.
type readJob struct {
	chunk ColumnChunk
	rg    RowGroupMeta
	ci    int
}

// readResult is where a readJob's decoded values land.
type readResult struct {
	vals []any
	err  error
}

// decodeColumns decompresses and decodes each job outside the results slice,
// which lets the caller keep reading chunks while the previous ones are still
// being worked on. The decompressed bytes are the largest thing a read
// allocates, so every goroutine returns its buffer to the pool it took it from.
// The pool lives on the Reader, so it keeps one buffer per column across row
// groups and grows to the widest one; after the first row group the decompress
// allocates nothing.
func (rd *Reader) decodeColumns(jobs []readJob, results []readResult) {
	schema := rd.footer.Schema
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			j := jobs[i]
			sch := schema[j.ci]
			meta := j.rg.Columns[j.ci]

			buf := sizedBuffer(j.chunk.RawLength, rd.buffers.Get().([]byte))
			raw, err := decompressInto(buf[:0], j.chunk.Data, j.chunk.Compress)
			if err != nil {
				results[i].err = fmt.Errorf("keine: decompressing column %d (%s): %w", j.ci, sch.Name, err)
				return
			}

			// Only the non-null values were encoded.
			numValues := int(j.rg.NumRows)
			if len(j.chunk.NullBitmap) > 0 {
				numValues -= int(meta.NullCount)
			}
			typed, err := decodeTyped(j.chunk.Encoding, raw, numValues, sch.Type)
			if err != nil {
				results[i].err = fmt.Errorf("keine: decoding column %d (%s): %w", j.ci, sch.Name, err)
				return
			}

			vals, err := boxColumn(typed, j.chunk.Encoding, sch.Type)
			if err != nil {
				results[i].err = err
				return
			}
			if len(j.chunk.NullBitmap) > 0 {
				vals = expandNulls(vals, DecodeBitmap(j.chunk.NullBitmap, int(j.rg.NumRows)), int(j.rg.NumRows))
			}
			// Nothing the caller gets back points into raw: strings and byte
			// slices are copied out of it as they are decoded. Put it back
			// afterwards rather than before, since decoding reads it.
			rd.buffers.Put(raw)
			results[i].vals = vals
		}(i)
	}
	wg.Wait()
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

	// The encoding produces a wider type than the column declares: Delta keeps
	// int64 diffs for every integer width. Narrow to the declared type before
	// converting.
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

// sizedBuffer returns an empty buffer of at least want bytes, reusing have when
// it is already that wide. A chunk carries its decompressed length, so the
// buffer a decode uses is allocated once instead of grown a piece at a time.
func sizedBuffer(want uint32, have []byte) []byte {
	if int(want) <= cap(have) {
		return have[:0]
	}
	return make([]byte, 0, want)
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
// nil when the column was written dense. This is the single column path, so its
// chunk buffer is the Reader's own and the decode that follows is serial.
func (rd *Reader) readColumn(start int64, rg RowGroupMeta, ci int) (typed any, nulls []bool, err error) {
	size := rg.Columns[ci].ByteLength
	if int64(cap(rd.chunkbuf)) < size {
		rd.chunkbuf = make([]byte, size)
	} else {
		rd.chunkbuf = rd.chunkbuf[:size]
	}
	chunk, err := rd.readChunk(start, rd.chunkbuf)
	if err != nil {
		return nil, nil, fmt.Errorf("keine: reading column %d (%s): %w", ci, rd.footer.Schema[ci].Name, err)
	}

	raw, err := decompressInto(sizedBuffer(chunk.RawLength, rd.raw), chunk.Data, chunk.Compress)
	if err != nil {
		return nil, nil, fmt.Errorf("keine: decompressing column %d (%s): %w", ci, rd.footer.Schema[ci].Name, err)
	}
	rd.raw = raw

	// Only the non-null values were encoded.
	numValues := int(rg.NumRows)
	if len(chunk.NullBitmap) > 0 {
		numValues -= int(rg.Columns[ci].NullCount)
	}
	typed, err = decodeTyped(chunk.Encoding, raw, numValues, rd.footer.Schema[ci].Type)
	if err != nil {
		return nil, nil, fmt.Errorf("keine: decoding column %d (%s): %w", ci, rd.footer.Schema[ci].Name, err)
	}

	if len(chunk.NullBitmap) > 0 {
		nulls = DecodeBitmap(chunk.NullBitmap, int(rg.NumRows))
	}
	return typed, nulls, nil
}

// readChunk seeks to one column chunk, reads its recorded byte length into
// region, and parses it. Reading the bytes and parsing them are split like this
// because every column reads into its own region of one buffer, which is what
// keeps a chunk alive while another goroutine decodes it.
func (rd *Reader) readChunk(start int64, region []byte) (ColumnChunk, error) {
	if _, err := rd.r.Seek(start, io.SeekStart); err != nil {
		return ColumnChunk{}, fmt.Errorf("keine: cannot seek to column: %w", err)
	}
	if _, err := io.ReadFull(rd.r, region); err != nil {
		return ColumnChunk{}, fmt.Errorf("keine: cannot read column bytes: %w", err)
	}
	var chunk ColumnChunk
	if err := parseChunk(region, &chunk); err != nil {
		return ColumnChunk{}, fmt.Errorf("keine: %w", err)
	}
	return chunk, nil
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
// were encoded, so vals is dense and nulls says where to spread it. nulls is the
// decoded bitmap, so it has a slot for every row in the group.
func expandNulls(vals []any, nulls []bool, numRows int) []any {
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
