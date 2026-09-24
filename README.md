# keine

keine is a columnar file format library for Go with no dependencies outside the
standard library.

A file is a sequence of row groups. Each row group stores its columns as separate
chunks, so a reader only decodes the columns a query asks for. Every column chunk
records the encoding and compression codec it was written with; the writer picks
both by measuring the alternatives on the data itself.

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
raw data length     uint32
data length         uint32
null bitmap
data
```

The footer holds the schema and the metadata for every row group: each column's
on-disk byte length, null count, min and max value, and value length bounds. It
is encoded with `encoding/gob`. The footer comes last and carries its own length,
so a reader opens a file with one seek to the end and then jumps straight to any
column of any row group.

The chunk carries its decompressed length as well as its stored length, so a
reader sizes its buffer once instead of growing it into shape. That field is
redundant with the footer's byte length only when the codec is none.

## Types and encodings

Thirteen type tags: bool, int8-64, uint8-64, float32, float64, bytes, string.

Six encodings:

| Encoding | Layout |
| --- | --- |
| Plain | Fixed width values back to back, little-endian. Strings and bytes get a uint32 length prefix. |
| RLEBitpack | One bit per value. Used for booleans. |
| Delta | First value verbatim, then differences from the previous value, as int64. |
| Dict | Bit-packed indices into a dictionary of distinct values. |
| OffsetBytes | An offset table over concatenated raw bytes. For strings and byte slices. |
| Affix | The prefix and suffix every value shares, then length-prefixed middles. For strings and byte slices. |

Affix is what a high cardinality string column lands on when its values come from
one domain: email addresses, file paths, URLs. The shared part is stored once
rather than per value, and what is left is that much shorter for the codec that
follows. The prefix and suffix may meet in the column's shortest value but must
not overlap in it, so a value is always its prefix, middle and suffix back to
back.

Dictionary entries are the encoded form of a value, the little-endian bytes for a
fixed width type and the raw bytes for a string or byte slice, rather than its
text. A float column can then be a dictionary without a formatting round trip, and
a dict column decodes as one plain read of the declared type. `DecodeDict`
returns `[][]byte`, and the typed read path narrows from there.

Nulls are handled before any of these apply. A nullable column's chunk carries a
bitmap with one bit per row; only the non-null values are encoded, and the reader
expands them back out.

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
| 200 shared-domain strings | 4490 | Affix+Flate | 384 |

Gzip and zlib wrap the same DEFLATE algorithm as flate but add framing, so they
lose the size contest to flate on every chunk. They remain implemented and tagged,
and a file written with them reads back fine, but the writer does not consider
them: measuring them costs two encode and compress passes per column and cannot
pay for itself.

## Choosing a layout

`ExperimentLayouts` is what the writer calls per column. Encoding and compression
are measured against each other on the actual values, so a sorted id column lands
on Delta and a low-cardinality string column lands on Dict without anyone
hard-coding that.

Which layout wins depends on the shape of a column, its cardinality, whether it is
monotonic, how long its values are, and not on how many rows it has. The
experiment measures a bounded, evenly spaced sample rather than every value, so
its cost stops scaling with the file. On a 200000-row, 5-column dataset the full
experiment took 9.7s and the sampled one 0.21s, and every column chose the same
encoding and codec, producing a byte-identical file.

The sample is not infallible. A column of 200000 floats with 10000 distinct
values looks like 8192 distinct values to an 8192-row sample, so dictionary
encoding looks useless and plain wins on the sample. Measured on the whole
column, dictionary would have taken the column from 446KB to 80KB. A full
cardinality pass over every column costs more than the 0.15 bytes/row it would
save, so the sample stands and the floor is known rather than chased.

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

`T` is the Go type for the column, `int64` for `TypeInt64`, `string` for
`TypeString`. A column with nulls has nowhere to put a `nil` in a `[]T`, so it
returns an error and `ReadRowGroup` reads those.

## Status

v0.1.0. The format is stable enough to write and read real data, but it is not a
production storage engine. There is no schema evolution, no concurrency control,
and no way to append to an existing file. Round-trip tests cover every type,
encoding and codec, and the layout experiment is exercised against columns chosen
to favour each one. The writer is deterministic: two writes of the same values
produce the same bytes, which a golden file test pins.

## Benchmarks

```
go test -bench=. ./...
```

`BenchmarkComparison` writes and reads one row group of five columns at 200000
rows and reports the file size and both directions of the transfer. Measured on a
Xeon X5687 with `go test -bench=BenchmarkComparison -count=3`:

```
BenchmarkComparison
    14.75 bytes/row
    write    110 ms   27 MB/s
    read      35 ms   83 MB/s
