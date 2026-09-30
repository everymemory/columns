// Reader error paths, and the zero-row and empty-column edges.

package keine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

// TestCanonicalReadErrors covers the read paths that narrow decoded values to
// the declared type. Only a chunk that disagrees with its schema reaches them:
// here, a dictionary holding a value the column cannot hold.
func TestCanonicalReadErrors(t *testing.T) {
	dictData, err := EncodeDict([]any{"not a number"})
	if err != nil {
		t.Fatalf("EncodeDict: %v", err)
	}
	chunk := craftChunk(EncDict, CompressNone, nil, dictData)
	meta := ColMeta{ByteLength: int64(len(chunk)), Encoding: EncDict, Compress: CompressNone}
	schema := []ColumnSchema{{Name: "i", Type: TypeInt16}}
	file := craftFile(chunk, meta, schema, 1)

	r, err := NewReader(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.ReadRowGroup(0, []int{0}); err == nil {
		t.Error("ReadRowGroup of a dict that will not coerce: want error, got nil")
	}
	if _, err := ReadColumn[int16](r, 0, 0); err == nil {
		t.Error("ReadColumn of a dict that will not coerce: want error, got nil")
	}

	// The column narrows fine here, so the failure is the caller's type.
	delta := craftChunk(EncDelta, CompressNone, nil, EncodeDelta([]int64{42}))
	deltaMeta := ColMeta{ByteLength: int64(len(delta)), Encoding: EncDelta, Compress: CompressNone}
	r2, err := NewReader(bytes.NewReader(craftFile(delta, deltaMeta, schema, 1)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := ReadColumn[string](r2, 0, 0); err == nil {
		t.Error("ReadColumn[string] on an int16 column: want error, got nil")
	}
	if got, err := ReadColumn[int16](r2, 0, 0); err != nil || got[0] != 42 {
		t.Errorf("ReadColumn[int16] = %v, err %v, want [42]", got, err)
	}

	// A chunk that cannot be decoded at all fails through ReadColumn the same way
	// it fails through ReadRowGroup: three bytes is not an int16 pair.
	bad := craftChunk(EncPlain, CompressNone, nil, []byte{1, 2, 3})
	badMeta := ColMeta{ByteLength: int64(len(bad)), Encoding: EncPlain, Compress: CompressNone}
	r3, err := NewReader(bytes.NewReader(craftFile(bad, badMeta, schema, 2)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := ReadColumn[int16](r3, 0, 0); err == nil {
		t.Error("ReadColumn of a chunk that will not decode: want error, got nil")
	}
}

// TestZeroRowGroup writes and reads a row group with no rows at all. Every
// column's encoded stream is empty, so a chunk is one empty block and the reader
// reassembles nothing.
func TestZeroRowGroup(t *testing.T) {
	schema := []ColumnSchema{
		{Name: "i", Type: TypeInt64},
		{Name: "s", Type: TypeString},
		{Name: "b", Type: TypeBytes, Nullable: true},
	}
	var buf bytes.Buffer
	w := NewWriter(&buf, schema)
	if err := w.AddRowGroup([][]any{{}, {}, {}}); err != nil {
		t.Fatalf("AddRowGroup of zero rows: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, err := r.ReadRowGroup(0, []int{0, 1, 2})
	if err != nil {
		t.Fatalf("ReadRowGroup of zero rows: %v", err)
	}
	for i, col := range got {
		if len(col) != 0 {
			t.Errorf("column %d of a zero row group: got %d values, want 0", i, len(col))
		}
	}

	// The typed path reads the same empty column.
	if ints, err := ReadColumn[int64](r, 0, 0); err != nil {
		t.Errorf("ReadColumn of a zero row group: %v", err)
	} else if len(ints) != 0 {
		t.Errorf("ReadColumn of a zero row group: got %d values, want 0", len(ints))
	}
}

// Empty strings are a case the writer and reader both have to handle. A decoder
// that points a string at its buffer takes the address of a zero length slice
// for one, which used to panic.
func TestEmptyStringColumn(t *testing.T) {
	for _, vals := range [][]string{
		{""},
		{"", "", ""},
		{"", "a", ""},
		{"a", "", "b"},
	} {
		schema := []ColumnSchema{{Name: "s", Type: TypeString}}
		var buf bytes.Buffer
		w := NewWriter(&buf, schema)
		if err := w.AddRowGroup([][]any{toAnySlice(vals)}); err != nil {
			t.Fatalf("AddRowGroup(%v): %v", vals, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		r, err := NewReader(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		got, err := r.ReadRowGroup(0, []int{0})
		if err != nil {
			t.Fatalf("ReadRowGroup(%v): %v", vals, err)
		}
		want := make([]any, len(vals))
		for i, v := range vals {
			want[i] = v
		}
		if !reflect.DeepEqual(got[0], want) {
			t.Errorf("round trip of %v = %v", vals, got[0])
		}
	}
}

func toAnySlice[T any](s []T) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// TestNarrowingReadErrors covers the reads that fail when a chunk's encoding
// produces values its column's declared type cannot hold. Delta widens every
// integer column to int64, so a column declared bytes has nowhere to put an
// int64: the reader must refuse the file rather than panic.
func TestNarrowingReadErrors(t *testing.T) {
	bytesSchema := []ColumnSchema{{Name: "b", Type: TypeBytes}}
	chunk := craftChunk(EncDelta, CompressNone, nil, EncodeDelta([]int64{42, 43}))
	meta := ColMeta{ByteLength: int64(len(chunk)), Encoding: EncDelta, Compress: CompressNone}
	file := craftFile(chunk, meta, bytesSchema, 2)

	r, err := NewReader(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadRowGroup(0, []int{0}); err == nil {
		t.Error("ReadRowGroup of a delta on a bytes column: want error, got nil")
	}
	if _, err := ReadColumn[[]byte](r, 0, 0); err == nil {
		t.Error("ReadColumn of a delta on a bytes column: want error, got nil")
	}
	if err := r.ReadRowGroupScoped(0, []int{0}, func(c *Columns) error {
		if _, err := Column[[]byte](c, 0); err == nil {
			return errors.New("Column[bytes] of a delta on a bytes column succeeded, want an error")
		}
		return nil
	}); err != nil {
		t.Errorf("ReadRowGroupScoped of a delta on a bytes column: %v", err)
	}

	// The same delta on the integer column it belongs to reads back exactly, so
	// the failure is the declared type rather than the encoding.
	intSchema := []ColumnSchema{{Name: "i", Type: TypeInt8}}
	intFile := craftFile(chunk, meta, intSchema, 2)
	r2, err := NewReader(bytes.NewReader(intFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.ReadRowGroup(0, []int{0}); err != nil {
		t.Errorf("ReadRowGroup of a delta on an int8 column: %v", err)
	}
	got, err := ReadColumn[int8](r2, 0, 0)
	if err != nil || !reflect.DeepEqual(got, []int8{42, 43}) {
		t.Errorf("ReadColumn[int8] = %v, err %v, want [42 43]", got, err)
	}
}

func TestReaderErrors(t *testing.T) {
	schema := []ColumnSchema{{Name: "i", Type: TypeInt16}}
	chunk := craftChunk(EncPlain, CompressNone, nil, []byte{1, 0, 2, 0})
	meta := ColMeta{ByteLength: int64(len(chunk)), Encoding: EncPlain, Compress: CompressNone}
	good := craftFile(chunk, meta, schema, 2)

	if _, err := NewReader(bytes.NewReader(nil)); err == nil {
		t.Error("NewReader of an empty file: want error, got nil")
	}
	if _, err := NewReader(bytes.NewReader([]byte(magic))); err == nil {
		t.Error("NewReader of a magic-only file: want error, got nil")
	}

	badMagic := []byte(magic)
	badMagic = append(badMagic, formatVersion)
	badMagic = append(badMagic, 0, 0, 0, 0)
	badMagic = append(badMagic, "XXXX"...)
	if _, err := NewReader(bytes.NewReader(badMagic)); err == nil {
		t.Error("NewReader with a bad trailing magic: want error, got nil")
	}

	tooLong := []byte(magic)
	tooLong = append(tooLong, formatVersion)
	tooLong = append(tooLong, 0xFF, 0, 0, 0)
	tooLong = append(tooLong, magic...)
	if _, err := NewReader(bytes.NewReader(tooLong)); err == nil {
		t.Error("NewReader with a footer length beyond the file: want error, got nil")
	}

	// The magic is right but the byte after it is not, which is what a file from
	// another version of this format looks like.
	wrongVersion := craftFile(chunk, meta, schema, 2)
	wrongVersion[len(magic)] = formatVersion + 1
	if _, err := NewReader(bytes.NewReader(wrongVersion)); err == nil {
		t.Error("NewReader of a later format version: want error, got nil")
	}
	corruptLead := craftFile(chunk, meta, schema, 2)
	corruptLead[0] = 'X'
	if _, err := NewReader(bytes.NewReader(corruptLead)); err == nil {
		t.Error("NewReader with a bad leading magic: want error, got nil")
	}

	// The footer length is honest but the bytes behind it are not a footer, so
	// decoding them has to fail rather than yield a schema.
	badFooter := []byte(magic)
	badFooter = append(badFooter, formatVersion)
	badFooter = append(badFooter, 0xDE, 0xAD, 0xBE, 0xEF)
	badFooter = binary.LittleEndian.AppendUint32(badFooter, 4)
	badFooter = append(badFooter, magic...)
	if _, err := NewReader(bytes.NewReader(badFooter)); err == nil {
		t.Error("NewReader with an undecodable footer: want error, got nil")
	}

	corrupt := []byte(magic)
	corrupt = append(corrupt, 0xDE, 0xAD, 0xBE, 0xEF)
	corrupt = binary.LittleEndian.AppendUint32(corrupt, 4)
	corrupt = append(corrupt, magic...)
	if _, err := NewReader(bytes.NewReader(corrupt)); err == nil {
		t.Error("NewReader with a corrupt footer: want error, got nil")
	}

	r, err := NewReader(bytes.NewReader(good))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.ReadRowGroup(1, []int{0}); err == nil {
		t.Error("ReadRowGroup past the last row group: want error, got nil")
	}
	if _, err := r.ReadRowGroup(-1, []int{0}); err == nil {
		t.Error("ReadRowGroup with a negative index: want error, got nil")
	}
	if _, err := r.ReadRowGroup(0, []int{1}); err == nil {
		t.Error("ReadRowGroup with a column index out of range: want error, got nil")
	}
	if _, err := r.ReadRowGroup(0, []int{-1}); err == nil {
		t.Error("ReadRowGroup with a negative column index: want error, got nil")
	}

	zstdChunk := craftChunk(EncPlain, CompressZstd, nil, []byte{1, 0})
	zstdFile := craftFile(zstdChunk, ColMeta{
		ByteLength: int64(len(zstdChunk)),
		Encoding:   EncPlain,
		Compress:   CompressZstd,
	}, schema, 1)
	r, err = NewReader(bytes.NewReader(zstdFile))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.ReadRowGroup(0, []int{0}); err == nil {
		t.Error("ReadRowGroup of a chunk claiming zstd: want error, got nil")
	}

	oddChunk := craftChunk(EncPlain, CompressNone, nil, []byte{1, 2, 3})
	oddFile := craftFile(oddChunk, ColMeta{
		ByteLength: int64(len(oddChunk)),
		Encoding:   EncPlain,
		Compress:   CompressNone,
	}, schema, 3)
	r, err = NewReader(bytes.NewReader(oddFile))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.ReadRowGroup(0, []int{0}); err == nil {
		t.Error("ReadRowGroup of undecodable data: want error, got nil")
	}

	truncated := craftFile(chunk[:5], meta, schema, 2)
	r, err = NewReader(bytes.NewReader(truncated))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.ReadRowGroup(0, []int{0}); err == nil {
		t.Error("ReadRowGroup of a chunk shorter than recorded: want error, got nil")
	}

	// NewReader performs four seeks and four reads: to the end, back to the start
	// for the magic and version, to the trailer and to the footer.
	for _, n := range []int{1, 2, 3, 4} {
		if _, err := NewReader(&flakyReadSeeker{r: bytes.NewReader(good), failSeek: n}); err == nil {
			t.Errorf("NewReader with a seek failing at call %d: want error, got nil", n)
		}
		if _, err := NewReader(&flakyReadSeeker{r: bytes.NewReader(good), failRead: n}); err == nil {
			t.Errorf("NewReader with a read failing at call %d: want error, got nil", n)
		}
	}

	r, err = NewReader(&flakyReadSeeker{r: bytes.NewReader(good), failSeek: 5})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.ReadRowGroup(0, []int{0}); err == nil {
		t.Error("ReadRowGroup with a seek failure: want error, got nil")
	}
}

// TestChunkTruncation covers the chunk reads a file disagreeing with its own
// metadata reaches: a recorded byte length too small for the chunk it describes,
// and one too large for the file behind it.
func TestChunkTruncation(t *testing.T) {
	schema := []ColumnSchema{{Name: "i", Type: TypeInt16}}
	data := []byte{1, 0, 2, 0}

	cases := []struct {
		name       string
		chunk      []byte
		byteLength int64
	}{
		{"shorter than a chunk header", craftChunk(EncPlain, CompressNone, nil, data), 3},
		{"missing null bitmap bytes", craftChunk(EncPlain, CompressNone, make([]byte, 8), data), 20},
		{"missing data bytes", craftChunk(EncPlain, CompressNone, nil, data), 16},
		{"longer than the file", craftChunk(EncPlain, CompressNone, nil, data), 4096},
	}
	for _, c := range cases {
		file := craftFile(c.chunk, ColMeta{
			ByteLength: c.byteLength,
			Encoding:   EncPlain,
			Compress:   CompressNone,
		}, schema, 2)
		r, err := NewReader(bytes.NewReader(file))
		if err != nil {
			t.Fatalf("%s: NewReader: %v", c.name, err)
		}
		if _, err := r.ReadRowGroup(0, []int{0}); err == nil {
			t.Errorf("%s: ReadRowGroup: want error, got nil", c.name)
		}
	}
}

// A chunk read that fails mid-way has to surface rather than returning a
// half-filled region.
func TestReadRowGroupReadFail(t *testing.T) {
	schema := []ColumnSchema{{Name: "i", Type: TypeInt16}}
	chunk := craftChunk(EncPlain, CompressNone, nil, []byte{1, 0, 2, 0})
	meta := ColMeta{ByteLength: int64(len(chunk)), Encoding: EncPlain, Compress: CompressNone}
	file := craftFile(chunk, meta, schema, 2)

	// The footer costs four reads: the leading magic and version, the length, the
	// trailing magic and the footer itself, so the fifth is the first byte of the
	// chunk.
	r, err := NewReader(&flakyReadSeeker{r: bytes.NewReader(file), failRead: len(magic) + 1})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.ReadRowGroup(0, []int{0}); err == nil {
		t.Error("ReadRowGroup with a failing read: want error, got nil")
	}

	// The scoped path reads the same chunks, so a read that fails there has to
	// fail too rather than handing the callback a column it cannot decode.
	r2, err := NewReader(&flakyReadSeeker{r: bytes.NewReader(file), failRead: len(magic) + 1})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if err := r2.ReadRowGroupScoped(0, []int{0}, func(c *Columns) error {
		return nil
	}); err == nil {
		t.Error("ReadRowGroupScoped with a failing read: want error, got nil")
	}

	// A decode that fails reaches the caller through Column rather than through
	// the read, since that is where the column was asked for. A chunk whose data
	// is not a whole number of values is the simplest such file.
	short := craftChunk(EncPlain, CompressNone, nil, []byte{1, 0, 2})
	shortFile := craftFile(short, ColMeta{ByteLength: int64(len(short)), Encoding: EncPlain, Compress: CompressNone},
		[]ColumnSchema{{Name: "i", Type: TypeInt16}}, 2)
	r3, err := NewReader(bytes.NewReader(shortFile))
	if err != nil {
		t.Fatal(err)
	}
	err = r3.ReadRowGroupScoped(0, []int{0}, func(c *Columns) error {
		if _, err := Column[int16](c, 0); err == nil {
			return errors.New("Column of a chunk short of a whole value succeeded, want an error")
		}
		return nil
	})
	if err != nil {
		t.Errorf("ReadRowGroupScoped of a short chunk: %v", err)
	}
}
