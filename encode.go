package keine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"
	"reflect"
)

// EncodePlain encodes vals in order. Fixed width numeric kinds are written
// little-endian; strings and byte slices get a uint32 length prefix followed by
// their raw bytes.
func EncodePlain(vals any) ([]byte, error) {
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
	rv := reflect.ValueOf(vals)
	if rv.Kind() != reflect.Slice {
		return nil, fmt.Errorf("keine: dict encode expects a slice, got %T", vals)
	}
	n := rv.Len()
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = fmt.Sprintf("%v", rv.Index(i).Interface())
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

// encodeWith dispatches col to the encoder for enc, first coercing the values
// to the Go types implied by typ.
func encodeWith(enc uint8, col []any, typ uint8) ([]byte, error) {
	switch enc {
	case EncPlain:
		canon, err := canonicalColumn(col, typ)
		if err != nil {
			return nil, err
		}
		return EncodePlain(canon)
	case EncRLEBitpack:
		bools := make([]bool, len(col))
		for i, v := range col {
			b, err := coerceBool(v)
			if err != nil {
				return nil, err
			}
			bools[i] = b
		}
		return EncodeRLEBitpack(bools), nil
	case EncDelta:
		ints := make([]int64, len(col))
		for i, v := range col {
			n, err := coerceInt(v, 64)
			if err != nil {
				return nil, err
			}
			ints[i] = n
		}
		return EncodeDelta(ints), nil
	case EncDict:
		return EncodeDict(col)
	case EncOffsetBytes:
		raw := make([][]byte, len(col))
		for i, v := range col {
			b, err := coerceBytes(v)
			if err != nil {
				return nil, err
			}
			raw[i] = b
		}
		return EncodeOffsetBytes(raw), nil
	default:
		return nil, fmt.Errorf("keine: unknown encoding %d", enc)
	}
}
