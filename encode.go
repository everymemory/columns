package keine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"reflect"
)

// EncodePlain encodes vals in order. Fixed width numeric kinds are written
// little-endian; strings and byte slices get a uint32 length prefix followed by
// their raw bytes.
func EncodePlain(vals any) ([]byte, error) {
	switch s := vals.(type) {
	case []bool:
		buf := make([]byte, len(s))
		for i, v := range s {
			if v {
				buf[i] = 1
			}
		}
		return buf, nil
	case []int8:
		buf := make([]byte, len(s))
		for i, v := range s {
			buf[i] = byte(v)
		}
		return buf, nil
	case []int16:
		buf := make([]byte, 2*len(s))
		for i, v := range s {
			binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
		}
		return buf, nil
	case []int32:
		buf := make([]byte, 4*len(s))
		for i, v := range s {
			binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
		}
		return buf, nil
	case []int64:
		buf := make([]byte, 8*len(s))
		for i, v := range s {
			binary.LittleEndian.PutUint64(buf[i*8:], uint64(v))
		}
		return buf, nil
	case []uint8:
		buf := make([]byte, len(s))
		copy(buf, s)
		return buf, nil
	case []uint16:
		buf := make([]byte, 2*len(s))
		for i, v := range s {
			binary.LittleEndian.PutUint16(buf[i*2:], v)
		}
		return buf, nil
	case []uint32:
		buf := make([]byte, 4*len(s))
		for i, v := range s {
			binary.LittleEndian.PutUint32(buf[i*4:], v)
		}
		return buf, nil
	case []uint64:
		buf := make([]byte, 8*len(s))
		for i, v := range s {
			binary.LittleEndian.PutUint64(buf[i*8:], v)
		}
		return buf, nil
	case []float32:
		buf := make([]byte, 4*len(s))
		for i, v := range s {
			binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
		}
		return buf, nil
	case []float64:
		buf := make([]byte, 8*len(s))
		for i, v := range s {
			binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
		}
		return buf, nil
	case []string:
		total := 0
		for _, v := range s {
			total += 4 + len(v)
		}
		buf := make([]byte, 0, total)
		var prefix [4]byte
		for _, v := range s {
			binary.LittleEndian.PutUint32(prefix[:], uint32(len(v)))
			buf = append(buf, prefix[:]...)
			buf = append(buf, v...)
		}
		return buf, nil
	case [][]byte:
		total := 0
		for _, v := range s {
			total += 4 + len(v)
		}
		buf := make([]byte, 0, total)
		var prefix [4]byte
		for _, v := range s {
			binary.LittleEndian.PutUint32(prefix[:], uint32(len(v)))
			buf = append(buf, prefix[:]...)
			buf = append(buf, v...)
		}
		return buf, nil
	}
	return encodePlainReflect(vals)
}

// encodePlainReflect handles slices EncodePlain has no case for, chiefly []any
// and slices whose elements are not one of the thirteen column types.
func encodePlainReflect(vals any) ([]byte, error) {
	rv := reflect.ValueOf(vals)
	if rv.Kind() != reflect.Slice {
		return nil, fmt.Errorf("keine: plain encode expects a slice, got %T", vals)
	}
	var buf bytes.Buffer
	for i := 0; i < rv.Len(); i++ {
		v := rv.Index(i)
		if v.Kind() == reflect.Interface {
			if v.IsNil() {
				return nil, fmt.Errorf("keine: plain encode cannot encode nil at index %d", i)
			}
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Bool:
			if v.Bool() {
				buf.WriteByte(1)
			} else {
				buf.WriteByte(0)
			}
		case reflect.String:
			writeLengthPrefixed(&buf, v.String())
		case reflect.Slice:
			if v.Type().Elem().Kind() != reflect.Uint8 {
				return nil, fmt.Errorf("keine: plain encode cannot encode %v", v.Type())
			}
			writeLengthPrefixed(&buf, string(v.Bytes()))
		default:
			if err := binary.Write(&buf, binary.LittleEndian, v.Interface()); err != nil {
				return nil, err
			}
		}
	}
	return buf.Bytes(), nil
}

