package keine

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime"
	"sync"

	"github.com/everymemory/keine/tokenizer"
)

// magic is written at the start of a file and again after the footer length.
const magic = "KEIN"

// The byte after the leading magic names the layout of everything that follows
// it. A reader that sees another version refuses the file, so a change to the
// layout on disk is a clean break rather than a silent corruption.
const (
	// formatVersion is the file every release before tokenized columns wrote: the
	// magic, the version, then the chunks. A writer with no tokenizer for any
	// column still writes it, so a file with no tokenized column is byte for byte
	// the file it always was.
	formatVersion = 1

	// tokenizedVersion is formatVersion with a tokenizer table between the version
	// and the chunks: a uint32 count and that many 32 byte SHA-256 digests. A
	// column names one by its one-based position in the table, and a reader
	// resolves the digest to the tokenizer the file was written with. The table is
	// written only when a column is tokenized, so a reader of either version sees
	// the chunks where it expects them.
	tokenizedVersion = 2
)

// digestLen is the size of the SHA-256 a tokenizer is named by.
const digestLen = 32

// Writer accumulates row groups into a keine file. Chunks are large sequential
// blocks, so writes go to w directly: buffering would only obscure which write
// failed.
type Writer struct {
	w         io.Writer
	schema    []ColumnSchema
	rowGroups []RowGroupMeta
	offset    int64
	writeErr  error

	opts Options

	// level is the compression level of every block. It starts as CompressLevel
	// and Optimize raises it to OptimizeLevel, so that the write uses the same
	// level the layouts were measured at. A column written at another level is
	// not the column the measurement picked, which makes the pass and the write
	// that follows it one setting.
	level int

	// layouts is what Optimize chose per column, and is nil until it is called.
	// A column the writer has not optimized is written the way opts and its own
	// type describe, so the two never disagree about who decided.
	layouts []LayoutResult

	// measure is one scratch buffer per column, for the measurements Optimize
	// makes. A column is the unit of parallelism below, so element i is only
	// touched by column i's goroutine and needs no synchronisation.
	measure []measureScratch

	// tokOf is the position in the header's tokenizer table of the model a column
	// is stored through, keyed by column index. A column without an entry has no
	// tokenizer, and its ColMeta records a zero, which names none.
	tokOf map[int]int

	// sem bounds the compressors one write has in flight, across every column and
	// every block of every column. It outlives a row group so the blocks of one
	// row group share it with the blocks of the next.
	sem chan struct{}
}

// Options is how a writer lays out the columns it has not been told to Optimize.
// Every field's zero value means something, so an empty Options is a valid
// configuration: it asks for the encoding each column's type implies and for no
// compression at all. NewWriter supplies Flate on top of that.
type Options struct {
	// Encoding is the encoding for a column the writer has not optimized. Zero
	// is not an encoding and means the writer takes one from the column's type:
	// RLEBitpack for bool, Delta for integers, Plain for the rest. An encoding
	// that cannot serve a column's type fails the write rather than being
	// substituted.
	Encoding uint8

	// Compress is the codec every column is compressed with. CompressNone, the
	// zero value, stores columns exactly as their encoder produced them. Zstd is
	// not in this build and is rejected.
	Compress uint8

	// CompressLevel is the level of the codecs that take one, for a column the
	// writer has not been asked to Optimize. Zero means level 3, and values
	// outside the range a codec accepts are pulled back to the nearest one it
	// accepts.
	CompressLevel int

	// OptimizeLevel is the level the Optimize pass measures at and every row
	// group after it is written at; zero means the best a codec offers. A harder
	// level costs write time alone, because a block compressed harder is no
	// harder to decompress, so on a dataset already worth a full pass the size it
	// gains is free at read. Set it lower to spend less on the pass, or set it to
	// CompressLevel to write at that level instead.
	OptimizeLevel int

	// BlockSize is the most encoded bytes in one independently compressed block.
	// Zero means 256KB, above DEFLATE's window and small enough that a wide
	// column still splits into several jobs.
	BlockSize int

	// Tokenizers is the tokenizer a column is stored through, keyed by column
	// index: a TypeString column at 2 handed a Falcon model is written as the ids
	// that model maps its values to. A column not in the map is laid out the way
	// its type implies, so a table with one text column among ten needs one entry.
	// Optimize decides whether the ids are worth it; without it, the column is
	// tokenized at TokenizedLayout's zero value, raw ids and per-value counts.
	Tokenizers map[int]*tokenizer.Model

	// TokenizedLayout is how a tokenized column the writer has not optimized
	// stores its ids and its value boundaries. Optimize picks its own, and the
	// one it picks is what the write uses.
	TokenizedLayout TokenizedLayout
}

