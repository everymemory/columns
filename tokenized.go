package keine

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"sort"

	"github.com/everymemory/keine/tokenizer"
)

// A Tokenized column stores a string or bytes column as the token ids a tokenizer
// maps its values to, rather than as the values' own bytes. The ids and the
// boundaries between values are laid out in a fixed-width, language agnostic
// form: a two byte header naming the representation and the boundary layout, then
// five little-endian uint32 counts, then the three streams they size.
//
// The whole payload is the chunk's encoded data, so it goes through the same block
// splitting and the same codecs every other encoding does. Nothing here is a
// tokenizer-specific compression path.
//
// A byte value with no token in the vocabulary is not dropped, the way the
// tokenizer's own pipeline drops it. The tokenizer carries such a byte out of the
// id stream as a marker id plus the raw byte, and this payload keeps that split:
// the marker travels with the ids and the byte travels in the escape stream, one
// entry per marker, in order. Bit-packing and remapping the ids only ever touch
// ids, so a raw byte never has to be squeezed into one.

// Representation tags for the token id stream.
const (
	// TokenizedRaw stores every id as a little-endian uint16, which is the width
	// the vocabulary is defined in. It is the representation that costs nothing to
	// build and nothing to decode, and the one every other one is measured
	// against.
	TokenizedRaw uint8 = 0

	// TokenizedRemap stores the distinct ids of the chunk in a table, ordered by
	// how often each one appears, and every token as the varint of its table
	// index. An id that appears often lands on a one byte index even though it is
	// a two byte id, so the table is what the size of a text column buys back.
	TokenizedRemap uint8 = 1

	// TokenizedBits packs every id at the width the chunk's own largest id needs,
	// which is usually well below sixteen bits. No table is stored, because the
	// width and the count settle the whole stream.
	TokenizedBits uint8 = 2
)

// Boundary tags for the layout that separates one value's ids from the next's.
const (
	// TokenizedCounts stores one uint32 per value holding its token count.
	TokenizedCounts uint8 = 0

	// TokenizedOffsets stores one uint32 per value holding its offset into the id
	// stream. The stream's own length closes the last value.
	TokenizedOffsets uint8 = 1

	// TokenizedDeltas stores the first value's token count and then each value's
	// difference from the one before it, one uint32 each, zigzag encoded. Values
	// whose length varies little from row to row make most of these small or zero,
	// which is what the codec that follows compresses well. The counts are not
	// monotonic, so an unsigned delta would underflow on a value shorter than the
	// one before it and store four billion where it means minus one; zigzag keeps
	// the magnitude of the difference instead.
	//
	// Delta-coding the offsets is not a fourth layout: the difference between two
	// consecutive offsets is the count of the first value, so it is the counts
	// stream with a zero in front of it.
	TokenizedDeltas uint8 = 2
)

const (
	// tokenizedHeaderLen is the fixed part of the payload: the two layout tags and
	// the five counts that size the streams that follow.
	tokenizedHeaderLen = 2 + 4 + 4 + 4 + 4
)

// TokenizedLayout is the layout a Tokenized column is stored in. It is what the
// optimizer picks per column and what a reader needs to interpret the payload.
type TokenizedLayout struct {
	Rep   uint8
	Bound uint8
}

// EncodeTokenized stores vals through tok as one token id stream and one boundary
// stream, in the layout asked for. vals is a slice of the column's own type,
// []string for a TypeString column and [][]byte for a TypeBytes one; a value is
// tokenized on its bytes either way, because the byte-level mapping is defined on
// bytes.
func EncodeTokenized(vals any, tok *tokenizer.Model, layout TokenizedLayout) ([]byte, error) {
	strings, bytes, err := tokenizedValues(vals)
	if err != nil {
		return nil, err
	}
	switch layout.Rep {
	case TokenizedRaw, TokenizedRemap, TokenizedBits:
	default:
		return nil, fmt.Errorf("keine: tokenized representation %d is not one this build writes", layout.Rep)
	}
	switch layout.Bound {
	case TokenizedCounts, TokenizedOffsets, TokenizedDeltas:
	default:
		return nil, fmt.Errorf("keine: tokenized boundary %d is not one this build writes", layout.Bound)
	}

	ids, escapes, counts := tokenizedColumn(strings, bytes, tok)
	return encodeTokenizedColumn(ids, escapes, counts, layout), nil
}

