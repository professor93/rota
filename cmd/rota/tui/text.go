package tui

import (
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/wire"
)

// clean is s made safe to draw in a cell: text from outside (a label, a dead
// reason, an error a server wrote) that shows as the characters it holds,
// is never taken as a command by the terminal, and is as wide to every
// measure that lays the screen out as it is to the terminal that draws it.
//
//   - Every escape sequence goes whole, as a space between two words and
//     as nothing beside a space or at either end: a CSI (ESC [, or the
//     C1 CSI as a character or as the lone byte 0x9b) to its final byte, an
//     OSC and the other string sequences to their BEL or ST, and any other
//     ESC with what it takes (textEscapes). A key the sequence's parameters
//     were glued to then stands as a word, a key a sequence split is read
//     whole, and a key whose characters a sequence took is read with them
//     put back (textRedact); each is taken out. What is not: a key with two
//     sequences inside its "sk-ant-", one taking a character of it and the
//     other putting characters of its own in (x ESC s ESC [0m k-ant-…):
//     whoever can place those can as well print a key with no prefix.
//   - Every C0 control, DEL and every C1 control left is dropped. A
//     newline, a tab and the other line breaks become a space. A byte that
//     is not UTF-8 becomes "?".
//   - Format characters that are never drawn are dropped: the bidi controls a
//     terminal may reorder a row by, the zero width space, the soft hyphen,
//     the byte order mark. Only the joiners, ZWJ and ZWNJ, stay; they are
//     part of what they join.
//   - VS15 and VS16 are dropped: an emoji keeps its default presentation.
//   - Hangul spelled out in letters, as macOS writes Korean file names, is
//     put back into its syllables (textHangul), losing nothing.
//   - Each character is then made as wide to Bubble Tea's renderer as to a
//     terminal that counts graphemes, and to rota's own tables, and a mark
//     with nothing to sit on is dropped (textSamePasses). Where they disagree,
//     a character keeps its first letter or becomes "?": an emoji its first
//     picture, an Indic syllable its first consonant, as F9 rules, since
//     nothing else keeps it one width to both.
//   - Whatever is shaped like an Anthropic key is taken out (wire.Unsecret).
//
// clean of what clean gave is the same text.
func clean(s string) string {
	s, _, _ = textClean(s)
	return s
}

// textClean is clean, with the passes and the bytes looked at again that
// textSamePasses and textRedact report: work a test holds linear in s.
func textClean(s string) (out string, passes, scanned int) {
	// A control stands between words as a space does, so a key just after
	// one is taken out before the control goes, and a key that dropping
	// controls joins up is taken out after.
	s, seams := textEscapes(wire.Unsecret(s))
	var b strings.Builder
	b.Grow(len(s))
	kept := seams[:0] // the seams, at where they are now
	for i := 0; i < len(s); {
		for len(seams) > 0 && seams[0].at == i {
			sm := seams[0]
			sm.at = b.Len()
			kept = append(kept, sm)
			seams = seams[1:]
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteByte('?')
		case r == '\n' || r == '\t' || r == '\u0085' || r == '\u2028' || r == '\u2029':
			b.WriteByte(' ')
		case r < 0x20 || r >= 0x7f && r < 0xa0:
		case r == '\u200c' || r == '\u200d':
			b.WriteRune(r)
		case unicode.Is(unicode.Cf, r), r == '\ufe0e' || r == '\ufe0f':
		default:
			b.WriteRune(r)
		}
	}
	for _, sm := range seams { // at the end of s
		sm.at = b.Len()
		kept = append(kept, sm)
	}
	red, looked := textRedact(b.String(), kept)
	out, passes, scanned = textSamePasses(textHangul(red))
	return wire.Unsecret(out), passes, scanned + looked
}

// textKey is the start of an Anthropic credential: wire.Unsecret's shape,
// which takes "sk-ant-" and at least 16 more of these characters as a key
// when it stands as a word of its own.
const textKey = "sk-ant-"

