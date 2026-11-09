//go:build linux || darwin

package hold

import (
	"slices"
	"strings"
	"testing"
)

// A variable that points at the daemon's control socket never reaches the
// CLI, and the log hears its name alone; a path that merely has the letters
// inside a longer name — a store kept under a folder called myctl.sock.d —
// is kept, or the account's CLI would run in the wrong home.
func TestNoControlDropsTheControlSocketAndNothingThatOnlyLooksLikeIt(t *testing.T) {
	const ctl = "/s/terminals/ctl.sock"
	env := []string{
		"ROTA_CONTROL=" + ctl,
		"SOCKETS=/a/b:/elsewhere/ctl.sock",
		"RELATIVE=ctl.sock",
		"QUERY=unix://" + ctl + "?mode=1",
		"CLAUDE_CONFIG_DIR=/tmp/myctl.sock.d/store/homes/1",
		"NOTE=ctl.sockets",
		"FOO=bar",
		"EMPTY=",
	}
	var dropped []string
	got := noControl(env, ctl, func(name string) { dropped = append(dropped, name) })
	want := []string{
		"CLAUDE_CONFIG_DIR=/tmp/myctl.sock.d/store/homes/1",
		"NOTE=ctl.sockets",
		"FOO=bar",
		"EMPTY=",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("kept %q, want %q", got, want)
	}
	if names := []string{"ROTA_CONTROL", "SOCKETS", "RELATIVE", "QUERY"}; !slices.Equal(dropped, names) {
		t.Fatalf("dropped %q, want %q", dropped, names)
	}
	for _, name := range dropped {
		if strings.Contains(name, "/") || strings.Contains(name, "=") {
			t.Fatalf("the log would hear a value: %q", name)
		}
	}
}
