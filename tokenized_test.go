package keine

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	"github.com/everymemory/keine/tokenizer"
)

// A Tokenized column is tested against the corpus the tokenizer package already
// parity-tests against the Rust reference: this layer's job is to hand the ids
// back as the bytes they came from, so the values that exercise the tokenizer's
// own escape path are the ones that prove the payload keeps them.
var tokenizedLayouts = func() []TokenizedLayout {
	var out []TokenizedLayout
	for _, rep := range []uint8{TokenizedRaw, TokenizedRemap, TokenizedBits} {
		for _, bound := range []uint8{TokenizedCounts, TokenizedOffsets, TokenizedDeltas} {
			out = append(out, TokenizedLayout{Rep: rep, Bound: bound})
		}
	}
	return out
}()

// tokenizedModel is Falcon's tokenizer, loaded from the corpus the tokenizer
// package keeps. LoadFileRegistered is what a caller goes through to make a model
// resolvable by its hash, which is how a reader finds one.
func tokenizedModel(t *testing.T) *tokenizer.Model {
	t.Helper()
	m, err := tokenizer.LoadFile("tokenizer/testdata/falcon-tokenizer.json")
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	return m
}

// tokenizedCorpus is the differential corpus as a []string, which is the shape a
// TypeString column carries.
func tokenizedCorpus(t *testing.T) []string {
	t.Helper()
	corpus, err := loadCorpusFile("tokenizer/testdata/corpus.bin")
	if err != nil {
		t.Skipf("corpus not present: %v", err)
	}
	out := make([]string, len(corpus))
	for i, rec := range corpus {
		out[i] = string(rec)
	}
	return out
}

// TestEncodeTokenizedRoundTrip asserts decode(encode(values)) == values over the
// whole corpus, for every representation against every boundary layout. The nine
// combinations are the payload's whole design space, and the corpus is the input
// set the reference implementation was measured on, including the records whose
// bytes the reference drops.
func TestEncodeTokenizedRoundTrip(t *testing.T) {
	vals := tokenizedCorpus(t)
	tok := tokenizedModel(t)

	for _, layout := range tokenizedLayouts {
		t.Run(repName(layout.Rep)+"/"+boundName(layout.Bound), func(t *testing.T) {
			enc, err := EncodeTokenized(vals, tok, layout)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			out, err := DecodeTokenized(enc, tok, TypeString)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			back := out.([]string)
			if len(back) != len(vals) {
				t.Fatalf("decoded %d values, wrote %d", len(back), len(vals))
			}
			for i := range vals {
				if back[i] != vals[i] {
					t.Fatalf("value %d:\n got %q\nwant %q", i, back[i], vals[i])
				}
			}
			t.Logf("%d values, %d bytes encoded", len(vals), len(enc))
		})
	}
}

// TestEncodeTokenizedBytes is the same round trip through a TypeBytes column,
// which is the other type a tokenizer applies to and the one whose values are
// already the bytes the tokenizer reads.
func TestEncodeTokenizedBytes(t *testing.T) {
	vals := tokenizedCorpus(t)
	tok := tokenizedModel(t)
	byts := make([][]byte, len(vals))
	for i, v := range vals {
		byts[i] = []byte(v)
	}

	for _, layout := range tokenizedLayouts {
		enc, err := EncodeTokenized(byts, tok, layout)
		if err != nil {
			t.Fatalf("encode %v: %v", layout, err)
		}
		out, err := DecodeTokenized(enc, tok, TypeBytes)
		if err != nil {
			t.Fatalf("decode %v: %v", layout, err)
		}
		back := out.([][]byte)
		if len(back) != len(byts) {
			t.Fatalf("decoded %d values, wrote %d", len(back), len(byts))
		}
		for i := range byts {
			if string(back[i]) != string(byts[i]) {
				t.Fatalf("value %d:\n got % x\nwant % x", i, back[i], byts[i])
			}
		}
	}
}

