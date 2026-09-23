package keine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
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

func TestWriteChunkErrors(t *testing.T) {
	chunk := ColumnChunk{
		Encoding:   EncPlain,
		Compress:   CompressNone,
		NullBitmap: []byte{1},
		Data:       []byte{1, 2, 3, 4},
	}
	for n := 0; n < 6; n++ {
		if err := WriteChunk(&failAfter{n: n}, chunk); err == nil {
			t.Errorf("WriteChunk with a writer failing after %d writes: want error, got nil", n)
		}
	}
}

func TestReadChunkTruncated(t *testing.T) {
	full := []byte{byte(EncPlain), byte(CompressNone)}
	full = binary.LittleEndian.AppendUint32(full, 2)
	full = append(full, 0x01, 0x02)
	full = binary.LittleEndian.AppendUint32(full, 4)
	full = append(full, 0xAA, 0xBB, 0xCC, 0xDD)

	for cut := 0; cut < len(full); cut++ {
		if _, err := ReadChunk(bytes.NewReader(full[:cut])); err == nil {
			t.Errorf("ReadChunk of %d of %d bytes: want error, got nil", cut, len(full))
		}
	}
	if _, err := ReadChunk(bytes.NewReader(full)); err != nil {
		t.Errorf("ReadChunk of a complete chunk: %v", err)
	}
}

func TestCompressCodecs(t *testing.T) {
	data := []byte{1, 2, 3}

	if got, err := Compress(data, CompressNone); err != nil || !bytes.Equal(got, data) {
		t.Errorf("Compress(None) = %v, %v, want %v, nil", got, err, data)
	}
	if got, err := Decompress(data, CompressNone); err != nil || !bytes.Equal(got, data) {
		t.Errorf("Decompress(None) = %v, %v, want %v, nil", got, err, data)
	}

	// Every stdlib codec that can both write and read must round trip, and
	// must actually transform the data: a codec that copies its input would
	// silently mask a broken implementation.
	for _, codec := range []uint8{CompressFlate, CompressGzip, CompressZlib, CompressLzw} {
		compressed, err := Compress(data, codec)
		if err != nil {
			t.Errorf("Compress(%s): %v", codecName(codec), err)
			continue
		}
		if bytes.Equal(compressed, data) {
			t.Errorf("Compress(%s) returned its input unchanged", codecName(codec))
		}
		got, err := Decompress(compressed, codec)
		if err != nil {
			t.Errorf("Decompress(%s): %v", codecName(codec), err)
			continue
		}
		if !bytes.Equal(got, data) {
			t.Errorf("Decompress(%s) = %v, want %v", codecName(codec), got, data)
		}
		// Each codec's stream is self describing, so garbage must be rejected
		// rather than decoded into something plausible.
		if _, err := Decompress([]byte{0xDE, 0xAD, 0xBE, 0xEF}, codec); err == nil {
			t.Errorf("Decompress(%s) of garbage: want error, got nil", codecName(codec))
		}
	}

	if _, err := Compress(data, CompressZstd); err == nil {
		t.Error("Compress with CompressZstd: want error, got nil")
	}
	if _, err := Compress(data, 99); err == nil {
		t.Error("Compress with an unknown codec: want error, got nil")
	}
	if _, err := Decompress(data, CompressZstd); err == nil {
		t.Error("Decompress with CompressZstd: want error, got nil")
	}
	if _, err := Decompress(data, 99); err == nil {
		t.Error("Decompress with an unknown codec: want error, got nil")
	}
}

