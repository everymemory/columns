package columns

import (
	"encoding/binary"
	"fmt"
	"math"
	"unsafe"

	"github.com/everymemory/columns/tokenizer"
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
// alone do not say how many values they hold, and the count is walked out of
// the data because the caller has no other source for it.
func DecodePlain(b []byte, typ uint8) ([]any, error) {
	typed, err := decodePlainTyped(b, typ, &dest{}, 0)
	if err != nil {
		return nil, err
	}
	return boxValues(typed), nil
}

// decodePlainTyped decodes plain data as a slice of the Go type for typ, []int64
// for TypeInt64 and so on, rather than []any. DecodePlain boxes the result; the
// typed reader uses this to skip boxing.
//
// n is the number of values in the data when the caller already knows it, which
// a reader gets from the row group's row count. Zero means the data is the only
// authority and the count has to be walked out of it first.
func decodePlainTyped(b []byte, typ uint8, d *dest, n int) (any, error) {
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
		return decodePlainVarlen(b, typ, d, n)
	default:
		return nil, fmt.Errorf("columns: unknown type %d", typ)
	}
}

// decodePlainVarlen decodes the length-prefixed plain form used by strings and
// byte slices. n is the value count the caller already knows, which a reader has
// from the row group's row count; zero means the data is the only authority and
// the count has to be walked out of it. Either source gives the byte total up
// front, so the result and its backing buffer are sized before any value is
// placed. Strings point into that shared buffer instead of copying, while byte
// slices still copy because they are mutable and a caller writing one would
// clobber its neighbours.
func decodePlainVarlen(b []byte, typ uint8, d *dest, n int) (any, error) {
	var total int
	if n > 0 {
		// Each value costs four bytes of prefix plus its own bytes, so the count
		// and the stream length settle the total between them. A stream too short
		// for the count cannot hold it; one too long holds values the count does
		// not admit, and the walk below reports that rather than this check.
		if total = len(b) - 4*n; total < 0 {
			return nil, fmt.Errorf("columns: plain data is %d bytes, too few for %d values", len(b), n)
		}
	} else {
		var ok bool
		n, total, ok = varlenStats(b)
		if !ok {
			return nil, fmt.Errorf("columns: plain data truncated at value %d", n)
		}
	}

	if typ == TypeString {
		// One buffer holds every value's bytes and each string points into it.
		// Strings are immutable, so sharing cannot corrupt a value, and N distinct
		// strings cost two allocations instead of N+1.
		text := sizedSlice(d.buf, total)
		d.buf = text
		out, _ := d.vals.([]string)
		out = sizedSlice(out, n)
		d.vals = out
		var off int
		for i := 0; i < n; i++ {
			val, rest, err := plainValue(b, i)
			if err != nil {
				return nil, err
			}
			// The byte budget was settled by the count rather than by the prefixes,
			// so a value that spends more of it than the count allows is what a
			// stream holding fewer values than n claims looks like.
			if off+len(val) > total {
				return nil, fmt.Errorf("columns: plain data truncated at value %d", i)
			}
			b = rest
			copy(text[off:off+len(val)], val)
			out[i] = stringAt(text, off, len(val))
			off += len(val)
		}
		if len(b) > 0 {
			return nil, fmt.Errorf("columns: plain data holds more than the %d values expected", n)
		}
		return out, nil
	}

	out, _ := d.vals.([][]byte)
	out = sizedSlice(out, n)
	d.vals = out
	for i := 0; i < n; i++ {
		val, rest, err := plainValue(b, i)
		if err != nil {
			return nil, err
		}
		b = rest
		out[i] = append([]byte(nil), val...)
	}
	if len(b) > 0 {
		return nil, fmt.Errorf("columns: plain data holds more than the %d values expected", n)
	}
	return out, nil
}