// textKeyByte is a character a key is made of after its prefix.
func textKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// textRedact is s, in which each seam marks where an escape sequence stood
// (on the space that stands for it, or between two bytes where none does)
// and holds the key characters the sequence took, with every key taken out
// that either of two readings finds: the sequences gone, so a key a
// sequence split shows as neither half, and each sequence's key characters
// put back, so a key whose "s" or "-a" a sequence took is found too. A key starts where s starts, after a
// character that is not a letter or digit, after a seam or inside one, as
// wire.Unsecret has it, so a key a sequence was glued to is taken out too.
// Its replacement is wire.Unsecret's. The second result is how many bytes
// of key bodies it read: each once, as a body is read only where a key
// starts.
func textRedact(s string, seams []textSeam) (string, int) {
	if len(seams) == 0 {
		return s, 0
	}
	looked := 0
	ateAt := make(map[int]string, len(seams)) // what the seams at a place took
	space := make(map[int]bool, len(seams))   // a seam's own space
	for _, sm := range seams {
		ateAt[sm.at] += sm.ate
		if sm.space {
			space[sm.at] = true
		}
	}
	type span struct{ from, to int } // in s
	var spans []span
	// Two readings: the sequences gone, and each sequence's key bytes kept.
	for _, keep := range []bool{false, true} {
		var j strings.Builder
		at := make([]int, 0, len(s))
		var eaten []bool
		for i := 0; i <= len(s); i++ {
			if k := ateAt[i]; keep {
				for x := 0; x < len(k); x++ {
					j.WriteByte(k[x])
					at = append(at, i)
					eaten = append(eaten, true)
				}
			}
			if i == len(s) || space[i] {
				continue
			}
			j.WriteByte(s[i])
			at = append(at, i)
			eaten = append(eaten, false)
		}
		js := j.String()
		for k := 0; k < len(js); {
			off := strings.Index(js[k:], textKey)
			if off < 0 {
				break
			}
			start := k + off
			word := start == 0 || eaten[start] || space[at[start]-1] || !(textKeyByte(js[start-1]) && js[start-1] != '_' && js[start-1] != '-')
			if !word {
				k = start + 1
				continue
			}
			end := start + len(textKey)
			for end < len(js) && textKeyByte(js[end]) {
				end++
			}
			looked += end - start
			if end-start-len(textKey) >= 16 {
				// A key ending on what a sequence took ends at the seam,
				// past its space if it has one.
				to := at[end-1] + 1
				if eaten[end-1] && !space[at[end-1]] {
					to--
				}
				spans = append(spans, span{at[start], to})
				k = end
				continue
			}
			k = start + 1
		}
	}
	if len(spans) == 0 {
		return s, looked
	}
	sort.Slice(spans, func(x, y int) bool { return spans[x].from < spans[y].from })
	var b strings.Builder
	done := 0
	for _, sp := range spans {
		if sp.to <= done {
			continue
		}
		if sp.from < done {
			sp.from = done
		} else {
			b.WriteString(s[done:sp.from])
		}
		b.WriteString(textKey + "[not repeated]")
		done = sp.to
	}
	b.WriteString(s[done:])
	return b.String(), looked
}

// textSeam is where an escape sequence stood in what textEscapes gives: at
// the space that stands for it (space), or where the bytes after it start,
// and the key characters it took (ate).
type textSeam struct {
	at    int
	space bool
	ate   string
}

// textBlank is a byte that is or becomes a space.
func textBlank(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }

// textEscapes is s with each escape sequence in it replaced by a space
// where it stood between two words, and by nothing beside a blank, at
// either end, or after another sequence, and its seams, in order. ECMA-48 delimits the
// sequences:
//   - ESC [, or CSI (U+009B, or 0x9b alone, which a terminal reading eight
//     bits takes as CSI): parameter bytes 0x30-0x3F, intermediate bytes
//     0x20-0x2F, and the final byte 0x40-0x7E;
//   - ESC ], P, X, ^ or _, or OSC, DCS, SOS, PM or APC as C1 characters:
//     everything up to BEL or ST (ESC \ or U+009C);
//   - ESC with intermediate bytes 0x20-0x2F and a final byte 0x30-0x7E.
//
// A sequence cut short goes as far as it got. Anything else, a lone ESC
// among them, is left for clean's control pass to drop. The scan is linear.
func textEscapes(s string) (string, []textSeam) {
	const esc = 0x1b
	var b strings.Builder
	var seams []textSeam
	waiting := 0  // seams at the end of seams whose place is not known yet
	blank := true // what b ends with is blank, or b is empty
	done, any := 0, false
	// settle places the waiting seams before what comes next: on a space
	// of their own when that is a word after a word, else on nothing.
	settle := func(next string) {
		if waiting == 0 {
			return
		}
		spaced := next != "" && !blank && !textBlank(next[0])
		for x := len(seams) - waiting; x < len(seams); x++ {
			seams[x].at, seams[x].space = b.Len(), spaced && x == len(seams)-waiting
		}
		if spaced {
			b.WriteByte(' ')
			blank = true
		}
		waiting = 0
	}
	put := func(from, to int) {
		if from > done {
			settle(s[done:from])
			b.WriteString(s[done:from])
			blank = textBlank(s[from-1])
		}
		var k []byte
		for x := from; x < to; x++ {
			if textKeyByte(s[x]) {
				k = append(k, s[x])
			}
		}
		seams = append(seams, textSeam{ate: string(k)})
		waiting++
		done, any = to, true
	}
	// csi is where the CSI whose parameters start at i ends.
	csi := func(i int) int {
		for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
			i++
		}
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
			i++
		}
		return i
	}
	// str is where the string sequence whose text starts at i ends: past its
	// BEL or ST, or at the end of s.
	str := func(i int) int {
		for i < len(s) {
			switch {
			case s[i] == 0x07:
				return i + 1
			case s[i] == esc && i+1 < len(s) && s[i+1] == '\\':
				return i + 2
			case strings.HasPrefix(s[i:], "\u009c"):
				return i + 2
			}
			i++
		}
		return i
	}
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == esc && i+1 < len(s):
			switch c := s[i+1]; {
			case c == '[':
				end := csi(i + 2)
				put(i, end)
				i = end
			case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
				end := str(i + 2)
				put(i, end)
				i = end
			case c >= 0x20 && c <= 0x7e:
				end := i + 1
				for end < len(s) && s[end] >= 0x20 && s[end] <= 0x2f {
					end++
				}
				if end < len(s) && s[end] >= 0x30 && s[end] <= 0x7e {
					end++
				}
				put(i, end)
				i = end
			default:
				i++
			}
		case r == '\u009b' || r == utf8.RuneError && n == 1 && s[i] == 0x9b:
			end := csi(i + n)
			put(i, end)
			i = end
		case r == '\u009d' || r == '\u0090' || r == '\u0098' || r == '\u009e' || r == '\u009f':
			end := str(i + n)
			put(i, end)
			i = end
		default:
			i += n
		}
	}
	if !any {
		return s, nil
	}
	settle(s[done:])
	b.WriteString(s[done:])
	return b.String(), seams
}

// textSamePasses is s with every character the measures disagree on
// replaced by one they agree on: its first rune, or "?" when they disagree
// on that too.
//
// Bubble Tea's renderer lays a row out by adding up its runes' widths
// (ansi.WcWidth), as a terminal does that was never asked to count
// graphemes; it counts graphemes only once a terminal says it does (mode
// 2027). For a character spelled with several runes the two differ: a
// family joined by ZWJ, 👨‍👩‍👧, is 2 columns as one grapheme and 6 as
// three emoji, a heart asked to be a picture, ❤️, is 2 and 1, a thumb with
// a skin tone 2 and 4. Whichever way a cell was padded, some terminal draws
// that row shifted. What both count the same is drawn the same: 👨, ❤, 👍.
// Plain letters, CJK, a flag and a letter with its accents stay as they are.
//
// rota's own tables (cmd/rota/width.go) count what is left the same way
// too, so a label is as wide here as in `rota list`. For that, a ZWJ that
// joins nothing is dropped, and a rune whose width terminals do not agree
// on is "?" (textUnsure).
//
// A mark or a joiner with nothing to sit on, at the start or after a
// character that takes none, is dropped: drawn, a mark would sit on
// whatever is left of the cell. A first letter that would join what follows
// it into a new grapheme (a Prepend letter, a Hangul initial) is "?"
// instead, which joins nothing, so one pass leaves text a second pass does
// not change, and the work is linear in s; the second pass is the proof.
// It says how many passes it took and the bytes it looked at again.
func textSamePasses(s string) (out string, passes, scanned int) {
	for passes = 1; ; passes++ {
		t, n := textPass(s)
		scanned += n
		if t == s {
			return s, passes, scanned
		}
		s = t
	}
}

