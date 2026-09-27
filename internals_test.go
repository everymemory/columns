package keine

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
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

// TestDecompressBlock covers two checks a well-formed file never trips: a block
// that runs past the column's buffer, and one that does not decompress to its
// recorded length. Both must fail rather than leave the decoder reading part of
// a column.
func TestDecompressBlock(t *testing.T) {
	raw := make([]byte, 8)

	past := ColumnBlock{RawLength: 4, Data: []byte{1, 2, 3, 4}}
	if err := decompressBlock(raw, 6, past, CompressNone); err == nil {
		t.Error("decompressBlock past the buffer: want error, got nil")
	}
	short := ColumnBlock{RawLength: 4, Data: []byte{1, 2}}
	if err := decompressBlock(raw, 0, short, CompressNone); err == nil {
		t.Error("decompressBlock of a block shorter than its raw length: want error, got nil")
	}
	long := ColumnBlock{RawLength: 2, Data: []byte{1, 2, 3, 4}}
	if err := decompressBlock(raw, 0, long, CompressNone); err == nil {
		t.Error("decompressBlock of a block longer than its raw length: want error, got nil")
	}

	fits := ColumnBlock{RawLength: 4, Data: []byte{1, 2, 3, 4}}
	if err := decompressBlock(raw, 4, fits, CompressNone); err != nil {
		t.Errorf("decompressBlock of a fitting block: %v", err)
	}
}

