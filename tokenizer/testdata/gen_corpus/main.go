// Command gen_corpus writes a deterministic differential corpus: a stream of
// length-prefixed byte records that the Rust reference oracle and the Go
// tokenizer both encode, so the two outputs can be compared record by record.
//
// The corpus is byte oriented rather than text oriented because the format has
// to store arbitrary bytes, so it carries inputs the HF pipeline cannot accept
// as a &str at all. Those records are still worth encoding: they pin the escape
// that keeps storage lossless where the reference pipeline drops bytes.
//
// Format: uint32 record count, then per record uint32 length and that many bytes.
package main

import (
	"encoding/binary"
	"math/rand"
	"os"
	"strings"
)

const seed = 20260928

func main() {
	var recs [][]byte

	appendStr := func(s string) { recs = append(recs, []byte(s)) }
	appendBytes := func(b []byte) { recs = append(recs, b) }

	for _, s := range fixedRecords {
		appendStr(s)
	}

	rnd := rand.New(rand.NewSource(seed))

	// Prose, punctuation and digits built from a fixed vocabulary, so the corpus
	// is reproducible and still covers the contractions and digit grouping the
	// pre-tokenizer splits on.
	words := strings.Fields(proseVocab)
	for i := 0; i < 400; i++ {
		n := 3 + rnd.Intn(18)
		sb := strings.Builder{}
		for j := 0; j < n; j++ {
			sb.WriteString(words[rnd.Intn(len(words))])
			switch rnd.Intn(6) {
			case 0:
				sb.WriteByte('.')
			case 1:
				sb.WriteByte(',')
			case 2:
				sb.WriteString("'s")
			case 3:
				sb.WriteByte(';')
			case 4:
				sb.WriteByte(':')
			}
			sb.WriteByte(' ')
		}
		appendStr(sb.String())
	}

	// Digits, including runs long enough to exercise the isolated-triple stage.
	for _, n := range []int{1, 2, 3, 4, 5, 6, 7, 9, 12, 20, 40} {
		digits := make([]byte, n)
		for i := range digits {
			digits[i] = byte('0' + rnd.Intn(10))
		}
		appendStr(string(digits))
		appendStr("value " + string(digits) + " units")
		appendStr(string(digits) + "." + string(digits[:n/2]))
	}
	appendStr("1,234,567.89 -42 +0 007 1e10 3.14159265358979")

	for _, s := range []string{
		"https://example.com/a/b?c=d&e=f#frag",
		"http://user:pw@host.example.museum:8080/path",
		"ftp://files.example.org/pub/readme.txt",
		"see https://huggingface.co/tiiuae/falcon-7b for the model",
		"first.last@example.org",
		"root+tag@sub.domain.example",
		"no-reply@mailer.example.com, other@example.net",
		"<html lang=\"en\"><body><p>Hi &amp; bye</p></body></html>",
		"<a href='/x'>link</a> <br/> <img src=\"a.png\" alt=\"\">",
		"<div class=\"a b\" data-id=42>&lt;tag&gt;</div>",
		"<!-- comment --><script>var x = 1 < 2 && 3 > 2;</script>",
		"&amp; &lt; &gt; &quot; &#39; &nbsp; &copy; &unknown;",
	} {
		appendStr(s)
	}

	for _, s := range codeRecords {
		appendStr(s)
	}

	// Unicode: scripts the byte level table maps to codepoints above 0x100, and
	// punctuation and symbols, which split differently because the Punctuation
	// stage matches ASCII punctuation and the Unicode P* categories only.
	for _, s := range unicodeRecords {
		appendStr(s)
	}
	for _, r := range symbolRunes {
		appendStr("a" + string(r) + "b")
		appendStr("word" + string(r) + " word")
	}

	// Whitespace: the GPT-2 pattern's trailing-space cases and the bytes the
	// table maps out of the printable range.
	for _, s := range []string{
		"", " ", "  ", "   ", "\t", "\n", "\r\n", "\r", "\v", "\f",
		"a", " a", "a ", " a ", "  a  ", "a\n", "\na", "a\ta",
		"a \n b", " \t\n\r\v\f ", "a\r\n\r\nb", "\n\n\n", "\t\t\tx",
	} {
		appendStr(s)
	}
	for i := 0; i < 60; i++ {
		sb := strings.Builder{}
		n := 1 + rnd.Intn(12)
		for j := 0; j < n; j++ {
			sb.WriteRune([]rune{' ', '\t', '\n', '\r', '\v', '\f'}[rnd.Intn(6)])
		}
		sb.WriteByte('x')
		appendStr(sb.String())
	}

	// Every byte value, alone and in context. The context cases are what found
	// the merge walk bug where an escaped byte used to absorb a neighbour.
	for b := 0; b < 256; b++ {
		c := byte(b)
		appendBytes([]byte{c})
		appendBytes([]byte{'a', c})
		appendBytes([]byte{c, 'a'})
		appendBytes([]byte{'a', c, 'b'})
		appendBytes([]byte{c, c})
		appendBytes([]byte{'H', 'i', c, 'x', 'y'})
	}

	// Malformed UTF-8: truncated sequences, overlong forms, a surrogate half and
	// a leading continuation byte.
	for _, b := range []byte{
		0xc0, 0xc1, 0xc2, 0xdf, 0xe0, 0xef, 0xf0, 0xf4, 0xf5, 0xf7, 0xf8, 0xfd, 0xfe, 0xff,
	} {
		appendBytes([]byte{b})
		appendBytes([]byte{b, 0x80})
		appendBytes([]byte{'a', b, 0x80, 'b'})
		appendBytes([]byte{0xe0, 0x80})
		appendBytes([]byte{0xf0, 0x80, 0x80})
		appendBytes([]byte{0xed, 0xa0, 0x80})
		appendBytes([]byte{0x80, 'a'})
	}
	appendBytes([]byte("mix\xc3\x28valid\xff\xfeand\xc3\xa9"))
	appendBytes([]byte("\x00\x01\x02\x1e\x1f\x7f\xad\xc0\xc1\xf2\xff"))

	// Long and repeated strings: repeated merges have to stay deterministic.
	for _, s := range []string{
		strings.Repeat("a", 1),
		strings.Repeat("the", 300),
		strings.Repeat(" the quick", 200),
		strings.Repeat("x", 1000),
		strings.Repeat("AbC", 250),
		strings.Repeat("  ", 300),
		strings.Repeat("12345", 100),
		strings.Repeat("https://example.com/x?", 60),
	} {
		appendStr(s)
	}
	for i := 0; i < 100; i++ {
		appendStr("same input every time")
	}

	// Special tokens, which the pipeline matches before pre-tokenization.
	for _, s := range specialRecords {
		appendStr(s)
	}
	for _, s := range specialTokens {
		appendStr(s)
		appendStr("text " + s + " text")
		appendStr(s + s)
		appendStr(s[:len(s)-1])
	}

	out := make([]byte, 0, 1<<20)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(recs)))
	for _, r := range recs {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(r)))
		out = append(out, r...)
	}
	if err := os.WriteFile(os.Args[1], out, 0o644); err != nil {
		panic(err)
	}
}

