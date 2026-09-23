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

// detach returns a copy of c whose slices own their bytes. ReadChunkInto fills a
// chunk's slices into caller-supplied scratch space, which a later read reuses;
// a chunk that outlives the read that produced it — one handed to another
// goroutine, or one kept while another column is read — has to be detached
// first or its data will be clobbered underneath it.
func (c ColumnChunk) detach() ColumnChunk {
	out := ColumnChunk{Encoding: c.Encoding, Compress: c.Compress}
	if c.NullBitmap != nil {
		out.NullBitmap = append([]byte(nil), c.NullBitmap...)
	}
	if c.Data != nil {
		out.Data = append([]byte(nil), c.Data...)
	}
	return out
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
	var bitmap, data []byte
	if err := readChunkInto(r, &c, &bitmap, &data); err != nil {
		return c, err
	}
	return c, nil
}

// readChunkInto is ReadChunk writing into the buffers at bitmap and data, which
// the caller reuses from one column to the next. Both stay live for as long as
// the chunk is used, so they cannot be recycled mid-read.
func readChunkInto(r io.Reader, c *ColumnChunk, bitmap, data *[]byte) error {
	var nullLen, dataLen uint32
	if err := binary.Read(r, binary.LittleEndian, &c.Encoding); err != nil {
		return fmt.Errorf("keine: cannot read chunk encoding: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &c.Compress); err != nil {
		return fmt.Errorf("keine: cannot read chunk codec: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &nullLen); err != nil {
		return fmt.Errorf("keine: cannot read bitmap length: %w", err)
	}
	*bitmap = growTo(*bitmap, int(nullLen))
	c.NullBitmap = (*bitmap)[:nullLen]
	if _, err := io.ReadFull(r, c.NullBitmap); err != nil {
		return fmt.Errorf("keine: cannot read null bitmap: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &dataLen); err != nil {
		return fmt.Errorf("keine: cannot read data length: %w", err)
	}
	*data = growTo(*data, int(dataLen))
	c.Data = (*data)[:dataLen]
	if _, err := io.ReadFull(r, c.Data); err != nil {
		return fmt.Errorf("keine: cannot read chunk data: %w", err)
	}
	return nil
}

// growTo returns a buffer holding at least n bytes, reusing b when it already
// does.
func growTo(b []byte, n int) []byte {
	if cap(b) >= n {
		return b
	}
	return make([]byte, n)
}
