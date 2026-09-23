package keine

import (
	"bytes"
	"reflect"
	"testing"
)

// EncodePlain has a fast path for each of the thirteen column types and falls
// back to reflection for everything else. The fast path is byte for byte what
// the reflection path produces, which is what lets it replace it: a column
// encoded either way reads back identically.
func TestEncodePlainFastPathMatchesReflection(t *testing.T) {
	cases := []struct {
		name  string
		typed any
	}{
		{"bool", []bool{true, false, true}},
		{"int8", []int8{-128, 0, 127}},
		{"int16", []int16{-32768, 1, 32767}},
		{"int32", []int32{-2147483648, 1, 2147483647}},
		{"int64", []int64{-1 << 62, 1, 1<<62 - 1}},
		{"uint8", []uint8{0, 128, 255}},
		{"uint16", []uint16{0, 40000, 65535}},
		{"uint32", []uint32{0, 4000000000, 4294967295}},
		{"uint64", []uint64{0, 1 << 40, 1<<64 - 1}},
		{"float32", []float32{0, -1.5, 3.25}},
		{"float64", []float64{0, -1.5, 3.25}},
		{"string", []string{"", "a", "longer value"}},
		{"bytes", [][]byte{{}, {1, 2, 3}, {9, 9}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fast, err := EncodePlain(c.typed)
			if err != nil {
				t.Fatalf("EncodePlain(%s): %v", c.name, err)
			}
			boxed := boxValues(c.typed)
			slow, err := encodePlainReflect(boxed)
			if err != nil {
				t.Fatalf("encodePlainReflect(%s): %v", c.name, err)
			}
			if !bytes.Equal(fast, slow) {
				t.Errorf("EncodePlain(%s) = %v, reflection path = %v", c.name, fast, slow)
			}
		})
	}
}

// canonicalColumnTyped must reject a value that cannot become the column's Go
// type, including values that parse but overflow a narrower width.
func TestCanonicalColumnTypedErrors(t *testing.T) {
	cases := []struct {
		val any
		typ uint8
	}{
		{nil, TypeBool},
		{"999", TypeInt8},
		{"99999", TypeInt16},
		{"99999999999", TypeInt32},
		{"not an int", TypeInt64},
		{"999", TypeUint8},
		{"99999", TypeUint16},
		{"99999999999", TypeUint32},
		{"not an int", TypeUint64},
		{"not a float", TypeFloat32},
		{"not a float", TypeFloat64},
		{nil, TypeString},
		{nil, TypeBytes},
	}
	for _, c := range cases {
		if _, err := canonicalColumnTyped([]any{c.val}, c.typ); err == nil {
			t.Errorf("canonicalColumnTyped(%T, %d): want error, got nil", c.val, c.typ)
		}
	}
}

// The layout helpers take a column that is already typed. A column whose Go
// type does not match the encoding, or that cannot be coerced at all, is
// rejected rather than measured against the alternatives.
func TestLayoutTypedInputs(t *testing.T) {
	if results := BenchmarkLayouts([]any{struct{}{}}, ColumnSchema{Name: "b", Type: TypeBool}); results != nil {
		t.Errorf("BenchmarkLayouts of an uncoercible column = %v, want nil", results)
	}
	sample := make([]int64, 2*maxExperimentRows)
	for i := range sample {
		sample[i] = int64(i)
	}
	if got := experimentSampleTyped(sample); len(got.([]int64)) >= len(sample) {
		t.Errorf("experimentSampleTyped kept %d of %d values", len(got.([]int64)), len(sample))
	}
	// A type the sampler has no case for passes through untouched, so it is
	// still measured rather than dropped.
	in := []int{1, 2, 3}
	if got := sampleDispatch(in, 2); !reflect.DeepEqual(got, in) {
		t.Errorf("sampleDispatch of an unsupported type = %v, want it unchanged", got)
	}
	if _, ok := measureLayout([]string{"x"}, ColumnSchema{Name: "b", Type: TypeBool}, EncRLEBitpack, CompressNone); ok {
		t.Error("measureLayout of a bool encoding on strings: want not ok")
	}
	// A type with no candidate encodings falls back to Plain+None.
	best := experimentLayoutsTyped([]int{1, 2, 3}, ColumnSchema{Name: "x", Type: 0xFF})
	if best.Encoding != EncPlain || best.Compress != CompressNone {
		t.Errorf("experimentLayoutsTyped fallback = %s, want Plain+None", best.Name)
	}
}
