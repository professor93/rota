package tui

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/wire"
)

// textMeasures says how wide s is to each thing that measures it: Bubble
// Tea's renderer, which counts runes (ansi.StringWidthWc) until a terminal
// says it counts graphemes, then counts graphemes (ansi.StringWidth). A
// terminal that is not asked advances once a rune, as the first does.
func textMeasures(s string) (wc, graphemes int) {
	return ansi.StringWidthWc(s), ansi.StringWidth(s)
}

// textEastAsian is whether this test binary measures ambiguous characters
// as two columns: x/ansi reads RUNEWIDTH_EASTASIAN once, when the binary
// starts, and TestTextBothEastAsianModes runs the binary both ways.
func textEastAsian() bool { return textWidth("…") == 2 }

// textNFD is s with each precomposed Hangul syllable spelled out in
// conjoining letters, as macOS writes Korean file names.
func textNFD(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0xac00 || r > 0xd7a3 {
			b.WriteRune(r)
			continue
		}
		i := r - 0xac00
		b.WriteRune(0x1100 + i/(21*28))
		b.WriteRune(0x1161 + i%(21*28)/28)
		if t := i % 28; t > 0 {
			b.WriteRune(0x11a7 + t)
		}
	}
	return b.String()
}

func TestCleanDropsControlsAndKeys(t *testing.T) {
	for in, want := range map[string]string{
		// A whole escape sequence goes: as a space between two words, and as
		// nothing beside a blank, at either end or after another sequence. A
		// CSI, an SGR, an OSC to its BEL or ST, an ESC pair, the C1 forms.
		"a\x1b[2Jb\nc\x07":     "a b c",
		"\x1b[1;31mred\x1b[0m": "red",
		"is\x1b[2J locked":     "is locked",
		"a\x1b[1m\x1b[2mb":     "a b",
		"a\x1b[1m\x1b[2m":      "a",
		// DCS (a sixel picture), SOS, PM and APC go to their ST or BEL.
		"y\x1bPq#0;2;0;0;0\x1b\\x":                  "y x",
		"y\x1bXsos\x1b\\x":                          "y x",
		"y\x1b^pm\x07x":                             "y x",
		"y\x1b_apc\x1b\\x":                          "y x",
		"y\x1b]0;title\x07x":                        "y x",
		"\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\": "link",
		"\x1b7saved\x1b(B":                          "saved",
		"c1\u009b2Kx":                               "c1 x",
		"c1\u009d2;t\u009cx":                        "c1 x",
		"tab\there":                                 "tab here",
		"crlf\r\nnext":                              "crlf next",
		"del\x7fc1\u0080x":                          "delc1x",
		"nel\u0085line\u2028par\u2029end":           "nel line par end",
		// A byte that is not UTF-8: 0x9b alone is CSI to a terminal that
		// reads 8 bits.
		"bad\x9b2Jx": "bad x",
		"bad\xff":    "bad?",
		// Bidi overrides, a zero width space, a soft hyphen and a BOM are
		// not drawn, and a terminal may reorder or hide by them.
		"\u202eevil\u202c\u200b\u00ad\ufeff": "evil",
		"plain":                              "plain",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	key := "sk-ant-api03-" + strings.Repeat("A", 40)
	for _, in := range []string{"x " + key, "x\n" + key, "x\x07" + key + "\x07", "refused: " + key + "."} {
		got := clean(in)
		if strings.Contains(got, "AAAAAAAAAAAAAAAA") {
			t.Errorf("clean(%q) = %q still shows the key", in, got)
		}
		if !strings.Contains(got, "sk-ant-[not repeated]") {
			t.Errorf("clean(%q) = %q does not say a key was taken out", in, got)
		}
	}
	// A key after an escape sequence is glued to its parameters until the
	// sequence goes, so it is taken out after; and one a sequence splits
	// shows as neither half.
	// An ESC takes the letter after it as its sequence, as a terminal does:
	// here the key's own "s", so no marker is left whole, and no key either.
	if got := clean("x\x1b" + key); strings.Contains(got, "AAAAAAAAAAAAAAAA") {
		t.Errorf("clean(x ESC key) = %q still shows the key", got)
	}
	for _, in := range []string{
		"refused " + key[:21] + "\x1b[0m" + key[21:],
		"x\x1b[1m" + key,
		"x\x1b[1m" + key[:21] + "\x1b[0m" + key[21:],
		"refused " + key[:21] + "\x1b]8;;u\x07" + key[21:],
		"\x1b[2J" + key,
		"\x1b[1m" + key + "\x1b[0m",
		"\x1b]8;;https://example.com\x1b\\" + key + "\x1b]8;;\x1b\\",
		"\x1b]8;;https://example.com\x07" + key + "\x1b]8;;\x07",
		"\u009b2J" + key,
		"\x9b2J" + key,
	} {
		got := clean(in)
		if strings.Contains(got, "AAAAAAAAAAAAAAAA") || !strings.Contains(got, "sk-ant-[not repeated]") {
			t.Errorf("clean(%q) = %q, want the key taken out", in, got)
		}
	}
	// A key split by a character clean drops is whole only once it is
	// dropped, so only the redaction after the dropping takes it out.
	for _, split := range []string{"\x1b", "\u200b", "\u200d", "\ufe0f"} {
		in := "refused sk-ant-api03-" + strings.Repeat("A", 8) + split + strings.Repeat("A", 32)
		if got := clean(in); got != "refused sk-ant-[not repeated]" {
			t.Errorf("clean(%q) = %q, want the key taken out", in, got)
		}
	}
	// Nothing clean gives can move a terminal's cursor or start a sequence.
	for r := rune(0); r < 0x100; r++ {
		got := clean("a" + string(r) + "b")
		for _, c := range got {
			if c < 0x20 || c >= 0x7f && c < 0xa0 {
				t.Errorf("clean of %U keeps control %U: %q", r, c, got)
			}
		}
	}
}

// TestTextRealWordsComeBack: what clean changes to keep every measure
// agreed is the character that needs it, never the text after it, and the
// work stays linear in the text however it is built.
func TestTextRealWordsComeBack(t *testing.T) {
	for in, want := range map[string]string{
		// Korean as macOS writes file names, in conjoining letters, comes
		// back as the syllables it spells.
		textNFD("/Users/kim/프로젝트"): "/Users/kim/프로젝트",
		textNFD("한국어 docs"):        "한국어 docs",
		// A syllable without a final, then a final letter: one syllable.
		"가\u11a8": "각",
		// A letter that would join what follows it into one grapheme is
		// "?", and the text after it stays.
		"\U000111c2refused: the server said no": "?efused: the server said no",
		"\u0d4eabc def ghi":                     "?bc def ghi",
		// A ZWJ trimmed from the end of a grapheme leaves a letter that
		// would join what follows: "?" too, and the text stays.
		"\u0d4e\u200dabc": "?abc",
		// The edges of what composes: an initial past U+1112, a vowel past
		// U+1175 and a final past U+11C2 spell no syllable.
		"\u1113\u1161":   "\u1113",
		"\u1100\u1176":   "\u1100",
		"가\u11c3":        "가",
		"\u1100가나다라 마바사": "?나다라 마바사",
		// These come through whole.
		"Tiếng Việt, Ελληνικά, русский, ไทย, ע\u05b4ב\u05b0ר\u05b4ית": "Tiếng Việt, Ελληνικά, русский, ไทย, ע\u05b4ב\u05b0ר\u05b4ית",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	// Work is counted, not timed: a pass is linear, and a second pass
	// changes nothing, however the text was built.
	for _, c := range []struct {
		name string
		s    string
		kept int // columns left at least
	}{
		{"a Prepend letter, then 64,000 letters", "\u0d4e" + strings.Repeat("a", 64000), 64000},
		{"32,000 Prepend letters, each before a letter", strings.Repeat("\U000111c2a", 32000), 32000},
		{"64,000 Hangul initials, one grapheme", strings.Repeat("\u1100", 64000), 1},
		{"a Hangul initial before each of 32,000 syllables", strings.Repeat("\u1100가", 32000), 32000},
		{"64,000 pictures joined by ZWJ, one grapheme", strings.Repeat("👨\u200d", 64000), 2},
		{"an SGR, then 32,000 key prefixes glued to a letter", "\x1b[0m" + strings.Repeat("xsk-ant-", 32000), 32000},
		{"100,000 SGRs back to back between two letters", "x" + strings.Repeat("\x1b[1m", 100000) + "y", 3},
	} {
		got, passes, looked := textClean(c.s)
		if passes > 2 {
			t.Errorf("%s: %d passes", c.name, passes)
		}
		if looked > 4*len(c.s) {
			t.Errorf("%s: %d bytes of %d looked at again to place joined letters", c.name, looked, len(c.s))
		}
		if textWidth(got) < c.kept {
			t.Errorf("%s: clean kept %d columns, want at least %d", c.name, textWidth(got), c.kept)
		}
	}
	// And the time it takes stays within a generous multiple of the same
	// length of plain ASCII, measured beside it under the same load.
	plain, hostile := strings.Repeat("a", 64000), "\u0d4e"+strings.Repeat("\U000111c2a", 32000)
	best := func(s string) time.Duration {
		b := time.Duration(1 << 62)
		for range 3 {
			start := time.Now()
			clean(s)
			b = min(b, time.Since(start))
		}
		return b
	}
	if p, h := best(plain), best(hostile); h > 50*p+200*time.Millisecond {
		t.Errorf("64,000 hostile characters took %v to clean, 64,000 ASCII %v", h, p)
	}
	// 200,000 sequences back to back share one seam, whose taken characters
	// are joined once, not once a sequence.
	if p, h := best(plain), best("x"+strings.Repeat("\x1b[1m", 200000)+"y"); h > 50*p+200*time.Millisecond {
		t.Errorf("200,000 SGRs took %v to clean, 64,000 ASCII %v", h, p)
	}
}

// textBody is 32 key characters no label holds by chance.
const textBody = "Q7wZr9Lm2Xv8Kp4Tn6Yh3Jc5Bd1Fg0Hs"

// textLeaks is whether out shows 12 or more of textBody's characters in a
// row, once its spaces are taken out.
func textLeaks(out string) bool {
	flat := strings.ReplaceAll(out, " ", "")
	for i := 0; i+12 <= len(textBody); i++ {
		if strings.Contains(flat, textBody[i:i+12]) {
			return true
		}
	}
	return false
}

// TestTextKeysInEveryArrangement: a key glued to a sequence, split by one,
// with a character of its prefix taken by one, or carried inside one, never
// shows its body.
func TestTextKeysInEveryArrangement(t *testing.T) {
	key := "sk-ant-api03-" + textBody
	b1, b2 := key[:21], key[21:]
	for _, c := range []struct{ name, in string }{
		{"whole", "x " + key},
		// A dropped format character stands between words, as the control
		// it is: a key glued after one still starts a word.
		{"glued after ZWSP", "x\u200b" + key},
		{"glued after VS16", "x\ufe0f" + key},
		{"glued after a soft hyphen", "x\u00ad" + key},
		{"glued after a bidi mark", "x\u200e" + key},
		// A key a dropped control splits is read whole, not taken out up
		// to the control with the rest left showing.
		{"split by BEL past 16 body characters", "\x07" + key[:28] + "\x07" + key[28:]},
		{"split twice, by ZWSP and BEL", "x " + key[:23] + "\u200b" + key[23:33] + "\x07" + key[33:]},
		{"split twice, by VS16 and DEL", "x " + key[:20] + "\ufe0f" + key[20:30] + "\x7f" + key[30:]},
		// A sequence that leaves no space still stands between words, so a
		// key right after it starts there, and one sequence inside the
		// prefix after it is read through.
		{"an SGR at the start, ESC inside the prefix", "\x1b[1msk-\x1bant-api03-" + textBody + "\x1b[1m"},
		{"an SGR after a space, raw CSI inside the prefix", " \x1b[1msk-an\x9bt-api03-" + textBody},
		{"an OSC at the start, CSI intermediate in the prefix", "\x1b]8;;u\x07sk-ant\x1b[-api03-" + textBody},
		// Beside a blank or at an end a sequence leaves no space, only its
		// seam.
		{"ESC takes the s after a space", "x \x1b" + key},
		{"ESC takes the s at the start", "\x1b" + key},
		{"split by CSI, a CSI at the end", "x " + b1 + "\x1b[0m" + b2 + "\x1b[0m"},
		{"split by CSI beside a space", "x \x1b[1m" + b1 + "\x1b[0m" + b2},
		{"CSI glued", "\x1b[2J" + key},
		{"split by CSI", "x " + b1 + "\x1b[0m" + b2},
		{"split by BEL", "x " + b1 + "\x07" + b2},
		{"split by ESC", "x " + b1 + "\x1b" + b2},
		{"ESC takes the s, whole key", "x\x1b" + key},
		{"ESC takes the s, split by BEL", "x\x1b" + b1 + "\x07" + b2},
		{"ESC takes the s, split by CSI", "x\x1b" + b1 + "\x1b[0m" + b2},
		{"ESC inside the prefix", "refused sk-\x1bant-api03-" + textBody},
		{"ESC - a inside the prefix", "refused sk-ant\x1b-api03-" + textBody},
		{"CSI final takes the s", "x\x1b[" + b1 + "\x07" + b2},
		{"CSI intermediate takes - and a", "refused sk-ant\x1b[-api03-" + textBody},
		{"C1 CSI takes the s, split", "x\u009b" + b1 + "\x07" + b2},
		{"raw 0x9b takes the s, split", "x\x9b" + b1 + "\x07" + b2},
		{"ESC in the prefix and CSI in the body", "refused sk-\x1bant-api03-" + textBody[:8] + "\x1b[1;2m" + textBody[8:]},
		{"OSC nested in a CSI", "x " + b1 + "\x1b[\x1b]8;;u\x07m" + b2},
		{"a sequence between every character", "x " + strings.Join(strings.Split(key, ""), "\x1b[0m")},
		{"OSC without its end before the key", "x\x1b]8;;" + key},
		{"OSC without its end inside the key", "x " + b1 + "\x1b]8;;" + b2},
		{"a sequence longer than the text", "\x1b[" + strings.Repeat("1;", 200) + key},
		{"the key as an OSC 8 link's text", "\x1b]8;;https://e.com\x1b\\" + key + "\x1b]8;;\x1b\\"},
		{"the key in an OSC 8 link's URL", "\x1b]8;;https://e.com/?k=" + key + "\x1b\\link\x1b]8;;\x1b\\"},
		{"DCS holding the key, ST as C1", "\u0090q" + key + "\u009cafter"},
		{"ZWSP inside the prefix", "refused sk-​ant-api03-" + textBody},
		{"VS16 inside the prefix", "refused sk-️ant-api03-" + textBody},
	} {
		if out := clean(c.in); textLeaks(out) {
			t.Errorf("%s: clean(%q) = %q shows the key", c.name, c.in, out)
		}
	}
}

// TestTextKeyFuzzLeaksNothingNew: of 20,000 random arrangements of a key
// (textKeyFuzz, a fixed seed), none shows its body that clean at da8b9bb
// took out, and none shows 12 of its body characters after a marker.
// testdata/text-key-fuzz-da8b9bb.txt lists the ones that showed there: a
// key glued to a letter, which wire.Unsecret leaves by design, a key split
// before a control after enough of it to be taken out by itself, and the
// residue clean's comment names.
func TestTextKeyFuzzLeaksNothingNew(t *testing.T) {
	data, err := os.ReadFile("testdata/text-key-fuzz-da8b9bb.txt")
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]bool{}
	for _, f := range strings.Fields(string(data)) {
		before[f] = true
	}
	const marker = "sk-ant-[not repeated]"
	leaks := 0
	for i, in := range textKeyFuzz(20000) {
		out := clean(in)
		if textLeaks(out) {
			leaks++
			if !before[strconv.Itoa(i)] {
				t.Errorf("arrangement %d, %q, shows the key: %q", i, in, out)
			}
		}
		// What a key was taken out of never shows the rest of it after
		// the marker.
		for rest := out; strings.Contains(rest, marker); {
			rest = rest[strings.Index(rest, marker)+len(marker):]
			if textLeaks(rest) {
				t.Errorf("arrangement %d, %q, shows the key after its marker: %q", i, in, out)
				break
			}
		}
		if again := clean(out); again != out {
			t.Errorf("arrangement %d: clean(%q) = %q, and clean of that is %q", i, in, out, again)
		}
	}
	if leaks > len(before) {
		t.Errorf("%d arrangements show the key, %d did at da8b9bb", leaks, len(before))
	}
	t.Logf("%d of 20,000 arrangements show the key; %d did at da8b9bb", leaks, len(before))
}

// textKeyFuzz is n random arrangements of a key, from a fixed seed: up to
// three pieces of junk (sequences, cut-short sequences, controls, ZWSP,
// VS16) inside the key, outside its "sk-ant-" when there is more than one,
// with context before and after.
func textKeyFuzz(n int) []string {
	key := "sk-ant-api03-" + textBody
	junk := []string{"\x1b[0m", "\x1b[1;31m", "\x1b", "\x07", "\x1b]8;;u\x07", "\x1b]8;;u\x1b\\", "\u009b2K", "\x9b", "\x1b(B", "\u200b", "\ufe0f", "\x1b[", "\x1b]"}
	ctx := []string{"", " ", "x", "x ", "\x1b[1m", " \x1b[1m", "x\x1b[1m", "\x1b", "\x1b]8;;u\x07", "\x07"}
	r := rand.New(rand.NewPCG(11, 13))
	out := make([]string, 0, n)
	for range n {
		var b strings.Builder
		b.WriteString(ctx[r.IntN(len(ctx))])
		pos := map[int]string{}
		pieces := r.IntN(4)
		for range pieces {
			p := r.IntN(len(key) + 1)
			if pieces > 1 && p < 8 {
				p = 8 + r.IntN(len(key)-7)
			}
			pos[p] += junk[r.IntN(len(junk))]
		}
		for p := 0; p <= len(key); p++ {
			b.WriteString(pos[p])
			if p < len(key) {
				b.WriteByte(key[p])
			}
		}
		b.WriteString(ctx[r.IntN(len(ctx))])
		out = append(out, b.String())
	}
	return out
}

// TestTextKeyResidue pins what clean's comment says it does not stop: two
// sequences inside "sk-ant-", one taking a character of it and the other
// putting characters of its own in. Should these ever be taken out, change
// the comment, then this test.
func TestTextKeyResidue(t *testing.T) {
	key := "sk-ant-api03-" + textBody
	for _, in := range []string{
		"x \x1b" + key[:1] + "\x1b[0m" + key[1:],
		"x \x1b[" + key[:2] + "\x1b[1;2m" + key[2:],
		"x \u009b" + key[:4] + "\x1b]8;;u\x07" + key[4:],
	} {
		if out := clean(in); !textLeaks(out) {
			t.Errorf("clean(%q) = %q: the residue clean's comment names is taken out now", in, out)
		}
	}
}

// TestTextRedactIsWireUnsecret: textRedact's reading of a key is
// wire.Unsecret's, on labels built at random with a fixed seed from what a
// key and its edges are made of. Should wire's shape change, this fails.
func TestTextRedactIsWireUnsecret(t *testing.T) {
	alpha := []string{"s", "k", "-", "a", "n", "t", "_", "A", "z", "9", " ", ".", "é", "sk-ant-", "sk-ant-api03-", "AAAAAAAA", "/", "\t"}
	r := rand.New(rand.NewPCG(7, 9))
	for range 50000 {
		var b strings.Builder
		for n := r.IntN(14); n >= 0; n-- {
			b.WriteString(alpha[r.IntN(len(alpha))])
		}
		s := b.String()
		got, _ := textRedact(" "+s, []textSeam{{at: 0, space: true}})
		if want := " " + wire.Unsecret(s); got != want {
			t.Fatalf("textRedact(%q) = %q, wire.Unsecret gives %q", s, got, want)
		}
	}
}

// TestTextEscapesNeverLeaveAControl: labels built at random with a fixed
// seed from escape sequences, their pieces, C1 controls, bytes that are not
// UTF-8 and the characters that make measures disagree come out with no
// control, clean already, and as wide to the renderer as by graphemes.
func TestTextEscapesNeverLeaveAControl(t *testing.T) {
	alpha := []string{"\x1b", "[", "]", "P", "X", "^", "_", "\\", "\x07", "\u009b", "\x9b", "\u009d", "\u009c", "\x9c", "\u0090",
		"0", "1", ";", "?", " ", "-", "m", "s", "k", "a", "n", "t", "A", "sk-ant-", "api03-", "AAAAAAAA", "AAAAAAAAAAAAAAAA",
		"é", "日", "‍", "️", "\xff", "\x00", "\n", "(", "B", "7", "ൎ", "ᄀ", "ᅡ", "👨"}
	r := rand.New(rand.NewPCG(3, 4))
	for range 50000 {
		var b strings.Builder
		for n := r.IntN(16); n >= 0; n-- {
			b.WriteString(alpha[r.IntN(len(alpha))])
		}
		in := b.String()
		out := clean(in)
		for _, c := range out {
			if c < 0x20 || c >= 0x7f && c < 0xa0 {
				t.Fatalf("clean(%q) = %q keeps control %U", in, out, c)
			}
		}
		if again := clean(out); again != out {
			t.Fatalf("clean(%q) = %q, and clean of that is %q", in, out, again)
		}
		if wc, g := textMeasures(out); wc != g {
			t.Fatalf("clean(%q) = %q is %d wide to the renderer and %d by graphemes", in, out, wc, g)
		}
	}
}

// textWide is labels whose width the measures disagree on, or might, and
// how wide clean leaves them. Each width is also the one rota's own tables
// give the cleaned text (cmd/rota/width.go's screenWidth, checked against
// it on 2026-11-10 for every code point, each with fifteen marks and
// joiners after it, and 7.6 million joined pairs), so a label is as wide in
// the TUI as it is in `rota list`. The widths are for ambiguous characters
// drawn narrow, as rota's tables count them.
var textWide = []struct {
	name, in, out string
	width         int
}{
	{"CJK", "日本語のアカウント", "日本語のアカウント", 18},
	{"ZWJ family", "👨\u200d👩\u200d👧 family", "👨 family", 9},
	{"ZWJ heart on fire", "❤\u200d🔥", "❤", 1},
	{"ZWJ rainbow flag", "🏳\ufe0f\u200d🌈", "🏳", 1},
	{"ZWJ with a tone", "👩\U0001f3fd\u200d💻 dev", "👩 dev", 6},
	{"VS16 heart", "❤\ufe0f x", "❤ x", 3},
	{"VS16 keycap", "1\ufe0f\u20e3", "1\u20e3", 1},
	{"VS16 on text", "a\ufe0f", "a", 1},
	{"VS16 sun", "☀\ufe0f", "☀", 1},
	{"skin tone", "👍\U0001f3fd", "👍", 2},
	{"flag", "🇯🇵 jp", "🇯🇵 jp", 5},
	{"half a flag", "🇯 x", "? x", 3},
	{"combining acute", "e\u0301te\u0301", "e\u0301te\u0301", 3},
	{"stacked marks", "x\u0308\u0301", "x\u0308\u0301", 1},
	{"Hangul spelled out", "\u1112\u1161\u11ab", "한", 2},
	{"Hangul syllable", "한국", "한국", 4},
	{"Hangul vowel alone", "x\u1161", "x?", 2},
	{"a ZWJ joining nothing", "↑\u200d👩", "↑👩", 3},
	{"ZWNJ", "a\u200cb", "a\u200cb", 2},
	{"unassigned", "a\u0378b", "a?b", 3},
	{"Yijing trigram, wide since Unicode 16", "\u2630 x", "? x", 3},
	{"Yijing digram, wide since Unicode 16", "\u268a x", "? x", 3},
	{"Tai Xuan Jing, wide since Unicode 16", "\U0001d300 x", "? x", 3},
	{"Tangut component", "\U00018d80 x", "? x", 3},
	{"private use", "\ue000", "\ue000", 1},
	{"a mark with nothing under it", "\u0301x", "x", 1},
	{"a mark after what takes no mark", "a\u2065\ufe0f", "a?", 2},
	{"emoji alone", "✅ ok", "✅ ok", 5},
	{"bars", "▕█░│▏", "▕█░│▏", 5},
}

// TestFitCountsColumnsNotBytes is Review Focus 1: a label is as wide to fit
// as to the renderer and the terminal, whichever way they count, and fit
// never draws past its width, with ambiguous characters narrow or wide.
func TestFitCountsColumnsNotBytes(t *testing.T) {
	wide := textEastAsian()
	got := fit("日本語ab", 5)
	if w := textWidth(got); w != 5 {
		t.Errorf("fit(日本語ab, 5) = %q is %d wide", got, w)
	}
	if !strings.HasSuffix(strings.TrimRight(got, " "), "…") {
		t.Errorf("fit(日本語ab, 5) = %q does not end in …", got)
	}
	cut, one := "日… ", "…"
	if wide {
		cut, one = "日…", "a"
	}
	if got := fit("日本語", 4); got != cut {
		t.Errorf("fit(日本語, 4) = %q, want %q: the wide character the edge would halve left out whole", got, cut)
	}
	if got := fit("ab", 5); got != "ab   " {
		t.Errorf("fit(ab, 5) = %q, want it padded", got)
	}
	if got := fit("abcdef", 1); got != one {
		t.Errorf("fit(abcdef, 1) = %q, want %q", got, one)
	}
	if got := fit("abc", 0); got != "" {
		t.Errorf("fit(abc, 0) = %q", got)
	}
	// An emoji with ZWJ counts as 2: clean leaves its first picture, which
	// every measure counts as 2.
	fam := clean("👨\u200d👩\u200d👧")
	if wc, g := textMeasures(fam); wc != 2 || g != 2 {
		t.Errorf("the family after clean is %q, %d wide to the renderer and %d by graphemes, want 2 and 2", fam, wc, g)
	}
	for _, c := range textWide {
		got := clean(c.in)
		if !wide && got != c.out {
			t.Errorf("%s: clean(%q) = %q, want %q", c.name, c.in, got, c.out)
		}
		wc, g := textMeasures(got)
		if wc != g || !wide && wc != c.width {
			t.Errorf("%s: %q is %d wide to the renderer and %d by graphemes, want %d", c.name, got, wc, g, c.width)
		}
		for w := 0; w <= wc+2; w++ {
			f := fit(got, w)
			if wc, g := textMeasures(f); wc != w || g != w {
				t.Errorf("%s: fit(%q, %d) = %q is %d wide to the renderer and %d by graphemes", c.name, got, w, f, wc, g)
			}
		}
	}
}

// TestTextEveryCharacterMeasuresOneWayAfterClean: whatever a server puts in
// a label, the renderer and a terminal counting graphemes agree on what
// clean leaves of it, so neither shifts the next column.
func TestTextEveryCharacterMeasuresOneWayAfterClean(t *testing.T) {
	marks := []string{"", "\ufe0f", "\ufe0e", "\u0301", "\U0001f3fd", "\u200d\U0001f469", "\u20e3", "\U0001f1ef",
		"\u200d❤", "\u200d", "\ufe0f\u200d\U0001f525", "\U0001f3fd\u200d\U0001f4bb", "a"}
	for r := rune(0); r <= 0x3ffff; r++ {
		if r >= 0xd800 && r <= 0xdfff || r >= 0x32000 && r < 0x3ffff {
			continue
		}
		for _, m := range marks {
			if m != "" && !(r >= 0x2000 && r < 0x3000 || r >= 0x1f000 && r < 0x20000 || r < 0x80) {
				continue
			}
			s := clean(string(r) + m)
			if wc, g := textMeasures(s); wc != g {
				t.Fatalf("clean(%U + %q) = %q is %d wide to the renderer and %d by graphemes", r, m, s, wc, g)
			}
			if again := clean(s); again != s {
				t.Fatalf("clean(%U + %q) = %q, and clean of that is %q", r, m, s, again)
			}
			if !utf8.ValidString(s) {
				t.Fatalf("clean(%U + %q) = %q is not UTF-8", r, m, s)
			}
		}
	}
	// Two pictures joined, as a server might join anything.
	var pics []rune
	for r := rune(0x2000); r < 0x2c00; r += 29 {
		pics = append(pics, r)
	}
	for r := rune(0x1f000); r < 0x1fb00; r += 17 {
		pics = append(pics, r)
	}
	for _, a := range pics {
		for _, z := range pics {
			for _, j := range []string{"\u200d", "\ufe0f\u200d", "\U0001f3fb\u200d"} {
				s := clean(string(a) + j + string(z))
				if wc, g := textMeasures(s); wc != g {
					t.Fatalf("clean(%U %q %U) = %q is %d wide to the renderer and %d by graphemes", a, j, z, s, wc, g)
				}
			}
		}
	}
}

func TestTextWrapKeepsEveryLineInItsWidth(t *testing.T) {
	got := wrap("the account was refused by the server", 12)
	want := []string{"the account", "was refused", "by the", "server"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrap = %q, want %q", got, want)
	}
	long := wrap("日本語のアカウントです abcdefghijklmnop", 7)
	for _, l := range long {
		if textWidth(l) > 7 {
			t.Errorf("wrap gave %q, %d wide", l, textWidth(l))
		}
	}
	if strings.Join(long, "") != "日本語のアカウントですabcdefghijklmnop" {
		t.Errorf("wrap lost or split a character: %q", long)
	}
	if got := wrap("", 10); len(got) != 0 {
		t.Errorf("wrap of nothing = %q", got)
	}
	if got := wrap("abc", 0); len(got) != 0 {
		t.Errorf("wrap into no width = %q", got)
	}
	if got := wrap("one\ntwo", 10); strings.Join(got, "|") != "one|two" {
		t.Errorf("wrap of two lines = %q", got)
	}
	// A character wider than the line takes a line of its own, and the
	// wrap goes on past it.
	if got := wrap("日本", 1); strings.Join(got, "|") != "日|本" {
		t.Errorf("wrap(日本, 1) = %q", got)
	}
}

func TestTextTimeLeftCountsUp(t *testing.T) {
	now := time.Date(2026, 11, 10, 20, 17, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		-time.Second:                    "no time",
		0:                               "no time",
		time.Millisecond:                "1s",
		59 * time.Second:                "59s",
		time.Minute:                     "1m",
		time.Minute + time.Second:       "2m",
		12 * time.Minute:                "12m",
		11*time.Minute + 30*time.Second: "12m",
	} {
		if got := timeLeft(now.Add(d), now); got != want {
			t.Errorf("timeLeft(now+%v) = %q, want %q", d, got, want)
		}
	}
}

// textModeVar names the mode a child run of TestTextBothEastAsianModes was
// started in: "narrow" or "wide".
const textModeVar = "ROTA_TUI_TEXT_MODE"

// TestTextBothEastAsianModes runs the width tests again in a test binary of
// their own, in the mode this run is not: with RUNEWIDTH_EASTASIAN=1, the
// supported setting for a terminal that draws ambiguous characters wide, or
// without it. x/ansi reads the variable as the binary starts, so only a new
// process changes the measure, and this run covers its own mode. The child
// is this binary, started by its own path, with PATH still the empty
// directory TestMain made, so it finds no CLI.
func TestTextBothEastAsianModes(t *testing.T) {
	if os.Getenv(textModeVar) != "" {
		t.Skip("a child run")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := "^(TestTextModeIsTheOneAsked|TestTextRandomLabelsMeasureOneWay|TestFitCountsColumnsNotBytes|TestTheBarTicksAtTheThreshold|TestCleanDropsControlsAndKeys|TestTextRealWordsComeBack|TestTextWrapKeepsEveryLineInItsWidth|TestTextEveryCharacterMeasuresOneWayAfterClean)$"
	// This run measures in one mode already; the child measures in the
	// other.
	mode := "wide"
	if textEastAsian() {
		mode = "narrow"
	}
	{
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "RUNEWIDTH_EASTASIAN=") && !strings.HasPrefix(kv, textModeVar+"=") && !strings.HasPrefix(kv, "PATH=") {
				env = append(env, kv)
			}
		}
		env = append(env, textModeVar+"="+mode, "PATH="+os.Getenv("PATH"))
		if mode == "wide" {
			env = append(env, "RUNEWIDTH_EASTASIAN=1")
		}
		cmd := exec.Command(exe, "-test.run", tests, "-test.count=1", "-test.v", "-test.timeout=20m")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the %s run failed: %v\n%s", mode, err, out)
		}
		if !strings.Contains(string(out), "--- PASS: TestTextModeIsTheOneAsked") {
			t.Errorf("the %s run did not measure in its mode:\n%s", mode, out)
		}
	}
}