// encodeTokenizedColumn is EncodeTokenized past the tokenizer: it renders ids,
// escapes and the per-value counts the tokenizer produced into the layout asked
// for. The nine layouts of one column all share a tokenization, and this is the
// part of the encode that differs between them, so a caller comparing layouts
// tokenizes once and renders nine.
func encodeTokenizedColumn(ids []uint16, escapes []byte, counts []uint32, layout TokenizedLayout) []byte {
	bound := tokenizedBoundaries(counts, layout.Bound)

	var idStream []byte
	var table []uint16
	switch layout.Rep {
	case TokenizedRaw:
		idStream = rawIDs(ids)
	case TokenizedRemap:
		idStream, table = remapIDs(ids)
	case TokenizedBits:
		idStream = bitpackIDs(ids)
		table = []uint16{largestID(ids)}
	}

	buf := make([]byte, 0, tokenizedHeaderLen+4*len(bound)+len(idStream)+len(escapes))
	buf = append(buf, layout.Rep, layout.Bound)
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(counts)))
	buf = append(buf, n[:]...)
	binary.LittleEndian.PutUint32(n[:], uint32(len(ids)))
	buf = append(buf, n[:]...)
	binary.LittleEndian.PutUint32(n[:], uint32(len(escapes)))
	buf = append(buf, n[:]...)
	binary.LittleEndian.PutUint32(n[:], uint32(len(table)))
	buf = append(buf, n[:]...)
	for i := 0; i < len(table); i++ {
		buf = append(buf, byte(table[i]), byte(table[i]>>8))
	}
	for _, b := range bound {
		buf = append(buf, byte(b), byte(b>>8), byte(b>>16), byte(b>>24))
	}
	buf = append(buf, idStream...)
	buf = append(buf, escapes...)
	return buf
}

// tokenizedValues returns vals as a []string and a [][]byte, one of which is the
// column's own form and the other nil. A caller handing over the wrong slice for
// the column's declared type is an error rather than a conversion, because the
// tokenizer is applied to bytes either way and a caller naming the type it did
// not supply has made a mistake worth reporting.
func tokenizedValues(vals any) ([]string, [][]byte, error) {
	switch s := vals.(type) {
	case []string:
		return s, nil, nil
	case [][]byte:
		return nil, s, nil
	}
	return nil, nil, fmt.Errorf("keine: tokenized encodes string and bytes columns, got %T", vals)
}

// tokenizedColumn tokenizes every value through tok and returns the flat id
// stream, the bytes the escape markers carry, and the token count of each value.
// The ids keep the escape markers and the bytes those markers stand for move into
// escapes, one per marker, so the id stream stays a stream of ids.
func tokenizedColumn(strings []string, byts [][]byte, tok *tokenizer.Model) (ids []uint16, escapes []byte, counts []uint32) {
	n := len(strings)
	if n == 0 {
		n = len(byts)
	}
	ids = make([]uint16, 0, n*8)
	escapes = make([]byte, 0)
	counts = make([]uint32, 0, n)
	if strings != nil {
		for _, v := range strings {
			valueIDs, valueEsc := tok.EncodeSplit([]byte(v))
			ids = append(ids, valueIDs...)
			escapes = append(escapes, valueEsc...)
			counts = append(counts, uint32(len(valueIDs)))
		}
		return ids, escapes, counts
	}
	for _, v := range byts {
		valueIDs, valueEsc := tok.EncodeSplit(v)
		ids = append(ids, valueIDs...)
		escapes = append(escapes, valueEsc...)
		counts = append(counts, uint32(len(valueIDs)))
	}
	return ids, escapes, counts
}