// validate reports whether opts asks for a codec and an encoding this build can
// produce, so an option the writer cannot serve is reported before it has written
// any bytes rather than partway through a row group.
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
	switch o.TokenizedLayout.Rep {
	case TokenizedRaw, TokenizedRemap, TokenizedBits:
	default:
		return fmt.Errorf("keine: tokenized representation %d is not one this build writes", o.TokenizedLayout.Rep)
	}
	switch o.TokenizedLayout.Bound {
	case TokenizedCounts, TokenizedOffsets, TokenizedDeltas:
	default:
		return fmt.Errorf("keine: tokenized boundary %d is not one this build writes", o.TokenizedLayout.Bound)
	}
	for i, tok := range o.Tokenizers {
		if tok == nil {
			return fmt.Errorf("keine: tokenizer for column %d is nil", i)
		}
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
	wr := &Writer{w: w, schema: schema, opts: opts,
		level: compressLevel(opts.CompressLevel),
		sem:   make(chan struct{}, runtime.NumCPU())}
	if err := opts.validate(); err != nil {
		wr.writeErr = err
		return wr
	}
	for i := range opts.Tokenizers {
		if i < 0 || i >= len(schema) {
			wr.writeErr = fmt.Errorf("keine: tokenizer for column %d, schema has %d columns", i, len(schema))
			return wr
		}
	}
	digests, tokOf := tokenizerTable(len(schema), opts.Tokenizers)
	wr.tokOf = tokOf

	head := []byte(magic)
	if len(digests) > 0 {
		head = tokenizerHeader(tokenizedVersion, digests)
	} else {
		head = append(head, formatVersion)
	}
	if _, err := w.Write(head); err != nil {
		wr.writeErr = err
		return wr
	}
	wr.offset = int64(len(head))
	return wr
}

// tokenizerTable orders the models a schema's columns use into the digest table
// the header writes. The walk is by column index, never by map order, because the
// table's order settles the bytes on disk; columns sharing a model share one
// entry, because the table names tokenizers and not the columns that use them.
func tokenizerTable(n int, toks map[int]*tokenizer.Model) (digests [][32]byte, of map[int]int) {
	of = make(map[int]int, len(toks))
	byHash := make(map[[32]byte]int, len(toks))
	for i := 0; i < n; i++ {
		tok, ok := toks[i]
		if !ok {
			continue
		}
		h := tok.Hash()
		if at, ok := byHash[h]; ok {
			of[i] = at
			continue
		}
		of[i] = len(digests)
		byHash[h] = len(digests)
		digests = append(digests, h)
	}
	return digests, of
}

// tokenizerHeader builds the leading bytes of a file with a tokenizer table: the
// magic, the version, the digest count, then the digests themselves. A column
// names one by its one-based position among them.
func tokenizerHeader(version byte, digests [][32]byte) []byte {
	head := make([]byte, len(magic)+1+4+digestLen*len(digests))
	copy(head, magic)
	head[len(magic)] = version
	binary.LittleEndian.PutUint32(head[len(magic)+1:], uint32(len(digests)))
	for i, d := range digests {
		copy(head[len(magic)+5+i*digestLen:], d[:])
	}
	return head
}

// Optimize encodes and compresses every candidate layout over every value of
// columns and keeps the winner per column, so the row groups written afterwards
// are stored the way the whole column earns rather than the way its type
// suggests. It does not guess at the shape of the data: it measures at the level
// the file will then be written at, so the layout it reports is the one the write
// produces.
//
// That thoroughness is the cost. A column of a million values pays for a full
// pass per candidate, where the layout a type implies costs nothing and is right
// more often than it is wrong. Call Optimize once for a dataset and every row
// group after it uses the layouts it chose.
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
	return w.measureLayouts(typed)
}

// OptimizeTyped is Optimize for a caller that holds each column as a slice of
// the Go type its schema implies: []int64 for an int64 column, []string for a
// string one. The pass costs what it costs either way, since it is compression
// bound and the columns it measures are the same ones the boxed entry hands it;
// this entry exists because without it a typed caller would have to box every
// value to reach the pass at all, which is the cost the typed entries remove.
func (w *Writer) OptimizeTyped(columns []any) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if _, err := w.checkTypedColumns(columns); err != nil {
		return err
	}
	return w.measureLayouts(columns)
}