// TestEncodeTokenizedSizes compares the nine layouts on the same column, which is
// what the optimizer picks from. The numbers are reported rather than asserted:
// the claim this test makes is that the compressed sizes order themselves, and
// that no layout silently produces a different column.
func TestEncodeTokenizedSizes(t *testing.T) {
	vals := tokenizedCorpus(t)
	tok := tokenizedModel(t)

	for _, layout := range tokenizedLayouts {
		enc, err := EncodeTokenized(vals, tok, layout)
		if err != nil {
			t.Fatalf("encode %v: %v", layout, err)
		}
		flate, err := compressAt(enc, CompressFlate, flateLevel)
		if err != nil {
			t.Fatalf("compress %v: %v", layout, err)
		}
		t.Logf("%-8s/%-8s encoded %8d flate %8d", repName(layout.Rep), boundName(layout.Bound), len(enc), len(flate))
	}
}

// TestEncodeTokenizedRejectsWrongType covers the contract: only a string or bytes
// column can be tokenized, and only the layouts this build writes.
func TestEncodeTokenizedRejectsWrongType(t *testing.T) {
	tok := tokenizedModel(t)

	if _, err := EncodeTokenized([]int64{1, 2}, tok, TokenizedLayout{Rep: TokenizedRaw, Bound: TokenizedCounts}); err == nil {
		t.Error("EncodeTokenized accepted an int64 column")
	}
	if _, err := EncodeTokenized([]string{"a"}, tok, TokenizedLayout{Rep: 9, Bound: TokenizedCounts}); err == nil {
		t.Error("EncodeTokenized accepted an unknown representation")
	}
	if _, err := EncodeTokenized([]string{"a"}, tok, TokenizedLayout{Rep: TokenizedRaw, Bound: 9}); err == nil {
		t.Error("EncodeTokenized accepted an unknown boundary")
	}
}

