package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// screenWidth is how many columns of a terminal s takes: what a table pads
// and wraps by, since a name in Chinese, Japanese or Korean, or one with an
// emoji in it, takes two columns a character where a letter takes one.
func screenWidth(s string) int {
	r := []rune(s)
	n := 0
	for i := 0; i < len(r); {
		end, w := char(r, i)
		n, i = n+w, end
	}
	return n
}

// char is where the character that starts at r[i] ends, and the columns it
// takes. Mostly it is one rune and the marks on it, but an emoji may be
// spelled with several, all drawn as one picture two columns wide: a flag
// is two regional indicators, 🇯🇵; a skin tone follows the hand or face it
// colours, 👍🏽; pictures joined by U+200D are one, a family, 👨‍👩‍👧, or a
// heart on fire, ❤‍🔥, even when the first of them is one column on its own;
// and U+FE0F after a symbol one column wide that has a picture, ❤️, or after
// a keycap's digit, 1️⃣, asks for the picture. After a letter or a space it
// changes nothing.
func char(r []rune, i int) (end, w int) {
	w = runeWidth(r[i])
	flag := regional(r[i]) // the first half of a flag, waiting for its second
	for end = i + 1; end < len(r); end++ {
		c := r[end]
		switch {
		case flag && regional(c):
			flag, w = false, 2
		case c == '\ufe0f':
			if w == 1 && picture(r[i]) {
				w = 2
			}
		case tone(c) && pictograph(r[i]):
			w = 2
		case r[end-1] == '\u200d' && pictograph(c) && pictograph(r[i]):
			w = 2
		case runeWidth(c) == 0:
		default:
			return end, w
		}
	}
	return end, w
}

// regional is a regional indicator, half a flag.
func regional(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }

// tone is a skin tone, which colours the emoji before it.
func tone(r rune) bool { return r >= 0x1f3fb && r <= 0x1f3ff }

// picture is a character one column wide as text that U+FE0F makes a
// picture two wide: a pictograph, or one of the few below their blocks that
// has a picture — a keycap's digit, "#" or "*", ©, ®, ‼ and ⁉.
func picture(r rune) bool {
	return pictograph(r) || strings.ContainsRune("#*0123456789©®‼⁉", r)
}

// pictograph is a rune of the blocks emoji come from: the symbols, arrows
// and dingbats of 2100–2BFF, and the pictures from U+1F000 on. None of them
// is a mark, so each takes a column or two.
func pictograph(r rune) bool {
	return r >= 0x2100 && r < 0x2c00 || r >= 0x1f000 && r < 0x20000
}

// runeWidth is the columns one rune takes on its own: none for a mark that
// sits on the character before it or a character that is not drawn at all,
// two for the wide characters below, and one for everything else — every
// letter, digit and sign of ASCII and Latin, the soft hyphen, the box the
// table is drawn with, and its "…".
//
// It is the East Asian Width property cut down to what a name is likely
// to hold, not the whole of it: the tables below say which.
func runeWidth(r rune) int {
	switch {
	case r < 0x300: // ASCII and Latin, before the first combining mark
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, jamo):
		return 0
	case unicode.Is(wide, r):
		return 2
	}
	return 1
}

// jamo is the vowels and finals of a Hangul syllable spelled out letter by
// letter, which a terminal draws into the first letter's two columns.
var jamo = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x1160, Hi: 0x11ff, Stride: 1},
		{Lo: 0xd7b0, Hi: 0xd7ff, Stride: 1},
	},
}

