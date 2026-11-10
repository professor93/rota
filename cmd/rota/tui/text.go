package tui

import (
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
//   - Every C0 control, DEL and every C1 control is dropped, ESC with them,
//     so what follows an ESC shows as text. A newline, a tab and the other
//     line breaks become a space. A byte that is not UTF-8 becomes "?": 0x9b
//     alone is CSI to a terminal that reads 8 bits.
//   - Format characters that are never drawn are dropped: the bidi controls a
//     terminal may reorder a row by, the zero width space, the soft hyphen,
//     the byte order mark. Only the joiners, ZWJ and ZWNJ, stay; they are
//     part of what they join.
//   - Each character is then made as wide to Bubble Tea's renderer as to a
//     terminal that counts graphemes, and to rota's own tables, and a mark
//     with nothing to sit on is dropped (see textSame).
//   - Whatever is shaped like an Anthropic key is taken out (wire.Unsecret).
//
// clean of what clean gave is the same text.
func clean(s string) string {
	// A control stands between words as a space does, so a key just after
	// one is taken out before the control goes, and a key that dropping
	// controls joins up is taken out after.
	s = wire.Unsecret(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
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
		case unicode.Is(unicode.Cf, r):
		default:
			b.WriteRune(r)
		}
	}
	return wire.Unsecret(textSame(b.String()))
}

// textSame is s with every character the measures disagree on replaced by
// one they agree on: its first rune, or "?" when they disagree on that too.
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
// A mark with nothing to sit on, at the start or after a character that
// takes none, is dropped: drawn, it would sit on whatever is left of the
// cell. What a replacement leaves can join what follows it into a new
// grapheme, so the passes repeat until nothing changes; each one that
// changes something leaves fewer runes, or only ones that need no second.
func textSame(s string) string {
	for {
		t := textPass(s)
		if t == s {
			return s
		}
		s = t
	}
}

// textPass is one pass of textSame.
func textPass(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for rest := s; rest != ""; {
		c, _ := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		rest = rest[len(c):]
		// A ZWJ that ends its cluster joins nothing: width.go would join
		// what follows it to what came before.
		c = strings.TrimRight(c, "\u200d")
		r, _ := utf8.DecodeRuneInString(c)
		switch one := string(r); {
		case c == "" || unicode.Is(unicode.M, r):
		case textAgreed(c):
			b.WriteString(c)
		case textAgreed(one):
			b.WriteString(one)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
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
// newest components; and a Hangul vowel or final standing alone, which
// some count as nothing and others as a column.
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
// then padded with spaces. A character is never split.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	n := textWidth(s)
	if n <= w {
		return s + strings.Repeat(" ", w-n)
	}
	var b strings.Builder
	used := 0
	for rest := s; rest != ""; {
		c, cw := ansi.FirstGraphemeCluster(rest, ansi.WcWidth)
		if used+cw > w-1 {
			break
		}
		b.WriteString(c)
		used += cw
		rest = rest[len(c):]
	}
	b.WriteString("…")
	return b.String() + strings.Repeat(" ", w-1-used)
}

// wrap is s, plain text that clean gave, broken into lines of at most w
// columns: at spaces where it can, inside a word too long for a line of its
// own, never inside a character. A newline in s ends a line. The lines are
// not padded. Nothing to wrap, or no width to wrap into, is no lines.
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