var specialTokens = []string{
	">>TITLE<<", ">>ABSTRACT<<", ">>INTRODUCTION<<", ">>COMMENT<<",
	">>ANSWER<<", ">>QUESTION<<", ">>SECTION<<", ">>END<<",
	">>OUTPUT<<", ">>NULL<<", ">>DOMAIN<<", ">>USER<<",
}

var specialRecords = []string{
	">>TITLE<<A story", "a >>TITLE<< b", ">>TITLE<<<<ABSTRACT<<",
	">>INTRODUCTION<< text here", "x>>ANSWER<<y", ">> COMMENT <<",
	">>TITLE", "TITLE<<", ">>title<<", ">>END>><<", ">>NULL\x00<<",
}

var codeRecords = []string{
	"package main\n\nfunc main() { x := 1 << 2; println(x) }\n",
	"for (int i = 0; i < n; i++) { a[i] = b[i] + c[i]; }\n",
	"const x = {a: 1, b: [2,3,4], c: \"str\"}; x.a += 2;\n",
	"if (a && b || !c) { return f(x, y) + g(z); }\n",
	"SELECT id, name FROM users WHERE id = 42 ORDER BY name;\n",
	"struct Point { int x, y; }; Point p = {1, 2};\n",
	"#include <stdio.h>\nint main(){printf(\"%d\\n\", 42);return 0;}\n",
	"import (\"fmt\"); fmt.Printf(\"%v\\n\", []int{1,2,3})\n",
	"y = x**2 + 3*x - 1  # a comment\nx //= 2; x %= 3; x <<= 4\n",
	"lambda a, b: a if a > b else b\n",
	"<> </> <!-- --> ${var} #{no} %{pct} {*args} **kwargs\n",
	"a->b->c; a.b.c(); a[0][1]; a&b|c^d~e!f\n",
	"\\n\\t\\\\\\'\\\"\\0\\x1f\\u00e9\\U0001F600\n",
	"    indented\n        more\n\ttabbed\n",
	"line1\r\nline2\r\nline3\n",
	"emoji: 😀🎉👨‍👩‍👧‍👦 test ✓ ✗ → ← ≡ ≠ ≤ ≥",
}

