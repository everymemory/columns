// Writer option and column validation errors.

package keine

import (
	"io"
	"testing"
)

func TestWriterErrors(t *testing.T) {
	schema := []ColumnSchema{{Name: "b", Type: TypeBool}}
	twoCols := []ColumnSchema{
		{Name: "a", Type: TypeBool},
		{Name: "b", Type: TypeBool},
	}

	// A failure writing the leading magic surfaces on first use.
	w := NewWriter(&failAfter{n: 0}, schema)
	if err := w.AddRowGroup([][]any{{true, false}}); err == nil {
		t.Error("AddRowGroup after a failed magic write: want error, got nil")
	}
	if err := w.Close(); err == nil {
		t.Error("Close after a failed magic write: want error, got nil")
	}

	// The format version is the write after it, and fails the same way.
	w = NewWriter(&failAfter{n: 1}, schema)
	if err := w.AddRowGroup([][]any{{true, false}}); err == nil {
		t.Error("AddRowGroup after a failed version write: want error, got nil")
	}

	w = NewWriter(io.Discard, schema)
	if err := w.AddRowGroup([][]any{{true}, {false}}); err == nil {
		t.Error("AddRowGroup with the wrong column count: want error, got nil")
	}

	w = NewWriter(io.Discard, twoCols)
	if err := w.AddRowGroup([][]any{{true, false}, {true}}); err == nil {
		t.Error("AddRowGroup with mismatched row counts: want error, got nil")
	}

	// Every candidate encoding rejects these values, so the chosen layout
	// cannot encode them either.
	w = NewWriter(io.Discard, schema)
	if err := w.AddRowGroup([][]any{{struct{}{}, struct{}{}}}); err == nil {
		t.Error("AddRowGroup of unencodable values: want error, got nil")
	}

	// Dictionary encoding accepts these but the statistics cannot be built.
	bytesSchema := []ColumnSchema{{Name: "c", Type: TypeBytes}}
	w = NewWriter(io.Discard, bytesSchema)
	if err := w.AddRowGroup([][]any{{42, 43}}); err == nil {
		t.Error("AddRowGroup of values that break statistics: want error, got nil")
	}

	// The underlying writer fails partway through the chunk. NewWriter has written
	// the magic and the version as one write, so the chunk header is the second.
	w = NewWriter(&failAfter{n: 2}, schema)
	if err := w.AddRowGroup([][]any{{true, false}}); err == nil {
		t.Error("AddRowGroup with a failing writer: want error, got nil")
	}

	// Close failing at each of its three writes. NewWriter has already written the
	// header in one, so Close's are the second, third and fourth.
	for n := 1; n <= 3; n++ {
		w := NewWriter(&failAfter{n: n}, nil)
		if err := w.AddRowGroup(nil); err != nil {
			t.Fatalf("AddRowGroup of an empty row group: %v", err)
		}
		if err := w.Close(); err == nil {
			t.Errorf("Close with a writer failing after %d writes: want error, got nil", n)
		}
	}
}
