//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// installDaemon is `rota serve install`: the store, by absolute path, gets a
// unit that runs `rota serve start --foreground` from the path this rota was
// started by — links kept, so an upgrade that moves one on is followed —
// with this environment's PATH and CLAUDE_CONFIG_DIR. A daemon running with
// no unit of this store's installed was started by hand, and is not taken
// over. With one installed, it is rewritten and restarted, which is how an
// install takes up a new binary or PATH; a daemon running then, the unit's
// or one started by hand meanwhile, is stopped first as serve stop stops
// one, its sessions running on, so the daemon the unit starts never finds
// the store taken. Since it may stop the daemon, it is not run from inside
// a held session, as serve stop is not.
//
// Each path it says, the store's and the unit's, is said with a space for
// each control character in it (see oneLine).
func (c *cli) installDaemon() error {
	home, terminals, err := daemonStore()
	if err != nil {
		return err
	}
	if err := refusedInsideHeldSession("rota serve install", terminals); err != nil {
		return err
	}
	if err := holdwire.SocketsFit(terminals); err != nil {
		return err
	}
	// Read as the daemon will read it, so a file it would refuse is refused
	// here rather than by a unit that fails again every five seconds.
	if _, _, _, err := daemonConfig("", home); err != nil {
		return err
	}
	path, err := unitFile(home)
	if err != nil {
		return err
	}
	_, err = os.Lstat(path)
	again := err == nil
	exe, err := invokedAs()
	if err != nil {
		return err
	}
	stopped := false
	if d, ok := daemonOf(terminals); ok {
		if !again {
			return fmt.Errorf("a rota daemon is running for %s%s, started by hand; rota serve stop it, then install", oneLine(home), pidOf(d))
		}
		if err := endDaemon(terminals); err != nil {
			return err
		}
		stopped = true
	}
	if written, err := writeUnit(exe, home, unitEnv(), again); err != nil {
		if written != "" {
			err = fmt.Errorf("%s is written, but %w; rota serve uninstall takes it away", oneLine(written), err)
		}
		if stopped {
			err = fmt.Errorf("%w. The rota daemon that was running is stopped, and its sessions keep running; rota serve start starts one again", err)
		}
		return err
	}
	if again {
		fmt.Fprintf(c.out, "rota daemon reinstalled for %s and restarted: %s\n", oneLine(home), oneLine(path))
	} else {
		fmt.Fprintf(c.out, "rota daemon installed for %s: %s\n", oneLine(home), oneLine(path))
	}
	if hint := lingerHint(); hint != "" {
		fmt.Fprintln(c.err, hint)
	}
	return nil
}

// uninstallDaemon is `rota serve uninstall`: the unit stopped and taken
// away. Every session goes on running, for the next daemon to take up. With
// no unit installed there is nothing to do, and the supervisor is not
// asked. Like serve stop, it is not run from inside a held session.
//
// When the supervisor cannot be asked, removeUnit decides what is safe; what
// it says names the supervisor's complaint, and whether the daemon was
// stopped or is running still, and never says it is gone when it is not.
func (c *cli) uninstallDaemon() error {
	home, terminals, err := daemonStore()
	if err != nil {
		return err
	}
	if err := refusedInsideHeldSession("rota serve uninstall", terminals); err != nil {
		return err
	}
	path, err := unitFile(home)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(c.out, "no rota daemon is installed for %s\n", oneLine(home))
		return nil
	}
	fate, err := removeUnit(home, terminals)
	n := len(runningRecords(terminals))
	verb := "keep"
	if n == 1 {
		verb = "keeps"
	}
	var kept unitKept
	switch {
	case errors.As(err, &kept):
		who := "a rota daemon"
		if kept.daemon != nil {
			who = "the rota daemon" + pidOf(kept.daemon)
		}
		return fmt.Errorf("%w; %s is still running and could not be stopped (%s), so %s is kept: %s; run rota serve uninstall again from %s",
			kept.supervisor, who, oneLine(kept.why.Error()), oneLine(path), keptWhy, keptFrom)
	case err != nil:
		_, lerr := os.Lstat(path)
		removed := errors.Is(lerr, fs.ErrNotExist)
		if removed {
			err = fmt.Errorf("%s is removed, but %w", oneLine(path), err)
		}
		if fate == nil {
			return err
		}
		did := "is no longer running"
		if fate.signalled {
			did = "was stopped as rota serve stop stops one"
		}
		err = fmt.Errorf("%w; the rota daemon%s %s, and %s %s running", err, pidOf(fate.daemon), did, sessionsN(n), verb)
		if !removed {
			err = fmt.Errorf("%w; %s is kept", err, oneLine(path))
		}
		return err
	}
	fmt.Fprintf(c.out, "rota daemon uninstalled for %s; %s %s running\n", oneLine(home), sessionsN(n), verb)
	return nil
}

