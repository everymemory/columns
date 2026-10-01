# columns

`columns` is a columnar file format library for Go, built by
[everymemory](https://github.com/everymemory). It has no dependencies outside the
standard library.

A file is a sequence of row groups. Each row group stores its columns as separate
chunks, so a reader only decodes the columns a query asks for. Every chunk records
its encoding and compression codec. The writer picks both from the column's type, or
from its values when `Optimize` is called.

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

The footer holds the schema and per-column metadata: byte length, null count, min and
max, and value length bounds. It is encoded with `encoding/gob` and carries its own
length, so a reader reaches any column of any row group with one seek to the end.

Encoded bytes are split into blocks of at most 256KB, each compressed separately, so
decompression runs in parallel within a column. Blocks are byte ranges of the encoded
stream, so concatenating them reproduces the encoder's output and no encoder or decoder
has to know about the split.

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

Affix fits high-cardinality string columns from one domain: email addresses, file paths,
URLs. The shared prefix and suffix are stored once. They may meet in the column's
shortest value but must not overlap in it.

Dictionary entries hold a value's encoded form, not its text, so a float column can be a
dictionary without a formatting round trip and a dict column decodes as one plain read of
the declared type. `DecodeDict` returns `[][]byte`; the typed read path narrows from there.

Tokenized stores a text column as the ids a tokenizer maps each value to, recording the
vocabulary once instead of per occurrence. The ids are little-endian `uint16`; three
representations of them are measured against three ways of recording value boundaries,
nine layouts in all, and size settles which one the file gets. The ids cannot be read back
without the tokenizer that made them, so a file names it by the SHA-256 of its
`tokenizer.json` and a reader refuses a hash it cannot resolve. Bytes the tokenizer's
pipeline would drop are carried in an escape stream, so arbitrary bytes read back as
written.

Nulls are handled before encoding. A nullable column carries a bitmap with one bit per row;
only non-null values are encoded, and the reader expands them back out.

## Compression

flate, gzip, zlib and lzw are implemented. `CompressNone` stores data uncompressed.
`CompressZstd` is defined but unimplemented, because this toolchain has no `compress/zstd`;
using it returns an error instead of writing uncompressed data silently.

Compression runs after encoding, and a layout is the pair, so the measurement covers the
combination. On 5000 rows of synthetic data:

| Column | Plain | Chosen | Bytes |
| --- | --- | --- | --- |
| boolean with runs | 5000 | RLEBitpack+Flate | 21 |
| arithmetic int64 | 40000 | Delta+Flate | 82 |
| 200 distinct strings | 137250 | Dict+Flate | 1397 |
| 200 shared-domain strings | 4490 | Affix+Flate | 384 |

gzip and zlib wrap the same DEFLATE algorithm as flate with added framing, so they lose on
size every chunk. They stay implemented and readable, but the writer never picks them:
measuring them costs two encode-and-compress passes per column and cannot pay for itself.

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

`AddRowGroup` takes `[][]any`, one slice per column, all the same length, and coerces
values to the schema's Go types. `ReadRowGroup` returns values typed to match the schema;
nulls come back as `nil`.

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

`Optimize` decides the layout for every row group written after it, so it goes before the
first `AddRowGroup`. It encodes each column with every candidate encoding, compresses each
with every codec, and keeps the smallest. Ties break on size alone, never measured time, so
the same columns write the same file on any machine. `NewWriterWithOptions` also configures
a writer that does not run the pass: `Encoding` zero means the type's choice, and any other
encoding applies to every column it can serve. An encoding that cannot serve a column's
type fails the write instead of being substituted.

The pass is expensive. On the comparison file below, five columns at 200000 rows, `Optimize`
takes the write from 33 ms to 7.1 s and the file from 15.56 to 14.55 bytes per row, because
it measures at the codec's best level while a plain write uses level 3. `OptimizeLevel`
lowers the pass cost and changes what it picks: a layout that wins at flate level 9 can lose
to `Plain` at level 3.

A column with no nulls can be read into a typed slice, which avoids boxing every value:

```go
ids, _ := columns.ReadColumn[int64](r, 0, 0)
```

`T` is the column's Go type: `int64` for `TypeInt64`, `string` for `TypeString`. A nullable
column returns an error, because `[]T` has no slot for a `nil`.

The write side has the same entry. `AddRowGroupTyped` takes `[]any` where each element is
the typed slice for its column:

```go
w.AddRowGroupTyped([]any{ids, names})
```

The bytes are identical to what `AddRowGroup` would write over the same values, and the
writer keeps no pointer into a caller's slice, so slices can be reused as soon as the call
returns. A nullable column is an error, and a slice whose element type is not the schema's
is an error, not a widening conversion. `AddRowGroup` coerces because `[]any` has not said
what the values are; this one has. `OptimizeTyped` is the pair for the layout pass.

A reader that scans the same columns repeatedly can borrow them instead:

```go
r.ReadRowGroupScoped(0, []int{0}, func(c *columns.Columns) error {
    ids, _ := columns.Column[int64](c, 0)
    // ids is valid until this returns
    return nil
})
```

The slices point into buffers the reader reuses, so after the first read a row group
allocates almost nothing. Copy out what you need before returning. The three read paths
cover three callers: `ReadRowGroup` returns `[]any` and handles nulls, `ReadColumn` reads
one column unboxed, and `ReadRowGroupScoped` allocates the least.

A text column can also be stored as tokenizer ids, the one encoding whose benefit the
column's type does not imply:

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

The map is keyed by column index. Columns in it get `Tokenized` among their candidates; the
rest get the layout their type implies. Registration exists because the reader has to find
the model again: a file names the tokenizer by the hash of the bytes it was built from, and
an unknown hash is refused with the hash named, so the caller can load what the file needs.
`LoadFileRegistered` loads with a hash the caller already has and fails if the file on disk
does not match it.

## Status

v0.3.1. The format can write and read real data but is not a production storage engine:
no schema evolution, no concurrency control, no appending to an existing file. Round-trip
tests cover every type, encoding and codec, columns long enough to span several blocks, the
default layouts, and `Optimize` on columns chosen to favour each encoding it can pick. The
writer is deterministic: identical inputs produce identical bytes, pinned by a golden file
test.

The byte after the leading magic is the format version. A reader that meets a version it
does not know refuses the file instead of misreading it, so a layout change is a clean
break. Files from v0.1.0 have no version byte and are refused. This build reads versions 1
and 2, and writes 2 only when a column is tokenized.

A file begins with the magic `ABCD`. v0.3.0 and v0.3.1 wrote `COLS`, and v0.1.0 wrote
`KEIN`; this build refuses both. Nothing after the magic changed, so the break is the magic
alone.

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

The three write rows use the same values. The typed write produces byte-identical output,
so the gap between the first two is what `AddRowGroup` spends canonicalising. `Optimize`
costs 1.01 bytes a row, about six percent of the file, and its time goes almost entirely to
the encodings that do not win. The scoped read's memory is transient: only the first read
allocates the columns. The boxed read carries 16 bytes of interface header per value, the
floor for a `[]any` return.

`BenchmarkTokenized` is the same shape with a text column of prose and a tokenizer on it,
at 20000 rows:

```
BenchmarkTokenized
    write                    5.9 ms          130.5 B/row
    write tokenized          1.5 s           79.67 B/row
    write optimized          7.1 s           27.47 B/row
    read scoped              29 ms    54 MB/s
```

The tokenized write is 250 times slower than the plain one, all of it in the tokenizer, and
the read is 54 MB/s against a plain string column's rate because ids have to be turned back
into text. `BenchmarkOptimize` reports the pass cost per column and `BenchmarkSizes`
reports every candidate's size, since a winner means little without what it beat.

## Compared with parquet

Measured against pyarrow 25.0.1, both formats fed the same values. The harness is not in
this repository; it needs a Python install and the library does not. Both sides were timed
best of five on the same idle X5687.

On 200000 rows in 5 columns:

| | bytes/row | write | read all | read 1 column |
| --- | --- | --- | --- | --- |
| columns | 15.56 | 33 ms | 9.5 ms (scoped) | 0.8 ms |
| parquet zstd | 19.57 | 114 ms | 14 ms | 5.4 ms |
| parquet snappy | 28.66 | 98 ms | 15 ms | 5.9 ms |
| parquet none | 54.98 | 96 ms | 15 ms | 2.3 ms |

`read all` for parquet is `ParquetFile.read`, which returns typed Arrow buffers and never
boxes a value; the fair columns counterpart is the scoped read. The boxed read takes about
24 ms and is not in the table, because it is not doing what parquet is. The columns row is
the default write. An `Optimize`d write of the same values is 14.55 bytes/row, smaller than
every parquet row here, and costs 7.1 s instead of 33 ms.

columns is smaller than parquet at every compression level, three times faster to write at
every one, and four times faster reading a single column. Raising pyarrow's default zstd
does not close the size gap: level 19 writes 15.80 bytes per row against columns' 15.56 and
takes 3.1 s to do it. Where parquet leads is elsewhere: nested types, a stable ecosystem,
readers in every language.

The tables above are synthetic. Real data:

One monthly shard of `open-index/hacker-news` on HF, 255218 rows, thirteen scalar columns:

| | bytes/row | write | read all |
| --- | --- | --- | --- |
| columns | 160.23 | 310 ms | 140 ms (scoped) |
| parquet zstd | 166.74 | 936 ms | 242 ms |
| parquet snappy | 236.64 | 742 ms | 306 ms |
| parquet none | 380.95 | 861 ms | 114 ms |

Real data keeps the write and read conclusions but not the size one. The file is mostly one
text column of HTML comment bodies, 86 MB of the 108 MB encoded, which flate turns into
36 MB. columns writes three times faster than parquet's default and reads faster scoped at
every zstd level, but from level 3 up parquet is smaller:

| | bytes/row | write | read all |
| --- | --- | --- | --- |
| columns | 160.23 | 310 ms | 140 ms (scoped) |
| parquet zstd 1 | 166.74 | 885 ms | 223 ms |
| parquet zstd 3 | 144.77 | 1314 ms | 278 ms |
| parquet zstd 6 | 137.07 | 2950 ms | 273 ms |
| parquet zstd 9 | 134.11 | 5787 ms | 269 ms |
| parquet zstd 19 | 122.31 | 39389 ms | 258 ms |

A shared phrase dictionary is the obvious thing to try on comment prose, and it does not
work: 87 percent of the column's 16-byte substrings occur once, and the sixty-five thousand
most frequent cover 8 percent of its bytes. Almost nothing repeats to factor out. The gap is
the entropy coder, not the match finder; stdlib Go has no zstd, and on data this close to
random the better coder wins.

Tokenizing the same column does close the gap:

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
took 39 seconds to write. Both optimized tokenized rows beat it as well: the level 3 pass
reaches 113.06, seven and a half percent under parquet's best, and the level 9 pass 108.90,
eleven percent. The tokenizer's vocabulary succeeds where a phrase dictionary failed: it is
65024 entries chosen for English rather than for this column, so the ids come from a narrow
alphabet while the text they spell does not.

That size costs write and read time. The write is thirty to over a hundred and fifty times
slower, all of it in the BPE, and the read is eight times slower because ids have to be
reassembled into text. Neither is a defect in the encoding; both are the tokenizer's cost.

`Optimize` at its default recovers less than that table suggests, because the pass writes at
flate 9 and the row above it is a level 3 file. Against the plain flate 9 write it is 144.75
to 147.43, just under two percent; most of the gain over the level 3 default comes from the
level, not the pass. What the pass itself buys shows per column: `by`, whose values cluster
by author, becomes a dictionary; `title`, which shares a site prefix and suffix, becomes
affix; `parent`, a mostly sparse id column, stops paying for subtraction on values it does
not have. The other nine columns keep the layout their type gave them. The pass picks the
right columns and changes little: 144.75 bytes per row is within a byte of parquet's zstd 3,
which parquet reaches in 1.3 s against the pass's 33.

Setting the pass to level 3 makes it cheaper and worse. It costs 18.7 s instead of 32.7 and
picks `Plain` for the text column, landing at 157.05, two percent better than no pass at
all. Which encoding wins depends on the level it was measured at, which is why the default
is the codec's best.

The shard also found two bugs the benchmark could not. Compressing a column's blocks used to
be a serial loop inside one per-column goroutine, so a 330-block text column ran on a single
core while fifteen sat idle; blocks now share one semaphore across the whole write, which
took this shard's write from 2312 ms to 953 ms without changing a byte of the file. A plain
string column used to walk its length prefixes twice, once to size the result and once to
place the values, when the row group already records the value count; passing the count
through took the shard's scoped read from 161 ms to 140 ms.

## The tokenizer's own cost

The tokenizer is where the tokenized rows above spend their time, so it is measured against
the implementation it was written to match: the Rust `tokenizers` crate, at the version the
Python wheel is built from, run as a standalone encoder over the same corpus and the same
`tokenizer.json`. That crate is a development oracle, not a dependency; this package has no
Hugging Face code in it at runtime.

Encoded on 50000 of the shard's comment bodies, 18.9 MB of text, over eight passes on this
side and three on the reference:

| | MB/s | ktok/s | peak memory |
| --- | --- | --- | --- |
| this package | 1.7 | 455 | 47 MB heap |
| rust reference | 1.1 | 282 | 55 MB RSS |

The Go side is not slower than the reference. It allocates heavily to get there, roughly 730
allocations and 80 KB per record against a 378 byte average, but throughput and memory are
the same order, so the tokenized rows above show what tokenization costs in any
implementation, not just this one.

## License

CC0 1.0 Universal. Everything in this repository is released to the public domain: no rights
reserved, no attribution required, and no restriction on commercial use. See
[LICENSE](LICENSE) for the full text, which also carries a fallback license for the
jurisdictions where a public domain dedication is not recognized.
