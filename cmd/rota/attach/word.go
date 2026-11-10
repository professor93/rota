package attach

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/professor93/rota/cmd/rota/client"
)

// KeyName is a chord key as a person types it: 0x1C is Ctrl-\, 0x1D Ctrl-],
// 0x1E Ctrl-^, 0x1F Ctrl-_, and 0x01 to 0x1A are Ctrl-A to Ctrl-Z. Every
// other control byte is named the same way, Ctrl- and the character 0x40
// above it, and any other byte by its value.
func KeyName(k byte) string {
	if k < 0x20 {
		return "Ctrl-" + string(rune(k+0x40))
	}
	return fmt.Sprintf("0x%02X", k)
}

// wordPlain is a name a shell takes as it is, with no quotes.
var wordPlain = regexp.MustCompile(`^[A-Za-z0-9._@%+=:,/-]+$`)

// Word is what to type to reach s again, as the last line of an attach says
// it: its name when a shell takes the name as it is; the name in single
// quotes when it is clean, with no control character, but needs them, each
// quote of its own ended, escaped and begun again, so a shell reads the
// name back as it is; and otherwise its id. A session called it's is
//
//	'it'\''s'
//
// That is the word `rota run --detach` prints after rota send. The name is
// what client.Match finds a live session by, and the id finds it, live or
// stopped, always.
func Word(s client.Session) string {
	n := s.Name
	switch {
	case n == "" || OneLine(n) != n:
		return OneLine(s.ID)
	case wordPlain.MatchString(n):
		return n
	}
	return "'" + strings.ReplaceAll(n, "'", `'\''`) + "'"
}

// OneLine is s with every control character, a newline among them, and every
// byte that is not UTF-8 made a space: a name, a path or a reply from outside
// set in a line of rota's own, which it must neither end early nor style.
// Only the value is cleaned, so the line's own ending is untouched. It is a
// copy of package main's oneLine.
func OneLine(s string) string {
	var b strings.Builder
	done := 0 // how much of s is in b, or 0 while nothing needed a space
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 || unicode.IsControl(r) {
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