func writeLengthPrefixed(buf *bytes.Buffer, s string) {
	var prefix [4]byte
	binary.LittleEndian.PutUint32(prefix[:], uint32(len(s)))
	buf.Write(prefix[:])
	buf.WriteString(s)
}

// EncodeRLEBitpack packs one bit per value, eight values per byte, most
// significant bit first. The result is padded to a byte boundary.
func EncodeRLEBitpack(vals []bool) []byte {
	b := make([]byte, (len(vals)+7)/8)
	for i, v := range vals {
		if v {
			b[i/8] |= 0x80 >> (i % 8)
		}
	}
	return b
}

// EncodeDelta writes the first value verbatim and each following value as its
// difference from the previous one, all as little-endian int64.
func EncodeDelta(vals []int64) []byte {
	buf := make([]byte, 0, 8*len(vals))
	var prev int64
	for i, v := range vals {
		d := v
		if i > 0 {
			d = v - prev
		}
		buf = binary.LittleEndian.AppendUint64(buf, uint64(d))
		prev = v
	}
	return buf
}

// EncodeOffsetBytes writes a uint32 element count, a uint32 offset per element
// into the raw byte block that follows, then the concatenated raw bytes.
// Integers are little-endian.
func EncodeOffsetBytes(vals [][]byte) []byte {
	buf := make([]byte, 0, 4+4*len(vals))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(vals)))
	var off uint32
	for _, v := range vals {
		buf = binary.LittleEndian.AppendUint32(buf, off)
		off += uint32(len(v))
	}
	for _, v := range vals {
		buf = append(buf, v...)
	}
	return buf
}

// EncodeAffix writes the bytes every value in the column has at its start and at
// its end, then each value with those bytes gone: a length-prefixed stream of
// what is left between them. Values that share a domain, a path or a key share
// that part once rather than per value, and the middles are that much shorter
// for whatever codec follows.
func EncodeAffix(vals any) ([]byte, error) {
	switch s := vals.(type) {
	case []string:
		pre, suf := columnAffix(s)
		return writeAffix(s, pre, suf), nil
	case [][]byte:
		pre, suf := columnAffix(s)
		return writeAffix(s, pre, suf), nil
	default:
		return nil, fmt.Errorf("keine: affix encodes string and bytes columns, got %T", vals)
	}
}

// columnAffix is the longest prefix and suffix shared by every value. The two
// are allowed to meet in the column's shortest value but not to overlap in it,
// so a value is always its prefix, its middle and its suffix back to back.
func columnAffix[T ~string | ~[]byte](s []T) (pre, suf T) {
	if len(s) == 0 {
		return
	}
	pre, suf = s[0], s[0]
	min := len(s[0])
	for _, v := range s[1:] {
		pre = pre[:sharedPrefix(pre, v)]
		suf = suf[len(suf)-sharedSuffix(suf, v):]
		if len(v) < min {
			min = len(v)
		}
	}
	if len(pre)+len(suf) > min {
		suf = suf[len(suf)-(min-len(pre)):]
	}
	return pre, suf
}

// writeAffix lays out the prefix, the suffix, then one length-prefixed middle
// per value.
func writeAffix[T ~string | ~[]byte](s []T, pre, suf T) []byte {
	buf := make([]byte, 0, 8+len(pre)+len(suf)+len(s)*(4+(len(pre)+len(suf))/2))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(pre)))
	buf = append(buf, pre...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(suf)))
	buf = append(buf, suf...)
	var n [4]byte
	for _, v := range s {
		mid := v[len(pre) : len(v)-len(suf)]
		binary.LittleEndian.PutUint32(n[:], uint32(len(mid)))
		buf = append(buf, n[:]...)
		buf = append(buf, mid...)
	}
	return buf
}

func sharedPrefix[T ~string | ~[]byte](a, b T) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func sharedSuffix[T ~string | ~[]byte](a, b T) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 1; i <= n; i++ {
		if a[len(a)-i] != b[len(b)-i] {
			return i - 1
		}
	}
	return n
}

