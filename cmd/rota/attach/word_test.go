package attach

import (
	"testing"

	"github.com/professor93/rota/cmd/rota/client"
)

// TestWordQuotesOrUsesTheID: what to type to reach a session is its name when
// a shell takes the name as it is, the name in single quotes when it is clean
// but needs them, its own quotes written as a shell reads them back, and the
// id when the name is empty or not clean.
func TestWordQuotesOrUsesTheID(t *testing.T) {
	const id = "4f2a9c1e"
	for _, c := range []struct{ name, want string }{
		{"api", "api"},
		{"rota/main@v1.4:x,y=z+%", "rota/main@v1.4:x,y=z+%"},
		{"my api", "'my api'"},
		{"a\x1bb", id},
		{"a\nb", id},
		{"it's", `'it'\''s'`},
		{"bob's api", `'bob'\''s api'`},
		{"", id},
	} {
		if got := Word(client.Session{ID: id, Name: c.name}); got != c.want {
			t.Errorf("Word(name %q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestKeyNames: a chord key is named as a person types it.
func TestKeyNames(t *testing.T) {
	for _, c := range []struct {
		k    byte
		want string
	}{
		{0x1c, `Ctrl-\`},
		{0x1d, "Ctrl-]"},
		{0x1e, "Ctrl-^"},
		{0x1f, "Ctrl-_"},
		{0x01, "Ctrl-A"},
		{0x02, "Ctrl-B"},
		{0x11, "Ctrl-Q"},
		{0x1a, "Ctrl-Z"},
	} {
		if got := KeyName(c.k); got != c.want {
			t.Errorf("KeyName(%#x) = %q, want %q", c.k, got, c.want)
		}
	}
	if DefaultKey != 0x1c {
		t.Errorf("DefaultKey = %#x, want 0x1c", DefaultKey)
	}
}

// TestOneLineCleansControls: every control character, a newline among them,
// becomes a space, so a value never ends or styles a line of rota's own.
func TestOneLineCleansControls(t *testing.T) {
	if got := OneLine("a\tb\nc\x1b"); got != "a b c " {
		t.Errorf("OneLine = %q, want %q", got, "a b c ")
	}
	if got := OneLine("ok, plain"); got != "ok, plain" {
		t.Errorf("OneLine changed a clean value: %q", got)
	}
	if got := OneLine("bad\xffbyte"); got != "bad byte" {
		t.Errorf("OneLine kept a bad byte: %q", got)
	}
}