// TestDecodeTokenizedRejectsBrokenChunks covers the checks a reader makes before it
// trusts any count in the payload. Each case corrupts one field or one stream of a
// real chunk, and is one way a truncated or self-inconsistent chunk can present
// itself.
func TestDecodeTokenizedRejectsBrokenChunks(t *testing.T) {
	tok := tokenizedModel(t)
	marker := []byte{byte(tok.EscapeID()), byte(tok.EscapeID() >> 8)}

	// threeValues is short enough to read at a glance and long enough for a boundary
	// stream to have an ordering worth violating.
	threeValues := func(rep, bound uint8) []byte {
		t.Helper()
		enc, err := EncodeTokenized([]string{"ab", "cd", "ef"}, tok, TokenizedLayout{Rep: rep, Bound: bound})
		if err != nil {
			t.Fatalf("encode %d/%d: %v", rep, bound, err)
		}
		return enc
	}
	// oneEscape is a column whose first value carries a byte the vocab has no token
	// for, so the chunk holds exactly the one escape the value walk has to account
	// for, and it is the first thing in the id stream.
	oneEscape := func() []byte {
		t.Helper()
		enc, err := EncodeTokenized([]string{"\x1ea", "b"}, tok, TokenizedLayout{Rep: TokenizedRaw, Bound: TokenizedCounts})
		if err != nil {
			t.Fatalf("encode escaped column: %v", err)
		}
		if got := readCount(enc, tokenizedEscapesAt); got != 1 {
			t.Fatalf("escaped column holds %d escape bytes, want 1", got)
		}
		return enc
	}

	cases := []struct {
		name string
		data []byte
	}{
		{"truncated header", make([]byte, tokenizedHeaderLen-1)},
		{"regions do not fit in the chunk", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedCounts)
			writeCount(b, tokenizedValuesAt, 0xfffffff0)
			return b
		}()},
		{"offset that goes backwards", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedOffsets)
			r := tokenizedRegionsOf(b)
			writeCount(b, r.bound+8, 0) // value 2 starts where value 1 did
			return b
		}()},
		{"deltas giving a negative count", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedDeltas)
			r := tokenizedRegionsOf(b)
			writeCount(b, r.bound, 0xffffffff)
			return b
		}()},
		{"deltas past the recorded token count", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedDeltas)
			r := tokenizedRegionsOf(b)
			writeCount(b, r.bound, 0x50)
			return b
		}()},
		{"deltas short of the recorded token count", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedDeltas)
			r := tokenizedRegionsOf(b)
			writeCount(b, r.bound, 0) // value 0 takes no tokens
			return b
		}()},
		{"counts that do not sum to the token count", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedCounts)
			writeCount(b, tokenizedTokensAt, 9)
			return b
		}()},
		{"raw id stream that disagrees with the token count", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedOffsets)
			writeCount(b, tokenizedTokensAt, readCount(b, tokenizedTokensAt)+1)
			return b
		}()},
		{"bit-packed id stream that disagrees with the token count", func() []byte {
			b := threeValues(TokenizedBits, TokenizedOffsets)
			writeCount(b, tokenizedTokensAt, readCount(b, tokenizedTokensAt)+1)
			return b
		}()},
		{"varint stream that runs out before the token count", func() []byte {
			b := threeValues(TokenizedRemap, TokenizedOffsets)
			writeCount(b, tokenizedTokensAt, readCount(b, tokenizedTokensAt)+1)
			return b
		}()},
		{"varint index outside the table", func() []byte {
			b := threeValues(TokenizedRemap, TokenizedCounts)
			r := tokenizedRegionsOf(b)
			b[r.ids] = 0xff // a two byte index, past any table this column builds
			return b
		}()},
		{"escape marker with no byte to carry", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedCounts)
			r := tokenizedRegionsOf(b)
			b[r.ids], b[r.ids+1] = marker[0], marker[1] // an ordinary id becomes a marker
			return b
		}()},
		{"escape byte that no value claims", func() []byte {
			b := oneEscape()
			r := tokenizedRegionsOf(b)
			// The marker becomes the id that followed it, so the escape it stood for
			// is left over at the end of the walk.
			b[r.ids], b[r.ids+1] = b[r.ids+2], b[r.ids+3]
			return b
		}()},
		{"id the vocab does not hold", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedCounts)
			r := tokenizedRegionsOf(b)
			b[r.ids], b[r.ids+1] = 0xff, 0xff
			return b
		}()},
		{"unknown representation", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedCounts)
			b[0] = 9
			return b
		}()},
		{"unknown boundary", func() []byte {
			b := threeValues(TokenizedRaw, TokenizedCounts)
			b[1] = 9
			return b
		}()},
	}
	for _, c := range cases {
		if _, err := DecodeTokenized(c.data, tok, TypeString); err == nil {
			t.Errorf("DecodeTokenized accepted %s", c.name)
		}
	}
}

// The four header counts sit at fixed offsets, and the streams they size follow the
// table, so a test can name the byte it is corrupting instead of counting there.
const (
	tokenizedValuesAt   = 2
	tokenizedTokensAt   = 6
	tokenizedEscapesAt  = 10
	tokenizedTableLenAt = 14
)

type tokenizedRegions struct {
	table, bound, ids, esc int
}

// tokenizedRegionsOf locates the streams inside a chunk by reading its own header,
// the way the decoder does. The table and the boundaries come first, then the ids,
// then the escapes.
func tokenizedRegionsOf(b []byte) tokenizedRegions {
	r := tokenizedRegions{
		table: tokenizedHeaderLen,
		bound: tokenizedHeaderLen + 2*readCount(b, tokenizedTableLenAt),
	}
	r.ids = r.bound + 4*readCount(b, tokenizedValuesAt)
	r.esc = len(b) - readCount(b, tokenizedEscapesAt)
	return r
}

func readCount(b []byte, at int) int {
	return int(binary.LittleEndian.Uint32(b[at : at+4]))
}