// wide is what a terminal draws two columns wide: the Wide and Fullwidth
// characters of East Asian scripts, and the emoji drawn as pictures by
// default. A symbol that is text unless asked otherwise — ☀, ✓, ★ — is one
// column, and is not here.
var wide = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x1100, Hi: 0x115f, Stride: 1}, // Hangul Jamo, first letters
		// Emoji by default among the symbols of 2300–2BFF.
		{Lo: 0x231a, Hi: 0x231b, Stride: 1}, // ⌚⌛
		{Lo: 0x2329, Hi: 0x232a, Stride: 1}, // 〈〉
		{Lo: 0x23e9, Hi: 0x23ec, Stride: 1},
		{Lo: 0x23f0, Hi: 0x23f0, Stride: 1},
		{Lo: 0x23f3, Hi: 0x23f3, Stride: 1},
		{Lo: 0x25fd, Hi: 0x25fe, Stride: 1},
		{Lo: 0x2614, Hi: 0x2615, Stride: 1}, // ☔☕
		{Lo: 0x2648, Hi: 0x2653, Stride: 1}, // the zodiac
		{Lo: 0x267f, Hi: 0x267f, Stride: 1},
		{Lo: 0x2693, Hi: 0x2693, Stride: 1},
		{Lo: 0x26a1, Hi: 0x26a1, Stride: 1}, // ⚡
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
		{Lo: 0x2705, Hi: 0x2705, Stride: 1}, // ✅
		{Lo: 0x270a, Hi: 0x270b, Stride: 1},
		{Lo: 0x2728, Hi: 0x2728, Stride: 1}, // ✨
		{Lo: 0x274c, Hi: 0x274c, Stride: 1}, // ❌
		{Lo: 0x274e, Hi: 0x274e, Stride: 1},
		{Lo: 0x2753, Hi: 0x2755, Stride: 1},
		{Lo: 0x2757, Hi: 0x2757, Stride: 1},
		{Lo: 0x2795, Hi: 0x2797, Stride: 1},
		{Lo: 0x27b0, Hi: 0x27b0, Stride: 1},
		{Lo: 0x27bf, Hi: 0x27bf, Stride: 1},
		{Lo: 0x2b1b, Hi: 0x2b1c, Stride: 1},
		{Lo: 0x2b50, Hi: 0x2b50, Stride: 1}, // ⭐
		{Lo: 0x2b55, Hi: 0x2b55, Stride: 1},
		// Radicals, CJK symbols and punctuation, the ideographic space.
		{Lo: 0x2e80, Hi: 0x303e, Stride: 1},
		// Hiragana, Katakana, Bopomofo, Hangul letters and enclosed CJK,
		// up to the numbers in circles on black squares, which are not.
		{Lo: 0x3041, Hi: 0x3247, Stride: 1},
		// The rest of enclosed CJK, compatibility CJK, the ideographs and
		// their Extension A, the Yijing hexagrams, and Yi.
		{Lo: 0x3250, Hi: 0xa4cf, Stride: 1},
		{Lo: 0xa960, Hi: 0xa97f, Stride: 1}, // more Hangul first letters
		{Lo: 0xac00, Hi: 0xd7a3, Stride: 1}, // Hangul syllables
		{Lo: 0xf900, Hi: 0xfaff, Stride: 1}, // compatibility ideographs
		{Lo: 0xfe10, Hi: 0xfe19, Stride: 1}, // vertical forms
		{Lo: 0xfe30, Hi: 0xfe6f, Stride: 1}, // CJK compatibility and small forms
		{Lo: 0xff00, Hi: 0xff60, Stride: 1}, // fullwidth forms
		{Lo: 0xffe0, Hi: 0xffe6, Stride: 1}, // fullwidth signs: ￠ to ￦
	},
	R32: []unicode.Range32{
		{Lo: 0x16fe0, Hi: 0x18d7f, Stride: 1}, // Tangut and Khitan
		{Lo: 0x1aff0, Hi: 0x1b2ff, Stride: 1}, // more kana, and Nüshu
		{Lo: 0x1f004, Hi: 0x1f004, Stride: 1}, // 🀄
		{Lo: 0x1f0cf, Hi: 0x1f0cf, Stride: 1}, // 🃏
		{Lo: 0x1f18e, Hi: 0x1f18e, Stride: 1}, // 🆎
		{Lo: 0x1f191, Hi: 0x1f19a, Stride: 1}, // 🆑 to 🆚
		{Lo: 0x1f200, Hi: 0x1f2ff, Stride: 1}, // enclosed ideographs
		// Pictographs and faces, transport and maps, and more pictographs:
		// all of them but the gaps, which are the ones that are text until
		// U+FE0F asks for the picture — 🌡, 🗂, 🖥, 🛠, 👁 and the like.
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
		{Lo: 0x1f7e0, Hi: 0x1f7eb, Stride: 1}, // coloured circles and squares
		{Lo: 0x1f7f0, Hi: 0x1f7f0, Stride: 1}, // 🟰
		{Lo: 0x1f90c, Hi: 0x1f93a, Stride: 1},
		{Lo: 0x1f93c, Hi: 0x1f945, Stride: 1},
		{Lo: 0x1f947, Hi: 0x1f9ff, Stride: 1},
		{Lo: 0x1fa70, Hi: 0x1faff, Stride: 1}, // and more
		{Lo: 0x20000, Hi: 0x2fffd, Stride: 1}, // ideographs, Extensions B to F
		{Lo: 0x30000, Hi: 0x3fffd, Stride: 1}, // and G on
	},
}

