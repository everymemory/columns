package keine

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unsafe"
)

// DecodePlain is the inverse of EncodePlain. typ is required because the bytes
// alone do not say how many values they hold.
func DecodePlain(b []byte, typ uint8) ([]any, error) {
	typed, err := decodePlainTyped(b, typ)
	if err != nil {
		return nil, err
	}
	return boxValues(typed), nil
}

// decodePlainTyped decodes plain data into a slice of the Go type for typ —
// []int64 for TypeInt64 and so on — rather than []any. DecodePlain boxes it, and
// the typed reader uses it to skip the boxing altogether.
func decodePlainTyped(b []byte, typ uint8) (any, error) {
	switch typ {
	case TypeBool:
		out := make([]bool, len(b))
		for i, c := range b {
			out[i] = c != 0
		}
		return out, nil
	case TypeInt8:
		out := make([]int8, len(b))
		for i, c := range b {
			out[i] = int8(c)
		}
		return out, nil
	case TypeInt16:
		return decodeFixedT(b, 2, func(s []byte) int16 { return int16(binary.LittleEndian.Uint16(s)) })
	case TypeInt32:
		return decodeFixedT(b, 4, func(s []byte) int32 { return int32(binary.LittleEndian.Uint32(s)) })
	case TypeInt64:
		return decodeFixedT(b, 8, func(s []byte) int64 { return int64(binary.LittleEndian.Uint64(s)) })
	case TypeUint8:
		return append([]uint8(nil), b...), nil
	case TypeUint16:
		return decodeFixedT(b, 2, func(s []byte) uint16 { return binary.LittleEndian.Uint16(s) })
	case TypeUint32:
		return decodeFixedT(b, 4, func(s []byte) uint32 { return binary.LittleEndian.Uint32(s) })
	case TypeUint64:
		return decodeFixedT(b, 8, func(s []byte) uint64 { return binary.LittleEndian.Uint64(s) })
	case TypeFloat32:
		return decodeFixedT(b, 4, func(s []byte) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(s)) })
	case TypeFloat64:
		return decodeFixedT(b, 8, func(s []byte) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(s)) })
	case TypeString, TypeBytes:
		return decodePlainVarlen(b, typ)
	default:
		return nil, fmt.Errorf("keine: unknown type %d", typ)
	}
}

// decodePlainVarlen decodes the length-prefixed plain form used by strings and
// byte slices. It takes the values' total length up front so it can size both
// the result and its backing buffer in one pass: strings are slices of that
// buffer rather than a copy each, which is what keeps a high cardinality string
// column from allocating once per value. Byte slices still get their own copy
// each, because they are mutable and a caller writing one would otherwise
// clobber its neighbours.
func decodePlainVarlen(b []byte, typ uint8) (any, error) {
	nvals, total, ok := varlenStats(b)
	if !ok {
		return nil, fmt.Errorf("keine: plain data truncated at value %d", nvals)
	}

	if typ == TypeString {
		// One buffer holds every value's bytes and each string points into it.
		// Strings are immutable, so sharing the buffer cannot corrupt a value,
		// and N distinct strings cost two allocations instead of N+1.
		text := make([]byte, total)
		out := make([]string, nvals)
		var off int
		for i := 0; i < nvals; i++ {
			n := int(binary.LittleEndian.Uint32(b[:4]))
			b = b[4:]
			copy(text[off:off+n], b[:n])
			out[i] = unsafe.String(&text[off], n)
			off += n
			b = b[n:]
		}
		return out, nil
	}

	out := make([][]byte, nvals)
	for i := 0; i < nvals; i++ {
		n := int(binary.LittleEndian.Uint32(b[:4]))
		b = b[4:]
		out[i] = append([]byte(nil), b[:n]...)
		b = b[n:]
	}
	return out, nil
}

// varlenStats is the length prefix pass over a plain varlen chunk: the number
// of values and how many bytes they occupy in total. Both are needed to size
// the result and its backing buffer before any value is placed. When the chunk
// is truncated, nvals is the index of the value it happened at.
func varlenStats(b []byte) (nvals, total int, ok bool) {
	for len(b) > 0 {
		if len(b) < 4 {
			return nvals, 0, false
		}
		n := binary.LittleEndian.Uint32(b[:4])
		b = b[4:]
		if uint64(len(b)) < uint64(n) {
			return nvals, 0, false
		}
		nvals++
		total += int(n)
		b = b[n:]
	}
	return nvals, total, true
}

func decodeFixedT[T any](b []byte, width int, convert func([]byte) T) ([]T, error) {
	if len(b)%width != 0 {
		return nil, fmt.Errorf("keine: plain data length %d is not a multiple of %d", len(b), width)
	}
	out := make([]T, len(b)/width)
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
	entries, err := decodeDictTyped(b)
	if err != nil {
		return nil, err
	}
	return boxValues(entries), nil
}

