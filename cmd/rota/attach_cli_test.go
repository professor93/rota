package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/professor93/rota/cmd/rota/attach"
	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/pty"
)

// What --attach refuses, it refuses before it looks for a daemon: an
// account, a prompt or a CLI's arguments, the flags that start a terminal
// rather than join one, no session at all, and a standard input or output
// that is not a terminal. Each is a usage error, and the store is left
// without a daemon.
func TestAttachFlagRefusals(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	const (
		account = "--attach takes a session, not an account: rota run --attach <session>"
		args    = "--attach takes no prompt or CLI arguments"
		others  = "--attach joins a session that is running; it takes no --detach, --share, --label or --remote-control"
		none    = "--attach takes the session to show here: rota run --attach <session>"
		twice   = "--attach takes one session; it was given more than once"
		off     = `--attach needs a terminal; rota send <session> "text" types into one from a script`
	)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "1", "--attach", "api"}, account},
		{[]string{"run", "--attach", "api", "1"}, args},
		{[]string{"run", "--attach", "api", "--", "x"}, args},
		{[]string{"run", "--attach", "api", "fix it"}, args},
		{[]string{"run", "--attach", "api", "-i"}, args},
		{[]string{"run", "--attach", "api", "--detach"}, others},
		{[]string{"run", "--attach", "api", "--share"}, others},
		{[]string{"run", "--share=watch", "--attach=api"}, others},
		{[]string{"run", "--attach", "api", "--label", "x"}, others},
		{[]string{"run", "--attach", "api", "--remote-control"}, others},
		{[]string{"run", "--attach"}, none},
		{[]string{"run", "--attach="}, none},
		{[]string{"run", "--attach", "--detach"}, none},
		{[]string{"run", "--attach", "--", "api"}, none},
		{[]string{"run", "--attach", "a", "--attach", "b"}, twice},
		{[]string{"run", "--attach=a", "--attach=a"}, twice},
		{[]string{"run", "--attach", "api", "--", "--json"}, args},
	} {
		out, errb, code := call(t, c.args...)
		if code != 2 || out != "" || !strings.Contains(errb, "error: "+c.want+"\n") {
			t.Errorf("rota %s: exit %d %q %q, want 2 and %q", strings.Join(c.args, " "), code, out, errb, c.want)
		}
	}

	// --json before -- is rota's, as it is for --detach: not an argument,
	// and the refusal it reaches is JSON.
	out, errb, code := call(t, "run", "--attach", "api", "--json")
	var doc map[string]string
	if code != 2 || errb != "" || json.Unmarshal([]byte(out), &doc) != nil || doc["error"] != off {
		t.Errorf("rota run --attach api --json off a terminal: exit %d %q %q, want 2 and {\"error\": %q}", code, out, errb, off)
	}

	// Standard input a pipe, with standard output a terminal, and the other
	// way round: neither is a terminal to hand the session.
	pipe, typing, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pipe.Close(); typing.Close() })
	master, slave, err := pty.Open()
	if !pty.Supported {
		// Nowhere to hold a session: the platform is what is said.
		was := stdin
		stdin = pipe
		t.Cleanup(func() { stdin = was })
		_, errb, code := call(t, "run", "--attach", "api")
		if code != 1 || !strings.Contains(errb, holdwire.ErrPlatform.Error()) {
			t.Fatalf("rota run --attach api here: exit %d %q, want 1 and %q", code, errb, holdwire.ErrPlatform)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	for _, c := range []struct {
		name string
		in   *os.File
		out  *os.File
	}{
		{"a pipe for stdin", pipe, slave},
		{"a pipe for stdout", slave, typing},
	} {
		was := stdin
		stdin = c.in
		var errb bytes.Buffer
		code := run([]string{"run", "--attach", "api"}, c.out, &errb)
		stdin = was
		if code != 2 || !strings.Contains(errb.String(), "error: "+off+"\n") {
			t.Errorf("rota run --attach api with %s: exit %d %q, want 2 and %q", c.name, code, errb.String(), off)
		}
	}

	_ = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "daemon.pid" {
			t.Errorf("a refused --attach left %s", path)
		}
		return nil
	})
}

// The word --detach says to type after rota send is the word an attach says
// to come back with, so a session is named alike everywhere rota names it:
// its name as it is, the name quoted for a shell, or its id.
func TestSendWordIsTheAttachWord(t *testing.T) {
	const id = "0123456789abcdef"
	for _, c := range []struct{ name, want string }{
		{"api", "api"},
		{"refactor.v2-x_y@host:9/a,b+c=d%", "refactor.v2-x_y@host:9/a,b+c=d%"},
		{"my job", "'my job'"},
		{"it's", `'it'\''s'`},
		{"$HOME", "'$HOME'"},
		{"x\x1b]0;owned\x07y", id},
		{"", id},
	} {
		s := rotaclient.Session{ID: id, Name: c.name}
		if got := sendWord(&s); got != c.want || got != attach.Word(s) {
			t.Errorf("sendWord of %q is %q, attach.Word %q; want both %q", c.name, got, attach.Word(s), c.want)
		}
	}
}
