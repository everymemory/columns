// Chunk layout and block splitting error paths.

package keine

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWriteChunkErrors(t *testing.T) {
	chunk := ColumnChunk{
		Encoding:   EncPlain,
		Compress:   CompressNone,
		NullBitmap: []byte{1},
		RawLength:  4,
		Blocks:     []ColumnBlock{{RawLength: 4, Data: []byte{1, 2, 3, 4}}},
	}
	for n := 0; n < 4; n++ {
		if err := WriteChunk(&failAfter{n: n}, chunk); err == nil {
			t.Errorf("WriteChunk with a writer failing after %d writes: want error, got nil", n)
		}
	}
}

func TestReadChunkTruncated(t *testing.T) {
	full := []byte{byte(EncPlain), byte(CompressNone)}
	full = binary.LittleEndian.AppendUint32(full, 2) // null bitmap length
	full = binary.LittleEndian.AppendUint32(full, 4) // raw length
	full = binary.LittleEndian.AppendUint32(full, 4) // stored length
	full = binary.LittleEndian.AppendUint32(full, 1) // block count
	full = binary.LittleEndian.AppendUint32(full, 4) // block raw length
	full = binary.LittleEndian.AppendUint32(full, 4) // block stored length
	full = append(full, 0x01, 0x02)                  // null bitmap
	full = append(full, 0xAA, 0xBB, 0xCC, 0xDD)      // block

	for cut := 0; cut < len(full); cut++ {
		if _, err := ReadChunk(bytes.NewReader(full[:cut])); err == nil {
			t.Errorf("ReadChunk of %d of %d bytes: want error, got nil", cut, len(full))
		}
	}
	if _, err := ReadChunk(bytes.NewReader(full)); err != nil {
		t.Errorf("ReadChunk of a complete chunk: %v", err)
	}
}

// TestReadChunkLyingHeader covers a corrupt or hostile chunk header: the block
// table asks for more bytes than the chunk has, and the raw length does not
// match the blocks. Both must fail rather than read past the chunk or return a
// column that is partly zeros.
func TestReadChunkLyingHeader(t *testing.T) {
	encode := func(raw, stored, blockRaw, blockStored uint32) []byte {
		b := []byte{byte(EncPlain), byte(CompressNone)}
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = binary.LittleEndian.AppendUint32(b, raw)
		b = binary.LittleEndian.AppendUint32(b, stored)
		b = binary.LittleEndian.AppendUint32(b, 1)
		b = binary.LittleEndian.AppendUint32(b, blockRaw)
		b = binary.LittleEndian.AppendUint32(b, blockStored)
		b = append(b, 0xAA, 0xBB, 0xCC, 0xDD)
		return b
	}
	for _, c := range []struct {
		name                       string
		raw, stored, bRaw, bStored uint32
	}{
		{"block past the chunk", 4, 4, 4, 8},
		{"blocks hold more than the header", 4, 8, 4, 4},
		{"raw length short of the block", 2, 4, 4, 4},
	} {
		if _, err := ReadChunk(bytes.NewReader(encode(c.raw, c.stored, c.bRaw, c.bStored))); err == nil {
			t.Errorf("%s: ReadChunk: want error, got nil", c.name)
		}
		chunk := ColumnChunk{}
		if err := parseChunk(encode(c.raw, c.stored, c.bRaw, c.bStored), &chunk); err == nil {
			t.Errorf("%s: parseChunk: want error, got nil", c.name)
		}
	}

	// A chunk that declares no blocks is malformed, not an empty column.
	noBlocks := []byte{byte(EncPlain), byte(CompressNone), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if _, err := ReadChunk(bytes.NewReader(noBlocks)); err == nil {
		t.Error("ReadChunk with no blocks: want error, got nil")
	}
	if err := parseChunk(noBlocks, &ColumnChunk{}); err == nil {
		t.Error("parseChunk with no blocks: want error, got nil")
	}
}
