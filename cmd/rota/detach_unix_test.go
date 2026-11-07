//go:build linux || darwin

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// rota run --detach as a person types it, on the daemon rig: each command is
// this test binary acting as rota in a process of its own, so is the daemon
// and each holder, and the CLI in a held session is the tty fake.

// detachedLine is what a --detach that worked says last: the account, the
// CLI, the session's name and id, and the name again in what to type next.
var detachedLine = regexp.MustCompile(`(?m)^rota: (\S+) via (\S+) — held as session (.+) \(([0-9a-f]{16})\); rota send (.+) "\.\.\." types into it, rota list --sessions lists it\n\z`)

// detach runs cmd, a --detach that must work, and says the session's id
// from its last line; m is the whole match.
func (r *daemonRig) detach(cmd *exec.Cmd) (id string, m []string, stderr string) {
	r.t.Helper()
	out, errb, code := r.run(cmd)
	m = detachedLine.FindStringSubmatch(errb)
	if code != 0 || out != "" || m == nil {
		r.t.Fatalf("rota %s: exit %d\nstdout %q\nstderr %q\nlog:\n%s", strings.Join(cmd.Args[1:], " "), code, out, errb, r.log())
	}
	return m[4], m, errb
}

// A detached CLI is a held session: live, named by --label, in the folder
// the command ran in, and at 120×40, because standard input here is not a
// terminal to measure. Nothing goes to stdout.
func TestDetachHandsTheCLIToAHolder(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	dir, err := os.MkdirTemp("", "w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cmd := r.cmd("run", "1", "--detach", "--label", "refactor")
	cmd.Dir = dir
	id, m, _ := r.detach(cmd)
	if m[1] != "claude/a@x" || m[2] != "claude" || m[3] != "refactor" || m[5] != "refactor" {
		t.Fatalf("the line names %q via %q, session %q, send to %q", m[1], m[2], m[3], m[5])
	}
	s := r.session(id)
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(s.Cwd)
	if s.State != "live" || s.Name != "refactor" || s.Label != "refactor" || s.Kind != holdwire.KindAccount ||
		s.Account != 1 || s.Provider != "claude" || !s.Attachable || got != want {
		t.Fatalf("the daemon lists it as %+v; want it live, named refactor, in %s", s, want)
	}
	p := r.attach(id)
	p.waitDoc("hello")
	p.waitOut("ready 120x40")
	p.send(0x2, []byte("x\r"))
	p.waitOut("got:x")
}

// With no daemon running, --detach starts one on its control socket alone —
// a port server.toml names is not opened — and says so on stderr before the
// line about the session.
func TestDetachStartsADaemonAndSaysSo(t *testing.T) {
	r := newDaemonRig(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if err := os.WriteFile(filepath.Join(r.home, "server.toml"), []byte("[server]\nlisten = \""+addr+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, errb := r.detach(r.cmd("run", "1", "--detach"))
	said := "rota: started the rota daemon for " + r.home + " (control socket only; rota serve stop ends it)\n"
	if lines := strings.SplitAfter(errb, "\n"); len(lines) != 3 || lines[0] != said {
		t.Fatalf("stderr is %q; want %q, then the session's line", errb, said)
	}
	if _, err := os.Stat(holdwire.DaemonPIDFile(r.terminals)); err != nil {
		t.Fatalf("no daemon.pid: %v", err)
	}
	if d, ok := holdwire.RunningDaemon(r.terminals); !ok || d.HTTP != "" {
		t.Fatalf("the daemon: %+v %v; want one running with no port", d, ok)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatalf("something listens on %s, which --detach's daemon must not open", addr)
	}
}

// The session starts from the environment of the command that asked for
// it, not the daemon's: a variable only the caller has reaches the CLI, and
// one that describes the caller's own terminal is kept from it. --json
// prints the session.
func TestDetachCarriesTheCallersEnvironment(t *testing.T) {
	r := newDaemonRig(t)
	// The daemon has neither, so whatever the CLI sees came from the caller.
	r.env = slices.DeleteFunc(r.env, func(e string) bool {
		return strings.HasPrefix(e, "FOO=") || strings.HasPrefix(e, "TMUX=")
	})
	r.start()
	// A Claude Code directory named relatively is the one meant from where
	// the command ran, though the holder runs in /.
	work, err := os.MkdirTemp("", "w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	if err := os.Mkdir(filepath.Join(work, "cc"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := r.cmd("--json", "run", "1", "--detach")
	cmd.Dir = work
	cmd.Env = append(cmd.Env, "FOO=bar", "TMUX=x", "CLAUDE_CONFIG_DIR=cc")
	out, errb, code := r.run(cmd)
	var s struct {
		ID, Name, State, Provider string
		Account                   int
		Ended                     *time.Time `json:"ended_at"`
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil || code != 0 || errb != "" ||
		!holdwire.IsID(s.ID) || s.State != "live" || s.Account != 1 || s.Provider != "claude" || s.Ended != nil {
		t.Fatalf("--json run --detach: exit %d %q %q (%v)\nlog:\n%s", code, out, errb, err, r.log())
	}
	p := r.attach(s.ID)
	p.waitDoc("hello")
	p.waitOut("ready 120x40")
	p.send(0x2, []byte("env FOO\r"))
	p.waitOut("env FOO=bar")
	p.send(0x2, []byte("env TMUX\r"))
	p.waitOut("env TMUX unset")
	// The account's own home is made from that directory, and its listing
	// names the one the command meant.
	real, _ := filepath.EvalSymlinks(filepath.Join(work, "cc"))
	p.send(0x2, []byte("envhas "+real+"\r"))
	p.waitOut("envhas yes")
}

// --remote-control with no account leaves the choice to the daemon, which
// readies the account it chose and adds Claude Code's flag for it (P14);
// with an account named, this command readies it and adds the flag, with
// its name, itself. Either way the setting is saved and said.
func TestDetachWithRemoteControl(t *testing.T) {
	r := newDaemonRig(t)
	until := time.Now().Add(time.Hour).UnixMilli()
	writeStore(t, r.home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","uuid":"u1","order":1,"token":{"accessToken":"A1","refreshToken":"r1","expiresAt":%d}},
		{"id":2,"provider":"claude","email":"b@x","uuid":"u2","order":2,"token":{"accessToken":"A2","refreshToken":"r2","expiresAt":%d}}]}`,
		until, until))
	r.start()
	for _, c := range []struct {
		args    []string
		account int
		who     string
		tail    []string
	}{
		{[]string{"run", "--detach", "--remote-control"}, 1, "claude/a@x", []string{"--remote-control"}},
		{[]string{"run", "2", "--detach", "--remote-control=laptop", "--", "--model", "opus"}, 2, "claude/b@x",
			[]string{"--model", "opus", "--remote-control", "laptop"}},
	} {
		id, _, errb := r.detach(r.cmd(c.args...))
		on := fmt.Sprintf("rota: remote control is on for %s from now on; `rota set %d --remote-control off` turns it off\n", c.who, c.account)
		if !strings.HasPrefix(errb, on) {
			t.Errorf("rota %s says %q; want it to begin %q", strings.Join(c.args, " "), errb, on)
		}
		if s := r.session(id); s.Account != c.account {
			t.Errorf("rota %s ran account %d, want %d", strings.Join(c.args, " "), s.Account, c.account)
		}
		m, err := holdwire.ReadMeta(holdwire.Dir(r.terminals, id))
		if err != nil || len(m.Args) < len(c.tail) || !slices.Equal(m.Args[len(m.Args)-len(c.tail):], c.tail) {
			t.Errorf("rota %s handed the CLI %q (%v), want it to end %q", strings.Join(c.args, " "), m.Args, err, c.tail)
		}
	}
	raw, err := os.ReadFile(filepath.Join(r.home, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Accounts []struct {
			RemoteControl bool `json:"remoteControl"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Accounts) != 2 || !doc.Accounts[0].RemoteControl || !doc.Accounts[1].RemoteControl {
		t.Fatalf("the setting is saved on both: %s", raw)
	}
}
