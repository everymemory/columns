# keine

keine is a columnar file format library for Go with no dependencies outside the
standard library.

A file is a sequence of row groups. Each row group stores its columns as separate
chunks, so a reader only decodes the columns a query asks for. Every column chunk
records the encoding and compression codec it was written with; the writer picks
both from the column's type, or from a measurement of its values if it is asked
to.

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

Compression is applied after encoding, and a layout is the pair of them, so what
is measured is the combination. On 5000 rows of synthetic data:

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

What a column is laid out as is decided once, and there are two ways to decide it.

`NewWriter` decides from the type. Bool packs to bits, integers are stored as
deltas, and floating point, bytes and strings are stored plainly, each compressed
with Flate. These are properties of the type rather than of the values, so they
cost nothing to choose and cannot be wrong the way a guess at the data's shape
can. They are also what an empty `Options` means: the defaults are documented by
the zero value rather than beside it.

`NewWriterWithOptions` changes any of it: the encoding, the codec, its level, and
the block size. An encoding that cannot serve a column's type fails the write
rather than being substituted, and a codec this build does not have fails before
any bytes are written.

`Optimize` decides from the values. It reads every column once, encodes it with
each candidate encoding and compresses each with each codec, at the level the file
will be written at, and keeps the smallest. Call it once for a dataset and every
row group after it is stored the way the whole column earns: a column of repeated
strings becomes a dictionary, a column of same-domain strings shares its affixes,
and a column the type serves well keeps the layout it would have had anyway. The
tiebreak is size alone, never measured time, so the same columns write the same
file on any machine.

That thoroughness is the cost. On the five-column, 200000-row comparison file
below, `Optimize` takes the write from 33 ms to 7.1 s and the file from 15.56 to
14.55 bytes per row. The pass measures at the best level the codec offers, where a
plain write stays at level 3, and that choice accounts for most of both numbers: a
block compressed harder costs no more to read back, so the extra write time is the
only price. The defaults are already right for two of those five columns, and the pass
pays for the other three by a byte a row. It is the right call when the file is
written once and read many times, and the wrong one when it is not; `OptimizeLevel`
buys back part of the time without changing what the pass chooses, and nothing
cheaper than the full walk exists, because a sample that cheap is a guess the
values did not get to answer.

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

The columns above are stored the way their type implies. Scanning the data first
chooses the layout from the values instead:

```go
w := keine.NewWriterWithOptions(&buf, schema, keine.Options{
    Compress:      keine.CompressFlate,
    CompressLevel: 6,
})
w.Optimize([][]any{
    {int64(1), int64(2), int64(3)},
    {"a@example.com", nil, "c@example.com"},
})
w.AddRowGroup([][]any{ /* the same or later values */ })
```

`Optimize` is a pass over the data and decides for every row group after it, so
it goes before the first `AddRowGroup`. It measures and writes at the best level
the codec offers, because a caller who runs the pass has already decided the data
is worth walking in full and a harder block costs nothing to read back; set
`OptimizeLevel` to spend less of the pass. `Options` is also how a writer that has
not been Optimized is told what to do: `Encoding` zero means the type's own
choice, and any other encoding is used for every column it can serve.

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

The write side has the same entry. `AddRowGroupTyped` takes `[]any` where each
element is the typed slice for its column, so a caller holding `[]int64` columns
writes them without boxing a value:

```go
w.AddRowGroupTyped([]any{ids, names})
```

The slices go straight to the encoders, so the file is the one `AddRowGroup`
would have written over the same values, and nothing the writer keeps points into
a caller's slice: every encoder allocates its own output, so the slices can be
reused as soon as the call returns. The contract mirrors the read side's — a
nullable column is an error, because a typed slice has nowhere for a null, and a
slice whose element type is not the schema's is an error rather than a widening
conversion. `AddRowGroup` coerces, because `[]any` has not said what the values
are; this one has. `OptimizeTyped` is the same pair for the layout pass.

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
encoding and codec, columns long enough to span several blocks, the default
layouts, and `Optimize` on columns chosen to favour each encoding it can pick.
The writer is deterministic: two writes of the same values produce the same bytes,
which a golden file test pins.

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
    write             15.56 bytes/row
    write             33 ms    94 MB/s   45 MB     720 allocs
    write typed       25 ms   125 MB/s   33 MB     690 allocs
    write optimized   14.55 bytes/row
    write optimized   7.1 s    0.4 MB/s  1.2 GB    4.4M allocs
    read              24 ms   128 MB/s   37 MB     735 allocs
    read scoped       10 ms   310 MB/s   266 KB    691 allocs
