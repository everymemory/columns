// Fakes and fixtures the error-path tests build inputs from. The chunk and file
// crafters live here because keine_test.go uses them too.

package keine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var errSynthetic = errors.New("keine test: synthetic failure")

// failAfter is an io.Writer that succeeds for the first n Write calls and then
// fails, so each error branch of a multi-write function can be reached.
type failAfter struct {
	n     int
	calls int
}

func (w *failAfter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.n {
		return 0, errSynthetic
	}
	return len(p), nil
}

// flakyReadSeeker fails its failSeek-th Seek call and its failRead-th Read call.
// A zero value means never fail.
type flakyReadSeeker struct {
	r        *bytes.Reader
	failSeek int
	failRead int
	seeks    int
	reads    int
}

func (f *flakyReadSeeker) Read(p []byte) (int, error) {
	f.reads++
	if f.failRead > 0 && f.reads == f.failRead {
		return 0, errSynthetic
	}
	return f.r.Read(p)
}

func (f *flakyReadSeeker) Seek(off int64, whence int) (int64, error) {
	f.seeks++
	if f.failSeek > 0 && f.seeks == f.failSeek {
		return 0, errSynthetic
	}
	return f.r.Seek(off, whence)
}

type failingResetReader struct{}

func (failingResetReader) Read([]byte) (int, error) { return 0, io.EOF }

func (failingResetReader) Close() error { return nil }

func (failingResetReader) Reset(io.Reader, []byte) error {
	return fmt.Errorf("keine test: reset refused")
}

// mockCompressor is a placeholder for a pool to hold. Nothing writes through
// it; it only has to be one pool's occupant rather than another's.
type mockCompressor struct{}

func (*mockCompressor) Write([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func (*mockCompressor) Close() error { return nil }

func (*mockCompressor) Reset(io.Writer) error { return nil }

// craftChunk assembles the on-disk bytes of one column chunk. Its data is
// stored uncompressed, so the raw length is the data length.
func craftChunk(enc, codec uint8, bitmap, data []byte) []byte {
	var b []byte
	b = append(b, enc, codec)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(bitmap)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = binary.LittleEndian.AppendUint32(b, 1) // one block holding the data
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = append(b, bitmap...)
	b = append(b, data...)
	return b
}

// craftFile wraps a hand-built chunk in a complete keine file.
func craftFile(chunk []byte, meta ColMeta, schema []ColumnSchema, numRows uint32) []byte {
	var f []byte
	f = append(f, magic...)
	f = append(f, formatVersion)
	f = append(f, chunk...)
	fb := encodeFooter(Footer{
		Schema: schema,
		RowGroups: []RowGroupMeta{{
			NumRows:    numRows,
			ByteOffset: int64(len(magic) + 1),
			Columns:    []ColMeta{meta},
		}},
	})
	f = append(f, fb...)
	f = binary.LittleEndian.AppendUint32(f, uint32(len(fb)))
	f = append(f, magic...)
	return f
}