// TestDecompressBlockTruncated reaches the read failure inside readAllInto. A
// stream cut mid-decompress fails differently from one that ends cleanly short
// of the space set aside for it, so the bounds and length checks in
// decompressBlock do not cover it.
func TestDecompressBlockTruncated(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 2000)
	compressed, err := Compress(data, CompressFlate)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	// Cutting the stream in half takes the terminating block with it, so the
	// reader fails rather than reporting an early end.
	cut := compressed[:len(compressed)/2]

	region := make([]byte, len(data))
	blk := ColumnBlock{RawLength: uint32(len(data)), Data: cut}
	if err := decompressBlock(region, 0, blk, CompressFlate); err == nil {
		t.Error("decompressBlock of a truncated stream: want error, got nil")
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

	// Every stdlib codec that can read and write must round trip, and must
	// change the bytes: a codec that returned its input would hide a broken
	// implementation.
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

// TestPooledFlateReaderResetError reaches the one failure a pooled decompressor
// can report. A real flate reader never fails a reset, so the test puts one in
// the pool that does. decompressInto must return that error and keep that reader
// out of the pool, where it would fail every read after it.
func TestPooledFlateReaderResetError(t *testing.T) {
	flateReaders.Put(&failingResetReader{})
	if _, err := decompressInto(nil, []byte{1, 2, 3}, CompressFlate); err == nil {
		t.Error("decompressInto with a reader that cannot reset: want error, got nil")
	}
	if got, ok := flateReaders.Get().(*failingResetReader); ok {
		t.Errorf("the reader that failed to reset went back into the pool: %v", got)
	}
}

type failingResetReader struct{}

func (failingResetReader) Read([]byte) (int, error) { return 0, io.EOF }
func (failingResetReader) Close() error             { return nil }
func (failingResetReader) Reset(io.Reader, []byte) error {
	return fmt.Errorf("keine test: reset refused")
}

// TestCompressStreamErrors reaches each write failure inside compressStream by
// collecting into a writer that fails. Each codec buffers and flushes on its own
// schedule, so the sweep tries every failure point: a flush during Write hits
// the Write error path, and the final flush at Close hits the Close error path.
func TestCompressStreamErrors(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18)
	for _, codec := range []uint8{CompressFlate, CompressGzip, CompressZlib, CompressLzw} {
		errored := false
		for n := 0; n <= 24; n++ {
			if err := compressStream(&failAfter{n: n}, data, codec, flateLevel); err != nil {
				errored = true
			}
		}
		if !errored {
			t.Errorf("compressStream(%s): no write failure observed across the sweep", codecName(codec))
		}
	}
	if err := compressStream(&failAfter{n: 0}, data, 99, flateLevel); err == nil {
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
	if !reflect.DeepEqual(got, [][]byte{{'x'}, {'x'}, {'x'}}) {
		t.Errorf("DecodeDict = %v", got)
	}
	if _, err := EncodeDict("not a slice"); err == nil {
		t.Error("EncodeDict of a non-slice: want error, got nil")
	}
	if _, err := EncodeDict([]any{1, "x"}); err == nil {
		t.Error("EncodeDict of mixed kinds: want error, got nil")
	}
	if _, err := EncodeDict([]any{[]int32{1}}); err == nil {
		t.Error("EncodeDict of a slice of slices: want error, got nil")
	}
	if _, err := EncodeDict([]any{[]byte("x"), []byte("y")}); err != nil {
		t.Errorf("EncodeDict of any holding byte slices: %v", err)
	}
}

// TestDictTypedRoundTrip covers the dictionary over every fixed width type,
// where entries hold the values' little-endian form, and over the two variable
// length types, where they hold the values' bytes.
func TestDictTypedRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		typ  uint8
		vals any
	}{
		{"bool", TypeBool, []bool{true, false, true, false, true}},
		{"int8", TypeInt8, []int8{1, 2, 3, 1, 2, 3}},
		{"int16", TypeInt16, []int16{1, 2, 3, 1, 2, 3}},
		{"int32", TypeInt32, []int32{1, 2, 3, 1, 2, 3}},
		{"int64", TypeInt64, []int64{1, 2, 3, 1, 2, 3}},
		{"uint8", TypeUint8, []uint8{1, 2, 3, 1, 2, 3}},
		{"uint16", TypeUint16, []uint16{1, 2, 3, 1, 2, 3}},
		{"uint32", TypeUint32, []uint32{1, 2, 3, 1, 2, 3}},
		{"uint64", TypeUint64, []uint64{1, 2, 3, 1, 2, 3}},
		{"float32", TypeFloat32, []float32{1.5, 2.5, 1.5, 2.5}},
		{"float64", TypeFloat64, []float64{1.5, 2.5, 1.5, 2.5}},
		{"string", TypeString, []string{"a", "b", "a", "b"}},
		{"bytes", TypeBytes, [][]byte{{1}, {2}, {1}, {2}}},
	}
	for _, c := range cases {
		encoded, err := EncodeDict(c.vals)
		if err != nil {
			t.Fatalf("%s: EncodeDict: %v", c.name, err)
		}
		got, err := decodeDictTyped(encoded, c.typ, &dest{})
		if err != nil {
			t.Fatalf("%s: decodeDictTyped: %v", c.name, err)
		}
		if !reflect.DeepEqual(got, c.vals) {
			t.Errorf("%s: round trip = %v, want %v", c.name, got, c.vals)
		}
	}

	// The same chunk read as part of a file goes through the reader's narrowing
	// path, which reads it into the declared type rather than rejecting it.
	int32vals := []int32{10, 20, 10, 20}
	data, err := EncodeDict(int32vals)
	if err != nil {
		t.Fatal(err)
	}
	chunk := craftChunk(EncDict, CompressNone, nil, data)
	meta := ColMeta{ByteLength: int64(len(chunk)), Encoding: EncDict, Compress: CompressNone}
	schema := []ColumnSchema{{Name: "i", Type: TypeInt32}}
	file := craftFile(chunk, meta, schema, uint32(len(int32vals)))
	r, err := NewReader(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	cols, err := r.ReadRowGroup(0, []int{0})
	if err != nil {
		t.Fatalf("ReadRowGroup: %v", err)
	}
	want := make([]any, len(int32vals))
	for i, v := range int32vals {
		want[i] = v
	}
	if !reflect.DeepEqual(cols[0], want) {
		t.Errorf("ReadRowGroup of a dict int32 column = %v, want %v", cols[0], want)
	}
	got, err := ReadColumn[int32](r, 0, 0)
	if err != nil {
		t.Fatalf("ReadColumn[int32]: %v", err)
	}
	if !reflect.DeepEqual(got, int32vals) {
		t.Errorf("ReadColumn[int32] = %v, want %v", got, int32vals)
	}
}

