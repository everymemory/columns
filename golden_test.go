package keine

import (
	"bytes"
	"testing"
)

// The writer has to be byte stable, so the file it produces for a known set of
// inputs is pinned here. The golden bytes are the round trip test's three row
// groups and thirteen typed columns, stored the way their types imply: the test
// writes through NewWriter, so no Optimize pass has read the columns and the
// golden file is also what a caller gets for doing nothing. The inputs are
// deterministic, so the comparison is exact rather than statistical.
func TestWriterOutputUnchanged(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, buildSchema())
	for g := 0; g < 3; g++ {
		if err := w.AddRowGroup(buildColumns(g)); err != nil {
			t.Fatalf("AddRowGroup(%d): %v", g, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	golden := goldenFile
	if !bytes.Equal(buf.Bytes(), golden) {
		t.Fatalf("writer output changed: wrote %d bytes, golden is %d bytes",
			buf.Len(), len(golden))
	}
}

// TestWriterDeterministic guards the tiebreak in BenchmarkLayouts, which is what
// Optimize picks a layout on. Two candidates that compress to the same size used
// to be ordered by measured decode time, which varies run to run, so the same
// columns could pick different layouts and write a different file.
func TestWriterDeterministic(t *testing.T) {
	write := func() []byte {
		var buf bytes.Buffer
		w := NewWriter(&buf, buildSchema())
		for g := 0; g < 3; g++ {
			if err := w.AddRowGroup(buildColumns(g)); err != nil {
				t.Fatalf("AddRowGroup(%d): %v", g, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return buf.Bytes()
	}

	first := write()
	for i := 0; i < 5; i++ {
		if got := write(); !bytes.Equal(got, first) {
			j := 0
			for j < len(first) && j < len(got) && first[j] == got[j] {
				j++
			}
			t.Fatalf("repeat %d wrote a different file, first differing byte at %d", i+1, j)
		}
	}
}