// measureLayouts encodes and compresses every candidate layout over every value
// of each typed column and keeps the winner per column, at the level the row
// groups afterwards are written at. Both entries reach it with columns already
// in the types the encoders consume, so a column is measured exactly as it is
// stored whichever one the caller used.
func (w *Writer) measureLayouts(typed []any) error {
	if len(w.measure) < len(typed) {
		w.measure = make([]measureScratch, len(typed))
	}
	w.layouts = make([]LayoutResult, len(typed))

	level := optimizeLevel(w.opts.OptimizeLevel)
	w.level = level
	var wg sync.WaitGroup
	for i := range typed {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Measuring is encode and compress work, so it takes the same slot a
			// block's compression would.
			w.sem <- struct{}{}
			defer func() { <-w.sem }()

			results := benchmarkLayoutsTyped(typed[i], w.schema[i], w.opts.Tokenizers[i], &w.measure[i], level)
			// A column either entry accepted has candidate encodings, so this is
			// never empty. An empty result would leave the zero layout, whose
			// encoding no encoder recognises, and the write would fail on it
			// rather than store the column some other way.
			if len(results) > 0 {
				w.layouts[i] = results[0]
			}
		}(i)
	}
	wg.Wait()
	return nil
}

// checkTypedColumns reports whether columns has one slice per schema column,
// each a slice of the Go type its schema implies, and one length across all of
// them. It returns the row count too, which the loop has to compute anyway, so
// a writer with no columns needs no special case. A nullable column is not one
// it can accept: a typed slice has nowhere to put a null, the way ReadColumn
// has nowhere to return one.
func (w *Writer) checkTypedColumns(columns []any) (uint32, error) {
	if len(columns) != len(w.schema) {
		return 0, fmt.Errorf("keine: expected %d columns, got %d", len(w.schema), len(columns))
	}
	numRows := uint32(0)
	for i, col := range columns {
		if w.schema[i].Nullable {
			return 0, fmt.Errorf("keine: column %d (%s) is nullable; use AddRowGroup for a column with nulls", i, w.schema[i].Name)
		}
		if !typedColumn(w.schema[i].Type, col) {
			return 0, typedColumnErr(i, w.schema[i], col)
		}
		n := uint32(sliceLen(col))
		if i == 0 {
			numRows = n
			continue
		}
		if n != numRows {
			return 0, fmt.Errorf("keine: column %d has %d rows, expected %d", i, n, numRows)
		}
	}
	return numRows, nil
}

// typedColumnErr explains why col is not the slice typ implies, naming both
// sides through reflect so no parallel table of type names is needed. An
// unknown type tag is reported the way canonicalColumnTyped reports one.
func typedColumnErr(i int, schema ColumnSchema, col any) error {
	want, ok := goType[schema.Type]
	if !ok {
		return fmt.Errorf("keine: column %d (%s) has unknown type %d", i, schema.Name, schema.Type)
	}
	return fmt.Errorf("keine: column %d (%s) is %v, want %v", i, schema.Name, reflect.TypeOf(col), want)
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
// since a row group needs both and this is the one pass that reads every value.
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

	typed, metas, bitmasks, err := w.collect(columns)
	if err != nil {
		return err
	}
	return w.writeRowGroup(numRows, typed, metas, bitmasks)
}

// AddRowGroupTyped is AddRowGroup for a caller that already holds each column as
// a slice of the Go type its schema implies: []int64 for an int64 column,
// []string for a string one. Nothing between the caller and the encoders
// re-reads the values, so a column is stored exactly as the caller had it and
// the file is byte for byte the one AddRowGroup would have written over the same
// values.
//
// The contract is the read side's, reversed: a nullable column is an error
// rather than a column with nil values, and a slice whose element type is not
// the one the schema implies is an error rather than a widening conversion.
// AddRowGroup widens an int into an int64 because a caller handing over []any
// has not said what the values are; this one has.
//
// Every encoder allocates its own output, so nothing the writer keeps points
// into a caller's slice and the slices can be reused as soon as this returns.
func (w *Writer) AddRowGroupTyped(columns []any) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	numRows, err := w.checkTypedColumns(columns)
	if err != nil {
		return err
	}

	metas := make([]ColMeta, len(columns))
	for i, col := range columns {
		fillStatsTyped(&metas[i], col)
	}
	return w.writeRowGroup(numRows, columns, metas, nil)
}

