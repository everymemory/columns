package tokenizer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// miniTokenizer is a tokenizer.json that satisfies every check Load performs:
// a BPE model behind a single ByteLevel stage. It is not a real vocabulary, only
// a second model distinct from the one in testdata, which is what the registry's
// collision rule needs to be exercised.
const miniTokenizer = `{"pre_tokenizer":{"type":"ByteLevel"},"model":{"type":"BPE","vocab":{"a":0,"b":1},"merges":[]},"added_tokens":[]}`

func TestRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	m, err := LoadFile(modelPath)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	if err := r.Register(m); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := r.Lookup(m.Hash())
	if !ok || got != m {
		h := m.Hash()
		t.Fatalf("Lookup(%x) = %v ok=%v, want the model back", h[:8], got, ok)
	}
	// Registering the same model again is quiet: a caller that loads the same
	// tokenizer in two places is not an error.
	if err := r.Register(m); err != nil {
		t.Fatalf("register the same model twice: %v", err)
	}
	if _, ok := r.Lookup([32]byte{1}); ok {
		t.Error("Lookup of a hash nothing was registered under returned a model")
	}
}

// TestRegisterCollision covers the rule the hash exists for. Two different models
// under one hash would let a reader decode a column with the tokenizer that did
// not make it, so that has to be refused rather than last-writer-wins.
func TestRegisterCollision(t *testing.T) {
	r := NewRegistry()
	m, err := LoadFile(modelPath)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	if err := r.Register(m); err != nil {
		t.Fatalf("register: %v", err)
	}
	other := &Model{hash: m.Hash()}
	if err := r.Register(other); err == nil {
		t.Fatal("registering a different model under a taken hash succeeded")
	}
}

func TestDefaultRegistry(t *testing.T) {
	if _, ok := Lookup([32]byte{2}); ok {
		t.Fatal("the default registry answered a hash nothing was registered under")
	}
	m, err := LoadFile(modelPath)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	if err := Register(m); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got, ok := Lookup(m.Hash()); !ok || got != m {
		h := m.Hash()
		t.Fatalf("Lookup(%x) = %v ok=%v", h[:8], got, ok)
	}
	// A second model landing in the default registry under the first one's hash is
	// the collision the package-level Register also has to refuse.
	if err := Register(&Model{hash: m.Hash()}); err == nil {
		t.Fatal("Register accepted a second model under a taken hash")
	}
}

func TestLoadFileRegistered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mini.json")
	if err := os.WriteFile(path, []byte(miniTokenizer), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := Load([]byte(miniTokenizer))
	if err != nil {
		t.Fatalf("load mini tokenizer: %v", err)
	}

	// A different model already filed under the hash has to stop the load, or a
	// caller would end up with a column resolved by a tokenizer that did not
	// build the file it is reading. This runs before the successful load below so
	// the hash is still free for it.
	if err := Register(&Model{hash: m.Hash()}); err != nil {
		t.Fatalf("pre-register a stand-in: %v", err)
	}
	if _, err := LoadFileRegistered(path, m.Hash()); err == nil {
		t.Error("LoadFileRegistered filed a model over one already registered")
	}

	// A second tokenizer, distinct from the first, is what the happy path needs.
	otherPath := filepath.Join(dir, "mini-other.json")
	other := strings.Replace(miniTokenizer, `"a":0`, `"c":0`, 1)
	if err := os.WriteFile(otherPath, []byte(other), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	otherModel, err := Load([]byte(other))
	if err != nil {
		t.Fatalf("load second mini tokenizer: %v", err)
	}

	got, err := LoadFileRegistered(otherPath, otherModel.Hash())
	if err != nil {
		t.Fatalf("LoadFileRegistered: %v", err)
	}
	if got.Hash() != otherModel.Hash() {
		t.Errorf("loaded model hashes to %x, want %x", got.Hash(), otherModel.Hash())
	}
	if _, ok := Lookup(got.Hash()); !ok {
		t.Error("LoadFileRegistered did not file the model in the default registry")
	}

	// A hash that does not match the file is a caller mistake worth reporting
	// outright: silently filing it would leave a column pointing at a tokenizer
	// whose own bytes say otherwise.
	var wrong [32]byte
	wrong[0] = 1
	if _, err := LoadFileRegistered(otherPath, wrong); err == nil {
		t.Error("LoadFileRegistered accepted a file that hashes elsewhere")
	}
	if _, err := LoadFileRegistered(filepath.Join(dir, "gone.json"), otherModel.Hash()); err == nil {
		t.Error("LoadFileRegistered accepted a missing file")
	}

	// A file that is present but is not a tokenizer at all fails at Load, before
	// any hash is compared.
	bad := filepath.Join(dir, "not.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadFileRegistered(bad, otherModel.Hash()); err == nil {
		t.Error("LoadFileRegistered accepted a file that is not tokenizer.json")
	}
}
