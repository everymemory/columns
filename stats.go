// Per-column statistics: the min and max a column's values imply, and its
// null count, recorded in ColMeta while a row group is written.

package keine

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"math"
)

// fillStatsTyped folds the byte form of every value into meta. typed is a
// canonical column, so every case it can be is one of the thirteen below and no
// default is needed.
//
// Fixed width columns are a special case: the min and the max are found by
// comparing the values themselves and serialised once each at the end, rather
// than serialising every value into a scratch buffer and comparing the bytes.
// A value's bytes are not a sort key at all. Little endian puts the least
// significant byte first, and two's complement puts every negative integer
// above every positive one, so a byte wise comparison of a column of int64 can
// report a min larger than its max. Comparing the values costs one comparison
// and one branch per value, and drops two length checks that are false for
// every value after the first.
func fillStatsTyped(meta *ColMeta, typed any) {
	switch s := typed.(type) {
	case []bool:
		lo, hi := byte(1), byte(0)
		for _, v := range s {
			if v {
				hi = 1
			} else {
				lo = 0
			}
		}
		if len(s) > 0 {
			recordFixed(meta, 1, []byte{lo}, []byte{hi})
		}
	case []int8:
		if lo, hi, ok := fixedEnds(s, 1, func(v int8) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 1, lo, hi)
		}
	case []int16:
		if lo, hi, ok := fixedEnds(s, 2, func(v int16) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 2, lo, hi)
		}
	case []int32:
		if lo, hi, ok := fixedEnds(s, 4, func(v int32) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 4, lo, hi)
		}
	case []int64:
		if lo, hi, ok := fixedEnds(s, 8, func(v int64) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 8, lo, hi)
		}
	case []uint8:
		if lo, hi, ok := fixedEnds(s, 1, func(v uint8) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 1, lo, hi)
		}
	case []uint16:
		if lo, hi, ok := fixedEnds(s, 2, func(v uint16) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 2, lo, hi)
		}
	case []uint32:
		if lo, hi, ok := fixedEnds(s, 4, func(v uint32) uint64 { return uint64(v) }); ok {
			recordFixed(meta, 4, lo, hi)
		}
	case []uint64:
		if lo, hi, ok := fixedEnds(s, 8, func(v uint64) uint64 { return v }); ok {
			recordFixed(meta, 8, lo, hi)
		}
	case []float32:
		if lo, hi, ok := fixedEnds(s, 4, func(v float32) uint64 { return uint64(math.Float32bits(v)) }); ok {
			recordFixed(meta, 4, lo, hi)
		}
	case []float64:
		if lo, hi, ok := fixedEnds(s, 8, func(v float64) uint64 { return math.Float64bits(v) }); ok {
			recordFixed(meta, 8, lo, hi)
		}
	case []string:
		var lo, hi string
		for i, v := range s {
			if i == 0 {
				lo, hi = v, v
				meta.MinLen = uint32(len(v))
				meta.MaxLen = uint32(len(v))
				continue
			}
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
			if uint32(len(v)) < meta.MinLen {
				meta.MinLen = uint32(len(v))
			}
			if uint32(len(v)) > meta.MaxLen {
				meta.MaxLen = uint32(len(v))
			}
		}
		meta.MinVal = append(meta.MinVal, lo...)
		meta.MaxVal = append(meta.MaxVal, hi...)
	case [][]byte:
		for _, v := range s {
			statBytes(meta, v)
		}
	}
}

// fixedEnds reports the smallest and largest of s, whose values are all width
// bytes, as serialised bytes. s is a column of an ordered type, so its values
// compare with < and the ends are found in one pass. toU64 writes one value the
// way the encoder stores it, little endian, which is what makes the result the
// bytes a caller comparing stored values would have found. ok is false for an
// empty column, whose min and max do not exist.
func fixedEnds[T cmp.Ordered](s []T, width int, toU64 func(T) uint64) (lo, hi []byte, ok bool) {
	if len(s) == 0 {
		return nil, nil, false
	}
	loV, hiV := s[0], s[0]
	for _, v := range s[1:] {
		if v < loV {
			loV = v
		}
		if hiV < v {
			hiV = v
		}
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], toU64(loV))
	lo = append([]byte(nil), b[:width]...)
	binary.LittleEndian.PutUint64(b[:], toU64(hiV))
	hi = append([]byte(nil), b[:width]...)
	return lo, hi, true
}

// recordFixed writes the ends of a fixed width column, whose values are all
// width bytes. The caller has found them already, so this only stores them.
func recordFixed(meta *ColMeta, width int, lo, hi []byte) {
	meta.MinVal = append(meta.MinVal, lo...)
	meta.MaxVal = append(meta.MaxVal, hi...)
	meta.MinLen = uint32(width)
	meta.MaxLen = uint32(width)
}

// statBytes folds b into meta's min and max. MinLen and MaxLen track value
// length separately from min and max, since a column's longest value is not
// necessarily its largest. Variable width columns use it per value; fixed width
// columns find their ends up front and go through recordFixed instead.
func statBytes(meta *ColMeta, b []byte) {
	if len(meta.MinVal) == 0 {
		meta.MinVal = append(meta.MinVal, b...)
		meta.MaxVal = append(meta.MaxVal, b...)
		meta.MinLen = uint32(len(b))
		meta.MaxLen = uint32(len(b))
		return
	}
	if bytes.Compare(b, meta.MinVal) < 0 {
		meta.MinVal = append(meta.MinVal[:0], b...)
	}
	if bytes.Compare(b, meta.MaxVal) > 0 {
		meta.MaxVal = append(meta.MaxVal[:0], b...)
	}
	if uint32(len(b)) < meta.MinLen {
		meta.MinLen = uint32(len(b))
	}
	if uint32(len(b)) > meta.MaxLen {
		meta.MaxLen = uint32(len(b))
	}
}
