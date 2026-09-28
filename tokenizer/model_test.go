package tokenizer

import (
	"encoding/json"
	"testing"
)

// tokenizerJSON builds a small model document for the error paths, which need a
// model that is wrong in one specific way rather than wrong overall.
func tokenizerDoc(vocab map[string]uint16, merges []string, preType string, stages ...string) []byte {
	doc := map[string]any{
		"model": map[string]any{
			"type":   "BPE",
			"vocab":  vocab,
			"merges": merges,
		},
	}
	if preType == "Sequence" {
		kind := make([]map[string]any, len(stages))
		for i, s := range stages {
			kind[i] = map[string]any{"type": s}
		}
		doc["pre_tokenizer"] = map[string]any{"type": "Sequence", "pretokenizers": kind}
	} else if preType != "" {
		doc["pre_tokenizer"] = map[string]any{"type": preType}
	}
	out, _ := json.Marshal(doc)
	return out
}

var smallVocab = map[string]uint16{"a": 0, "b": 1, "ab": 2}

func TestLoadRejectsModelsItCannotReproduce(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"not json", []byte("{")},
		{"not BPE", tokenizerDocNamed("WordPiece", smallVocab, nil, "")},
		{"empty vocab", tokenizerDocNamed("BPE", map[string]uint16{}, nil, "")},
		{"vocab too large for an escape id", tokenizerDocNamed("BPE", map[string]uint16{"a": 0xffff}, nil, "")},
		{"merge is not two tokens", tokenizerDoc(smallVocab, []string{"abc"}, "Sequence", "ByteLevel")},
		{"merge names an absent token", tokenizerDoc(smallVocab, []string{"a z"}, "Sequence", "ByteLevel")},
		{"unimplemented stage", tokenizerDoc(smallVocab, nil, "Sequence", "Whitespace")},
		{"sequence with no stages", tokenizerDoc(smallVocab, nil, "Sequence")},
		{"unsupported pre-tokenizer", tokenizerDoc(smallVocab, nil, "Whitespace")},
	}
	for _, c := range cases {
		if m, err := Load(c.raw); err == nil {
			t.Errorf("%s: Load succeeded, want an error (model %v)", c.name, m)
		}
	}
}

// tokenizerDocNamed is tokenizerDoc with a model type of its own, for the cases
// that vary it.
func tokenizerDocNamed(modelType string, vocab map[string]uint16, merges []string, preType string) []byte {
	raw := tokenizerDoc(vocab, merges, preType)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	doc["model"].(map[string]any)["type"] = modelType
	out, _ := json.Marshal(doc)
	return out
}

func TestLoadAcceptsByteLevelPreTokenizer(t *testing.T) {
	if _, err := Load(tokenizerDoc(smallVocab, nil, "ByteLevel")); err != nil {
		t.Errorf("ByteLevel pre-tokenizer: %v", err)
	}
}

// TestLoadRejectsMalformedStage covers a Sequence entry that is not an object:
// the outer parse accepts any JSON value there, so it is the per-stage one that
// has to report it.
func TestLoadRejectsMalformedStage(t *testing.T) {
	raw := []byte(`{"model":{"type":"BPE","vocab":{"a":0}}` +
		`,"pre_tokenizer":{"type":"Sequence","pretokenizers":["ByteLevel"]}}`)
	if _, err := Load(raw); err == nil {
		t.Error("Load of a Sequence whose stage is not an object succeeded, want an error")
	}
}

func TestLoadFileReportsMissingFile(t *testing.T) {
	if _, err := LoadFile("/tmp/keine/no-such-tokenizer.json"); err == nil {
		t.Error("LoadFile on a missing file succeeded, want an error")
	}
}

// TestModelAccessors covers the sizes a caller comparing storage would read.
func TestModelAccessors(t *testing.T) {
	m, err := Load(tokenizerDoc(smallVocab, []string{"a b"}, "Sequence", "ByteLevel"))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.VocabSize(); got != 4 {
		t.Errorf("VocabSize = %d, want 4 (three vocab ids plus the escape id)", got)
	}
	if m.ModelBytes() == 0 {
		t.Error("ModelBytes is 0 on a loaded model")
	}
	if toks := m.SortedTokens(); len(toks) != 3 || toks[0] != "a" {
		t.Errorf("SortedTokens = %v, want [a ab b]", toks)
	}
}