// TestTextModeIsTheOneAsked is the check a child run of
// TestTextBothEastAsianModes makes that it measures as it was asked to.
func TestTextModeIsTheOneAsked(t *testing.T) {
	switch os.Getenv(textModeVar) {
	case "":
		t.Skip("run by TestTextBothEastAsianModes")
	case "wide":
		if !textEastAsian() {
			t.Fatal("asked for ambiguous characters wide, and … is one column")
		}
	default:
		if textEastAsian() {
			t.Fatal("asked for ambiguous characters narrow, and … is two columns")
		}
	}
}

// TestTextWidthGoIsCopied: the copy of width.go's measure at the end of this
// file is width.go's, declaration for declaration, but for its names and
// comments.
func TestTextWidthGoIsCopied(t *testing.T) {
	decls := func(path string, keep func(string) bool, rename func(string) string) map[string]string {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if keep(d.Name.Name) {
					names[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					if v, ok := sp.(*ast.ValueSpec); ok && keep(v.Names[0].Name) {
						names[v.Names[0].Name] = true
					}
				}
			}
		}
		out := map[string]string{}
		for _, d := range f.Decls {
			name := ""
			switch d := d.(type) {
			case *ast.FuncDecl:
				name = d.Name.Name
				d.Doc = nil
			case *ast.GenDecl:
				if v, ok := d.Specs[0].(*ast.ValueSpec); ok {
					name = v.Names[0].Name
				}
				d.Doc = nil
			}
			if !names[name] {
				continue
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && names[id.Name] {
					id.Name = rename(id.Name)
				}
				return true
			})
			var b bytes.Buffer
			if err := printer.Fprint(&b, token.NewFileSet(), d); err != nil {
				t.Fatal(err)
			}
			out[rename(name)] = b.String()
		}
		return out
	}
	measure := map[string]bool{"screenWidth": true, "char": true, "regional": true, "tone": true,
		"picture": true, "pictograph": true, "runeWidth": true, "jamo": true, "wide": true}
	theirs := decls("../width.go", func(n string) bool { return measure[n] },
		func(n string) string { return "textGo" + strings.ToUpper(n[:1]) + n[1:] })
	ours := decls("text_test.go", func(n string) bool { return strings.HasPrefix(n, "textGo") },
		func(n string) string { return n })
	if len(theirs) != len(measure) {
		t.Fatalf("width.go has %d of the measure's %d declarations", len(theirs), len(measure))
	}
	for name, src := range theirs {
		if ours[name] != src {
			t.Errorf("%s differs from width.go's:\n%s\nwant:\n%s", name, ours[name], src)
		}
	}
	if len(ours) != len(theirs) {
		t.Errorf("the copy has %d declarations, width.go's measure %d", len(ours), len(theirs))
	}
}

