package tokenizer

import (
	"fmt"
	"os"
	"sync"
)

// A Registry maps a tokenizer's hash to the model built from those bytes. A
// reader resolves the tokenizer a file names by its hash and refuses anything
// it cannot verify, so two tokenizers that share a name are never substituted
// for one another and a file is only ever decoded by the tokenizer that made it.
type Registry struct {
	mu     sync.RWMutex
	models map[[32]byte]*Model
}

// NewRegistry holds an empty registry. The package-level Default is what the
// reader uses when the caller has not supplied one.
func NewRegistry() *Registry {
	return &Registry{models: make(map[[32]byte]*Model)}
}

// Default is the registry Register and Lookup use when no other is given.
var Default = NewRegistry()

// Register keeps m under the hash of the bytes it was built from. Registering
// the same model twice is quiet; registering a different model under a hash
// that is already taken is an error, because that hash is what a file on disk
// identifies the tokenizer by.
func (r *Registry) Register(m *Model) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := m.Hash()
	if have, ok := r.models[h]; ok && have != m {
		return fmt.Errorf("tokenizer: a different model is already registered for %x", h[:])
	}
	r.models[h] = m
	return nil
}

// Lookup returns the model registered under hash, or nil and false when no
// model matches. A model registered here is one whose own bytes hash to the key
// it is filed under, since that is what Register keys on.
func (r *Registry) Lookup(hash [32]byte) (*Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[hash]
	return m, ok
}

// Register adds m to the default registry.
func Register(m *Model) error { return Default.Register(m) }

// Lookup is Lookup on the default registry.
func Lookup(hash [32]byte) (*Model, bool) { return Default.Lookup(hash) }

// LoadFileRegistered reads a tokenizer.json file, builds its model, verifies
// that the bytes hash to want, and files it in the default registry. It is the
// convenience loader for a caller that has the file locally and knows what it
// should be; a reader that finds a hash it cannot resolve reports the hash and
// lets the caller decide what to load.
func LoadFileRegistered(path string, want [32]byte) (*Model, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := Load(raw)
	if err != nil {
		return nil, err
	}
	if got := m.Hash(); got != want {
		return nil, fmt.Errorf("tokenizer: %s hashes to %x, want %x", path, got[:], want[:])
	}
	if err := Default.Register(m); err != nil {
		return nil, err
	}
	return m, nil
}
