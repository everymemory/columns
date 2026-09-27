package keine

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unsafe"
)

// dest holds one column's decoded values, reused across reads. Decoders write
// their results into vals instead of allocating, so a repeating column costs no
// allocation after the first read. String and byte slice values point into buf,
// and a dictionary column unpacks its indices into idx.
//
// The returned values alias these buffers, so a path that reuses a dest may only
// hand out its values while it knows the caller is done with them. The boxing
// paths pass a fresh dest instead.
type dest struct {
	vals any
	buf  []byte
	idx  []uint32
}

// sizedSlice returns n elements, reusing have's capacity when it is already wide
// enough. Writing into the result instead of allocating is what makes a repeated
// column free after its first read. An empty column still gets an empty slice,
// not a nil one, so a caller comparing with reflect.DeepEqual sees the same
// value each time.
func sizedSlice[T any](have []T, n int) []T {
	if have != nil && cap(have) >= n {
		return have[:n]
	}
	return make([]T, n)
}

// DecodePlain is the inverse of EncodePlain. typ is required because the bytes
// alone do not say how many values they hold.
func DecodePlain(b []byte, typ uint8) ([]any, error) {
	typed, err := decodePlainTyped(b, typ, &dest{})
	if err != nil {
		return nil, err
	}
	return boxValues(typed), nil
}

// decodePlainTyped decodes plain data as a slice of the Go type for typ, []int64
// for TypeInt64 and so on, rather than []any. DecodePlain boxes the result; the
// typed reader uses this to skip boxing.
func decodePlainTyped(b []byte, typ uint8, d *dest) (any, error) {
	switch typ {
	case TypeBool:
		out, _ := d.vals.([]bool)
		out = sizedSlice(out, len(b))
		d.vals = out
		for i, c := range b {
			out[i] = c != 0
		}
		return out, nil
	case TypeInt8:
		out, _ := d.vals.([]int8)
		out = sizedSlice(out, len(b))
		d.vals = out
		for i, c := range b {
			out[i] = int8(c)
		}
		return out, nil
	case TypeInt16:
		return decodeFixedT(b, 2, func(s []byte) int16 { return int16(binary.LittleEndian.Uint16(s)) }, d)
	case TypeInt32:
		return decodeFixedT(b, 4, func(s []byte) int32 { return int32(binary.LittleEndian.Uint32(s)) }, d)
	case TypeInt64:
		return decodeFixedT(b, 8, func(s []byte) int64 { return int64(binary.LittleEndian.Uint64(s)) }, d)
	case TypeUint8:
		out, _ := d.vals.([]uint8)
		out = append(out[:0], b...)
		d.vals = out
		return out, nil
	case TypeUint16:
		return decodeFixedT(b, 2, func(s []byte) uint16 { return binary.LittleEndian.Uint16(s) }, d)
	case TypeUint32:
		return decodeFixedT(b, 4, func(s []byte) uint32 { return binary.LittleEndian.Uint32(s) }, d)
	case TypeUint64:
		return decodeFixedT(b, 8, func(s []byte) uint64 { return binary.LittleEndian.Uint64(s) }, d)
	case TypeFloat32:
		return decodeFixedT(b, 4, func(s []byte) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(s)) }, d)
	case TypeFloat64:
		return decodeFixedT(b, 8, func(s []byte) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(s)) }, d)
	case TypeString, TypeBytes:
		return decodePlainVarlen(b, typ, d)
	default:
		return nil, fmt.Errorf("keine: unknown type %d", typ)
	}
}

// decodePlainVarlen decodes the length-prefixed plain form used by strings and
// byte slices. varlenStats supplies the value count and total length up front,
// so the result and its backing buffer are sized in one pass. Strings point into
// that shared buffer instead of copying, while byte slices still copy because
// they are mutable and a caller writing one would clobber its neighbours.
func decodePlainVarlen(b []byte, typ uint8, d *dest) (any, error) {
	nvals, total, ok := varlenStats(b)
	if !ok {
		return nil, fmt.Errorf("keine: plain data truncated at value %d", nvals)
	}

	if typ == TypeString {
		// One buffer holds every value's bytes and each string points into it.
		// Strings are immutable, so sharing cannot corrupt a value, and N distinct
		// strings cost two allocations instead of N+1.
		text := sizedSlice(d.buf, total)
		d.buf = text
		out, _ := d.vals.([]string)
		out = sizedSlice(out, nvals)
		d.vals = out
		var off int
		for i := 0; i < nvals; i++ {
			n := int(binary.LittleEndian.Uint32(b[:4]))
			b = b[4:]
			copy(text[off:off+n], b[:n])
			out[i] = stringAt(text, off, n)
			off += n
			b = b[n:]
		}
		return out, nil
	}

	out, _ := d.vals.([][]byte)
	out = sizedSlice(out, nvals)
	d.vals = out
	for i := 0; i < nvals; i++ {
		n := int(binary.LittleEndian.Uint32(b[:4]))
		b = b[4:]
		out[i] = append([]byte(nil), b[:n]...)
		b = b[n:]
	}
	return out, nil
}