// TestTextRandomLabelsMeasureOneWay builds labels at random, with a fixed
// seed, from the characters that make measures disagree: Prepend letters,
// conjoining jamo and their edges, the joiners and variation selectors,
// emoji, flags, skin tones, a keycap, marks, Devanagari, halfwidth kana,
// controls, CJK and unassigned code points. Whatever clean makes of each is
// clean already, as wide to the renderer as by graphemes and as to rota's
// own tables, valid UTF-8, done in at most two passes, and loses no ASCII
// letter but the one a joining letter before it takes.
func TestTextRandomLabelsMeasureOneWay(t *testing.T) {
	alpha := []string{"a", "b", " ", "x", "\u0d4e", "\U000111c2", "\U00011a84", "\u1100", "\u1112", "\u1161",
		"\u1175", "\u11a8", "\u11a7", "\u11c2", "가", "한", "\u200d", "\u200c", "\ufe0f", "\ufe0e", "👨", "👩", "❤", "🏳",
		"🌈", "\U0001f3fd", "\U0001f1ef", "\U0001f1f5", "\u20e3", "1", "\u0301", "\u0308", "न", "\u094d", "\u093f",
		"द", "\u0940", "ｶ", "\uff9e", "\x1b", "\u200b", "\u0600", "日", "\u2630", "\u0378", "\u3099", "か"}
	joins := func(s string) int {
		n := 0
		for _, r := range s {
			// A joining letter, and an ESC, which takes what follows it as
			// the rest of its sequence.
			if r == 0x0d4e || r == 0x111c2 || r == 0x11a84 || r == 0x0600 || r == 0x1b {
				n++
			}
		}
		return n
	}
	letters := func(s string) int { return strings.Count(s, "a") + strings.Count(s, "b") + strings.Count(s, "x") }
	r := rand.New(rand.NewPCG(1, 2))
	for range 50000 {
		var b strings.Builder
		for range 1 + r.IntN(12) {
			b.WriteString(alpha[r.IntN(len(alpha))])
		}
		in := b.String()
		out, passes, _ := textClean(in)
		wc, g := textMeasures(out)
		switch {
		case clean(out) != out:
			t.Fatalf("clean(%q) = %q, and clean of that is %q", in, out, clean(out))
		case wc != g:
			t.Fatalf("clean(%q) = %q is %d wide to the renderer and %d by graphemes", in, out, wc, g)
		case !textEastAsian() && wc != textGoScreenWidth(out):
			t.Fatalf("clean(%q) = %q is %d wide to the renderer and %d to rota's tables", in, out, wc, textGoScreenWidth(out))
		case passes > 2:
			t.Fatalf("clean(%q) took %d passes", in, passes)
		case !utf8.ValidString(out):
			t.Fatalf("clean(%q) = %q is not UTF-8", in, out)
		case letters(out) < letters(in)-joins(in):
			t.Fatalf("clean(%q) = %q lost letters", in, out)
		}
	}
}

