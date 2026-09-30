package columns

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/everymemory/columns/tokenizer"
)

// benchColumn is one synthetic column used by the benchmarks.
type benchColumn struct {
	name   string
	schema ColumnSchema
	build  func(n int) []any
}

var benchColumns = []benchColumn{
	{
		name:   "bool",
		schema: ColumnSchema{Name: "b", Type: TypeBool},
		build: func(n int) []any {
			col := make([]any, n)
			for i := range col {
				col[i] = i%100 < 90
			}
			return col
		},
	},
	{
		name:   "int64-run",
		schema: ColumnSchema{Name: "i", Type: TypeInt64},
		build: func(n int) []any {
			col := make([]any, n)
			for i := range col {
				col[i] = int64(i * 7)
			}
			return col
		},
	},
	{
		name:   "int64-random",
		schema: ColumnSchema{Name: "i", Type: TypeInt64},
		build: func(n int) []any {
			col := make([]any, n)
			x := int64(0x2545F4914F6CDD1D)
			for i := range col {
				x = x*x%x<<13 ^ x>>7
				col[i] = x
			}
			return col
		},
	},
	{
		name:   "float64",
		schema: ColumnSchema{Name: "f", Type: TypeFloat64},
		build: func(n int) []any {
			col := make([]any, n)
			for i := range col {
				col[i] = float64(i) * 1.0001
			}
			return col
		},
	},
	{
		name:   "string-low-card",
		schema: ColumnSchema{Name: "s", Type: TypeString},
		build: func(n int) []any {
			col := make([]any, n)
			for i := range col {
				col[i] = fmt.Sprintf("customer-%d@example.com", i%200)
			}
			return col
		},
	},
	{
		name:   "string-high-card",
		schema: ColumnSchema{Name: "s", Type: TypeString},
		build: func(n int) []any {
			col := make([]any, n)
			for i := range col {
				col[i] = fmt.Sprintf("id-%d-suffix", i)
			}
			return col
		},
	},
}

const benchRows = 10000

