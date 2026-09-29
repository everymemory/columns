// Package keine reads and writes a columnar file format.
//
// A file is a sequence of row groups, each storing its columns as independent
// chunks, followed by a footer holding the schema and per-column metadata. The
// footer sits at the end of the file with its own length, so a reader can
// locate it with a single seek to the back of the file and then read any column
// of any row group without scanning the rest.
//
// How a column is laid out is either a property of its type or a measurement of
// its values. NewWriter takes the first: a bool column packs to bits, an
// integer column is stored as deltas, and everything else is stored plainly,
// each compressed with Flate. Those layouts cost nothing to choose and are right
// often enough that a caller who does nothing gets a reasonable file. Passing
// Options to NewWriterWithOptions changes the encoding, the codec, its level and
// the block size, and an empty Options names the defaults.
//
// A row group is written from either [][]any, which AddRowGroup coerces to the
// schema's types, or []any of typed slices, which AddRowGroupTyped hands to the
// encoders as the caller holds them. The two write the same file; the typed entry
// skips the coercion, and refuses a column whose values are not already the type
// its schema implies. ReadColumn is the read side of the same trade.
//
// Optimize trades a pass over the data for knowing what is in it. It encodes and
// compresses every candidate layout against each column's own values, at the
// level the file will be written at, and keeps the smallest. Call it once for a
// dataset and every row group after it is written at the layout that won: a
// column of repeated strings becomes a dictionary, a column of related strings
// shares its affixes, and a column the type serves well keeps the layout it
// would have had anyway.
//
// A text column can also be stored as token ids, which is the Tokenized encoding.
// Options.Tokenizers names the tokenizer for the columns that should have it, and
// the file names it back by the hash of the tokenizer it was built from, resolved
// by a reader through a tokenizer.Registry. The ids are only readable by the
// tokenizer that made them, so a hash the registry cannot resolve is an error
// rather than a substitution.
//
// See the README for the on-disk layout.
package keine
