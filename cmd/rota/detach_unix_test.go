//go:build linux || darwin

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/pty"
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
	id, m, errb := r.detach(cmd)
	if m[1] != "claude/a@x" || m[2] != "claude" || m[3] != "refactor" || m[5] != "refactor" {
		t.Fatalf("the line names %q via %q, session %q, send to %q", m[1], m[2], m[3], m[5])
	}
	// The daemon was there first: nothing is said of starting one.
	if errb != m[0] {
		t.Fatalf("stderr is %q; with a daemon running it is the session's line alone", errb)
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

// At a terminal, the session starts at that terminal's size: the one the
// person typing --detach is looking at, rather than 120×40.
func TestDetachStartsAtTheTerminalsSize(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	cmd := r.cmd("run", "1", "--detach", "--label", "sized")
	master, err := pty.Start(cmd, 97, 31)
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited; master.Close() })
	screen := watchScreen(master)
	screen.wait(t, "lists it")
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("--detach at a terminal never finished: %q", screen.text())
	}
	m := regexp.MustCompile(`held as session sized \(([0-9a-f]{16})\)`).FindStringSubmatch(screen.text())
	if cmd.ProcessState.ExitCode() != 0 || m == nil {
		t.Fatalf("--detach at a terminal: exit %d %q\nlog:\n%s", cmd.ProcessState.ExitCode(), screen.text(), r.log())
	}
	p := r.attach(m[1])
	p.waitDoc("hello")
	p.waitOut("ready 97x31")
}