// EncodeDict builds a dictionary of the distinct values in vals, keyed on each
// value's encoded form, then stores the uint32 index of every value bitpacked at
// ceil(log2(dictSize)) bits, followed by the dictionary entries in OFFSET_BYTES
// layout.
func EncodeDict(vals any) ([]byte, error) {
	keys, n, err := dictKeys(vals)
	if err != nil {
		return nil, err
	}

	order := make([]string, 0, n)
	lookup := make(map[string]uint32, n)
	indices := make([]uint32, n)
	for i, key := range keys {
		if _, ok := lookup[key]; !ok {
			lookup[key] = uint32(len(order))
			order = append(order, key)
		}
		indices[i] = lookup[key]
	}

	dictSize := uint32(len(order))
	nbits := uint32(0)
	if dictSize > 1 {
		nbits = uint32(bits.Len(uint(dictSize) - 1))
	}

	buf := make([]byte, 0, 12+len(keys))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(n))
	buf = binary.LittleEndian.AppendUint32(buf, dictSize)
	buf = binary.LittleEndian.AppendUint32(buf, nbits)
	buf = append(buf, bitpackIndices(indices, uint(nbits))...)

	entries := make([][]byte, len(order))
	for i, key := range order {
		entries[i] = []byte(key)
	}
	buf = append(buf, EncodeOffsetBytes(entries)...)
	return buf, nil
}

// dictKeys returns the dictionary key of every element of vals. Strings key on
// themselves and byte slices on their bytes; the fixed width types key on their
// little-endian form, which is what the reader decodes back. Encoding a whole
// column once into a flat buffer and slicing the keys out of it keeps them at
// one allocation for the buffer rather than a formatted string per value.
func dictKeys(vals any) ([]string, int, error) {
	if s, ok := vals.([]string); ok {
		return s, len(s), nil
	}
	if s, ok := vals.([][]byte); ok {
		keys := make([]string, len(s))
		for i, v := range s {
			keys[i] = string(v)
		}
		return keys, len(s), nil
	}
	switch s := vals.(type) {
	case []bool:
		return dictFlat(s, 1, func(v bool, b []byte) {
			if v {
				b[0] = 1
			}
		})
	case []int8:
		return dictFlat(s, 1, func(v int8, b []byte) { b[0] = byte(v) })
	case []int16:
		return dictFlat(s, 2, func(v int16, b []byte) { binary.LittleEndian.PutUint16(b, uint16(v)) })
	case []int32:
		return dictFlat(s, 4, func(v int32, b []byte) { binary.LittleEndian.PutUint32(b, uint32(v)) })
	case []int64:
		return dictFlat(s, 8, func(v int64, b []byte) { binary.LittleEndian.PutUint64(b, uint64(v)) })
	case []uint8:
		return dictFlat(s, 1, func(v uint8, b []byte) { b[0] = v })
	case []uint16:
		return dictFlat(s, 2, func(v uint16, b []byte) { binary.LittleEndian.PutUint16(b, v) })
	case []uint32:
		return dictFlat(s, 4, func(v uint32, b []byte) { binary.LittleEndian.PutUint32(b, v) })
	case []uint64:
		return dictFlat(s, 8, func(v uint64, b []byte) { binary.LittleEndian.PutUint64(b, v) })
	case []float32:
		return dictFlat(s, 4, func(v float32, b []byte) { binary.LittleEndian.PutUint32(b, math.Float32bits(v)) })
	case []float64:
		return dictFlat(s, 8, func(v float64, b []byte) { binary.LittleEndian.PutUint64(b, math.Float64bits(v)) })
	}

	// A slice of interfaces has no one encoded form, so its elements key on
	// themselves: strings and byte slices as their bytes, anything else as the
	// text the reader parses back.
	rv := reflect.ValueOf(vals)
	if rv.Kind() != reflect.Slice {
		return nil, 0, fmt.Errorf("keine: dict encode expects a slice, got %T", vals)
	}
	keys := make([]string, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		v := rv.Index(i)
		if v.Kind() == reflect.Interface {
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.String:
			keys[i] = v.String()
		case reflect.Slice:
			if v.Type().Elem().Kind() != reflect.Uint8 {
				return nil, 0, fmt.Errorf("keine: dict cannot encode %v", v.Type())
			}
			keys[i] = string(v.Bytes())
		default:
			return nil, 0, fmt.Errorf("keine: dict cannot encode %v", v.Type())
		}
	}
	return keys, rv.Len(), nil
}