// BenchmarkOptimize measures the cost of scanning a whole column, which Optimize
// pays once per dataset to stop guessing from the type. BenchmarkSizes shows what
// that scan buys.
func BenchmarkOptimize(b *testing.B) {
	for _, c := range benchColumns {
		col := c.build(benchRows)
		schema := []ColumnSchema{c.schema}
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				buf := &bytes.Buffer{}
				w := NewWriter(buf, schema)
				if err := w.Optimize([][]any{col}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSizes reports the cost of each candidate. The choice Optimize makes is
// only meaningful next to the alternatives.
func BenchmarkSizes(b *testing.B) {
	for _, c := range benchColumns {
		col := c.build(benchRows)
		for _, r := range BenchmarkLayouts(col, c.schema, nil) {
			b.Run(c.name+"/"+r.Name, func(b *testing.B) {
				b.ReportMetric(float64(r.CompressedSize), "bytes")
				b.ReportMetric(float64(r.CompressedSize)/float64(benchRows), "B/row")
			})
		}
	}
}

// BenchmarkWriter covers AddRowGroup and Close, which is the whole write path.
func BenchmarkWriter(b *testing.B) {
	for _, c := range benchColumns {
		col := c.build(benchRows)
		schema := []ColumnSchema{c.schema}
		b.Run(c.name, func(b *testing.B) {
			buf := &bytes.Buffer{}
			w := NewWriter(buf, schema)
			if err := w.AddRowGroup([][]any{col}); err != nil {
				b.Fatal(err)
			}
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(buf.Len()), "bytes")
			b.ReportMetric(float64(buf.Len())/float64(benchRows), "B/row")

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf.Reset()
				w = NewWriter(buf, schema)
				if err := w.AddRowGroup([][]any{col}); err != nil {
					b.Fatal(err)
				}
				if err := w.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRead covers reading back every column of one row group.
func BenchmarkRead(b *testing.B) {
	for _, c := range benchColumns {
		col := c.build(benchRows)
		schema := []ColumnSchema{c.schema}

		buf := &bytes.Buffer{}
		w := NewWriter(buf, schema)
		if err := w.AddRowGroup([][]any{col}); err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
		file := buf.Bytes()

		r, err := NewReader(bytes.NewReader(file))
		if err != nil {
			b.Fatal(err)
		}

		b.Run(c.name, func(b *testing.B) {
			b.ReportMetric(float64(len(file)), "bytes")
			b.SetBytes(int64(len(file)))

			for i := 0; i < b.N; i++ {
				if _, err := r.ReadRowGroup(0, []int{0}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkTypedRead reads a column into []int64 through ReadColumn, which skips
// the interface boxing ReadRowGroup pays for. Against BenchmarkRead on the same
// column it shows what that boxing costs.
func BenchmarkTypedRead(b *testing.B) {
	for _, c := range benchColumns {
		if c.schema.Type != TypeInt64 {
			continue
		}
		col := c.build(benchRows)
		schema := []ColumnSchema{c.schema}

		buf := &bytes.Buffer{}
		w := NewWriter(buf, schema)
		if err := w.AddRowGroup([][]any{col}); err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
		file := buf.Bytes()

		r, err := NewReader(bytes.NewReader(file))
		if err != nil {
			b.Fatal(err)
		}

		b.Run(c.name, func(b *testing.B) {
			b.ReportMetric(float64(len(file)), "bytes")
			b.SetBytes(int64(len(file)))

			for i := 0; i < b.N; i++ {
				if _, err := ReadColumn[int64](r, 0, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// comparisonColumns are the shapes a columnar workload actually sees: a
// monotonic key, a random key, a low entropy float, a low cardinality string and
// a high cardinality string that shares a suffix. comparisonRows is the row count
// the README's whole-file numbers are reported at.
var comparisonColumns = []*benchColumn{
	&benchColumns[1], // int64-run
	&benchColumns[2], // int64-random
	&benchColumns[3], // float64
	&benchColumns[4], // string-low-card
	&benchColumns[5], // string-high-card
}

const comparisonRows = 200000

// BenchmarkComparison writes and reads one row group of all five comparison
// columns in a single file, which is how a workload uses the format: the columns
// are encoded in parallel and share one footer. It reports the file size and both
// directions of the transfer, so the README's numbers reproduce with
// `go test -bench=BenchmarkComparison`.
func BenchmarkComparison(b *testing.B) {
	schema := make([]ColumnSchema, len(comparisonColumns))
	columns := make([][]any, len(comparisonColumns))
	for i, c := range comparisonColumns {
		schema[i] = c.schema
		columns[i] = c.build(comparisonRows)
	}

	file := func() []byte {
		buf := &bytes.Buffer{}
		w := NewWriter(buf, schema)
		if err := w.AddRowGroup(columns); err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
		return buf.Bytes()
	}
	data := file()
	b.ReportMetric(float64(len(data)), "bytes")
	b.ReportMetric(float64(len(data))/float64(comparisonRows), "B/row")

	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		b.Fatal(err)
	}
	read := make([]int, len(comparisonColumns))
	for i := range read {
		read[i] = i
	}

	b.Run("write", func(b *testing.B) {
		b.ReportMetric(float64(len(data))/float64(comparisonRows), "B/row")
		for i := 0; i < b.N; i++ {
			file()
		}
	})
	b.Run("write typed", func(b *testing.B) {
		// AddRowGroupTyped hands the encoders the slices the caller already has,
		// so the difference against write is the canonicalisation collect does.
		typed := make([]any, len(columns))
		for i, col := range columns {
			t, err := canonicalColumnTyped(col, schema[i].Type)
			if err != nil {
				b.Fatal(err)
			}
			typed[i] = t
		}
		write := func() []byte {
			buf := &bytes.Buffer{}
			w := NewWriter(buf, schema)
			if err := w.AddRowGroupTyped(typed); err != nil {
				b.Fatal(err)
			}
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}
			return buf.Bytes()
		}
		first := write()
		if !bytes.Equal(first, data) {
			b.Fatalf("typed write produced %d bytes, the boxed write %d", len(first), len(data))
		}

		b.ResetTimer()
		b.ReportMetric(float64(len(first))/float64(comparisonRows), "B/row")
		for i := 0; i < b.N; i++ {
			write()
		}
	})
	b.Run("write optimized", func(b *testing.B) {
		// The default layouts are the type's, and Optimize is what it costs to trade
		// them for the column's. Next to the default write, the difference is the
		// price of the pass and what it bought.
		optimized := func() []byte {
			buf := &bytes.Buffer{}
			w := NewWriter(buf, schema)
			if err := w.Optimize(columns); err != nil {
				b.Fatal(err)
			}
			if err := w.AddRowGroup(columns); err != nil {
				b.Fatal(err)
			}
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}
			return buf.Bytes()
		}
		first := optimized()

		b.ResetTimer()
		b.ReportMetric(float64(len(first))/float64(comparisonRows), "B/row")
		for i := 0; i < b.N; i++ {
			optimized()
		}
	})
	b.Run("read", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for i := 0; i < b.N; i++ {
			if _, err := r.ReadRowGroup(0, read); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("read scoped", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for i := 0; i < b.N; i++ {
			err := r.ReadRowGroupScoped(0, read, func(c *Columns) error {
				for j := range read {
					switch comparisonColumns[j].schema.Type {
					case TypeInt64:
						if _, err := Column[int64](c, j); err != nil {
							return err
						}
					case TypeFloat64:
						if _, err := Column[float64](c, j); err != nil {
							return err
						}
					default:
						if _, err := Column[string](c, j); err != nil {
							return err
						}
					}
				}
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkPartialRead measures skipping the columns a query does not want. That
// is what ColMeta.ByteLength exists for.
func BenchmarkPartialRead(b *testing.B) {
	const cols = 8
	schema := make([]ColumnSchema, cols)
	columns := make([][]any, cols)
	for i := 0; i < cols; i++ {
		schema[i] = ColumnSchema{Name: fmt.Sprintf("c%d", i), Type: TypeString}
		col := make([]any, benchRows)
		for j := range col {
			col[j] = fmt.Sprintf("value-%d-%d", i, j%100)
		}
		columns[i] = col
	}

	buf := &bytes.Buffer{}
	w := NewWriter(buf, schema)
	if err := w.AddRowGroup(columns); err != nil {
		b.Fatal(err)
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
	file := buf.Bytes()

	r, err := NewReader(bytes.NewReader(file))
	if err != nil {
		b.Fatal(err)
	}

	b.Run("one-of-eight", func(b *testing.B) {
		b.SetBytes(int64(len(file)))
		for i := 0; i < b.N; i++ {
			if _, err := r.ReadRowGroup(0, []int{3}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// tokenizedRows is the row count the tokenized benchmark builds. A text column
// is what tokenization is for, and its values are longer than the comparison
// file's, so fewer of them is still enough text for the pass to measure against
// and keeps the benchmark runnable: the tokenizer walks every value once.
const tokenizedRows = 20000

// tokenizedColumns is a key plus a column of prose. Prose is the case the
// comparison file's two string columns are not: its words repeat but its values
// do not, so the whole value is not compressible and the ids of its pieces are.
var tokenizedColumns = []benchColumn{
	benchColumns[1], // int64-run
	{
		name:   "prose",
		schema: ColumnSchema{Name: "t", Type: TypeString},
		build: func(n int) []any {
			words := proseWords
			col := make([]any, n)
			rnd := rand.New(rand.NewSource(tokenizedSeed))
			for i := range col {
				var sb strings.Builder
				k := 8 + rnd.Intn(24)
				for j := 0; j < k; j++ {
					sb.WriteString(words[rnd.Intn(len(words))])
					switch rnd.Intn(6) {
					case 0:
						sb.WriteByte(',')
					case 1:
						sb.WriteByte('.')
					case 2:
						sb.WriteString("'s")
					}
					sb.WriteByte(' ')
				}
				col[i] = sb.String()
			}
			return col
		},
	},
}

// tokenizedSeed keeps the prose column reproducible, so the file the benchmark
// writes is the same one every run and the size it reports does not move.
const tokenizedSeed = 20260929

// proseWords is the vocabulary the prose column draws from. Real text is what
// tokenization is for, and a fixed word list keeps the column reproducible
// while it still covers the contractions and punctuation the pre-tokenizer
// splits on.
var proseWords = strings.Fields(`the quick brown fox jumps over lazy dog and then ran away into
the forest where it found a small wooden house with a red door and green windows
whose chimney smoked while birds sang songs about distant mountains and rivers
flowing through the valleys below the clouds drifted across the sky like ships
upon the ocean that crashed against the rocky shore leaving foam behind each time
the water retreated back toward the horizon where the sun was setting in colors
of orange purple pink and gold the old man walked the road thinking about the
days gone past and the memories that flooded his mind like water rushing through
a broken dam the children played in the field kicking a ball and laughing in a
joy so pure and unfiltered that their voices carried on the wind and reached his
ears and made him smile and remember when he was young and played in this very
same field many years ago when nothing had changed and everything had changed
all at once and at the same time a paradox wrapped inside an enigma and a
mystery in itself a question without an answer that he sought to find in the
meaning of the words written on the page between the lines and in the spaces
where the silence spoke volumes more than any sentence ever could express`)

// BenchmarkTokenized writes and reads a text column stored as token ids. It is
// the same shape as BenchmarkComparison with a tokenizer on the text column, so
// the two benchmark the same code path a caller reaches through Options.
func BenchmarkTokenized(b *testing.B) {
	schema := make([]ColumnSchema, len(tokenizedColumns))
	columns := make([][]any, len(tokenizedColumns))
	for i, c := range tokenizedColumns {
		schema[i] = c.schema
		columns[i] = c.build(tokenizedRows)
	}
	tok := registeredModel(b)

	var plain, tokenized []byte
	write := func(opts Options, optimize bool) []byte {
		buf := &bytes.Buffer{}
		w := NewWriterWithOptions(buf, schema, opts)
		if optimize {
			if err := w.Optimize(columns); err != nil {
				b.Fatal(err)
			}
		}
		if err := w.AddRowGroup(columns); err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
		return buf.Bytes()
	}
	plain = write(Options{}, false)
	tokenized = write(Options{Tokenizers: map[int]*tokenizer.Model{1: tok}}, false)

	// The values are the same in both files, so the reader has to hand back the
	// same column either way; the ids are only how it is stored.
	r, err := NewReader(bytes.NewReader(tokenized))
	if err != nil {
		b.Fatal(err)
	}
	if _, err := r.ReadRowGroup(0, []int{1}); err != nil {
		b.Fatal(err)
	}

	b.Run("write", func(b *testing.B) {
		b.ReportMetric(float64(len(plain))/float64(tokenizedRows), "B/row")
		for i := 0; i < b.N; i++ {
			write(Options{}, false)
		}
	})
	b.Run("write tokenized", func(b *testing.B) {
		b.ReportMetric(float64(len(tokenized))/float64(tokenizedRows), "B/row")
		for i := 0; i < b.N; i++ {
			write(Options{Tokenizers: map[int]*tokenizer.Model{1: tok}}, false)
		}
	})
	// Optimize re-tokenizes the column once to measure all nine layouts against it,
	// which is most of this row's time. The pass is free to prefer another
	// encoding, since prose drawn from a fixed word list is a dictionary's best
	// case, so the row reports what it picked rather than assuming the tokenizer
	// won.
	b.Run("write optimized", func(b *testing.B) {
		opts := Options{Tokenizers: map[int]*tokenizer.Model{1: tok}}
		first := write(opts, true)
		b.ResetTimer()
		b.ReportMetric(float64(len(first))/float64(tokenizedRows), "B/row")
		for i := 0; i < b.N; i++ {
			write(opts, true)
		}
		rr, err := NewReader(bytes.NewReader(first))
		if err != nil {
			b.Fatal(err)
		}
		picked := rr.footer.RowGroups[0].Columns[1]
		b.Logf("the pass picked %s+%s for the text column at %.2f B/row",
			encName(picked.Encoding), codecName(picked.Compress),
			float64(len(first))/float64(tokenizedRows))
	})
	b.Run("read scoped", func(b *testing.B) {
		b.SetBytes(int64(len(tokenized)))
		for i := 0; i < b.N; i++ {
			err := r.ReadRowGroupScoped(0, []int{0, 1}, func(c *Columns) error {
				if _, err := Column[int64](c, 0); err != nil {
					return err
				}
				if _, err := Column[string](c, 1); err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
