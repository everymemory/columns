// Decoder bounds, truncation and round trips.

package columns

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

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
		{EncTokenized, nil, TypeString},
		{99, nil, TypeInt8},
	} {
		if _, err := decodeWith(c.enc, c.data, 1, c.typ, nil); err == nil {
			t.Errorf("decodeWith(%d, ..., %d): want error, got nil", c.enc, c.typ)
		}
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