// A label with an escape in it is the session's name as it was given, and
// the line --detach says draws each control character in it as a space,
// as rota draws an account's name; the next thing to type names the
// session by its id, since the name as drawn would match nothing.
func TestDetachSaysADirtyLabelCleaned(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	const label = "x\x1b]0;owned\x07y"
	id, m, errb := r.detach(r.cmd("run", "1", "--detach", "--label", label))
	if strings.ContainsAny(errb, "\x1b\x07") || m[3] != "x ]0;owned y" || m[5] != id {
		t.Fatalf("stderr %q: the session is called %q and sent to as %q; want x ]0;owned y and its id %s", errb, m[3], m[5], id)
	}
	if s := r.session(id); s.Name != label || s.Label != label {
		t.Fatalf("the daemon lists it as %q, %q; the name is kept as it was given", s.Name, s.Label)
	}
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

// The race Review Focus 3 names: no daemon when --detach looks, and by the
// time its own start runs, a serve start's daemon — one serving a port —
// holds the store. --detach names that daemon, and never says it started
// one on its control socket alone. Nothing outside can arrange the moment
// between Ensure's look and its start, so this --detach runs in this
// process with ensureDaemon standing in, which starts the other daemon
// there.
func TestDetachThatLosesTheRaceNamesTheDaemonThatServes(t *testing.T) {
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
	// Whatever this process starts is rota, and finds the fake claude.
	t.Setenv("ROTA_AS_RUN", "1")
	for _, e := range r.env {
		if path, ok := strings.CutPrefix(e, "PATH="); ok {
			t.Setenv("PATH", path)
		}
	}
	was := ensureDaemon
	t.Cleanup(func() { ensureDaemon = was })
	var pid int
	ensureDaemon = func(terminals string, start func() error) (*rotaclient.Client, bool, error) {
		if _, err := rotaclient.Dial(terminals); !errors.Is(err, rotaclient.ErrNoDaemon) {
			t.Errorf("a daemon answered before the race: %v", err)
		}
		var http string
		if pid, http = r.start(); http == "" {
			t.Errorf("the daemon that won serves no port")
		}
		if err := start(); err != nil {
			return nil, false, err
		}
		c, err := rotaclient.Dial(terminals)
		return c, true, err
	}
	out, errb, code := call(t, "run", "1", "--detach")
	lines := strings.SplitAfter(errb, "\n")
	said := fmt.Sprintf("rota: a rota daemon is already running for %s (pid %d)\n", r.home, pid)
	if code != 0 || out != "" || len(lines) != 3 || lines[0] != said || !detachedLine.MatchString(lines[1]) {
		t.Fatalf("run --detach that lost the race: exit %d %q\nstderr %q\nwant it to begin %q", code, out, errb, said)
	}
	if d, ok := holdwire.RunningDaemon(r.terminals); !ok || d.PID != pid {
		t.Fatalf("the daemon serving is %+v, not the one that won (pid %d)", d, pid)
	}
}

// What the daemon refuses when it readies the account it chose for Remote
// Control, it refuses as the command line does an account it was named:
// another provider's account, and one on a token, which Remote Control
// refuses and whose reason is said rather than hidden. An account named
// with --remote-control is refused in the flag's own words, as rota run
// without --detach refuses it.
func TestDetachRemoteControlRefusals(t *testing.T) {
	t.Run("another provider", func(t *testing.T) {
		r := newDaemonRig(t)
		writeStore(t, r.home, `{"ordered":true,"nextId":2,"accounts":[
			{"id":1,"provider":"codex","email":"c@x","order":1,"token":{"accessToken":"x"},"extra":{"id_token":"h.p.s"}}]}`)
		r.start()
		for args, c := range map[string]struct {
			code int
			want string
		}{
			"run --detach --remote-control":   {1, "error: Remote Control is Claude Code's, and codex/c@x is a codex account\n"},
			"run 1 --detach --remote-control": {2, "error: --remote-control is Claude Code's, and codex/c@x is a codex account\n"},
		} {
			out, errb, got := r.rota(strings.Fields(args)...)
			if got != c.code || out != "" || !strings.HasPrefix(errb, c.want) {
				t.Errorf("rota %s: exit %d %q %q; want %d and %q", args, got, out, errb, c.code, c.want)
			}
		}
	})
	t.Run("a token", func(t *testing.T) {
		r := newDaemonRig(t)
		r.start()
		out, errb, code := r.rota("run", "--detach", "--remote-control")
		if code != 1 || out != "" || !strings.Contains(errb, "Remote Control needs claude/a@x's own stored login") {
			t.Fatalf("exit %d %q %q; want the reason, not an internal error", code, out, errb)
		}
	})
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
		ID, Name, State string
		Account         struct {
			ID       int
			Provider string
		}
		Ended bool
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil || code != 0 || errb != "" ||
		!holdwire.IsID(s.ID) || s.State != "live" || s.Account.ID != 1 || s.Account.Provider != "claude" || s.Ended {
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

// A held CLI is given ROTA_HOME exactly when a run's CLI is: never, since
// it names the file the store's tokens live in, not even when the command
// that asked had it set, as this one does to name the rig's store. The
// holder runs with it too, and adds nothing of its own.
func TestAHeldCLIGetsROTAHOMEAsARunsCLIDoes(t *testing.T) {
	r := newDaemonRig(t)
	if envValue(r.env, "ROTA_HOME") == "" {
		t.Fatal("the rig names its store by ROTA_HOME, which this test is about")
	}
	r.start()
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(`{"type":"result","subtype":"success","is_error":false,"session_id":"s1","result":"ROTA_HOME={{env:ROTA_HOME|unset}}","num_turns":1}`))
	cmd := r.cmd("run", "1", "hello")
	cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, errb, code := r.run(cmd); code != 0 || !strings.Contains(out, "ROTA_HOME=unset") {
		t.Fatalf("a run's CLI: exit %d %q %q; want ROTA_HOME unset", code, out, errb)
	}
	id, _, _ := r.detach(r.cmd("run", "1", "--detach"))
	p := r.attach(id)
	p.waitOut("ready")
	p.send(0x2, []byte("env ROTA_HOME\r"))
	p.waitOut("env ROTA_HOME unset")
}

// --json prints the session as the daemon describes it, in the shape GET
// /v1/terminals lists it in — the account an object, ended a bool — so one
// reader reads both.
func TestDetachJSONIsTheSessionAsTheAPIDescribesIt(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	out, errb, code := r.rota("--json", "run", "1", "--detach", "--label", "shape")
	var cli map[string]any
	if err := json.Unmarshal([]byte(out), &cli); err != nil || code != 0 || errb != "" {
		t.Fatalf("--json run --detach: exit %d %q %q (%v)", code, out, errb, err)
	}
	id, _ := cli["id"].(string)
	_, doc := ctlGet(t, r.sock, "/v1/terminals")
	var api map[string]any
	list, _ := doc["terminals"].([]any)
	for _, d := range list {
		if m, _ := d.(map[string]any); m["id"] == id {
			api = m
		}
	}
	if api == nil {
		t.Fatalf("GET /v1/terminals lists no %s: %v", id, doc)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(cli)), slices.Sorted(maps.Keys(api))) {
		t.Fatalf("--json says %v\nGET /v1/terminals says %v", slices.Sorted(maps.Keys(cli)), slices.Sorted(maps.Keys(api)))
	}
	for _, key := range []string{"id", "name", "label", "kind", "cwd", "started", "held", "ended", "account", "pid"} {
		if !reflect.DeepEqual(cli[key], api[key]) {
			t.Errorf("%s: --json says %v, GET /v1/terminals %v", key, cli[key], api[key])
		}
	}
	if acct, _ := cli["account"].(map[string]any); acct["id"] != 1.0 || acct["label"] != "a@x" || acct["provider"] != "claude" || cli["ended"] != false {
		t.Errorf("--json: account %v, ended %v", cli["account"], cli["ended"])
	}
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
