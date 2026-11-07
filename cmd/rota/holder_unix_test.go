//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
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
// holder it was lent to is told to, and is waited for.
//
// Before the exec the claim is lent, not kept across it, and the exec closes
// this process's own copy, as the real one does to a close-on-exec file: the
// holder holds it through the exec on its own.
func TestAHandoverWhoseExecFailsLetsGoOfTheLentClaimAtOnce(t *testing.T) {
	lock := seedCodex(t)
	bin := t.TempDir()
	fakecli.Install(t, bin, "codex", fakecli.Spec{})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The holder is this test binary answering to rota's command line.
	t.Setenv("ROTA_AS_RUN", "1")
	orig := execProcess
	t.Cleanup(func() { execProcess = orig })
	var lent, heldThrough bool
	execProcess = func(string, []string, []string) error {
		for _, f := range store.ClaimFiles() {
			if f.Name() != lock {
				continue
			}
			lent = closeOnExecFile(t, f)
			_ = f.Close()
		}
		heldThrough = inUse(t)
		return errors.New("exec failed on purpose")
	}
	if _, errOut, code := call(t, "run", "1"); code == 0 || !strings.Contains(errOut, "exec failed on purpose") {
		t.Fatalf("the failed exec is the run's error: %d %q", code, errOut)
	}
	if !lent {
		t.Fatal("the claim is lent to a holder, still close-on-exec, rather than kept across the exec")
	}
	if !heldThrough {
		t.Fatal("the holder holds the claim once the exec has closed this process's copy")
	}
	if inUse(t) {
		t.Fatal("a handover whose exec failed has let go of the claim by the time it returns")
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

func closeOnExecFile(t *testing.T, f *os.File) bool {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	return flags&syscall.FD_CLOEXEC != 0
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