// prefix is how many of r's runes fit in n columns: as many characters as
// there is room for (see char), a wide one that would straddle the edge
// left out, so neither a wide character nor an emoji is ever split.
func prefix(r []rune, n int) int {
	w := 0
	for i := 0; i < len(r); {
		end, cw := char(r, i)
		if w += cw; w > n {
			return i
		}
		i = end
	}
	return len(r)
}

// suffix is how many of r's runes, counted back from its end, fit in n
// columns: the characters (see char) from the first one whose start leaves
// no more than n columns to draw, so it agrees with screenWidth on every
// width, and neither a wide character nor an emoji is ever split — the one
// the edge would halve is left out whole, and nothing is kept from the
// middle of one.
func suffix(r []rune, n int) int {
	var at, ws []int // where each character starts, and its columns
	left := 0        // the columns from at[j] to the end
	for i := 0; i < len(r); {
		end, w := char(r, i)
		at, ws = append(at, i), append(ws, w)
		left += w
		i = end
	}
	for j, i := range at {
		if left <= n {
			return len(r) - i
		}
		left -= ws[j]
	}
	return 0
}

// chars is how many characters s has (see char): a letter with the marks on
// it is one, and so is an emoji however many runes spell it.
func chars(s string) int {
	r := []rune(s)
	n := 0
	for i := 0; i < len(r); n++ {
		i, _ = char(r, i)
	}
	return n
}

// printable is s with every control character but the newline — a tab, a
// carriage return, an escape, DEL — and every byte that is not UTF-8 made a
// space. Each moves a terminal's cursor where no count of columns can
// follow it, or is drawn in a way the count cannot know, so a directory with
// a tab in it would push the rest of its row out of line.
func printable(s string) string {
	var b strings.Builder
	done := 0 // how much of s is in b, or 0 while nothing needed a space
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 || r != '\n' && unicode.IsControl(r) {
			b.WriteString(s[done:i])
			b.WriteByte(' ')
			done = i + size
		}
		i += size
	}
	if done == 0 {
		return s
	}
	b.WriteString(s[done:])
	return b.String()
}

// oneLine is printable with the newline a space as well: a name, a path or
// a reply from outside set in a line of rota's own, which it must not end
// early. Only the value is cleaned, so the line's own ending is untouched.
func oneLine(s string) string { return strings.ReplaceAll(printable(s), "\n", " ") }

// oneLineError is an error said by oneLine: one whose message names a path
// or quotes something from outside, set in a line of rota's own. errors.Is
// and errors.As still find the error it was.
type oneLineError struct{ err error }

func (e oneLineError) Error() string { return oneLine(e.err.Error()) }
func (e oneLineError) Unwrap() error { return e.err }
