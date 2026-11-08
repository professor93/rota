//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/lend"
	"github.com/professor93/rota/internal/proc"
	"github.com/professor93/rota/store"
)

// seedCodex writes a store holding one codex account, on a token good for an
// hour so nothing is refreshed, and returns the claim's lock file.
func seedCodex(t *testing.T) (lock string) {
	t.Helper()
	rotaHome := t.TempDir()
	t.Setenv("ROTA_HOME", rotaHome)
	until := time.Now().Add(time.Hour).UnixMilli()
	doc := fmt.Sprintf(`{"accounts":[{"id":1,"provider":"codex","email":"c@x","order":1,`+
		`"token":{"accessToken":"tok","refreshToken":"r","expiresAt":%d},`+
		`"extra":{"id_token":"idt","account_id":"acct"},"staged":"-"}],"nextId":2,"ordered":true}`, until)
	if err := os.WriteFile(filepath.Join(rotaHome, "accounts.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(rotaHome, "homes", "codex-1", ".rota-run.lock")
}

func inUse(t *testing.T) bool {
	t.Helper()
	s, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	return s.InUse(s.Find(1))
}

// The handover lends its claim to a holder rather than keeping it across the
// exec. Codex's TUI leaves its app-server daemon running when it ends, and on
// macOS a codex agent's tools are started holding whatever the CLI did not
// close: a claim kept across the exec went to all of them, and the account
// read busy until the last had gone. Here a real rota replaces itself with a fake
// codex, which leaves a daemon of its own behind as Codex does. While the
// CLI runs the account is claimed and a second run is refused; once it has
// exited the account is free, although the daemon is still there.
func TestAFinishedSessionLeavesItsAccountFreeWhileItsDaemonRuns(t *testing.T) {
	seedCodex(t)
	bin, tmp := t.TempDir(), t.TempDir()
	daemonUp, cliUp := filepath.Join(tmp, "daemon.pid"), filepath.Join(tmp, "cli.pid")
	daemon := fakecli.Install(t, bin, "codex-daemon", fakecli.Spec{KeepStdin: true, Sleep: "60s", Ready: daemonUp})
	fakecli.Install(t, bin, "codex", fakecli.Spec{
		Detach: daemon, KeepStdin: true, Sleep: "60s", Ready: cliUp,
		Term: "exit", TermTouch: filepath.Join(tmp, "cli.done"),
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// A file rather than a pipe: the daemon is left holding what the CLI
	// wrote to, and a pipe would not be done with until the daemon was.
	out, err := os.Create(filepath.Join(tmp, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.Command(os.Args[0], "run", "1")
	cmd.Env = append(os.Environ(), "ROTA_AS_RUN=1")
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	cli := fakecli.WaitReady(t, cliUp)
	daemonPID := fakecli.WaitReady(t, daemonUp)
	t.Cleanup(func() { _ = syscall.Kill(daemonPID, syscall.SIGKILL) })
	if cli != cmd.Process.Pid {
		t.Fatalf("rota became the CLI, keeping its pid: rota %d, the CLI %d", cmd.Process.Pid, cli)
	}

	if !inUse(t) {
		t.Fatal("the account is claimed while the CLI runs")
	}
	if _, errOut, code := call(t, "run", "1"); code == 0 || !strings.Contains(errOut, "already running") {
		t.Fatalf("a second run is refused while the CLI runs: %d %q", code, errOut)
	}

	if err := syscall.Kill(cli, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		raw, _ := os.ReadFile(out.Name())
		t.Fatalf("the CLI exits as it was asked to: %v\n%s", err, raw)
	}
	within(t, "the account is free once the CLI has exited", 10*time.Second, func() bool { return !inUse(t) })
	if !proc.Alive(daemonPID) {
		t.Fatal("while the daemon it left behind is still running")
	}
}

// A handover whose exec fails lets go of the claim before it returns: the
// holder it was lent to is told to, and is waited for. And the claim was
// lent, not kept across the exec: what the exec started, the way an exec
// keeps descriptors, holds none of it.
func TestAHandoverWhoseExecFailsLetsGoOfTheLentClaimAtOnce(t *testing.T) {
	seedCodex(t)
	bin := t.TempDir()
	fakecli.Install(t, bin, "codex", fakecli.Spec{})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The holder is this test binary answering to rota's command line.
	t.Setenv("ROTA_AS_RUN", "1")
	orig := execProcess
	t.Cleanup(func() { execProcess = orig })
	var cli *exec.Cmd
	execProcess = func(string, []string, []string) error {
		// Whatever is not close-on-exec goes with it, as with an exec.
		cli = exec.Command("/bin/sleep", "30")
		if err := cli.Start(); err != nil {
			return err
		}
		return errors.New("exec failed on purpose")
	}
	_, errOut, code := call(t, "run", "1")
	if cli != nil && cli.Process != nil {
		t.Cleanup(func() { _ = cli.Process.Kill(); _ = cli.Wait() })
	}
	if code == 0 || !strings.Contains(errOut, "exec failed on purpose") {
		t.Fatalf("the failed exec is the run's error: %d %q", code, errOut)
	}
	if inUse(t) {
		t.Fatal("a handover whose exec failed has let go of the claim by the time it returns, " +
			"and what the exec started holds none of it")
	}
}

// A handover that panics on its way to the CLI lets go of a claim it lent,
// too, and the panic goes on as it was.
func TestAHandoverThatPanicsLetsGoOfTheLentClaim(t *testing.T) {
	seedCodex(t)
	bin := t.TempDir()
	fakecli.Install(t, bin, "codex", fakecli.Spec{})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ROTA_AS_RUN", "1")
	orig := execProcess
	t.Cleanup(func() { execProcess = orig })
	boom := errors.New("a handover panicking on purpose")
	execProcess = func(string, []string, []string) error { panic(boom) }
	func() {
		defer func() {
			if v := recover(); v != boom {
				t.Fatalf("the panic reaches the caller, as it was: %v", v)
			}
		}()
		call(t, "run", "1")
	}()
	if inUse(t) {
		t.Fatal("the account is let go of on the way out")
	}
}

// Telling a holder to let go returns once it has: once the answer's end is
// closed, which a holder does after its claims. A run whose exec failed
// returns with the account free, not about to be.
func TestLettingGoWaitsForTheHolder(t *testing.T) {
	wordR, wordW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	answerR, answerW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var free atomic.Bool
	go func() {
		var b [1]byte
		if n, _ := wordR.Read(b[:]); n > 0 {
			time.Sleep(200 * time.Millisecond) // a holder slow to close its claims
			free.Store(true)
		}
		_ = answerW.Close()
		_ = wordR.Close()
	}()
	(&holder{word: wordW, answer: answerR}).letGo()
	if !free.Load() {
		t.Fatal("letGo returned before the holder had let go")
	}
}

// exitOf answers when a process has exited, a zombie its parent has yet to
// wait for included, and at once for one that has already gone.
func TestExitOfAnswersWhenAProcessExits(t *testing.T) { exitOfAnswers(t) }

func exitOfAnswers(t *testing.T) {
	t.Helper()
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	wait, err := exitOf(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	gone := make(chan struct{})
	go func() { wait(); close(gone) }()
	select {
	case <-gone:
		t.Fatal("a process still running has not exited")
	case <-time.After(300 * time.Millisecond):
	}
	// Not waited for: a zombie has exited all the same.
	_ = child.Process.Kill()
	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Fatal("never answered that the process exited")
	}
	_ = child.Wait()
	wait, err = exitOf(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a process already gone is answered at once")
	}
}

// `rota serve claim` takes a pid and a count, and nothing else.
func TestServeClaimTakesAPidAndACount(t *testing.T) {
	for _, args := range [][]string{{"serve", "claim"}, {"serve", "claim", "1"}, {"serve", "claim", "x", "1"}, {"serve", "claim", "1", "0"}, {"serve", "claim", "1", "1", "2"}} {
		if _, errOut, code := call(t, args...); code != 2 || !strings.Contains(errOut, "serve claim") {
			t.Fatalf("%v: %d %q", args, code, errOut)
		}
	}
}

// lendToHolder takes the codex account's claim in this process, lends it to a
// real holder watching this process, and lets go of this process's own: the
// holder alone holds it then.
func lendToHolder(t *testing.T, lock string) *holder {
	t.Helper()
	s, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	release, ok := s.Hold(s.Find(1))
	_ = s.Close()
	if !ok {
		t.Fatal("the account was claimed already")
	}
	defer release()
	copies, err := lend.Claims()
	if err != nil {
		t.Fatal(err)
	}
	var files []*os.File
	for _, f := range copies {
		defer f.Close()
		if f.Name() == lock {
			files = append(files, f)
		}
	}
	got, err := holdInBackground(files)
	if err != nil {
		t.Fatal(err)
	}
	h := got.(*holder)
	t.Cleanup(func() { _ = syscall.Kill(h.pid, syscall.SIGKILL); h.close() })
	return h
}

// A holder lets go when the CLI it holds for exits, or when the run says so,
// and for nothing else short of SIGKILL: the signals that ask a process to
// end — what `pkill rota` sends to stop a server — leave every running
// session's account claimed. SIGKILL frees it early, as it did the CLI that
// held it before.
func TestOnlySIGKILLEndsAHolderEarly(t *testing.T) {
	lock := seedCodex(t)
	t.Setenv("ROTA_AS_RUN", "1")
	h := lendToHolder(t, lock)
	if !inUse(t) {
		t.Fatal("the holder holds the claim")
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT} {
		if err := syscall.Kill(h.pid, sig); err != nil {
			t.Fatalf("%v: %v", sig, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if !proc.Alive(h.pid) || !inUse(t) {
		t.Fatal("asked to end, the holder goes on holding the claim")
	}
	h.letGo()
	if inUse(t) {
		t.Fatal("told to by the run, it has let go")
	}

	h = lendToHolder(t, lock)
	if err := syscall.Kill(h.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	within(t, "SIGKILL ends the holder, and the claim with it", 10*time.Second, func() bool { return !inUse(t) })
}

// A holder typed by hand, without what a run lends it at fds 3 on — two
// pipes and the claims — refuses to run.
func TestAHolderLentNothingRefuses(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	plain := make([]*os.File, 3)
	for i := range plain {
		f, err := os.CreateTemp(t.TempDir(), "plain")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		plain[i] = f
	}
	for _, c := range []struct {
		what  string
		extra []*os.File
	}{{"nothing at all", nil}, {"files where the pipes go", plain}} {
		cmd := exec.Command(os.Args[0], "serve", "claim", strconv.Itoa(os.Getpid()), "1")
		cmd.Env = append(os.Environ(), "ROTA_AS_RUN=1")
		cmd.ExtraFiles = c.extra
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(out), "serve claim is internal") {
			t.Fatalf("%s: %v %q", c.what, err, out)
		}
	}
}

// Only a codex run lends its claim. The handover asks for a holder for a
// codex account, and for a claude account it does not: the claim is kept
// across the exec, as it always was, and the CLI holds it alone once the
// handover has let go of its own. A codex account whose holder cannot be had
// is kept across the exec the same way. Here the replacing is a child
// started the way an exec keeps descriptors.
func TestOnlyACodexRunLendsItsClaim(t *testing.T) {
	for _, c := range []struct {
		provider string
		lends    bool
	}{{"claude", false}, {"codex", true}} {
		t.Run(c.provider, func(t *testing.T) {
			if c.lends {
				seedCodex(t)
				bin := t.TempDir()
				fakecli.Install(t, bin, "codex", fakecli.Spec{})
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			} else {
				seedLiving(t, "")
			}
			origHolder, origExec := startHolder, execProcess
			t.Cleanup(func() { startHolder, execProcess = origHolder, origExec })
			asked := 0
			startHolder = func([]*os.File) (claimHolder, error) {
				asked++
				return nil, errNoHolder
			}
			var cli *exec.Cmd
			execProcess = func(string, []string, []string) error {
				cli = exec.Command("/bin/sleep", "30")
				return cli.Start()
			}
			if _, errOut, code := call(t, "run", "1"); code != 0 {
				t.Fatalf("%d %q", code, errOut)
			}
			if cli == nil || cli.Process == nil {
				t.Fatal("the handover never happened")
			}
			t.Cleanup(func() { _ = cli.Process.Kill(); _ = cli.Wait() })
			if lent := asked > 0; lent != c.lends {
				t.Fatalf("a holder was asked for %d times", asked)
			}
			if !inUse(t) {
				t.Fatal("the CLI the handover became holds the account's claim")
			}
			_ = cli.Process.Kill()
			_ = cli.Wait()
			within(t, "the claim is free once the CLI has gone", 10*time.Second, func() bool { return !inUse(t) })
		})
	}
}

// A claude run keeps its claim across the exec and starts no holder, even
// where one could be had, as before holders were: Claude Code's daemon was
// seen holding no claim of rota's, and a claude account reads in use while
// any claude process lives in its home besides. Here a real rota replaces
// itself with a fake claude that leaves a daemon behind, which, unlike
// Claude Code's, keeps what it inherited. That shows where the claim went:
// to the CLI, and through it to the daemon, which still holds it once the
// CLI has exited, where a holder would have let go then. A second run of
// the account starts while the first runs, its claim being shared.
func TestAClaudeRunKeepsItsClaimAcrossTheExec(t *testing.T) {
	seedLiving(t, "")
	bin, tmp := t.TempDir(), t.TempDir()
	daemonUp, cliUp := filepath.Join(tmp, "daemon.pid"), filepath.Join(tmp, "cli.pid")
	daemon := fakecli.Install(t, bin, "claude-daemon", fakecli.Spec{KeepStdin: true, Sleep: "60s", Ready: daemonUp})
	fakecli.Install(t, bin, "claude", fakecli.Spec{
		Detach: daemon, KeepStdin: true, Sleep: "60s", Ready: cliUp,
		Term: "exit", TermTouch: filepath.Join(tmp, "cli.done"),
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := os.Create(filepath.Join(tmp, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.Command(os.Args[0], "run", "1")
	// Where a holder could be had, as it is for codex.
	cmd.Env = append(os.Environ(), "ROTA_AS_RUN=1")
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	cli := fakecli.WaitReady(t, cliUp)
	daemonPID := fakecli.WaitReady(t, daemonUp)
	t.Cleanup(func() { _ = syscall.Kill(daemonPID, syscall.SIGKILL) })
	if cli != cmd.Process.Pid {
		t.Fatalf("rota became the CLI, keeping its pid: rota %d, the CLI %d", cmd.Process.Pid, cli)
	}
	if !inUse(t) {
		t.Fatal("the account is claimed while the CLI runs")
	}
	argv := handover(t)
	if _, errOut, code := call(t, "run", "1"); code != 0 || len(*argv) == 0 {
		t.Fatalf("a second run of a claude account starts while the first runs: %d %q", code, errOut)
	}
	if err := syscall.Kill(cli, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		raw, _ := os.ReadFile(out.Name())
		t.Fatalf("the CLI exits as it was asked to: %v\n%s", err, raw)
	}
	// A holder lets go within moments of the CLI's exit; what the CLI left
	// holding the claim does not.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !inUse(t) {
			raw, _ := os.ReadFile(out.Name())
			t.Fatalf("the claim went across the exec, to the daemon the CLI left, not to a holder\n%s", raw)
		}
	}
	if err := syscall.Kill(daemonPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	within(t, "the account is free once the daemon holding the claim has gone", 10*time.Second, func() bool { return !inUse(t) })
}

// What a holder answers is read for its pid, for a holder that cannot be
// had here, which is no failure, or for why it failed.
func TestTheHoldersAnswerIsRead(t *testing.T) {
	for _, c := range []struct {
		said string
		pid  int
		none bool
		why  string
	}{
		{said: "ready 4242\n", pid: 4242},
		{said: "none\n", none: true},
		{said: "fail kqueue: too many open files\n", why: "kqueue: too many open files"},
		{said: "ready\n", why: `the holder said "ready"`},
		{said: "ready -1\n", why: `the holder said "ready -1"`},
		{said: "fail\n", why: `the holder said "fail"`},
		{said: "what\n", why: `the holder said "what"`},
	} {
		pid, err := heard(c.said)
		switch {
		case c.none && !errors.Is(err, errNoHolder),
			c.why != "" && (err == nil || err.Error() != c.why),
			c.pid != pid:
			t.Errorf("%q: %d %v", c.said, pid, err)
		}
	}
}
