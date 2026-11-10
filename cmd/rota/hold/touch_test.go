//go:build linux || darwin

package hold

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
	"github.com/professor93/rota/store"
)

func mtime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// TestARunningHolderTouchesItsRecordEveryMinute: a holder that goes without
// recording an ending, with no daemon to see it go, is dated by the last
// time it touched its record, so it touches it at least every minute while
// the CLI runs, and stops once the CLI has ended.
func TestARunningHolderTouchesItsRecordEveryMinute(t *testing.T) {
	if touchEvery > time.Minute {
		t.Fatalf("the record is touched every %v", touchEvery)
	}
	old := touchEvery
	touchEvery = 20 * time.Millisecond
	t.Cleanup(func() { touchEvery = old })

	c := newFakeChild()
	r := newRig(t, 0, c)
	dir := holdwire.Dir(r.terminals, testID)
	if err := holdwire.WriteMeta(dir, &holdwire.Meta{ID: testID, Kind: holdwire.KindShell}); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-time.Hour)
	if err := os.Chtimes(holdwire.MetaPath(dir), long, long); err != nil {
		t.Fatal(err)
	}
	h := &holder{sess: r.s, rec: &metaFile{dir: dir}, log: func(string, ...any) {}}
	kept := make(chan struct{})
	go func() {
		defer close(kept)
		h.keep()
	}()
	for deadline := time.Now().Add(5 * time.Second); mtime(t, holdwire.MetaPath(dir)).Before(time.Now().Add(-time.Second)); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the record was never touched")
		}
	}
	c.exit(0)
	select {
	case <-kept:
	case <-time.After(5 * time.Second):
		t.Fatal("the holder kept the record after its CLI ended")
	}
}

// TestATouchNeverMovesARecordPastItsEnding: once the ending is recorded the
// touch leaves the record's time alone, and the ending is when the session
// stopped whatever the record's time says.
func TestATouchNeverMovesARecordPastItsEnding(t *testing.T) {
	terminals := filepath.Join(t.TempDir(), "terminals")
	dir := holdwire.Dir(terminals, testID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := &metaFile{dir: dir, m: holdwire.Meta{ID: testID, Kind: holdwire.KindShell, Started: time.Now().Add(-2 * time.Hour)}}
	ended := time.Now().Add(-time.Hour)
	code := 3
	if err := rec.update(func(m *holdwire.Meta) { m.Ended, m.Exit = ended, &code }); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(holdwire.MetaPath(dir), ended, ended); err != nil {
		t.Fatal(err)
	}
	if err := rec.touch(); err != nil {
		t.Fatal(err)
	}
	if at := mtime(t, holdwire.MetaPath(dir)); !at.Equal(ended) {
		t.Fatalf("a touch after the ending moved the record to %v", at)
	}
	// However the time moved — by hand, or by anything else — the ending
	// recorded wins.
	now := time.Now()
	if err := os.Chtimes(holdwire.MetaPath(dir), now, now); err != nil {
		t.Fatal(err)
	}
	if r := holdwire.Judge(terminals, testID); r.State != holdwire.StateEnded || !r.Stopped.Equal(ended) {
		t.Fatalf("the record reads %s, stopped at %v, not at its ending %v", r.State, r.Stopped, ended)
	}
}

// TestHelperHolds is `rota serve hold` for TestAHolderKilledOutrightIsDatedWithinATouch,
// touching its record every ROTA_HOLD_TEST_TOUCH.
func TestHelperHolds(t *testing.T) {
	home := os.Getenv("ROTA_HOLD_TEST_HOME")
	if home == "" {
		t.Skip("a helper process")
	}
	every, err := time.ParseDuration(os.Getenv("ROTA_HOLD_TEST_TOUCH"))
	if err != nil {
		t.Fatal(err)
	}
	touchEvery = every
	os.Exit(Main(os.Stdin, os.Stdout, func() (*store.Store, error) { return store.Open(home) }))
}

// TestAHolderKilledOutrightIsDatedWithinATouch: a holder killed outright
// records no ending, and with no daemon to see it go its session reads
// interrupted, stopped no earlier than one touch before the kill.
func TestAHolderKilledOutrightIsDatedWithinATouch(t *testing.T) {
	const every = 200 * time.Millisecond
	home, err := os.MkdirTemp("", "rh") // short enough for a session's socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	terminals, err := st.TerminalDir()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	shell := fakecli.Install(t, t.TempDir(), "sh", fakecli.Spec{Tty: true})

	req, err := json.Marshal(holdwire.Request{ID: testID, Kind: holdwire.KindShell, Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHolds$")
	cmd.Env = append(os.Environ(), "ROTA_HOLD_TEST_HOME="+home, "ROTA_HOLD_TEST_TOUCH="+every.String(), "SHELL="+shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = bytes.NewReader(append(req, '\n'))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	dir := holdwire.Dir(terminals, testID)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
		if m, err := holdwire.ReadMeta(dir); err == nil && proc.Alive(m.ChildPID) {
			_ = syscall.Kill(m.ChildPID, syscall.SIGKILL)
		}
	})
	var ready holdwire.Ready
	if err := json.NewDecoder(out).Decode(&ready); err != nil || !ready.OK {
		t.Fatalf("the holder answered %+v (%v)", ready, err)
	}
	m, err := holdwire.ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Several touches, so that the start is long past when it is killed.
	time.Sleep(6 * every)
	killed := time.Now()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-exited
	for deadline := time.Now().Add(5 * time.Second); holdwire.Locked(holdwire.LockPath(dir)); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the lock outlived the holder")
		}
	}
	r := holdwire.Judge(terminals, testID)
	if r.State != holdwire.StateInterrupted {
		t.Fatalf("the session reads %s", r.State)
	}
	// One touch, and room for a tick a loaded machine is late with.
	const slack = 300 * time.Millisecond
	if r.Stopped.After(killed) || killed.Sub(r.Stopped) > every+slack {
		t.Fatalf("the session stopped at %v; it started at %v and was killed at %v, touched every %v", r.Stopped, m.Started, killed, every)
	}
}
