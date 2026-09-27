package keine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"
)

// magic is written at the start of a file and again after the footer length.
const magic = "KEIN"

// formatVersion is the byte after the leading magic. A reader that sees another
// version refuses the file rather than misreading it, so a change to the layout
// on disk is a clean break instead of a silent corruption.
const formatVersion = 1

// Writer accumulates row groups into a keine file. Writes go to w directly:
// chunks are large sequential blocks, and buffering would only obscure which
// write failed.
type Writer struct {
	w         io.Writer
	schema    []ColumnSchema
	rowGroups []RowGroupMeta
	offset    int64
	writeErr  error

	opts Options

	// layouts is what Optimize chose per column, and stays nil until it is
	// called. A column the writer has not optimized is written the way opts and
	// its own type describe, so the two never disagree about who decided.
	layouts []LayoutResult

	// measure is one scratch buffer per column, for the compression measurements
	// Optimize makes. A column is the unit of parallelism below, so element i is
	// only ever touched by column i's goroutine and needs no synchronisation.
	measure []measureScratch

	// sem bounds the compressors one write has in flight, across every column
	// and every block of every column. It outlives a row group so the blocks of
	// one share the machine with the blocks of the next rather than each row
	// group sizing its own.
	sem chan struct{}
}

// Options is how a writer lays out the columns it has not been told to Optimize.
// Every field's zero value means something, so an empty Options is a valid
// configuration: it asks for the encoding each column's type implies and for no
// compression at all. NewWriter supplies Flate on top of that.
type Options struct {
	// Encoding is the encoding for a column the writer has not optimized. Zero
	// is not an encoding, and means the writer takes one from the column's type:
	// RLEBitpack for bool, Delta for integers, Plain for the rest. An encoding
	// that cannot serve a column's type fails the write rather than being
	// substituted.
	Encoding uint8

	// Compress is the codec every column is compressed with. CompressNone, the
	// zero value, stores columns exactly as their encoder produced them. Zstd is
	// not in this build and is rejected.
	Compress uint8

	// CompressLevel is the level of the codecs that take one. Zero means level 3,
	// and values outside the range a codec accepts are pulled back to the nearest
	// one that is. Optimize measures at this level, so what it picks is what the
	// file gets.
	CompressLevel int

	// BlockSize is the most encoded bytes in one independently compressed block.
	// Zero means 256KB, above DEFLATE's window and small enough that a wide
	// column still splits into several jobs.
	BlockSize int
}

// validate reports whether opts asks for a codec and an encoding this build can
// produce. Encoding and compression failures inside the write are silent
// otherwise: a block that cannot compress is stored empty, and the file is
// short rather than wrong in a way a reader notices.
func (o Options) validate() error {
	switch o.Compress {
	case CompressNone, CompressFlate, CompressGzip, CompressZlib, CompressLzw:
	default:
		return fmt.Errorf("keine: compression codec %d is not available in this build", o.Compress)
	}
	switch o.Encoding {
	case 0, EncPlain, EncRLEBitpack, EncDelta, EncDict, EncOffsetBytes, EncAffix:
	default:
		return fmt.Errorf("keine: unknown encoding %d", o.Encoding)
	}
	return nil
}

// NewWriter writes the leading magic and the format version, and lays columns
// out the way their type implies under Flate. NewWriterWithOptions changes
// either of those. If a write fails, the error is returned by the first
// AddRowGroup or Close call.
func NewWriter(w io.Writer, schema []ColumnSchema) *Writer {
	return NewWriterWithOptions(w, schema, Options{Compress: CompressFlate})
}

// NewWriterWithOptions is NewWriter with the layout the caller asks for. An
// option this build cannot serve is reported by the first AddRowGroup or Close
// call, like a failing write, and nothing is written to w for it.
func NewWriterWithOptions(w io.Writer, schema []ColumnSchema, opts Options) *Writer {
	wr := &Writer{w: w, schema: schema, opts: opts, sem: make(chan struct{}, runtime.NumCPU())}
	if err := opts.validate(); err != nil {
		wr.writeErr = err
		return wr
	}
	if _, err := w.Write([]byte(magic)); err != nil {
		wr.writeErr = err
		return wr
	}
	if _, err := w.Write([]byte{formatVersion}); err != nil {
		wr.writeErr = err
		return wr
	}
	wr.offset = int64(len(magic) + 1)
	return wr
}

