package sshd

import (
	"slices"
	"testing"
)

func TestWordsParse(t *testing.T) {
	for _, c := range []struct {
		line string
		want []string
	}{
		{`new --label "a b" -- --model 'x y'`, []string{"new", "--label", "a b", "--", "--model", "x y"}},
		{"a\tb \t c", []string{"a", "b", "c"}},
		{"  list  ", []string{"list"}},
		{"", nil},
		{" \t ", nil},
		// Quotes join what they touch, as in a shell, and an empty pair is
		// an empty word.
		{`a"b c"d`, []string{"ab cd"}},
		{`'' x`, []string{"", "x"}},
		// Nothing but a quote means anything: no escapes, no globs, no
		// variables, no separators.
		{`a\ b`, []string{`a\`, "b"}},
		{`"it's"`, []string{"it's"}},
		{`'say "hi"'`, []string{`say "hi"`}},
		{`list; rm -rf ~ $HOME *`, []string{"list;", "rm", "-rf", "~", "$HOME", "*"}},
		{"a\nb", []string{"a\nb"}},
	} {
		got, err := words(c.line)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("words(%q) = %q, %v; want %q", c.line, got, err, c.want)
		}
	}
	for _, line := range []string{`a"b`, `'`, `x "y`, `'a' "b`} {
		if got, err := words(line); err == nil {
			t.Errorf("words(%q) = %q; want an unbalanced quote refused", line, got)
		}
	}
}