// textPass is one pass of textSamePasses, and the bytes textAlone looked at.
func textPass(s string) (string, int) {
	scanned := 0
	alone := func(c, rest string) bool {
		ok, n := textAlone(c, rest)
		scanned += n
		return ok
	}
	var b strings.Builder
	b.Grow(len(s))
	for rest := s; rest != ""; {
		c, _ := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		rest = rest[len(c):]
		// A ZWJ that ends its cluster joins nothing: width.go would join
		// what follows it to what came before.
		whole := len(c)
		c = strings.TrimRight(c, "\u200d")
		r, _ := utf8.DecodeRuneInString(c)
		switch one := string(r); {
		case c == "" || unicode.Is(unicode.M, r) || r == '\u200c' || r == '\u200d':
		case textAgreed(c) && (len(c) == whole || alone(c, rest)):
			b.WriteString(c)
		case textAgreed(one) && alone(one, rest):
			b.WriteString(one)
		default:
			b.WriteByte('?')
		}
	}
	return b.String(), scanned
}

// textAlone is whether c, written where a longer grapheme stood, stays a
// grapheme of its own before rest rather than joining its start, and how
// many bytes it looked at to say. Only rest's first grapheme is looked at,
// so the check costs what that one does.
func textAlone(c, rest string) (bool, int) {
	if rest == "" {
		return true, 0
	}
	next, _ := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
	look := c + next
	joined, _ := ansi.FirstGraphemeCluster(look, ansi.GraphemeWidth)
	return joined == c, len(look)
}

