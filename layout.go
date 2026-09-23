package keine

import (
	"fmt"
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
		return []uint8{EncPlain}
	case TypeBytes:
		return []uint8{EncOffsetBytes, EncDict}
	case TypeString:
		return []uint8{EncPlain, EncOffsetBytes, EncDict}
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
// absent because this toolchain does not ship compress/zstd.
var layoutCodecs = []uint8{
	CompressNone,
	CompressFlate,
	CompressGzip,
	CompressZlib,
	CompressLzw,
}

// BenchmarkLayouts measures every candidate encoding and codec for col and
// returns them sorted by CompressedSize, breaking ties by DecodeNs.
func BenchmarkLayouts(col []any, schema ColumnSchema) []LayoutResult {
	var results []LayoutResult
	for _, enc := range layoutCandidates(schema.Type) {
		for _, codec := range layoutCodecs {
			r, ok := measureLayout(col, schema, enc, codec)
			if ok {
				results = append(results, r)
			}
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].CompressedSize != results[j].CompressedSize {
			return results[i].CompressedSize < results[j].CompressedSize
		}
		return results[i].DecodeNs < results[j].DecodeNs
	})
	return results
}

// ExperimentLayouts returns the BenchmarkLayouts entry with the smallest
// CompressedSize, or Plain+None when no candidate works.
func ExperimentLayouts(col []any, schema ColumnSchema) LayoutResult {
	results := BenchmarkLayouts(col, schema)
	if len(results) == 0 {
		return LayoutResult{Name: encName(EncPlain) + "+" + codecName(CompressNone), Encoding: EncPlain, Compress: CompressNone}
	}
	return results[0]
}

func measureLayout(col []any, schema ColumnSchema, enc, codec uint8) (LayoutResult, bool) {
	encodeStart := time.Now().UnixNano()
	encoded, err := encodeWith(enc, col, schema.Type)
	if err != nil {
		return LayoutResult{}, false
	}
	compressed, err := Compress(encoded, codec)
	if err != nil {
		return LayoutResult{}, false
	}
	encodeEnd := time.Now().UnixNano()

	// Decompress accepts the same codecs as Compress.
	raw, _ := Decompress(compressed, codec)
	if _, err := decodeWith(enc, raw, len(col), schema.Type); err != nil {
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
