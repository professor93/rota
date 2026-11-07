//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/wire"
)

// What the daemon refuses, and what it does not wait for: serve stop from
// inside a held session, a root named relatively, a readiness probe that
// would wait on a silent socket — and the binary its holders come from
// after an upgrade.

// The session probe is this test binary as a rota whose commands come one
// per line on standard input, each answered on one line with its exit
// status and what it said: a rota that stays in the session it was started
// in, which a test can make a held session's before it asks anything.
func init() {
	if os.Getenv("ROTA_SESSION_PROBE") != "1" {
		return
	}
	lines := bufio.NewScanner(os.Stdin)
	for lines.Scan() {
		var out, errb bytes.Buffer
		code := run(strings.Fields(lines.Text()), &out, &errb)
		fmt.Printf("%d %q\n", code, out.String()+errb.String())
	}
	os.Exit(0)
}

// sessionProbe starts the session probe in a session of its own, with env,
// and returns its pid, which leads that session, and a way to ask it one
// command line: the exit status and what it said. It is stopped when the
// test ends.
func sessionProbe(t *testing.T, env []string) (pid int, ask func(line string) (int, string)) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = slices.Concat(env, []string{"ROTA_SESSION_PROBE=1"})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	answers := bufio.NewScanner(out)
	return cmd.Process.Pid, func(line string) (int, string) {
		t.Helper()
		fmt.Fprintln(in, line)
		if !answers.Scan() {
			t.Fatalf("the probe said nothing to %q: %v", line, answers.Err())
		}
		code, said, _ := strings.Cut(answers.Text(), " ")
		n, err := strconv.Atoi(code)
		if err != nil {
			t.Fatalf("the probe answered %q", answers.Text())
		}
		s, err := strconv.Unquote(said)
		if err != nil {
			t.Fatalf("the probe answered %q", answers.Text())
		}
		return n, s
	}
}

