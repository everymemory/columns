package keine

import (
	"fmt"
	"reflect"
	"sort"
	"time"
)

// LayoutResult is the measured outcome of encoding one column with a single
// encoding and compression codec.
type LayoutResult struct {
	Name           string
	EncodedSize    int
	CompressedSize int
	EncodeNs       int64
	DecodeNs       int64
	Encoding       uint8
	Compress       uint8
}

// layoutCandidates returns the encodings worth trying for a column type.
func layoutCandidates(typ uint8) []uint8 {
	switch typ {
	case TypeBool:
		return []uint8{EncRLEBitpack, EncPlain}
	case TypeInt8, TypeInt16, TypeInt32, TypeInt64,
		TypeUint8, TypeUint16, TypeUint32, TypeUint64:
		return []uint8{EncPlain, EncDelta, EncDict}
	case TypeFloat32, TypeFloat64:
		return []uint8{EncPlain, EncDict}
	case TypeBytes:
		return []uint8{EncOffsetBytes, EncDict, EncAffix}
	case TypeString:
		return []uint8{EncPlain, EncOffsetBytes, EncDict, EncAffix}
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
// runs and make the same input write two different files.
func BenchmarkLayouts(col []any, schema ColumnSchema) []LayoutResult {
	typed, err := canonicalColumnTyped(col, schema.Type)
	if err != nil {
		return nil
	}
	return benchmarkLayoutsTyped(typed, schema, &measureScratch{}, flateLevel)
}

// benchmarkLayoutsTyped is BenchmarkLayouts for an already canonical column, so
// a writer that has already coerced the values measures them once instead of
// twice. ms is the scratch buffer the measurements reuse, held for the whole
// pass. level is the level the column will actually be compressed at, since
// measuring at another level would not rank the layout the file gets.
func benchmarkLayoutsTyped(col any, schema ColumnSchema, ms *measureScratch, level int) []LayoutResult {
	var results []LayoutResult
	for _, enc := range layoutCandidates(schema.Type) {
		for _, codec := range layoutCodecs {
			r, ok := measureLayout(col, schema, enc, codec, level, ms)
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

// measureLayout measures one encoding and codec for a column.
func measureLayout(col any, schema ColumnSchema, enc, codec uint8, level int, ms *measureScratch) (LayoutResult, bool) {
	encoded, err := encodeWith(enc, col, schema.Type)
	if err != nil {
		return LayoutResult{}, false
	}
	return measureEncoded(encoded, enc, codec, level, sliceLen(col), schema.Type, ms)
}

// measureEncoded measures one encoding and codec for a column already in its
// encoded form. Every codec consumes the same bytes, so one encode serves all of
// them through this entry point. The compressed bytes come from ms and are only
// borrowed for the round trip; the decompressed column is allocated fresh.
func measureEncoded(encoded []byte, enc, codec uint8, level, n int, typ uint8, ms *measureScratch) (LayoutResult, bool) {
	encodeStart := time.Now().UnixNano()
	compressed, err := ms.compress(encoded, codec, level)
	if err != nil {
		return LayoutResult{}, false
	}
	encodeEnd := time.Now().UnixNano()

	// Decompress accepts the same codecs as Compress.
	raw, _ := Decompress(compressed, codec)
	if _, err := decodeWith(enc, raw, n, typ); err != nil {
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
