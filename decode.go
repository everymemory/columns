package keine

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// DecodePlain is the inverse of EncodePlain. typ is required because the bytes
// alone do not say how many values they hold.
func DecodePlain(b []byte, typ uint8) ([]any, error) {
	switch typ {
	case TypeBool:
		out := make([]any, len(b))
		for i, c := range b {
			out[i] = c != 0
		}
		return out, nil
	case TypeInt8:
		out := make([]any, len(b))
		for i, c := range b {
			out[i] = int8(c)
		}
		return out, nil
	case TypeInt16:
		return decodeFixed(b, 2, func(s []byte) any { return int16(binary.LittleEndian.Uint16(s)) })
	case TypeInt32:
		return decodeFixed(b, 4, func(s []byte) any { return int32(binary.LittleEndian.Uint32(s)) })
	case TypeInt64:
		return decodeFixed(b, 8, func(s []byte) any { return int64(binary.LittleEndian.Uint64(s)) })
	case TypeUint8:
		out := make([]any, len(b))
		for i, c := range b {
			out[i] = c
		}
		return out, nil
	case TypeUint16:
		return decodeFixed(b, 2, func(s []byte) any { return binary.LittleEndian.Uint16(s) })
	case TypeUint32:
		return decodeFixed(b, 4, func(s []byte) any { return binary.LittleEndian.Uint32(s) })
	case TypeUint64:
		return decodeFixed(b, 8, func(s []byte) any { return binary.LittleEndian.Uint64(s) })
	case TypeFloat32:
		return decodeFixed(b, 4, func(s []byte) any { return math.Float32frombits(binary.LittleEndian.Uint32(s)) })
	case TypeFloat64:
		return decodeFixed(b, 8, func(s []byte) any { return math.Float64frombits(binary.LittleEndian.Uint64(s)) })
	case TypeString, TypeBytes:
		out := make([]any, 0, len(b)/8)
		for len(b) > 0 {
			if len(b) < 4 {
				return nil, fmt.Errorf("keine: plain data truncated at value %d", len(out))
			}
			n := binary.LittleEndian.Uint32(b[:4])
			b = b[4:]
			if uint64(len(b)) < uint64(n) {
				return nil, fmt.Errorf("keine: plain data truncated at value %d", len(out))
			}
			if typ == TypeString {
				out = append(out, string(b[:n]))
			} else {
				out = append(out, append([]byte(nil), b[:n]...))
			}
			b = b[n:]
		}
		return out, nil
	default:
		return nil, fmt.Errorf("keine: unknown type %d", typ)
	}
}

func decodeFixed(b []byte, width int, convert func([]byte) any) ([]any, error) {
	if len(b)%width != 0 {
		return nil, fmt.Errorf("keine: plain data length %d is not a multiple of %d", len(b), width)
	}
	out := make([]any, len(b)/width)
	for i := range out {
		out[i] = convert(b[i*width : (i+1)*width])
	}
	return out, nil
}

// DecodeRLEBitpack is the inverse of EncodeRLEBitpack. It unpacks n values.
func DecodeRLEBitpack(b []byte, n int) []bool {
	out := make([]bool, n)
	for i := 0; i < n; i++ {
		if i/8 >= len(b) {
			break
		}
		if b[i/8]&(0x80>>(i%8)) != 0 {
			out[i] = true
		}
	}
	return out
}

// DecodeDelta is the inverse of EncodeDelta.
func DecodeDelta(b []byte) ([]int64, error) {
	if len(b)%8 != 0 {
		return nil, fmt.Errorf("keine: delta data length %d is not a multiple of 8", len(b))
	}
	out := make([]int64, len(b)/8)
	var prev int64
	for i := 0; i < len(out); i++ {
		d := int64(binary.LittleEndian.Uint64(b[i*8:]))
		if i > 0 {
			d += prev
		}
		out[i] = d
		prev = d
	}
	return out, nil
}

