package tokenizer

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// Encode maps text to token ids. Bytes with no token in the vocab are not
// dropped: each emits the escape id followed by the raw byte, so decoding
// recovers the input exactly. That is the one deliberate deviation from the
// tokenizer's own behaviour, which has no representation for those bytes.
func (m *Model) Encode(text string) []uint16 {
	return m.EncodeBytes([]byte(text))
}

// EncodeBytes is Encode over bytes. Byte boundaries are authoritative here: the
// byte-level mapping is defined on bytes, and a caller handing over text that
// has already been through a UTF-8 encode would otherwise have its byte
// boundaries read back from runes, which is not the same thing.
//
// Encoding cannot fail. Every piece the pre-tokenizer produces is byte-level
// output, so each character maps to a byte the escape can carry.
func (m *Model) EncodeBytes(raw []byte) []uint16 {
	ids := make([]uint16, 0, 64)
	for _, part := range m.splitSpecial(raw) {
		if id, ok := m.special[string(part)]; ok {
			ids = append(ids, id)
			continue
		}
		for _, p := range m.preTokenizeBytes(part) {
			m.encodePiece(p, &ids)
		}
	}
	return ids
}

// EncodeSplit is EncodeBytes with the byte each escape carries held out of the id
// stream: an escape is the escape id alone in ids and its byte in escapes, in the
// order the escapes appear. A storage layer that bit-packs the ids or remaps them
// onto a smaller alphabet cannot carry a raw byte inside a stream of ids, so this
// is the form it works from. ids and the escape markers in it are the same stream
// EncodeBytes produces; only the bytes that followed the markers moved.
func (m *Model) EncodeSplit(raw []byte) (ids []uint16, escapes []byte) {
	inline := m.EncodeBytes(raw)
	for i := 0; i < len(inline); {
		id := inline[i]
		ids = append(ids, id)
		i++
		if id == m.escapeID {
			escapes = append(escapes, byte(inline[i]))
			i++
		}
	}
	return ids, escapes
}

// EscapeID is the id that stands for the raw byte the encoder carried out of the
// id stream. A decoder treats it as a marker rather than as a token.
func (m *Model) EscapeID() uint16 { return m.escapeID }

// DecodeSplit is the inverse of EncodeSplit: escapes supplies the bytes the
// escape ids carry, in the order the markers appear. ids is the stream
// EncodeBytes would have produced with those bytes interleaved back in, so the
// two reach the same text through the same walk.
func (m *Model) DecodeSplit(ids []uint16, escapes []byte) (string, error) {
	inline := make([]uint16, 0, len(ids)+len(escapes))
	e := 0
	for _, id := range ids {
		inline = append(inline, id)
		if id != m.escapeID {
			continue
		}
		if e >= len(escapes) {
			return "", fmt.Errorf("tokenized: escape id with no byte to carry")
		}
		inline = append(inline, uint16(escapes[e]))
		e++
	}
	if e != len(escapes) {
		return "", fmt.Errorf("tokenized: %d escape bytes were never claimed", len(escapes)-e)
	}
	return m.Decode(inline)
}

// splitSpecial walks raw and cuts out every occurrence of a special token. HF
// matches special tokens before any pre-tokenizer stage, so a special token's
// content never reaches the pre-tokenizer; the text between two of them does.
func (m *Model) splitSpecial(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(raw); {
		if n := m.specialLen(raw[i:]); n > 0 {
			if i > start {
				out = append(out, raw[start:i])
			}
			out = append(out, raw[i:i+n])
			i += n
			start = i
			continue
		}
		i++
	}
	if len(raw) > start {
		out = append(out, raw[start:])
	}
	return out
}

// specialLen is the length of the longest special token at the start of raw, or
// 0 when none matches. The longest match is what HF's alternation picks.
func (m *Model) specialLen(raw []byte) int {
	best := 0
	for s := range m.special {
		if len(s) > len(raw) || best >= len(s) {
			continue
		}
		if bytes.HasPrefix(raw, []byte(s)) {
			best = len(s)
		}
	}
	return best
}

