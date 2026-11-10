//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
	"github.com/professor93/rota/internal/pty"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

// `rota serve hold` as the daemon starts it: the test binary acting as rota,
// in a session of its own, with its request on standard input and a fake CLI
// on PATH. The test process takes the store's daemon lock, so it is the one
// process the holder lets link.

func TestAHolderRunsTheCLIAndSaysItIsReady(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	req := holdRequest(t)
	h := startHolderProc(t, terminals, bin, req)
	if !h.ready.OK || h.ready.PID != h.cmd.Process.Pid || !proc.Alive(h.ready.PID) {
		t.Fatalf("the holder says it is ready, and is the process it says: %+v (pid %d)", h.ready, h.cmd.Process.Pid)
	}

	if m := modeOf(t, h.dir); m != 0o700 {
		t.Fatalf("the session's directory is %o", m)
	}
	if m := modeOf(t, holdwire.MetaPath(h.dir)); m != 0o600 {
		t.Fatalf("meta.json is %o", m)
	}
	if m := modeOf(t, holdwire.SockPath(h.dir)); m != 0o600 {
		t.Fatalf("the socket is %o", m)
	}
	m := h.meta()
	if m.ID != req.ID || m.Name != "held one" || m.Kind != holdwire.KindAccount || m.Account != 1 || m.Provider != "claude" {
		t.Fatalf("meta.json says what the session is: %+v", m)
	}
	if m.HolderPID != h.ready.PID || m.HolderStart.IsZero() || m.BootID == "" ||
		!holdwire.SameProcess(m.HolderPID, m.HolderStart, m.HolderStartTicks, m.BootID) {
		t.Fatalf("and who holds it: %+v", m)
	}
	// Linux keeps its start in ticks too, which no setting of the clock
	// moves; macOS has no ticks and needs none.
	if (m.HolderStartTicks != 0) != (runtime.GOOS == "linux") {
		t.Fatalf("the holder's start in ticks on %s is %d", runtime.GOOS, m.HolderStartTicks)
	}
	if m.ChildPID <= 0 || m.ChildPID == m.HolderPID || !proc.Alive(m.ChildPID) ||
		!holdwire.SameProcess(m.ChildPID, m.ChildStart, 0, m.BootID) {
		t.Fatalf("and which CLI it holds: %+v", m)
	}
	if m.Protocol != holdwire.Major || m.Version != wire.Version || m.Cwd != req.Cwd ||
		strings.Join(m.Args, " ") != "--model opus" || m.Started.IsZero() {
		t.Fatalf("and the rest: %+v", m)
	}
	if !m.TokenUntil.Equal(h.ready.TokenUntil) || m.Exit != nil || !m.Ended.IsZero() || m.Conversation != "" {
		t.Fatalf("and nothing it does not know yet: %+v (ready %+v)", m, h.ready)
	}
	if !holdwire.Locked(holdwire.LockPath(h.dir)) {
		t.Fatal("a live holder holds its lock")
	}

	l := h.link()
	l.wait("ready 120x40")
}

func TestAHolderTakesNothingFromArgvAndKeepsNoToken(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	h := startHolderProc(t, terminals, bin, holdRequest(t))
	if !h.ready.OK {
		t.Fatalf("%+v", h.ready)
	}
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(h.ready.PID) + "/cmdline")
		if err != nil {
			t.Fatal(err)
		}
		if _, rest, _ := strings.Cut(string(raw), "\x00"); rest != "serve\x00hold\x00" {
			t.Fatalf("the holder's arguments are %q", rest)
		}
	}
	l := h.link()
	l.wait("ready")
	l.typed("bye\r")
	if code := l.exit(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if code := h.code(); code != 0 {
		t.Fatalf("the holder exited %d", code)
	}
	for _, path := range []string{holdwire.MetaPath(h.dir), holdwire.LogPath(h.dir)} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 {
			t.Fatalf("%s is empty", path)
		}
		if bytes.Contains(raw, []byte(holdToken)) || bytes.Contains(raw, []byte("tok-held")) {
			t.Fatalf("%s holds the account's token:\n%s", path, raw)
		}
	}
	if raw, _ := json.Marshal(h.ready); bytes.Contains(raw, []byte("tok-held")) {
		t.Fatalf("the answer holds the token: %s", raw)
	}
}