// daemonExitWait is how long uninstall waits, once the daemon has let go of
// the store, for its process to be gone: it finishes what it is serving for
// up to serveGrace first. A variable so a test need not wait.
var daemonExitWait = serveGrace + 2*time.Second

// daemonExited waits up to within for d's process to have exited, and says
// whether it has. A process no longer the one d names has; so has a zombie,
// which its supervisor reaps on the spot, while one nobody reaps — under a
// container's pid 1 that never waits, say — would otherwise be waited for
// in vain. exitOf holds on to the process, not its number. Where it cannot
// be had, d is looked at every 50 ms. A variable so a test can stand in for
// a daemon that is itself.
var daemonExited = func(d *holdwire.Daemon, within time.Duration) bool {
	same := func() bool { return holdwire.SameProcess(d.PID, d.ProcStart, d.ProcStartTicks, d.BootID) }
	if !same() {
		return true
	}
	deadline := time.Now().Add(within)
	if wait, err := exitOf(d.PID); err == nil {
		exited := make(chan struct{})
		go func() { wait(); close(exited) }()
		select {
		case <-exited:
			return true
		case <-time.After(time.Until(deadline)):
			return false
		}
	}
	for same() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// signalTheDaemon is signalDaemon, and a variable only so that a test can
// have the daemon go on its own between removeUnit's look and the signal,
// which nothing outside can arrange.
var signalTheDaemon = signalDaemon

// daemonFate is what became of the daemon running when the supervisor could
// not be asked to stop the unit: gone, after the SIGTERM uninstall sent it,
// or on its own.
type daemonFate struct {
	daemon    *holdwire.Daemon
	signalled bool
}

// removeUnit takes the store's unit away: the supervisor stops it, which
// leaves every session running, and its file is deleted.
//
// A supervisor that cannot be asked — no user bus to reach, say — leaves
// the daemon running under the unit, and systemd, once it reloads a unit
// whose file is gone, no longer has its KillMode=process: the daemon
// stopping would then end every session the unit holds. So the daemon is
// stopped first, as serve stop stops one, and its process waited for until
// it is gone, while the unit is still loaded as it was written: it lets go
// of the store before it has finished serving, and a reload by anybody in
// between would catch it on its way out. One started by hand is stopped
// too: without the supervisor the two cannot be told apart. The supervisor
// is then not asked to reload; it forgets the stopped unit at its next
// reload. With no daemon running there is nothing to end and nothing to
// wait for.
//
// A daemon that cannot be stopped, or whose process outlives
// daemonExitWait, keeps the unit's file, and its link, where they are: the
// error is a unitKept. Otherwise what became of a daemon that was running
// is returned, with whatever went wrong.
func removeUnit(home, terminals string) (*daemonFate, error) {
	serr := stopUnit(home)
	if serr == nil {
		return nil, deleteUnit(home, true)
	}
	seen, ok := daemonOf(terminals)
	if !ok {
		return nil, errors.Join(serr, deleteUnit(home, true))
	}
	signalled, err := signalTheDaemon(terminals)
	var unnamed unnamedDaemon
	switch {
	case errors.As(err, &unnamed):
		return nil, unitKept{supervisor: serr, why: err}
	case err != nil:
		return nil, unitKept{supervisor: serr, daemon: seen, why: err}
	}
	d := seen
	if signalled != nil {
		d = signalled
	}
	if !daemonExited(d, daemonExitWait) {
		return nil, unitKept{supervisor: serr, daemon: d,
			why: fmt.Errorf("it let go of the store but has not exited within %s", daemonExitWait)}
	}
	return &daemonFate{daemon: d, signalled: signalled != nil}, errors.Join(serr, deleteUnit(home, false))
}

// unitKept is a unit whose file uninstall kept, because the supervisor
// could not be asked to stop it and its daemon could not be stopped either.
// daemon is nil for one daemon.pid names no process for.
type unitKept struct {
	supervisor, why error
	daemon          *holdwire.Daemon
}

func (k unitKept) Error() string {
	return fmt.Sprintf("%v; the rota daemon could not be stopped: %v", k.supervisor, k.why)
}

func (k unitKept) Unwrap() []error { return []error{k.supervisor, k.why} }