var unicodeRecords = []string{
	"中文测试，这是一个句子。",
	"日本語のテキストです。カタカナひらがな漢字",
	"Привет, мир! Это тест 123.",
	"Ελληνικά γράμματα και αριθμούς 456.",
	"مرحبا بالعالم هذا اختبار 789.",
	"שלום עולם, זהו מבחן.",
	"हिन्दी भाषा का परीक्षण है।",
	"Tiếng Việt có dấu: àáạảã âầấậẩẫ 123.",
	"Português:ação, coração, pão, não, é, 456.",
	"Français: être, où, à, ç, œ, 789.",
	"Deutsch: Größe, Fuß, Maß, ß, äöü.",
	"Español: ¡Hola! ¿Qué tal? ñ, ü, 123.",
	"Czech: Příliš žluťoučký kůň 456.",
	"Polski: zażółć gęślą jaźń 789.",
	"Combining: é ä ō ç ů 123.",
	"Zalgo: á̂̃̄̅̆̇ 456.",
	"Emoji: 😀🚀❤️👍🏽👨‍💻 789 flags: 🇺🇸🇯🇵🇫🇷",
	"Math: ∑∞√≠≈±×÷∫π ½¼¾ ①②③ 456.",
	"Arrows and blocks: ←→↑↓ ↔ ↦ ▲▼ ●○ ■□ 789.",
	"CJK punctuation: 「」『』【】（）〈〉。、・；：",
	"Fullwidth: ＡＢＣ１２３　ｘｙｚ 456.",
	"Supplementary: 𝔥𝔢𝔩𝔩𝔬 𝕎𝕆𝕃𝔽 𝟏𝟐𝟑 𝔸𝔹ℂ.",
	"Linear B and Ogham: 𐀀𐀁𐀂 ᚛᚜ᚑᚌᚐᚋ 789.",
	"Korean: 안녕하세요 한글 테스트 12345.",
	"Thai: สวัสดีชาวโลก นี่คือการทดสอบ 678.",
}

var symbolRunes = []rune{
	'$', '¢', '£', '¤', '¥', '€', '¯', '¬', '¦', '°', '±', 'µ',
	'×', '÷', '≠', '≤', '≥', '∞', '√', '∑', '∫', '∂', '∇', '∆',
	'©', '®', '™', '§', '¶', '†', '‡', '•', '…', '‰', '′', '″',
	'↔', '⇒', '⇐', '→', '←', '♠', '♣', '♥', '♦', '☺', '☻', '☼',
}

