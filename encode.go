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

// EncodeDict builds a dictionary keyed on the fmt.Sprintf form of each value,
// then stores the uint32 index of every value bitpacked at ceil(log2(dictSize))
// bits, followed by the dictionary entries in OFFSET_BYTES layout.
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
// themselves and byte slices on their fmt form, which is what the reader parses
// back; anything else falls back to the fmt form of the value, so a column of
// any type can still be dictionary encoded through []any.
func dictKeys(vals any) ([]string, int, error) {
	if s, ok := vals.([]string); ok {
		return s, len(s), nil
	}
	if s, ok := vals.([][]byte); ok {
		keys := make([]string, len(s))
		for i, v := range s {
			keys[i] = fmt.Sprintf("%v", v)
		}
		return keys, len(s), nil
	}
	rv := reflect.ValueOf(vals)
	if rv.Kind() != reflect.Slice {
		return nil, 0, fmt.Errorf("keine: dict encode expects a slice, got %T", vals)
	}
	keys := make([]string, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		keys[i] = fmt.Sprintf("%v", rv.Index(i).Interface())
	}
	return keys, rv.Len(), nil
}

// bitpackIndices packs indices using nbits per value, most significant bit
// first, padded to a byte boundary.
func bitpackIndices(indices []uint32, nbits uint) []byte {
	if nbits == 0 {
		return make([]byte, 0)
	}
	out := make([]byte, (uint64(len(indices))*uint64(nbits)+7)/8)
	var pos uint64
	for _, idx := range indices {
		for j := uint(0); j < nbits; j++ {
			if idx&(1<<(nbits-1-j)) != 0 {
				out[pos/8] |= byte(0x80 >> (pos % 8))
			}
			pos++
		}
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
