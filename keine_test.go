package keine

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"
)

const rowsPerGroup = 5

var allTypes = []uint8{
	TypeBool, TypeInt8, TypeInt16, TypeInt32, TypeInt64,
	TypeUint8, TypeUint16, TypeUint32, TypeUint64,
	TypeFloat32, TypeFloat64, TypeBytes, TypeString,
}

// columnNullable alternates so the round trip covers nullable and non-nullable
// columns of every kind.
var columnNullable = []bool{
	true, false, true, false, true, false, true, false, true, false, true, false, true,
}

func genValue(typ uint8, g, r int) any {
	base := int64(g*100 + r)
	switch typ {
	case TypeBool:
		return base%2 == 0
	case TypeInt8:
		return int8(base)
	case TypeInt16:
		return int16(base)
	case TypeInt32:
		return int32(base)
	case TypeInt64:
		return base
	case TypeUint8:
		return uint8(base)
	case TypeUint16:
		return uint16(base)
	case TypeUint32:
		return uint32(base + 1)
	case TypeUint64:
		return uint64(base + 1)
	case TypeFloat32:
		return float32(base) + 0.5
	case TypeFloat64:
		return float64(base) + 0.25
	case TypeBytes:
		return []byte{byte(base), byte(base + 1), byte(base + 2)}
	case TypeString:
		return fmt.Sprintf("v-%d-%d", g, r)
	default:
		return nil
	}
}

func buildSchema() []ColumnSchema {
	schema := make([]ColumnSchema, len(allTypes))
	for i, typ := range allTypes {
		schema[i] = ColumnSchema{
			Name:     fmt.Sprintf("col%d_%d", i, typ),
			Type:     typ,
			Nullable: columnNullable[i],
		}
	}
	return schema
}

func buildColumns(g int) [][]any {
	cols := make([][]any, len(allTypes))
	for c, typ := range allTypes {
		col := make([]any, rowsPerGroup)
		for r := 0; r < rowsPerGroup; r++ {
			if columnNullable[c] && (g+r)%3 == 0 {
				col[r] = nil
			} else {
				col[r] = genValue(typ, g, r)
			}
		}
		cols[c] = col
	}
	return cols
}