var fixedRecords = []string{
	"", " ", "a", "A", "ab", "a b", "abc def ghi",
	"Hello, world!", "The quick brown fox jumps over the lazy dog.",
	"Don't stop believin'", "I'm, I've, I'll, I'd, we're, it's, can't, won't",
	"a's b's c'd e're f'll g've h'm i't",
	"  leading and trailing  ",
	"tabs\tand\ttabs", "new\nlines\nhere", "mixed \t \n \r text",
	"UPPER lower MiXeD CaSe",
	"1234567890", "0", "00", "000", "0000", "00000",
	"1 12 123 1234 12345 123456",
	"abc123def456ghi789",
	"3.14 2.71828 1.41e15 -0.5 +9",
	"punctuation: ,.;:!?\"'()[]{}<>/\\|@#$%^&*-_+=~`",
	"brackets () [] {} <> «»",
	"quotes 'single' \"double\" `back`",
	"dash - en–dash em—dash minus−",
	"slash / \\ // \\\\ a/b a\\b",
	"at@dot.com hash#tag dollar$sign percent%",
	"amp & amp; lt lt; gt gt;",
	"one  two   three    five     spaces",
	"a\n\n\nb", "a \nb", "a\n b", "\n\nstart", "end\n\n",
	"trailing space ", " leading space",
	"xy", "xyz", "xyzzy", "supercalifragilisticexpialidocious",
	"antidisestablishmentarianism pneumonoultramicroscopicsilicovolcanoconiosis",
	"re- re- use pre fix un do able",
	"co-operate co-ordinate re-evaluate de-ice",
	"e-mail web-site on-line off-line",
	"https://a.b", "www.a.b", "a.b.c", "file.txt", "a.tar.gz",
	"x.com/y?z=1&w=2",
	"<a>", "</a>", "<br/>", "<!-->", "<?php?>",
	"0x1F 0xDEADBEEF 0o755 0b1011",
	"rgb(255,128,0) #ff8000 #FFF",
	"2026-09-28T12:34:56Z 2026-09-28 12:34:56",
	"+1 (555) 123-4567 ext. 7890",
	"$1,234.56 €1.234,56 £12.50",
	"1st 2nd 3rd 4th 11th 21st 100th",
	"Ctrl-C Ctrl-V alt+F4 esc",
	"a×b c÷d e=f g≠h",
	"naïve façade reißt Straße Größe",
	"Wikipedia says: 'test 123.'",
	"multiple    spaces\tand\ttabs\n\nnewlines",
	"\x00", "\x00\x00", "a\x00b", "\x7f", "\x1f\x1e\x1d",
	"tab\tat\tend\t",
}

var proseVocab = `the quick brown fox jumps over lazy dog and then ran away into
forest where found small wooden house with red door green windows chimney smoke
rising slowly from top while birds sang songs about distant mountains rivers
flowing through valleys below clouds drifted across sky like ships upon ocean
waves that crashed against rocky shore leaving foam behind each time retreated
back toward horizon where sun was setting colors orange purple pink gold light
spread across landscape illuminating everything its path old man walked road
thinking about days gone past memories flooded mind like water rushing broken
dam could not stop them even if wanted tried focus present moment here now
children played field kicking ball laughing shouting joy pure unfiltered their
voices carried wind reached ears made smile remember when young also played
this very same many years ago nothing changed everything changed all once same
time paradox wrapped inside enigma mystery itself question without answer sought
find meaning words written page between lines spaces silence spoke volumes more
than any sentence ever could express thought feeling emotion raw uncut direct
heart soul whatever name given that place inside which knows truth refuses lie
accept pretense disguise mask worn society expectation conformity pressure conform
break free fly soar above limitations imagined real both alike distinct separate
journey begins single step continues until destination reached begins again cycle
eternal return spiral ascending descending simultaneously chaos order dance
together partners familiar strangers meeting first millionth time recognize face
own reflected mirror window glass transparent solid barrier invisible visible
light shadow two sides coin spinning air neither lands other up down sideways
diagonal every direction none particular choice made unmade remake choose again
different same outcome probability statistics lie truth lies numbers tell stories
stories numbers mathematics language universe speaks fluently learn grammar syntax
vocabulary punctuation marks stops commas pauses breath inhale exhale rhythm
cadence tempo speed slow fast medium pace walk run sprint crawl stand still
motion itself relative observer frame reference point origin coordinates mapped
space time continuum fabric reality threads woven together pattern emerges chaos
noise signal interference static clear channel open closed locked key hidden
plain sight obvious overlook search far wide near close here there everywhere
nowhere particular place moment eternity infinite finite bounds limits stretch
beyond horizon edge world flat round sphere floating void emptiness fullness
contain all nothing zero one binary bits bytes words sentences paragraphs
chapters books libraries universes knowledge wisdom understanding ignorance bliss`