// textHangul is s with each Hangul syllable spelled out in conjoining
// letters, an initial, a vowel and maybe a final, written as the one
// precomposed syllable they spell, and a precomposed syllable without a
// final followed by a final letter given it. Unicode defines the mapping by
// arithmetic, so nothing is lost and nothing is needed but this. A vowel
// or final left over is still unsure (textUnsure).
func textHangul(s string) string {
	jamo := false
	for _, r := range s {
		if r >= 0x1100 && r <= 0x11ff {
			jamo = true
			break
		}
	}
	if !jamo {
		return s
	}
	const sBase, lBase, vBase, tBase = 0xac00, 0x1100, 0x1161, 0x11a7
	in := []rune(s)
	out := make([]rune, 0, len(in))
	for i := 0; i < len(in); i++ {
		r := in[i]
		final := func() bool { return i+1 < len(in) && in[i+1] > tBase && in[i+1] <= 0x11c2 }
		switch {
		case r >= lBase && r <= 0x1112 && i+1 < len(in) && in[i+1] >= vBase && in[i+1] <= 0x1175:
			syl := sBase + ((r-lBase)*21+(in[i+1]-vBase))*28
			i++
			if final() {
				syl += in[i+1] - tBase
				i++
			}
			out = append(out, syl)
		case r >= sBase && r <= 0xd7a3 && (r-sBase)%28 == 0 && final():
			out = append(out, r+in[i+1]-tBase)
			i++
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// textAgreed is whether every measure counts c, one grapheme, the same.
func textAgreed(c string) bool {
	for _, r := range c {
		if textUnsure(r) {
			return false
		}
	}
	return ansi.StringWidth(c) == ansi.StringWidthWc(c)
}

// textUnsure is a rune that one terminal draws wider than another: one
// Unicode has not assigned, which a terminal draws as whatever box it has;
// one whose width Unicode 16 changed from one column to two, the trigrams
// and digrams of the Yijing and the Tai Xuan Jing symbols; Tangut's
// newest components; and a Hangul vowel or final that textHangul left
// standing alone, which some count as nothing and others as a column.
func textUnsure(r rune) bool {
	switch {
	case r >= 0x1160 && r <= 0x11ff, r >= 0xd7b0 && r <= 0xd7ff:
		return true
	case r >= 0x2630 && r <= 0x2637, r >= 0x268a && r <= 0x268f:
		return true
	case r >= 0x1d300 && r <= 0x1d376, r >= 0x18d80 && r <= 0x18dff:
		return true
	}
	// Not unicode.C, which holds the unassigned too. Of the controls and
	// format characters, only the joiners reach here.
	return !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.Co, unicode.Cf)
}

// textWidth is how many columns s takes on the screen, as Bubble Tea's
// renderer counts them: the one measure fit, wrap, the bars and the ladders
// use. On what clean gives, a terminal counting graphemes agrees.
func textWidth(s string) int { return ansi.StringWidthWc(s) }

// fit is s, plain text that clean gave, in exactly w columns: cut with "…"
// when it is wider, a wide character the edge would halve left out whole,
// then padded with spaces. A character is never split. "…" is measured as
// everything else is: with RUNEWIDTH_EASTASIAN=1, for a terminal that draws
// ambiguous characters wide, it is two columns, and where two do not fit
// the cut text is padded with spaces alone.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	n := textWidth(s)
	if n <= w {
		return s + strings.Repeat(" ", w-n)
	}
	tail := "…"
	if textWidth(tail) > w {
		tail = ""
	}
	room := w - textWidth(tail)
	var b strings.Builder
	used := 0
	for rest := s; rest != ""; {
		c, cw := ansi.FirstGraphemeCluster(rest, ansi.WcWidth)
		if used+cw > room {
			break
		}
		b.WriteString(c)
		used += cw
		rest = rest[len(c):]
	}
	b.WriteString(tail)
	return b.String() + strings.Repeat(" ", room-used)
}

// wrap is s, plain text that clean gave, broken into lines of at most w
// columns: at spaces where it can, inside a word too long for a line of its
// own, never inside a character. A character wider than w takes a line of
// its own, wider than w. A newline in s ends a line. The lines are not
// padded. Nothing to wrap, or no width to wrap into, is no lines.
func wrap(s string, w int) []string {
	if w <= 0 {
		return nil
	}
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		line, lw := "", 0
		for _, word := range strings.Fields(para) {
			ww := textWidth(word)
			switch {
			case lw > 0 && lw+1+ww <= w:
				line, lw = line+" "+word, lw+1+ww
				continue
			case lw > 0:
				lines = append(lines, line)
				line, lw = "", 0
			}
			for ww > w {
				head, hw := textHead(word, w)
				lines = append(lines, head)
				word, ww = word[len(head):], ww-hw
			}
			line, lw = word, ww
		}
		if lw > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

// textHead is the longest start of s that fits in w columns, and its width:
// at least one character, so a line too narrow for it still moves on.
func textHead(s string, w int) (string, int) {
	end, used := 0, 0
	for rest := s; rest != ""; {
		c, cw := ansi.FirstGraphemeCluster(rest, ansi.WcWidth)
		if used+cw > w && end > 0 {
			break
		}
		end, used = end+len(c), used+cw
		rest = rest[len(c):]
	}
	return s[:end], used
}

// timeLeft is how long until ends, rounded up as a countdown is: "12m",
// "45s", and "no time" once it has passed. It is the CLI's own wording for
// how long a login stays open, read against the TUI's clock.
func timeLeft(ends, now time.Time) string {
	d := ends.Sub(now)
	switch {
	case d <= 0:
		return "no time"
	case d < time.Minute:
		return strconv.Itoa(int((d+time.Second-1)/time.Second)) + "s"
	}
	return strconv.Itoa(int((d+time.Minute-1)/time.Minute)) + "m"
}
