// Per-column statistics.

package columns

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

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

	// Fixed width values are stored little endian, whose most significant byte is
	// the last one, so the min and the max have to be found by comparing values
	// rather than comparing the bytes from index 0. 2.5 and 0.5 first differ in
	// the byte before the sign, and a byte wise comparison from the low end reads
	// that byte first and calls 2.5 the smaller.
	ff, err := canonicalColumnTyped([]any{
		float64(2.5), float64(0.5), float64(4.5), float64(-1.0),
	}, TypeFloat64)
	if err != nil {
		t.Fatalf("canonicalColumnTyped: %v", err)
	}
	var f64Meta ColMeta
	fillStatsTyped(&f64Meta, ff)
	var want [8]byte
	binary.LittleEndian.PutUint64(want[:], math.Float64bits(-1.0))
	if !bytes.Equal(f64Meta.MinVal, want[:]) {
		t.Errorf("fillStatsTyped float64 min = %x, want %x", f64Meta.MinVal, want)
	}
	binary.LittleEndian.PutUint64(want[:], math.Float64bits(4.5))
	if !bytes.Equal(f64Meta.MaxVal, want[:]) {
		t.Errorf("fillStatsTyped float64 max = %x, want %x", f64Meta.MaxVal, want)
	}
	if f64Meta.MinLen != 8 || f64Meta.MaxLen != 8 {
		t.Errorf("fillStatsTyped float64 MinLen = %d, MaxLen = %d, want 8 and 8",
			f64Meta.MinLen, f64Meta.MaxLen)
	}

	// Signed integers are likewise ordered by value, not by byte.
	ii, err := canonicalColumnTyped([]any{
		int64(300), int64(-300), int64(1),
	}, TypeInt64)
	if err != nil {
		t.Fatalf("canonicalColumnTyped: %v", err)
	}
	var i64Meta ColMeta
	fillStatsTyped(&i64Meta, ii)
	smallest := int64(-300)
	binary.LittleEndian.PutUint64(want[:], uint64(smallest))
	if !bytes.Equal(i64Meta.MinVal, want[:]) {
		t.Errorf("fillStatsTyped int64 min = %x, want %x", i64Meta.MinVal, want)
	}
	binary.LittleEndian.PutUint64(want[:], 300)
	if !bytes.Equal(i64Meta.MaxVal, want[:]) {
		t.Errorf("fillStatsTyped int64 max = %x, want %x", i64Meta.MaxVal, want)
	}

	// Booleans are one byte each, so the min is false and the max is true.
	bb2, err := canonicalColumnTyped([]any{true, false, true}, TypeBool)
	if err != nil {
		t.Fatalf("canonicalColumnTyped: %v", err)
	}
	var boolMeta ColMeta
	fillStatsTyped(&boolMeta, bb2)
	if !bytes.Equal(boolMeta.MinVal, []byte{0}) || !bytes.Equal(boolMeta.MaxVal, []byte{1}) {
		t.Errorf("fillStatsTyped bool min = %v, max = %v", boolMeta.MinVal, boolMeta.MaxVal)
	}

	if _, err := canonicalColumnTyped([]any{nil}, TypeBool); err == nil {
		t.Error("canonicalColumnTyped of nil: want error, got nil")
	}
}
