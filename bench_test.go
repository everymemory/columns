package keine

import (
	"bytes"
	"fmt"
	"testing"
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
				col[i] = fmt.Sprintf("id-%d-keine-suffix", i)
			}
			return col
		},
	},
}

const benchRows = 10000

// BenchmarkOptimize measures what scanning a whole column costs, which Optimize
// pays once per dataset to stop guessing from the type. BenchmarkSizes next to it
// shows what the scan buys.
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

// BenchmarkSizes reports what each candidate costs, since the choice Optimize
// makes is only meaningful next to the alternatives.
func BenchmarkSizes(b *testing.B) {
	for _, c := range benchColumns {
		col := c.build(benchRows)
		for _, r := range BenchmarkLayouts(col, c.schema) {
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
// the interface boxing ReadRowGroup pays for. Set against BenchmarkRead on the
// same column, it shows what that boxing costs.
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
// a high cardinality string that shares a suffix. comparisonRows is the row
// count the whole-file numbers in the README are reported at.
var comparisonColumns = []*benchColumn{
	&benchColumns[1], // int64-run
	&benchColumns[2], // int64-random
	&benchColumns[3], // float64
	&benchColumns[4], // string-low-card
	&benchColumns[5], // string-high-card
}

const comparisonRows = 200000

// BenchmarkComparison writes and reads one row group of all five comparison
// columns in a single file, which is how a workload actually uses the format:
// the columns are encoded in parallel and share one footer. It reports the file
// size and both directions of the transfer, so the README's numbers are
// reproducible with `go test -bench=BenchmarkComparison`.
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
	b.Run("write optimized", func(b *testing.B) {
		// The default layouts are the type's, and Optimize is what it costs to
		// trade them for the column's. Reported next to the default write, the
		// difference is the price of the pass and what it bought.
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
		b.ReportMetric(float64(len(first))/float64(comparisonRows), "B/row")

		b.ResetTimer()
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

// BenchmarkPartialRead measures skipping the columns a query does not want,
// which is what ColMeta.ByteLength exists for.
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
