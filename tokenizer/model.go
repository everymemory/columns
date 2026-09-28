// Package tokenizer reads a Hugging Face tokenizer.json and encodes bytes to the
// token ids that file describes, with no Hugging Face code involved at runtime.
//
// The model it is built for is Falcon-7B's tokenizer.json: a BPE model behind a
// four stage pre-tokenizer (Punctuation, ByteLevel, Digits, Split), with
// byte-level decoding. The stages and the merge walk are implemented here from
// the file's own configuration, and the ids match the reference implementation
// on the differential corpus in testdata.
//
// The tokenizer is a lossless storage front end, not an inference front end.
// Hugging Face's pipeline drops byte values that have no token in the vocabulary
// (28 of them in Falcon); this package emits an escape id followed by the raw
// byte instead, so encoding arbitrary bytes and decoding the result gives the
// original bytes back. Everywhere the two disagree, this one keeps the input.
package tokenizer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Model is a tokenizer model loaded from a tokenizer.json file. The vocab maps
// a token string to its id; mergeRank holds the merges keyed by the ids of the
// pair they join, with lower ranks applied first. Hash is the SHA-256 of the
// exact bytes the model was loaded from, which is what a file carrying a
// tokenized column identifies the tokenizer by.
type Model struct {
	vocab      map[string]uint16
	tokens     []string
	mergeRank  map[uint64]uint16
	escapeID   uint16
	special    map[string]uint16
	maxID      uint16
	modelBytes int
	hash       [32]byte
}

type tokenizerJSON struct {
	PreTokenizer struct {
		Type          string            `json:"type"`
		PreTokenizers []json.RawMessage `json:"pretokenizers"`
	} `json:"pre_tokenizer"`
	Model struct {
		Type   string            `json:"type"`
		Vocab  map[string]uint16 `json:"vocab"`
		Merges []string          `json:"merges"`
	} `json:"model"`
	AddedTokens []struct {
		ID      int    `json:"id"`
		Content string `json:"content"`
		Special bool   `json:"special"`
	} `json:"added_tokens"`
}

// LoadFile reads and validates a tokenizer.json file, then builds the merge
// lookup. It fails on a model this implementation does not reproduce: a
// non-BPE model, a vocab that does not fit in uint16, or a merge that names a
// token the vocab does not hold.
func LoadFile(path string) (*Model, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Load(raw)
}

// Load builds a Model from tokenizer.json bytes. The bytes are model data, not
// code: nothing in them is executed, and the file's own configuration is what
// the pre-tokenizer and decoder stages are built from.
func Load(raw []byte) (*Model, error) {
	var j tokenizerJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("tokenized: parse tokenizer.json: %w", err)
	}
	if j.Model.Type != "BPE" {
		return nil, fmt.Errorf("tokenized: model type %q, want BPE", j.Model.Type)
	}
	if len(j.Model.Vocab) == 0 {
		return nil, fmt.Errorf("tokenized: empty vocab")
	}

	m := &Model{vocab: j.Model.Vocab}
	if err := m.buildTokens(); err != nil {
		return nil, err
	}
	if err := m.buildMerges(j.Model.Merges); err != nil {
		return nil, err
	}
	m.buildSpecial(j.AddedTokens)

	// The pre-tokenizer must be the Sequence this implementation reproduces.
	// A single ByteLevel stage would also work; anything else is a model whose
	// token ids this package does not promise to match.
	if err := checkPreTokenizer(j.PreTokenizer); err != nil {
		return nil, err
	}

	m.modelBytes = len(raw)
	// The tokenizer is identified by the bytes it was built from, hashed as
	// given. Nothing about the model is normalized before hashing, so a
	// reformat of the same vocabulary is a different tokenizer, which is what
	// keeps a reader from substituting one for another.
	m.hash = sha256.Sum256(raw)
	// One id above the vocab's range stands for "the byte that follows has no
	// token". It costs nothing in the common stream and makes the encode of
	// arbitrary bytes lossless, which the tokenizer is not on its own.
	m.escapeID = m.maxID + 1
	return m, nil
}

