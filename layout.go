package keine

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/everymemory/keine/tokenizer"
)

// LayoutResult is the measured outcome of encoding one column with a single
// encoding and compression codec. Rep and Bound name which of the tokenized
// layouts the measurement made the winner, and are zero for every other encoding.
type LayoutResult struct {
	Name           string
	EncodedSize    int
	CompressedSize int
	EncodeNs       int64
	DecodeNs       int64
	Encoding       uint8
	Compress       uint8
	Rep            uint8
	Bound          uint8
}

// layoutCandidates returns the encodings worth trying for a column type. A column
// with a tokenizer also gets Tokenized, whose nine layouts are measured together
// because they share one tokenization.
func layoutCandidates(typ uint8, tokenized bool) []uint8 {
	switch typ {
	case TypeBool:
		return []uint8{EncRLEBitpack, EncPlain}
	case TypeInt8, TypeInt16, TypeInt32, TypeInt64,
		TypeUint8, TypeUint16, TypeUint32, TypeUint64:
		return []uint8{EncPlain, EncDelta, EncDict}
	case TypeFloat32, TypeFloat64:
		return []uint8{EncPlain, EncDict}
	case TypeBytes:
		out := []uint8{EncOffsetBytes, EncDict, EncAffix}
		if tokenized {
			out = append(out, EncTokenized)
		}
		return out
	case TypeString:
		out := []uint8{EncPlain, EncOffsetBytes, EncDict, EncAffix}
		if tokenized {
			out = append(out, EncTokenized)
		}
		return out
	default:
		return nil
	}
}

func encName(enc uint8) string {
	switch enc {
	case EncPlain:
		return "Plain"
	case EncRLEBitpack:
		return "RLEBitpack"
	case EncDelta:
		return "Delta"
	case EncDict:
		return "Dict"
	case EncOffsetBytes:
		return "OffsetBytes"
	case EncAffix:
		return "Affix"
	case EncTokenized:
		return "Tokenized"
	default:
		return fmt.Sprintf("Enc%d", enc)
	}
}

func codecName(codec uint8) string {
	switch codec {
	case CompressNone:
		return "None"
	case CompressZstd:
		return "Zstd"
	case CompressFlate:
		return "Flate"
	case CompressGzip:
		return "Gzip"
	case CompressZlib:
		return "Zlib"
	case CompressLzw:
		return "Lzw"
	default:
		return fmt.Sprintf("Codec%d", codec)
	}
}

// layoutCodecs are the codecs considered for each column. CompressZstd is
// missing because this toolchain does not ship compress/zstd. Gzip and Zlib wrap
// the same DEFLATE core as Flate in extra framing, so they are always larger and
// never win on size. They stay implemented for reading and for callers that ask
// for them by name.
var layoutCodecs = []uint8{
	CompressNone,
	CompressFlate,
	CompressLzw,
}

// defaultLayout is the layout a writer uses for a column it has not been asked
// to Optimize. It follows the column's type rather than its values, so it costs
// nothing to choose and cannot be misled by a sample, but it also cannot tell
// that a column of sorted integers is monotonic or that a column of repeated
// strings belongs in a dictionary. Optimize spends the time to learn that.
//
// Bool always packs to a bit and Delta always costs one subtraction per value,
// so both are right for any data of their type. Plain is the fallback for
// floating point and bytes, whose values this package makes no assumption about.
// Flate is the codec because it is the stdlib's general purpose compressor.
func defaultLayout(typ uint8) (enc, codec uint8) {
	switch typ {
	case TypeBool:
		return EncRLEBitpack, CompressFlate
	case TypeInt8, TypeInt16, TypeInt32, TypeInt64,
		TypeUint8, TypeUint16, TypeUint32, TypeUint64:
		return EncDelta, CompressFlate
	default:
		return EncPlain, CompressFlate
	}
}

// BenchmarkLayouts measures every candidate encoding and codec for col at the
// default compression level and returns them sorted by CompressedSize, breaking
// ties by EncodedSize. The tiebreak never looks at DecodeNs. Decode time is
// measurement noise, so using it could flip two equally sized candidates between
// runs and make the same input write two different files. tok is the tokenizer a
// tokenized column would be stored through, nil for a column that is none, in
// which case Tokenized is not among the candidates.
func BenchmarkLayouts(col []any, schema ColumnSchema, tok *tokenizer.Model) []LayoutResult {
	typed, err := canonicalColumnTyped(col, schema.Type)
	if err != nil {
		return nil
	}
	return benchmarkLayoutsTyped(typed, schema, tok, &measureScratch{}, flateLevel)
}