```

`BenchmarkExperiment` is the layout choice cost the writer pays per column.
`BenchmarkSizes` reports every candidate's size, since the winner is only
meaningful next to what it beat. `BenchmarkWriter` and `BenchmarkRead` cover the
whole write and read paths, `BenchmarkTypedRead` the unboxed read path, and
`BenchmarkPartialRead` the cost of skipping columns.

## Compared with parquet

The comparison below was taken once, against pyarrow, with both formats fed the
same values from the same pseudo-random stream so neither saw easier data. The
harness and the dataset are not in this repository, and the dataset is not the one
`BenchmarkComparison` uses, so the 9.07 bytes/row there and the 14.75 above are
two different files. Treat the table as a one-time measurement, not something to
reproduce here.

On 200000 rows in 5 columns:

| | bytes/row | write | read all | read 1 column | read typed |
| --- | --- | --- | --- | --- | --- |
| keine | 9.07 | 132ms | 32ms | 10ms | 32ms |
| parquet zstd | 12.86 | 104ms | 16ms | 9ms | — |
| parquet snappy | 21.32 | 94ms | 16ms | 8ms | — |
| parquet none | 43.71 | 80ms | 14ms | 8ms | — |

keine is smaller than parquet at every compression level and within about a
quarter of its write speed, and reading one column is level with it. Reading all
five is where it still loses.

## Where the time goes

Encoding and compressing a column touches no other, so the writer runs them
across columns at once and writes each result in order, which keeps the bytes
identical to a serial write. The reader splits the same way: chunk bytes come
through one shared file handle serially, and decompression and decoding run
across columns. That is what closed most of the read gap.

What is left of the write gap is `compress/flate`. The standard library ships no
zstd, so there is no faster codec to reach for, only a slower setting to stop
using. DEFLATE's cost is not symmetric in what it is given, so the level is where
the time lives. On this dataset's columns, measured at every level: the
pseudo-random float64 column compresses 2.81x at level 6 in 200ms and 2.69x at
level 3 in 35ms, and the near-distinct string column compresses 5.62x in 82ms and
5.44x in 43ms. The last three levels buy four hundredths of a ratio for six times
the time. Writing at level 3 costs about nine percent of the file size and halves
the write, which is why the table above is 9.07 bytes/row rather than 9.06.

Parquet pays nothing to choose a layout: its encodings are compiled in. keine
measures them per column, on a sample, and that measurement is inside the write
time above, about 38ms of the 132ms. It is what buys the size advantage over a
format with a better compressor.

Reading all five columns is still behind, and the profile says why: flate
decompression is 43% of that read and GC is another 25%. The decoding itself is no
longer the cost it was. Boxing every value into an interface used to allocate a
copy of each one; a column now shares one backing array instead, so a 10000-value
int64 column reads with 10 allocations rather than 10010 and takes about half the
time. Strings got the same treatment: a dictionary column converts each distinct
entry once and shares those headers across every value that repeats it, and a
plain string column's values are slices of one buffer rather than a copy each.
That took the comparison file's read from 134ms to 32ms and its allocations from
403000 to 3500.

What is left is DEFLATE itself, and it costs unevenly. The same 1.6MB decompresses
in 0.43ms for a column of near-monotonic deltas and in 10ms for a column of
low-entropy floats, a 25x spread from entropy alone, independent of buffer
management. The two slow columns are the whole remaining gap, so no amount of
buffer pooling closes it; a faster codec would.

The typed read path exists for the case where the schema is known and a query
touches few columns. `ReadColumn` reads three integer columns in 32ms where the
boxed path takes longer for the same work, because it skips the interface boxing
`ReadRowGroup` pays for a uniform return type.