func TestRoundTrip(t *testing.T) {
	schema := buildSchema()

	var buf bytes.Buffer
	w := NewWriter(&buf, schema)
	for g := 0; g < 3; g++ {
		if err := w.AddRowGroup(buildColumns(g)); err != nil {
			t.Fatalf("AddRowGroup(%d): %v", g, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !bytes.HasPrefix(buf.Bytes(), []byte(magic)) {
		t.Errorf("file does not start with magic %q", magic)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte(magic)) {
		t.Errorf("file does not end with magic %q", magic)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if got, want := r.RowGroupCount(), 3; got != want {
		t.Errorf("RowGroupCount = %d, want %d", got, want)
	}
	if got := r.Schema(); len(got) != len(schema) {
		t.Errorf("Schema() has %d columns, want %d", len(got), len(schema))
	} else {
		for i := range schema {
			if got[i] != schema[i] {
				t.Errorf("Schema()[%d] = %+v, want %+v", i, got[i], schema[i])
			}
		}
	}

	all := make([]int, len(allTypes))
	for i := range all {
		all[i] = i
	}

	for g := 0; g < 3; g++ {
		want := buildColumns(g)
		got, err := r.ReadRowGroup(g, all)
		if err != nil {
			t.Fatalf("ReadRowGroup(%d, all): %v", g, err)
		}
		for c := range want {
			if !reflect.DeepEqual(got[c], want[c]) {
				t.Errorf("row group %d column %d (%s): got %v, want %v",
					g, c, schema[c].Name, got[c], want[c])
			}
		}
	}

	// Partial read of a subset of columns from the middle row group.
	subset := []int{0, 5, 12}
	got, err := r.ReadRowGroup(1, subset)
	if err != nil {
		t.Fatalf("ReadRowGroup(1, %v): %v", subset, err)
	}
	want := buildColumns(1)
	for k, c := range subset {
		if !reflect.DeepEqual(got[k], want[c]) {
			t.Errorf("partial read column %d (%s): got %v, want %v",
				c, schema[c].Name, got[k], want[c])
		}
	}
}

func TestLayoutExperiment(t *testing.T) {
	// A boolean column separates the encodings sharply: RLEBitpack costs one
	// bit per value, Plain one byte per value.
	const n = 10000
	boolCol := make([]any, n)
	for i := range boolCol {
		boolCol[i] = i%3 == 0
	}

	results := BenchmarkLayouts(boolCol, ColumnSchema{Name: "b", Type: TypeBool})
	if len(results) == 0 {
		t.Fatal("BenchmarkLayouts returned no candidates")
	}
	for i := 1; i < len(results); i++ {
		if results[i-1].CompressedSize > results[i].CompressedSize {
			t.Errorf("candidates not sorted by CompressedSize: %v", results)
		}
	}

	var rle, plain *LayoutResult
	for i := range results {
		r := &results[i]
		switch {
		case r.Encoding == EncRLEBitpack && r.Compress == CompressNone:
			rle = r
		case r.Encoding == EncPlain && r.Compress == CompressNone:
			plain = r
		}
	}
	if rle == nil {
		t.Fatal("BenchmarkLayouts omitted the RLEBitpack+None candidate")
	}
	if plain == nil {
		t.Fatal("BenchmarkLayouts omitted the Plain+None candidate")
	}
	if rle.CompressedSize >= plain.CompressedSize {
		t.Errorf("RLEBitpack+None is %d bytes, did not beat Plain+None at %d bytes",
			rle.CompressedSize, plain.CompressedSize)
	}
	if plain.CompressedSize/rle.CompressedSize < 4 {
		t.Errorf("RLEBitpack+None is only %.1fx smaller than Plain+None, expected closer to 8x",
			float64(plain.CompressedSize)/float64(rle.CompressedSize))
	}

	best := ExperimentLayouts(boolCol, ColumnSchema{Name: "b", Type: TypeBool})
	if best.Encoding != EncRLEBitpack {
		t.Errorf("ExperimentLayouts picked %s, want RLEBitpack", best.Name)
	}

	// High cardinality strings: every value is distinct, so dictionary
	// encoding only adds index overhead and cannot be the best layout.
	strCol := make([]any, n)
	for i := range strCol {
		strCol[i] = fmt.Sprintf("id-%d-keine-common-suffix", i)
	}
	strResults := BenchmarkLayouts(strCol, ColumnSchema{Name: "s", Type: TypeString})
	if len(strResults) == 0 {
		t.Fatal("BenchmarkLayouts returned no candidates for strings")
	}
	for i := 1; i < len(strResults); i++ {
		if strResults[i-1].CompressedSize > strResults[i].CompressedSize {
			t.Errorf("string candidates not sorted by CompressedSize: %v", strResults)
		}
	}
	if strResults[0].Encoding == EncDict {
		t.Errorf("Dict+None won on an all-distinct column: %v", strResults[0])
	}

	// Compression is now in play: a repetitive string column must beat its
	// uncompressed Plain layout by a wide margin, and the winner has to use a
	// real codec rather than CompressNone.
	var plainNone int
	for _, r := range strResults {
		if r.Encoding == EncPlain && r.Compress == CompressNone {
			plainNone = r.CompressedSize
		}
	}
	if plainNone == 0 {
		t.Fatal("BenchmarkLayouts omitted the Plain+None candidate for strings")
	}
	if strResults[0].Compress == CompressNone {
		t.Errorf("best string layout is %s, expected a compressed one", strResults[0].Name)
	}
	if strResults[0].CompressedSize >= plainNone/4 {
		t.Errorf("best layout is %d bytes, did not beat Plain+None at %d by much",
			strResults[0].CompressedSize, plainNone)
	}
}

func TestExperimentSample(t *testing.T) {
	small := make([]any, maxExperimentRows)
	for i := range small {
		small[i] = i
	}
	if got := experimentSample(small); !reflect.DeepEqual(got, small) {
		t.Errorf("experimentSample of a column at the cap returned a different slice")
	}

	big := make([]any, maxExperimentRows*4)
	for i := range big {
		big[i] = i
	}
	got := experimentSample(big)
	if len(got) > maxExperimentRows {
		t.Fatalf("experimentSample returned %d values, cap is %d", len(got), maxExperimentRows)
	}
	if len(got) <= 1 {
		t.Fatal("experimentSample returned too few values to compare shapes")
	}
	if got[0] != big[0] {
		t.Errorf("sample starts at %v, want the first value %v", got[0], big[0])
	}
	// Even spacing lands on multiples of the stride, so the last sampled value
	// is within one stride of the end rather than exactly the last value.
	stride := (len(big) + maxExperimentRows - 1) / maxExperimentRows
	last := got[len(got)-1].(int)
	if last < len(big)-stride {
		t.Errorf("sample ends at index %d, want within stride %d of %d", last, stride, len(big))
	}
	for i := 1; i < len(got); i++ {
		if got[i].(int) <= got[i-1].(int) {
			t.Errorf("sample is not in increasing order at %d", i)
		}
	}
}

// ExperimentLayouts measures a sample of a long column. The layout it picks has
// to match what measuring the whole column would have picked, otherwise writing
// a bigger file would change its encoding.
func TestExperimentLayoutSampleMatches(t *testing.T) {
	const n = maxExperimentRows * 4
	full := make([]any, n)
	for i := range full {
		full[i] = fmt.Sprintf("bucket-%d", i%64)
	}
	schema := ColumnSchema{Name: "b", Type: TypeString}

	sampled := ExperimentLayouts(full, schema)
	measured := BenchmarkLayouts(full, schema)[0]
	if sampled.Encoding != measured.Encoding || sampled.Compress != measured.Compress {
		t.Errorf("ExperimentLayouts picked %s on the sample but %s on the full column",
			sampled.Name, measured.Name)
	}
	if sampled.CompressedSize == 0 {
		t.Error("ExperimentLayouts returned a zero size")
	}
}

func TestRowGroupMeta(t *testing.T) {
	schema := []ColumnSchema{
		{Name: "id", Type: TypeInt64, Nullable: true},
		{Name: "note", Type: TypeString, Nullable: true},
	}
	cols := [][]any{
		{int64(1), int64(2), nil, int64(4)},
		{"a", "bb", "ccc", nil},
	}

	var buf bytes.Buffer
	w := NewWriter(&buf, schema)
	if err := w.AddRowGroup(cols); err != nil {
		t.Fatalf("AddRowGroup: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if got := r.RowGroupCount(); got != 1 {
		t.Fatalf("RowGroupCount = %d, want 1", got)
	}
	rg, err := r.RowGroupMeta(0)
	if err != nil {
		t.Fatalf("RowGroupMeta(0): %v", err)
	}
	if rg.NumRows != 4 {
		t.Errorf("NumRows = %d, want 4", rg.NumRows)
	}
	// The file starts with the magic header, so the row group follows it.
	if rg.ByteOffset != 4 {
		t.Errorf("ByteOffset = %d, want 4", rg.ByteOffset)
	}
	if len(rg.Columns) != 2 {
		t.Fatalf("got %d columns, want 2", len(rg.Columns))
	}
	if rg.Columns[0].NullCount != 1 || rg.Columns[1].NullCount != 1 {
		t.Errorf("null counts = %d, %d, want 1, 1", rg.Columns[0].NullCount, rg.Columns[1].NullCount)
	}
	if rg.Columns[0].ByteLength <= 0 {
		t.Error("ByteLength of the first column was not recorded")
	}
	if got := string(rg.Columns[1].MinVal); got != "a" {
		t.Errorf("MinVal of note = %q, want %q", got, "a")
	}
	if got := string(rg.Columns[1].MaxVal); got != "ccc" {
		t.Errorf("MaxVal of note = %q, want %q", got, "ccc")
	}
	if rg.Columns[1].MinLen != 1 || rg.Columns[1].MaxLen != 3 {
		t.Errorf("MinLen/MaxLen of note = %d, %d, want 1, 3", rg.Columns[1].MinLen, rg.Columns[1].MaxLen)
	}

	// Metadata is readable without decoding any chunk data.
	if _, err := r.RowGroupMeta(1); err == nil {
		t.Error("RowGroupMeta(1) succeeded, want out of range")
	}
	if _, err := r.RowGroupMeta(-1); err == nil {
		t.Error("RowGroupMeta(-1) succeeded, want out of range")
	}
}

func TestBitmap(t *testing.T) {
	cases := [][]bool{
		{},
		{true},
		{false},
		{true, true, true, true, true, true, true, true},
		{false, false, false, false, false, false, false, false},
		{true, false, true, false, true, false, true, false},
		{true, false, false, true, false, false, false},
		{true, false, false, false, false, false, false, false, true},
		{false, false, false, false, false, false, false, false, false, false},
	}
	for _, in := range cases {
		b := EncodeBitmap(in)
		if got := DecodeBitmap(b, len(in)); !reflect.DeepEqual(got, in) {
			t.Errorf("DecodeBitmap(EncodeBitmap(%v)) = %v", in, got)
		}
		if len(b) != (len(in)+7)/8 {
			t.Errorf("EncodeBitmap(%v) is %d bytes, want %d", in, len(b), (len(in)+7)/8)
		}
	}
}