// varlenStats walks the length prefixes of a plain varlen chunk and returns the
// number of values and how many bytes they occupy in total. Both are needed to
// size the result and its backing buffer before any value is placed. On a
// truncated chunk nvals is the index where it happened.
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

// stringAt is the string at b[off:off+n] without copying it. A zero length value
// returns "" because taking the address of an empty slice's first element
// panics.
func stringAt(b []byte, off, n int) string {
	if n == 0 {
		return ""
	}
	return unsafe.String(&b[off], n)
}

func decodeFixedT[T any](b []byte, width int, convert func([]byte) T, d *dest) ([]T, error) {
	if len(b)%width != 0 {
		return nil, fmt.Errorf("keine: plain data length %d is not a multiple of %d", len(b), width)
	}
	out, _ := d.vals.([]T)
	out = sizedSlice(out, len(b)/width)
	d.vals = out
	for i := range out {
		out[i] = convert(b[i*width : (i+1)*width])
	}
	return out, nil
}

// DecodeRLEBitpack is the inverse of EncodeRLEBitpack. It unpacks n values.
func DecodeRLEBitpack(b []byte, n int) []bool {
	return decodeRLEBitpack(b, n, &dest{})
}

// decodeRLEBitpack is DecodeRLEBitpack writing into d. The bitmap only sets the
// true bits, so a reused destination is cleared first; bits left set by a
// previous read would be indistinguishable from the ones written now.
func decodeRLEBitpack(b []byte, n int, d *dest) []bool {
	out, _ := d.vals.([]bool)
	out = sizedSlice(out, n)
	d.vals = out
	clear(out)
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
	return decodeDelta(b, &dest{})
}

func decodeDelta(b []byte, d *dest) ([]int64, error) {
	if len(b)%8 != 0 {
		return nil, fmt.Errorf("keine: delta data length %d is not a multiple of 8", len(b))
	}
	out, _ := d.vals.([]int64)
	out = sizedSlice(out, len(b)/8)
	d.vals = out
	var prev int64
	for i := 0; i < len(out); i++ {
		diff := int64(binary.LittleEndian.Uint64(b[i*8:]))
		if i > 0 {
			diff += prev
		}
		out[i] = diff
		prev = diff
	}
	return out, nil
}

// DecodeOffsetBytes is the inverse of EncodeOffsetBytes.
func DecodeOffsetBytes(b []byte) ([][]byte, error) {
	return decodeOffsetBytes(b, &dest{})
}

func decodeOffsetBytes(b []byte, d *dest) ([][]byte, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("keine: offset bytes data truncated")
	}
	n := int(binary.LittleEndian.Uint32(b[:4]))
	hdr := 4 + 4*n
	if len(b) < hdr {
		return nil, fmt.Errorf("keine: offset bytes header truncated")
	}
	// Read each offset where it is used rather than unpacking the whole column of
	// them first, which would allocate a slice nobody keeps.
	at := func(i int) int { return int(binary.LittleEndian.Uint32(b[4+4*i:])) }
	raw := b[hdr:]

	out, _ := d.vals.([][]byte)
	out = sizedSlice(out, n)
	d.vals = out
	for i := 0; i < n; i++ {
		start := at(i)
		end := len(raw)
		if i+1 < n {
			end = at(i + 1)
		}
		if start < 0 || end < start || end > len(raw) {
			return nil, fmt.Errorf("keine: invalid offset %d in offset bytes data", start)
		}
		out[i] = append([]byte(nil), raw[start:end]...)
	}
	return out, nil
}

// DecodeAffix is the inverse of EncodeAffix. Each value is its prefix, its
// middle and its suffix, and the middles form a length-prefixed stream, so the
// result and its backing buffer are sized before the first value is read. As in
// the plain path, strings are slices of that buffer and byte slices each get
// their own copy, since a caller writing one would clobber its neighbours.
func DecodeAffix(b []byte, typ uint8) (any, error) {
	return decodeAffix(b, typ, &dest{})
}

