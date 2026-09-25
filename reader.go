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
	// rawbufs holds one decompression buffer per column of that row group, and raw
	// the decompressed bytes of the column the typed path decodes. Nothing a
	// caller gets back points into any of them once decoding finishes, so all
	// three are kept for the next read instead of being collected. A Reader holds
	// the position of its ReadSeeker and is not safe to use from multiple
	// goroutines, so they need no synchronisation; each is indexed by the job that
	// owns it, and no two jobs share an index.
	rgbuf    []byte
	raw      []byte
	chunkbuf []byte
	rawbufs  [][]byte

	// dests holds one reusable decode destination per column of the row group the
	// scoped path is reading. Nothing that path hands a caller points anywhere but
	// into its own column's destination, and it hands them out only for the
	// duration of its callback, so they can be reused rather than collected. The
	// other read paths hand out values the caller keeps, so they decode into a
	// fresh destination every time.
	dests []*dest
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

	lead := make([]byte, len(magic)+1)
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("keine: cannot seek to start of file: %w", err)
	}
	if _, err := io.ReadFull(r, lead); err != nil {
		return nil, fmt.Errorf("keine: cannot read the leading magic: %w", err)
	}
	if string(lead[:len(magic)]) != magic {
		return nil, fmt.Errorf("keine: leading magic %q is not %q", lead[:len(magic)], magic)
	}
	if version := lead[len(magic)]; version != formatVersion {
		return nil, fmt.Errorf("keine: file is format version %d, this build reads version %d", version, formatVersion)
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

	jobs, err := rd.readJobs(rg, colIndexes)
	if err != nil {
		return nil, err
	}

	// The caller keeps what it is given, so every column decodes into a
	// destination of its own rather than into one the next read reuses.
	dests := make([]*dest, len(jobs))
	for i := range dests {
		dests[i] = &dest{}
	}
	typed, errs := rd.decodeJobs(jobs, dests)
	for k, t := range typed {
		if errs[k] != nil {
			return nil, errs[k]
		}
		j := jobs[k]
		sch := rd.footer.Schema[j.ci]
		vals, err := boxColumn(t, j.chunk.Encoding, sch.Type)
		if err != nil {
			return nil, err
		}
		if len(j.chunk.NullBitmap) > 0 {
			vals = expandNulls(vals, DecodeBitmap(j.chunk.NullBitmap, int(j.rg.NumRows)), int(j.rg.NumRows))
		}
		out[k] = vals
	}

	return out, nil
}

// ReadRowGroupScoped reads the requested columns of one row group and calls fn
// with a view of them. The view hands back typed slices rather than []any, so no
// value is boxed in an interface, but the slices point into buffers this Reader
// owns and hands to its next read: fn must be finished with them before it
// returns, because holding one past the call reads whatever the next read wrote
// over it. The columns are decoded in parallel, same as ReadRowGroup, and only
// the ones asked for.
//
// A nullable column has nowhere to put a nil in a typed slice, so it is an error
// here and ReadRowGroup reads those.
func (rd *Reader) ReadRowGroupScoped(index int, cols []int, fn func(*Columns) error) error {
	rg, err := rd.RowGroupMeta(index)
	if err != nil {
		return err
	}
	for _, ci := range cols {
		if ci < 0 || ci >= len(rg.Columns) {
			return fmt.Errorf("keine: column index %d out of range (have %d)", ci, len(rg.Columns))
		}
		if rg.Columns[ci].NullCount > 0 {
			return fmt.Errorf("keine: column %d (%s) has %d null values; the scoped path returns typed slices, which have nowhere for a nil, so ReadRowGroup reads those",
				ci, rd.footer.Schema[ci].Name, rg.Columns[ci].NullCount)
		}
	}

	jobs, err := rd.readJobs(rg, cols)
	if err != nil {
		return err
	}

	for len(rd.dests) < len(jobs) {
		rd.dests = append(rd.dests, &dest{})
	}
	typed, errs := rd.decodeJobs(jobs, rd.dests[:len(jobs)])
	return fn(&Columns{rd: rd, rg: rg, cols: cols, typed: typed, errs: errs})
}