// tokenizedBoundaries lays out the per-value token counts as the boundary stream
// the tag asks for. Each entry is a uint32, so the three layouts are the same
// length and differ only in what the numbers are; the codec that follows is what
// makes one smaller than another.
func tokenizedBoundaries(counts []uint32, bound uint8) []uint32 {
	out := make([]uint32, len(counts))
	switch bound {
	case TokenizedCounts:
		copy(out, counts)
	case TokenizedOffsets:
		var off uint32
		for i, c := range counts {
			out[i] = off
			off += c
		}
	case TokenizedDeltas:
		var prev uint32
		for i, c := range counts {
			out[i] = zigzagDelta(c, prev)
			prev = c
		}
	}
	return out
}

// zigzagDelta is the difference between two counts as a uint32 that keeps the
// magnitude of the difference rather than its sign, so a negative delta is a
// small odd number instead of a wrap around the top of the range.
func zigzagDelta(c, prev uint32) uint32 {
	d := int64(c) - int64(prev)
	return uint32((d << 1) ^ (d >> 63))
}

// unzigzagDelta is the inverse of zigzagDelta. It returns the signed difference
// the caller accumulates, because the accumulation has to stay signed: the deltas
// of a counts stream can run below the running total only in a column whose
// values alternate long and short.
func unzigzagDelta(u uint32) int64 {
	return int64(u>>1) ^ -int64(u&1)
}

// largestID is the widest id in the stream, which is the width the bit-packed form
// packs at. An empty stream has none, and zero is the width that unpacks nothing.
func largestID(ids []uint16) uint16 {
	var maxID uint16
	for _, id := range ids {
		if id > maxID {
			maxID = id
		}
	}
	return maxID
}

// rawIDs is the little-endian uint16 pass, the representation that costs nothing to
// build and nothing to decode, and the one the other two are measured against.
func rawIDs(ids []uint16) []byte {
	out := make([]byte, 0, 2*len(ids))
	for _, id := range ids {
		out = append(out, byte(id), byte(id>>8))
	}
	return out
}

// bitpackIDs packs ids at ceil(log2(maxID+1)) bits each, most significant bit
// first, padded to a byte boundary. The width is settled by the stream itself, so
// the reader needs nothing beyond it and the count.
func bitpackIDs(ids []uint16) []byte {
	nbits := uint(bits.Len(uint(largestID(ids))))
	if nbits == 0 {
		return nil
	}
	out := make([]byte, (uint64(len(ids))*uint64(nbits)+7)/8)
	var acc uint64
	var bitsHeld uint
	p := 0
	for _, id := range ids {
		acc = acc<<nbits | uint64(id)
		bitsHeld += nbits
		for bitsHeld >= 8 {
			out[p] = byte(acc >> (bitsHeld - 8))
			p++
			bitsHeld -= 8
		}
		acc &= (1 << bitsHeld) - 1
	}
	if bitsHeld > 0 {
		out[p] = byte(acc << (8 - bitsHeld))
	}
	return out
}

// remapIDs builds a table of the distinct ids ordered by how often they appear,
// most frequent first, and writes each token as the varint of its index. The
// ordering is settled by the counts and, between ids that appear equally often,
// by the id itself, so the same column always remaps to the same table.
func remapIDs(ids []uint16) (stream []byte, table []uint16) {
	freq := make(map[uint16]uint32, len(ids))
	for _, id := range ids {
		freq[id]++
	}
	distinct := make([]uint16, 0, len(freq))
	for id := range freq {
		distinct = append(distinct, id)
	}
	sort.Slice(distinct, func(i, j int) bool {
		if freq[distinct[i]] != freq[distinct[j]] {
			return freq[distinct[i]] > freq[distinct[j]]
		}
		return distinct[i] < distinct[j]
	})

	index := make(map[uint16]uint16, len(distinct))
	for i, id := range distinct {
		index[id] = uint16(i)
	}
	var varint [binary.MaxVarintLen64]byte
	out := make([]byte, 0, len(ids))
	for _, id := range ids {
		out = append(out, varint[:binary.PutUvarint(varint[:], uint64(index[id]))]...)
	}
	return out, distinct
}

