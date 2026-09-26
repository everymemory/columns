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
format version     1 byte
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
block count         uint32
block raw lengths   uint32 per block
block lengths       uint32 per block
null bitmap
block 0
block 1
...
```

The footer holds the schema and the metadata for every row group: each column's
on-disk byte length, null count, min and max value, and value length bounds. It
is encoded with `encoding/gob`. The footer comes last and carries its own length,
so a reader opens a file with one seek to the end and then jumps straight to any
column of any row group.

A column's encoded bytes are split into blocks of at most 256KB and each block is
compressed on its own, so decompression is work that can run in parallel within a
column rather than one serial stream per column. Blocks are byte ranges of the
encoded stream rather than ranges of values, so concatenating the decompressed
blocks gives the encoder's exact output and no encoder or decoder has to know the
split happened. A column shorter than a block stays a single block.

The chunk carries its total decompressed length, and each block its own, so a
reader sizes its buffer once and each block lands in its own region of it rather
than being appended into place. That field is redundant with the footer's byte
length only when the codec is none.

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

When the same reader scans the same columns over and over, `ReadRowGroupScoped`
hands the values to a callback as typed slices and takes them back when it
returns. Nothing is boxed, and the slices point into buffers the reader keeps
for its next read, so after the first one a row group costs almost no
allocation:

```go
r.ReadRowGroupScoped(0, []int{0}, func(c *keine.Columns) error {
    ids, _ := keine.Column[int64](c, 0)
    // ids is valid until this returns
    return nil
})
```

Holding a slice past the callback reads whatever the next read wrote over it, so
it is a borrowing API: copy out what you need before returning. A nullable
column is an error here for the same reason it is in `ReadColumn`.

## Status

v0.2.0. The format is stable enough to write and read real data, but it is not a
production storage engine. There is no schema evolution, no concurrency control,
and no way to append to an existing file. Round-trip tests cover every type,
encoding and codec, columns long enough to span several blocks, and the layout
experiment is exercised against columns chosen to favour each one. The writer is
deterministic: two writes of the same values produce the same bytes, which a
golden file test pins.

The byte after the leading magic is the format version, currently 1. A reader that
meets another version refuses the file rather than misreading it, so a layout
change is a clean break. Files written by v0.1.0 have no version byte and will be
refused.

## Benchmarks

```
go test -bench=. ./...
```

`BenchmarkComparison` writes and reads one row group of five columns at 200000
rows and reports the file size and both directions of the transfer. Measured on a
Xeon X5687 with `go test -bench=BenchmarkComparison -count=3`:

```
BenchmarkComparison
    15.07 bytes/row
    write        69 ms   42 MB/s   62 MB    57568 allocs
    read         25 ms   121 MB/s  32.7 MB    900 allocs
    read scoped  12 ms   256 MB/s   137 KB    853 allocs