func TestAHolderRefusesAPerson(t *testing.T) {
	const byHand = "rota serve hold is started by the rota daemon (rota serve start), never by hand"
	holdStore(t)

	// At a terminal: standard input is one, and so is everything else.
	cmd := exec.Command(os.Args[0], "serve", "hold")
	cmd.Env = append(os.Environ(), "ROTA_AS_RUN=1")
	master, err := pty.Start(cmd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	screen := watchScreen(master)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("a holder at a terminal waited for a request")
	}
	if code := cmd.ProcessState.ExitCode(); code != 2 {
		t.Fatalf("at a terminal it exits 2, not %d", code)
	}
	screen.wait(t, byHand)

	// Not at a terminal, but in somebody else's session.
	cmd = exec.Command(os.Args[0], "serve", "hold")
	cmd.Env = append(os.Environ(), "ROTA_AS_RUN=1")
	cmd.Stdin = strings.NewReader(`{"id":"0123456789abcdef","kind":"account","account":1}`)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("outside a session of its own it exits 2: %v %q", err, out.String())
	}
	if !strings.Contains(out.String(), byHand) {
		t.Fatalf("and says why: %q", out.String())
	}
}

func TestTheChildsEnvironmentIsScrubbed(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	req := holdRequest(t)
	// What the terminal the request came from said about itself, and a
	// store that is not the one the daemon serves.
	req.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMUX=x", "CLAUDECODE=1", "TERM_PROGRAM=iTerm.app", "CODEX_THREAD_ID=y", "FOO=bar",
		"ROTA_CONTROL="+holdwire.CtlSock(terminals), "ROTA_HOME=/nowhere/at/all", "TERM=dumb",
	)
	h := startHolderProc(t, terminals, bin, req)
	if !h.ready.OK {
		t.Fatalf("%+v", h.ready)
	}
	l := h.link()
	l.wait("ready")
	for ask, want := range map[string]string{
		"env TMUX":            "env TMUX unset",
		"env CLAUDECODE":      "env CLAUDECODE unset",
		"env TERM_PROGRAM":    "env TERM_PROGRAM unset",
		"env CODEX_THREAD_ID": "env CODEX_THREAD_ID unset",
		"env FOO":             "env FOO=bar",
		"env TERM":            "env TERM=xterm-256color",
		"env COLORTERM":       "env COLORTERM=truecolor",
		"envhas ctl.sock":     "envhas no",
		"envhas /nowhere":     "envhas no",
	} {
		if got := l.line(ask); got != want {
			t.Errorf("%s: the CLI says %q, want %q", ask, got, want)
		}
	}
	// The log says which variable was kept from the CLI, and never what it
	// held.
	raw, err := os.ReadFile(holdwire.LogPath(h.dir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("name=ROTA_CONTROL")) || bytes.Contains(raw, []byte(holdwire.CtlSock(terminals))) {
		t.Fatalf("holder.log names the dropped variable and not its value:\n%s", raw)
	}
}

