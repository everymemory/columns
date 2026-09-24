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

// Writer accumulates row groups into a keine file. Writes go to w directly:
// chunks are large sequential blocks, and buffering would only obscure which
// write failed.
type Writer struct {
	w         io.Writer
	schema    []ColumnSchema
	rowGroups []RowGroupMeta
	offset    int64
	writeErr  error
}

// NewWriter writes the leading magic. If that write fails, the error is
// returned by the first AddRowGroup or Close call.
func NewWriter(w io.Writer, schema []ColumnSchema) *Writer {
	wr := &Writer{w: w, schema: schema}
	if _, err := w.Write([]byte(magic)); err != nil {
		wr.writeErr = err
		return wr
	}
	wr.offset = int64(len(magic))
	return wr
}

// AddRowGroup encodes each column with the layout ExperimentLayouts selects,
// writes the chunks sequentially, and records their metadata. Columns whose
// schema is nullable may hold nil values; those are recorded in a null bitmap
// and only the remaining values are encoded.
func (w *Writer) AddRowGroup(columns [][]any) error {
	if w.writeErr != nil {
		return w.writeErr
	}
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

	rg := RowGroupMeta{
		NumRows:    numRows,
		ByteOffset: w.offset,
		Columns:    make([]ColMeta, len(columns)),
	}

	// Null handling and coercion read the caller's slices and are cheap, so they
	// stay serial. Each column's typed slice and statistics feed the work below.
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
		// Coerce the column once: the statistics, the layout experiment and the
		// final encode all consume the same typed slice.
		t, err := canonicalColumnTyped(dense, schema.Type)
		if err != nil {
			return fmt.Errorf("keine: collecting stats for column %d (%s): %w", i, schema.Name, err)
		}
		fillStatsTyped(&meta, t)

		typed[i] = t
		metas[i] = meta
		bitmasks[i] = EncodeBitmap(nulls)
	}

	// Choosing a layout, encoding and compressing are independent per column and
	// are where the write time actually goes — the compressor is the largest item
	// on the profile — so they run across columns at once. A column's result is
	// written in order afterwards, which keeps the bytes identical to a serial
	// write: no encoder sees another column's data.
	chunks := encodeColumns(typed, w.schema)

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

// encodeColumns picks a layout for each column and encodes and compresses it,
// returning one chunk per column. Encoding one column touches no other, so the
// work is spread across cores; a table wider than the machine has cores still
// runs no more encoders at once than there are.
func encodeColumns(typed []any, schema []ColumnSchema) []ColumnChunk {
	chunks := make([]ColumnChunk, len(typed))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i := range typed {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			best := experimentLayoutsTyped(typed[i], schema[i])
			// Every value in the column has been canonicalized already, and the
			// winning layout round tripped successfully inside the experiment, so
			// encoding it again cannot fail.
			encoded, _ := encodeWith(best.Encoding, typed[i], schema[i].Type)
			// best.Compress is one of the codecs Compress implements.
			compressed, _ := Compress(encoded, best.Compress)
			chunks[i] = ColumnChunk{
				Encoding:  best.Encoding,
				Compress:  best.Compress,
				RawLength: uint32(len(encoded)),
				Data:      compressed,
			}
		}(i)
	}
	wg.Wait()
	return chunks
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