// TestSplitMerge covers the delimiter rule, including a token that itself holds
// a space: the delimiter is the final space, so the two names are the parts on
// either side of it.
func TestSplitMerge(t *testing.T) {
	cases := []struct {
		in   string
		a, b string
		ok   bool
	}{
		{"a b", "a", "b", true},
		{"Ġab Ċ", "Ġab", "Ċ", true},
		{"ab", "", "", false},
	}
	for _, c := range cases {
		a, b, ok := splitMerge(c.in)
		if ok != c.ok || (ok && (a != c.a || b != c.b)) {
			t.Errorf("splitMerge(%q) = %q %q %v, want %q %q %v",
				c.in, a, b, ok, c.a, c.b, c.ok)
		}
	}
}

// TestContractions covers each of the pattern's seven suffixes and the
// apostrophe sequences that are not among them.
func TestContractions(t *testing.T) {
	cases := []struct {
		in string
		n  int
	}{
		{"", 0},
		{"'x", 0},
		{"'s", 2}, {"'t", 2}, {"'m", 2}, {"'d", 2},
		{"'re", 3}, {"'ve", 3}, {"'ll", 3},
		{"'r", 0}, {"'v", 0}, {"'l", 0},
		{"'rx", 0}, {"'lx", 0},
		{"'e", 0},
	}
	for _, c := range cases {
		if n := contractionLen(toChars(c.in)); n != c.n {
			t.Errorf("contractionLen(%q) = %d, want %d", c.in, n, c.n)
		}
	}
}

func toChars(s string) []lchar { return bytesToChars([]byte(s)) }

// TestGpt2Words covers the pattern's alternatives on inputs short enough to
// read at a glance, in the terms the walk itself uses.
func TestGpt2Words(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"abc", []string{"abc"}},
		{"ab1", []string{"ab", "1"}},
		{"a!!b", []string{"a", "!!", "b"}},
		{"a  b", []string{"a", " ", " b"}},
		{"  ", []string{"  "}},
		{"a  ", []string{"a", "  "}},
		{"  a", []string{" ", " a"}},
		{"a\tb", []string{"a", "\t", "b"}},
		{"a\n\nb", []string{"a", "\n", "\n", "b"}},
		{"it's", []string{"it", "'s"}},
		{"we'll", []string{"we", "'ll"}},
		{"they're", []string{"they", "'re"}},
		{"a ", []string{"a", " "}},
		{" ", []string{" "}},
	}
	for _, c := range cases {
		words := gpt2Words(toChars(c.in))
		got := make([]string, len(words))
		for i, w := range words {
			got[i] = charsText(w)
		}
		if len(got) != len(c.want) {
			t.Errorf("gpt2Words(%q) = %q, want %q", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("gpt2Words(%q) = %q, want %q", c.in, got, c.want)
				break
			}
		}
	}
}

// TestAllDigits covers the empty string, which is not a digit run even though
// it holds no non-digit character.
func TestAllDigits(t *testing.T) {
	if allDigits("") {
		t.Error("allDigits(\"\") = true, want false")
	}
	if !allDigits("123") {
		t.Error("allDigits(\"123\") = false, want true")
	}
}

// TestCharToByteRejectsUnmapped covers the characters above the table's range,
// which the decoder meets as stored text rather than as bytes.
func TestCharToByteRejectsUnmapped(t *testing.T) {
	for _, r := range []rune{0x00, 0x200, 0x20ac} {
		if b, ok := charToByte(r); ok {
			t.Errorf("charToByte(U+%04X) = 0x%02x, want not mapped", r, b)
		}
	}
}

// TestDecodeRejectsBadStreams covers the two streams the decoder cannot read:
// an escape with no byte after it, and an id the vocab does not hold.
func TestDecodeRejectsBadStreams(t *testing.T) {
	m, err := Load(tokenizerDoc(smallVocab, nil, "Sequence", "ByteLevel"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Decode([]uint16{m.escapeID}); err == nil {
		t.Error("Decode of a trailing escape id succeeded, want an error")
	}
	if _, err := m.Decode([]uint16{m.escapeID + 1}); err == nil {
		t.Error("Decode of an id outside the vocab succeeded, want an error")
	}
}

// TestDecodeEmitsStoredText covers a token whose characters no byte maps to.
// No Falcon token is like that, so the model is built by hand here.
func TestDecodeEmitsStoredText(t *testing.T) {
	m := &Model{
		vocab:    map[string]uint16{"a": 0, "€": 1},
		tokens:   []string{"a", "€"},
		escapeID: 2,
	}
	out, err := m.Decode([]uint16{0, 1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if out != "a€a" {
		t.Errorf("Decode = %q, want a€a", out)
	}
}