func decodeAffix(b []byte, typ uint8, d *dest) (any, error) {
	if len(b) < 8 {
		return nil, fmt.Errorf("keine: affix data is %d bytes, too small for a header", len(b))
	}
	preLen := binary.LittleEndian.Uint32(b[:4])
	b = b[4:]
	if uint64(len(b)) < uint64(preLen)+4 {
		return nil, fmt.Errorf("keine: affix header wants %d bytes of prefix, data is %d", preLen, len(b))
	}
	pre := b[:preLen]
	b = b[preLen:]
	sufLen := binary.LittleEndian.Uint32(b[:4])
	b = b[4:]
	if uint64(len(b)) < uint64(sufLen) {
		return nil, fmt.Errorf("keine: affix header wants %d bytes of suffix, data is %d", sufLen, len(b))
	}
	suf := b[:sufLen]
	b = b[sufLen:]

	nvals, total, ok := varlenStats(b)
	if !ok {
		return nil, fmt.Errorf("keine: affix middles truncated at value %d", nvals)
	}

	if typ == TypeString {
		size := nvals*(len(pre)+len(suf)) + total
		text := sizedSlice(d.buf, size)
		d.buf = text
		out, _ := d.vals.([]string)
		out = sizedSlice(out, nvals)
		d.vals = out
		off := 0
		for i := 0; i < nvals; i++ {
			n := int(binary.LittleEndian.Uint32(b[:4]))
			b = b[4:]
			region := text[off : off+len(pre)+n+len(suf)]
			copy(region, pre)
			copy(region[len(pre):], b[:n])
			copy(region[len(pre)+n:], suf)
			out[i] = stringAt(region, 0, len(region))
			off += len(region)
			b = b[n:]
		}
		return out, nil
	}

	out, _ := d.vals.([][]byte)
	out = sizedSlice(out, nvals)
	d.vals = out
	for i := 0; i < nvals; i++ {
		n := int(binary.LittleEndian.Uint32(b[:4]))
		b = b[4:]
		v := make([]byte, 0, len(pre)+n+len(suf))
		v = append(v, pre...)
		v = append(v, b[:n]...)
		v = append(v, suf...)
		out[i] = v
		b = b[n:]
	}
	return out, nil
}

// DecodeDict is the inverse of EncodeDict. A dictionary carries its entries but
// not the type of the values they encode, so the values come back as the bytes
// they were keyed on. A reader that knows the column's type decodes them through
// the typed path instead.
func DecodeDict(b []byte) ([][]byte, error) {
	vals, err := decodeDictTyped(b, TypeBytes, &dest{})
	if err != nil {
		return nil, err
	}
	return vals.([][]byte), nil
}

// decodeDictTyped decodes a dictionary chunk into the values of typ. Entries hold
// the encoded form of a value, little-endian for the fixed width types and raw
// bytes for strings and byte slices. A fixed width type's entries are laid out in
// value order and decoded as one plain column; a string column converts each
// distinct entry once rather than once per value.
func decodeDictTyped(b []byte, typ uint8, d *dest) (any, error) {
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
	indices := decodeBitunpack(b[12:12+idxBytes], nvals, uint(nbits), d)

	// The entries are the dictionary's own values, never returned to the caller,
	// so they go into a destination of their own.
	entries, err := decodeOffsetBytes(b[12+idxBytes:], &dest{})
	if err != nil {
		return nil, err
	}
	if uint32(len(entries)) != dictSize {
		return nil, fmt.Errorf("keine: dict size mismatch: header says %d, found %d", dictSize, len(entries))
	}

	if fixedWidth(typ) == 0 {
		// Strings and byte slices key on their own bytes, so the entries are the
		// values already. A string column converts each distinct entry once, and
		// every value that repeats it shares that string.
		if typ == TypeString {
			dict := make([]string, len(entries))
			for i, e := range entries {
				dict[i] = string(e)
			}
			return indexDict(dict, indices, d)
		}
		return indexDict(entries, indices, d)
	}
	return fixedDictEntries(entries, indices, typ, d)
}

// indexDict maps each index to its entry.
func indexDict[T any](dict []T, indices []uint32, d *dest) ([]T, error) {
	out, _ := d.vals.([]T)
	out = sizedSlice(out, len(indices))
	d.vals = out
	for i, idx := range indices {
		if int(idx) >= len(dict) {
			return nil, fmt.Errorf("keine: dict index %d out of range", idx)
		}
		out[i] = dict[idx]
	}
	return out, nil
}

