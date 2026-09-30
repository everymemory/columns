// Layout candidates and the encoding and codec name tables.

package columns

import (
	"reflect"
	"testing"
)

func TestLayoutHelpers(t *testing.T) {
	if got := layoutCandidates(0xFF, false); got != nil {
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

	if _, ok := measureLayout([]any{int8(1)}, ColumnSchema{Name: "x", Type: TypeInt8}, EncPlain, CompressZstd, flateLevel, &measureScratch{}, nil); ok {
		t.Error("measureLayout with a codec that cannot compress: want not ok")
	}
	if _, ok := measureLayout([]any{int8(1)}, ColumnSchema{Name: "x", Type: TypeInt8}, EncPlain, CompressFlate, flateLevel, &measureScratch{}, nil); !ok {
		t.Error("measureLayout with CompressFlate: want ok")
	}
	if _, err := canonicalColumnTyped([]any{struct{}{}}, TypeBool); err == nil {
		t.Error("canonicalColumnTyped of an uncoercible value: want error, got nil")
	}
	if _, ok := measureLayout([]string{"x"}, ColumnSchema{Name: "x", Type: TypeBool}, EncRLEBitpack, CompressNone, flateLevel, &measureScratch{}, nil); ok {
		t.Error("measureLayout with a column of the wrong Go type: want not ok")
	}

	// Dictionary encoding accepts any value because it keys on the fmt form, but
	// the decoded strings must convert back to the column type. Strings that do
	// not parse as integers fail the round trip, which is the decode check
	// measureLayout exists to catch.
	if _, ok := measureLayout([]string{"abc", "def"}, ColumnSchema{Name: "x", Type: TypeInt32}, EncDict, CompressNone, flateLevel, &measureScratch{}, nil); ok {
		t.Error("measureLayout with values that cannot be decoded back: want not ok")
	}

	// An empty column gives every candidate a size of zero, so the ordering the
	// sort falls back to is the encoded size.
	results := benchmarkLayoutsTyped([]bool{}, ColumnSchema{Name: "x", Type: TypeBool}, nil, &measureScratch{}, flateLevel)
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
		got := layoutCandidates(tc.typ, false)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("layoutCandidates(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A column with a tokenizer adds one candidate, and only the two text-like
	// types can take it: an int64 column holding tokens is still an int64 column.
	for _, tc := range []struct {
		typ  uint8
		name string
		want []uint8
	}{
		{TypeString, "string", []uint8{EncPlain, EncOffsetBytes, EncDict, EncAffix, EncTokenized}},
		{TypeBytes, "bytes", []uint8{EncOffsetBytes, EncDict, EncAffix, EncTokenized}},
		{TypeInt64, "int64", []uint8{EncPlain, EncDelta, EncDict}},
	} {
		got := layoutCandidates(tc.typ, true)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("layoutCandidates(%s, tokenized) = %v, want %v", tc.name, got, tc.want)
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