// DecodeOffsetBytes is the inverse of EncodeOffsetBytes.
func DecodeOffsetBytes(b []byte) ([][]byte, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("keine: offset bytes data truncated")
	}
	n := int(binary.LittleEndian.Uint32(b[:4]))
	hdr := 4 + 4*n
	if len(b) < hdr {
		return nil, fmt.Errorf("keine: offset bytes header truncated")
	}
	offsets := make([]int, n)
	for i := 0; i < n; i++ {
		offsets[i] = int(binary.LittleEndian.Uint32(b[4+4*i:]))
	}
	raw := b[hdr:]
	out := make([][]byte, n)
	for i := 0; i < n; i++ {
		start := offsets[i]
		end := len(raw)
		if i+1 < n {
			end = offsets[i+1]
		}
		if start < 0 || end < start || end > len(raw) {
			return nil, fmt.Errorf("keine: invalid offset %d in offset bytes data", start)
		}
		out[i] = append([]byte(nil), raw[start:end]...)
	}
	return out, nil
}

// DecodeDict is the inverse of EncodeDict. Values come back as the string form
// the dictionary was keyed on; canonicalColumn converts them to the column's
// declared type.
func DecodeDict(b []byte) ([]any, error) {
	if len(b) < 12 {
		return nil, fmt.Errorf("keine: dict data truncated")
	}
	nvals := int(binary.LittleEndian.Uint32(b[:4]))
	dictSize := binary.LittleEndian.Uint32(b[4:8])
	nbits := binary.LittleEndian.Uint32(b[8:12])
	idxBytes := int((uint64(nvals)*uint64(nbits) + 7) / 8)
	if len(b) < 12+idxBytes {
		return nil, fmt.Errorf("keine: dict indices truncated")
	}
	indices := bitunpack(b[12:12+idxBytes], nvals, uint(nbits))

	entries, err := DecodeOffsetBytes(b[12+idxBytes:])
	if err != nil {
		return nil, err
	}
	if uint32(len(entries)) != dictSize {
		return nil, fmt.Errorf("keine: dict size mismatch: header says %d, found %d", dictSize, len(entries))
	}

	out := make([]any, nvals)
	for i := 0; i < nvals; i++ {
		if int(indices[i]) >= len(entries) {
			return nil, fmt.Errorf("keine: dict index %d out of range", indices[i])
		}
		out[i] = string(entries[indices[i]])
	}
	return out, nil
}

// bitunpack is the inverse of bitpackIndices.
func bitunpack(b []byte, n int, nbits uint) []uint32 {
	out := make([]uint32, n)
	if nbits == 0 {
		return out
	}
	var pos uint64
	for i := 0; i < n; i++ {
		var v uint32
		for j := uint(0); j < nbits; j++ {
			if pos/8 < uint64(len(b)) && b[pos/8]&(byte(0x80)>>(pos%8)) != 0 {
				v |= 1 << (nbits - 1 - j)
			}
			pos++
		}
		out[i] = v
	}
	return out
}

// decodeWith dispatches data to the decoder for enc, then converts the values
// to the Go types implied by typ. n is the number of values in the chunk.
func decodeWith(enc uint8, data []byte, n int, typ uint8) ([]any, error) {
	var vals []any
	switch enc {
	case EncPlain:
		var err error
		vals, err = DecodePlain(data, typ)
		if err != nil {
			return nil, err
		}
	case EncRLEBitpack:
		bools := DecodeRLEBitpack(data, n)
		vals = make([]any, len(bools))
		for i, v := range bools {
			vals[i] = v
		}
	case EncDelta:
		ints, err := DecodeDelta(data)
		if err != nil {
			return nil, err
		}
		vals = make([]any, len(ints))
		for i, v := range ints {
			vals[i] = v
		}
	case EncDict:
		var err error
		vals, err = DecodeDict(data)
		if err != nil {
			return nil, err
		}
	case EncOffsetBytes:
		raw, err := DecodeOffsetBytes(data)
		if err != nil {
			return nil, err
		}
		vals = make([]any, len(raw))
		for i, v := range raw {
			vals[i] = v
		}
	default:
		return nil, fmt.Errorf("keine: unknown encoding %d", enc)
	}
	return canonicalColumn(vals, typ)
}

