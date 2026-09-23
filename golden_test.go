package keine

import (
	"bytes"
	"testing"
)

// The typed encode path rewrote every encoder and the statistics pass, so it
// needs proof that it writes the same file. The golden bytes were captured from
// the reflection-based writer at 139d0da on the same three row groups and 13
// typed columns the round trip test uses; the inputs are deterministic, so a
// byte comparison is exact rather than statistical.
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

// TestWriterDeterministic guards the tiebreak in BenchmarkLayouts. Two
// candidates that compress to the same size used to be ordered by measured
// decode time, which varies run to run, so the same columns could pick
// different layouts and write a different file.
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