// fixedDictEntries lays a fixed width type's entries out in value order so the
// chunk decodes as one plain column of the declared type. An entry of the wrong
// width means the file disagrees with its own schema, so decoding stops rather
// than letting a short entry shift every value after it.
func fixedDictEntries(entries [][]byte, indices []uint32, typ uint8, d *dest) (any, error) {
	width := fixedWidth(typ)
	// The flat stream is the plain column this chunk decodes as. It reuses d.buf,
	// the buffer a string column would have needed, which a fixed width column
	// never uses in the same decode.
	flat := sizedSlice(d.buf, len(indices)*width)
	d.buf = flat
	off := 0
	for _, idx := range indices {
		if int(idx) >= len(entries) {
			return nil, fmt.Errorf("keine: dict index %d out of range", idx)
		}
		e := entries[idx]
		if len(e) != width {
			return nil, fmt.Errorf("keine: dict entry is %d bytes, a value of type %d is %d", len(e), typ, width)
		}
		copy(flat[off:off+width], e)
		off += width
	}
	return decodePlainTyped(flat, typ, d)
}

// fixedWidth is the encoded width of a fixed width type, and zero for the types
// whose values carry their own length.
func fixedWidth(typ uint8) int {
	switch typ {
	case TypeBool, TypeInt8, TypeUint8:
		return 1
	case TypeInt16, TypeUint16:
		return 2
	case TypeInt32, TypeUint32, TypeFloat32:
		return 4
	case TypeInt64, TypeUint64, TypeFloat64:
		return 8
	}
	return 0
}

// bitunpack is the inverse of bitpackIndices. b must hold n values at nbits each,
// a length the caller has already checked, so the loop assembles bytes into
// values instead of testing one bit at a time. The accumulator never holds more
// than nbits + 7 bits, which fits in a uint64 for every width a uint32 index can
// need.
func bitunpack(b []byte, n int, nbits uint) []uint32 {
	return decodeBitunpack(b, n, nbits, &dest{})
}

// decodeBitunpack is bitunpack writing into d. As in the bitmap path, a reused
// destination is cleared first, because nbits == 0 means every index is zero and
// returns early without writing any of them.
func decodeBitunpack(b []byte, n int, nbits uint, d *dest) []uint32 {
	out := sizedSlice(d.idx, n)
	d.idx = out
	clear(out)
	if nbits == 0 {
		return out
	}
	var acc uint64
	var bits uint
	for i := 0; i < n; i++ {
		for bits < nbits {
			acc = acc<<8 | uint64(b[0])
			b = b[1:]
			bits += 8
		}
		out[i] = uint32(acc >> (bits - nbits))
		bits -= nbits
		acc &= (1 << bits) - 1
	}
	return out
}

// decodeTyped dispatches a chunk to the decoder for enc and returns the values as
// the Go type that decoder produces, []int64 from EncDelta and []string from
// EncDict. n is the number of values in the chunk. Pass a fresh d when the caller
// keeps the result, because every value returned points into it.
func decodeTyped(enc uint8, data []byte, n int, typ uint8, d *dest) (any, error) {
	switch enc {
	case EncPlain:
		return decodePlainTyped(data, typ, d)
	case EncRLEBitpack:
		return decodeRLEBitpack(data, n, d), nil
	case EncDelta:
		return decodeDelta(data, d)
	case EncDict:
		return decodeDictTyped(data, typ, d)
	case EncAffix:
		return decodeAffix(data, typ, d)
	case EncOffsetBytes:
		return decodeOffsetBytes(data, d)
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
		return true
	case EncAffix:
		return typ == TypeString || typ == TypeBytes
	case EncOffsetBytes:
		return typ == TypeBytes
	default:
		return false
	}
}

// decodeWith dispatches data to the decoder for enc, then converts the values
// to the Go types implied by typ. n is the number of values in the chunk.
func decodeWith(enc uint8, data []byte, n int, typ uint8) ([]any, error) {
	typed, err := decodeTyped(enc, data, n, typ, &dest{})
	if err != nil {
		return nil, err
	}
	return boxColumn(typed, enc, typ)
}

// boxValues converts a typed slice, []int64, []string and so on, into []any
// without copying a value. Each interface header points at its element where it
// already sits in the source slice, so a column costs one allocation instead of
// one per value.
//
// Interior pointers into s are safe because the decoders build a fresh slice for
// every column and the caller takes ownership of it, so nothing reuses that
// backing array. A type the format never uses has no values to box, and the
// decoders only ever pass one of the thirteen.
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

	// Converting the zero value once yields the type pointer every interface in
	// the column then shares. Converting a real element would give the same
	// pointer and cost the same single allocation.
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
// It is the empty counterpart of runtime.iface, with a type pointer in place of
// an itab.
type efaceHeader struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

// asValues converts a typed slice into []T. The second result is false when the
// slice's element type is not T, so a wrong type is an error rather than a
// silent mismatch.
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
	// without checking each value.
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

// coerceBytes returns the raw bytes of v. A string holding the fmt "%v" form of
// a byte slice, which is what a dictionary encoded byte column decodes to, is
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