// dictFlat lays s out in its plain form and returns one key per value, each a
// slice of that buffer.
func dictFlat[T any](s []T, width int, put func(v T, b []byte)) ([]string, int, error) {
	flat := make([]byte, width*len(s))
	for i, v := range s {
		put(v, flat[i*width:(i+1)*width])
	}
	keys := make([]string, len(s))
	for i := range keys {
		keys[i] = string(flat[i*width : (i+1)*width])
	}
	return keys, len(s), nil
}

// bitpackIndices packs indices using nbits per value, most significant bit
// first, padded to a byte boundary. The accumulator never holds more than
// nbits + 7 bits, which fits in a uint64 for every width a uint32 index can
// need, so one path handles all of them.
func bitpackIndices(indices []uint32, nbits uint) []byte {
	if nbits == 0 {
		return make([]byte, 0)
	}
	out := make([]byte, (uint64(len(indices))*uint64(nbits)+7)/8)
	var acc uint64
	var bits uint
	p := 0
	for _, idx := range indices {
		acc = acc<<nbits | uint64(idx)
		bits += nbits
		for bits >= 8 {
			out[p] = byte(acc >> (bits - 8))
			p++
			bits -= 8
		}
		acc &= (1 << bits) - 1
	}
	if bits > 0 {
		out[p] = byte(acc << (8 - bits))
	}
	return out
}

// encodeWith dispatches typed, a column already coerced to the Go type typ
// implies, to the encoder for enc.
func encodeWith(enc uint8, typed any, typ uint8) ([]byte, error) {
	switch enc {
	case EncPlain:
		return EncodePlain(typed)
	case EncRLEBitpack:
		bools, ok := typed.([]bool)
		if !ok {
			return nil, fmt.Errorf("keine: rle bitpack encodes bool columns, got %T", typed)
		}
		return EncodeRLEBitpack(bools), nil
	case EncDelta:
		ints, err := toInt64s(typed)
		if err != nil {
			return nil, err
		}
		return EncodeDelta(ints), nil
	case EncDict:
		return EncodeDict(typed)
	case EncAffix:
		return EncodeAffix(typed)
	case EncOffsetBytes:
		raw, ok := typed.([][]byte)
		if !ok {
			return nil, fmt.Errorf("keine: offset bytes encodes bytes columns, got %T", typed)
		}
		return EncodeOffsetBytes(raw), nil
	default:
		return nil, fmt.Errorf("keine: unknown encoding %d", enc)
	}
}

// toInt64s widens an integer column to []int64, which is what Delta stores. It
// is a bit cast rather than a range check because Delta already carries int64
// differences for every integer width and the reader narrows back.
func toInt64s(typed any) ([]int64, error) {
	switch s := typed.(type) {
	case []int8:
		out := make([]int64, len(s))
		for i, v := range s {
			out[i] = int64(v)
		}
		return out, nil
	case []int16:
		out := make([]int64, len(s))
		for i, v := range s {
			out[i] = int64(v)
		}
		return out, nil
	case []int32:
		out := make([]int64, len(s))
		for i, v := range s {
			out[i] = int64(v)
		}
		return out, nil
	case []int64:
		return s, nil
	case []uint8:
		out := make([]int64, len(s))
		for i, v := range s {
			out[i] = int64(v)
		}
		return out, nil
	case []uint16:
		out := make([]int64, len(s))
		for i, v := range s {
			out[i] = int64(v)
		}
		return out, nil
	case []uint32:
		out := make([]int64, len(s))
		for i, v := range s {
			out[i] = int64(v)
		}
		return out, nil
	case []uint64:
		out := make([]int64, len(s))
		for i, v := range s {
			out[i] = int64(v)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("keine: delta encodes integer columns, got %T", typed)
	}
}
