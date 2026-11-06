package main

import "unicode"

// screenWidth is how many columns of a terminal s takes: what a table pads
// and wraps by, since a name in Chinese, Japanese or Korean, or one with an
// emoji in it, takes two columns a character where a letter takes one.
func screenWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// runeWidth is the columns one rune takes: none for a mark that sits on
// the character before it or a character that is not drawn at all, two for
// the wide characters below, and one for everything else — every letter,
// digit and sign of ASCII and Latin, the box the table is drawn with, and
// its "…".
//
// It is the East Asian Width property cut down to what a name is likely
// to hold, not the whole of it: the tables below say which.
func runeWidth(r rune) int {
	switch {
	case r < 0x300: // ASCII and Latin, before the first combining mark
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unseen):
		return 0
	case unicode.Is(wide, r):
		return 2
	}
	return 1
}

// unseen is what takes no column and is not a mark: the zero-width space,
// joiners and direction marks, the word joiner, the byte-order mark, the
// tags a flag's emoji is spelled with, and the vowels and finals of a
// Hangul syllable spelled out letter by letter, which a terminal draws
// into the first letter's two columns. The variation selector that asks
// for an emoji (U+FE0F) is a mark already, and needs no line here.
var unseen = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x1160, Hi: 0x11ff, Stride: 1}, // Hangul Jamo vowels and finals
		{Lo: 0x200b, Hi: 0x200f, Stride: 1}, // zero-width space to RLM
		{Lo: 0x2060, Hi: 0x2064, Stride: 1}, // word joiner, invisible operators
		{Lo: 0xfeff, Hi: 0xfeff, Stride: 1}, // byte-order mark
	},
	R32: []unicode.Range32{
		{Lo: 0xe0001, Hi: 0xe007f, Stride: 1}, // tags
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
		// Hiragana, Katakana, Bopomofo, Hangul letters, enclosed and
		// compatibility CJK, and Extension A of the ideographs.
		{Lo: 0x3041, Hi: 0x4dbf, Stride: 1},
		{Lo: 0x4e00, Hi: 0xa4cf, Stride: 1}, // the ideographs, and Yi
		{Lo: 0xa960, Hi: 0xa97f, Stride: 1}, // more Hangul first letters
		{Lo: 0xac00, Hi: 0xd7a3, Stride: 1}, // Hangul syllables
		{Lo: 0xf900, Hi: 0xfaff, Stride: 1}, // compatibility ideographs
		{Lo: 0xfe10, Hi: 0xfe19, Stride: 1}, // vertical forms
		{Lo: 0xfe30, Hi: 0xfe6f, Stride: 1}, // CJK compatibility and small forms
		{Lo: 0xff00, Hi: 0xff60, Stride: 1}, // fullwidth forms
		{Lo: 0xffe0, Hi: 0xffe6, Stride: 1}, // fullwidth signs: ￠ to ￦
	},
	R32: []unicode.Range32{
		{Lo: 0x1f18e, Hi: 0x1f18e, Stride: 1}, // 🆎
		{Lo: 0x1f191, Hi: 0x1f19a, Stride: 1}, // 🆑 to 🆚
		{Lo: 0x1f200, Hi: 0x1f2ff, Stride: 1}, // enclosed ideographs
		{Lo: 0x1f300, Hi: 0x1f64f, Stride: 1}, // pictographs and faces
		{Lo: 0x1f680, Hi: 0x1f6ff, Stride: 1}, // transport and maps
		{Lo: 0x1f7e0, Hi: 0x1f7eb, Stride: 1}, // coloured circles and squares
		{Lo: 0x1f900, Hi: 0x1f9ff, Stride: 1}, // more pictographs
		{Lo: 0x1fa70, Hi: 0x1faff, Stride: 1}, // and more
		{Lo: 0x20000, Hi: 0x2fffd, Stride: 1}, // ideographs, Extensions B to F
		{Lo: 0x30000, Hi: 0x3fffd, Stride: 1}, // and G on
	},
}

// prefix is how many of r's runes fit in n columns: as many as there is
// room for, a wide one that would straddle the edge left out, and the marks
// on the last one kept with it.
func prefix(r []rune, n int) int {
	w := 0
	for k, c := range r {
		if w += runeWidth(c); w > n {
			return k
		}
	}
	return len(r)
}