// DecodeTokenized is the inverse of EncodeTokenized. It returns the values as the
// slice of the column's own type, []string for a TypeString column and [][]byte
// for a TypeBytes one, so a reader that has already typed its result needs no
// conversion.
func DecodeTokenized(b []byte, tok *tokenizer.Model, typ uint8) (any, error) {
	if len(b) < tokenizedHeaderLen {
		return nil, fmt.Errorf("keine: tokenized data is %d bytes, too small for a header", len(b))
	}
	rep := b[0]
	bound := b[1]
	at := 2
	read32 := func() uint32 {
		v := binary.LittleEndian.Uint32(b[at : at+4])
		at += 4
		return v
	}
	valueCount := int(read32())
	tokenCount := int(read32())
	escapeCount := int(read32())
	tableLen := int(read32())

	// The four counts size three regions — the table, the boundaries, and the
	// escapes — and those have to fit inside the chunk before any of them is
	// trusted. The sum runs in uint64 because two uint32 counts times their element
	// widths can wrap a 32 bit int and pass the very check meant to catch them.
	need := uint64(at) + 2*uint64(tableLen) + 4*uint64(valueCount) + uint64(escapeCount)
	if need > uint64(len(b)) {
		return nil, fmt.Errorf("keine: tokenized header wants more data than the chunk holds")
	}
	tableStart := at
	tableEnd := tableStart + 2*tableLen
	boundEnd := tableEnd + 4*valueCount

	table := make([]uint16, tableLen)
	for i := 0; i < tableLen; i++ {
		table[i] = uint16(b[tableStart+2*i]) | uint16(b[tableStart+2*i+1])<<8
	}

	spans, err := tokenizedSpans(b[tableEnd:boundEnd], bound, valueCount, tokenCount)
	if err != nil {
		return nil, err
	}

	// The id stream is everything between the boundaries and the escapes, which
	// both the header and the streams' own lengths settle, so the reader does not
	// have to walk the ids to find where the escapes begin.
	idBytes := b[boundEnd : len(b)-escapeCount]
	escapes := b[len(b)-escapeCount:]

	ids, err := tokenizedReadIDs(idBytes, rep, tokenCount, table)
	if err != nil {
		return nil, err
	}

	// Each value's span of ids is contiguous, and the escapes it claims are the
	// ones its markers stand for, taken in order. A value with no marker takes no
	// byte, which is the common case: only bytes without a token of their own
	// produce one.
	escapeAt := 0
	out := make([]string, valueCount)
	for i := 0; i < valueCount; i++ {
		start, end := spans[i], spans[i+1]
		markers := 0
		for _, id := range ids[start:end] {
			if id == tok.EscapeID() {
				markers++
			}
		}
		if escapeAt+markers > escapeCount {
			return nil, fmt.Errorf("keine: tokenized value %d claims escapes the stream does not hold", i)
		}
		text, err := tok.DecodeSplit(ids[start:end], escapes[escapeAt:escapeAt+markers])
		if err != nil {
			return nil, fmt.Errorf("keine: decoding tokenized value %d: %w", i, err)
		}
		escapeAt += markers
		out[i] = text
	}
	if escapeAt != escapeCount {
		return nil, fmt.Errorf("keine: tokenized chunk has %d escape bytes no value claimed", escapeCount-escapeAt)
	}

	if typ == TypeBytes {
		byts := make([][]byte, valueCount)
		for i, s := range out {
			byts[i] = []byte(s)
		}
		return byts, nil
	}
	return out, nil
}

