//go:build linux || darwin

package holdwire

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/professor93/rota/internal/proc"
	rota "github.com/professor93/rota/lib"
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

// A daemon.pid left by a daemon that crashed names nobody. With the lock
// free nothing runs and the store is claimed over it; with the lock held, by
// a daemon that has yet to write who it is, the old record is still not that
// daemon, so nobody is named rather than a pid that may be anybody's.
func TestAStaleDaemonPIDNamesNobody(t *testing.T) {
	pid := os.Getpid()
	start, err := proc.StartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := proc.BootID()
	if err != nil {
		t.Fatal(err)
	}
	stale := func(t *testing.T, terminals string, d Daemon) {
		t.Helper()
		raw, err := rota.Encode(&d)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(DaemonPIDFile(terminals), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// This very pid, so only the start time and the boot can tell the record
	// from this process.
	for name, d := range map[string]Daemon{
		"another start": {PID: pid, Version: "1.3.0", ProcStart: start.Add(-time.Hour), BootID: boot},
		"another boot":  {PID: pid, Version: "1.3.0", ProcStart: start, BootID: boot + "-before"},
		"nothing known": {PID: pid, Version: "1.3.0"},
	} {
		t.Run(name, func(t *testing.T) {
			terminals := t.TempDir()
			stale(t, terminals, d)
			if err := os.WriteFile(DaemonLock(terminals), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if got, ok := RunningDaemon(terminals); ok {
				t.Fatalf("a crashed daemon runs: %+v", got)
			}

			held, err := tryLock(DaemonLock(terminals))
			if err != nil {
				t.Fatal(err)
			}
			got, ok := RunningDaemon(terminals)
			if !ok || got == nil || got.PID != 0 {
				t.Errorf("a daemon not yet written down is %+v (%v)", got, ok)
			}
			_, err = ClaimDaemon(terminals, "1.4.0-test")
			var running DaemonRunning
			if !errors.As(err, &running) || running.PID != 0 {
				t.Errorf("a claim refused by a daemon not yet written down: %v", err)
			}
			unlock(held)

			release, err := ClaimDaemon(terminals, "1.4.0-test")
			if err != nil {
				t.Fatalf("a store whose daemon crashed could not be claimed: %v", err)
			}
			defer release()
			got, ok = RunningDaemon(terminals)
			if !ok || got.PID != pid || got.Version != "1.4.0-test" || !got.ProcStart.Equal(start) || got.BootID != boot {
				t.Fatalf("the claim left daemon.pid as %+v (%v)", got, ok)
			}
		})
	}
}

// The daemon says where else it serves, in its own record and nowhere else:
// before its claim, or after it is let go of, nothing is written.
func TestOnlyTheClaimRecordsItsHTTP(t *testing.T) {
	terminals := t.TempDir()
	if err := RecordHTTP(terminals, "127.0.0.1:8787"); err == nil {
		t.Fatal("an address was recorded with no claim")
	}
	release, err := ClaimDaemon(terminals, "1.4.0-test")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := RunningDaemon(terminals)
	if before.HTTP != "" {
		t.Fatalf("a new claim serves http on %q", before.HTTP)
	}
	if err := RecordHTTP(terminals, "127.0.0.1:8787"); err != nil {
		t.Fatal(err)
	}
	d, ok := RunningDaemon(terminals)
	if want := *before; !ok || d.HTTP != "127.0.0.1:8787" ||
		d.PID != want.PID || d.Version != want.Version || !d.Started.Equal(want.Started) ||
		!d.ProcStart.Equal(want.ProcStart) || d.BootID != want.BootID {
		t.Fatalf("daemon.pid became %+v from %+v", d, before)
	}
	if fi, err := os.Stat(DaemonPIDFile(terminals)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("daemon.pid: %v %v", fi, err)
	}
	release()
	if err := RecordHTTP(terminals, "127.0.0.1:8787"); err == nil {
		t.Fatal("an address was recorded after the claim was let go of")
	}
	if _, err := os.Stat(DaemonPIDFile(terminals)); !os.IsNotExist(err) {
		t.Fatalf("a refused record wrote daemon.pid: %v", err)
	}
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