```

The scoped read returns typed slices into the reader's own buffers, so the
memory it reports is transient: only the first read allocates the columns. The
boxed read hands every value to the caller, and 16 bytes of that per value is
the interface header itself, which is the floor for a `[]any` return.

`BenchmarkExperiment` is the layout choice cost the writer pays per column.
`BenchmarkSizes` reports every candidate's size, since the winner is only
meaningful next to what it beat. `BenchmarkWriter` and `BenchmarkRead` cover the
whole write and read paths, `BenchmarkTypedRead` the unboxed read path, and
`BenchmarkPartialRead` the cost of skipping columns.

## Compared with parquet

Measured against pyarrow 25.0.1, both formats fed the *same* values: the five
columns `BenchmarkComparison` builds were dumped to raw files and handed to
pyarrow, so neither saw easier data and the 15.07 bytes/row is the same file in
both rows of the table. The harness is not in this repository — it needs a
Python install, and the library does not — but it is a handful of lines around
`pq.write_table` and `pq.ParquetFile.read`, and both sides were timed best of
five on the same idle X5687.

On 200000 rows in 5 columns:

| | bytes/row | write | read all | read 1 column |
| --- | --- | --- | --- | --- |
| keine | 15.07 | 69 ms | 12 ms (scoped) | 1.9 ms |
| parquet zstd | 16.04 | 121 ms | 15 ms | 4.2 ms |
| parquet snappy | 24.33 | 111 ms | 16 ms | 4.2 ms |
| parquet none | 51.08 | 92–192 ms | 15 ms | 2.3 ms |

`read all` for parquet is `ParquetFile.read`, which returns an Arrow table of
typed columnar buffers and never boxes a value. The fair keine counterpart is
the scoped read, which hands back typed slices the same way; that is the 12 ms
above, and it is faster than parquet's 15. The boxed read is a different
contract — it returns owned values in interfaces, which costs 16 bytes a value
before any decoding, and no `[]any` return can go below that. It measures about
23 ms and is not in the table, because it is not the same operation parquet is
doing.

keine is now smaller than parquet at every compression level, faster to write at
every one of them, and faster reading a single column by more than two times.
Parquet's `none` write time was unstable across runs, between 92 and 192 ms, and
is reported as a range rather than picked from; nothing else in the table moved
by more than a few percent between runs.

Where parquet still leads is not in the numbers above. It has nested types, a
stable ecosystem, and readers in every language. keine has none of that, and the
read gap this table used to show is closed only because the scoped read now
exists to compare against Arrow's buffers rather than against a boxed `[]any`.

The table is synthetic data, which is worth checking against the real thing. One
monthly shard of `open-index/hacker-news` on HF — 255218 rows — was fed through
both formats, taking the thirteen scalar columns and leaving the three list ones
out, since keine has no nested type. This is the subset the format can express
rather than a claim about the table:

| | bytes/row | write | read all |
| --- | --- | --- | --- |
| keine | 158.98 | 953 ms | 211 ms (scoped) |
| parquet zstd | 166.74 | 703 ms | 235 ms |
| parquet snappy | 236.64 | 514 ms | 296 ms |
| parquet none | 380.95 | 253 ms | 111 ms |

Real data inverts the write conclusion. Synthetic data is mostly integers, and
a text column of HTML comment bodies is most of this file: 86 MB of the 108 MB
encoded, which flate turns into 36 MB. keine is still smaller than parquet's zstd
by five percent and still reads a little faster, but it writes 35% slower,
because DEFLATE at level 3 is what it has and zstd is what parquet has. Parquet's
uncompressed file is the fastest both ways and 2.4 times the size, which is the
trade that is actually being bought.

The shard also found a bug the benchmark could not. Compressing a column's blocks
used to be a serial loop inside one per-column goroutine, so that 330-block text
column ran on a single core while fifteen sat idle, and the README's claim that
the writer parallelises across blocks was not what the code did. Blocks now share
one semaphore across the whole write, the way the reader already did, which took
this shard's write from 2312 ms to 953 ms without changing a byte of the file.

## Where the time goes

Encoding and compressing a column touches no other, so the writer runs them
across blocks and columns at once and writes each result in order, which keeps the
bytes identical to a serial write. The reader splits the same way: chunk bytes
come through one shared file handle serially, and each block's decompression and
each column's decoding run across cores. A five-column read on a 16-core machine
used to occupy five cores; the same read now fans out to about twenty blocks and
fills the machine, which took the comparison file's read from 34ms to 27ms.

What is left of the write gap is `compress/flate`. The standard library ships no
zstd, so there is no faster codec to reach for, only a slower setting to stop
using. DEFLATE's cost is not symmetric in what it is given, so the level is where
the time lives. On this dataset's columns, measured at every level: the
pseudo-random float64 column compresses 2.81x at level 6 in 200ms and 2.69x at
level 3 in 35ms, and the near-distinct string column compresses 5.62x in 82ms and
5.44x in 43ms. The last three levels buy four hundredths of a ratio for six times
the time. Writing at level 3 costs about nine percent of the file size and halves
the write.

Parquet pays nothing to choose a layout: its encodings are compiled in. keine
measures them per column, on a sample, and that measurement is inside the write
time above, about 26ms of the 69ms, against 35ms of compressing the blocks. It is
what buys the size advantage over a format with a better compressor.

A write's allocations were the same machinery, twice over. Each compressor owns a
hash table and a sliding window, and the experiment compresses a column once per
codec while the writer compresses each of its blocks, so one write built several
hundred of them; they are now pooled and reset, which is what Reset is for, and
the buffer a measurement landed in belongs to its column for the whole write
rather than being grown per candidate. That took the comparison file's write from
108 MB to 61 MB. What is left of it is the work itself: the dictionary encoder
keying a value through `fmt` once per distinct entry, the canonical copy of the
caller's `[]any`, and the compressed bytes the file is made of, which have to be
somewhere until they are written.

The read profile after the block split was roughly a third DEFLATE, a fifth GC and
a seventh decoding. The decoding is no longer the cost it was: boxing every value
into an interface used to allocate a copy of each one, and a column now shares one
backing array instead, so a 10000-value int64 column reads with 10 allocations
rather than 10010. Strings got the same treatment, and that change alone took the
comparison file's read from 134ms to 32ms and its allocations from 403000 to 3500.

The remaining allocations were the decoders' own result slices and the DEFLATE
machinery behind them, which is what the scoped path now holds onto: each decoder
writes into a destination the reader keeps, and each block's decompressor is reset
rather than built. A scoped read of the comparison file allocates 137 KB, of which
nothing is the columns. A boxed read still allocates its values, because those are
the caller's to keep; 16 bytes a value of that is the interface header, which is
the floor a `[]any` return cannot go below.

The block split costs about two percent of the file size, since each of a column's
DEFLATE streams carries its own Huffman table instead of one for the column, and
took the comparison file's read from 34ms to 27ms. Reusing the buffers has since
taken it to 25ms. Larger blocks measured a slightly smaller file and no faster a
read: at five columns the reader runs out of work before it runs out of cores.

The remainder is DEFLATE itself, and it costs unevenly. The same 1.6MB decompresses
in 0.43ms for a column of near-monotonic deltas and in 10ms for a column of
low-entropy floats, a 25x spread from entropy alone, independent of buffer
management. That is why the comparison file's slow columns dominate: 8 of its 15
bytes per row are a pseudo-random int64 column no encoding and no codec shrinks,
and removing the float column entirely would save about seven percent of read time.
Closing the rest is a codec question, not a parallelism one.

With the allocations gone, DEFLATE is what a scoped read is: about 60% of its 12ms.
The remainder is decoding and the parallel machinery around it.

That 12ms is what parquet's typed read takes 15ms to do on the same values, which
is the point the codec argument reaches: keine is slower at decompressing and
still finishes first, because it decompresses less and does it across more cores.
The gap that remains is not a codec gap but a contract one — the boxed read below
costs its interface headers, and no `[]any` return can avoid them.

The three read paths are for three callers. `ReadRowGroup` returns `[]any` and
covers nulls, and its cost is the interface headers as much as the bytes.
`ReadColumn` reads one column into a typed slice and skips them. `ReadRowGroupScoped`
reads several, takes the values back when it is done, and is the one that gets
close to allocating nothing: it is what a scan over the same schema wants, and the
one to reach for when the read is the workload rather than the values it returns.
