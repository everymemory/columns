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
	Data       []byte
}

// WriteChunk writes c to w as: encoding byte, compress byte, null bitmap
// (uint32 length plus bytes), then data (uint32 length plus bytes). Integers
// are little-endian.
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
	if _, err := w.Write(c.NullBitmap); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(c.Data))); err != nil {
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
	c.NullBitmap = make([]byte, nullLen)
	if _, err := io.ReadFull(r, c.NullBitmap); err != nil {
		return c, fmt.Errorf("keine: cannot read null bitmap: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &dataLen); err != nil {
		return c, fmt.Errorf("keine: cannot read data length: %w", err)
	}
	c.Data = make([]byte, dataLen)
	if _, err := io.ReadFull(r, c.Data); err != nil {
		return c, fmt.Errorf("keine: cannot read chunk data: %w", err)
	}
	return c, nil
}
