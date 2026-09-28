package tokenizer

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// preTokenizeBytes splits raw bytes into the pieces the BPE walk then merges,
// in the order the tokenizer's Sequence carries them: punctuation runs,
// byte-level word splitting, digit grouping, then isolation of three-digit
// runs.
//
// The model's stages agree on text that is valid UTF-8, which is everything HF
// is defined on. Bytes that are not valid UTF-8 are still carried through: each
// becomes a single logical character of its own, so it round-trips even where
// HF has no answer.
func (m *Model) preTokenizeBytes(raw []byte) []string {
	chars := bytesToChars(raw)
	pieces := splitOnPunct(chars)
	pieces = byteLevelSplit(pieces)
	pieces = groupDigitRuns(pieces)
	pieces = isolateDigitTriples(pieces)
	return pieces
}

// lchar is one logical character: the rune the pre-tokenizer's classes see, and
// the bytes it stands for. A rune decoded from UTF-8 stands for its whole
// encoding; a byte that was not valid UTF-8 stands for itself alone.
type lchar struct {
	r     rune
	bytes []byte
}

// bytesToChars reads raw as logical characters. A byte sequence that is a
// complete rune decodes to that rune and stands for its whole encoding; a byte
// that completes no sequence is a character of its own, whose rune is its
// byte-level image so the byte is preserved where HF has no answer for it.
//
// DecodeRune reports both a valid ASCII byte and an invalid one with size 1, so
// completeness is what separates them.
func bytesToChars(raw []byte) []lchar {
	out := make([]lchar, 0, len(raw))
	for i := 0; i < len(raw); {
		if utf8.FullRune(raw[i:]) {
			r, size := utf8.DecodeRune(raw[i:])
			out = append(out, lchar{r: r, bytes: raw[i : i+size]})
			i += size
			continue
		}
		b := raw[i]
		out = append(out, lchar{r: byteToChar(b), bytes: raw[i : i+1]})
		i++
	}
	return out
}

// splitOnPunct is the Punctuation(Contiguous) stage. A run of punctuation
// characters is one piece; the characters between two runs are another.
//
// Punctuation is the ASCII punctuation characters together with the Unicode P*
// categories. ASCII comes in because a character such as '$' is a symbol in
// Unicode but punctuation to this stage, and a symbol such as ¯ or ≠ is not
// punctuation at all, so it stays with the letters around it and the byte-level
// pattern groups it as the "not a letter, digit or space" alternative — which
// is what lets a leading space and a symbol end up as one token.
func splitOnPunct(chars []lchar) []string {
	var out []string
	var b []lchar
	flush := func() {
		if len(b) > 0 {
			out = append(out, charsText(b))
			b = nil
		}
	}
	inPunct := false
	for _, c := range chars {
		isP := isPunctChar(c.r)
		if len(b) > 0 && isP != inPunct {
			flush()
		}
		inPunct = isP
		b = append(b, c)
	}
	flush()
	return out
}

// isPunctChar is the Punctuation stage's notion of a punctuation character.
func isPunctChar(r rune) bool {
	if r < 0x80 {
		return asciiPunct(r)
	}
	return unicode.IsPunct(r)
}

// asciiPunct reports whether r is one of the ASCII punctuation characters,
// 0x21-0x2f, 0x3a-0x40, 0x5b-0x60 and 0x7b-0x7e.
func asciiPunct(r rune) bool {
	switch {
	case r >= 0x21 && r <= 0x2f:
		return true
	case r >= 0x3a && r <= 0x40:
		return true
	case r >= 0x5b && r <= 0x60:
		return true
	case r >= 0x7b && r <= 0x7e:
		return true
	}
	return false
}

// charsText is the original bytes of a logical character slice, unchanged.
func charsText(chars []lchar) string {
	var sb strings.Builder
	for _, c := range chars {
		sb.Write(c.bytes)
	}
	return sb.String()
}

// byteLevelSplit is the ByteLevel stage. The GPT-2 word pattern runs on the
// piece's characters as they are, and each piece it produces is then mapped to
// byte-level form. Running the pattern before the mapping is what HF does: the
// pattern's \s and \p{L} see real whitespace and letters, and mapping
// afterwards is what turns a space into U+0120 inside a word.
func byteLevelSplit(pieces []string) []string {
	out := make([]string, 0, len(pieces))
	for _, p := range pieces {
		for _, word := range gpt2Words(bytesToChars([]byte(p))) {
			out = append(out, byteLevelText(word))
		}
	}
	return out
}

