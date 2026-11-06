//go:build linux || darwin

package holdwire

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/proc"
)

// TestHelperHolder is not a test: it is the holder the tests below stop,
// run as a child of this test binary. It takes the session's lock, says it
// is ready, and writes down every hangup it gets; it ends on the first one
// unless it was told to be deaf, so the kill that follows is what ends it.
func TestHelperHolder(t *testing.T) {
	dir := os.Getenv("HOLDWIRE_HELPER_DIR")
	if dir == "" {
		t.Skip("run as a child by the stop tests")
	}
	release, err := Lock(dir)
	if err != nil {
		os.Exit(2)
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	os.Stdout.WriteString("ready\n")
	time.AfterFunc(time.Minute, func() { os.Exit(3) })
	for range hup {
		f, _ := os.OpenFile(filepath.Join(dir, "signals"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		f.WriteString("hangup\n")
		f.Close()
		if os.Getenv("HOLDWIRE_HELPER_DEAF") == "" {
			release()
			os.Exit(0)
		}
	}
}

// helper starts one helper holder and returns its record, with the start
// time and boot id a real holder would have written.
func helper(t *testing.T, deaf bool) (*Record, *exec.Cmd) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHolder$")
	cmd.Env = append(os.Environ(), "HOLDWIRE_HELPER_DIR="+dir)
	if deaf {
		cmd.Env = append(cmd.Env, "HOLDWIRE_HELPER_DEAF=1")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the helper said %q (%v)", line, err)
	}
	start, err := proc.StartTime(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := proc.BootID()
	if err != nil {
		t.Fatal(err)
	}
	m := &Meta{ID: "0123456789abcdef", HolderPID: cmd.Process.Pid, HolderStart: start, BootID: boot}
	return &Record{ID: m.ID, Dir: dir, Meta: m, State: StateLive}, cmd
}

func hangups(r *Record) int {
	raw, _ := os.ReadFile(filepath.Join(r.Dir, "signals"))
	return strings.Count(string(raw), "hangup")
}

// A holder is hung up, and once its lock is free it is left alone: the
// kill is only for a holder still holding on when the time is up.
func TestStopHolderHangsUp(t *testing.T) {
	r, cmd := helper(t, false)
	kill, err := StopHolder(r, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	asked := time.Now()
	kill()
	if took := time.Since(asked); took >= 5*time.Second {
		t.Fatalf("the kill waited out its time (%s) for a holder that had gone", took)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the holder did not end on its own: %v", err)
	}
	if hangups(r) != 1 {
		t.Fatalf("the holder got %d hangups, want 1", hangups(r))
	}
}

// One that ignores the hangup and keeps its lock is killed when the time is
// up.
func TestStopHolderKillsWhatWillNotStop(t *testing.T) {
	r, cmd := helper(t, true)
	kill, err := StopHolder(r, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	kill()
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("a deaf holder ended with %v, want SIGKILL", err)
	}
	if hangups(r) != 1 {
		t.Fatalf("the holder got %d hangups, want 1 before the kill", hangups(r))
	}
}

// A record whose pid is no longer the process it names is never signalled:
// the kernel hands pids out again, and the process with it now may be
// anybody's.
func TestStopHolderSignalsNothingThatIsNotTheHolder(t *testing.T) {
	r, cmd := helper(t, false)
	r.Meta.HolderStart = r.Meta.HolderStart.Add(time.Hour)
	if _, err := StopHolder(r, time.Second); !errors.Is(err, ErrNotSameProcess) {
		t.Fatalf("a record an hour off answers %v, want ErrNotSameProcess", err)
	}
	if err := Signal(r.Meta.HolderPID, r.Meta.HolderStart, r.Meta.BootID, syscall.SIGHUP); !errors.Is(err, ErrNotSameProcess) {
		t.Fatalf("Signal answers %v, want ErrNotSameProcess", err)
	}
	time.Sleep(100 * time.Millisecond)
	if hangups(r) != 0 || cmd.ProcessState != nil {
		t.Fatal("a process the record does not name was signalled")
	}
	for _, bad := range []*Record{{ID: r.ID, Dir: r.Dir}, {ID: r.ID, Dir: r.Dir, Meta: &Meta{}}} {
		if _, err := StopHolder(bad, time.Second); !errors.Is(err, ErrNotSameProcess) {
			t.Fatalf("a record naming nobody answers %v", err)
		}
	}
}