// What follows is cmd/rota/width.go's measure, the one rota's tables use,
// copied with its names given this file's stem: package main cannot be
// imported. TestTextWidthGoIsCopied fails the moment the two differ.

func textGoScreenWidth(s string) int {
	r := []rune(s)
	n := 0
	for i := 0; i < len(r); {
		end, w := textGoChar(r, i)
		n, i = n+w, end
	}
	return n
}

func textGoChar(r []rune, i int) (end, w int) {
	w = textGoRuneWidth(r[i])
	flag := textGoRegional(r[i])
	for end = i + 1; end < len(r); end++ {
		c := r[end]
		switch {
		case flag && textGoRegional(c):
			flag, w = false, 2
		case c == '\ufe0f':
			if w == 1 && textGoPicture(r[i]) {
				w = 2
			}
		case textGoTone(c) && textGoPictograph(r[i]):
			w = 2
		case r[end-1] == '\u200d' && textGoPictograph(c) && textGoPictograph(r[i]):
			w = 2
		case textGoRuneWidth(c) == 0:
		default:
			return end, w
		}
	}
	return end, w
}

func textGoRegional(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }

func textGoTone(r rune) bool { return r >= 0x1f3fb && r <= 0x1f3ff }

func textGoPicture(r rune) bool {
	return textGoPictograph(r) || strings.ContainsRune("#*0123456789©®‼⁉", r)
}

