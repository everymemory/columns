package keine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
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
		if err := fillStats(&meta, dense, schema.Type); err != nil {
			return fmt.Errorf("keine: collecting stats for column %d (%s): %w", i, schema.Name, err)
		}

		best := ExperimentLayouts(dense, schema)
		// Every value in dense has been canonicalized by fillStats, and the
		// winning layout round tripped successfully inside ExperimentLayouts.
		encoded, _ := encodeWith(best.Encoding, dense, schema.Type)
		// best.Compress is one of the codecs Compress implements.
		compressed, _ := Compress(encoded, best.Compress)

		cw := &countingWriter{w: w.w}
		if err := WriteChunk(cw, ColumnChunk{
			Encoding:   best.Encoding,
			Compress:   best.Compress,
			NullBitmap: EncodeBitmap(nulls),
			Data:       compressed,
		}); err != nil {
			return fmt.Errorf("keine: writing column %d (%s): %w", i, schema.Name, err)
		}
		w.offset += cw.count

		meta.ByteLength = cw.count
		meta.Encoding = best.Encoding
		meta.Compress = best.Compress
		rg.Columns[i] = meta
	}

	w.rowGroups = append(w.rowGroups, rg)
	return nil
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

// fillStats records the min/max value bytes and value lengths over vals.
func fillStats(meta *ColMeta, vals []any, typ uint8) error {
	seen := false
	for _, v := range vals {
		b, err := valueBytes(v, typ)
		if err != nil {
			return err
		}
		if !seen {
			meta.MinVal = append([]byte(nil), b...)
			meta.MaxVal = append([]byte(nil), b...)
			meta.MinLen = uint32(len(b))
			meta.MaxLen = uint32(len(b))
			seen = true
			continue
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
	return nil
}

// valueBytes returns a comparable byte form of v for min/max statistics.
// Fixed width values use their plain encoding; strings and byte slices use
// their raw bytes so the length prefix does not affect ordering.
func valueBytes(v any, typ uint8) ([]byte, error) {
	cv, err := canonicalValue(v, typ)
	if err != nil {
		return nil, err
	}
	switch s := cv.(type) {
	case string:
		return []byte(s), nil
	case []byte:
		return s, nil
	default:
		return EncodePlain([]any{cv})
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
