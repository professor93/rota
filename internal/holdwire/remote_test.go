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
	write("{rc.json", `{"remoteControlAtStartup":true}`)

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
		// The re-review's probes of a46fbc9: Commander takes the next
		// argument as a required flag's value even when it is "--", so the
		// flag after it is still a flag. The flags and --settings are read
		// wherever they stand, after "--" too, and as a value of another flag.
		{"--model", "--", "--rc"},
		{"-n", "--", "--remote-control"},
		{"-n", "--", "--remote-control=x"},
		{"--name", "--", "--settings", `{"remoteControlAtStartup":true}`},
		{"--append-system-prompt", "--", "--rc=desk"},
		{"-cn", "--", "--rc"},
		{"--add-dir", "--", "--rc"},
		{"--", "--rc"},
		{"--", "--settings=" + on},
		{"--name", "--rc"},
		{"--settings", "--rc"},
		// A value is a document only when it is one, "{" to "}", as Claude
		// Code tells them apart; otherwise it is a file.
		{"--settings", "{rc.json"},
		// --managed-settings is the policy tier's document, the first source
		// Claude Code reads remoteControlAtStartup from, read the same way.
		{"--managed-settings", `{"remoteControlAtStartup":true}`},
		{`--managed-settings={"remoteControlAtStartup":true}`},
		{"--model", "--", "--managed-settings", ` {"remoteControlAtStartup":true} `},
	}
	for _, args := range asks {
		if got, _ := AsksRemoteControl(args, dir); !got {
			t.Errorf("%q: %v, want it to ask for Remote Control", args, got)
		}
	}
	// Every --settings is read: one that cannot be read does not hide one
	// that turns it on, and is still said, for a caller that refuses it.
	for _, args := range [][]string{
		{"--settings", "missing.json", "--settings", `{"remoteControlAtStartup":true}`},
		{"--settings", `{"remoteControlAtStartup":true}`, "--settings", "missing.json"},
	} {
		if got, err := AsksRemoteControl(args, dir); !got || err == nil {
			t.Errorf("%q: %v %v, want it to ask, and the unreadable one said", args, got, err)
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
		{"--", "--rcx"},
		{"--", "say --rc please"},
		{"--settings", `{"model":"x"}`},
		{"--settings", `{"remoteControlAtStartup":false}`},
		{"--settings", off},
		{"--settings"},
		{"--managed-settings", `{"model":"x"}`},
	}
	for _, args := range plain {
		if got, err := AsksRemoteControl(args, dir); got || err != nil {
			t.Errorf("%q: %v %v, want it to ask for nothing", args, got, err)
		}
	}
	// A settings value is a value: the word after --settings or
	// --managed-settings is never read as Claude Code's command, though it
	// is an error as a settings value.
	for _, args := range [][]string{{"--settings", "rc"}, {"--managed-settings", "rc"}, {"--settings", "remote-control"}} {
		if got, _ := AsksRemoteControl(args, dir); got {
			t.Errorf("%q: the value is read as the command", args)
		}
	}
	// What cannot be read cannot be said to ask for nothing. A
	// --managed-settings takes no file, so its value is a document or wrong.
	for _, c := range []struct {
		args []string
		flag string
	}{
		{[]string{"--settings", filepath.Join(dir, "missing.json")}, "--settings"},
		{[]string{"--settings", broken}, "--settings"},
		{[]string{"--settings", `{"remoteControlAtStartup":`}, "--settings"},
		{[]string{"--settings="}, "--settings"},
		{[]string{"--settings", dir}, "--settings"},
		{[]string{"--managed-settings", on}, "--managed-settings"},
		{[]string{"--managed-settings", "[true]"}, "--managed-settings"},
		{[]string{"--managed-settings="}, "--managed-settings"},
	} {
		got, err := AsksRemoteControl(c.args, dir)
		if got || err == nil || !strings.Contains(err.Error(), c.flag) {
			t.Errorf("%q: %v %v, want an error that names %s", c.args, got, err, c.flag)
		}
	}
}

// What a session's --managed-settings say, wherever they stand: whether
// there is one at all, which is all a caller over TCP needs to refuse it;
// whether one turns remoteControlAtStartup on; and whether one turns
// disableRemoteControl off, which would lift the guard, since the policy
// tier ranks above --settings.
func TestReadManagedSettings(t *testing.T) {
	for _, c := range []struct {
		args                   []string
		present, on, reenables bool
		bad                    bool
	}{
		{nil, false, false, false, false},
		{[]string{"--settings", `{"remoteControlAtStartup":true}`}, false, false, false, false},
		{[]string{"--managed-settings", `{"model":"x"}`}, true, false, false, false},
		{[]string{"--", "--managed-settings=" + `{"remoteControlAtStartup":true}`}, true, true, false, false},
		{[]string{"--managed-settings", `{"disableRemoteControl":false}`}, true, false, true, false},
		{[]string{"--managed-settings", `{"disableRemoteControl":true}`}, true, false, false, false},
		{[]string{"--managed-settings", "not json"}, true, false, false, true},
		{[]string{"--managed-settings"}, false, false, false, false},
	} {
		m := ReadManaged(c.args)
		if m.Present != c.present || m.AtStartup != c.on || m.Reenables != c.reenables || (m.Err != nil) != c.bad {
			t.Errorf("%q: %+v, want present %v, at startup %v, reenables %v, bad %v", c.args, m, c.present, c.on, c.reenables, c.bad)
		}
	}
}