// Columns is the view of the row group one ReadRowGroupScoped call read. Its
// slices are valid until that call's fn returns.
type Columns struct {
	rd    *Reader
	rg    RowGroupMeta
	cols  []int
	typed []any
	errs  []error
}

// Column is one of the columns ReadRowGroupScoped was asked for, as a slice of
// the column's Go type: int64 for TypeInt64, string for TypeString. i is the
// position in the set that was requested, not the position in the schema. T must
// match the type the column decodes to; asking for another is an error rather
// than a silent mismatch.
//
// The one case that still allocates is a narrow integer column encoded with
// Delta, whose differences decode as int64 no matter the declared width. It is
// narrowed to the declared type, which is one slice the caller owns.
func Column[T any](c *Columns, i int) ([]T, error) {
	if i < 0 || i >= len(c.typed) {
		return nil, fmt.Errorf("keine: column %d out of range (requested %d)", i, len(c.typed))
	}
	if c.errs[i] != nil {
		return nil, c.errs[i]
	}

	t := c.typed[i]
	if !encProducesType(c.rg.Columns[c.cols[i]].Encoding, c.rd.footer.Schema[c.cols[i]].Type) {
		vals, err := canonicalColumn(boxValues(t), c.rd.footer.Schema[c.cols[i]].Type)
		if err != nil {
			return nil, err
		}
		out := make([]T, len(vals))
		for k, v := range vals {
			tv, ok := v.(T)
			if !ok {
				return nil, fmt.Errorf("keine: column %d (%s) narrows to %T, not %T",
					i, c.rd.footer.Schema[c.cols[i]].Name, v, *new(T))
			}
			out[k] = tv
		}
		return out, nil
	}

	out, ok := t.([]T)
	if !ok {
		return nil, fmt.Errorf("keine: column %d (%s) decodes to %T, not %T",
			i, c.rd.footer.Schema[c.cols[i]].Name, t, *new(T))
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

// readJobs reads the chunks of the requested columns into one buffer, each in
// its own region, and parses them. One buffer holds every requested chunk so a
// chunk handed to a decode goroutine needs no copy: nothing a later column reads
// overlaps it. The buffer stays with the Reader for the next row group.
func (rd *Reader) readJobs(rg RowGroupMeta, colIndexes []int) ([]readJob, error) {
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
	for len(rd.rawbufs) < len(jobs) {
		rd.rawbufs = append(rd.rawbufs, nil)
	}
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
	return jobs, nil
}

// decodeJobs decompresses and decodes each job's column into a slice of its
// declared Go type, and leaves boxing and null expansion to the caller, so a
// caller that wants typed slices does not pay for interfaces. A column's blocks
// decompress into one buffer the size of the column, each into its own region of
// it, so reassembly is placement rather than a copy; decoding waits for them and
// then runs in parallel too. Each buffer is the Reader's own and indexed by job,
// so it survives a garbage collection and keeps the widest column it has seen:
// after the first row group the decompression allocates nothing. A sync.Pool
// would have been cleared at the next collection, and this read path allocates
// enough per call that the pool missed more often than it hit.
//
// dests supplies each job's decode destination. A caller that keeps the values it
// was given passes a fresh one per job, since every value a decoder returns
// points into its destination; the scoped path hands the same destinations back
// every read, which is what makes its second and later reads allocate nothing.
//
// The semaphore is taken around a goroutine's own work and released before it
// waits on anything, so a column waiting for its blocks never occupies a slot
// another block needs.
func (rd *Reader) decodeJobs(jobs []readJob, dests []*dest) ([]any, []error) {
	schema := rd.footer.Schema
	typed := make([]any, len(jobs))
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j := jobs[i]
			sch := schema[j.ci]
			meta := j.rg.Columns[j.ci]

			raw := sizedBuffer(j.chunk.RawLength, rd.rawbufs[i])[:j.chunk.RawLength]
			blockErrs := make([]error, len(j.chunk.Blocks))

			var blockWg sync.WaitGroup
			off := uint32(0)
			for k, blk := range j.chunk.Blocks {
				k, blk, at := k, blk, off
				off += blk.RawLength
				blockWg.Add(1)
				go func() {
					defer blockWg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					if err := decompressBlock(raw, at, blk, j.chunk.Compress); err != nil {
						blockErrs[k] = err
					}
				}()
			}
			blockWg.Wait()

			for k, err := range blockErrs {
				if err != nil {
					errs[i] = fmt.Errorf("keine: decompressing column %d (%s), block %d: %w", j.ci, sch.Name, k, err)
					rd.rawbufs[i] = raw[:0]
					return
				}
			}

			// Only the non-null values were encoded.
			numValues := int(j.rg.NumRows)
			if len(j.chunk.NullBitmap) > 0 {
				numValues -= int(meta.NullCount)
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			t, err := decodeTyped(j.chunk.Encoding, raw, numValues, sch.Type, dests[i])
			if err != nil {
				errs[i] = fmt.Errorf("keine: decoding column %d (%s): %w", j.ci, sch.Name, err)
				rd.rawbufs[i] = raw[:0]
				return
			}
			rd.rawbufs[i] = raw[:0]
			typed[i] = t
		}(i)
	}
	wg.Wait()
	return typed, errs
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

	raw, err := readBlocks(sizedBuffer(chunk.RawLength, rd.raw), chunk)
	if err != nil {
		return nil, nil, fmt.Errorf("keine: decompressing column %d (%s): %w", ci, rd.footer.Schema[ci].Name, err)
	}
	rd.raw = raw

	// Only the non-null values were encoded.
	numValues := int(rg.NumRows)
	if len(chunk.NullBitmap) > 0 {
		numValues -= int(rg.Columns[ci].NullCount)
	}
	typed, err = decodeTyped(chunk.Encoding, raw, numValues, rd.footer.Schema[ci].Type, &dest{})
	if err != nil {
		return nil, nil, fmt.Errorf("keine: decoding column %d (%s): %w", ci, rd.footer.Schema[ci].Name, err)
	}

	if len(chunk.NullBitmap) > 0 {
		nulls = DecodeBitmap(chunk.NullBitmap, int(rg.NumRows))
	}
	return typed, nulls, nil
}

// readBlocks reassembles chunk's encoded stream into buf, which must be at least
// RawLength bytes wide. This is the single column path, so the blocks are done in
// order: one column has nothing to run them alongside.
func readBlocks(buf []byte, chunk ColumnChunk) ([]byte, error) {
	raw := buf[:chunk.RawLength]
	off := uint32(0)
	for _, blk := range chunk.Blocks {
		if err := decompressBlock(raw, off, blk, chunk.Compress); err != nil {
			return nil, err
		}
		off += blk.RawLength
	}
	return raw, nil
}

// decompressBlock decompresses blk into its region of raw, which is the bytes at
// off for blk.RawLength. A block whose contents do not land where its header
// says is an error rather than a silent shortening of the column: the region
// stays zeroed and the length check catches the result that grew elsewhere.
func decompressBlock(raw []byte, off uint32, blk ColumnBlock, codec uint8) error {
	if uint64(off)+uint64(blk.RawLength) > uint64(len(raw)) {
		return fmt.Errorf("block wants %d bytes at offset %d, the column holds %d", blk.RawLength, off, len(raw))
	}
	out, err := decompressInto(raw[off:off:off+blk.RawLength], blk.Data, codec)
	if err != nil {
		return err
	}
	if uint32(len(out)) != blk.RawLength {
		return fmt.Errorf("decompressed to %d bytes, header says %d", len(out), blk.RawLength)
	}
	return nil
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
