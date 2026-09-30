package columns

import (
	"bytes"
	"encoding/gob"
)

// Footer is the file metadata block stored immediately before the trailing
// footer length and magic bytes.
type Footer struct {
	Schema    []ColumnSchema
	RowGroups []RowGroupMeta
}

// RowGroupMeta describes one row group as written by AddRowGroup.
type RowGroupMeta struct {
	NumRows    uint32
	ByteOffset int64
	Columns    []ColMeta
}

// ColMeta describes one column chunk within a row group.
type ColMeta struct {
	ByteLength int64
	NullCount  uint32
	MinVal     []byte
	MaxVal     []byte
	MinLen     uint32
	MaxLen     uint32
	Encoding   uint8
	Compress   uint8
	// Tokenizer is the one-based position of the tokenizer a tokenized column is
	// stored through, in the table the file header holds. Zero names none, so a
	// file written before tokenized columns, whose gob record carries no such
	// field, decodes to the same zero and reads as an untokenized column.
	Tokenizer int
}

// encodeFooter serializes f with encoding/gob, which cannot fail on the fixed
// shape of Footer: its only error sources are a value gob cannot represent and a
// writer that rejects a byte, and Footer is this package's own struct encoding
// into a buffer that never rejects one.
func encodeFooter(f Footer) []byte {
	var buf bytes.Buffer
	_ = gob.NewEncoder(&buf).Encode(f)
	return buf.Bytes()
}

// decodeFooter is the inverse of encodeFooter.
func decodeFooter(b []byte) (Footer, error) {
	var f Footer
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&f); err != nil {
		return f, err
	}
	return f, nil
}