// writeRowGroup is the tail both entries share: encode every column with the
// layout chosen for it, write the chunks in order, and record the row group.
// typed and metas are one per column, already in the types the encoders consume;
// bitmasks is one per column too, and nil on the typed path, where no column can
// hold a null. WriteChunk writes a zero length for a nil bitmap the way it does
// for the empty slice EncodeBitmap returns, so the bytes do not depend on which
// entry the caller used.
func (w *Writer) writeRowGroup(numRows uint32, typed []any, metas []ColMeta, bitmasks [][]byte) error {
	rg := RowGroupMeta{
		NumRows:    numRows,
		ByteOffset: w.offset,
		Columns:    make([]ColMeta, len(typed)),
	}

	// Encoding and compressing a column touches no other, so both spread across
	// cores; the compressor is where the write's time goes, and a table wider
	// than the machine has cores still runs no more compressors at once than
	// there are. Each column's result is written in order afterwards, which keeps
	// the bytes identical to a serial write, since no encoder sees another
	// column's data. The schema fixes the column count for the writer's lifetime,
	// so these buffers grow once and later row groups reuse them.
	if len(w.measure) < len(typed) {
		w.measure = make([]measureScratch, len(typed))
	}
	chunks, err := encodeColumns(typed, w.schema, w.layouts, w.opts, w.level, w.measure, w.sem)
	if err != nil {
		return err
	}

	for i := range typed {
		meta := metas[i]
		chunk := chunks[i]
		if bitmasks != nil {
			chunk.NullBitmap = bitmasks[i]
		}

		cw := &countingWriter{w: w.w}
		if err := WriteChunk(cw, chunk); err != nil {
			return fmt.Errorf("keine: writing column %d (%s): %w", i, w.schema[i].Name, err)
		}
		w.offset += cw.count

		meta.ByteLength = cw.count
		meta.Encoding = chunk.Encoding
		meta.Compress = chunk.Compress
		// The tokenizer table sits in the header, so a column names its entry
		// rather than carrying the digest. Zero names none, which is what a
		// column the reader has no tokenizer for decodes as.
		if at, ok := w.tokOf[i]; ok {
			meta.Tokenizer = at + 1
		}
		metas[i] = meta
	}
	copy(rg.Columns, metas)

	w.rowGroups = append(w.rowGroups, rg)
	return nil
}

// encodeColumns encodes and compresses each column with the layout chosen for it,
// returning one chunk per column. Encoding a column touches no other, and
// neither does compressing one block of it, so both spread across cores. measure
// supplies one scratch buffer per column, whose element i belongs to column i
// alone.
//
// level is the level every block is compressed at, and is the one the layouts
// were measured at when they came from Optimize. Compressing at another level
// would write a column other than the one the pass picked.
//
// sem is shared with the blocks, so a column takes a slot per block it is
// compressing rather than for its whole run. A column waiting on its blocks
// holds none, so the semaphore cannot be exhausted by columns waiting for work
// it is withholding.
func encodeColumns(typed []any, schema []ColumnSchema, layouts []LayoutResult, opts Options, level int, measure []measureScratch, sem chan struct{}) ([]ColumnChunk, error) {
	chunks := make([]ColumnChunk, len(typed))
	errs := make([]error, len(typed))
	var wg sync.WaitGroup
	for i := range typed {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			enc, codec := chosenLayout(i, schema[i], layouts, opts)
			var encoded []byte
			var err error
			if enc == EncTokenized {
				encoded, err = EncodeTokenized(typed[i], opts.Tokenizers[i], tokenizedLayoutFor(i, layouts, opts))
			} else {
				encoded, err = encodeWith(enc, typed[i], schema[i].Type)
			}
			if err != nil {
				errs[i] = err
				return
			}
			blks, used := splitBlocks(encoded, codec, level, blockOfSize(opts.BlockSize), sem)
			chunks[i] = ColumnChunk{
				Encoding:  enc,
				Compress:  used,
				RawLength: uint32(len(encoded)),
				Blocks:    blks,
			}
		}(i)
	}
	wg.Wait()

	// The columns run concurrently, but the caller hears about the first column
	// that could not be encoded, not whichever goroutine happened to report
	// first.
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("keine: encoding column %d (%s): %w", i, schema[i].Name, err)
		}
	}
	return chunks, nil
}

// chosenLayout is the layout column i is written with. Optimize's choice comes
// first, since it has measured the column; otherwise an encoding the caller set
// overrides the one the type implies, and the codec is the caller's either way.
func chosenLayout(i int, schema ColumnSchema, layouts []LayoutResult, opts Options) (uint8, uint8) {
	if layouts != nil {
		return layouts[i].Encoding, layouts[i].Compress
	}
	if _, ok := opts.Tokenizers[i]; ok {
		return EncTokenized, opts.Compress
	}
	if opts.Encoding == 0 {
		enc, _ := defaultLayout(schema.Type)
		return enc, opts.Compress
	}
	return opts.Encoding, opts.Compress
}