func textGoPictograph(r rune) bool {
	return r >= 0x2100 && r < 0x2c00 || r >= 0x1f000 && r < 0x20000
}

func textGoRuneWidth(r rune) int {
	switch {
	case r < 0x300:
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, textGoJamo):
		return 0
	case unicode.Is(textGoWide, r):
		return 2
	}
	return 1
}

var textGoJamo = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x1160, Hi: 0x11ff, Stride: 1},
		{Lo: 0xd7b0, Hi: 0xd7ff, Stride: 1},
	},
}

var textGoWide = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x1100, Hi: 0x115f, Stride: 1},
		{Lo: 0x231a, Hi: 0x231b, Stride: 1},
		{Lo: 0x2329, Hi: 0x232a, Stride: 1},
		{Lo: 0x23e9, Hi: 0x23ec, Stride: 1},
		{Lo: 0x23f0, Hi: 0x23f0, Stride: 1},
		{Lo: 0x23f3, Hi: 0x23f3, Stride: 1},
		{Lo: 0x25fd, Hi: 0x25fe, Stride: 1},
		{Lo: 0x2614, Hi: 0x2615, Stride: 1},
		{Lo: 0x2648, Hi: 0x2653, Stride: 1},
		{Lo: 0x267f, Hi: 0x267f, Stride: 1},
		{Lo: 0x2693, Hi: 0x2693, Stride: 1},
		{Lo: 0x26a1, Hi: 0x26a1, Stride: 1},
		{Lo: 0x26aa, Hi: 0x26ab, Stride: 1},
		{Lo: 0x26bd, Hi: 0x26be, Stride: 1},
		{Lo: 0x26c4, Hi: 0x26c5, Stride: 1},
		{Lo: 0x26ce, Hi: 0x26ce, Stride: 1},
		{Lo: 0x26d4, Hi: 0x26d4, Stride: 1},
		{Lo: 0x26ea, Hi: 0x26ea, Stride: 1},
		{Lo: 0x26f2, Hi: 0x26f3, Stride: 1},
		{Lo: 0x26f5, Hi: 0x26f5, Stride: 1},
		{Lo: 0x26fa, Hi: 0x26fa, Stride: 1},
		{Lo: 0x26fd, Hi: 0x26fd, Stride: 1},
		{Lo: 0x2705, Hi: 0x2705, Stride: 1},
		{Lo: 0x270a, Hi: 0x270b, Stride: 1},
		{Lo: 0x2728, Hi: 0x2728, Stride: 1},
		{Lo: 0x274c, Hi: 0x274c, Stride: 1},
		{Lo: 0x274e, Hi: 0x274e, Stride: 1},
		{Lo: 0x2753, Hi: 0x2755, Stride: 1},
		{Lo: 0x2757, Hi: 0x2757, Stride: 1},
		{Lo: 0x2795, Hi: 0x2797, Stride: 1},
		{Lo: 0x27b0, Hi: 0x27b0, Stride: 1},
		{Lo: 0x27bf, Hi: 0x27bf, Stride: 1},
		{Lo: 0x2b1b, Hi: 0x2b1c, Stride: 1},
		{Lo: 0x2b50, Hi: 0x2b50, Stride: 1},
		{Lo: 0x2b55, Hi: 0x2b55, Stride: 1},
		{Lo: 0x2e80, Hi: 0x303e, Stride: 1},
		{Lo: 0x3041, Hi: 0x3247, Stride: 1},
		{Lo: 0x3250, Hi: 0xa4cf, Stride: 1},
		{Lo: 0xa960, Hi: 0xa97f, Stride: 1},
		{Lo: 0xac00, Hi: 0xd7a3, Stride: 1},
		{Lo: 0xf900, Hi: 0xfaff, Stride: 1},
		{Lo: 0xfe10, Hi: 0xfe19, Stride: 1},
		{Lo: 0xfe30, Hi: 0xfe6f, Stride: 1},
		{Lo: 0xff00, Hi: 0xff60, Stride: 1},
		{Lo: 0xffe0, Hi: 0xffe6, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x16fe0, Hi: 0x18d7f, Stride: 1},
		{Lo: 0x1aff0, Hi: 0x1b2ff, Stride: 1},
		{Lo: 0x1f004, Hi: 0x1f004, Stride: 1},
		{Lo: 0x1f0cf, Hi: 0x1f0cf, Stride: 1},
		{Lo: 0x1f18e, Hi: 0x1f18e, Stride: 1},
		{Lo: 0x1f191, Hi: 0x1f19a, Stride: 1},
		{Lo: 0x1f200, Hi: 0x1f2ff, Stride: 1},
		{Lo: 0x1f300, Hi: 0x1f320, Stride: 1},
		{Lo: 0x1f32d, Hi: 0x1f335, Stride: 1},
		{Lo: 0x1f337, Hi: 0x1f37c, Stride: 1},
		{Lo: 0x1f37e, Hi: 0x1f393, Stride: 1},
		{Lo: 0x1f3a0, Hi: 0x1f3ca, Stride: 1},
		{Lo: 0x1f3cf, Hi: 0x1f3d3, Stride: 1},
		{Lo: 0x1f3e0, Hi: 0x1f3f0, Stride: 1},
		{Lo: 0x1f3f4, Hi: 0x1f3f4, Stride: 1},
		{Lo: 0x1f3f8, Hi: 0x1f43e, Stride: 1},
		{Lo: 0x1f440, Hi: 0x1f440, Stride: 1},
		{Lo: 0x1f442, Hi: 0x1f4fc, Stride: 1},
		{Lo: 0x1f4ff, Hi: 0x1f53d, Stride: 1},
		{Lo: 0x1f54b, Hi: 0x1f54e, Stride: 1},
		{Lo: 0x1f550, Hi: 0x1f567, Stride: 1},
		{Lo: 0x1f57a, Hi: 0x1f57a, Stride: 1},
		{Lo: 0x1f595, Hi: 0x1f596, Stride: 1},
		{Lo: 0x1f5a4, Hi: 0x1f5a4, Stride: 1},
		{Lo: 0x1f5fb, Hi: 0x1f64f, Stride: 1},
		{Lo: 0x1f680, Hi: 0x1f6c5, Stride: 1},
		{Lo: 0x1f6cc, Hi: 0x1f6cc, Stride: 1},
		{Lo: 0x1f6d0, Hi: 0x1f6d2, Stride: 1},
		{Lo: 0x1f6d5, Hi: 0x1f6df, Stride: 1},
		{Lo: 0x1f6eb, Hi: 0x1f6ef, Stride: 1},
		{Lo: 0x1f6f4, Hi: 0x1f6ff, Stride: 1},
		{Lo: 0x1f7e0, Hi: 0x1f7eb, Stride: 1},
		{Lo: 0x1f7f0, Hi: 0x1f7f0, Stride: 1},
		{Lo: 0x1f90c, Hi: 0x1f93a, Stride: 1},
		{Lo: 0x1f93c, Hi: 0x1f945, Stride: 1},
		{Lo: 0x1f947, Hi: 0x1f9ff, Stride: 1},
		{Lo: 0x1fa70, Hi: 0x1faff, Stride: 1},
		{Lo: 0x20000, Hi: 0x2fffd, Stride: 1},
		{Lo: 0x30000, Hi: 0x3fffd, Stride: 1},
	},
}