// canonicalColumn converts decoded values to the Go types implied by typ, so a
// round trip gives back values equal to the ones written.
func canonicalColumn(vals []any, typ uint8) ([]any, error) {
	out := make([]any, len(vals))
	for i, v := range vals {
		cv, err := canonicalValue(v, typ)
		if err != nil {
			return nil, err
		}
		out[i] = cv
	}
	return out, nil
}

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

func coerceBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		return strconv.ParseBool(x)
	case []byte:
		return strconv.ParseBool(string(x))
	case int64:
		return x != 0, nil
	case nil:
		return false, fmt.Errorf("keine: cannot coerce nil to bool")
	}
	return false, fmt.Errorf("keine: cannot coerce %T to bool", v)
}

func coerceInt(v any, bits int) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case uint:
		return int64(x), nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		return int64(x), nil
	case float32:
		return int64(x), nil
	case float64:
		return int64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return strconv.ParseInt(x, 10, bits)
	case []byte:
		return strconv.ParseInt(string(x), 10, bits)
	case nil:
		return 0, fmt.Errorf("keine: cannot coerce nil to int")
	}
	return 0, fmt.Errorf("keine: cannot coerce %T to int", v)
}

func coerceUint(v any, bits int) (uint64, error) {
	switch x := v.(type) {
	case uint64:
		return x, nil
	case uint:
		return uint64(x), nil
	case uint8:
		return uint64(x), nil
	case uint16:
		return uint64(x), nil
	case uint32:
		return uint64(x), nil
	case int64:
		return uint64(x), nil
	case int:
		return uint64(x), nil
	case int8:
		return uint64(x), nil
	case int16:
		return uint64(x), nil
	case int32:
		return uint64(x), nil
	case float32:
		return uint64(x), nil
	case float64:
		return uint64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return strconv.ParseUint(x, 10, bits)
	case []byte:
		return strconv.ParseUint(string(x), 10, bits)
	case nil:
		return 0, fmt.Errorf("keine: cannot coerce nil to uint")
	}
	return 0, fmt.Errorf("keine: cannot coerce %T to uint", v)
}

func coerceFloat(v any, bits int) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int8:
		return float64(x), nil
	case int16:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case uint64:
		return float64(x), nil
	case uint:
		return float64(x), nil
	case uint8:
		return float64(x), nil
	case uint16:
		return float64(x), nil
	case uint32:
		return float64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return strconv.ParseFloat(x, bits)
	case []byte:
		return strconv.ParseFloat(string(x), bits)
	case nil:
		return 0, fmt.Errorf("keine: cannot coerce nil to float")
	}
	return 0, fmt.Errorf("keine: cannot coerce %T to float", v)
}

func coerceString(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case []byte:
		return string(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case bool:
		return strconv.FormatBool(x), nil
	case nil:
		return "", fmt.Errorf("keine: cannot coerce nil to string")
	}
	return "", fmt.Errorf("keine: cannot coerce %T to string", v)
}

// coerceBytes returns the raw bytes of v. Strings holding the fmt "%v" form of
// a byte slice, which is what dictionary encoded byte columns decode to, are
// parsed back into a slice.
func coerceBytes(v any) ([]byte, error) {
	switch x := v.(type) {
	case []byte:
		return append([]byte(nil), x...), nil
	case string:
		return parseBytesList(x)
	case nil:
		return nil, fmt.Errorf("keine: cannot coerce nil to bytes")
	}
	return nil, fmt.Errorf("keine: cannot coerce %T to bytes", v)
}

func parseBytesList(s string) ([]byte, error) {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("keine: cannot parse %q as a byte slice", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []byte{}, nil
	}
	parts := strings.Fields(inner)
	out := make([]byte, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 8)
		if err != nil {
			return nil, fmt.Errorf("keine: cannot parse %q as a byte slice: %w", s, err)
		}
		out[i] = byte(n)
	}
	return out, nil
}
