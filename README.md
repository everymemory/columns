# keine

keine is a columnar file format library written in Go with no dependencies
outside the standard library.

A file is a sequence of row groups. Each row group stores its columns as
separate chunks, so a reader only has to decode the columns a query asks for.
Every column chunk records the encoding and compression codec it was written
with, and the writer picks both by measuring the alternatives rather than
guessing.

## File layout

```
"KEIN"
row group 0
    column chunk
    column chunk
    ...
row group 1
    ...
footer
footer length      uint32, little-endian
"KEIN"
```

A column chunk is:

```
encoding            1 byte
compress            1 byte
null bitmap length  uint32
null bitmap
data length         uint32
data
```

The footer holds the schema and the metadata for every row group: each column's
on-disk byte length, null count, min and max value, and value length bounds. It
is encoded with `encoding/gob`. Because the footer comes last and carries its
own length, a reader can open a file with one seek to the end and then jump
straight to any column of any row group.

## Types and encodings

Thirteen type tags: bool, int8-64, uint8-64, float32, float64, bytes, string.

Five encodings:

| Encoding | Layout |
| --- | --- |
| Plain | Fixed width values back to back, little-endian. Strings and bytes get a uint32 length prefix. |
| RLEBitpack | One bit per value. Used for booleans. |
| Delta | First value verbatim, then differences from the previous value, as int64. |
| Dict | Bit-packed indices into a dictionary of distinct values. |
| OffsetBytes | An offset table over concatenated raw bytes. For strings and byte slices. |

Nulls are handled before any of these apply. A nullable column's chunk carries a
bitmap with one bit per row; only the non-null values are encoded, and the
reader expands them back out.

## Compression

Four codecs from the standard library are wired up: flate, gzip, zlib and lzw.
`CompressNone` stores data uncompressed. `CompressZstd` is defined but
unimplemented, because this toolchain does not ship `compress/zstd`; using it
returns an error rather than silently writing uncompressed data.

Compression is applied after encoding, so the layout experiment measures the
combination. On 5000 rows of synthetic data:

| Column | Plain | Chosen | Bytes |
| --- | --- | --- | --- |
| boolean with runs | 5000 | RLEBitpack+Flate | 21 |
| arithmetic int64 | 40000 | Delta+Flate | 82 |
| 200 distinct strings | 137250 | Dict+Flate | 1397 |

Gzip and zlib wrap the same DEFLATE algorithm as flate but add framing, so they
lose the size contest to flate on every chunk. They remain implemented and tagged
— a file written with them reads back fine — but the writer does not consider
them, because measuring them costs two encode and compress passes per column and
cannot pay for itself.

## Choosing a layout

`ExperimentLayouts` is what the writer calls per column. Encoding and compression
are measured against each other on the actual values, so a sorted id column lands
on Delta and a low-cardinality string column lands on Dict without anyone
hard-coding that.

Which layout wins depends on the shape of a column — its cardinality, whether it
is monotonic, how long its values are — and not on how many rows it has. So the
experiment measures a bounded, evenly spaced sample rather than every value, and
its cost stops scaling with the file. On a 200000-row, 5-column dataset the full
experiment took 9.7s and the sampled one 0.21s, and every column chose the same
encoding and codec, producing a byte-identical file.

## Usage

```go
schema := []keine.ColumnSchema{
    {Name: "id", Type: keine.TypeInt64},
    {Name: "email", Type: keine.TypeString, Nullable: true},
}

var buf bytes.Buffer
w := keine.NewWriter(&buf, schema)
w.AddRowGroup([][]any{
    {int64(1), int64(2), int64(3)},
    {"a@example.com", nil, "c@example.com"},
})
w.Close()

r, _ := keine.NewReader(bytes.NewReader(buf.Bytes()))
cols, _ := r.ReadRowGroup(0, []int{1}) // email only; id is never read
```

`AddRowGroup` accepts `[][]any` with one slice per column, all the same length.
Values are coerced to the schema's Go types, so passing an `int` for a
`TypeInt64` column works and passing a string that does not parse is an error.
`ReadRowGroup` returns values typed to match what was written; rows that were
null come back as `nil`.

When the column holds no nulls, `ReadColumn` reads it into a typed slice instead,
which avoids boxing every value in an interface:

```go
ids, _ := keine.ReadColumn[int64](r, 0, 0)
```

`T` is the Go type for the column — `int64` for `TypeInt64`, `string` for
`TypeString`. A column with nulls has nowhere to put a `nil` in a `[]T`, so it
returns an error and `ReadRowGroup` reads those.

## Status

This is a format exploration, not a production storage engine. It has full
round-trip tests for every type, encoding and codec, and the layout experiment
is exercised against columns chosen to favour each one. There is no schema
evolution, no concurrency control, and no story for appending to an existing
file. Anything writing a real dataset should expect to outgrow it.

## Benchmarks

```
go test -bench=. ./...
```

`BenchmarkExperiment` is the cost the writer pays per column to choose a layout.
`BenchmarkSizes` reports every candidate's size, since the winner is only
meaningful next to what it beat. `BenchmarkWriter` and `BenchmarkRead` cover the
whole write and read paths, `BenchmarkTypedRead` covers the unboxed read path,
and `BenchmarkPartialRead` covers skipping columns.

## Compared with parquet

On 200000 rows in 5 columns, against pyarrow, both formats fed the same values
from the same pseudo-random stream so neither sees easier data:

| | bytes/row | write | read all | read 1 column | read typed |
| --- | --- | --- | --- | --- | --- |
| keine | 9.06 | 260ms | 35ms | 8ms | 33ms |
| parquet zstd | 12.86 | 102ms | 17ms | 9ms | — |
| parquet snappy | 21.32 | 96ms | 18ms | 8ms | — |
| parquet none | 43.71 | 81ms | 13ms | 8ms | — |

keine is smaller than parquet at every compression level, and reading one column
is level with it. Writing is still slower, and what is left of the gap sits
mostly in one place: `compress/flate` at its default level.

Encoding and compressing a column touches no other, so the writer runs them
across columns at once and writes each result in order, which keeps the bytes
identical to a serial write. The reader splits the same way: chunk bytes come
through one shared file handle serially, and decompression and decoding run
across columns. That is what closed most of the read gap. The work that remains
is flate, and flate's cost is not symmetric: a column of pseudo-random float64
values compresses 3.80x at level 6 in 109ms, and 3.75x at level 3 in 28ms. The
standard library ships no zstd, so there is no faster codec to reach for, only a
slower setting to stop using.

Parquet also pays nothing to choose a layout: its encodings are compiled in.
keine measures them per column, which costs about 40ms of this write and is why
it can be smaller than a format with a better compressor.

Reading all five columns is still behind, and the profile says why: flate
decompression is 43% of that read and GC is another 25%. The decoding itself is
no longer the cost it was — boxing every value into an interface used to
allocate a copy of each one, and a column now shares one backing array instead,
so a 10000-value int64 column reads with 10 allocations rather than 10010 and
takes about half the time. Strings got the same treatment: a dictionary column
converts each distinct entry once and shares those headers across every value
that repeats it, and a plain string column's values are slices of one buffer
rather than a copy each. That took the comparison file's read from 134ms to 35ms
and its allocations from 403000 to 3500 while the output bytes stayed identical.