// TestCompressStreamErrors reaches each write failure inside compressStream.
// Collecting into a failing writer is what makes those branches observable.
// Each codec buffers and flushes on its own schedule, so the sweep tries every
// failure point: a flush during Write hits the Write error path, and the final
// flush at Close hits the Close error path.
func TestCompressStreamErrors(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18)
	for _, codec := range []uint8{CompressFlate, CompressGzip, CompressZlib, CompressLzw} {
		errored := false
		for n := 0; n <= 24; n++ {
			if err := compressStream(&failAfter{n: n}, data, codec); err != nil {
				errored = true
			}
		}
		if !errored {
			t.Errorf("compressStream(%s): no write failure observed across the sweep", codecName(codec))
		}
	}
	if err := compressStream(&failAfter{n: 0}, data, 99); err == nil {
		t.Error("compressStream with an unknown codec: want error, got nil")
	}
}

func TestFooterCodec(t *testing.T) {
	f := Footer{
		Schema: []ColumnSchema{{Name: "a", Type: TypeInt32, Nullable: true}},
		RowGroups: []RowGroupMeta{{
			NumRows:    7,
			ByteOffset: 4,
			Columns: []ColMeta{{
				ByteLength: 12,
				NullCount:  1,
				MinVal:     []byte{1},
				MaxVal:     []byte{9},
				MinLen:     1,
				MaxLen:     4,
				Encoding:   EncPlain,
				Compress:   CompressNone,
			}},
		}},
	}
	got, err := decodeFooter(encodeFooter(f))
	if err != nil {
		t.Fatalf("decodeFooter: %v", err)
	}
	if !reflect.DeepEqual(got, f) {
		t.Errorf("footer round trip = %+v, want %+v", got, f)
	}
	if _, err := decodeFooter([]byte{0xDE, 0xAD, 0xBE, 0xEF}); err == nil {
		t.Error("decodeFooter of garbage: want error, got nil")
	}
}

func TestEncodePlainErrors(t *testing.T) {
	if _, err := EncodePlain(7); err == nil {
		t.Error("EncodePlain of a non-slice: want error, got nil")
	}
	if _, err := EncodePlain([]any{nil}); err == nil {
		t.Error("EncodePlain of a slice holding nil: want error, got nil")
	}
	if _, err := EncodePlain([][]int{{1, 2}}); err == nil {
		t.Error("EncodePlain of a slice of non-byte slices: want error, got nil")
	}
	if _, err := EncodePlain([]func(){func() {}}); err == nil {
		t.Error("EncodePlain of a slice of funcs: want error, got nil")
	}
}

func TestDecodePlainErrors(t *testing.T) {
	if _, err := DecodePlain([]byte{0x01}, TypeString); err == nil {
		t.Error("DecodePlain with a truncated length prefix: want error, got nil")
	}
	tooLong := binary.LittleEndian.AppendUint32(nil, 10)
	tooLong = append(tooLong, 0x01, 0x02)
	if _, err := DecodePlain(tooLong, TypeString); err == nil {
		t.Error("DecodePlain with a length beyond the data: want error, got nil")
	}
	if _, err := DecodePlain(nil, 0xFF); err == nil {
		t.Error("DecodePlain with an unknown type: want error, got nil")
	}
	if _, err := DecodePlain([]byte{1, 2, 3}, TypeInt16); err == nil {
		t.Error("DecodePlain of a non-multiple length: want error, got nil")
	}
}

func TestDecodePlainBytes(t *testing.T) {
	encoded, err := EncodePlain([]any{[]byte{1, 2, 3}, "hello"})
	if err != nil {
		t.Fatalf("EncodePlain: %v", err)
	}
	got, err := DecodePlain(encoded, TypeBytes)
	if err != nil {
		t.Fatalf("DecodePlain: %v", err)
	}
	if len(got) != 2 || !bytes.Equal(got[0].([]byte), []byte{1, 2, 3}) || !bytes.Equal(got[1].([]byte), []byte("hello")) {
		t.Errorf("DecodePlain(TypeBytes) = %v", got)
	}
}

