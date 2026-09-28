package tokenizer

import (
	"encoding/binary"
	"os"
	"testing"
)

// The differential corpus and the reference output committed beside it were both
// produced outside this package: the corpus by testdata/gen_corpus, and the
// reference ids and decoded text by an oracle built from the Hugging Face
// tokenizers Rust crate at v0.21.4, which is the implementation the Python
// wheel is built from and the one that loads Falcon's tokenizer.json
// natively. Matching those ids is the parity claim.
//
// The oracle encodes &str, as the public Rust and Python APIs both do, so a
// record that is not valid UTF-8 reaches it only through from_utf8_lossy and
// comes back with bytes replaced. Those records carry a zero valid flag and are
// excluded from the id comparison: HF drops information there, and this package
// exists to keep it. The round trip covers them instead.

const (
	modelPath  = "testdata/falcon-tokenizer.json"
	corpusPath = "testdata/corpus.bin"
	oraclePath = "testdata/corpus-oracle.bin"
)

type record struct {
	bytes   []byte
	valid   bool
	ids     []uint16
	decoded []byte
}

// loadCorpus reads the corpus and the oracle output together, so a record's
// bytes and the reference ids it is compared against can never drift apart.
func loadCorpus(t *testing.T) ([]record, *Model) {
	t.Helper()
	m, err := LoadFile(modelPath)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}

	corpus, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Skipf("corpus not present: %v", err)
	}
	oracle, err := os.ReadFile(oraclePath)
	if err != nil {
		t.Skipf("oracle output not present: %v", err)
	}

	r := 0
	read32 := func(b []byte, at *int) uint32 {
		v := binary.LittleEndian.Uint32(b[*at : *at+4])
		*at += 4
		return v
	}

	count := int(read32(corpus, &r))
	or := 0
	if got := int(read32(oracle, &or)); got != count {
		t.Fatalf("corpus has %d records, oracle output has %d", count, got)
	}
	records := make([]record, 0, count)
	for i := 0; i < count; i++ {
		len := int(read32(corpus, &r))
		rec := corpus[r : r+len]
		r += len

		valid := oracle[or] != 0
		or++
		n := int(read32(oracle, &or))
		ids := make([]uint16, 0, n)
		for j := 0; j < n; j++ {
			ids = append(ids, uint16(read32(oracle, &or)))
		}
		dlen := int(read32(oracle, &or))
		decoded := oracle[or : or+dlen]
		or += dlen

		records = append(records, record{bytes: rec, valid: valid, ids: ids, decoded: decoded})
	}
	return records, m
}

// TestDifferentialAgainstRust is the main correctness test. On every record the
// reference implementation hands back intact, this package must produce its
// exact ids and its exact decoded text.
//
// "Intact" has to be decided from the reference's own output, not from the
// record's UTF-8 validity. A record made of bytes that have no token in the
// vocabulary still reaches the reference as valid &str, and the reference's BPE
// drops the characters it cannot look up, so its ids decode to less than the
// input: "\x1f\x1e\x1d" encodes to a single id that decodes to "\x1f". Those
// records are counted as dropped and checked by round trip instead, where
// matching ids is impossible by construction and the escape id is the point.
func TestDifferentialAgainstRust(t *testing.T) {
	records, m := loadCorpus(t)

	checked, dropped, repaired := 0, 0, 0
	for _, rec := range records {
		if !rec.valid {
			repaired++
			continue
		}
		if string(rec.decoded) != string(rec.bytes) {
			dropped++
			continue
		}
		ids := m.Encode(string(rec.bytes))
		if !equalIDs(ids, rec.ids) {
			t.Fatalf("first id mismatch: %q\n got %v\nwant %v", rec.bytes, ids, rec.ids)
		}
		decoded, err := m.Decode(ids)
		if err != nil {
			t.Fatalf("decode %q: %v", rec.bytes, err)
		}
		if decoded != string(rec.decoded) {
			t.Fatalf("decoded text differs on %q:\n got %q\nwant %q", rec.bytes, decoded, rec.decoded)
		}
		checked++
	}

	t.Logf("matched the Rust ids and decoded text on %d records "+
		"(%d the reference dropped bytes from, %d repaired by from_utf8_lossy; both checked by round trip)",
		checked, dropped, repaired)
	if checked < 1000 {
		t.Errorf("only %d records matched the oracle, expected the corpus to be larger", checked)
	}
}

// TestRoundTripArbitraryBytes asserts decode(encode(bytes)) == bytes on every
// record, including the ones the reference pipeline loses information on. That
// is the property the storage format needs, and it is stronger than parity on
// those inputs.
func TestRoundTripArbitraryBytes(t *testing.T) {
	records, m := loadCorpus(t)

	failed := 0
	for _, rec := range records {
		ids := m.EncodeBytes(rec.bytes)
		out, err := m.Decode(ids)
		if err != nil {
			t.Errorf("decode % x: %v", rec.bytes, err)
			failed++
			continue
		}
		if out != string(rec.bytes) {
			t.Errorf("round trip % x:\n got % x\nwant % x", rec.bytes, []byte(out), rec.bytes)
			failed++
		}
	}
	if failed > 0 {
		t.Fatalf("%d round trips failed", failed)
	}
}

