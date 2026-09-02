package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/professor93/rota/wire"
)

// Bare `rota` is a glance, not a manual: the short banner with a pointer to
// --help. The full text still lives behind help / -h / --help.
func TestBareRotaIsShortAndHelpIsFull(t *testing.T) {
	short, _, code := call(t)
	if code != 0 || !strings.Contains(short, "rota --help") {
		t.Fatalf("bare rota must point at --help: %d %q", code, short)
	}
	if !strings.HasPrefix(short, "rota "+wire.Version) {
		t.Fatalf("bare rota opens with its version: %q", short)
	}
	if strings.Contains(short, "The id is optional") {
		t.Fatalf("bare rota must not print the whole manual: %q", short)
	}
	if len(strings.Split(short, "\n")) > 16 {
		t.Fatalf("short means short: %d lines", len(strings.Split(short, "\n")))
	}
	full, _, code := call(t, "--help")
	if code != 0 || !strings.Contains(full, "The id is optional") {
		t.Fatalf("--help carries the full text: %d", code)
	}
	if help, _, _ := call(t, "help"); help != full {
		t.Fatal("help and --help are the same document")
	}
}

// `rota set -h` asks for the flags before naming an account, as the banner
// promises for every command; it must not be read as an account id.
func TestSetHelpNeedsNoID(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		out, errOut, code := call(t, "set", flag)
		if code != 0 || !strings.Contains(out, "usage: rota set") {
			t.Fatalf("set %s: code %d out %q err %q", flag, code, out, errOut)
		}
	}
}

// Every everyday run flag has a one-letter form.
func TestRunFlagsHaveShortForms(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROTA_HOME", dir)
	seedAccounts(t, dir, `{"accounts":[
		{"id":1,"provider":"claude","uuid":"c1","order":1,"token":{"accessToken":"tok"}}],"nextId":2,"ordered":true}`)
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"session_id":"s1","result":"ARGS=%s","num_turns":1}\n' "$*"` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	out, errb, code := call(t, "run", "1", "-m", "sonnet", "-e", "low", "-S", "hello")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	for _, want := range []string{"--model claude-sonnet-5", "--effort low", "--no-session-persistence"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in: %s", want, out)
		}
	}
}