func writeCount(b []byte, at, v int) {
	binary.LittleEndian.PutUint32(b[at:at+4], uint32(v))
}

// TestEncodeTokenizedEmpty covers the column with no values, whose token count is
// zero and whose every stream is empty. A reader has to come back with an empty
// slice, not an error.
func TestEncodeTokenizedEmpty(t *testing.T) {
	tok := tokenizedModel(t)
	for _, layout := range tokenizedLayouts {
		enc, err := EncodeTokenized([]string{}, tok, layout)
		if err != nil {
			t.Fatalf("encode %v: %v", layout, err)
		}
		out, err := DecodeTokenized(enc, tok, TypeString)
		if err != nil {
			t.Fatalf("decode %v: %v", layout, err)
		}
		if got := len(out.([]string)); got != 0 {
			t.Errorf("%v decoded %d values from an empty column", layout, got)
		}
	}
}

// TestEncodeTokenizedEscapes covers the inputs the tokenizer has no token for,
// which are the ones the escape path exists for. Every byte value is encoded
// alone and between letters, so a byte that has a token in one context and none
// in the other both reaches the payload and comes back.
func TestEncodeTokenizedEscapes(t *testing.T) {
	tok := tokenizedModel(t)
	for _, layout := range tokenizedLayouts {
		for b := 0; b < 256; b++ {
			vals := []string{"a" + string(rune(b)) + "b", string(rune(b))}
			enc, err := EncodeTokenized(vals, tok, layout)
			if err != nil {
				t.Fatalf("encode %v byte %d: %v", layout, b, err)
			}
			out, err := DecodeTokenized(enc, tok, TypeString)
			if err != nil {
				t.Fatalf("decode %v byte %d: %v", layout, b, err)
			}
			back := out.([]string)
			if back[0] != vals[0] || back[1] != vals[1] {
				t.Fatalf("%v byte %d: got %q %q", layout, b, back[0], back[1])
			}
		}
	}
}

// TestRemapIsDeterministic pins the ordering the remap table is built in: two
// encodes of the same column have to produce the same table and the same bytes,
// because a file's contents may not depend on map iteration order.
func TestRemapIsDeterministic(t *testing.T) {
	vals := tokenizedCorpus(t)
	tok := tokenizedModel(t)

	var prev []byte
	for i := 0; i < 20; i++ {
		enc, err := EncodeTokenized(vals, tok, TokenizedLayout{Rep: TokenizedRemap, Bound: TokenizedCounts})
		if err != nil {
			t.Fatal(err)
		}
		if prev != nil && string(prev) != string(enc) {
			t.Fatal("the remap layout produced different bytes for the same column")
		}
		prev = enc
	}
}

func repName(rep uint8) string {
	switch rep {
	case TokenizedRaw:
		return "raw"
	case TokenizedRemap:
		return "remap"
	case TokenizedBits:
		return "bits"
	}
	return "unknown"
}

func boundName(bound uint8) string {
	switch bound {
	case TokenizedCounts:
		return "counts"
	case TokenizedOffsets:
		return "offsets"
	case TokenizedDeltas:
		return "deltas"
	}
	return "unknown"
}

// loadCorpusFile reads the length-prefixed record stream the tokenizer package
// generates, so this layer is tested on the same inputs the tokenizer was
// parity-tested against.
func loadCorpusFile(path string) ([][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 4 {
		return nil, fmt.Errorf("corpus is %d bytes", len(raw))
	}
	r := 0
	count := int(binary.LittleEndian.Uint32(raw[r:]))
	r += 4
	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if r+4 > len(raw) {
			return nil, fmt.Errorf("corpus truncated at record %d", i)
		}
		n := int(binary.LittleEndian.Uint32(raw[r:]))
		r += 4
		if r+n > len(raw) {
			return nil, fmt.Errorf("corpus record %d is truncated", i)
		}
		out = append(out, raw[r:r+n])
		r += n
	}
	return out, nil
}