// rota serve stop, with --all or without, is refused inside a held session
// — run by the agent in one, or anything it started — as ctl.sock lets such
// a caller only watch: a guard against ending the daemon, or every session,
// from inside one by mistake. Once the session is over, the same process
// may.
func TestStopIsRefusedInsideAHeldSession(t *testing.T) {
	r := newDaemonRig(t)
	daemon, _ := r.start()
	probe, ask := sessionProbe(t, r.env)

	// A session whose holder, and then whose CLI, leads the probe's session.
	const id = "00000000000000d1"
	dir := holdwire.Dir(r.terminals, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := func(holder, child int) {
		t.Helper()
		m := &holdwire.Meta{ID: id, Kind: holdwire.KindAccount, Account: 1, Provider: "claude",
			HolderPID: holder, ChildPID: child, Protocol: holdwire.Major, Version: wire.Version, Started: time.Now()}
		if err := holdwire.WriteMeta(dir, m); err != nil {
			t.Fatal(err)
		}
	}
	release, err := holdwire.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	for _, c := range []struct {
		holder, child int
		line          string
	}{
		{probe, 0, "serve stop"},
		{0, probe, "serve stop --all --yes"},
	} {
		record(c.holder, c.child)
		code, said := ask(c.line)
		if code != 1 || !strings.Contains(said, "inside held session "+id) || !strings.Contains(said, "run it from outside the session") {
			t.Fatalf("%s inside a held session: exit %d %q", c.line, code, said)
		}
	}
	if d, ok := holdwire.RunningDaemon(r.terminals); !ok || d.PID != daemon {
		t.Fatalf("the daemon was stopped from inside a held session: %+v", d)
	}

	release()
	if code, said := ask("serve stop"); code != 0 || said != "rota daemon stopped; 0 sessions keep running\n" {
		t.Fatalf("serve stop once the session was over: exit %d %q", code, said)
	}
}

// A daemon runs in /, so a path in its file named relatively would be one
// under / rather than the one meant. Daemon mode refuses each by its key,
// whether it is started in the background or by a supervisor, and nothing
// serves.
func TestTheDaemonRefusesARelativePath(t *testing.T) {
	r := newDaemonRig(t)
	for _, c := range []struct{ key, file string }{
		{`runs.roots: "work"`, "[runs]\nroots = [\"work\"]\n"},
		{`tls.cert: "cert.pem"`, "[tls]\ncert = \"cert.pem\"\nkey = \"/abs/key.pem\"\n"},
		{`tls.key: "key.pem"`, "[tls]\ncert = \"/abs/cert.pem\"\nkey = \"key.pem\"\n"},
		{`auth.token_file: "token"`, "[auth]\ntoken_file = \"token\"\n"},
		{`store.dir: "store"`, "[store]\ndir = \"store\"\n"},
	} {
		if err := os.WriteFile(filepath.Join(r.home, "server.toml"), []byte(c.file), 0o600); err != nil {
			t.Fatal(err)
		}
		// The log quotes the reason, and the quotes inside it are escaped.
		refused := func(said string) bool {
			said = strings.ReplaceAll(said, `\"`, `"`)
			return strings.Contains(said, c.key+" is relative") && strings.Contains(said, "absolute in daemon mode")
		}
		if out, errb, code := r.rota("serve", "start"); code != 1 || out != "" || !refused(errb) {
			t.Fatalf("serve start with %s: exit %d %q %q", c.key, code, out, errb)
		}
		if out, errb, code := r.rota("serve", "start", "--foreground"); code != 1 || out != "" || errb != "" || !refused(r.log()) {
			t.Fatalf("serve start --foreground with %s: exit %d %q %q\nlog:\n%s", c.key, code, out, errb, r.log())
		}
		if _, ok := holdwire.RunningDaemon(r.terminals); ok {
			t.Fatalf("a daemon serves with %s", c.key)
		}
	}
}

// The readiness probe asks once and does not wait: a daemon that holds the
// store and does not answer is not ready yet, said at once, so serve start
// waits daemonReady in all and no longer.
func TestTheReadinessProbeDoesNotWait(t *testing.T) {
	r := newDaemonRig(t)
	release, err := holdwire.ClaimDaemon(r.terminals, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: r.sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	began := time.Now()
	if d := daemonAnswering(r.terminals); d != nil || time.Since(began) > time.Second {
		t.Fatalf("a silent daemon was probed as %+v after %s", d, time.Since(began))
	}
}

// The holders a daemon starts come from the binary its path names now: the
// one it resolved when it started, for as long as that is there — a binary
// replaced in place included — and once an upgrade has taken that one away,
// the one the path it was started by leads to now.
func TestHoldersFollowAnUpgrade(t *testing.T) {
	dir := t.TempDir()
	v1 := filepath.Join(dir, "1.4.0", "rota")
	v2 := filepath.Join(dir, "1.4.1", "rota")
	for _, v := range []string{v1, v2} {
		if err := os.MkdirAll(filepath.Dir(v), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(v, []byte(v), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	real := func(p string) string {
		t.Helper()
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first, second := real(v1), real(v2)
	link := filepath.Join(dir, "rota")
	if err := os.Symlink(v1, link); err != nil {
		t.Fatal(err)
	}
	hold := daemonHoldCommand(link)
	if c := hold(); c.Path != first || !slices.Equal(c.Args[1:], []string{"serve", "hold"}) {
		t.Fatalf("a holder is %s %q, want %s serve hold", c.Path, c.Args, first)
	}
	// Replaced in place: the same path, a new binary.
	if err := os.WriteFile(v1+".new", []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(v1+".new", v1); err != nil {
		t.Fatal(err)
	}
	if c := hold(); c.Path != first {
		t.Fatalf("after a replacement in place a holder is %s, want %s", c.Path, first)
	}
	// Upgraded: the link moves on and the old version goes.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(v2, link); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(v1)); err != nil {
		t.Fatal(err)
	}
	if c := hold(); c.Path != second {
		t.Fatalf("after an upgrade a holder is %s, want %s", c.Path, second)
	}
	// Gone altogether: the path as it was given, for exec to say what is
	// missing.
	if err := os.RemoveAll(filepath.Dir(v2)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if c := hold(); c.Path != link {
		t.Fatalf("with nothing left a holder is %s, want %s", c.Path, link)
	}
}

// invokedAs is the path rota was started by, with its link kept: as typed,
// or found on PATH. An argv[0] that is not this very binary — one a parent
// made up, or a name PATH does not find — is not believed, and the
// executable's own path is used.
func TestInvokedAsKeepsTheLinkItWasStartedBy(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	was := os.Args[0]
	t.Cleanup(func() { os.Args[0] = was })
	bin := t.TempDir()
	link := filepath.Join(bin, "rota-under-test")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(bin, "not-rota")
	if err := os.WriteFile(other, []byte("not this binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, c := range []struct{ arg0, want string }{
		{link, link},                          // typed as a path
		{"rota-under-test", link},             // found on PATH
		{other, self},                         // a path, but to another file
		{"not-rota", self},                    // found on PATH, but another file
		{"nowhere-on-path", self},             // not found at all
		{filepath.Join(bin, "missing"), self}, // a path to nothing
	} {
		os.Args[0] = c.arg0
		if got, err := invokedAs(); err != nil || got != c.want {
			t.Errorf("argv[0] %q: invokedAs is %q (%v), want %q", c.arg0, got, err, c.want)
		}
	}
}

// A daemon runs in /, so start hands it the person's Claude Code directory
// by absolute path, as it does the store, when the environment names it
// relatively: a session's CLI is then told the directory meant, not one
// under /.
func TestStartNamesTheClaudeDirectoryAbsolutely(t *testing.T) {
	r := newDaemonRig(t)
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "person-claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := r.cmd("serve", "start")
	cmd.Dir = parent
	cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR=person-claude", "PWD="+parent)
	r.startWith(cmd)
	p := r.attach(r.newSession("where"))
	p.waitOut("ready 100x30")
	p.send(0x2, []byte("env ROTA_CLAUDE_HOME\r"))
	// The fake ends a line in CR LF, and the terminal turns its LF into
	// another CR LF: the CR right after the path says the path ends there.
	p.waitOut("env ROTA_CLAUDE_HOME=" + filepath.Join(parent, "person-claude") + "\r")
}