func TestBitmapAndRLEBounds(t *testing.T) {
	// More values requested than bits available: the tail stays unset.
	got := DecodeBitmap([]byte{0x01}, 20)
	want := make([]bool, 20)
	want[0] = true
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecodeBitmap = %v, want %v", got, want)
	}

	gotRLE := DecodeRLEBitpack([]byte{0xFF}, 20)
	wantRLE := make([]bool, 20)
	for i := 0; i < 8; i++ {
		wantRLE[i] = true
	}
	if !reflect.DeepEqual(gotRLE, wantRLE) {
		t.Errorf("DecodeRLEBitpack = %v, want %v", gotRLE, wantRLE)
	}
}

func TestDecodeDeltaErrors(t *testing.T) {
	if _, err := DecodeDelta([]byte{1, 2, 3}); err == nil {
		t.Error("DecodeDelta of a non-multiple length: want error, got nil")
	}
}

func TestDecodeOffsetBytesErrors(t *testing.T) {
	if _, err := DecodeOffsetBytes([]byte{1, 2, 3}); err == nil {
		t.Error("DecodeOffsetBytes with fewer than 4 bytes: want error, got nil")
	}
	claimsFive := binary.LittleEndian.AppendUint32(nil, 5)
	claimsFive = append(claimsFive, 1, 2, 3)
	if _, err := DecodeOffsetBytes(claimsFive); err == nil {
		t.Error("DecodeOffsetBytes with a truncated header: want error, got nil")
	}
	invalid := binary.LittleEndian.AppendUint32(nil, 2)
	invalid = binary.LittleEndian.AppendUint32(invalid, 100)
	invalid = binary.LittleEndian.AppendUint32(invalid, 0)
	if _, err := DecodeOffsetBytes(invalid); err == nil {
		t.Error("DecodeOffsetBytes with an out of range offset: want error, got nil")
	}

	encoded := EncodeOffsetBytes([][]byte{{1, 2}, {}, {3}})
	got, err := DecodeOffsetBytes(encoded)
	if err != nil {
		t.Fatalf("DecodeOffsetBytes: %v", err)
	}
	want := [][]byte{{1, 2}, nil, {3}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecodeOffsetBytes = %v, want %v", got, want)
	}
}

func TestDecodeDictErrors(t *testing.T) {
	if _, err := DecodeDict([]byte{1, 2, 3}); err == nil {
		t.Error("DecodeDict with fewer than 12 bytes: want error, got nil")
	}

	var b []byte
	// Indices claim more bytes than the data holds.
	b = binary.LittleEndian.AppendUint32(b, 100)
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 8)
	if _, err := DecodeDict(b); err == nil {
		t.Error("DecodeDict with truncated indices: want error, got nil")
	}

	// Corrupt dictionary blob.
	b = b[:0]
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, 5, 0, 0, 0, 1, 2)
	if _, err := DecodeDict(b); err == nil {
		t.Error("DecodeDict with a corrupt dictionary blob: want error, got nil")
	}

	// Header claims more entries than the dictionary holds.
	b = b[:0]
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 7)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, 0, 0, 0, 0)
	if _, err := DecodeDict(b); err == nil {
		t.Error("DecodeDict with a mismatched dictionary size: want error, got nil")
	}

	// An index past the end of the dictionary.
	b = b[:0]
	b = binary.LittleEndian.AppendUint32(b, 2)
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = append(b, 0x80)                   // indices: 1, 0
	b = append(b, 1, 0, 0, 0, 0, 0, 0, 0) // one entry, empty raw bytes
	if _, err := DecodeDict(b); err == nil {
		t.Error("DecodeDict with an out of range index: want error, got nil")
	}
}

func TestDictSingleEntry(t *testing.T) {
	encoded, err := EncodeDict([]any{"x", "x", "x"})
	if err != nil {
		t.Fatalf("EncodeDict: %v", err)
	}
	got, err := DecodeDict(encoded)
	if err != nil {
		t.Fatalf("DecodeDict: %v", err)
	}
	if !reflect.DeepEqual(got, []any{"x", "x", "x"}) {
		t.Errorf("DecodeDict = %v", got)
	}
	if _, err := EncodeDict("not a slice"); err == nil {
		t.Error("EncodeDict of a non-slice: want error, got nil")
	}
}