// Optimize measures every candidate encoding and codec against every value of
// columns and keeps the winner for each, so the row groups written afterwards
// are stored the way the whole column earns rather than the way its type
// suggests. It does not guess at the shape of the data: it encodes and compresses
// every candidate over the column itself, at the level the file will be written
// at, so the layout it reports is the one the write produces.
//
// That thoroughness is the cost. A column of a million values pays for a full
// pass per candidate, where the layout a type implies costs nothing and is
// right more often than it is wrong. Call Optimize once for a dataset and every
// row group after it uses the layouts it chose.
func (w *Writer) Optimize(columns [][]any) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if err := w.checkColumns(columns); err != nil {
		return err
	}
	typed, _, _, err := w.collect(columns)
	if err != nil {
		return err
	}

	if len(w.measure) < len(columns) {
		w.measure = make([]measureScratch, len(columns))
	}
	w.layouts = make([]LayoutResult, len(columns))

	level := compressLevel(w.opts.CompressLevel)
	var wg sync.WaitGroup
	for i := range typed {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Measuring is encode and compress work, so it takes the same slot a
			// block's compression would, and the pass keeps the machine as full
			// as the write does.
			w.sem <- struct{}{}
			defer func() { <-w.sem }()

			results := benchmarkLayoutsTyped(typed[i], w.schema[i], &w.measure[i], level)
			// A column collect accepted has candidate encodings, so this is never
			// empty. An empty result leaves the zero layout, whose encoding no
			// encoder recognises, and the write then fails on it rather than
			// quietly storing the column some other way.
			if len(results) > 0 {
				w.layouts[i] = results[0]
			}
		}(i)
	}
	wg.Wait()
	return nil
}

// checkColumns reports whether columns has one slice per schema column and one
// length across all of them, which is everything the writer needs before it can
// read any value.
func (w *Writer) checkColumns(columns [][]any) error {
	if len(columns) != len(w.schema) {
		return fmt.Errorf("keine: expected %d columns, got %d", len(w.schema), len(columns))
	}
	numRows := uint32(0)
	if len(columns) > 0 {
		numRows = uint32(len(columns[0]))
	}
	for i, col := range columns {
		if uint32(len(col)) != numRows {
			return fmt.Errorf("keine: column %d has %d rows, expected %d", i, len(col), numRows)
		}
	}
	return nil
}

// collect reads the caller's columns once, dropping the nulls a nullable column
// allows and coercing each to the Go type its schema implies. Optimize and the
// write both consume the typed columns this hands back, so a column is measured
// exactly as it is stored. The statistics and the null bitmaps come with them,
// since a row group needs both and this is the one pass that has to look at
// every value.
func (w *Writer) collect(columns [][]any) ([]any, []ColMeta, [][]byte, error) {
	typed := make([]any, len(columns))
	metas := make([]ColMeta, len(columns))
	bitmasks := make([][]byte, len(columns))
	for i, col := range columns {
		schema := w.schema[i]

		var nulls []bool
		var dense []any
		if schema.Nullable {
			nulls = make([]bool, len(col))
			dense = make([]any, 0, len(col))
			for j, v := range col {
				if v == nil {
					nulls[j] = true
					continue
				}
				dense = append(dense, v)
			}
		} else {
			dense = col
		}

		meta := ColMeta{}
		for _, v := range nulls {
			if v {
				meta.NullCount++
			}
		}
		t, err := canonicalColumnTyped(dense, schema.Type)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("keine: collecting stats for column %d (%s): %w", i, schema.Name, err)
		}
		fillStatsTyped(&meta, t)

		typed[i] = t
		metas[i] = meta
		bitmasks[i] = EncodeBitmap(nulls)
	}
	return typed, metas, bitmasks, nil
}

