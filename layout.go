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
// absent because this toolchain does not ship compress/zstd. Gzip and Zlib wrap
// the same DEFLATE core as Flate with extra framing, so they are strictly
// larger and never win the size contest; they stay implemented for reading and
// for callers that ask for them by name.
var layoutCodecs = []uint8{
	CompressNone,
	CompressFlate,
	CompressLzw,
}

// BenchmarkLayouts measures every candidate encoding and codec for col and
// returns them sorted by CompressedSize, breaking ties by EncodedSize and then
// by candidate order. The tiebreak never consults DecodeNs: that is measured
// time, and consulting it would make two equally sized candidates win or lose
// on noise, so the same column could pick a different layout on a different run
// and the same input would not write the same file twice.
func BenchmarkLayouts(col []any, schema ColumnSchema) []LayoutResult {
	typed, err := canonicalColumnTyped(col, schema.Type)
	if err != nil {
		return nil
	}
	return benchmarkLayoutsTyped(typed, schema)
}

// maxExperimentRows caps how many values ExperimentLayouts measures. Layout
// choice depends on the shape of a column, not its length: cardinality,
// monotonicity and value lengths all stabilise well below this many values, so
// a sample costs a fraction of the encode and decode passes while picking the
// same winner.
const maxExperimentRows = 8192

// experimentSample returns up to maxExperimentRows evenly spaced values from
// col. Even spacing rather than a prefix keeps the sample representative when a
// column is sorted or clustered.
func experimentSample(col []any) []any {
	return sampleTyped(col, maxExperimentRows)
}

// experimentSampleTyped is experimentSample for an already typed column.
func experimentSampleTyped(typed any) any {
	return sampleDispatch(typed, maxExperimentRows)
}

// sampleTyped strides through s, keeping up to max evenly spaced values.
func sampleTyped[T any](s []T, max int) []T {
	if len(s) <= max {
		return s
	}
	stride := (len(s) + max - 1) / max
	out := make([]T, 0, max)
	for i := 0; i < len(s); i += stride {
		out = append(out, s[i])
	}
	return out
}

// sampleDispatch is sampleTyped without a statically known element type, since
// a canonical column arrives as an any holding a []T.
func sampleDispatch(typed any, max int) any {
	switch s := typed.(type) {
	case []bool:
		return sampleTyped(s, max)
	case []int8:
		return sampleTyped(s, max)
	case []int16:
		return sampleTyped(s, max)
	case []int32:
		return sampleTyped(s, max)
	case []int64:
		return sampleTyped(s, max)
	case []uint8:
		return sampleTyped(s, max)
	case []uint16:
		return sampleTyped(s, max)
	case []uint32:
		return sampleTyped(s, max)
	case []uint64:
		return sampleTyped(s, max)
	case []float32:
		return sampleTyped(s, max)
	case []float64:
		return sampleTyped(s, max)
	case []string:
		return sampleTyped(s, max)
	case [][]byte:
		return sampleTyped(s, max)
	default:
		return typed
	}
}

// ExperimentLayouts returns the BenchmarkLayouts entry with the smallest
// CompressedSize, or Plain+None when no candidate works. It measures a sample
// of the column rather than every value, so the cost does not grow with the
// row count.
func ExperimentLayouts(col []any, schema ColumnSchema) LayoutResult {
	typed, err := canonicalColumnTyped(col, schema.Type)
	if err != nil {
		return LayoutResult{Name: encName(EncPlain) + "+" + codecName(CompressNone), Encoding: EncPlain, Compress: CompressNone}
	}
	return experimentLayoutsTyped(typed, schema)
}

// experimentLayoutsTyped is ExperimentLayouts for an already canonical column,
// so a writer that has already coerced the values measures them once more
// rather than twice.
func experimentLayoutsTyped(typed any, schema ColumnSchema) LayoutResult {
	results := benchmarkLayoutsTyped(experimentSampleTyped(typed), schema)
	if len(results) == 0 {
		return LayoutResult{Name: encName(EncPlain) + "+" + codecName(CompressNone), Encoding: EncPlain, Compress: CompressNone}
	}
	return results[0]
}

func benchmarkLayoutsTyped(col any, schema ColumnSchema) []LayoutResult {
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
		return results[i].EncodedSize < results[j].EncodedSize
	})
	return results
}

func measureLayout(col any, schema ColumnSchema, enc, codec uint8) (LayoutResult, bool) {
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
	if _, err := decodeWith(enc, raw, sliceLen(col), schema.Type); err != nil {
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