// TestModelHashMatchesTokenizerFile pins the digest a tokenized column carries.
// A reader resolves the tokenizer by this value and refuses a file whose own
// bytes hash elsewhere, so a change to the committed tokenizer.json has to be
// made deliberately, and the digest below updated with it.
func TestModelHashMatchesTokenizerFile(t *testing.T) {
	m, err := LoadFile(modelPath)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	want := [32]byte{
		0xd6, 0xc5, 0xcd, 0xac, 0x14, 0x21, 0xea, 0x99,
		0x87, 0x22, 0xaa, 0x88, 0xea, 0x1c, 0xb6, 0x30,
		0xb2, 0xa9, 0xec, 0x63, 0xbf, 0x8a, 0x85, 0x33,
		0x1b, 0xb3, 0xd9, 0xd3, 0x21, 0x50, 0x71, 0x86,
	}
	if got := m.Hash(); got != want {
		t.Errorf("model hash = %x, want %x", got, want)
	}
}

// TestEveryByteRoundTrips covers all 256 byte values alone and in context, the
// inputs where the reference pipeline is lossy and the escape earns its place.
func TestEveryByteRoundTrips(t *testing.T) {
	m, err := LoadFile(modelPath)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	for b := 0; b < 256; b++ {
		for _, ctx := range []string{"", "a", "b", "hello ", " world"} {
			in := append([]byte(ctx), byte(b))
			in = append(in, ctx...)
			ids := m.EncodeBytes(in)
			out, err := m.Decode(ids)
			if err != nil {
				t.Fatalf("decode byte %d in %q: %v", b, ctx, err)
			}
			if out != string(in) {
				t.Errorf("byte %d in %q: got %q", b, ctx, out)
			}
		}
	}
}

// TestEmptyAndWhitespace pins the cases pre-tokenizer regexes are most likely to
// disagree on, independently of the corpus. The ids are the reference
// implementation's own for these strings, so a change to the whitespace
// alternatives fails here rather than only showing up as a corpus mismatch.
func TestEmptyAndWhitespace(t *testing.T) {
	m, err := LoadFile(modelPath)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	cases := map[string][]uint16{
		"":      {},
		" ":     {204},
		"  ":    {258},
		"\n":    {193},
		"a\n\n": {76, 1001},
	}
	for in, want := range cases {
		got := m.Encode(in)
		if !equalIDs(got, want) {
			t.Errorf("encode %q: got %v, want %v", in, got, want)
		}
		out, err := m.Decode(got)
		if err != nil || out != in {
			t.Errorf("decode %q: got %q, err %v", in, out, err)
		}
	}
}

// TestByteLevelTableMatchesGPT2 pins the byte-to-character table against the
// values the reference implementation gives. Checking the table against itself
// is not enough: an off-by-one in the U+0100 range still inverts cleanly, and
// 0xad is the one byte where that is easy to get wrong, because it sits between
// two ranges the table keeps.
func TestByteLevelTableMatchesGPT2(t *testing.T) {
	cases := []struct {
		b byte
		r rune
	}{
		{0x00, 0x100}, {0x20, 0x120}, {0x21, 0x21}, {0x7e, 0x7e},
		{0x7f, 0x121}, {0xa0, 0x142}, {0xa1, 0xa1}, {0xab, 0xab},
		{0xac, 0xac}, {0xad, 0x143}, {0xae, 0xae}, {0xff, 0xff},
	}
	for _, c := range cases {
		if got := byteToChar(c.b); got != c.r {
			t.Errorf("byteToChar(0x%02x) = U+%04X, want U+%04X", c.b, got, c.r)
		}
		if b, ok := charToByte(c.r); !ok || b != c.b {
			t.Errorf("charToByte(U+%04X) = 0x%02x ok=%v, want 0x%02x", c.r, b, ok, c.b)
		}
	}
	for b := 0; b < 256; b++ {
		got, ok := charToByte(byteToChar(byte(b)))
		if !ok || got != byte(b) {
			t.Errorf("byte 0x%02x does not round-trip through the table", b)
		}
	}
}

// TestSpecialTokens pins the rule that a special token is an id of its own and
// never reaches the pre-tokenizer, even though every one of Falcon's is made of
// characters the Punctuation stage would otherwise split on.
func TestSpecialTokens(t *testing.T) {
	m, err := LoadFile(modelPath)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	cases := map[string][]uint16{
		">>TITLE<<":             {0},
		"a >>TITLE<< b":         {76, 204, 0, 262},
		">>ANSWER<<x":           {5, 99},
		">>COMMENT<<>>TITLE<<":  {4, 0},
		">>INTRODUCTION<< text": {2, 2288},
	}
	for in, want := range cases {
		got := m.Encode(in)
		if !equalIDs(got, want) {
			t.Errorf("encode %q: got %v, want %v", in, got, want)
		}
		out, err := m.Decode(got)
		if err != nil || out != in {
			t.Errorf("decode %q: got %q, err %v", in, out, err)
		}
	}
}

func equalIDs(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