// AddRowGroup encodes each column with the layout Optimize chose for it, or the
// one the writer's options imply when Optimize has not been called, writes the
// chunks sequentially, and records their metadata. Columns whose schema is
// nullable may hold nil values; those are recorded in a null bitmap and only the
// remaining values are encoded.
func (w *Writer) AddRowGroup(columns [][]any) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if err := w.checkColumns(columns); err != nil {
		return err
	}

	numRows := uint32(0)
	if len(columns) > 0 {
		numRows = uint32(len(columns[0]))
	}

	rg := RowGroupMeta{
		NumRows:    numRows,
		ByteOffset: w.offset,
		Columns:    make([]ColMeta, len(columns)),
	}

	typed, metas, bitmasks, err := w.collect(columns)
	if err != nil {
		return err
	}

	// Encoding and compressing are independent per column and are where the write
	// time goes, the compressor above all. Each column splits into blocks and
	// compresses them across cores, and a table wider than the machine has cores
	// still runs no more compressors at once than there are. A column's result is
	// written in order afterwards, which keeps the bytes identical to a serial
	// write: no encoder sees another column's data. The schema fixes the column
	// count for the writer's lifetime, so this grows once and later row groups
	// reuse the same buffers.
	if len(w.measure) < len(columns) {
		w.measure = make([]measureScratch, len(columns))
	}
	chunks, err := encodeColumns(typed, w.schema, w.layouts, w.opts, w.measure, w.sem)
	if err != nil {
		return err
	}

	for i := range columns {
		meta := metas[i]
		chunk := chunks[i]
		chunk.NullBitmap = bitmasks[i]

		cw := &countingWriter{w: w.w}
		if err := WriteChunk(cw, chunk); err != nil {
			return fmt.Errorf("keine: writing column %d (%s): %w", i, w.schema[i].Name, err)
		}
		w.offset += cw.count

		meta.ByteLength = cw.count
		meta.Encoding = chunk.Encoding
		meta.Compress = chunk.Compress
		metas[i] = meta
	}
	copy(rg.Columns, metas)

	w.rowGroups = append(w.rowGroups, rg)
	return nil
}

// encodeColumns encodes and compresses each column with the layout chosen for it,
// returning one chunk per column. Encoding one column touches no other, and
// neither does compressing one block of it, so both spread across cores.
// measure supplies one scratch buffer per column, whose element i belongs to
// column i alone.
//
// sem is the one the blocks share, so a column takes a slot per block it is
// compressing rather than for its whole run. Holding one while compressing
// serially is what made a table's widest column the write's critical path on its
// own, and a column waiting on its blocks holds none, so the semaphore cannot be
// exhausted by columns waiting for work it is withholding.
func encodeColumns(typed []any, schema []ColumnSchema, layouts []LayoutResult, opts Options, measure []measureScratch, sem chan struct{}) ([]ColumnChunk, error) {
	chunks := make([]ColumnChunk, len(typed))
	errs := make([]error, len(typed))
	var wg sync.WaitGroup
	for i := range typed {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			enc, codec := chosenLayout(i, schema[i], layouts, opts)
			encoded, err := encodeWith(enc, typed[i], schema[i].Type)
			if err != nil {
				errs[i] = err
				return
			}
			chunks[i] = ColumnChunk{
				Encoding:  enc,
				Compress:  codec,
				RawLength: uint32(len(encoded)),
				Blocks:    splitBlocks(encoded, codec, opts.CompressLevel, blockOfSize(opts.BlockSize), sem),
			}
		}(i)
	}
	wg.Wait()

	// The columns run concurrently but the caller hears about one failure, the
	// first column that could not be encoded, rather than whichever goroutine
	// happened to report first.
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("keine: encoding column %d (%s): %w", i, schema[i].Name, err)
		}
	}
	return chunks, nil
}

// chosenLayout is the layout column i is written with. Optimize's choice comes
// first, since it has measured the column; otherwise an encoding the caller set
// overrides the one the type implies, while the codec is the caller's either way.
func chosenLayout(i int, schema ColumnSchema, layouts []LayoutResult, opts Options) (uint8, uint8) {
	if layouts != nil {
		return layouts[i].Encoding, layouts[i].Compress
	}
	if opts.Encoding == 0 {
		enc, _ := defaultLayout(schema.Type)
		return enc, opts.Compress
	}
	return opts.Encoding, opts.Compress
}

// blockOfSize is the block size a writer uses, where zero and anything below a
// block means the default. A block size of one is honoured by the arithmetic but
// costs eight bytes of header per byte of data, so a caller asking for it gets
// the file they described.
func blockOfSize(size int) int {
	if size <= 0 {
		return blockSize
	}
	return size
}

// countingWriter tallies the bytes written through it.
type countingWriter struct {
	w     io.Writer
	count int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.count += int64(n)
	return n, err
}

