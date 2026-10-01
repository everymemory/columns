# columns

`columns` is a columnar file format library for Go, built by
[everymemory](https://github.com/everymemory). It has no dependencies outside the
standard library.

A file is a sequence of row groups, each storing its columns as separate chunks, so a
reader only decodes the columns a query asks for. Every chunk records the encoding and
compression codec it was written with; the writer picks both from the column's type, or
from a measurement of its values if it is asked to.

## File layout

```
"ABCD"
format version     1 byte
tokenizer count    uint32, little-endian, version 2 only
tokenizer digests  32 bytes each, version 2 only
row group 0
    column chunk
    column chunk
    ...
row group 1
    ...
footer
footer length      uint32, little-endian
"ABCD"
```

A column chunk is:

```
encoding            1 byte
compress            1 byte
null bitmap length  uint32
raw data length     uint32
data length         uint32
block count         uint32
block raw lengths   uint32 per block
block lengths       uint32 per block
null bitmap
block 0
block 1
...
```

The footer holds the schema and per-column metadata — byte length, null count, min and
max, value length bounds — encoded with `encoding/gob`. It comes last and carries its
own length, so a reader opens a file with one seek to the end and jumps straight to any
column of any row group.

Encoded bytes are split into blocks of at most 256KB, each compressed on its own, so
decompression runs in parallel within a column. Blocks are byte ranges of the encoded
stream, not ranges of values, so concatenating them gives the encoder's exact output and
no encoder or decoder has to know the split happened.

## Types and encodings

Thirteen type tags: bool, int8-64, uint8-64, float32, float64, bytes, string.

| Encoding | Layout |
| --- | --- |
| Plain | Fixed width values back to back, little-endian. Strings and bytes get a uint32 length prefix. |
| RLEBitpack | One bit per value. Used for booleans. |
| Delta | First value verbatim, then differences from the previous value, as int64. |
| Dict | Bit-packed indices into a dictionary of distinct values. |
| OffsetBytes | An offset table over concatenated raw bytes. For strings and byte slices. |
| Affix | The prefix and suffix every value shares, then length-prefixed middles. For strings and byte slices. |
| Tokenized | A text column stored as the ids a tokenizer maps its values to, plus the boundaries that separate them. |

Affix is what a high cardinality string column from one domain lands on — email
addresses, file paths, URLs — storing the shared part once. The prefix and suffix may
meet in the column's shortest value but must not overlap in it.

Dictionary entries are the encoded form of a value, not its text, so a float column can
be a dictionary without a formatting round trip. `DecodeDict` returns `[][]byte`, and the
typed read path narrows from there.

Tokenized records the vocabulary once rather than spelling it out per occurrence. Its ids
are little-endian `uint16`, and three representations of them are measured against three
ways of recording value boundaries — nine layouts, settled by size like every other
encoding. The ids cannot be read back without the tokenizer that made them, so a file
names it by the SHA-256 of its `tokenizer.json` and a reader refuses a hash it cannot
resolve. Bytes the tokenizer's own pipeline would drop are carried in an escape stream
alongside the ids, so a column of arbitrary bytes reads back exactly as written.

Nulls are handled before any of these apply: a nullable column carries a bitmap with one
bit per row, only the non-null values are encoded, and the reader expands them back out.

## Compression

flate, gzip, zlib and lzw are wired up. `CompressNone` stores data uncompressed.
`CompressZstd` is defined but unimplemented — this toolchain has no `compress/zstd` — and
returns an error rather than silently writing uncompressed data.

Compression is applied after encoding, and a layout is the pair of them, so what is
measured is the combination. On 5000 rows of synthetic data:

| Column | Plain | Chosen | Bytes |
| --- | --- | --- | --- |
| boolean with runs | 5000 | RLEBitpack+Flate | 21 |
| arithmetic int64 | 40000 | Delta+Flate | 82 |
| 200 distinct strings | 137250 | Dict+Flate | 1397 |
| 200 shared-domain strings | 4490 | Affix+Flate | 384 |

gzip and zlib wrap the same DEFLATE algorithm as flate but add framing, so they lose the
size contest on every chunk. They remain implemented and a file written with them reads
back fine, but the writer does not consider them: measuring them costs two passes per
column and cannot pay for itself.

## Usage

```go
schema := []columns.ColumnSchema{
    {Name: "id", Type: columns.TypeInt64},
    {Name: "email", Type: columns.TypeString, Nullable: true},
}

var buf bytes.Buffer
w := columns.NewWriter(&buf, schema)
w.AddRowGroup([][]any{
    {int64(1), int64(2), int64(3)},
    {"a@example.com", nil, "c@example.com"},
})
w.Close()

r, _ := columns.NewReader(bytes.NewReader(buf.Bytes()))
cols, _ := r.ReadRowGroup(0, []int{1}) // email only; id is never read
```

`AddRowGroup` takes `[][]any` with one slice per column, all the same length, and coerces
values to the schema's Go types. `ReadRowGroup` returns values typed to match what was
written; nulls come back as `nil`.

Scanning the data first chooses the layout from the values instead of the type:

```go
w := columns.NewWriterWithOptions(&buf, schema, columns.Options{
    Compress:      columns.CompressFlate,
    CompressLevel: 6,
})
w.Optimize([][]any{
    {int64(1), int64(2), int64(3)},
    {"a@example.com", nil, "c@example.com"},
})
w.AddRowGroup([][]any{ /* the same or later values */ })
```

`Optimize` decides for every row group after it, so it goes before the first
`AddRowGroup`. It encodes each column with every candidate encoding, compresses each with
every codec, and keeps the smallest — the tiebreak is size alone, never measured time, so
the same columns write the same file on any machine. `NewWriterWithOptions` is also how an
unoptimized writer is told what to do: `Encoding` zero means the type's own choice, and
any other encoding is used for every column it can serve. An encoding that cannot serve a
column's type fails the write rather than being substituted.

That thoroughness is the cost. On the five-column, 200000-row comparison file below,
`Optimize` takes the write from 33 ms to 7.1 s and the file from 15.56 to 14.55 bytes per
row, because it measures at the codec's best level where a plain write stays at level 3.
`OptimizeLevel` buys back part of the pass time and also changes what it picks, since a
layout that wins once flate is squeezing hard can lose to `Plain` when it is not.

A column with no nulls can be read into a typed slice, which avoids boxing every value:

```go
ids, _ := columns.ReadColumn[int64](r, 0, 0)
```

`T` is the Go type for the column — `int64` for `TypeInt64`, `string` for `TypeString`. A
nullable column has nowhere to put a `nil` in a `[]T`, so it returns an error.

The write side has the same entry. `AddRowGroupTyped` takes `[]any` where each element is
the typed slice for its column:

```go
w.AddRowGroupTyped([]any{ids, names})
```

The file is the one `AddRowGroup` would have written over the same values, and nothing the
writer keeps points into a caller's slice, so the slices can be reused as soon as the call
returns. A nullable column is an error, and a slice whose element type is not the schema's
is an error rather than a widening conversion — `AddRowGroup` coerces, because `[]any` has
not said what the values are; this one has. `OptimizeTyped` is the same pair for the layout
pass.

A reader that scans the same columns repeatedly can borrow them instead:

```go
r.ReadRowGroupScoped(0, []int{0}, func(c *columns.Columns) error {
    ids, _ := columns.Column[int64](c, 0)
    // ids is valid until this returns
    return nil
})
```

The slices point into buffers the reader keeps for its next read, so after the first one a
row group costs almost no allocation. Copy out what you need before returning. These three
read paths are for three callers: `ReadRowGroup` returns `[]any` and covers nulls,
`ReadColumn` reads one column unboxed, and `ReadRowGroupScoped` is the one that gets close
to allocating nothing.

A text column can also be stored as tokenizer ids, the one encoding that buys something the
type cannot tell you:

```go
tok, err := tokenizer.LoadFile("falcon-tokenizer.json")
if err != nil {
    return err
}
if err := tokenizer.Register(tok); err != nil {
    return err
}

w := columns.NewWriterWithOptions(&buf, schema, columns.Options{
    Tokenizers: map[int]*tokenizer.Model{1: tok},
})
```

The map is keyed by column index. A column in it has `Tokenized` among its candidates; a
column not in it is laid out the way its type implies. The model is registered because the
reader has to find it again — a file names the tokenizer by the hash of the bytes it was
built from, and a hash not in the registry is refused with the hash named so the caller can
load what the file needs. `LoadFileRegistered` is the same loader with a hash the caller
already knows, which fails if the file on disk is not the one expected.

## Status

v0.3.1. The format is stable enough to write and read real data, but it is not a production
storage engine. There is no schema evolution, no concurrency control, and no way to append
to an existing file. Round-trip tests cover every type, encoding and codec, columns long
enough to span several blocks, the default layouts, and `Optimize` on columns chosen to
favour each encoding it can pick. The writer is deterministic: two writes of the same values
produce the same bytes, which a golden file test pins.

The byte after the leading magic is the format version. A reader that meets another version
refuses the file rather than misreading it, so a layout change is a clean break. Files
written by v0.1.0 have no version byte and will be refused. This build reads versions 1 and
2, and writes 2 only when a column is tokenized.

A file begins with the magic `ABCD`. v0.3.0 and v0.3.1 wrote `COLS`, and v0.1.0 wrote
`KEIN`; a reader from this build refuses both. The layout beyond the magic is unchanged in
either case, and the version byte still names it, so the break is the magic alone.

## Benchmarks

```
go test -bench=. ./...
```

`BenchmarkComparison` writes and reads one row group of five columns at 200000 rows.
Measured on a Xeon X5687 with `go test -bench=BenchmarkComparison -count=3`:

```
BenchmarkComparison
    write             15.56 bytes/row
    write             33 ms    94 MB/s   45 MB     720 allocs
    write typed       25 ms   125 MB/s   33 MB     690 allocs
    write optimized   14.55 bytes/row
    write optimized   7.1 s    0.4 MB/s  1.2 GB    4.4M allocs
    read              24 ms   128 MB/s   37 MB     735 allocs
    read scoped       10 ms   310 MB/s   266 KB    691 allocs
```

The three write rows are the same values; the typed write produces byte-identical output, so
the gap between the first two is what `AddRowGroup` spends canonicalising. Optimize costs
1.01 bytes a row, about six percent of the file, and its time is almost entirely the
encodings that do not win. The scoped read's memory is transient — only the first read
allocates the columns — while the boxed read carries 16 bytes of interface header per value,
the floor for a `[]any` return.

`BenchmarkTokenized` is the same shape with a text column of prose and a tokenizer on it,
at 20000 rows:

```
BenchmarkTokenized
    write                    5.9 ms          130.5 B/row
    write tokenized          1.5 s           79.67 B/row
    write optimized          7.1 s           27.47 B/row
    read scoped              29 ms    54 MB/s
```

The tokenized write is 250 times slower than the plain one and all of that is the tokenizer;
the read is 54 MB/s rather than a plain string column's rate because the ids have to be
turned back into text. `BenchmarkOptimize` reports the pass cost per column and
`BenchmarkSizes` reports every candidate's size, since a winner is only meaningful next to
what it beat.

## Compared with parquet

Measured against pyarrow 25.0.1, both formats fed the *same* values. The harness is not in
this repository — it needs a Python install and the library does not — and both sides were
timed best of five on the same idle X5687.

On 200000 rows in 5 columns:

| | bytes/row | write | read all | read 1 column |
| --- | --- | --- | --- | --- |
| columns | 15.56 | 33 ms | 9.5 ms (scoped) | 0.8 ms |
| parquet zstd | 19.57 | 114 ms | 14 ms | 5.4 ms |
| parquet snappy | 28.66 | 98 ms | 15 ms | 5.9 ms |
| parquet none | 54.98 | 96 ms | 15 ms | 2.3 ms |

`read all` for parquet is `ParquetFile.read`, which returns typed Arrow buffers and never
boxes a value; the fair columns counterpart is the scoped read. The boxed read measures
about 24 ms and is not in the table, because it is not the same operation parquet is doing.
The columns row is the default write; an `Optimize`d write of the same values is 14.55
bytes/row, smaller than every parquet row here, and costs 7.1 s rather than 33 ms.

columns is smaller than parquet at every compression level, three times faster to write at
every one of them, and four times faster reading a single column. Turning pyarrow's default
zstd up does not close the size gap either: level 19 writes 15.80 bytes per row against
columns' 15.56 and costs 3.1 s to do it. Where parquet still leads is not in these numbers —
it has nested types, a stable ecosystem, and readers in every language.

The table is synthetic data, so the real thing is worth a look. One monthly shard of
`open-index/hacker-news` on HF, 255218 rows, thirteen scalar columns:

| | bytes/row | write | read all |
| --- | --- | --- | --- |
| columns | 160.23 | 310 ms | 140 ms (scoped) |
| parquet zstd | 166.74 | 936 ms | 242 ms |
| parquet snappy | 236.64 | 742 ms | 306 ms |
| parquet none | 380.95 | 861 ms | 114 ms |

Real data keeps the write and read conclusions but not the size one. The file is mostly one
text column of HTML comment bodies, 86 MB of the 108 MB encoded, which flate turns into
36 MB. columns writes three times faster than parquet's default and reads faster scoped at
every zstd level, but from level 3 up parquet is the smaller file:

| | bytes/row | write | read all |
| --- | --- | --- | --- |
| columns | 160.23 | 310 ms | 140 ms (scoped) |
| parquet zstd 1 | 166.74 | 885 ms | 223 ms |
| parquet zstd 3 | 144.77 | 1314 ms | 278 ms |
| parquet zstd 6 | 137.07 | 2950 ms | 273 ms |
| parquet zstd 9 | 134.11 | 5787 ms | 269 ms |
| parquet zstd 19 | 122.31 | 39389 ms | 258 ms |

That is not a gap an encoding closes, or so it seems until the obvious candidate is
measured. A shared phrase dictionary over the column is the one to try, and it does not
work: 87 percent of the column's 16-byte substrings occur once, and the sixty-five thousand
most frequent cover 8 percent of its bytes. The gap is the entropy coder, not the match
finder — stdlib Go has no zstd, and on data this close to random the better coder wins.

Tokenizing the same column is the one encoding that changes what the codec sees rather than
how it looks for repetition, and on this shard it does close the gap:

| | bytes/row | pass | write | read all |
| --- | --- | --- | --- | --- |
| columns flate 3 | 160.23 | — | 315 ms | 144 ms (scoped) |
| columns flate 3, text tokenized | 129.52 | — | 49.0 s | 1.17 s (scoped) |
| columns flate 9 | 147.43 | — | 1.51 s | 133 ms (scoped) |
| columns flate 9, text tokenized | 121.33 | — | 49.6 s | 1.08 s (scoped) |
| columns flate 9, optimized | 144.75 | 32.7 s | 1.06 s | 153 ms (scoped) |
| columns flate 9, optimized, text tokenized | 108.90 | 166 s | 51.9 s | 1.26 s (scoped) |
| columns flate 3, optimized | 157.05 | 18.7 s | 315 ms | 140 ms (scoped) |
| columns flate 3, optimized, text tokenized | 113.06 | 136 s | 52.3 s | 1.29 s (scoped) |
| parquet zstd 19 | 122.31 | — | 39.4 s | 258 ms |

Tokenized at flate 9 is smaller than every parquet row in the table, including the one that
took 39 seconds to write. Both optimized tokenized rows beat it too: the level 3 pass lands
at 113.06, seven and a half percent under parquet's best, and the level 9 pass at 108.90,
eleven percent. Where the phrase dictionary failed, the tokenizer's own vocabulary succeeds
— it is 65024 entries chosen for English rather than for this column, so the ids come from a
narrow alphabet while the text they spell does not.

The size is one column of the table and the other three are its price. The write is thirty
to over a hundred and fifty times slower and all of it is the BPE; the read is eight times
slower because the ids have to be reassembled into text. Neither is a defect in the
encoding; both are the tokenizer's own cost.

`Optimize` at its default recovers far less than that table makes it look, because the pass
writes at flate 9 and the row above it is a level 3 file. Held against the plain flate 9
write it is 144.75 against 147.43, just under two percent; most of the improvement over the
level 3 default is the level, not the pass. What the pass itself buys is visible per column:
`by`, whose values cluster by author, becomes a dictionary; `title`, which shares a site
prefix and suffix, becomes affix; `parent`, a mostly-sparse id column, stops paying for
subtraction on values it does not have. The other nine columns keep the layout their type
gave them. The pass is right about which columns it changes and expensive for how little it
changes — 144.75 bytes per row is parquet's zstd 3 to within a byte, which parquet reaches
in 1.3 s rather than the pass's 33.

Asking the pass for level 3 makes it cheaper and worse: it costs 18.7 s rather than 32.7 and
picks `Plain` for the text column, landing at 157.05, two percent better than not running a
pass at all. Which encoding wins depends on the level it was measured at, which is why the
default is the codec's best.

The shard also found two bugs the benchmark could not. Compressing a column's blocks used to
be a serial loop inside one per-column goroutine, so a 330-block text column ran on a single
core while fifteen sat idle; blocks now share one semaphore across the whole write, which
took this shard's write from 2312 ms to 953 ms without changing a byte of the file. And a
plain string column used to walk its length prefixes twice, once to size the result and once
to place the values, when the row group already records the value count; passing the count
through took the shard's scoped read from 161 ms to 140 ms.

## The tokenizer's own cost

The tokenizer is where the tokenized rows above spend their time, so it is measured against
the implementation it was written to match: the Rust `tokenizers` crate, at the version the
Python wheel is built from, as a standalone encoder over the same corpus and the same
`tokenizer.json`. That crate is a development oracle, not a dependency — this package has no
Hugging Face code in it at runtime.

Encoded on 50000 of the shard's comment bodies, 18.9 MB of text, over eight passes on this
side and three on the reference:

| | MB/s | ktok/s | peak memory |
| --- | --- | --- | --- |
| this package | 1.7 | 455 | 47 MB heap |
| rust reference | 1.1 | 282 | 55 MB RSS |

The Go side is not slower than the reference, which is the thing to check rather than assume.
It allocates heavily to do it — roughly 730 allocations and 80 KB per record against a 378
byte average — but throughput and memory are the same order, so the encoding's cost above is
what tokenization costs in any implementation rather than in this one.

## License

CC0 1.0 Universal. Everything in this repository is released to the public domain: no rights
reserved, no attribution required, and no restriction on commercial use. See
[LICENSE](LICENSE) for the full text, which also carries a fallback license for the
jurisdictions where a public domain dedication is not recognized.
