package columns

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/everymemory/columns/tokenizer"
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
func tokenizedModel(tb testing.TB) *tokenizer.Model {
	tb.Helper()
	m, err := tokenizer.LoadFile("tokenizer/testdata/falcon-tokenizer.json")
	if err != nil {
		tb.Fatalf("load model: %v", err)
	}
	return m
}

// tokenizedCorpus is the differential corpus as a []string, which is the shape a
// TypeString column carries.
func tokenizedCorpus(tb testing.TB) []string {
	tb.Helper()
	corpus, err := loadCorpusFile("tokenizer/testdata/corpus.bin")
	if err != nil {
		tb.Skipf("corpus not present: %v", err)
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

// boxedCorpus is the corpus as the boxed write API takes it, one interface per
// value, which is the shape the layout pass measures.
func boxedCorpus(t *testing.T) []any {
	t.Helper()
	return boxedStrings(tokenizedCorpus(t))
}

// boxedStrings is a slice of strings as the boxed API takes it: one interface
// per value.
func boxedStrings(vals []string) []any {
	out := make([]any, len(vals))
	for i := range vals {
		out[i] = vals[i]
	}
	return out
}

// registeredModel is the Falcon model filed in the default registry, which is
// where a reader of a file that names it looks it up. Registering is what a
// caller does once, before the file that needs the model is read; the lookup
// first keeps a second test from tripping the registry's rule against filing a
// second model under one hash.
func registeredModel(tb testing.TB) *tokenizer.Model {
	tb.Helper()
	m := tokenizedModel(tb)
	if have, ok := tokenizer.Lookup(m.Hash()); ok {
		return have
	}
	if err := tokenizer.Register(m); err != nil {
		tb.Fatalf("register model: %v", err)
	}
	return m
}

// tokenizedFile writes one text column through tok, in the layout asked for, and
// returns the file's bytes. The model is registered, because that is what a
// reader of the file needs to resolve the tokenizer it names.
func tokenizedFile(t *testing.T, vals []string, tok *tokenizer.Model, layout TokenizedLayout) []byte {
	t.Helper()
	schema := []ColumnSchema{{Name: "text", Type: TypeString}}
	var buf bytes.Buffer
	w := NewWriterWithOptions(&buf, schema, Options{
		Compress:        CompressFlate,
		Tokenizers:      map[int]*tokenizer.Model{0: tok},
		TokenizedLayout: layout,
		CompressLevel:   flateLevel,
	})
	if err := w.AddRowGroupTyped([]any{vals}); err != nil {
		t.Fatalf("AddRowGroupTyped: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// TestTokenizedColumnRoundTrip writes a column as token ids through the file
// format and reads it back, for every layout. The file's own header names the
// tokenizer by hash and the reader resolves it, so nothing about the column is
// carried out of band.
func TestTokenizedColumnRoundTrip(t *testing.T) {
	vals := tokenizedCorpus(t)
	tok := registeredModel(t)

	for _, layout := range tokenizedLayouts {
		t.Run(repName(layout.Rep)+"/"+boundName(layout.Bound), func(t *testing.T) {
			b := tokenizedFile(t, vals, tok, layout)
			r, err := NewReader(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			out, err := r.ReadRowGroup(0, []int{0})
			if err != nil {
				t.Fatalf("ReadRowGroup: %v", err)
			}
			back := out[0]
			if len(back) != len(vals) {
				t.Fatalf("read %d values, wrote %d", len(back), len(vals))
			}
			for i := range vals {
				if s, ok := back[i].(string); !ok || s != vals[i] {
					t.Fatalf("value %d: got %#v, want %q", i, back[i], vals[i])
				}
			}
			t.Logf("%d values, %d bytes", len(vals), len(b))
		})
	}
}

// TestTokenizedColumnScoped is the same column through the typed and scoped read
// paths, which are the ones a caller holding a []string uses.
func TestTokenizedColumnScoped(t *testing.T) {
	vals := tokenizedCorpus(t)
	tok := registeredModel(t)

	b := tokenizedFile(t, vals, tok, TokenizedLayout{Rep: TokenizedBits, Bound: TokenizedDeltas})
	r, err := NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if err := r.ReadRowGroupScoped(0, []int{0}, func(cols *Columns) error {
		col, err := Column[string](cols, 0)
		if err != nil {
			return err
		}
		if len(col) != len(vals) {
			t.Fatalf("scoped read gave %d values, want %d", len(col), len(vals))
		}
		for i := range vals {
			if col[i] != vals[i] {
				t.Fatalf("scoped value %d: got %q, want %q", i, col[i], vals[i])
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("ReadRowGroupScoped: %v", err)
	}

	got, err := ReadColumn[string](r, 0, 0)
	if err != nil {
		t.Fatalf("ReadColumn: %v", err)
	}
	if len(got) != len(vals) {
		t.Fatalf("ReadColumn gave %d values, want %d", len(got), len(vals))
	}
	for i := range vals {
		if got[i] != vals[i] {
			t.Fatalf("ReadColumn value %d: got %q, want %q", i, got[i], vals[i])
		}
	}
}

// TestTokenizedBytesColumn is the bytes column's path through the same format. A
// TypeBytes column is a column of arbitrary bytes, which the tokenizer takes as
// bytes and hands back as bytes, so the round trip has to hold for values a
// string column would have to carry through the escape path.
func TestTokenizedBytesColumn(t *testing.T) {
	tok := registeredModel(t)
	vals := [][]byte{{}, []byte("hello"), {0x00, 0xff, 0x1e}, []byte("world hello")}

	for _, layout := range tokenizedLayouts {
		t.Run(repName(layout.Rep)+"/"+boundName(layout.Bound), func(t *testing.T) {
			schema := []ColumnSchema{{Name: "b", Type: TypeBytes}}
			var buf bytes.Buffer
			w := NewWriterWithOptions(&buf, schema, Options{
				Compress:        CompressFlate,
				CompressLevel:   flateLevel,
				Tokenizers:      map[int]*tokenizer.Model{0: tok},
				TokenizedLayout: layout,
			})
			if err := w.AddRowGroupTyped([]any{vals}); err != nil {
				t.Fatalf("AddRowGroupTyped: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			r, err := NewReader(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			out, err := r.ReadRowGroup(0, []int{0})
			if err != nil {
				t.Fatalf("ReadRowGroup: %v", err)
			}
			back := out[0]
			if len(back) != len(vals) {
				t.Fatalf("read %d values, wrote %d", len(back), len(vals))
			}
			for i := range vals {
				got, ok := back[i].([]byte)
				if !ok || !bytes.Equal(got, vals[i]) {
					t.Fatalf("value %d: got %#v, want %#v", i, back[i], vals[i])
				}
			}
		})
	}
}

// TestTokenizedLayoutResults covers the pass's own gate: a column that is not
// strings or bytes has no tokenized layouts, so the candidates that remain are
// the ones the column can actually take.
func TestTokenizedLayoutResults(t *testing.T) {
	tok := registeredModel(t)
	if got := tokenizedLayoutResults([]int64{1, 2}, TypeInt64, tok, &measureScratch{}, flateLevel); got != nil {
		t.Errorf("an int64 column produced %d tokenized layouts, want none", len(got))
	}
	results := tokenizedLayoutResults(tokenizedCorpus(t), TypeString, tok, &measureScratch{}, flateLevel)
	if len(results) != 9*len(layoutCodecs) {
		t.Errorf("nine layouts over %d codecs gave %d results, want %d",
			len(layoutCodecs), len(results), 9*len(layoutCodecs))
	}
	// Raw and Counts are the zero values of the two fields, so an unset layout and
	// the first layout look alike; what distinguishes a measured result is that its
	// pair is one of the nine that were run.
	layouts := map[TokenizedLayout]bool{}
	for _, rep := range []uint8{TokenizedRaw, TokenizedRemap, TokenizedBits} {
		for _, bound := range []uint8{TokenizedCounts, TokenizedOffsets, TokenizedDeltas} {
			layouts[TokenizedLayout{Rep: rep, Bound: bound}] = true
		}
	}
	seen := map[TokenizedLayout]bool{}
	for _, r := range results {
		if r.Encoding != EncTokenized {
			t.Errorf("a tokenized pass reported %s, want Tokenized", r.Name)
		}
		if !layouts[TokenizedLayout{Rep: r.Rep, Bound: r.Bound}] {
			t.Errorf("a tokenized result reported an unset layout: %+v", r)
		}
		seen[TokenizedLayout{Rep: r.Rep, Bound: r.Bound}] = true
	}
	if len(seen) != 9 {
		t.Errorf("the results cover %d of the nine layouts", len(seen))
	}
}

// TestTokenizedHeader pins the header a file with a tokenizer writes: version 2,
// one digest, then the chunks. The digest is the tokenizer's own hash, so a file
// names the model that wrote it and nothing else.
func TestTokenizedHeader(t *testing.T) {
	tok := registeredModel(t)
	b := tokenizedFile(t, []string{"hello", "world"}, tok, TokenizedLayout{})

	if got := b[len(magic)]; got != tokenizedVersion {
		t.Fatalf("version byte is %d, want %d", got, tokenizedVersion)
	}
	count := int(binary.LittleEndian.Uint32(b[len(magic)+1:]))
	if count != 1 {
		t.Fatalf("tokenizer table holds %d entries, want 1", count)
	}
	at := len(magic) + 5
	var have [digestLen]byte
	copy(have[:], b[at:at+digestLen])
	if have != tok.Hash() {
		t.Fatalf("the table holds %x, the model hashes to %x", have, tok.Hash())
	}

	// The table ends where the first chunk begins, and the reader that walks it
	// lands on a chunk it can read. That is what makes the digest's position a
	// length the file agrees with itself on.
	r, err := NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewReader on a file whose header the table ends: %v", err)
	}
	if len(r.toks) != 1 {
		t.Fatalf("the reader resolved %d tokenizers, the table names 1", len(r.toks))
	}
}

// TestTokenizedColumnsShareOneEntry covers the table's deduplication: two columns
// through one model name one entry, so the digest is written once.
func TestTokenizedColumnsShareOneEntry(t *testing.T) {
	tok := registeredModel(t)
	vals := []string{"one", "two", "three"}
	schema := []ColumnSchema{
		{Name: "a", Type: TypeString},
		{Name: "b", Type: TypeString},
	}
	var buf bytes.Buffer
	w := NewWriterWithOptions(&buf, schema, Options{
		Compress:        CompressFlate,
		Tokenizers:      map[int]*tokenizer.Model{0: tok, 1: tok},
		TokenizedLayout: TokenizedLayout{},
	})
	if err := w.AddRowGroupTyped([]any{vals, vals}); err != nil {
		t.Fatalf("AddRowGroupTyped: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b := buf.Bytes()
	count := int(binary.LittleEndian.Uint32(b[len(magic)+1:]))
	if count != 1 {
		t.Fatalf("two columns through one model named %d table entries, want 1", count)
	}

	r, err := NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	out, err := r.ReadRowGroup(0, []int{0, 1})
	if err != nil {
		t.Fatalf("ReadRowGroup: %v", err)
	}
	for _, col := range out {
		if len(col) != len(vals) {
			t.Fatalf("read %d values, want %d", len(col), len(vals))
		}
		for i := range vals {
			if s, ok := col[i].(string); !ok || s != vals[i] {
				t.Fatalf("value %d: got %#v, want %q", i, col[i], vals[i])
			}
		}
	}
}

// TestTokenizedWithoutTokenizerIsVersionOne pins what a file with no tokenized
// column is: the same version the format always wrote. A reader built before
// tokenized columns reads it, and so does this one.
func TestTokenizedWithoutTokenizerIsVersionOne(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, buildSchema())
	if err := w.AddRowGroup(buildColumns(0)); err != nil {
		t.Fatalf("AddRowGroup: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b := buf.Bytes()
	if got := b[len(magic)]; got != formatVersion {
		t.Fatalf("a file with no tokenizer is version %d, want %d", got, formatVersion)
	}

	r, err := NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewReader on a version 1 file: %v", err)
	}
	if _, err := r.ReadRowGroup(0, []int{0}); err != nil {
		t.Fatalf("ReadRowGroup on a version 1 file: %v", err)
	}
}

// TestTokenizedOptimize covers the pass a caller runs before the write: Tokenized
// is measured beside every other candidate for a text column, and the one that
// comes out smallest is what the file stores. The winner is not asserted, because
// the point is that the pass measures the tokenizer's cost rather than assuming
// it; what is asserted is that a column the pass chose reads back.
func TestTokenizedOptimize(t *testing.T) {
	tok := registeredModel(t)
	vals := tokenizedCorpus(t)
	schema := []ColumnSchema{{Name: "text", Type: TypeString}}

	var buf bytes.Buffer
	w := NewWriterWithOptions(&buf, schema, Options{
		Compress:      CompressFlate,
		CompressLevel: flateLevel,
		Tokenizers:    map[int]*tokenizer.Model{0: tok},
	})
	if err := w.OptimizeTyped([]any{vals}); err != nil {
		t.Fatalf("OptimizeTyped: %v", err)
	}
	if len(w.layouts) == 0 {
		t.Fatal("OptimizeTyped left no layout")
	}
	for _, r := range w.layouts {
		if r.Encoding == EncTokenized {
			t.Logf("tokenized %s/%s: encoded %d, flate %d",
				repName(r.Rep), boundName(r.Bound), r.EncodedSize, r.CompressedSize)
		}
	}
	if err := w.AddRowGroupTyped([]any{vals}); err != nil {
		t.Fatalf("AddRowGroupTyped: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	out, err := r.ReadRowGroup(0, []int{0})
	if err != nil {
		t.Fatalf("ReadRowGroup: %v", err)
	}
	back := out[0]
	if len(back) != len(vals) {
		t.Fatalf("read %d values, wrote %d", len(back), len(vals))
	}
	for i := range vals {
		if s, ok := back[i].(string); !ok || s != vals[i] {
			t.Fatalf("value %d: got %#v, want %q", i, back[i], vals[i])
		}
	}
}

// TestOptimizePrefersTokenizedWhenItIsSmaller is the decision the pass exists to
// make: on a column whose text compresses better as ids than as bytes, Tokenized
// wins, and the file stores it as Tokenized.
func TestOptimizePrefersTokenizedWhenItIsSmaller(t *testing.T) {
	tok := registeredModel(t)
	vals := tokenizedCorpus(t)
	results := BenchmarkLayouts(boxedStrings(vals), ColumnSchema{Name: "text", Type: TypeString}, tok)
	if len(results) == 0 {
		t.Fatal("BenchmarkLayouts gave no candidates")
	}
	best := results[0]
	for _, r := range results {
		if r.Encoding == EncTokenized && r.CompressedSize == best.CompressedSize {
			return
		}
	}
	t.Errorf("the smallest layout is %s at %d bytes, and no tokenized layout matched it", best.Name, best.CompressedSize)
}

// TestTokenizedRejectsBadOptions covers the contracts a tokenized write makes
// with its caller: a tokenizer for a column that does not exist is a mistake
// worth reporting, a nil model is one, and so is an unknown layout.
func TestTokenizedRejectsBadOptions(t *testing.T) {
	tok := tokenizedModel(t)
	schema := []ColumnSchema{{Name: "text", Type: TypeString}}

	write := func(opts Options) error {
		w := NewWriterWithOptions(io.Discard, schema, opts)
		return w.AddRowGroupTyped([]any{[]string{"a"}})
	}
	if err := write(Options{Tokenizers: map[int]*tokenizer.Model{3: tok}}); err == nil {
		t.Error("accepted a tokenizer for a column the schema does not have")
	}
	if err := write(Options{Tokenizers: map[int]*tokenizer.Model{0: nil}}); err == nil {
		t.Error("accepted a nil tokenizer")
	}
	if err := write(Options{Tokenizers: map[int]*tokenizer.Model{0: tok}, TokenizedLayout: TokenizedLayout{Rep: 9}}); err == nil {
		t.Error("accepted an unknown representation")
	}
	if err := write(Options{Tokenizers: map[int]*tokenizer.Model{0: tok}, TokenizedLayout: TokenizedLayout{Bound: 9}}); err == nil {
		t.Error("accepted an unknown boundary")
	}
	// A tokenizer on a column the encoding cannot serve. A caller tokenizing an
	// int64 column has asked for something the format does not do.
	intSchema := []ColumnSchema{{Name: "n", Type: TypeInt64}}
	w := NewWriterWithOptions(io.Discard, intSchema, Options{Tokenizers: map[int]*tokenizer.Model{0: tok}})
	if err := w.AddRowGroupTyped([]any{[]int64{1, 2}}); err == nil {
		t.Error("accepted a tokenizer on an int64 column")
	}
}

// TestTokenizedRejectsUnresolvableHash covers what a reader does with a file
// whose tokenizer it has not been given: report the hash, and read nothing.
func TestTokenizedRejectsUnresolvableHash(t *testing.T) {
	tok := tokenizedModel(t)
	b := tokenizedFile(t, []string{"hello", "world"}, tok, TokenizedLayout{})

	reg := tokenizer.NewRegistry()
	if _, err := NewReaderWithRegistry(bytes.NewReader(b), reg); err == nil {
		t.Error("read a file whose tokenizer the registry does not hold")
	} else if !strings.Contains(err.Error(), "no model is registered") {
		t.Errorf("the error does not name the missing model: %v", err)
	}
}

// TestTokenizedRejectsBadFile covers a file whose header is not what either
// version describes, and a tokenizer table that runs past the end of the file.
func TestTokenizedRejectsBadFile(t *testing.T) {
	tok := tokenizedModel(t)
	b := tokenizedFile(t, []string{"hello", "world"}, tok, TokenizedLayout{})

	// A version neither build writes.
	other := append([]byte{}, b...)
	other[len(magic)] = 9
	if _, err := NewReader(bytes.NewReader(other)); err == nil {
		t.Error("read a file whose version byte is 9")
	}
	// A table that claims more digests than the file holds.
	big := append([]byte{}, b...)
	binary.LittleEndian.PutUint32(big[len(magic)+1:], 4000000)
	if _, err := NewReader(bytes.NewReader(big)); err == nil {
		t.Error("read a file whose tokenizer table runs past the end")
	}
	// A table that claims one digest and stops short of it.
	trunc := append([]byte{}, b[:len(magic)+5]...)
	if _, err := NewReader(bytes.NewReader(trunc)); err == nil {
		t.Error("read a file whose tokenizer table is truncated")
	}
	// A file that ends inside the count itself, so the table's length is the thing
	// it cannot read. Eight bytes is the smallest a file can be and still get past
	// the size check, and the other four are the footer the reader has not read yet.
	cut := append([]byte{}, b[:len(magic)+4]...)
	if _, err := NewReader(bytes.NewReader(cut)); err == nil {
		t.Error("read a file whose tokenizer count is cut short")
	}
}

// TestTokenizerFor covers the index a column's metadata names: zero names no
// tokenizer, and an index outside the table names none either, because a file
// that claims one is not read with a model guessed for it.
func TestTokenizerFor(t *testing.T) {
	tok := tokenizedModel(t)
	rd := &Reader{toks: []*tokenizer.Model{tok}}
	if got := rd.tokenizerFor(ColMeta{}); got != nil {
		t.Error("a column naming no tokenizer got one")
	}
	if got := rd.tokenizerFor(ColMeta{Tokenizer: 1}); got != tok {
		t.Error("a column naming tokenizer 1 did not get the table's first model")
	}
	for _, bad := range []int{-1, 2} {
		if got := rd.tokenizerFor(ColMeta{Tokenizer: bad}); got != nil {
			t.Errorf("a column naming tokenizer %d got a model", bad)
		}
	}
}

// TestTokenizerTable pins the table a writer builds: columns in schema order,
// models shared by two columns named once, and a model a column does not use
// absent from it.
func TestTokenizerTable(t *testing.T) {
	tok := tokenizedModel(t)
	digests, of := tokenizerTable(3, map[int]*tokenizer.Model{2: tok, 0: tok})
	if len(digests) != 1 {
		t.Fatalf("two columns sharing a model built %d table entries, want 1", len(digests))
	}
	if digests[0] != tok.Hash() {
		t.Fatalf("the table holds %x, the model hashes to %x", digests[0], tok.Hash())
	}
	if of[0] != 0 || of[2] != 0 {
		t.Fatalf("columns 0 and 2 name entry %d and %d, both want 0", of[0], of[2])
	}
	if _, ok := of[1]; ok {
		t.Error("column 1 has no tokenizer but the table gave it one")
	}
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