// TestDictTypedErrors covers the dictionary reads a chunk that disagrees with
// its own schema reaches on a fixed width column: an index with no entry, and an
// entry that is not the width the type declares.
func TestDictTypedErrors(t *testing.T) {
	// One entry, two indices, the second pointing past it.
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, 2)
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = append(b, 0x80)                   // indices: 1, 0
	b = append(b, 1, 0, 0, 0, 0, 0, 0, 0) // one eight byte entry
	if _, err := decodeDictTyped(b, TypeInt64, &dest{}); err == nil {
		t.Error("decodeDictTyped with an out of range index: want error, got nil")
	}

	// An entry that is not the width of the type every other value has.
	entries, err := EncodeDict([]int16{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeDictTyped(entries, TypeInt64, &dest{}); err == nil {
		t.Error("decodeDictTyped with entries of the wrong width: want error, got nil")
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

// TestBitunpackClearsDestination decodes twice into the same destination. The
// first call fills it with nonzero indices, the second decodes a one-entry
// dictionary that takes no bits per index and so writes nothing: without a
// clear, every index keeps the value the first call left behind.
func TestBitunpackClearsDestination(t *testing.T) {
	indices := []uint32{0, 1, 2, 3, 4, 5, 6, 7, 0, 7, 3}
	d := &dest{}
	got := decodeBitunpack(bitpackIndices(indices, 3), len(indices), 3, d)
	if !reflect.DeepEqual(got, indices) {
		t.Fatalf("bitpack round trip into a destination = %v, want %v", got, indices)
	}

	zero := make([]uint32, len(indices))
	got = decodeBitunpack(nil, len(indices), 0, d)
	if !reflect.DeepEqual(got, zero) {
		t.Errorf("bitunpack of zero bits into a used destination = %v, want all zero", got)
	}
}

// TestRLEBitpackClearsDestination makes the same check for the bitmap decoder,
// which sets only the true bits and leaves the false ones at whatever the
// destination already held.
func TestRLEBitpackClearsDestination(t *testing.T) {
	const n = 16
	d := &dest{}
	if got := decodeRLEBitpack([]byte{0xFF, 0xFF}, n, d); !allTrue(got) {
		t.Fatalf("bitmap of all true bits = %v, want all true", got)
	}
	if got := decodeRLEBitpack(nil, n, d); allTrue(got) {
		t.Errorf("bitmap of no bits into a used destination = %v, want all false", got)
	}
}

func allTrue(b []bool) bool {
	for _, v := range b {
		if !v {
			return false
		}
	}
	return len(b) > 0
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

// canonicalValue coerces one value to the Go type typ implies. Only this test
// file asks for a single value; the writer and reader coerce whole columns.
func canonicalValue(v any, typ uint8) (any, error) {
	switch typ {
	case TypeBool:
		return coerceBool(v)
	case TypeInt8:
		n, err := coerceInt(v, 8)
		if err != nil {
			return nil, err
		}
		return int8(n), nil
	case TypeInt16:
		n, err := coerceInt(v, 16)
		if err != nil {
			return nil, err
		}
		return int16(n), nil
	case TypeInt32:
		n, err := coerceInt(v, 32)
		if err != nil {
			return nil, err
		}
		return int32(n), nil
	case TypeInt64:
		return coerceInt(v, 64)
	case TypeUint8:
		n, err := coerceUint(v, 8)
		if err != nil {
			return nil, err
		}
		return uint8(n), nil
	case TypeUint16:
		n, err := coerceUint(v, 16)
		if err != nil {
			return nil, err
		}
		return uint16(n), nil
	case TypeUint32:
		n, err := coerceUint(v, 32)
		if err != nil {
			return nil, err
		}
		return uint32(n), nil
	case TypeUint64:
		return coerceUint(v, 64)
	case TypeFloat32:
		f, err := coerceFloat(v, 32)
		if err != nil {
			return nil, err
		}
		return float32(f), nil
	case TypeFloat64:
		return coerceFloat(v, 64)
	case TypeString:
		return coerceString(v)
	case TypeBytes:
		return coerceBytes(v)
	default:
		return nil, fmt.Errorf("keine: unknown type %d", typ)
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

	if _, ok := measureLayout([]any{int8(1)}, ColumnSchema{Name: "x", Type: TypeInt8}, EncPlain, CompressZstd, flateLevel, &measureScratch{}); ok {
		t.Error("measureLayout with a codec that cannot compress: want not ok")
	}
	if _, ok := measureLayout([]any{int8(1)}, ColumnSchema{Name: "x", Type: TypeInt8}, EncPlain, CompressFlate, flateLevel, &measureScratch{}); !ok {
		t.Error("measureLayout with CompressFlate: want ok")
	}
	if _, err := canonicalColumnTyped([]any{struct{}{}}, TypeBool); err == nil {
		t.Error("canonicalColumnTyped of an uncoercible value: want error, got nil")
	}
	if _, ok := measureLayout([]string{"x"}, ColumnSchema{Name: "x", Type: TypeBool}, EncRLEBitpack, CompressNone, flateLevel, &measureScratch{}); ok {
		t.Error("measureLayout with a column of the wrong Go type: want not ok")
	}

	// Dictionary encoding accepts any value because it keys on the fmt form, but
	// the decoded strings must convert back to the column type. Strings that do
	// not parse as integers fail the round trip, which is the decode check
	// measureLayout exists to catch.
	if _, ok := measureLayout([]string{"abc", "def"}, ColumnSchema{Name: "x", Type: TypeInt32}, EncDict, CompressNone, flateLevel, &measureScratch{}); ok {
		t.Error("measureLayout with values that cannot be decoded back: want not ok")
	}

	// An empty column gives every candidate a size of zero, so the ordering the
	// sort falls back to is the encoded size.
	results := benchmarkLayoutsTyped([]bool{}, ColumnSchema{Name: "x", Type: TypeBool}, &measureScratch{}, flateLevel)
	if len(results) != 6 {
		t.Fatalf("benchmarkLayoutsTyped of an empty bool column: got %d results, want 6", len(results))
	}
	if results[0].EncodedSize != 0 || results[0].CompressedSize != 0 {
		t.Errorf("benchmarkLayoutsTyped of an empty bool column: first result = %+v, want zero sizes", results[0])
	}
	for i := 1; i < len(results); i++ {
		if results[i].CompressedSize < results[i-1].CompressedSize {
			t.Errorf("benchmarkLayoutsTyped result %d is larger than result %d: %+v then %+v", i-1, i, results[i-1], results[i])
		}
	}
}

// The candidates a type can choose among are fixed, so the table is the whole
// contract: a type either has candidates or it has none, and the ones it has are
// the encodings that can read it back.
func TestLayoutCandidates(t *testing.T) {
	for _, tc := range []struct {
		typ  uint8
		name string
		want []uint8
	}{
		{TypeBool, "bool", []uint8{EncRLEBitpack, EncPlain}},
		{TypeInt8, "int8", []uint8{EncPlain, EncDelta, EncDict}},
		{TypeInt64, "int64", []uint8{EncPlain, EncDelta, EncDict}},
		{TypeUint32, "uint32", []uint8{EncPlain, EncDelta, EncDict}},
		{TypeFloat32, "float32", []uint8{EncPlain, EncDict}},
		{TypeFloat64, "float64", []uint8{EncPlain, EncDict}},
		{TypeBytes, "bytes", []uint8{EncOffsetBytes, EncDict, EncAffix}},
		{TypeString, "string", []uint8{EncPlain, EncOffsetBytes, EncDict, EncAffix}},
		{0xFF, "unknown", nil},
	} {
		got := layoutCandidates(tc.typ)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("layoutCandidates(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// encName and codecName only exist for messages a person reads, but every name
// has to resolve, since a candidate the table omits is one a failure message
// cannot describe.
func TestEncAndCodecNames(t *testing.T) {
	for _, tc := range []struct {
		enc  uint8
		name string
	}{
		{EncPlain, "Plain"},
		{EncRLEBitpack, "RLEBitpack"},
		{EncDelta, "Delta"},
		{EncDict, "Dict"},
		{EncOffsetBytes, "OffsetBytes"},
		{EncAffix, "Affix"},
		{99, "Enc99"},
	} {
		if got := encName(tc.enc); got != tc.name {
			t.Errorf("encName(%d) = %q, want %q", tc.enc, got, tc.name)
		}
	}
	for _, tc := range []struct {
		codec uint8
		name  string
	}{
		{CompressNone, "None"},
		{CompressZstd, "Zstd"},
		{CompressFlate, "Flate"},
		{CompressGzip, "Gzip"},
		{CompressZlib, "Zlib"},
		{CompressLzw, "Lzw"},
		{99, "Codec99"},
	} {
		if got := codecName(tc.codec); got != tc.name {
			t.Errorf("codecName(%d) = %q, want %q", tc.codec, got, tc.name)
		}
	}
}

// compressLevel pulls a level back into the range the codecs accept, and treats
// zero as the default, since that is what an empty Options means. Go's own
// sentinels pass through: DefaultCompression is a level the stdlib understands.
func TestCompressLevel(t *testing.T) {
	for _, tc := range []struct {
		level int
		want  int
	}{
		{0, flateLevel},
		{-3, flate.HuffmanOnly},
		{flate.HuffmanOnly, flate.HuffmanOnly},
		{-1, -1},
		{flate.BestSpeed, flate.BestSpeed},
		{flate.BestCompression, flate.BestCompression},
		{100, flate.BestCompression},
	} {
		if got := compressLevel(tc.level); got != tc.want {
			t.Errorf("compressLevel(%d) = %d, want %d", tc.level, got, tc.want)
		}
	}
}

// optimizeLevel is compressLevel for the Optimize pass rather than the write.
// Zero means the best a codec offers, since a caller running Optimize has
// already decided the data is worth the cost. Anything else stays a level a
// codec accepts.
func TestOptimizeLevel(t *testing.T) {
	for _, tc := range []struct {
		level int
		want  int
	}{
		{0, flate.BestCompression},
		{-3, flate.HuffmanOnly},
		{flate.BestSpeed, flate.BestSpeed},
		{6, 6},
		{100, flate.BestCompression},
	} {
		if got := optimizeLevel(tc.level); got != tc.want {
			t.Errorf("optimizeLevel(%d) = %d, want %d", tc.level, got, tc.want)
		}
	}
}

// DEFLATE bakes its level into the writer at construction, so a pooled writer
// only serves the level it was made for. The pools are keyed by codec and level
// both, and a writer asked for level 9 has to compress at level 9 rather than at
// whatever level filled the pool first.
func TestCodecPoolIsKeyedByLevel(t *testing.T) {
	// The pools are process global, so the test uses a codec no writer asks for
	// and drops its keys on the way out: a mock left in a pool real code reads
	// is worse than no test at all.
	t.Cleanup(func() {
		codecWriters.Delete(poolKey{codec: 99, level: 1})
		codecWriters.Delete(poolKey{codec: 99, level: 9})
	})

	newWriter := func() any { return &mockCompressor{} }
	one := codecPool(99, 1, newWriter)
	nine := codecPool(99, 9, newWriter)
	if one == nine {
		t.Error("levels 1 and 9 share a pool, so a writer would compress at the wrong level")
	}
	if got := codecPool(99, 1, newWriter); got != one {
		t.Error("codecPool did not return the same pool for a level it has seen")
	}
}

// mockCompressor is a placeholder for a pool to hold. Nothing writes through
// it; it only has to be one pool's occupant rather than another's.
type mockCompressor struct{}

func (*mockCompressor) Write([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (*mockCompressor) Close() error              { return nil }
func (*mockCompressor) Reset(io.Writer) error     { return nil }

// Affix encoding is built on shared prefixes and suffixes. Both compare only
// the shorter of the two values, or a long value next to a short one reads past
// its end.
func TestSharedAffixBounds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		a, b         string
		prefix, suff int
	}{
		{"identical", "keine", "keine", 5, 5},
		{"nothing shared", "abc", "xyz", 0, 0},
		{"prefix only", "prefix-suffix", "prefix-other", 7, 0},
		{"suffix only", "one-suffix", "two-suffix", 0, 7},
		{"first shorter", "keine", "keine-longer", 5, 0},
		{"second shorter", "longer-keine", "keine", 0, 5},
	} {
		if got := sharedPrefix(tc.a, tc.b); got != tc.prefix {
			t.Errorf("%s: sharedPrefix(%q, %q) = %d, want %d", tc.name, tc.a, tc.b, got, tc.prefix)
		}
		if got := sharedSuffix(tc.a, tc.b); got != tc.suff {
			t.Errorf("%s: sharedSuffix(%q, %q) = %d, want %d", tc.name, tc.a, tc.b, got, tc.suff)
		}
	}
}

// encodeWith reports a type it cannot encode rather than encoding something
// close to it, and every encoding it offers has to accept the column it is for.
func TestEncodeWith(t *testing.T) {
	if _, err := encodeWith(EncOffsetBytes, []string{"a"}, TypeBytes); err == nil {
		t.Error("encodeWith of a string column as offset bytes: want error, got nil")
	}
	if _, err := encodeWith(99, []int64{1}, TypeInt64); err == nil {
		t.Error("encodeWith of an unknown encoding: want error, got nil")
	}
	if _, err := canonicalColumnTyped([]any{int64(1)}, 99); err == nil {
		t.Error("canonicalColumnTyped of an unknown type: want error, got nil")
	}
	if _, err := encodeWith(EncOffsetBytes, [][]byte{{1, 2}, {3}}, TypeBytes); err != nil {
		t.Errorf("encodeWith of a bytes column as offset bytes: %v", err)
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

	// The format version is the write after it, and fails the same way.
	w = NewWriter(&failAfter{n: 1}, schema)
	if err := w.AddRowGroup([][]any{{true, false}}); err == nil {
		t.Error("AddRowGroup after a failed version write: want error, got nil")
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

	// The underlying writer fails partway through the chunk. NewWriter has
	// written the magic and the version, so the chunk header is the third write.
	w = NewWriter(&failAfter{n: 2}, schema)
	if err := w.AddRowGroup([][]any{{true, false}}); err == nil {
		t.Error("AddRowGroup with a failing writer: want error, got nil")
	}

	// Close failing at each of its three writes. NewWriter has already written the
	// magic and the format version, so Close's are the third, fourth and fifth.
	for n := 2; n <= 4; n++ {
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

// TestAffixRoundTrip covers the shapes affix has to get right: a shared prefix,
// a shared suffix, both at once, a prefix and suffix that would overlap in the
// column's shortest value, and the cases with nothing to share or nothing to
// write.
func TestAffixRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		vals []string
	}{
		{"empty column", nil},
		{"one value", []string{"solo"}},
		{"all identical", []string{"same", "same", "same"}},
		{"all empty", []string{"", "", ""}},
		{"prefix only", []string{"user-1", "user-22", "user-333"}},
		{"suffix only", []string{"1@example.com", "22@example.com", "333@example.com"}},
		{"prefix and suffix", []string{"u1@a", "u22@a", "u333@a"}},
		{"affixes overlap", []string{"ab", "aab", "aaab"}},
		{"nothing shared", []string{"abc", "def", "ghi"}},
	}
	for _, c := range cases {
		encoded, err := EncodeAffix(c.vals)
		if err != nil {
			t.Fatalf("%s: EncodeAffix: %v", c.name, err)
		}
		got, err := DecodeAffix(encoded, TypeString)
		if err != nil {
			t.Fatalf("%s: DecodeAffix: %v", c.name, err)
		}
		out, _ := got.([]string)
		if len(out) != len(c.vals) {
			t.Fatalf("%s: round trip has %d values, want %d", c.name, len(out), len(c.vals))
		}
		for i := range out {
			if out[i] != c.vals[i] {
				t.Errorf("%s: value %d = %q, want %q", c.name, i, out[i], c.vals[i])
			}
		}

		// A byte column goes through the same encoder and comes back as copies
		// rather than shared headers.
		bytes := make([][]byte, len(c.vals))
		for i, v := range c.vals {
			bytes[i] = []byte(v)
		}
		encoded, err = EncodeAffix(bytes)
		if err != nil {
			t.Fatalf("%s: EncodeAffix([][]byte): %v", c.name, err)
		}
		got, err = DecodeAffix(encoded, TypeBytes)
		if err != nil {
			t.Fatalf("%s: DecodeAffix([][]byte): %v", c.name, err)
		}
		bout, _ := got.([][]byte)
		if !reflect.DeepEqual(bout, bytes) {
			t.Errorf("%s: byte round trip = %v, want %v", c.name, bout, bytes)
		}
	}

	// Affix's shortest value has to keep the prefix and suffix from overlapping,
	// or the values it writes back are not the ones it read.
	short, _ := EncodeAffix([]string{"ab", "aaab"})
	long, _ := DecodeAffix(short, TypeString)
	if got, _ := long.([]string); !reflect.DeepEqual(got, []string{"ab", "aaab"}) {
		t.Errorf("affix with a short value = %v, want [ab aaab]", got)
	}

	if _, err := EncodeAffix([]int64{1}); err == nil {
		t.Error("EncodeAffix of an int64 column: want error, got nil")
	}
}

// TestAffixTruncated covers the header and middle reads a short chunk reaches.
// A cut inside the middle stream may still land on a value boundary, in which
// case the chunk decodes as fewer values than it holds.
func TestAffixTruncated(t *testing.T) {
	vals := []string{"u1@x", "u2@x"}
	full, err := EncodeAffix(vals)
	if err != nil {
		t.Fatal(err)
	}
	for cut := 0; cut < len(full); cut++ {
		got, err := DecodeAffix(full[:cut], TypeString)
		if err == nil {
			out, _ := got.([]string)
			if len(out) > len(vals) {
				t.Errorf("DecodeAffix of %d bytes returned %d values, more than it holds", cut, len(out))
			}
			for i := range out {
				if out[i] != vals[i] {
					t.Errorf("DecodeAffix of %d bytes: value %d = %q, want %q", cut, i, out[i], vals[i])
				}
			}
		}
	}
	if _, err := DecodeAffix(full, TypeString); err != nil {
		t.Errorf("DecodeAffix of a complete chunk: %v", err)
	}

	// A length that runs past the data.
	if _, err := DecodeAffix(binary.LittleEndian.AppendUint32([]byte{0, 0}, 40), TypeString); err == nil {
		t.Error("DecodeAffix with a prefix longer than the data: want error, got nil")
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