// A held CLI reads the project settings of the folder it starts in, not of
// wherever its holder runs, so that is the folder the warning about them
// names — in holder.log, where a holder's warnings go. A value is never
// written.
func TestAHeldSessionIsWarnedAboutTheSettingsOfItsOwnFolder(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	put := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere := t.TempDir()
	put(filepath.Join(elsewhere, ".claude", "settings.json"), `{"env":{"CLAUDE_CODE_USE_VERTEX":"SECRET-1"}}`)
	t.Chdir(elsewhere)
	req := holdRequest(t)
	own := filepath.Join(req.Cwd, ".claude", "settings.json")
	put(own, `{"env":{"ANTHROPIC_BASE_URL":"https://SECRET.example"}}`)

	h := startHolderProc(t, terminals, bin, req)
	if !h.ready.OK {
		t.Fatalf("%+v", h.ready)
	}
	h.link().wait("ready")
	raw, err := os.ReadFile(holdwire.LogPath(h.dir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(own+" sets env.ANTHROPIC_BASE_URL")) ||
		bytes.Contains(raw, []byte("SECRET")) || bytes.Contains(raw, []byte("VERTEX")) {
		t.Fatalf("holder.log warns about %s, and only it, with no value:\n%s", own, raw)
	}
}

// The holder reads the CLI's registry while it runs and records the
// conversation it names. What each read does with another claude's file, or
// with none, is tested a read at a time in package hold
// (TestEachRegistryReadKeepsTheConversationItKnows), where no poll has to be
// waited for.
func TestTheConversationComesFromTheRegistry(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	h := startHolderProc(t, terminals, bin, holdRequest(t))
	if !h.ready.OK {
		t.Fatalf("%+v", h.ready)
	}
	l := h.link()
	l.wait("ready")
	m := h.meta()

	want := "fake-" + strconv.Itoa(m.ChildPID)
	if got := l.line("registry idle"); got != "registry idle" {
		t.Fatalf("the fake could not write its registry: %q", got)
	}
	within(t, "meta.json names the conversation", 5*time.Second, func() bool { return h.meta().Conversation == want })
	got := h.meta()
	cwd, _ := filepath.EvalSymlinks(m.Cwd)
	if got.ConversationName != "fake" || (got.ConversationCwd != m.Cwd && got.ConversationCwd != cwd) {
		t.Fatalf("and what it is called and where it works: %+v", got)
	}
}

// heldInUse says whether account id of the store at home is in use: its
// claim held, or for a claude account a Claude Code process alive in its
// home besides.
func heldInUse(t *testing.T, home string, id int) bool {
	t.Helper()
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	return st.InUse(st.Find(id))
}

// Review Focus 1: the account's claim is the CLI's, not the holder's, where
// the CLI is given its claim, as a claude CLI is by an interactive run too
// (lend.Lends).
func TestTheClaimLivesWithTheCLI(t *testing.T) {
	home, terminals := holdStore(t)
	// A claude that takes no notice of its hang-up and stays a few seconds
	// after its terminal goes, as one finishing its work does.
	bin := holdFake(t, "claude", fakecli.Spec{Linger: "3s"})
	req := holdRequest(t)
	req.Args = nil
	h := startHolderProc(t, terminals, bin, req)
	if !h.ready.OK {
		t.Fatalf("%+v", h.ready)
	}
	if !heldInUse(t, home, 1) {
		t.Fatal("an account held by a session is in use")
	}
	child := h.meta().ChildPID
	// The fake takes no notice of a hang-up only once it runs: by the time
	// it says it is ready it does, and not necessarily before.
	h.link().wait("ready")

	if err := h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.code()
	if !proc.Alive(child) {
		t.Fatal("the CLI was meant to outlive its holder for a moment")
	}
	if !heldInUse(t, home, 1) {
		t.Fatal("a killed holder freed the claim while its CLI lives")
	}
	recs := holdwire.Scan(terminals)
	if len(recs) != 1 || recs[0].State != holdwire.StateInterrupted {
		t.Fatalf("a killed holder's session is interrupted: %+v", recs)
	}

	within(t, "the CLI goes", 15*time.Second, func() bool { return gone(child) })
	within(t, "the claim goes with the CLI", 5*time.Second, func() bool { return !heldInUse(t, home, 1) })
}

// A codex CLI is kept from its account's claim, which its holder keeps
// instead, as `rota serve claim` does for an interactive codex run
// (lend.Lends). Codex leaves its app-server daemon running when the CLI
// ends, and a claim the CLI had inherited would go on to the daemon and keep
// the account claimed after the session had ended. Here the fake codex
// leaves behind a daemon of its own, which keeps whatever it inherited.
// While the CLI runs the account is claimed and busy; once the session has
// ended it is free, although the daemon is still there.
func TestAHeldCodexSessionLeavesItsAccountFreeWhileItsDaemonRuns(t *testing.T) {
	home, terminals := holdStore(t)
	daemonUp := filepath.Join(t.TempDir(), "daemon.pid")
	daemon := fakecli.Install(t, t.TempDir(), "codex-daemon",
		fakecli.Spec{KeepStdin: true, Sleep: "60s", Linger: "60s", Ready: daemonUp})
	bin := holdFake(t, "codex", fakecli.Spec{Detach: daemon})
	req := holdRequest(t)
	req.Account, req.Args = 2, nil
	h := startHolderProc(t, terminals, bin, req)
	if !h.ready.OK {
		t.Fatalf("%+v", h.ready)
	}
	l := h.link()
	l.wait("ready")
	daemonPID := fakecli.WaitReady(t, daemonUp)
	t.Cleanup(func() { _ = syscall.Kill(daemonPID, syscall.SIGKILL) })
	if !heldInUse(t, home, 2) {
		t.Fatal("the account is claimed while its CLI runs")
	}

	l.typed("bye\r")
	if code := h.code(); code != 0 {
		t.Fatalf("the holder of a session that ended exits 0, not %d", code)
	}
	within(t, "the account is free once its session has ended", 10*time.Second, func() bool { return !heldInUse(t, home, 2) })
	if !proc.Alive(daemonPID) {
		t.Fatal("while the daemon its CLI left behind still runs")
	}
}

func TestAFailedPrepareSaysWhyAndLeavesNothing(t *testing.T) {
	_, terminals := holdStore(t)
	ran := filepath.Join(t.TempDir(), "ran")
	bin := holdFake(t, "claude", fakecli.Spec{Ready: ran})

	req := holdRequest(t)
	req.Account = 9
	h := startHolderProc(t, terminals, bin, req)
	if h.ready.OK || h.ready.Kind != holdwire.KindNoAccount || h.ready.Error == "" {
		t.Fatalf("no account 9 is said as such: %+v", h.ready)
	}
	if code := h.code(); code != 1 {
		t.Fatalf("and the holder exits 1, not %d", code)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("a CLI ran for a request that failed")
	}
	if _, err := os.Lstat(h.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a holder that failed left its directory: %v", err)
	}
}

// A holder that fails after its CLI started takes the CLI down with it.
func TestAHolderFailingAfterItsCLIStartedLeavesNothingRunning(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	req := holdRequest(t)
	// Something that is not a socket, and cannot be removed, where the
	// socket goes.
	dir := holdwire.Dir(terminals, req.ID)
	if err := os.MkdirAll(filepath.Join(holdwire.SockPath(dir), "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := startHolderProc(t, terminals, bin, req)
	if h.ready.OK || h.ready.Kind != holdwire.KindOther || h.ready.Error == "" {
		t.Fatalf("a socket that cannot be made is a failure: %+v", h.ready)
	}
	if code := h.code(); code != 1 {
		t.Fatalf("the holder exits 1, not %d", code)
	}
	// The directory was not the holder's to remove, so its log is still
	// there to say which CLI it started.
	raw, err := os.ReadFile(holdwire.LogPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`msg="the CLI started" pid=(\d+)`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("the CLI was meant to have started:\n%s", raw)
	}
	pid, _ := strconv.Atoi(string(m[1]))
	if !gone(pid) {
		t.Fatalf("the CLI (pid %d) outlived the holder that failed", pid)
	}
	if _, err := os.Stat(holdwire.MetaPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a holder that failed left a record: %v", err)
	}
	if holdwire.Locked(holdwire.LockPath(dir)) {
		t.Fatal("and its lock is free")
	}
}

// The daemon may go away while a session starts, and so never read its
// answer. By then the CLI runs and its record is whole: the holder keeps the
// session for the next daemon to find, as it would had the daemon gone a
// moment later, rather than dying of the broken pipe.
func TestAHolderWhoseAnswerNobodyReadsKeepsHolding(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	req := holdRequest(t)
	in, give, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	h := launchHolder(t, terminals, bin, req.ID, in, w)
	in.Close()
	w.Close()
	// Nobody is left to read the answer before the holder has its request.
	r.Close()
	if _, err := give.Write(requestLine(t, req)); err != nil {
		t.Fatal(err)
	}
	give.Close()

	said := func(what string) bool {
		raw, _ := os.ReadFile(holdwire.LogPath(h.dir))
		return bytes.Contains(raw, []byte(what))
	}
	within(t, "the holder is ready", 30*time.Second, func() bool {
		select {
		case <-h.exited:
			return true
		default:
			return said(`msg=ready `)
		}
	})
	select {
	case <-h.exited:
		t.Fatalf("a holder whose answer nobody read ended: %v", h.cmd.ProcessState)
	default:
	}
	if !said(`msg="nobody read the answer`) {
		raw, _ := os.ReadFile(holdwire.LogPath(h.dir))
		t.Fatalf("the holder does not say that nobody read its answer:\n%s", raw)
	}
	// The broken pipe is taken, not ignored: an ignored signal stays ignored
	// across exec, and the CLI and every command it runs would start so.
	if runtime.GOOS == "linux" && ignores(t, h.cmd.Process.Pid, syscall.SIGPIPE) {
		t.Fatal("the holder ignores SIGPIPE, and so would its CLI")
	}
	if recs := holdwire.Scan(terminals); len(recs) != 1 || recs[0].State != holdwire.StateLive || !proc.Alive(recs[0].Meta.ChildPID) {
		t.Fatalf("the session is held for the next daemon to find: %+v", recs)
	}

	// And it is held as any other: the next daemon links, and the ending is
	// recorded.
	l := h.link()
	l.wait("ready 120x40")
	l.typed("bye\r")
	if code := l.exit(); code != 0 {
		t.Fatalf("the link is told exit %d", code)
	}
	if code := h.code(); code != 0 {
		t.Fatalf("the holder exits %d (%v)", code, h.cmd.ProcessState)
	}
	if m := h.meta(); m.Exit == nil || *m.Exit != 0 || m.Ended.IsZero() {
		t.Fatalf("meta.json records the ending: %+v", m)
	}
}

func TestTheExitIsRecorded(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	h := startHolderProc(t, terminals, bin, holdRequest(t))
	if !h.ready.OK {
		t.Fatalf("%+v", h.ready)
	}
	l := h.link()
	l.wait("ready")
	l.typed("bye\r")
	if code := l.exit(); code != 0 {
		t.Fatalf("the link is told exit %d", code)
	}
	if code := h.code(); code != 0 {
		t.Fatalf("the holder exits %d", code)
	}
	m := h.meta()
	if m.Exit == nil || *m.Exit != 0 || m.Ended.IsZero() || m.Ended.Before(m.Started) {
		t.Fatalf("meta.json records the ending: %+v", m)
	}
	if holdwire.Locked(holdwire.LockPath(h.dir)) {
		t.Fatal("the lock is free once the holder is gone")
	}
	if _, err := os.Lstat(holdwire.SockPath(h.dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket is gone: %v", err)
	}
	if recs := holdwire.Scan(terminals); len(recs) != 1 || recs[0].State != holdwire.StateEnded {
		t.Fatalf("the session reads ended: %+v", recs)
	}
}

// The daemon stops a holder with SIGTERM; the CLI is hung up, and its ending
// recorded, as if it had ended by itself.
func TestATermToTheHolderHangsUpTheCLI(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	h := startHolderProc(t, terminals, bin, holdRequest(t))
	if !h.ready.OK {
		t.Fatalf("%+v", h.ready)
	}
	child := h.meta().ChildPID
	// The CLI is hung up only once it says it is ready: a signal that
	// reaches a fake sooner can end it before it has run a line.
	h.link().wait("ready")
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := h.code(); code != 0 {
		t.Fatalf("the holder exits %d", code)
	}
	if proc.Alive(child) {
		t.Fatal("the CLI outlived a holder that was stopped")
	}
	if m := h.meta(); m.Exit == nil || m.Ended.IsZero() {
		t.Fatalf("the ending is recorded: %+v", m)
	}
}

// A holder killed outright, with no daemon to see it go, records no
// ending: the session reads interrupted, stopped within a minute — the
// holder's touch — before it was killed, in its record and in `rota list
// --sessions --json` alike. The hold package's
// TestAHolderKilledOutrightIsDatedWithinATouch holds the bound with a
// shorter touch.
func TestAHolderKilledWithNoDaemonStoppedWithinAMinute(t *testing.T) {
	_, terminals := holdStoreAlone(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	req := holdRequest(t)
	h := startHolderProc(t, terminals, bin, req)
	if !h.ready.OK {
		t.Fatalf("%+v", h.ready)
	}
	started := h.meta().Started
	time.Sleep(200 * time.Millisecond)
	killed := time.Now()
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	h.code()
	var r holdwire.Record
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		r = holdwire.Judge(terminals, req.ID)
		if r.State == holdwire.StateInterrupted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session reads %s", r.State)
		}
	}
	if r.Stopped.After(killed) || killed.Sub(r.Stopped) > time.Minute {
		t.Fatalf("the session stopped at %v; it started at %v and was killed at %v", r.Stopped, started, killed)
	}
	out, errb, code := call(t, "--json", "list", "--short", "--sessions")
	var doc struct {
		Held []map[string]any `json:"held"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Held) != 1 {
		t.Fatalf("list --sessions --json: exit %d %q\n%s", code, errb, out)
	}
	got := doc.Held[0]
	if got["state"] != "interrupted" || got["exit"] != float64(-1) || got["ended_at"] != r.Stopped.Format(time.RFC3339) {
		t.Fatalf("the session is listed as %v; it stopped at %s", got, r.Stopped.Format(time.RFC3339))
	}
}
