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
lose the size contest to flate on every chunk. They are included because the
format defines them, but a writer that wants to skip the wasted experiment time
can drop them from `layoutCodecs` in `layout.go`.

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
whole write and read paths, and `BenchmarkPartialRead` covers skipping columns.