// plainValue is the length-prefixed value at the start of b along with the bytes
// that follow it. i is the value's position, which is all the truncation error
// has to report.
func plainValue(b []byte, i int) (val, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, fmt.Errorf("columns: plain data truncated at value %d", i)
	}
	n := int(binary.LittleEndian.Uint32(b[:4]))
	b = b[4:]
	if len(b) < n {
		return nil, nil, fmt.Errorf("columns: plain data truncated at value %d", i)
	}
	return b[:n], b[n:], nil
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
		return nil, fmt.Errorf("columns: plain data length %d is not a multiple of %d", len(b), width)
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
		return nil, fmt.Errorf("columns: delta data length %d is not a multiple of 8", len(b))
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
		return nil, fmt.Errorf("columns: offset bytes data truncated")
	}
	n := int(binary.LittleEndian.Uint32(b[:4]))
	hdr := 4 + 4*n
	if len(b) < hdr {
		return nil, fmt.Errorf("columns: offset bytes header truncated")
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
			return nil, fmt.Errorf("columns: invalid offset %d in offset bytes data", start)
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
		return nil, fmt.Errorf("columns: affix data is %d bytes, too small for a header", len(b))
	}
	preLen := binary.LittleEndian.Uint32(b[:4])
	b = b[4:]
	if uint64(len(b)) < uint64(preLen)+4 {
		return nil, fmt.Errorf("columns: affix header wants %d bytes of prefix, data is %d", preLen, len(b))
	}
	pre := b[:preLen]
	b = b[preLen:]
	sufLen := binary.LittleEndian.Uint32(b[:4])
	b = b[4:]
	if uint64(len(b)) < uint64(sufLen) {
		return nil, fmt.Errorf("columns: affix header wants %d bytes of suffix, data is %d", sufLen, len(b))
	}
	suf := b[:sufLen]
	b = b[sufLen:]

	nvals, total, ok := varlenStats(b)
	if !ok {
		return nil, fmt.Errorf("columns: affix middles truncated at value %d", nvals)
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
		return nil, fmt.Errorf("columns: dict data truncated")
	}
	nvals := int(binary.LittleEndian.Uint32(b[:4]))
	dictSize := binary.LittleEndian.Uint32(b[4:8])
	nbits := binary.LittleEndian.Uint32(b[8:12])
	idxBytes := int((uint64(nvals)*uint64(nbits) + 7) / 8)
	if len(b) < 12+idxBytes {
		return nil, fmt.Errorf("columns: dict indices truncated")
	}
	indices := decodeBitunpack(b[12:12+idxBytes], nvals, uint(nbits), d)

	// The entries are the dictionary's own values, never returned to the caller,
	// so they go into a destination of their own.
	entries, err := decodeOffsetBytes(b[12+idxBytes:], &dest{})
	if err != nil {
		return nil, err
	}
	if uint32(len(entries)) != dictSize {
		return nil, fmt.Errorf("columns: dict size mismatch: header says %d, found %d", dictSize, len(entries))
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
			return nil, fmt.Errorf("columns: dict index %d out of range", idx)
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
			return nil, fmt.Errorf("columns: dict index %d out of range", idx)
		}
		e := entries[idx]
		if len(e) != width {
			return nil, fmt.Errorf("columns: dict entry is %d bytes, a value of type %d is %d", len(e), typ, width)
		}
		copy(flat[off:off+width], e)
		off += width
	}
	return decodePlainTyped(flat, typ, d, len(indices))
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
func decodeTyped(enc uint8, data []byte, n int, typ uint8, d *dest, tok *tokenizer.Model) (any, error) {
	switch enc {
	case EncPlain:
		return decodePlainTyped(data, typ, d, n)
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
	case EncTokenized:
		if tok == nil {
			return nil, fmt.Errorf("columns: tokenized chunk with no tokenizer to read it with")
		}
		return DecodeTokenized(data, tok, typ)
	default:
		return nil, fmt.Errorf("columns: unknown encoding %d", enc)
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
	case EncTokenized:
		return typ == TypeString || typ == TypeBytes
	default:
		return false
	}
}

// decodeWith dispatches data to the decoder for enc, then converts the values
// to the Go types implied by typ. n is the number of values in the chunk. tok is
// the tokenizer a tokenized column was written with, nil for every other one.
func decodeWith(enc uint8, data []byte, n int, typ uint8, tok *tokenizer.Model) ([]any, error) {
	typed, err := decodeTyped(enc, data, n, typ, &dest{}, tok)
	if err != nil {
		return nil, err
	}
	return boxColumn(typed, enc, typ)
}