// decodeDictTyped decodes a dictionary chunk into its strings.
func decodeDictTyped(b []byte) ([]string, error) {
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

	// Converting an entry to a string copies its bytes, and a dictionary column
	// repeats each entry many times over. Converting the distinct entries once
	// and then sharing those headers turns one allocation per value into one per
	// distinct value.
	dict := make([]string, len(entries))
	for i, e := range entries {
		dict[i] = string(e)
	}

	out := make([]string, nvals)
	for i := 0; i < nvals; i++ {
		if int(indices[i]) >= len(dict) {
			return nil, fmt.Errorf("keine: dict index %d out of range", indices[i])
		}
		out[i] = dict[indices[i]]
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

// decodeTyped dispatches a chunk to the decoder for enc and returns the values
// as a slice of the Go type that decoder produces — []int64 from EncDelta,
// []string from EncDict. n is the number of values in the chunk.
func decodeTyped(enc uint8, data []byte, n int, typ uint8) (any, error) {
	switch enc {
	case EncPlain:
		return decodePlainTyped(data, typ)
	case EncRLEBitpack:
		return DecodeRLEBitpack(data, n), nil
	case EncDelta:
		return DecodeDelta(data)
	case EncDict:
		return decodeDictTyped(data)
	case EncOffsetBytes:
		return DecodeOffsetBytes(data)
	default:
		return nil, fmt.Errorf("keine: unknown encoding %d", enc)
	}
}

// encProducesType reports whether the decoder for enc already yields typ's Go
// type. Every encoder except Dict writes values in their declared form, so most
// columns need no conversion after decoding.
func encProducesType(enc, typ uint8) bool {
	switch enc {
	case EncPlain:
		return true
	case EncRLEBitpack:
		return typ == TypeBool
	case EncDelta:
		return typ == TypeInt64
	case EncDict:
		return typ == TypeString
	case EncOffsetBytes:
		return typ == TypeBytes
	default:
		return false
	}
}

// decodeWith dispatches data to the decoder for enc, then converts the values
// to the Go types implied by typ. n is the number of values in the chunk.
func decodeWith(enc uint8, data []byte, n int, typ uint8) ([]any, error) {
	typed, err := decodeTyped(enc, data, n, typ)
	if err != nil {
		return nil, err
	}
	return boxColumn(typed, enc, typ)
}

// boxValues converts a typed slice — []int64, []string and so on — into []any
// without copying a single value. Each interface header points at its element
// where it already sits in the source slice, so a 10000-value column costs one
// allocation instead of one per value: converting a value type to an interface
// allocates a box for it, and a column of those is a column of boxes.
//
// Keeping interior pointers into s is safe because the decoders build a fresh
// slice for every column and the caller takes ownership of it, so nothing
// reuses that backing array. A type the format does not use has no values to
// box, and the decoders only ever hand it one of the thirteen.
func boxValues(typed any) []any {
	switch s := typed.(type) {
	case []bool:
		return boxSlice(s)
	case []int8:
		return boxSlice(s)
	case []int16:
		return boxSlice(s)
	case []int32:
		return boxSlice(s)
	case []int64:
		return boxSlice(s)
	case []uint8:
		return boxSlice(s)
	case []uint16:
		return boxSlice(s)
	case []uint32:
		return boxSlice(s)
	case []uint64:
		return boxSlice(s)
	case []float32:
		return boxSlice(s)
	case []float64:
		return boxSlice(s)
	case []string:
		return boxSlice(s)
	case [][]byte:
		return boxSlice(s)
	}
	return nil
}

// boxSlice is boxValues for one element type.
func boxSlice[T any](s []T) []any {
	out := make([]any, len(s))
	if len(s) == 0 {
		return out
	}

	// Converting the zero value once yields the exact type pointer this element
	// type needs, which every interface in the column then shares. Converting a
	// real element would do the same and cost the same one allocation.
	var zero T
	sample := any(zero)
	tag := (*efaceHeader)(unsafe.Pointer(&sample)).typ

	hdrs := unsafe.Slice((*efaceHeader)(unsafe.Pointer(unsafe.SliceData(out))), len(out))
	for i := range s {
		hdrs[i].typ = tag
		hdrs[i].data = unsafe.Pointer(&s[i])
	}
	return out
}

// efaceHeader is the layout of an empty interface, which is what a []any holds.
// It is runtime.iface's empty counterpart, with a type pointer in place of an
// itab.
type efaceHeader struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

// asValues converts a typed slice into []T. The second result is false when the
// slice's element type is not T, so asking for the wrong type is an error
// rather than a silent mismatch.
func asValues[T any](typed any) ([]T, bool) {
	switch s := typed.(type) {
	case []bool:
		return convertSlice[bool, T](s)
	case []int8:
		return convertSlice[int8, T](s)
	case []int16:
		return convertSlice[int16, T](s)
	case []int32:
		return convertSlice[int32, T](s)
	case []int64:
		return convertSlice[int64, T](s)
	case []uint8:
		return convertSlice[uint8, T](s)
	case []uint16:
		return convertSlice[uint16, T](s)
	case []uint32:
		return convertSlice[uint32, T](s)
	case []uint64:
		return convertSlice[uint64, T](s)
	case []float32:
		return convertSlice[float32, T](s)
	case []float64:
		return convertSlice[float64, T](s)
	case []string:
		return convertSlice[string, T](s)
	case [][]byte:
		return convertSlice[[]byte, T](s)
	}
	return nil, false
}

func convertSlice[S any, T any](s []S) ([]T, bool) {
	// A slice holds one element type, so the zero value settles whether T fits
	// without checking every value.
	var zero S
	if _, ok := any(zero).(T); !ok {
		return nil, false
	}
	out := make([]T, len(s))
	for i, v := range s {
		out[i] = any(v).(T)
	}
	return out, true
}

// canonicalColumn converts decoded values to the Go types implied by typ, so a
// round trip gives back values equal to the ones written.
func canonicalColumn(vals []any, typ uint8) ([]any, error) {
	typed, err := canonicalColumnTyped(vals, typ)
	if err != nil {
		return nil, err
	}
	return boxValues(typed), nil
}

// canonicalColumnTyped is canonicalColumn returning a slice of the Go type typ
// implies rather than []any. The encoders and the statistics pass consume it
// directly, so a column is coerced once and never boxed on the way to disk.
func canonicalColumnTyped(vals []any, typ uint8) (any, error) {
	switch typ {
	case TypeBool:
		return canonicalTyped(vals, func(v any) (bool, error) { return coerceBool(v) })
	case TypeInt8:
		return canonicalTyped(vals, func(v any) (int8, error) {
			n, err := coerceInt(v, 8)
			if err != nil {
				return 0, err
			}
			return int8(n), nil
		})
	case TypeInt16:
		return canonicalTyped(vals, func(v any) (int16, error) {
			n, err := coerceInt(v, 16)
			if err != nil {
				return 0, err
			}
			return int16(n), nil
		})
	case TypeInt32:
		return canonicalTyped(vals, func(v any) (int32, error) {
			n, err := coerceInt(v, 32)
			if err != nil {
				return 0, err
			}
			return int32(n), nil
		})
	case TypeInt64:
		return canonicalTyped(vals, func(v any) (int64, error) { return coerceInt(v, 64) })
	case TypeUint8:
		return canonicalTyped(vals, func(v any) (uint8, error) {
			n, err := coerceUint(v, 8)
			if err != nil {
				return 0, err
			}
			return uint8(n), nil
		})
	case TypeUint16:
		return canonicalTyped(vals, func(v any) (uint16, error) {
			n, err := coerceUint(v, 16)
			if err != nil {
				return 0, err
			}
			return uint16(n), nil
		})
	case TypeUint32:
		return canonicalTyped(vals, func(v any) (uint32, error) {
			n, err := coerceUint(v, 32)
			if err != nil {
				return 0, err
			}
			return uint32(n), nil
		})
	case TypeUint64:
		return canonicalTyped(vals, func(v any) (uint64, error) { return coerceUint(v, 64) })
	case TypeFloat32:
		return canonicalTyped(vals, func(v any) (float32, error) {
			f, err := coerceFloat(v, 32)
			if err != nil {
				return 0, err
			}
			return float32(f), nil
		})
	case TypeFloat64:
		return canonicalTyped(vals, func(v any) (float64, error) { return coerceFloat(v, 64) })
	case TypeString:
		return canonicalTyped(vals, func(v any) (string, error) { return coerceString(v) })
	case TypeBytes:
		return canonicalTyped(vals, func(v any) ([]byte, error) { return coerceBytes(v) })
	default:
		return nil, fmt.Errorf("keine: unknown type %d", typ)
	}
}

// canonicalTyped coerces vals elementwise through coerce and collects the
// results into a []T.
func canonicalTyped[T any](vals []any, coerce func(any) (T, error)) ([]T, error) {
	out := make([]T, len(vals))
	for i, v := range vals {
		x, err := coerce(v)
		if err != nil {
			return nil, err
		}
		out[i] = x
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