func (m *Model) buildTokens() error {
	m.tokens = make([]string, 0, len(m.vocab))
	for tok, id := range m.vocab {
		if id > m.maxID {
			m.maxID = id
		}
		m.tokens = append(m.tokens, tok)
	}
	if uint32(m.maxID)+1 > 0xFFFE {
		return fmt.Errorf("tokenized: max id %d leaves no room for an escape id", m.maxID)
	}
	sort.Slice(m.tokens, func(i, j int) bool { return m.vocab[m.tokens[i]] < m.vocab[m.tokens[j]] })
	return nil
}

// mergeKey is the packed pair of two token ids. Rank lookups are by pair, so
// packing them keeps the merge map to one allocation per merge.
func mergeKey(a, b uint16) uint64 {
	return uint64(a)<<16 | uint64(b)
}

func (m *Model) buildMerges(merges []string) error {
	m.mergeRank = make(map[uint64]uint16, len(merges))
	for rank, pair := range merges {
		a, b, ok := splitMerge(pair)
		if !ok {
			return fmt.Errorf("tokenized: merge %d %q is not two tokens", rank, pair)
		}
		ida, ok1 := m.vocab[a]
		idb, ok2 := m.vocab[b]
		if !ok1 || !ok2 {
			// A merge naming an absent token can never fire, so it carries no
			// rank the encoder can look up. Reporting it keeps the model's
			// merge count and the encoder's agreement.
			return fmt.Errorf("tokenized: merge %d %q names a token absent from the vocab", rank, pair)
		}
		m.mergeRank[mergeKey(ida, idb)] = uint16(rank)
	}
	return nil
}

// splitMerge separates a merge's two space-delimited tokens. A token may itself
// contain a space (ByteLevel maps 0x20 to U+0120, not to a literal space, so a
// stored token holding a space is a different string from the merge delimiter).
// The delimiter is the final space, which is the reading that gives two names.
func splitMerge(pair string) (string, string, bool) {
	i := strings.LastIndex(pair, " ")
	if i < 0 {
		return "", "", false
	}
	return pair[:i], pair[i+1:], true
}

func (m *Model) buildSpecial(added []struct {
	ID      int    `json:"id"`
	Content string `json:"content"`
	Special bool   `json:"special"`
}) {
	m.special = make(map[string]uint16, len(added))
	for _, t := range added {
		if t.Special {
			m.special[t.Content] = uint16(t.ID)
		}
	}
}

// stage is one pre-tokenizer in the Sequence, with the fields this
// implementation reads.
type stage struct {
	typ      string
	regex    string
	behavior string
	invert   bool
	digits   bool
}

func checkPreTokenizer(j struct {
	Type          string            `json:"type"`
	PreTokenizers []json.RawMessage `json:"pretokenizers"`
}) error {
	if j.Type != "Sequence" && j.Type != "ByteLevel" {
		return fmt.Errorf("tokenized: pre_tokenizer type %q, want Sequence or ByteLevel", j.Type)
	}
	if j.Type == "ByteLevel" {
		return nil
	}
	var stages []stage
	for _, raw := range j.PreTokenizers {
		var s struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		stages = append(stages, stage{typ: s.Type})
		if s.Type != "Punctuation" && s.Type != "ByteLevel" &&
			s.Type != "Digits" && s.Type != "Split" {
			return fmt.Errorf("tokenized: pre-tokenizer stage %q is not implemented", s.Type)
		}
	}
	if len(stages) == 0 {
		return fmt.Errorf("tokenized: Sequence has no stages")
	}
	return nil
}

// VocabSize is the number of tokens in the model, including the escape id.
func (m *Model) VocabSize() int { return int(m.escapeID) + 1 }

// ModelBytes is the size of the tokenizer.json this was loaded from, for
// comparing verbatim storage against the compact form.
func (m *Model) ModelBytes() int { return m.modelBytes }

// Hash is the SHA-256 of the tokenizer.json bytes the model was built from.
func (m *Model) Hash() [32]byte { return m.hash }