// byteLevelText is the byte-level form of a logical character slice: every byte
// a character stands for becomes its byte-level character. A character decoded
// from UTF-8 contributes each byte of its encoding, which is what HF does; a
// lone byte contributes itself.
func byteLevelText(chars []lchar) string {
	var sb strings.Builder
	for _, c := range chars {
		for _, b := range c.bytes {
			sb.WriteRune(byteToChar(b))
		}
	}
	return sb.String()
}

// byteToChar is the GPT-2 byte-to-unicode table: a byte maps to itself when it
// is printable ASCII or one of the latin-1 code points the table keeps, and to
// U+0100 and above otherwise, so every byte lands on a distinct character that
// survives a round trip through tools that mangle control bytes.
//
// The bytes that keep their own code point are 0x21-0x7e, 0xa1-0xac and
// 0xae-0xff. Everything else, including 0x20 and 0x7f, moves above U+0100.
func byteToChar(b byte) rune {
	switch {
	case b >= 0x21 && b <= 0x7e:
		return rune(b)
	case b >= 0xa1 && b <= 0xac:
		return rune(b)
	case b >= 0xae && b <= 0xff:
		return rune(b)
	}
	// The remaining bytes are assigned in order from U+0100. The counter for one
	// is its position among the bytes not handled above, which are 0x00-0x20,
	// 0x7f-0xa0 and 0xad: 33 up to 0x21, 34 more from 0x7f, and one at 0xad.
	return rune(0x100 + int(unassignedOffset(b)))
}

// unassignedOffset is the number of bytes below b that the table does not map
// to themselves. It is only called for such a byte: 0x00-0x20, 0x7f-0xa0, or
// 0xad.
func unassignedOffset(b byte) int {
	switch {
	case b <= 0x20:
		return int(b)
	case b <= 0xa0:
		return 0x21 + int(b) - 0x7f
	default:
		// b is 0xad, the one byte in the gap between the two latin-1 ranges.
		// The bytes below it the table does not keep are 0x00-0x20 and
		// 0x7f-0xa0, 33 and 34 of them.
		return 0x21 + 0xa1 - 0x7f
	}
}

// charToByte inverts byteToChar. It returns false for a character no byte maps
// to, which is the decoder's signal that a stored token is not byte-level
// output.
func charToByte(r rune) (byte, bool) {
	switch {
	case r >= 0x21 && r <= 0x7e:
		return byte(r), true
	case r >= 0xa1 && r <= 0xac:
		return byte(r), true
	case r >= 0xae && r <= 0xff:
		return byte(r), true
	case r >= 0x100 && r <= 0x1ff:
		return byte(offsetToUnassigned(int(r - 0x100))), true
	}
	return 0, false
}

// offsetToUnassigned inverts unassignedOffset.
func offsetToUnassigned(off int) int {
	switch {
	case off < 0x21:
		return off
	case off < 0x21+0xa1-0x7f:
		return 0x7f + (off - 0x21)
	default:
		return 0xad
	}
}

// gpt2Words splits text the way the GPT-2 pattern does, on the text's own
// characters rather than on byte-level mapped ones. The pattern is
//
//	's|'t|'re|'ve|'m|'ll|'d| ?\p{L}+| ?\p{N}+| ?[^\s\p{L}\p{N}]+|\s+(?!\S)|\s+
//
// Go's regexp has no lookahead, so the two whitespace alternatives are handled
// directly: \s+(?!\S) takes all but the last character of a whitespace run that
// a non-space character follows, because that character's leading space
// alternative consumes the last one; \s+ takes a run that ends the input.
func gpt2Words(chars []lchar) [][]lchar {
	var out [][]lchar
	i := 0
	for i < len(chars) {
		if n := contractionLen(chars[i:]); n > 0 {
			out = append(out, chars[i:i+n])
			i += n
			continue
		}
		// ' ?' is a literal space, not \s: only an ASCII space is taken as a
		// word's leading character.
		if chars[i].r == ' ' {
			if i+1 < len(chars) && !unicode.IsSpace(chars[i+1].r) {
				n := wordRun(chars, i+1)
				out = append(out, chars[i:i+1+n])
				i += 1 + n
				continue
			}
			// A space followed by space or end of input falls through to the
			// whitespace alternatives, which take the whole run.
		}
		if unicode.IsSpace(chars[i].r) {
			n := spaceRun(chars[i:])
			out = append(out, chars[i:i+n])
			i += n
			continue
		}
		n := wordRun(chars, i)
		out = append(out, chars[i:i+n])
		i += n
	}
	return out
}

