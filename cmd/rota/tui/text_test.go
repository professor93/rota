package tui

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
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
		"a\x1b[2Jb\nc\x07":                "a[2Jb c",
		"tab\there":                       "tab here",
		"crlf\r\nnext":                    "crlf next",
		"del\x7fc1\u009bx":                "delc1x",
		"nel\u0085line\u2028par\u2029end": "nel line par end",
		// A byte that is not UTF-8: 0x9b alone is CSI to a terminal that
		// reads 8 bits.
		"bad\x9b[2J": "bad?[2J",
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
	for _, in := range []string{"x " + key, "x\n" + key, "x\x1b" + key + "\x07", "refused: " + key + "."} {
		got := clean(in)
		if strings.Contains(got, "AAAAAAAAAAAAAAAA") {
			t.Errorf("clean(%q) = %q still shows the key", in, got)
		}
		if !strings.Contains(got, "sk-ant-[not repeated]") {
			t.Errorf("clean(%q) = %q does not say a key was taken out", in, got)
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
		"\u1100가나다라 마바사":                        "?나다라 마바사",
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
	} {
		before := textScanned.Load()
		got, passes := textSamePasses(textHangul(c.s))
		if passes > 2 {
			t.Errorf("%s: %d passes", c.name, passes)
		}
		if looked := textScanned.Load() - before; looked > 4*int64(len(c.s)) {
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
	{"VS16 keycap", "1\ufe0f\u20e3", "1", 1},
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
// their own, once with RUNEWIDTH_EASTASIAN=1, the supported setting for a
// terminal that draws ambiguous characters wide, and once without it: x/ansi
// reads the variable as the binary starts, so only a new process changes
// the measure. The child is this binary, started by its own path, with PATH
// still the empty directory TestMain made, so it finds no CLI.
func TestTextBothEastAsianModes(t *testing.T) {
	if os.Getenv(textModeVar) != "" {
		t.Skip("a child run")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := "^(TestTextModeIsTheOneAsked|TestFitCountsColumnsNotBytes|TestTheBarTicksAtTheThreshold|TestCleanDropsControlsAndKeys|TestTextRealWordsComeBack|TestTextWrapKeepsEveryLineInItsWidth|TestTextEveryCharacterMeasuresOneWayAfterClean)$"
	for _, mode := range []string{"narrow", "wide"} {
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
			t.Errorf("the %s run failed: %v\n%s", mode, err, out)
			continue
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