// benchmarkLayoutsTyped is BenchmarkLayouts for an already canonical column, so
// a writer that has already coerced the values measures them once instead of
// twice. ms is the scratch buffer the measurements reuse, held for the whole
// pass. level is the level the column will actually be compressed at, since
// measuring at another level would not rank the layout the file gets.
func benchmarkLayoutsTyped(col any, schema ColumnSchema, tok *tokenizer.Model, ms *measureScratch, level int) []LayoutResult {
	var results []LayoutResult
	for _, enc := range layoutCandidates(schema.Type, tok != nil) {
		if enc == EncTokenized {
			// The nine layouts of one tokenized column share the ids, so they are
			// measured apart from the per-encoding loop, which builds a stream
			// once and serves every codec from it.
			results = append(results, tokenizedLayoutResults(col, schema.Type, tok, ms, level)...)
			continue
		}
		// One encoding serves every codec, since they all consume the same bytes.
		// Encoding inside the codec loop would build the same byte stream once per
		// codec, and Dict's dictionary is most of the pass's time and allocations.
		encoded, err := encodeWith(enc, col, schema.Type)
		if err != nil {
			continue
		}
		n := sliceLen(col)
		for _, codec := range layoutCodecs {
			r, ok := measureEncoded(encoded, enc, codec, level, n, schema.Type, ms, tok)
			if ok {
				results = append(results, r)
			}
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].CompressedSize != results[j].CompressedSize {
			return results[i].CompressedSize < results[j].CompressedSize
		}
		return results[i].EncodedSize < results[j].EncodedSize
	})
	return results
}

// tokenizedLayoutResults measures the nine ways a tokenized column can lay out
// its ids and its value boundaries: three representations of the ids against
// three boundary streams. The tokenizer is the cost of the column, and every
// layout renders from the same ids, so it runs once here rather than once per
// candidate. What differs between layouts is what the comparison is on: the
// encoded size, the boundary stream the codec sees, and the table a layout has
// to carry. A column that cannot be tokenized has no results, which leaves the
// other candidates to win on their own.
//
// The winner is settled by size alone, the same rule every other candidate is
// ranked by; the tokenizer's own throughput and the decode time it costs are
// recorded but never the tiebreak, so a column that is slower to read is only
// stored as ids when it is also smaller.
func tokenizedLayoutResults(col any, typ uint8, tok *tokenizer.Model, ms *measureScratch, level int) []LayoutResult {
	strings, byts, err := tokenizedValues(col)
	if err != nil {
		return nil
	}
	ids, escapes, counts := tokenizedColumn(strings, byts, tok)
	n := len(counts)

	var results []LayoutResult
	for _, rep := range []uint8{TokenizedRaw, TokenizedRemap, TokenizedBits} {
		for _, bound := range []uint8{TokenizedCounts, TokenizedOffsets, TokenizedDeltas} {
			// The nine layouts are all well formed and all render from the same ids,
			// so neither the render nor the round trip after it can fail here; the
			// codecs are the same fixed list every other encoding is measured
			// through. A layout that could not be read back would be a bug in the
			// payload, not a candidate to skip.
			encoded := encodeTokenizedColumn(ids, escapes, counts, TokenizedLayout{Rep: rep, Bound: bound})
			for _, codec := range layoutCodecs {
				r, _ := measureEncoded(encoded, EncTokenized, codec, level, n, typ, ms, tok)
				r.Rep, r.Bound = rep, bound
				results = append(results, r)
			}
		}
	}
	return results
}

// measureLayout measures one encoding and codec for a column.
// measureLayout measures one encoding and codec of col. tok is the tokenizer a
// tokenized column is read back with, nil for every other encoding.
func measureLayout(col any, schema ColumnSchema, enc, codec uint8, level int, ms *measureScratch, tok *tokenizer.Model) (LayoutResult, bool) {
	encoded, err := encodeWith(enc, col, schema.Type)
	if err != nil {
		return LayoutResult{}, false
	}
	return measureEncoded(encoded, enc, codec, level, sliceLen(col), schema.Type, ms, tok)
}

// measureEncoded measures one encoding and codec for a column already in its
// encoded form. Every codec consumes the same bytes, so one encode serves all of
// them through this entry point. The compressed bytes come from ms and are only
// borrowed for the round trip; the decompressed column is allocated fresh.
func measureEncoded(encoded []byte, enc, codec uint8, level, n int, typ uint8, ms *measureScratch, tok *tokenizer.Model) (LayoutResult, bool) {
	encodeStart := time.Now().UnixNano()
	compressed, err := ms.compress(encoded, codec, level)
	if err != nil {
		return LayoutResult{}, false
	}
	encodeEnd := time.Now().UnixNano()

	// Decompress accepts the same codecs as Compress.
	raw, _ := Decompress(compressed, codec)
	if _, err := decodeWith(enc, raw, n, typ, tok); err != nil {
		return LayoutResult{}, false
	}
	decodeEnd := time.Now().UnixNano()

	return LayoutResult{
		Name:           encName(enc) + "+" + codecName(codec),
		EncodedSize:    len(encoded),
		CompressedSize: len(compressed),
		EncodeNs:       encodeEnd - encodeStart,
		DecodeNs:       decodeEnd - encodeEnd,
		Encoding:       enc,
		Compress:       codec,
	}, true
}

// sliceLen is the element count of a typed column arriving as an any.
func sliceLen(typed any) int {
	return reflect.ValueOf(typed).Len()
}
