package keine

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
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

// typeNames is used in failure messages, where a bare type tag is unhelpful.
var typeNames = map[uint8]string{
	TypeBool:    "bool",
	TypeInt8:    "int8",
	TypeInt16:   "int16",
	TypeInt32:   "int32",
	TypeInt64:   "int64",
	TypeUint8:   "uint8",
	TypeUint16:  "uint16",
	TypeUint32:  "uint32",
	TypeUint64:  "uint64",
	TypeFloat32: "float32",
	TypeFloat64: "float64",
	TypeBytes:   "bytes",
	TypeString:  "string",
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

// benchmarkLayoutsAt is BenchmarkLayouts at a given level, for a test that has to
// measure a column the way Optimize is about to measure it. BenchmarkLayouts
// itself stays at the default level, which is what the writer does when it has not
// been asked to optimise.
func benchmarkLayoutsAt(t *testing.T, col []any, schema ColumnSchema, level int) []LayoutResult {
	t.Helper()
	typed, err := canonicalColumnTyped(col, schema.Type)
	if err != nil {
		t.Fatalf("canonicalColumnTyped(%s): %v", schema.Name, err)
	}
	return benchmarkLayoutsTyped(typed, schema, nil, &measureScratch{}, level)
}

func TestBenchmarkLayouts(t *testing.T) {
	// A boolean column separates the encodings sharply: RLEBitpack costs one
	// bit per value, Plain one byte per value.
	const n = 10000
	boolCol := make([]any, n)
	for i := range boolCol {
		boolCol[i] = i%3 == 0
	}

	results := BenchmarkLayouts(boolCol, ColumnSchema{Name: "b", Type: TypeBool}, nil)
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

	// High cardinality strings: every value is distinct, so dictionary
	// encoding only adds index overhead and cannot be the best layout.
	strCol := make([]any, n)
	for i := range strCol {
		strCol[i] = fmt.Sprintf("id-%d-keine-common-suffix", i)
	}
	strResults := BenchmarkLayouts(strCol, ColumnSchema{Name: "s", Type: TypeString}, nil)
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

	// Compression is now in play. A repetitive string column must beat its
	// uncompressed Plain layout by a wide margin, and the winner has to use a real
	// codec rather than CompressNone.
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

// The layout a writer falls back on has to be a property of the type alone, since
// it is chosen before any value is read. Whatever a column holds, this is what it
// gets: bool packs to a bit, deltas cost a subtraction, everything else is stored
// plainly.
func TestDefaultLayout(t *testing.T) {
	tests := []struct {
		name       string
		typ        uint8
		enc, codec uint8
	}{
		{"bool", TypeBool, EncRLEBitpack, CompressFlate},
		{"int8", TypeInt8, EncDelta, CompressFlate},
		{"int16", TypeInt16, EncDelta, CompressFlate},
		{"int32", TypeInt32, EncDelta, CompressFlate},
		{"int64", TypeInt64, EncDelta, CompressFlate},
		{"uint8", TypeUint8, EncDelta, CompressFlate},
		{"uint16", TypeUint16, EncDelta, CompressFlate},
		{"uint32", TypeUint32, EncDelta, CompressFlate},
		{"uint64", TypeUint64, EncDelta, CompressFlate},
		{"float32", TypeFloat32, EncPlain, CompressFlate},
		{"float64", TypeFloat64, EncPlain, CompressFlate},
		{"bytes", TypeBytes, EncPlain, CompressFlate},
		{"string", TypeString, EncPlain, CompressFlate},
		// A type no schema carries gets Plain, the encoding everything accepts.
		{"unknown", 0xFF, EncPlain, CompressFlate},
	}
	for _, tc := range tests {
		enc, codec := defaultLayout(tc.typ)
		if enc != tc.enc || codec != tc.codec {
			t.Errorf("defaultLayout(%s) = %s+%s, want %s+%s",
				tc.name, encName(enc), codecName(codec), encName(tc.enc), codecName(tc.codec))
		}
	}
}

// A writer that has not been Optimized stores each column the way its type
// implies, and the metadata it writes back says so. The default has to survive a
// round trip, since it is what a caller gets for doing nothing.
func TestWriterUsesDefaultLayouts(t *testing.T) {
	schema := []ColumnSchema{
		{Name: "b", Type: TypeBool},
		{Name: "i", Type: TypeInt64},
		{Name: "s", Type: TypeString},
	}
	cols := [][]any{
		{true, false, true, true, false},
		{int64(1), int64(3), int64(6), int64(10), int64(15)},
		{"alpha", "beta", "gamma", "delta", "epsilon"},
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
	rg, err := r.RowGroupMeta(0)
	if err != nil {
		t.Fatalf("RowGroupMeta: %v", err)
	}
	want := []struct {
		name       string
		enc, codec uint8
	}{
		{"b", EncRLEBitpack, CompressNone},
		{"i", EncDelta, CompressNone},
		{"s", EncPlain, CompressNone},
	}
	for i, wnt := range want {
		got := rg.Columns[i]
		if got.Encoding != wnt.enc || got.Compress != wnt.codec {
			t.Errorf("column %s encoded %s+%s, want %s+%s", wnt.name,
				encName(got.Encoding), codecName(got.Compress),
				encName(wnt.enc), codecName(wnt.codec))
		}
	}
	back, err := r.ReadRowGroup(0, []int{0, 1, 2})
	if err != nil {
		t.Fatalf("ReadRowGroup: %v", err)
	}
	for i, col := range cols {
		if len(back[i]) != len(col) {
			t.Errorf("column %d read back %d values, wrote %d", i, len(back[i]), len(col))
		}
	}
}

// Options overrides the layout the type implies and the codec the writer defaults
// to, and the columns still have to round trip through what was asked for. The
// codec's zero value is CompressNone, which is a valid codec, so an empty Options
// asks for the type's default encoding stored uncompressed.
//
// A codec that cannot shrink a column is dropped rather than stored at a loss, so
// the codec a chunk reports is what the data earned, not always the one asked for.
// Four values are too little for flate to beat at its default level, and enough
// once the deltas run to one repeated byte, so the two flate cases below disagree
// about the outcome.
func TestWriterOptions(t *testing.T) {
	schema := []ColumnSchema{
		{Name: "i", Type: TypeInt64},
		{Name: "s", Type: TypeString},
	}
	cols := [][]any{
		{int64(1), int64(2), int64(3), int64(4)},
		{"a", "b", "c", "d"},
	}

	opts := []struct {
		name string
		opts Options
		want [][2]uint8
	}{
		{name: "empty options", opts: Options{}, want: [][2]uint8{{EncDelta, CompressNone}, {EncPlain, CompressNone}}},
		{name: "plain encoding", opts: Options{Encoding: EncPlain}, want: [][2]uint8{{EncPlain, CompressNone}, {EncPlain, CompressNone}}},
		{name: "no compression", opts: Options{Compress: CompressNone}, want: [][2]uint8{{EncDelta, CompressNone}, {EncPlain, CompressNone}}},
		{name: "lzw", opts: Options{Compress: CompressLzw}, want: [][2]uint8{{EncDelta, CompressLzw}, {EncPlain, CompressLzw}}},
		{name: "plain on flate", opts: Options{Encoding: EncPlain, Compress: CompressFlate}, want: [][2]uint8{{EncPlain, CompressNone}, {EncPlain, CompressNone}}},
		{name: "level and block size", opts: Options{Compress: CompressFlate, CompressLevel: 9, BlockSize: 64}, want: [][2]uint8{{EncDelta, CompressFlate}, {EncPlain, CompressNone}}},
	}
	for _, o := range opts {
		t.Run(o.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriterWithOptions(&buf, schema, o.opts)
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
			rg, err := r.RowGroupMeta(0)
			if err != nil {
				t.Fatalf("RowGroupMeta: %v", err)
			}
			for i, wnt := range o.want {
				if got := rg.Columns[i]; got.Encoding != wnt[0] || got.Compress != wnt[1] {
					t.Errorf("column %d stored %s+%s, want %s+%s", i,
						encName(got.Encoding), codecName(got.Compress),
						encName(wnt[0]), codecName(wnt[1]))
				}
			}
			back, err := r.ReadRowGroup(0, []int{0, 1})
			if err != nil {
				t.Fatalf("ReadRowGroup: %v", err)
			}
			for i, col := range cols {
				for j, v := range col {
					if back[i][j] != v {
						t.Errorf("column %d row %d read back %v, wrote %v", i, j, back[i][j], v)
					}
				}
			}
		})
	}
}

// An option this build cannot serve has to be refused before anything is written,
// rather than producing a file that is silently missing its data.
func TestWriterOptionsRejected(t *testing.T) {
	schema := []ColumnSchema{{Name: "i", Type: TypeInt64}}
	cols := [][]any{{int64(1), int64(2)}}

	for _, o := range []struct {
		name string
		opts Options
	}{
		{"zstd codec", Options{Compress: CompressZstd}},
		{"unknown codec", Options{Compress: 99}},
		{"unknown encoding", Options{Encoding: 99}},
	} {
		t.Run(o.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriterWithOptions(&buf, schema, o.opts)
			if buf.Len() != 0 {
				t.Errorf("wrote %d bytes for options the writer refuses", buf.Len())
			}
			if err := w.AddRowGroup(cols); err == nil {
				t.Error("AddRowGroup with unusable options: want error, got nil")
			}
			if err := w.Close(); err == nil {
				t.Error("Close with unusable options: want error, got nil")
			}
		})
	}
}

// An encoding that cannot serve a column's type is the caller's choice, so the
// write reports it instead of substituting something else.
func TestWriterEncodingWrongForType(t *testing.T) {
	schema := []ColumnSchema{{Name: "s", Type: TypeString}}
	cols := [][]any{{"a", "b", "c"}}

	var buf bytes.Buffer
	w := NewWriterWithOptions(&buf, schema, Options{Encoding: EncRLEBitpack, Compress: CompressFlate})
	if err := w.AddRowGroup(cols); err == nil {
		t.Error("AddRowGroup of a bool encoding on strings: want error, got nil")
	}
}

// Optimize reads the whole column rather than trusting the type, so a column the
// default serves badly has to come out smaller, and the file still has to read
// back. Two hundred values repeated a hundred times each is the input the
// value-aware encodings are for, and what Plain pays for.
func TestOptimizeBeatsDefault(t *testing.T) {
	col := make([]any, 20000)
	for i := range col {
		col[i] = fmt.Sprintf("customer-%d@example.com", i%200)
	}
	schema := []ColumnSchema{{Name: "s", Type: TypeString}}

	// The default for a string column is Plain, which is what Optimize has to beat
	// to be worth the pass. Measured at the level the pass measures at, since a
	// candidate compressed at another level is not the one it chose between.
	var plain LayoutResult
	for _, r := range benchmarkLayoutsAt(t, col, schema[0], flate.BestCompression) {
		if r.Encoding == EncPlain && r.Compress == CompressFlate {
			plain = r
		}
	}
	if plain.CompressedSize == 0 {
		t.Fatal("the column measured to no Plain+Flate candidate")
	}

	var buf bytes.Buffer
	w := NewWriter(&buf, schema)
	if err := w.AddRowGroup([][]any{col}); err != nil {
		t.Fatalf("AddRowGroup: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	defaultLen := buf.Len()

	buf.Reset()
	w = NewWriter(&buf, schema)
	if err := w.Optimize([][]any{col}); err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	picked := w.layouts[0]
	if picked.Encoding == EncPlain {
		t.Errorf("Optimize kept Plain on 200 repeated values, want an encoding that reads them")
	}
	if picked.CompressedSize >= plain.CompressedSize {
		t.Errorf("Optimize picked %s at %d bytes, Plain+Flate is %d",
			picked.Name, picked.CompressedSize, plain.CompressedSize)
	}
	if err := w.AddRowGroup([][]any{col}); err != nil {
		t.Fatalf("AddRowGroup: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := buf.Len(); got >= defaultLen {
		t.Errorf("optimized file is %d bytes, the default layout wrote %d", got, defaultLen)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	back, err := r.ReadRowGroup(0, []int{0})
	if err != nil {
		t.Fatalf("ReadRowGroup: %v", err)
	}
	for i, v := range col {
		if back[0][i] != v {
			t.Fatalf("value %d read back %v, wrote %v", i, back[0][i], v)
		}
	}
}

// Optimize measures at OptimizeLevel and the row groups after it are written at
// that level, so a writer whose CompressLevel is lower writes the optimised
// columns at the pass's level rather than its own. A column written at a level
// other than the one it was measured at is not the column the measurement picked.
func TestOptimizeWritesAtItsOwnLevel(t *testing.T) {
	// Sentences that agree on shape and disagree on the numbers, so a good match
	// is long and a near miss is close. That is what DEFLATE searches harder for at
	// the higher levels, and what makes the file respond to the level at all.
	rng := rand.New(rand.NewSource(11))
	col := make([]any, 3000)
	for i := range col {
		col[i] = fmt.Sprintf("row %d of the shard: the column store wrote %d blocks at level %d and read them back at level %d, which the benchmark then reported as %d bytes per row",
			i, rng.Intn(64), rng.Intn(9)+1, rng.Intn(9)+1, rng.Intn(9000))
	}
	schema := []ColumnSchema{{Name: "s", Type: TypeString}}

	write := func(optimize bool, compressLevel, optimizeLevel int) []byte {
		var buf bytes.Buffer
		w := NewWriterWithOptions(&buf, schema, Options{
			Compress:      CompressFlate,
			CompressLevel: compressLevel,
			OptimizeLevel: optimizeLevel,
		})
		if optimize {
			if err := w.Optimize([][]any{col}); err != nil {
				t.Fatalf("Optimize: %v", err)
			}
		}
		if err := w.AddRowGroup([][]any{col}); err != nil {
			t.Fatalf("AddRowGroup: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return buf.Bytes()
	}

	// The column has to cost the higher level something, or the rest of the test
	// compares two identical files and says nothing.
	low := len(write(false, 1, 0))
	high := len(write(false, 9, 0))
	if low <= high {
		t.Fatalf("level 1 wrote %d bytes and level 9 wrote %d; this column does not respond to the level", low, high)
	}

	// CompressLevel is 1 and OptimizeLevel is 9, so the pass is the only thing
	// that can put the file at level 9.
	if got := len(write(true, 1, 9)); got > high {
		t.Errorf("Optimize on a level 1 writer wrote %d bytes, more than the %d a plain level 9 write produced; the row group was not written at the level it was measured at", got, high)
	}
}

// Optimize decides once and applies to every row group after it, since the
// columns of a dataset share a shape across the groups it is split into.
func TestOptimizeAppliesToLaterRowGroups(t *testing.T) {
	schema := []ColumnSchema{{Name: "s", Type: TypeString}}

	// Thirty-two buckets over three thousand values: Plain is not the answer,
	// whichever encoding wins.
	col := make([]any, 3000)
	for i := range col {
		col[i] = fmt.Sprintf("bucket-%d", i%32)
	}
	results := benchmarkLayoutsAt(t, col, schema[0], flate.BestCompression)
	if len(results) == 0 {
		t.Fatal("the column measured to no candidates")
	}
	if results[0].Encoding == EncPlain {
		t.Fatalf("the test's column does not invite Plain, it chose %s", results[0].Name)
	}
	want := results[0].Encoding

	var buf bytes.Buffer
	w := NewWriter(&buf, schema)
	for g := 0; g < 3; g++ {
		group := make([]any, 3000)
		for i := range group {
			group[i] = fmt.Sprintf("bucket-%d", (i+g*17)%32)
		}
		if g == 0 {
			if err := w.Optimize([][]any{col}); err != nil {
				t.Fatalf("Optimize: %v", err)
			}
			if got := w.layouts[0].Encoding; got != want {
				t.Errorf("Optimize picked %s on the first group, the column measures %s", encName(got), encName(want))
			}
		}
		if err := w.AddRowGroup([][]any{group}); err != nil {
			t.Fatalf("AddRowGroup(%d): %v", g, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if got := r.RowGroupCount(); got != 3 {
		t.Fatalf("RowGroupCount = %d, want 3", got)
	}
	for g := 0; g < 3; g++ {
		rg, err := r.RowGroupMeta(g)
		if err != nil {
			t.Fatalf("RowGroupMeta(%d): %v", g, err)
		}
		if got := rg.Columns[0]; got.Encoding != want {
			t.Errorf("row group %d encoded %s, Optimize chose %s", g, encName(got.Encoding), encName(want))
		}
	}
}

// Optimize validates its input the way AddRowGroup does, since it reads the same
// columns.
func TestOptimizeErrors(t *testing.T) {
	schema := []ColumnSchema{
		{Name: "i", Type: TypeInt64},
		{Name: "s", Type: TypeString},
	}
	good := [][]any{{int64(1)}, {"a"}}

	t.Run("wrong column count", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewWriter(&buf, schema)
		if err := w.Optimize([][]any{{int64(1)}}); err == nil {
			t.Error("Optimize with one column of a two column schema: want error, got nil")
		}
	})
	t.Run("ragged rows", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewWriter(&buf, schema)
		if err := w.Optimize([][]any{{int64(1), int64(2)}, {"a"}}); err == nil {
			t.Error("Optimize with columns of different lengths: want error, got nil")
		}
	})
	t.Run("uncoercible values", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewWriter(&buf, schema)
		if err := w.Optimize([][]any{{struct{}{}}, {"a"}}); err == nil {
			t.Error("Optimize of a value no type accepts: want error, got nil")
		}
	})
	t.Run("after a failing writer", func(t *testing.T) {
		w := NewWriterWithOptions(&bytes.Buffer{}, schema, Options{Compress: CompressZstd})
		if err := w.Optimize(good); err == nil {
			t.Error("Optimize on a writer that cannot write: want error, got nil")
		}
	})
	t.Run("writes no data on error", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewWriter(&buf, schema)
		_ = w.Optimize([][]any{{struct{}{}}, {"a"}})
		// The header is written at construction, so the file starts at the magic and
		// the version. Nothing follows it: no chunk, no footer.
		if got := buf.Len(); got != len(magic)+1 {
			t.Errorf("wrote %d bytes while reporting an error, want the %d byte header only",
				got, len(magic)+1)
		}
	})
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
	// The file starts with the magic and the format version, so the row group
	// follows them.
	if rg.ByteOffset != int64(len(magic)+1) {
		t.Errorf("ByteOffset = %d, want %d", rg.ByteOffset, len(magic)+1)
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

// TestReadColumn reads every type back as its Go type rather than []any. The
// caller picks T, so a column and its reader have to agree.
func TestReadColumn(t *testing.T) {
	schema := make([]ColumnSchema, len(allTypes))
	for i, typ := range allTypes {
		schema[i] = ColumnSchema{Name: fmt.Sprintf("t%d_%d", i, typ), Type: typ}
	}
	cols := make([][]any, len(allTypes))
	for c, typ := range allTypes {
		col := make([]any, rowsPerGroup)
		for r := 0; r < rowsPerGroup; r++ {
			col[r] = genValue(typ, 0, r)
		}
		cols[c] = col
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

	cases := []struct {
		typ  uint8
		read func(*Reader, int) (any, error)
	}{
		{TypeBool, func(r *Reader, c int) (any, error) { return ReadColumn[bool](r, 0, c) }},
		{TypeInt8, func(r *Reader, c int) (any, error) { return ReadColumn[int8](r, 0, c) }},
		{TypeInt16, func(r *Reader, c int) (any, error) { return ReadColumn[int16](r, 0, c) }},
		{TypeInt32, func(r *Reader, c int) (any, error) { return ReadColumn[int32](r, 0, c) }},
		{TypeInt64, func(r *Reader, c int) (any, error) { return ReadColumn[int64](r, 0, c) }},
		{TypeUint8, func(r *Reader, c int) (any, error) { return ReadColumn[uint8](r, 0, c) }},
		{TypeUint16, func(r *Reader, c int) (any, error) { return ReadColumn[uint16](r, 0, c) }},
		{TypeUint32, func(r *Reader, c int) (any, error) { return ReadColumn[uint32](r, 0, c) }},
		{TypeUint64, func(r *Reader, c int) (any, error) { return ReadColumn[uint64](r, 0, c) }},
		{TypeFloat32, func(r *Reader, c int) (any, error) { return ReadColumn[float32](r, 0, c) }},
		{TypeFloat64, func(r *Reader, c int) (any, error) { return ReadColumn[float64](r, 0, c) }},
		{TypeBytes, func(r *Reader, c int) (any, error) { return ReadColumn[[]byte](r, 0, c) }},
		{TypeString, func(r *Reader, c int) (any, error) { return ReadColumn[string](r, 0, c) }},
	}

	for _, c := range cases {
		i := -1
		for j, typ := range allTypes {
			if typ == c.typ {
				i = j
			}
		}
		got, err := c.read(r, i)
		if err != nil {
			t.Errorf("ReadColumn[%s] failed: %v", typeNames[c.typ], err)
			continue
		}
		slice := reflect.ValueOf(got)
		if slice.Len() != rowsPerGroup {
			t.Errorf("ReadColumn[%s] returned %d values, want %d",
				typeNames[c.typ], slice.Len(), rowsPerGroup)
			continue
		}
		for k := 0; k < rowsPerGroup; k++ {
			want := genValue(c.typ, 0, k)
			if !reflect.DeepEqual(slice.Index(k).Interface(), want) {
				t.Errorf("ReadColumn[%s][%d] = %v, want %v",
					typeNames[c.typ], k, slice.Index(k).Interface(), want)
			}
		}
	}

	// Asking for a type the column does not hold is an error, not a wrong slice.
	if _, err := ReadColumn[string](r, 0, 4); err == nil {
		t.Error("ReadColumn[string] on an int64 column succeeded, want a type mismatch error")
	}

	if _, err := ReadColumn[int64](r, 1, 0); err == nil {
		t.Error("ReadColumn on row group 1 succeeded, want out of range")
	}
	if _, err := ReadColumn[int64](r, -1, 0); err == nil {
		t.Error("ReadColumn on row group -1 succeeded, want out of range")
	}
	if _, err := ReadColumn[int64](r, 0, len(allTypes)); err == nil {
		t.Error("ReadColumn past the last column succeeded, want out of range")
	}
	if _, err := ReadColumn[int64](r, 0, -1); err == nil {
		t.Error("ReadColumn on column -1 succeeded, want out of range")
	}
}

// ReadColumn reads through the same chunk reading and decoding paths as
// ReadRowGroup, so a file whose chunk cannot be decoded has to fail there too.
func TestReadColumnCorrupt(t *testing.T) {
	schema := []ColumnSchema{{Name: "i", Type: TypeInt16}}

	zstdChunk := craftChunk(EncPlain, CompressZstd, nil, []byte{1, 0})
	zstdFile := craftFile(zstdChunk, ColMeta{
		ByteLength: int64(len(zstdChunk)),
		Encoding:   EncPlain,
		Compress:   CompressZstd,
	}, schema, 1)
	r, err := NewReader(bytes.NewReader(zstdFile))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := ReadColumn[int16](r, 0, 0); err == nil {
		t.Error("ReadColumn of a chunk claiming zstd succeeded, want an error")
	}

	// Two bytes for one value and a third left over is not a valid int16 column.
	oddChunk := craftChunk(EncPlain, CompressNone, nil, []byte{1, 2, 3})
	oddFile := craftFile(oddChunk, ColMeta{
		ByteLength: int64(len(oddChunk)),
		Encoding:   EncPlain,
		Compress:   CompressNone,
	}, schema, 1)
	r, err = NewReader(bytes.NewReader(oddFile))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := ReadColumn[int16](r, 0, 0); err == nil {
		t.Error("ReadColumn of undecodable data succeeded, want an error")
	}
}

// ReadColumn seeks before it reads, so a seek that fails has to surface there too.
func TestReadColumnSeekFail(t *testing.T) {
	schema := []ColumnSchema{{Name: "i", Type: TypeInt64}}
	cols := [][]any{{int64(1), int64(2)}}

	var buf bytes.Buffer
	w := NewWriter(&buf, schema)
	if err := w.AddRowGroup(cols); err != nil {
		t.Fatalf("AddRowGroup: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// NewReader performs four seeks, so the fifth is ReadColumn's seek to the chunk.
	r, err := NewReader(&flakyReadSeeker{r: bytes.NewReader(buf.Bytes()), failSeek: 5})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := ReadColumn[int64](r, 0, 0); err == nil {
		t.Error("ReadColumn with a failing seek succeeded, want an error")
	}
}

// A column holding nulls has nowhere to put a nil in a []T, so ReadColumn
// declines it and ReadRowGroup handles that case.
func TestReadColumnNulls(t *testing.T) {
	schema := []ColumnSchema{{Name: "n", Type: TypeInt64, Nullable: true}}
	cols := [][]any{{int64(1), nil, int64(3)}}

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
	if _, err := ReadColumn[int64](r, 0, 0); err == nil {
		t.Error("ReadColumn on a column with nulls succeeded, want an error")
	}

	got, err := r.ReadRowGroup(0, []int{0})
	if err != nil {
		t.Fatalf("ReadRowGroup: %v", err)
	}
	want := []any{int64(1), nil, int64(3)}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("ReadRowGroup = %v, want %v", got[0], want)
	}
}

// A dense column written beside a nullable one still decodes through the same
// path, and a column with no nulls but a nullable schema reads typed too.
func TestReadColumnNullableSchemaDense(t *testing.T) {
	schema := []ColumnSchema{{Name: "n", Type: TypeInt64, Nullable: true}}
	cols := [][]any{{int64(1), int64(2), int64(3)}}

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
	got, err := ReadColumn[int64](r, 0, 0)
	if err != nil {
		t.Fatalf("ReadColumn: %v", err)
	}
	if !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Errorf("ReadColumn = %v, want [1 2 3]", got)
	}
}

// TestReadRowGroupScoped covers the typed path. Every type has to come back
// through it unchanged, a column has to be readable alongside its neighbours, and
// the slices only stay valid inside the call. Asking for a type a column does not
// hold, or a position that was not requested, is an error rather than a wrong
// slice.
func TestReadRowGroupScoped(t *testing.T) {
	schema := make([]ColumnSchema, len(allTypes))
	for i, typ := range allTypes {
		schema[i] = ColumnSchema{Name: fmt.Sprintf("t%d_%d", i, typ), Type: typ}
	}
	cols := make([][]any, len(allTypes))
	for c, typ := range allTypes {
		col := make([]any, rowsPerGroup)
		for r := 0; r < rowsPerGroup; r++ {
			col[r] = genValue(typ, 0, r)
		}
		cols[c] = col
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

	reads := map[uint8]func(*Columns, int) (any, error){
		TypeBool:    func(c *Columns, i int) (any, error) { return Column[bool](c, i) },
		TypeInt8:    func(c *Columns, i int) (any, error) { return Column[int8](c, i) },
		TypeInt16:   func(c *Columns, i int) (any, error) { return Column[int16](c, i) },
		TypeInt32:   func(c *Columns, i int) (any, error) { return Column[int32](c, i) },
		TypeInt64:   func(c *Columns, i int) (any, error) { return Column[int64](c, i) },
		TypeUint8:   func(c *Columns, i int) (any, error) { return Column[uint8](c, i) },
		TypeUint16:  func(c *Columns, i int) (any, error) { return Column[uint16](c, i) },
		TypeUint32:  func(c *Columns, i int) (any, error) { return Column[uint32](c, i) },
		TypeUint64:  func(c *Columns, i int) (any, error) { return Column[uint64](c, i) },
		TypeFloat32: func(c *Columns, i int) (any, error) { return Column[float32](c, i) },
		TypeFloat64: func(c *Columns, i int) (any, error) { return Column[float64](c, i) },
		TypeBytes:   func(c *Columns, i int) (any, error) { return Column[[]byte](c, i) },
		TypeString:  func(c *Columns, i int) (any, error) { return Column[string](c, i) },
	}

	for i, typ := range allTypes {
		err := r.ReadRowGroupScoped(0, []int{i}, func(c *Columns) error {
			got, err := reads[typ](c, 0)
			if err != nil {
				return fmt.Errorf("Column[%s]: %w", typeNames[typ], err)
			}
			slice := reflect.ValueOf(got)
			if slice.Len() != rowsPerGroup {
				return fmt.Errorf("Column[%s] returned %d values, want %d",
					typeNames[typ], slice.Len(), rowsPerGroup)
			}
			for k := 0; k < rowsPerGroup; k++ {
				want := genValue(typ, 0, k)
				if !reflect.DeepEqual(slice.Index(k).Interface(), want) {
					return fmt.Errorf("Column[%s][%d] = %v, want %v",
						typeNames[typ], k, slice.Index(k).Interface(), want)
				}
			}
			return nil
		})
		if err != nil {
			t.Errorf("ReadRowGroupScoped for %s: %v", typeNames[typ], err)
		}
	}

	// Reading columns together has to give each one its own values. The positions
	// are the requested set, not the schema, so asking out of order reads out of
	// order.
	at := map[uint8]int{}
	for i, typ := range allTypes {
		at[typ] = i
	}
	order := []int{at[TypeString], at[TypeInt64], at[TypeFloat64]}
	err = r.ReadRowGroupScoped(0, order, func(c *Columns) error {
		strs, err := Column[string](c, 0)
		if err != nil {
			return err
		}
		ints, err := Column[int64](c, 1)
		if err != nil {
			return err
		}
		flts, err := Column[float64](c, 2)
		if err != nil {
			return err
		}
		for k := 0; k < rowsPerGroup; k++ {
			wantS := genValue(TypeString, 0, k).(string)
			if strs[k] != wantS {
				return fmt.Errorf("strings[%d] = %q, want %q", k, strs[k], wantS)
			}
			if ints[k] != genValue(TypeInt64, 0, k) {
				return fmt.Errorf("ints[%d] = %d, want %v", k, ints[k], genValue(TypeInt64, 0, k))
			}
			if flts[k] != genValue(TypeFloat64, 0, k) {
				return fmt.Errorf("floats[%d] = %v, want %v", k, flts[k], genValue(TypeFloat64, 0, k))
			}
		}
		return nil
	})
	if err != nil {
		t.Errorf("ReadRowGroupScoped of three columns together: %v", err)
	}

	// Asking for a type the column does not hold is an error, not a wrong slice.
	if err := r.ReadRowGroupScoped(0, []int{at[TypeInt64]}, func(c *Columns) error {
		if _, err := Column[string](c, 0); err == nil {
			t.Error("Column[string] on an int64 column succeeded, want a type mismatch error")
		}
		// A position past the requested set is out of range even when the schema
		// has a column there.
		if _, err := Column[int64](c, 1); err == nil {
			t.Error("Column at position 1 succeeded, want out of range")
		}
		if _, err := Column[int64](c, -1); err == nil {
			t.Error("Column at position -1 succeeded, want out of range")
		}
		return nil
	}); err != nil {
		t.Errorf("ReadRowGroupScoped of a mismatched type: %v", err)
	}

	if err := r.ReadRowGroupScoped(0, []int{len(allTypes)}, func(c *Columns) error {
		return nil
	}); err == nil {
		t.Error("ReadRowGroupScoped past the last column succeeded, want out of range")
	}
	if err := r.ReadRowGroupScoped(0, []int{-1}, func(c *Columns) error {
		return nil
	}); err == nil {
		t.Error("ReadRowGroupScoped of column -1 succeeded, want out of range")
	}
	if err := r.ReadRowGroupScoped(1, []int{0}, func(c *Columns) error {
		return nil
	}); err == nil {
		t.Error("ReadRowGroupScoped on row group 1 succeeded, want out of range")
	}

	// The caller's error reaches the caller, and the read it came from is not
	// silent about it.
	want := errors.New("keine test: stop early")
	if got := r.ReadRowGroupScoped(0, []int{0}, func(c *Columns) error {
		return want
	}); !errors.Is(got, want) {
		t.Errorf("ReadRowGroupScoped returned %v, want the caller's %v", got, want)
	}
}

// A nullable column has no room in a typed slice, so the scoped path refuses it
// rather than handing back a column with a hole in it.
func TestReadRowGroupScopedNulls(t *testing.T) {
	schema := []ColumnSchema{
		{Name: "n", Type: TypeInt64, Nullable: true},
		{Name: "d", Type: TypeInt64},
	}
	cols := [][]any{
		{int64(1), nil, int64(3)},
		{int64(1), int64(2), int64(3)},
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
	if err := r.ReadRowGroupScoped(0, []int{0}, func(c *Columns) error {
		return nil
	}); err == nil {
		t.Error("ReadRowGroupScoped of a nullable column succeeded, want an error")
	}

	// A dense column in the same file reads fine, so the refusal is the nulls.
	if err := r.ReadRowGroupScoped(0, []int{1}, func(c *Columns) error {
		got, err := Column[int64](c, 0)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, []int64{1, 2, 3}) {
			return fmt.Errorf("Column[int64] = %v, want [1 2 3]", got)
		}
		return nil
	}); err != nil {
		t.Errorf("ReadRowGroupScoped of the dense column: %v", err)
	}
}

// A chunk whose encoding produces a wider type than its column declares still
// narrows on the scoped path, so a caller asking for the declared type gets it.
func TestReadRowGroupScopedNarrows(t *testing.T) {
	chunk := craftChunk(EncDelta, CompressNone, nil, EncodeDelta([]int64{42, 43}))
	meta := ColMeta{ByteLength: int64(len(chunk)), Encoding: EncDelta, Compress: CompressNone}
	file := craftFile(chunk, meta, []ColumnSchema{{Name: "i", Type: TypeInt8}}, 2)

	r, err := NewReader(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	err = r.ReadRowGroupScoped(0, []int{0}, func(c *Columns) error {
		got, err := Column[int8](c, 0)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, []int8{42, 43}) {
			return fmt.Errorf("Column[int8] = %v, want [42 43]", got)
		}
		// Asking for the intermediate type is a mismatch. The column declares int8,
		// so that is what it decodes to here.
		if _, err := Column[int64](c, 0); err == nil {
			return errors.New("Column[int64] on an int8 column succeeded, want a type mismatch error")
		}
		return nil
	})
	if err != nil {
		t.Errorf("ReadRowGroupScoped of a delta on an int8 column: %v", err)
	}
}

// TestMultiBlockRoundTrip covers columns long enough to split into several
// blocks. A block boundary is a byte offset into the encoded stream, so the
// decoder has to reassemble values identical to the ones written, and a column has
// to round trip through both read paths when it spans blocks.
func TestMultiBlockRoundTrip(t *testing.T) {
	// Enough values for a little over two blocks at eight bytes each, so the
	// last block is short and the first two are full.
	const rows = 2*blockSize/8 + 1000

	ints := make([]any, rows)
	strs := make([]any, rows)
	for i := range ints {
		ints[i] = int64(i*31 + 7)
		strs[i] = fmt.Sprintf("value-%d", i)
	}
	schema := []ColumnSchema{
		{Name: "i", Type: TypeInt64},
		{Name: "s", Type: TypeString},
	}

	var buf bytes.Buffer
	w := NewWriter(&buf, schema)
	if err := w.AddRowGroup([][]any{ints, strs}); err != nil {
		t.Fatalf("AddRowGroup: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if got, err := r.ReadRowGroup(0, []int{0}); err != nil {
		t.Fatalf("ReadRowGroup: %v", err)
	} else if !reflect.DeepEqual(got[0], ints) {
		t.Errorf("int64 column spanning blocks round tripped to %d values, want %d", len(got[0]), rows)
	}

	// The string column, and the typed read path, exercise the same blocks.
	if got, err := r.ReadRowGroup(0, []int{1}); err != nil {
		t.Fatalf("ReadRowGroup of the string column: %v", err)
	} else if !reflect.DeepEqual(got[0], strs) {
		t.Errorf("string column spanning blocks round tripped to %d values, want %d", len(got[0]), rows)
	}
	if got, err := ReadColumn[int64](r, 0, 0); err != nil {
		t.Fatalf("ReadColumn: %v", err)
	} else if len(got) != rows {
		t.Errorf("ReadColumn of a multi block column: got %d values, want %d", len(got), rows)
	}
}

// TestReadRowGroupScopedReused reads the same columns over and over. The scoped
// path decodes into destinations the Reader keeps, so a second read overwrites the
// first's values. A decoder that only writes some of its elements would leave the
// previous read's behind for the caller to see. The encodings that do that are the
// ones with a zero or a run-length form: a bitmap, bit packed indices and a
// dictionary whose values are all one entry.
func TestReadRowGroupScopedReused(t *testing.T) {
	schema := []ColumnSchema{
		{Name: "b", Type: TypeBool},
		{Name: "i", Type: TypeInt64},
		{Name: "s", Type: TypeString},
	}
	allFalse := make([]any, rowsPerGroup)
	for i := range allFalse {
		allFalse[i] = false
	}
	ints := make([]any, rowsPerGroup)
	for i := range ints {
		ints[i] = int64(i)
	}
	strs := make([]any, rowsPerGroup)
	for i := range strs {
		strs[i] = "same"
	}

	var buf bytes.Buffer
	w := NewWriter(&buf, schema)
	if err := w.AddRowGroup([][]any{allFalse, ints, strs}); err != nil {
		t.Fatalf("AddRowGroup: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	// read returns the columns it asked for and copies them out of the Reader's
	// buffers, so the values survive the next call. The copy exists because nothing
	// else does it.
	read := func(cols []int) ([][]any, error) {
		got := make([][]any, len(cols))
		err := r.ReadRowGroupScoped(0, cols, func(c *Columns) error {
			for k := range cols {
				if c.errs[k] != nil {
					return c.errs[k]
				}
				got[k] = boxValues(c.typed[k])
			}
			return nil
		})
		return got, err
	}

	want := [][]any{allFalse, ints, strs}
	for pass := 0; pass < 3; pass++ {
		got, err := read([]int{0, 1, 2})
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		for k := range want {
			if !reflect.DeepEqual(got[k], want[k]) {
				t.Errorf("pass %d, column %d: got %v, want %d values", pass, k, got[k], len(want[k]))
			}
		}
	}

	// A narrower read reuses only the first destinations and leaves the rest
	// holding the wide read's values, so a later wide read has to overwrite all
	// of them rather than the few it shares an index with.
	if _, err := read([]int{0}); err != nil {
		t.Fatalf("narrow read: %v", err)
	}
	got, err := read([]int{0, 1, 2})
	if err != nil {
		t.Fatalf("wide read after a narrow one: %v", err)
	}
	for k := range want {
		if !reflect.DeepEqual(got[k], want[k]) {
			t.Errorf("after a narrow read, column %d: got %v, want %d values", k, got[k], len(want[k]))
		}
	}

	// A boxed read and a typed read on the same Reader mix destinations of their
	// own with the shared ones, and have to come out right in either order.
	boxed, err := r.ReadRowGroup(0, []int{1})
	if err != nil {
		t.Fatalf("ReadRowGroup: %v", err)
	}
	if !reflect.DeepEqual(boxed[0], ints) {
		t.Errorf("ReadRowGroup after scoped reads: got %d values, want %d", len(boxed[0]), rowsPerGroup)
	}
	typed, err := read([]int{1})
	if err != nil {
		t.Fatalf("scoped read after ReadRowGroup: %v", err)
	}
	if !reflect.DeepEqual(typed[0], ints) {
		t.Errorf("scoped read after ReadRowGroup: got %d values, want %d", len(typed[0]), rowsPerGroup)
	}
}

// TestReadRowGroupScopedAcrossRowGroups reads two row groups through one
// Reader. The destinations and the decompression buffers are the Reader's and
// are shared between the groups, so the second group's values have to land in
// buffers the first group filled. The second group is deliberately the neutral
// one for the encodings that only write some of their elements, which is where
// leftovers would show.
func TestReadRowGroupScopedAcrossRowGroups(t *testing.T) {
	schema := []ColumnSchema{
		{Name: "b", Type: TypeBool},
		{Name: "s", Type: TypeString},
	}
	trues := make([]any, rowsPerGroup)
	distinct := make([]any, rowsPerGroup)
	for i := range trues {
		trues[i] = true
		distinct[i] = fmt.Sprintf("value-%d", i)
	}
	falses := make([]any, rowsPerGroup)
	sames := make([]any, rowsPerGroup)
	for i := range falses {
		falses[i] = false
		sames[i] = "same"
	}

	var buf bytes.Buffer
	w := NewWriter(&buf, schema)
	if err := w.AddRowGroup([][]any{trues, distinct}); err != nil {
		t.Fatalf("AddRowGroup of the first group: %v", err)
	}
	if err := w.AddRowGroup([][]any{falses, sames}); err != nil {
		t.Fatalf("AddRowGroup of the second group: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if r.RowGroupCount() != 2 {
		t.Fatalf("file has %d row groups, want 2", r.RowGroupCount())
	}

	// The first group fills the destinations, the second has to overwrite all of
	// it. Reading the first group twice also covers a repeat using the same
	// buffers the first read left behind.
	for i := 0; i < 2; i++ {
		if err := r.ReadRowGroupScoped(0, []int{0, 1}, func(c *Columns) error {
			bools, err := Column[bool](c, 0)
			if err != nil {
				return err
			}
			strs, err := Column[string](c, 1)
			if err != nil {
				return err
			}
			for k := range bools {
				if !bools[k] {
					return fmt.Errorf("bools[%d] = false, want true", k)
				}
				if strs[k] != distinct[k] {
					return fmt.Errorf("strings[%d] = %q, want %q", k, strs[k], distinct[k])
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("reading the first group, pass %d: %v", i, err)
		}
	}

	err = r.ReadRowGroupScoped(1, []int{0, 1}, func(c *Columns) error {
		bools, err := Column[bool](c, 0)
		if err != nil {
			return err
		}
		strs, err := Column[string](c, 1)
		if err != nil {
			return err
		}
		for k := range bools {
			if bools[k] {
				return fmt.Errorf("bools[%d] = true, want false; the bitmap was not cleared", k)
			}
			if strs[k] != "same" {
				return fmt.Errorf("strings[%d] = %q, want %q", k, strs[k], "same")
			}
		}
		return nil
	})
	if err != nil {
		t.Errorf("reading the second group after the first: %v", err)
	}
}

func TestTypedDecodeErrors(t *testing.T) {
	if _, err := decodePlainTyped([]byte{0x01}, TypeBytes, &dest{}, 0); err == nil {
		t.Error("decodePlainTyped with a truncated byte length prefix: want error, got nil")
	}
	prefix := []byte{0xff, 0xff, 0xff, 0xff}
	if _, err := decodePlainTyped(append(prefix, 1, 2), TypeBytes, &dest{}, 0); err == nil {
		t.Error("decodePlainTyped with a byte length beyond the data: want error, got nil")
	}
	if _, err := decodePlainTyped(nil, 0xFF, &dest{}, 0); err == nil {
		t.Error("decodePlainTyped with an unknown type: want error, got nil")
	}
	if _, err := decodeTyped(0xFF, nil, 0, TypeInt64, &dest{}, nil); err == nil {
		t.Error("decodeTyped with an unknown encoding: want error, got nil")
	}
	if got, ok := asValues[int64](nil); ok {
		t.Errorf("asValues on a non-slice = %v, want not ok", got)
	}
	if encProducesType(0xFF, TypeInt64) {
		t.Error("encProducesType of an unknown encoding = true, want false")
	}
	for _, enc := range []uint8{EncPlain, EncRLEBitpack, EncDelta, EncDict, EncOffsetBytes} {
		if !encProducesType(enc, typeEncodingMatch[enc]) {
			t.Errorf("encProducesType(%d) = false, want true for its own type", enc)
		}
	}
}

// typeEncodingMatch is the type each encoding produces values of directly.
var typeEncodingMatch = map[uint8]uint8{
	EncPlain:       TypeInt64,
	EncRLEBitpack:  TypeBool,
	EncDelta:       TypeInt64,
	EncDict:        TypeString,
	EncOffsetBytes: TypeBytes,
	EncAffix:       TypeString,
}

// A reader knows how many values a chunk holds before it decodes them, from the
// row group's row count, so decodePlainTyped takes the count and does not have to
// walk the length prefixes first. The count sizes the buffers, and the data still
// has to agree with it: a stream that holds fewer values than the count claims, or
// more, is refused.
func TestPlainValueCount(t *testing.T) {
	prefix := func(n int) []byte {
		var p [4]byte
		binary.LittleEndian.PutUint32(p[:], uint32(n))
		return p[:]
	}
	enc := func(vals ...string) []byte {
		var b []byte
		for _, v := range vals {
			b = append(b, prefix(len(v))...)
			b = append(b, v...)
		}
		return b
	}
	one := enc("ab")
	cutShort := append(enc("ab"), 0x01, 0x02)
	tooLong := append(prefix(5), 1)
	for _, tc := range []struct {
		name string
		data []byte
		n    int
		typ  uint8
	}{
		{"count beyond the data", one[:1], 1, TypeString},
		{"fewer values than the count", cutShort, 2, TypeString},
		{"a value longer than the count leaves room for", tooLong, 1, TypeString},
		{"a value longer than the count leaves room for", tooLong, 1, TypeBytes},
		{"a prefix the data cuts short", cutShort, 2, TypeBytes},
		{"a value the count leaves out", enc("", ""), 1, TypeString},
		{"a value the byte count leaves out", enc("", ""), 1, TypeBytes},
	} {
		if _, err := decodePlainTyped(tc.data, tc.typ, &dest{}, tc.n); err == nil {
			t.Errorf("decodePlainTyped with %s: want error, got nil", tc.name)
		}
	}

	// A count that fits decodes the same values a walk of the data would have found.
	strs, err := decodePlainTyped(enc("ab", "", "cdef"), TypeString, &dest{}, 3)
	if err != nil {
		t.Fatalf("decodePlainTyped(strings, n=3): %v", err)
	}
	if got, _ := strs.([]string); !reflect.DeepEqual(got, []string{"ab", "", "cdef"}) {
		t.Errorf("decodePlainTyped(strings, n=3) = %v, want [ab  cdef]", got)
	}
	byts, err := decodePlainTyped(enc("ab", "", "cdef"), TypeBytes, &dest{}, 3)
	if err != nil {
		t.Fatalf("decodePlainTyped(bytes, n=3): %v", err)
	}
	got, _ := byts.([][]byte)
	want := [][]byte{[]byte("ab"), nil, []byte("cdef")}
	if len(got) != len(want) {
		t.Fatalf("decodePlainTyped(bytes, n=3) returned %d values, want %d", len(got), len(want))
	}
	for k := range want {
		if string(got[k]) != string(want[k]) {
			t.Errorf("decodePlainTyped(bytes, n=3)[%d] = %q, want %q", k, got[k], want[k])
		}
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

// buildTypedSchema is buildSchema with no nullable column, since a typed slice
// has nowhere to put a null.
func buildTypedSchema() []ColumnSchema {
	schema := make([]ColumnSchema, len(allTypes))
	for i, typ := range allTypes {
		schema[i] = ColumnSchema{Name: fmt.Sprintf("col%d_%d", i, typ), Type: typ}
	}
	return schema
}

// buildValueColumns is buildColumns with the nulls left out, so the same values
// can go to either entry.
func buildValueColumns(g int) [][]any {
	cols := make([][]any, len(allTypes))
	for c, typ := range allTypes {
		col := make([]any, rowsPerGroup)
		for r := 0; r < rowsPerGroup; r++ {
			col[r] = genValue(typ, g, r)
		}
		cols[c] = col
	}
	return cols
}

// buildTypedColumns is the typed form of buildValueColumns: the same values,
// run through the canonicaliser the boxed entry uses, so a column handed to
// AddRowGroupTyped is the column AddRowGroup would have built from the same
// values.
func buildTypedColumns(g int) []any {
	cols := make([]any, len(allTypes))
	for c, typ := range allTypes {
		typed, err := canonicalColumnTyped(buildValueColumns(g)[c], typ)
		if err != nil {
			panic(fmt.Sprintf("canonicalColumnTyped(%s): %v", typeNames[typ], err))
		}
		cols[c] = typed
	}
	return cols
}

// TestAddRowGroupTyped writes the same three row groups both ways and compares
// the files byte for byte, which is the whole claim the typed entry makes: no
// canonicalisation, and no change on disk.
func TestAddRowGroupTyped(t *testing.T) {
	schema := buildTypedSchema()

	var boxed, typed bytes.Buffer
	bw := NewWriter(&boxed, schema)
	tw := NewWriter(&typed, schema)
	for g := 0; g < 3; g++ {
		if err := bw.AddRowGroup(buildValueColumns(g)); err != nil {
			t.Fatalf("AddRowGroup(%d): %v", g, err)
		}
		if err := tw.AddRowGroupTyped(buildTypedColumns(g)); err != nil {
			t.Fatalf("AddRowGroupTyped(%d): %v", g, err)
		}
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !bytes.Equal(boxed.Bytes(), typed.Bytes()) {
		t.Errorf("typed write produced %d bytes, the boxed write %d over the same values",
			len(typed.Bytes()), len(boxed.Bytes()))
	}

	r, err := NewReader(bytes.NewReader(typed.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	all := make([]int, len(allTypes))
	for i := range all {
		all[i] = i
	}
	for g := 0; g < 3; g++ {
		got, err := r.ReadRowGroup(g, all)
		if err != nil {
			t.Fatalf("ReadRowGroup(%d, all): %v", g, err)
		}
		want := buildValueColumns(g)
		for c := range want {
			if !reflect.DeepEqual(got[c], want[c]) {
				t.Errorf("row group %d column %d (%s): got %v, want %v",
					g, c, schema[c].Name, got[c], want[c])
			}
		}
	}
}

// TestOptimizeTyped runs the pass both ways and then writes with the layouts it
// chose, so a typed caller reaches the same file the boxed one does.
func TestOptimizeTyped(t *testing.T) {
	schema := buildTypedSchema()

	var boxed, typed bytes.Buffer
	bw := NewWriter(&boxed, schema)
	tw := NewWriter(&typed, schema)

	if err := bw.Optimize(buildValueColumns(0)); err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if err := tw.OptimizeTyped(buildTypedColumns(0)); err != nil {
		t.Fatalf("OptimizeTyped: %v", err)
	}
	for i := range allTypes {
		box, typ := bw.layouts[i], tw.layouts[i]
		if box.Encoding != typ.Encoding || box.Compress != typ.Compress ||
			box.EncodedSize != typ.EncodedSize || box.CompressedSize != typ.CompressedSize {
			t.Errorf("column %d (%s): typed pass chose %v, boxed chose %v",
				i, schema[i].Name, typ, box)
		}
	}

	for g := 0; g < 3; g++ {
		if err := bw.AddRowGroup(buildValueColumns(g)); err != nil {
			t.Fatalf("AddRowGroup(%d): %v", g, err)
		}
		if err := tw.AddRowGroupTyped(buildTypedColumns(g)); err != nil {
			t.Fatalf("AddRowGroupTyped(%d): %v", g, err)
		}
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !bytes.Equal(boxed.Bytes(), typed.Bytes()) {
		t.Errorf("after Optimize, the typed write produced %d bytes and the boxed one %d",
			len(typed.Bytes()), len(boxed.Bytes()))
	}

	// What the reader reports has to be the layout the pass picked, not the one
	// the type implies.
	r, err := NewReader(bytes.NewReader(typed.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	for g := 0; g < 3; g++ {
		rg, err := r.RowGroupMeta(g)
		if err != nil {
			t.Fatalf("RowGroupMeta(%d): %v", g, err)
		}
		for c := range allTypes {
			if rg.Columns[c].Encoding != tw.layouts[c].Encoding {
				t.Errorf("row group %d column %d: encoding is %d, the pass chose %d",
					g, c, rg.Columns[c].Encoding, tw.layouts[c].Encoding)
			}
		}
	}
}

// TestAddRowGroupTypedErrors covers the contract the typed entry refuses, each
// case one the boxed entry would have served.
func TestAddRowGroupTypedErrors(t *testing.T) {
	schema := buildTypedSchema()
	cols := buildTypedColumns(0)

	cases := []struct {
		name    string
		schema  []ColumnSchema
		columns func() []any
	}{{
		name:   "nullable column",
		schema: nullableSchema(schema),
		columns: func() []any {
			return cols
		},
	}, {
		name:   "wrong element type",
		schema: schema,
		columns: func() []any {
			c := append([]any(nil), cols...)
			c[4] = []int32{1, 2, 3}
			return c
		},
	}, {
		name:   "untyped nil column",
		schema: schema,
		columns: func() []any {
			c := append([]any(nil), cols...)
			c[0] = nil
			return c
		},
	}, {
		name:   "mismatched lengths",
		schema: schema,
		columns: func() []any {
			c := append([]any(nil), cols...)
			c[1] = c[1].([]int8)[:rowsPerGroup-1]
			return c
		},
	}, {
		name:   "wrong column count",
		schema: schema,
		columns: func() []any {
			return cols[:len(cols)-1]
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf, tc.schema)
			if err := w.AddRowGroupTyped(tc.columns()); err == nil {
				t.Error("AddRowGroupTyped succeeded, want a type or null error")
			}
			// A refused column is refused before any bytes are written, so the
			// file still has only its header and Close can still finish it.
			if buf.Len() != len(magic)+1 {
				t.Errorf("after a refused AddRowGroupTyped the buffer holds %d bytes, want %d",
					buf.Len(), len(magic)+1)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close after a refused AddRowGroupTyped: %v", err)
			}
		})
	}

	// OptimizeTyped refuses the same columns.
	var buf bytes.Buffer
	w := NewWriter(&buf, nullableSchema(schema))
	if err := w.OptimizeTyped(cols); err == nil {
		t.Error("OptimizeTyped on a nullable column succeeded, want an error")
	}
}

// nullableSchema is schema with every column nullable, which is how the typed
// entries cannot be used.
func nullableSchema(schema []ColumnSchema) []ColumnSchema {
	out := make([]ColumnSchema, len(schema))
	for i, s := range schema {
		s.Nullable = true
		out[i] = s
	}
	return out
}

// TestTypedEntriesReportSetup covers the paths the typed entries share with the
// boxed ones: a writer that never got past its own setup reports that instead of
// trying a row group, and a schema type no column carries is a mismatch rather
// than a column stored some other way.
func TestTypedEntriesReportSetup(t *testing.T) {
	schema := buildTypedSchema()
	cols := buildTypedColumns(0)

	var buf bytes.Buffer
	w := NewWriterWithOptions(&buf, schema, Options{Compress: 99})
	if err := w.AddRowGroupTyped(cols); err == nil {
		t.Error("AddRowGroupTyped with an unusable codec succeeded, want the setup error")
	}
	if err := w.OptimizeTyped(cols); err == nil {
		t.Error("OptimizeTyped with an unusable codec succeeded, want the setup error")
	}

	// A type tag none of the thirteen columns carries.
	unknown := append([]ColumnSchema(nil), schema...)
	unknown[0] = ColumnSchema{Name: "mystery", Type: 99}
	var buf2 bytes.Buffer
	w = NewWriter(&buf2, unknown)
	if err := w.AddRowGroupTyped(cols); err == nil {
		t.Error("AddRowGroupTyped with an unknown schema type succeeded, want an error")
	}
	if err := w.OptimizeTyped(cols); err == nil {
		t.Error("OptimizeTyped with an unknown schema type succeeded, want an error")
	}
}
