package holdwire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every spelling of Remote Control that Claude Code 2.1.322 takes on its
// command line is read: its flag and the flag's alias, bare, with a name
// after an equals sign or as the next argument; its command and the
// command's alias as any operand; the three more words its entry point
// takes as the first argument; and --settings that turns
// remoteControlAtStartup on, inline in either form or in a file, relative
// to the directory the CLI starts in. The first ten are the security
// review's probe of 2035e75, every one of which a token could start
// unreadied.
//
// A flag's value is a value: a worktree, a session or an agent named
// remote-control or rc asks for nothing, and neither does one of the
// three words anywhere but first.
func TestAsksRemoteControlReadsWhatClaudeCodeTakes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	on := write("on.json", `{"remoteControlAtStartup":true}`)
	off := write("off.json", `{"remoteControlAtStartup":false,"model":"x"}`)
	write("rel.json", string([]byte{0xEF, 0xBB, 0xBF})+`{"remoteControlAtStartup": true}`) // after a byte-order mark
	broken := write("broken.json", `{"remoteControlAtStartup":`)

	asks := [][]string{
		// The review's probe table.
		{"--remote-control"},
		{"--rc"},
		{"--rc=laptop"},
		{"--rc", "laptop"},
		{"rc"},
		{"remote"},
		{"sync"},
		{"bridge"},
		{"--settings", `{"remoteControlAtStartup":true}`},
		{`--settings={"remoteControlAtStartup":true}`},
		// And the rest.
		{"--remote-control=laptop"},
		{"--remote-control", "laptop"},
		{"remote-control"},
		{"--model", "opus", "rc"},
		{"--model", "opus", "remote-control"},
		{"--verbose", "rc"},
		{"--debug", "rc"},
		{"--resume", "abc", "--remote-control"},
		{"--", "remote-control"},
		{"--", "rc"},
		{"--settings", ` { "remoteControlAtStartup" : true } `},
		{"--settings", on},
		{"--settings=" + on},
		{"--settings", "rel.json"},
	}
	for _, args := range asks {
		if got, err := AsksRemoteControl(args, dir); !got || err != nil {
			t.Errorf("%q: %v %v, want it to ask for Remote Control", args, got, err)
		}
	}
	plain := [][]string{
		{},
		{"--model", "opus"},
		{"--remote-controlled"},
		{"--rcx"},
		{"--remote-control-session-name-prefix=x"},
		{"--remote-control-session-name-prefix", "rc"},
		{"--name", "sync"},
		{"--name", "remote-control"},
		{"-n", "rc"},
		{"-w", "remote-control"},
		{"--worktree", "rc"},
		{"--agent", "remote-control"},
		{"--resume", "rc"},
		{"--verbose", "remote"},
		{"--", "remote"},
		{"--", "--rc"},
		{"--settings", `{"model":"x"}`},
		{"--settings", `{"remoteControlAtStartup":false}`},
		{"--settings", off},
		{"--settings"},
	}
	for _, args := range plain {
		if got, err := AsksRemoteControl(args, dir); got || err != nil {
			t.Errorf("%q: %v %v, want it to ask for nothing", args, got, err)
		}
	}
	// What cannot be read cannot be said to ask for nothing.
	for _, args := range [][]string{
		{"--settings", filepath.Join(dir, "missing.json")},
		{"--settings", broken},
		{"--settings", `{"remoteControlAtStartup":`},
		{"--settings="},
		{"--settings", dir},
	} {
		got, err := AsksRemoteControl(args, dir)
		if got || err == nil || !strings.Contains(err.Error(), "--settings") {
			t.Errorf("%q: %v %v, want an error that names --settings", args, got, err)
		}
	}
}
