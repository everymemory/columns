// Package keine reads and writes a columnar file format.
//
// A file is a sequence of row groups, each storing its columns as independent
// chunks, followed by a footer holding the schema and per-column metadata. The
// footer sits at the end of the file with its own length, so a reader can
// locate it with a single seek to the back of the file and then read any column
// of any row group without scanning the rest.
//
// The writer chooses an encoding and compression codec for each column by
// measuring the candidates, so no column is stored in a layout that costs more
// than the alternatives.
//
// See the README for the on-disk layout.
package keine