// fillStatsTyped folds the byte form of every value into meta. Fixed width
// values are encoded into an eight byte scratch, so no value allocates. typed
// is a canonical column, so every case it can be is one of the thirteen below
// and no default is needed.
func fillStatsTyped(meta *ColMeta, typed any) {
	switch s := typed.(type) {
	case []bool:
		var b [1]byte
		for _, v := range s {
			if v {
				b[0] = 1
			} else {
				b[0] = 0
			}
			statBytes(meta, b[:])
		}
	case []int8:
		var b [1]byte
		for _, v := range s {
			b[0] = byte(v)
			statBytes(meta, b[:])
		}
	case []int16:
		var b [2]byte
		for _, v := range s {
			binary.LittleEndian.PutUint16(b[:], uint16(v))
			statBytes(meta, b[:])
		}
	case []int32:
		var b [4]byte
		for _, v := range s {
			binary.LittleEndian.PutUint32(b[:], uint32(v))
			statBytes(meta, b[:])
		}
	case []int64:
		var b [8]byte
		for _, v := range s {
			binary.LittleEndian.PutUint64(b[:], uint64(v))
			statBytes(meta, b[:])
		}
	case []uint8:
		var b [1]byte
		for _, v := range s {
			b[0] = v
			statBytes(meta, b[:])
		}
	case []uint16:
		var b [2]byte
		for _, v := range s {
			binary.LittleEndian.PutUint16(b[:], v)
			statBytes(meta, b[:])
		}
	case []uint32:
		var b [4]byte
		for _, v := range s {
			binary.LittleEndian.PutUint32(b[:], v)
			statBytes(meta, b[:])
		}
	case []uint64:
		var b [8]byte
		for _, v := range s {
			binary.LittleEndian.PutUint64(b[:], v)
			statBytes(meta, b[:])
		}
	case []float32:
		var b [4]byte
		for _, v := range s {
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
			statBytes(meta, b[:])
		}
	case []float64:
		var b [8]byte
		for _, v := range s {
			binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
			statBytes(meta, b[:])
		}
	case []string:
		var lo, hi string
		for i, v := range s {
			if i == 0 {
				lo, hi = v, v
				meta.MinLen = uint32(len(v))
				meta.MaxLen = uint32(len(v))
				continue
			}
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
			if uint32(len(v)) < meta.MinLen {
				meta.MinLen = uint32(len(v))
			}
			if uint32(len(v)) > meta.MaxLen {
				meta.MaxLen = uint32(len(v))
			}
		}
		meta.MinVal = append(meta.MinVal, lo...)
		meta.MaxVal = append(meta.MaxVal, hi...)
	case [][]byte:
		for _, v := range s {
			statBytes(meta, v)
		}
	}
}

// statBytes folds b into meta's min and max. MinLen and MaxLen track value
// length separately from min and max, since a column's longest value is not
// necessarily its largest.
func statBytes(meta *ColMeta, b []byte) {
	if len(meta.MinVal) == 0 {
		meta.MinVal = append(meta.MinVal, b...)
		meta.MaxVal = append(meta.MaxVal, b...)
		meta.MinLen = uint32(len(b))
		meta.MaxLen = uint32(len(b))
		return
	}
	if bytes.Compare(b, meta.MinVal) < 0 {
		meta.MinVal = append(meta.MinVal[:0], b...)
	}
	if bytes.Compare(b, meta.MaxVal) > 0 {
		meta.MaxVal = append(meta.MaxVal[:0], b...)
	}
	if uint32(len(b)) < meta.MinLen {
		meta.MinLen = uint32(len(b))
	}
	if uint32(len(b)) > meta.MaxLen {
		meta.MaxLen = uint32(len(b))
	}
}

// Close writes the footer, its length as a little-endian uint32, and the
// trailing magic. It must be called once after all row groups.
func (w *Writer) Close() error {
	if w.writeErr != nil {
		return w.writeErr
	}

	footerBytes := encodeFooter(Footer{Schema: w.schema, RowGroups: w.rowGroups})
	if _, err := w.w.Write(footerBytes); err != nil {
		return err
	}
	w.offset += int64(len(footerBytes))

	if err := binary.Write(w.w, binary.LittleEndian, uint32(len(footerBytes))); err != nil {
		return err
	}
	if _, err := w.w.Write([]byte(magic)); err != nil {
		return err
	}
	return nil
}