// contractionLen is the length of the contraction at the start of chars, or 0
// when there is none. The apostrophe is an ASCII one; the suffixes are the
// pattern's seven.
func contractionLen(chars []lchar) int {
	if len(chars) < 2 || chars[0].r != '\'' {
		return 0
	}
	switch chars[1].r {
	case 's', 't', 'm', 'd':
		return 2
	case 'r', 'v':
		if len(chars) > 2 && chars[2].r == 'e' {
			return 3
		}
		return 0
	case 'l':
		if len(chars) > 2 && chars[2].r == 'l' {
			return 3
		}
		return 0
	}
	return 0
}

// wordRun is the length of the maximal run of one character class starting at
// i, excluding any leading space the caller already consumed. Letters, digits
// and other non-space characters each form their own class, matching the
// pattern's three ' ?' alternatives.
func wordRun(chars []lchar, i int) int {
	class := charClass(chars[i].r)
	j := i
	for j < len(chars) && !unicode.IsSpace(chars[j].r) && charClass(chars[j].r) == class {
		j++
	}
	return j - i
}

// charClass is the three-way split the pattern's middle alternatives make:
// letters, numbers, and everything else that is not whitespace.
func charClass(r rune) int {
	switch {
	case unicode.IsLetter(r):
		return 0
	case unicode.IsNumber(r):
		return 1
	}
	return 2
}

// spaceRun is the length of the whitespace run at the start of chars, under the
// pattern's two whitespace alternatives. When a non-space character follows the
// run, the last character in the run is consumed as that character's leading
// space, so this returns one less than the run's length. When the run ends the
// input, all of it is one piece.
//
// A run of one whitespace character followed by a non-space character is the
// one case where both alternatives yield the same split: \s+(?!\S) cannot match
// without the lookahead failing, and \s+ takes the single character. It still
// has to return at least one, or the caller would not advance.
func spaceRun(chars []lchar) int {
	j := 0
	for j < len(chars) && unicode.IsSpace(chars[j].r) {
		j++
	}
	if j == len(chars) {
		return j
	}
	if j == 1 {
		return 1
	}
	return j - 1
}

// groupDigitRuns is the Digits(individual_digits=false) stage: a run of digits
// is its own piece, and so is the text between digit runs. It runs after
// byte-level splitting, so its job is to pull digit runs out of pieces that
// also hold letters.
func groupDigitRuns(pieces []string) []string {
	out := make([]string, 0, len(pieces))
	for _, p := range pieces {
		out = append(out, splitDigitRuns(p)...)
	}
	return out
}

// splitDigitRuns separates digit runs from the rest of a piece. A digit run
// stays whole; individual_digits=false means the digits are not split one per
// piece at this stage.
func splitDigitRuns(s string) []string {
	var out []string
	var b strings.Builder
	inDigits := false
	for _, r := range s {
		isD := unicode.IsDigit(r)
		if b.Len() > 0 && isD != inDigits {
			out = append(out, b.String())
			b.Reset()
		}
		inDigits = isD
		b.WriteRune(r)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// isolateDigitTriples is the Split([0-9][0-9][0-9]) stage with behavior
// Isolated: every three consecutive digits becomes its own piece, and the
// digits left over are pieces of their own.
func isolateDigitTriples(pieces []string) []string {
	out := make([]string, 0, len(pieces))
	for _, p := range pieces {
		out = append(out, isolateTriples(p)...)
	}
	return out
}

// isolateTriples walks a piece left to right. All-digit pieces are split into
// triples from the start, with a shorter final piece. Pieces that are not all
// digits are untouched, since the pattern only matches digits.
func isolateTriples(s string) []string {
	if !allDigits(s) {
		return []string{s}
	}
	var out []string
	runes := []rune(s)
	for i := 0; i < len(runes); i += 3 {
		end := i + 3
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