func TestBitpackRoundTrip(t *testing.T) {
	indices := []uint32{0, 1, 2, 3, 4, 5, 6, 7, 0, 7, 3}
	got := bitunpack(bitpackIndices(indices, 3), len(indices), 3)
	if !reflect.DeepEqual(got, indices) {
		t.Errorf("bitpack round trip = %v, want %v", got, indices)
	}
	if len(bitpackIndices([]uint32{1, 2}, 0)) != 0 {
		t.Error("bitpackIndices with zero bits should return no bytes")
	}
}

func TestCoerceBool(t *testing.T) {
	cases := []struct {
		in      any
		want    bool
		wantErr bool
	}{
		{true, true, false},
		{false, false, false},
		{int64(1), true, false},
		{int64(0), false, false},
		{"true", true, false},
		{"false", false, false},
		{"not a bool", false, true},
		{[]byte("true"), true, false},
		{[]byte("nope"), false, true},
		{nil, false, true},
		{struct{}{}, false, true},
	}
	for _, c := range cases {
		got, err := coerceBool(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("coerceBool(%T): want error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("coerceBool(%T): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceBool(%T) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCoerceInt(t *testing.T) {
	ok := []struct {
		in   any
		want int64
	}{
		{int64(5), 5}, {int(5), 5}, {int8(5), 5}, {int16(5), 5}, {int32(5), 5},
		{uint(5), 5}, {uint8(5), 5}, {uint16(5), 5}, {uint32(5), 5}, {uint64(5), 5},
		{float32(5.9), 5}, {float64(5.9), 5},
		{true, 1}, {false, 0},
		{"42", 42}, {[]byte("42"), 42},
	}
	for _, c := range ok {
		got, err := coerceInt(c.in, 64)
		if err != nil {
			t.Errorf("coerceInt(%T): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceInt(%T) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, in := range []any{"not a number", []byte("nope"), nil, struct{}{}} {
		if _, err := coerceInt(in, 64); err == nil {
			t.Errorf("coerceInt(%T): want error, got nil", in)
		}
	}
	if _, err := coerceInt("99999999999999999999", 64); err == nil {
		t.Error("coerceInt of an overflowing string: want error, got nil")
	}
}

func TestCoerceUint(t *testing.T) {
	ok := []struct {
		in   any
		want uint64
	}{
		{uint64(5), 5}, {uint(5), 5}, {uint8(5), 5}, {uint16(5), 5}, {uint32(5), 5},
		{int64(5), 5}, {int(5), 5}, {int8(5), 5}, {int16(5), 5}, {int32(5), 5},
		{float32(5.9), 5}, {float64(5.9), 5},
		{true, 1}, {false, 0},
		{"42", 42}, {[]byte("42"), 42},
	}
	for _, c := range ok {
		got, err := coerceUint(c.in, 64)
		if err != nil {
			t.Errorf("coerceUint(%T): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceUint(%T) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, in := range []any{"-1", []byte("nope"), nil, struct{}{}} {
		if _, err := coerceUint(in, 64); err == nil {
			t.Errorf("coerceUint(%T): want error, got nil", in)
		}
	}
}

func TestCoerceFloat(t *testing.T) {
	ok := []struct {
		in   any
		want float64
	}{
		{float64(1.5), 1.5}, {float32(1.5), 1.5},
		{int64(2), 2}, {int(2), 2}, {int8(2), 2}, {int16(2), 2}, {int32(2), 2},
		{uint64(2), 2}, {uint(2), 2}, {uint8(2), 2}, {uint16(2), 2}, {uint32(2), 2},
		{true, 1}, {false, 0},
		{"1.5", 1.5}, {[]byte("1.5"), 1.5},
	}
	for _, c := range ok {
		got, err := coerceFloat(c.in, 64)
		if err != nil {
			t.Errorf("coerceFloat(%T): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceFloat(%T) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, in := range []any{"not a number", []byte("nope"), nil, struct{}{}} {
		if _, err := coerceFloat(in, 64); err == nil {
			t.Errorf("coerceFloat(%T): want error, got nil", in)
		}
	}
}

func TestCoerceString(t *testing.T) {
	ok := []struct {
		in   any
		want string
	}{
		{"abc", "abc"}, {[]byte("abc"), "abc"},
		{int64(-5), "-5"}, {uint64(5), "5"}, {float64(5.5), "5.5"},
		{true, "true"}, {false, "false"},
	}
	for _, c := range ok {
		got, err := coerceString(c.in)
		if err != nil {
			t.Errorf("coerceString(%T): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("coerceString(%T) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, in := range []any{nil, struct{}{}} {
		if _, err := coerceString(in); err == nil {
			t.Errorf("coerceString(%T): want error, got nil", in)
		}
	}
}

func TestCoerceBytesAndParse(t *testing.T) {
	raw := []byte{1, 2, 3}
	got, err := coerceBytes(raw)
	if err != nil {
		t.Fatalf("coerceBytes: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("coerceBytes = %v, want %v", got, raw)
	}
	// The input slice must not be shared with the result.
	got[0] = 9
	if raw[0] != 1 {
		t.Errorf("coerceBytes returned a slice aliased with its input")
	}

	cases := []struct {
		in      string
		want    []byte
		wantErr bool
	}{
		{"[]", []byte{}, false},
		{"[1 2 3]", []byte{1, 2, 3}, false},
		{"garbage", nil, true},
		{"[999]", nil, true},
		{"[abc]", nil, true},
	}
	for _, c := range cases {
		got, err := parseBytesList(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseBytesList(%q): want error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBytesList(%q): %v", c.in, err)
			continue
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("parseBytesList(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, in := range []any{"garbage", nil, struct{}{}} {
		if _, err := coerceBytes(in); err == nil {
			t.Errorf("coerceBytes(%T): want error, got nil", in)
		}
	}
}

func TestCanonicalValues(t *testing.T) {
	ok := []struct {
		v    any
		typ  uint8
		want any
	}{
		{true, TypeBool, true},
		{int64(-3), TypeInt8, int8(-3)},
		{int64(-3), TypeInt16, int16(-3)},
		{int64(-3), TypeInt32, int32(-3)},
		{int64(-3), TypeInt64, int64(-3)},
		{uint64(3), TypeUint8, uint8(3)},
		{uint64(3), TypeUint16, uint16(3)},
		{uint64(3), TypeUint32, uint32(3)},
		{uint64(3), TypeUint64, uint64(3)},
		{float64(1.5), TypeFloat32, float32(1.5)},
		{float64(1.5), TypeFloat64, float64(1.5)},
		{"abc", TypeString, "abc"},
		{[]byte{1, 2}, TypeBytes, []byte{1, 2}},
	}
	for _, c := range ok {
		got, err := canonicalValue(c.v, c.typ)
		if err != nil {
			t.Errorf("canonicalValue(%T, %d): %v", c.v, c.typ, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("canonicalValue(%T, %d) = %v, want %v", c.v, c.typ, got, c.want)
		}
	}

	for _, c := range []struct {
		v   any
		typ uint8
	}{
		{"300", TypeInt8},
		{"99999", TypeInt16},
		{"99999999999", TypeInt32},
		{"-1", TypeUint8},
		{"-1", TypeUint16},
		{"-1", TypeUint32},
		{"notafloat", TypeFloat32},
		{nil, TypeBool},
		{nil, TypeInt64},
		{nil, TypeUint64},
		{nil, TypeFloat64},
		{nil, TypeString},
		{nil, TypeBytes},
		{nil, 0xFF},
	} {
		if _, err := canonicalValue(c.v, c.typ); err == nil {
			t.Errorf("canonicalValue(%T, %d): want error, got nil", c.v, c.typ)
		}
	}
	if _, err := canonicalColumn([]any{"x"}, TypeInt8); err == nil {
		t.Error("canonicalColumn with an uncoercible value: want error, got nil")
	}
}

func TestEncodeWithErrors(t *testing.T) {
	for _, c := range []struct {
		enc   uint8
		typed any
		typ   uint8
	}{
		{EncPlain, []any{nil}, TypeInt8},
		{EncRLEBitpack, []string{"x"}, TypeBool},
		{EncDelta, []string{"x"}, TypeInt64},
		{EncOffsetBytes, []string{"x"}, TypeBytes},
		{99, []int8{1}, TypeInt8},
	} {
		if _, err := encodeWith(c.enc, c.typed, c.typ); err == nil {
			t.Errorf("encodeWith(%d, %T, %d): want error, got nil", c.enc, c.typed, c.typ)
		}
	}
}

func TestDecodeWithErrors(t *testing.T) {
	for _, c := range []struct {
		enc  uint8
		data []byte
		typ  uint8
	}{
		{EncPlain, []byte{0x01}, TypeString},
		{EncDelta, []byte{1, 2, 3}, TypeInt64},
		{EncDict, []byte{1, 2}, TypeString},
		{EncOffsetBytes, []byte{1, 2}, TypeBytes},
		{99, nil, TypeInt8},
	} {
		if _, err := decodeWith(c.enc, c.data, 1, c.typ); err == nil {
			t.Errorf("decodeWith(%d, ..., %d): want error, got nil", c.enc, c.typ)
		}
	}
}

func TestLayoutHelpers(t *testing.T) {
	if got := layoutCandidates(0xFF); got != nil {
		t.Errorf("layoutCandidates of an unknown type = %v, want nil", got)
	}
	if got := encName(99); got != "Enc99" {
		t.Errorf("encName(99) = %q, want %q", got, "Enc99")
	}
	if got := codecName(CompressZstd); got != "Zstd" {
		t.Errorf("codecName(CompressZstd) = %q, want %q", got, "Zstd")
	}
	for _, c := range []struct {
		codec uint8
		name  string
	}{
		{CompressFlate, "Flate"},
		{CompressGzip, "Gzip"},
		{CompressZlib, "Zlib"},
		{CompressLzw, "Lzw"},
	} {
		if got := codecName(c.codec); got != c.name {
			t.Errorf("codecName(%d) = %q, want %q", c.codec, got, c.name)
		}
	}
	if got := codecName(99); got != "Codec99" {
		t.Errorf("codecName(99) = %q, want %q", got, "Codec99")
	}

	// No candidate is valid for an unknown type, so ExperimentLayouts falls
	// back to Plain+None.
	best := ExperimentLayouts([]any{}, ColumnSchema{Name: "x", Type: 0xFF})
	if best.Encoding != EncPlain || best.Compress != CompressNone {
		t.Errorf("ExperimentLayouts fallback = %+v, want Plain+None", best)
	}

	if _, ok := measureLayout([]any{int8(1)}, ColumnSchema{Name: "x", Type: TypeInt8}, EncPlain, CompressZstd); ok {
		t.Error("measureLayout with a codec that cannot compress: want not ok")
	}
	if _, ok := measureLayout([]any{int8(1)}, ColumnSchema{Name: "x", Type: TypeInt8}, EncPlain, CompressFlate); !ok {
		t.Error("measureLayout with CompressFlate: want ok")
	}
	if _, err := canonicalColumnTyped([]any{struct{}{}}, TypeBool); err == nil {
		t.Error("canonicalColumnTyped of an uncoercible value: want error, got nil")
	}
	if _, ok := measureLayout([]string{"x"}, ColumnSchema{Name: "x", Type: TypeBool}, EncRLEBitpack, CompressNone); ok {
		t.Error("measureLayout with a column of the wrong Go type: want not ok")
	}

	// Dictionary encoding accepts any value (it keys on the fmt form), but the
	// decoded strings must convert back to the column type. Strings that do
	// not parse as integers make the round trip fail, which is what the decode
	// check in measureLayout exists to catch.
	if _, ok := measureLayout([]string{"abc", "def"}, ColumnSchema{Name: "x", Type: TypeInt32}, EncDict, CompressNone); ok {
		t.Error("measureLayout with values that cannot be decoded back: want not ok")
	}
}

func TestWriterErrors(t *testing.T) {
	schema := []ColumnSchema{{Name: "b", Type: TypeBool}}
	twoCols := []ColumnSchema{
		{Name: "a", Type: TypeBool},
		{Name: "b", Type: TypeBool},
	}

	// A failure writing the leading magic surfaces on first use.
	w := NewWriter(&failAfter{n: 0}, schema)
	if err := w.AddRowGroup([][]any{{true, false}}); err == nil {
		t.Error("AddRowGroup after a failed magic write: want error, got nil")
	}
	if err := w.Close(); err == nil {
		t.Error("Close after a failed magic write: want error, got nil")
	}

	w = NewWriter(io.Discard, schema)
	if err := w.AddRowGroup([][]any{{true}, {false}}); err == nil {
		t.Error("AddRowGroup with the wrong column count: want error, got nil")
	}

	w = NewWriter(io.Discard, twoCols)
	if err := w.AddRowGroup([][]any{{true, false}, {true}}); err == nil {
		t.Error("AddRowGroup with mismatched row counts: want error, got nil")
	}

	// Every candidate encoding rejects these values, so the chosen layout
	// cannot encode them either.
	w = NewWriter(io.Discard, schema)
	if err := w.AddRowGroup([][]any{{struct{}{}, struct{}{}}}); err == nil {
		t.Error("AddRowGroup of unencodable values: want error, got nil")
	}

	// Dictionary encoding accepts these but the statistics cannot be built.
	bytesSchema := []ColumnSchema{{Name: "c", Type: TypeBytes}}
	w = NewWriter(io.Discard, bytesSchema)
	if err := w.AddRowGroup([][]any{{42, 43}}); err == nil {
		t.Error("AddRowGroup of values that break statistics: want error, got nil")
	}

	// The underlying writer fails partway through the chunk.
	w = NewWriter(&failAfter{n: 1}, schema)
	if err := w.AddRowGroup([][]any{{true, false}}); err == nil {
		t.Error("AddRowGroup with a failing writer: want error, got nil")
	}

	// Close failing at each of its three writes.
	for n := 1; n <= 3; n++ {
		w := NewWriter(&failAfter{n: n}, nil)
		if err := w.AddRowGroup(nil); err != nil {
			t.Fatalf("AddRowGroup of an empty row group: %v", err)
		}
		if err := w.Close(); err == nil {
			t.Errorf("Close with a writer failing after %d writes: want error, got nil", n)
		}
	}
}

func TestFillStats(t *testing.T) {
	// Byte slices compare as raw bytes, so the widest value is also the
	// longest one.
	bb, err := canonicalColumnTyped([]any{
		[]byte{1, 2, 3}, []byte{1}, []byte{1, 2, 3, 4, 5},
	}, TypeBytes)
	if err != nil {
		t.Fatalf("canonicalColumnTyped: %v", err)
	}
	var meta ColMeta
	fillStatsTyped(&meta, bb)
	if !bytes.Equal(meta.MinVal, []byte{1}) || !bytes.Equal(meta.MaxVal, []byte{1, 2, 3, 4, 5}) {
		t.Errorf("fillStatsTyped min = %v, max = %v", meta.MinVal, meta.MaxVal)
	}
	if meta.MinLen != 1 || meta.MaxLen != 5 {
		t.Errorf("fillStatsTyped MinLen = %d, MaxLen = %d, want 1 and 5", meta.MinLen, meta.MaxLen)
	}

	// Strings compare as strings but are stored as bytes, and their lengths
	// vary independently of their order: "a" is smallest, "ccc" largest, and
	// "bb" is neither.
	ss, err := canonicalColumnTyped([]any{"ccc", "a", "bb", "a"}, TypeString)
	if err != nil {
		t.Fatalf("canonicalColumnTyped: %v", err)
	}
	var strMeta ColMeta
	fillStatsTyped(&strMeta, ss)
	if !bytes.Equal(strMeta.MinVal, []byte("a")) || !bytes.Equal(strMeta.MaxVal, []byte("ccc")) {
		t.Errorf("fillStatsTyped string min = %q, max = %q", strMeta.MinVal, strMeta.MaxVal)
	}
	if strMeta.MinLen != 1 || strMeta.MaxLen != 3 {
		t.Errorf("fillStatsTyped string MinLen = %d, MaxLen = %d, want 1 and 3",
			strMeta.MinLen, strMeta.MaxLen)
	}

	// An empty column leaves the statistics unset.
	var empty ColMeta
	fillStatsTyped(&empty, []string(nil))
	if empty.MinVal != nil || empty.MaxVal != nil || empty.MinLen != 0 || empty.MaxLen != 0 {
		t.Errorf("fillStatsTyped of no values left statistics set: %+v", empty)
	}
	if _, err := canonicalColumnTyped([]any{nil}, TypeBool); err == nil {
		t.Error("canonicalColumnTyped of nil: want error, got nil")
	}
}

// craftChunk assembles the on-disk bytes of one column chunk.
func craftChunk(enc, codec uint8, bitmap, data []byte) []byte {
	var b []byte
	b = append(b, enc, codec)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(bitmap)))
	b = append(b, bitmap...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = append(b, data...)
	return b
}

// craftFile wraps a hand-built chunk in a complete keine file.
func craftFile(chunk []byte, meta ColMeta, schema []ColumnSchema, numRows uint32) []byte {
	var f []byte
	f = append(f, magic...)
	f = append(f, chunk...)
	fb := encodeFooter(Footer{
		Schema: schema,
		RowGroups: []RowGroupMeta{{
			NumRows:    numRows,
			ByteOffset: int64(len(magic)),
			Columns:    []ColMeta{meta},
		}},
	})
	f = append(f, fb...)
	f = binary.LittleEndian.AppendUint32(f, uint32(len(fb)))
	f = append(f, magic...)
	return f
}

// TestCanonicalReadErrors covers the read paths that narrow decoded values to
// the declared type, which only a file whose chunk disagrees with its schema
// can reach: a dictionary holding a value the column cannot hold.
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
	badMagic = append(badMagic, 0, 0, 0, 0)
	badMagic = append(badMagic, "XXXX"...)
	if _, err := NewReader(bytes.NewReader(badMagic)); err == nil {
		t.Error("NewReader with a bad trailing magic: want error, got nil")
	}

	tooLong := []byte(magic)
	tooLong = append(tooLong, 0xFF, 0, 0, 0)
	tooLong = append(tooLong, magic...)
	if _, err := NewReader(bytes.NewReader(tooLong)); err == nil {
		t.Error("NewReader with a footer length beyond the file: want error, got nil")
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

	for _, n := range []int{1, 2, 3} {
		if _, err := NewReader(&flakyReadSeeker{r: bytes.NewReader(good), failSeek: n}); err == nil {
			t.Errorf("NewReader with a seek failing at call %d: want error, got nil", n)
		}
		if _, err := NewReader(&flakyReadSeeker{r: bytes.NewReader(good), failRead: n}); err == nil {
			t.Errorf("NewReader with a read failing at call %d: want error, got nil", n)
		}
	}

	r, err = NewReader(&flakyReadSeeker{r: bytes.NewReader(good), failSeek: 4})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.ReadRowGroup(0, []int{0}); err == nil {
		t.Error("ReadRowGroup with a seek failure: want error, got nil")
	}
}