// encodePiece applies the BPE walk to one pre-token. A piece that is itself a
// vocab entry is one id and skips the walk.
func (m *Model) encodePiece(piece string, ids *[]uint16) {
	if id, ok := m.vocab[piece]; ok {
		*ids = append(*ids, id)
		return
	}
	*ids = append(*ids, m.bpe(piece)...)
}

// noToken is the symbol for a character the vocab has no token for. It sits
// above every vocab id, so no merge key containing it is ever in the rank
// table: a merge names two tokens, and this character has none.
const noToken uint16 = 0xffff

// sym is one symbol of the merge walk. raw is the byte a noToken character
// stands for, and 0 for a token id.
type sym struct {
	id  uint16
	raw byte
}

// bpe is the merge walk over one piece. Characters the vocab has a token for are
// merged in rank order; a character with no token walks as noToken, which no
// merge can cross, and comes out as an escape carrying its byte.
//
// Escaping happens after the walk, not during it. A byte with no token of its
// own can still be part of a longer token the merges build, so ending the walk
// at one would split tokens that HF keeps whole.
//
// Every character in a piece is byte-level output, so charToByte always finds
// the byte a character stands for here.
func (m *Model) bpe(piece string) []uint16 {
	syms := make([]sym, 0, len(piece))
	for _, r := range piece {
		if id, ok := m.vocab[string(r)]; ok {
			syms = append(syms, sym{id: id})
			continue
		}
		b, _ := charToByte(r)
		syms = append(syms, sym{id: noToken, raw: b})
	}
	syms = m.applyMerges(syms)
	out := make([]uint16, 0, len(syms))
	for _, s := range syms {
		if s.id == noToken {
			out = append(out, m.escapeID, uint16(s.raw))
			continue
		}
		out = append(out, s.id)
	}
	return out
}

// applyMerges walks the symbol list applying the lowest-ranked merge until none
// applies. It rescans after each merge, which is O(n * merges) in the worst
// case; pieces are short, and the scan is over a slice of sym.
func (m *Model) applyMerges(syms []sym) []sym {
	for {
		best := -1
		bestRank := uint16(0)
		found := false
		for i := 0; i+1 < len(syms); i++ {
			rank, ok := m.mergeRank[mergeKey(syms[i].id, syms[i+1].id)]
			if !ok {
				continue
			}
			if !found || rank < bestRank {
				best, bestRank, found = i, rank, true
			}
		}
		if !found {
			return syms
		}
		// The merged symbol is the vocab entry for the two symbols'
		// concatenation, which the model guarantees is present: a merge names
		// the pair and the vocab names the result.
		syms[best].id = m.tokenForPair(syms[best].id, syms[best+1].id)
		syms[best].raw = 0
		syms = append(syms[:best+1], syms[best+2:]...)
	}
}

// tokenForPair is the id of the vocab entry for two symbols' concatenation.
func (m *Model) tokenForPair(a, b uint16) uint16 {
	combined := m.tokens[a] + m.tokens[b]
	return m.vocab[combined]
}

// Decode turns ids back into text. The escape id introduces the raw byte that
// follows it; every other id is looked up as a token, and the byte-level
// characters are mapped back to bytes in one pass.
func (m *Model) Decode(ids []uint16) (string, error) {
	var b strings.Builder
	b.Grow(len(ids) * 4)
	for i := 0; i < len(ids); i++ {
		id := ids[i]
		if id == m.escapeID {
			if i+1 >= len(ids) {
				return "", fmt.Errorf("tokenized: escape id at end of stream")
			}
			i++
			b.WriteByte(byte(ids[i]))
			continue
		}
		if int(id) >= len(m.tokens) {
			return "", fmt.Errorf("tokenized: id %d is outside the vocab", id)
		}
		tok := m.tokens[id]
		for _, r := range tok {
			by, ok := charToByte(r)
			if !ok {
				// A character no byte maps to is text the vocab stores as
				// written rather than as byte-level output, such as a special
				// token's own string.
				b.WriteRune(r)
				continue
			}
			b.WriteByte(by)
		}
	}
	return b.String(), nil
}

// SortedTokens is the vocab in id order, for tests that check storage.
func (m *Model) SortedTokens() []string {
	out := make([]string, len(m.tokens))
	copy(out, m.tokens)
	sort.Strings(out)
	return out
}