```

The three write rows are the same values. The first stores each column the way its
type implies, the second hands the writer the typed slices a reader of the same
column would hold, and the third runs `Optimize` first. The typed write produces
byte-identical output, so the time and the memory between the first two rows is
what `AddRowGroup` spends canonicalising: each column is walked and re-homed into
a slice of the type its schema implies, and every value the caller already held
typed is boxed on the way in. Optimize costs 1.01 bytes a row, about six percent
of the file, and its time is almost entirely the encodings that do not win, the
affix table over a near-distinct string column and a dictionary over pseudo-random
integers, measured because they are candidates rather than plausible winners.

The scoped read returns typed slices into the reader's own buffers, so the
memory it reports is transient: only the first read allocates the columns. The
boxed read hands every value to the caller, and 16 bytes of that per value is
the interface header itself, which is the floor for a `[]any` return.

`BenchmarkOptimize` is the pass cost per column, and `BenchmarkSizes` reports
every candidate's size, since a winner is only meaningful next to what it beat.
`BenchmarkWriter` and `BenchmarkRead` cover the whole write and read paths,
`BenchmarkTypedRead` the unboxed read path, and `BenchmarkPartialRead` the cost
of skipping columns.

## Compared with parquet

Measured against pyarrow 25.0.1, both formats fed the *same* values: the five
columns `BenchmarkComparison` builds were dumped to raw files and handed to
pyarrow, so neither saw easier data and the bytes/row is the same file in both
rows of the table. The harness is not in this repository — it needs a Python
install, and the library does not — but it is a handful of lines around
`pq.write_table` and `pq.ParquetFile.read`, and both sides were timed best of
five on the same idle X5687.

On 200000 rows in 5 columns:

| | bytes/row | write | read all | read 1 column |
| --- | --- | --- | --- | --- |
| keine | 15.56 | 33 ms | 9.5 ms (scoped) | 0.8 ms |
| parquet zstd | 19.57 | 114 ms | 14 ms | 5.4 ms |
| parquet snappy | 28.66 | 98 ms | 15 ms | 5.9 ms |
| parquet none | 54.98 | 96 ms | 15 ms | 2.3 ms |

`read all` for parquet is `ParquetFile.read`, which returns an Arrow table of
typed columnar buffers and never boxes a value. The fair keine counterpart is
the scoped read, which hands back typed slices the same way; that is the 9.5 ms
above, and it is faster than parquet's 14. The boxed read is a different
contract: it returns owned values in interfaces, which costs 16 bytes a value
before any decoding, and no `[]any` return can go below that. It measures about
24 ms and is not in the table, because it is not the same operation parquet is
doing.

keine's row is the default write, the one that stores each column the way its
type implies. An `Optimize`d write of the same values is 14.55 bytes/row, smaller
than every parquet row here, and it costs 7.1 s rather than 33 ms: the pass reads
every value and measures every candidate at the codec's best level, and the
default is what the table reports because most callers do not want to pay that.

keine is smaller than parquet at every compression level, three times faster to
write at every one of them, and four times faster reading a single column. The
`parquet zstd` row is pyarrow's default, which is zstd level 1; turning it up on
this table does not close the size gap either, since level 19 writes 15.80 bytes
per row against keine's 15.56 and costs 3.1 s to do it. Real data does not behave
this way, and the shard below says which way.

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
| keine | 160.23 | 310 ms | 140 ms (scoped) |
| parquet zstd | 166.74 | 936 ms | 242 ms |
| parquet snappy | 236.64 | 742 ms | 306 ms |
| parquet none | 380.95 | 861 ms | 114 ms |

`parquet zstd` above is pyarrow's default, which asks zstd for level 1 — the
weakest of its levels, and the only one keine outsizes. Turning it up costs
parquet write time and buys file size, at a rate worth seeing next to the row
above:

| | bytes/row | write | read all |
| --- | --- | --- | --- |
| keine | 160.23 | 310 ms | 140 ms (scoped) |
| parquet zstd 1 | 166.74 | 885 ms | 223 ms |
| parquet zstd 3 | 144.77 | 1314 ms | 278 ms |
| parquet zstd 6 | 137.07 | 2950 ms | 273 ms |
| parquet zstd 9 | 134.11 | 5787 ms | 269 ms |
| parquet zstd 19 | 122.31 | 39389 ms | 258 ms |

Real data keeps the write and read conclusions the synthetic data reached. This
file is mostly one text column of HTML comment bodies, 86 MB of the 108 MB encoded,
which flate turns into 36 MB, and the rest is thirteen million integers and a few
short strings. keine writes three times faster than parquet's default and reads
faster scoped at every zstd level, because zstd's decompression cost barely rises
with the level while flate's is already spread across blocks. The size conclusion
does not survive the comparison: from level 3 up parquet is the smaller file, by
ten percent there and by a quarter at level 19.

That is not a gap an encoding can close, which is worth checking rather than
assuming. The obvious candidate is a shared phrase dictionary over the column —
the text is comment prose, and prose repeats its own boilerplate. Measured, it
does not: 87 percent of the column's 16-byte substrings occur once, the four
thousand most frequent ones cover 4 percent of its bytes, and the sixty-five
thousand most frequent cover 8. Almost nothing recurs to factor out, so flate's
window already holds what repetition there is, and a column-wide dictionary has
nothing left to add. The gap is the entropy coder, not the match finder: stdlib
Go has no zstd, and on data this close to random the better coder simply wins. Parquet's uncompressed file is
the fastest read of the four and 2.4 times the size, which is the other end of it.

`Optimize` on the same shard takes the write to 35.4 s and the file to 144.75
bytes per row, so the pass costs a hundred times the write and recovers ten percent
of the file. What it buys is visible per column: `by`, a string column whose values
cluster by author, becomes a dictionary; `title`, whose values share a site prefix
and a suffix, becomes affix; `parent`, a mostly-sparse id column, stops paying for
subtraction on values it does not have. The other nine columns keep the layout
their type gave them. That is the honest shape of the trade: the pass is right
about which columns it changes and ruinously expensive for how little it changes.
It also arrives somewhere parquet already was: 144.75 bytes per row is parquet's
zstd level 3 to within a byte, which parquet reaches in 1.3 s rather than 35.4 s.
The pass buys knowledge the format can carry forward to every later row group, and
it costs twenty-seven times what the same size costs parquet once.

The shard also found a bug the benchmark could not. Compressing a column's blocks
used to be a serial loop inside one per-column goroutine, so that 330-block text
column ran on a single core while fifteen sat idle, and the README's claim that
the writer parallelises across blocks was not what the code did. Blocks now share
one semaphore across the whole write, the way the reader already did, which took
this shard's write from 2312 ms to 953 ms without changing a byte of the file.

The same shard found a read-side version of it. A plain string column used to walk
its length prefixes twice: once to size the result, once to place the values. The
row group already records the value count, so the first walk was measuring
something the file already said. Passing the count through took the shard's scoped
read from 161 ms to 140 ms, all of it on that one text column, whose prefixes walk
86 MB.

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
the time lives — but only on the write side. Decompression does not search, so a
block compressed harder takes no longer to read back, which makes the level the
one knob whose price is write time alone.

That is why the default and `Optimize` part ways on it. A writer that has not been
asked to scan the data writes at level 3, because the layout a type implies is
free and the caller has not decided the data is worth a pass. `Optimize` has
decided exactly that, so it measures and writes at level 9. On a 255000-row
hacker-news shard the difference is eight percent of the file: 160.23 bytes per row
at level 3, 153.31 at level 6, 147.43 at level 9. Reading any of the three costs
about the same, because decompression does not search. The pass also picks its
layouts at that level, so the encoding it reports is the one the file gets.
`OptimizeLevel` sets it, and `CompressLevel` sets the level of every row group
written without a pass.

Parquet pays nothing to choose a layout: its encodings are compiled in. A keine
write that has not been Optimized pays the same nothing, because the layout comes
from the type. The 33 ms above is encoding and compressing the five columns and
nothing else, which is what buys the size advantage over a format with a better
compressor: keine has a worse one and spends the time it saved on not measuring.

`Optimize` trades that back. Its 7.2 s is a full encode and compress pass per
candidate per column, thirty-six measurements on this file, each one now at the
codec's best level rather than the write's default. Only three of them change
anything. A caller who wants it pays for the thirty-three that do not, because
there is no way to know which three without measuring all of them.

A write's allocations were the same machinery, twice over. Each compressor owns a
hash table and a sliding window, and a writer that measures a candidate per codec
while also compressing each of its blocks builds several hundred of them; they are
pooled and reset, which is what Reset is for, and the buffer a measurement lands
in belongs to its column for the whole write rather than being grown per
candidate. That took the comparison file's write from 108 MB to 61 MB. What is
left of it is the work itself: the dictionary encoder keying a value through `fmt`
once per distinct entry, the canonical copy of the caller's `[]any`, and the
compressed bytes the file is made of, which have to be
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
