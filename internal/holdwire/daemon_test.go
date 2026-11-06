//go:build linux || darwin

package holdwire

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/professor93/rota/internal/proc"
)

// One store has one daemon: the second claim is told whose it is, and a
// claim let go of can be made again.
func TestTwoDaemonsCannotClaimOneStore(t *testing.T) {
	terminals := t.TempDir()
	if d, ok := RunningDaemon(terminals); ok {
		t.Fatalf("a daemon runs before any claimed: %+v", d)
	}
	release, err := ClaimDaemon(terminals, "1.4.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ClaimDaemon(terminals, "1.4.0-other")
	var running DaemonRunning
	if !errors.As(err, &running) || running != (DaemonRunning{PID: os.Getpid()}) {
		t.Fatalf("the second claim: %v", err)
	}
	d, ok := RunningDaemon(terminals)
	if !ok || d == nil {
		t.Fatal("a held claim is not running")
	}
	start, _ := proc.StartTime(os.Getpid())
	boot, _ := proc.BootID()
	if d.PID != os.Getpid() || d.Version != "1.4.0-test" || d.BootID != boot || !d.ProcStart.Equal(start) ||
		time.Since(d.Started) > time.Minute {
		t.Fatalf("daemon.pid says %+v", d)
	}
	if fi, err := os.Stat(DaemonPIDFile(terminals)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("daemon.pid: %v %v", fi, err)
	}

	release()
	release()
	if d, ok := RunningDaemon(terminals); ok {
		t.Fatalf("a released claim still runs: %+v", d)
	}
	if _, err := os.Stat(DaemonPIDFile(terminals)); !os.IsNotExist(err) {
		t.Fatalf("daemon.pid outlived its daemon: %v", err)
	}
	again, err := ClaimDaemon(terminals, "1.4.0-test")
	if err != nil {
		t.Fatalf("a free store could not be claimed: %v", err)
	}
	again()
}

// A pid is the process that was recorded only while its start time and the
// boot it runs in both still match: anything else is a pid reused.
func TestSameProcessIsAboutThisProcess(t *testing.T) {
	pid := os.Getpid()
	start, err := proc.StartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := proc.BootID()
	if err != nil {
		t.Fatal(err)
	}
	if !SameProcess(pid, start, boot) {
		t.Fatal("this process is not itself")
	}
	if !SameProcess(pid, start.Add(time.Second), boot) {
		t.Fatal("a second's slack is not allowed")
	}
	if SameProcess(pid, start.Add(5*time.Second), boot) {
		t.Fatal("a process five seconds younger is this one")
	}
	if SameProcess(pid, start, boot+"-another") {
		t.Fatal("a process from another boot is this one")
	}
	if SameProcess(pid, time.Time{}, boot) || SameProcess(0, start, boot) || SameProcess(pid, start, "") {
		t.Fatal("a record with something missing is trusted")
	}
}