// tokenizedSpans turns the boundary stream into the half-open id offsets of each
// value, one more than the value count because the stream's own length closes the
// last value. Offsets are already that form; counts and deltas are accumulated
// into it. A boundary stream that does not land the last value at the token count
// the header records is a chunk that disagrees with itself.
func tokenizedSpans(b []byte, bound uint8, valueCount, tokenCount int) ([]int, error) {
	spans := make([]int, valueCount+1)
	read32 := func(i int) uint32 {
		return binary.LittleEndian.Uint32(b[4*i : 4*i+4])
	}
	switch bound {
	case TokenizedOffsets:
		var prev int
		for i := 0; i < valueCount; i++ {
			off := int(read32(i))
			if off < prev || off > tokenCount {
				return nil, fmt.Errorf("keine: tokenized offset %d is %d, outside %d..%d", i, off, prev, tokenCount)
			}
			spans[i] = off
			prev = off
		}
		spans[valueCount] = tokenCount
	case TokenizedCounts:
		var off int
		for i := 0; i < valueCount; i++ {
			off += int(read32(i))
			spans[i+1] = off
		}
		if off != tokenCount {
			return nil, fmt.Errorf("keine: tokenized counts sum to %d, header records %d", off, tokenCount)
		}
	case TokenizedDeltas:
		// The deltas are differences between consecutive counts, so recovering a
		// value's offset takes two accumulations: the deltas rebuild each count, and
		// the counts build the running offset.
		var off, count int64
		for i := 0; i < valueCount; i++ {
			count += unzigzagDelta(read32(i))
			if count < 0 {
				return nil, fmt.Errorf("keine: tokenized deltas give value %d a count of %d", i, count)
			}
			off += count
			if off > int64(tokenCount) {
				return nil, fmt.Errorf("keine: tokenized deltas put value %d at %d, past the %d ids the header records", i, off, tokenCount)
			}
			spans[i+1] = int(off)
		}
		if off != int64(tokenCount) {
			return nil, fmt.Errorf("keine: tokenized deltas sum to %d ids, header records %d", off, tokenCount)
		}
	default:
		return nil, fmt.Errorf("keine: tokenized boundary %d is not one this build reads", bound)
	}
	return spans, nil
}

// tokenizedReadIDs renders the stored stream back into the id slice the header
// counts. The three representations are the inverses of the three writers: a
// straight uint16 pass, an unpack at the width the table's largest id needs, and
// a varint walk through the table.
func tokenizedReadIDs(b []byte, rep uint8, tokenCount int, table []uint16) ([]uint16, error) {
	out := make([]uint16, tokenCount)
	switch rep {
	case TokenizedRaw:
		if len(b) != 2*tokenCount {
			return nil, fmt.Errorf("keine: tokenized id stream is %d bytes, %d ids take %d", len(b), tokenCount, 2*tokenCount)
		}
		for i := 0; i < tokenCount; i++ {
			out[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
	case TokenizedBits:
		// The width is the widest id the table maps back to, and the stream the
		// writer pads to a byte boundary, so its length settles the id count. The
		// length has to be checked here rather than after the unpack, because the
		// unpack reads until it has produced the count it was given.
		nbits := uint(bits.Len(uint(maxTableID(table))))
		want := (uint64(tokenCount)*uint64(nbits) + 7) / 8
		if uint64(len(b)) != want {
			return nil, fmt.Errorf("keine: tokenized id stream is %d bytes, %d ids at %d bits take %d", len(b), tokenCount, nbits, want)
		}
		for i, id := range decodeBitunpack(b, tokenCount, nbits, &dest{}) {
			out[i] = uint16(id)
		}
	case TokenizedRemap:
		at := 0
		for i := 0; i < tokenCount; i++ {
			idx, n := binary.Uvarint(b[at:])
			if n <= 0 {
				return nil, fmt.Errorf("keine: tokenized varint stream truncated at id %d", i)
			}
			if int(idx) >= len(table) {
				return nil, fmt.Errorf("keine: tokenized id index %d is outside the table of %d", idx, len(table))
			}
			out[i] = table[idx]
			at += n
		}
	default:
		return nil, fmt.Errorf("keine: tokenized representation %d is not one this build reads", rep)
	}
	return out, nil
}

// maxTableID is the largest id a remap table maps back to, which is the width the
// bit-packed form needs. An empty table means an empty column, and a zero width
// unpacks nothing.
func maxTableID(table []uint16) uint16 {
	var maxID uint16
	for _, id := range table {
		if id > maxID {
			maxID = id
		}
	}
	return maxID
}