// tokenizedLayoutFor is the layout a tokenized column's ids and boundaries are
// stored in: the one Optimize measured the smallest, or the one the options ask
// for when the writer decides the layout itself.
func tokenizedLayoutFor(i int, layouts []LayoutResult, opts Options) TokenizedLayout {
	if layouts != nil {
		return TokenizedLayout{Rep: layouts[i].Rep, Bound: layouts[i].Bound}
	}
	return opts.TokenizedLayout
}

// blockOfSize is the block size a writer uses; zero and anything below a block
// mean the default. A block size of one is honoured by the arithmetic but costs
// eight bytes of header per byte of data, so a caller asking for it gets the
// file they described.
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

// fillStatsTyped folds the byte form of every value into meta. typed is a
// canonical column, so every case it can be is one of the thirteen below and no
// default is needed.
//
// Fixed width columns are a special case: the min and the max are found by
// comparing the values themselves and serialised once each at the end, rather
// than serialising every value into a scratch buffer and comparing the bytes.
// A value's bytes are not a sort key at all. Little endian puts the least
// significant byte first, and two's complement puts every negative integer
// above every positive one, so a byte wise comparison of a column of int64 can
// report a min larger than its max. Comparing the values costs one comparison
// and one branch per value, and drops two length checks that are false for
// every value after the first.
func fillStatsTyped(meta *ColMeta, typed any) {
	switch s := typed.(type) {
	case []bool:
		lo, hi := byte(1), byte(0)
		for _, v := range s {
			if v {
				hi = 1
			} else {
				lo = 0
			}
		}
		if len(s) > 0 {
			recordFixed(meta, 1, []byte{lo}, []byte{hi})
		}
	case []int8:
		if lo, hi, ok := fixedEnds(s, 1, func(v int8) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 1, lo, hi)
		}
	case []int16:
		if lo, hi, ok := fixedEnds(s, 2, func(v int16) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 2, lo, hi)
		}
	case []int32:
		if lo, hi, ok := fixedEnds(s, 4, func(v int32) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 4, lo, hi)
		}
	case []int64:
		if lo, hi, ok := fixedEnds(s, 8, func(v int64) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 8, lo, hi)
		}
	case []uint8:
		if lo, hi, ok := fixedEnds(s, 1, func(v uint8) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 1, lo, hi)
		}
	case []uint16:
		if lo, hi, ok := fixedEnds(s, 2, func(v uint16) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 2, lo, hi)
		}
	case []uint32:
		if lo, hi, ok := fixedEnds(s, 4, func(v uint32) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 4, lo, hi)
		}
	case []uint64:
		if lo, hi, ok := fixedEnds(s, 8, func(v uint64) uint64 { return v }); ok {
			recordFixed(meta, 8, lo, hi)
		}
	case []float32:
		if lo, hi, ok := fixedEnds(s, 4, func(v float32) uint64 { return uint64(math.Float32bits(v)) }); ok {
			recordFixed(meta, 4, lo, hi)
		}
	case []float64:
		if lo, hi, ok := fixedEnds(s, 8, func(v float64) uint64 { return math.Float64bits(v) }); ok {
			recordFixed(meta, 8, lo, hi)
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

// fixedEnds reports the smallest and largest of s, whose values are all width
// bytes, as serialised bytes. s is a column of an ordered type, so its values
// compare with < and the ends are found in one pass. toU64 writes one value the
// way the encoder stores it, little endian, which is what makes the result the
// bytes a caller comparing stored values would have found. ok is false for an
// empty column, whose min and max do not exist.
func fixedEnds[T cmp.Ordered](s []T, width int, toU64 func(T) uint64) (lo, hi []byte, ok bool) {
	if len(s) == 0 {
		return nil, nil, false
	}
	loV, hiV := s[0], s[0]
	for _, v := range s[1:] {
		if v < loV {
			loV = v
		}
		if hiV < v {
			hiV = v
		}
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], toU64(loV))
	lo = append([]byte(nil), b[:width]...)
	binary.LittleEndian.PutUint64(b[:], toU64(hiV))
	hi = append([]byte(nil), b[:width]...)
	return lo, hi, true
}

// recordFixed writes the ends of a fixed width column, whose values are all
// width bytes. The caller has found them already, so this only stores them.
func recordFixed(meta *ColMeta, width int, lo, hi []byte) {
	meta.MinVal = append(meta.MinVal, lo...)
	meta.MaxVal = append(meta.MaxVal, hi...)
	meta.MinLen = uint32(width)
	meta.MaxLen = uint32(width)
}

// statBytes folds b into meta's min and max. MinLen and MaxLen track value
// length separately from min and max, since a column's longest value is not
// necessarily its largest. Variable width columns use it per value; fixed width
// columns find their ends up front and go through recordFixed instead.
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
