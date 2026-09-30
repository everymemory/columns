// Encoder bounds and dispatch.

package keine

import (
	"reflect"
	"testing"
)

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
	if typedColumn(TypeInt64, []int32{1}) {
		t.Error("typedColumn accepted []int32 for an int64 column")
	}
	if !typedColumn(TypeInt64, []int64{1, 2}) {
		t.Error("typedColumn rejected a []int64 column for TypeInt64")
	}
	if typedColumn(99, []int64{1}) {
		t.Error("typedColumn accepted a column for an unknown type")
	}
	if _, err := encodeWith(EncOffsetBytes, [][]byte{{1, 2}, {3}}, TypeBytes); err != nil {
		t.Errorf("encodeWith of a bytes column as offset bytes: %v", err)
	}
}
