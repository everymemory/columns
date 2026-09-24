package keine

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ColumnChunk is one encoded column as stored on disk.
type ColumnChunk struct {
	Encoding   uint8
	Compress   uint8
	NullBitmap []byte
	// RawLength is len(Data) after decompression. It is the encoded length when
	// the codec is CompressNone. A reader sizes its decompression buffer from it,
	// so a chunk decompresses into one allocation instead of growing into its
	// answer a piece at a time.
	RawLength uint32
	Data      []byte
}

// parseChunk reads a chunk out of b, which must hold exactly the chunk's bytes.
// c's slices point into b, so b has to outlive c — the reader keeps its buffer
// for the next row group, and a chunk handed to another goroutine is decoded
// before that happens.
func parseChunk(b []byte, c *ColumnChunk) error {
	if len(b) < 14 {
		return fmt.Errorf("keine: chunk is %d bytes, too small for a header", len(b))
	}
	c.Encoding = b[0]
	c.Compress = b[1]
	nullLen := binary.LittleEndian.Uint32(b[2:6])
	c.RawLength = binary.LittleEndian.Uint32(b[6:10])
	dataLen := binary.LittleEndian.Uint32(b[10:14])
	if uint64(len(b)) < uint64(14)+uint64(nullLen)+uint64(dataLen) {
		return fmt.Errorf("keine: chunk header wants %d bytes of data, chunk is %d", dataLen, len(b))
	}
	c.NullBitmap = b[14 : 14+nullLen]
	c.Data = b[14+nullLen : 14+nullLen+dataLen]
	return nil
}

// WriteChunk writes c to w as: encoding byte, compress byte, null bitmap
// length, raw data length, data length (three uint32s), then the null bitmap
// and the data. Integers are little-endian.
func WriteChunk(w io.Writer, c ColumnChunk) error {
	if err := binary.Write(w, binary.LittleEndian, c.Encoding); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, c.Compress); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(c.NullBitmap))); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, c.RawLength); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(c.Data))); err != nil {
		return err
	}
	if _, err := w.Write(c.NullBitmap); err != nil {
		return err
	}
	if _, err := w.Write(c.Data); err != nil {
		return err
	}
	return nil
}

// ReadChunk reads one chunk written by WriteChunk.
func ReadChunk(r io.Reader) (ColumnChunk, error) {
	var c ColumnChunk
	var nullLen, dataLen uint32
	if err := binary.Read(r, binary.LittleEndian, &c.Encoding); err != nil {
		return c, fmt.Errorf("keine: cannot read chunk encoding: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &c.Compress); err != nil {
		return c, fmt.Errorf("keine: cannot read chunk codec: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &nullLen); err != nil {
		return c, fmt.Errorf("keine: cannot read bitmap length: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &c.RawLength); err != nil {
		return c, fmt.Errorf("keine: cannot read raw data length: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &dataLen); err != nil {
		return c, fmt.Errorf("keine: cannot read chunk data length: %w", err)
	}
	c.NullBitmap = make([]byte, nullLen)
	if _, err := io.ReadFull(r, c.NullBitmap); err != nil {
		return c, fmt.Errorf("keine: cannot read null bitmap: %w", err)
	}
	c.Data = make([]byte, dataLen)
	if _, err := io.ReadFull(r, c.Data); err != nil {
		return c, fmt.Errorf("keine: cannot read chunk data: %w", err)
	}
	return c, nil
}
