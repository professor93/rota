//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

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
	stopped, err := removeUnit(home, terminals)
	n := len(runningRecords(terminals))
	verb := "keep"
	if n == 1 {
		verb = "keeps"
	}
	var kept unitKept
	switch {
	case errors.As(err, &kept):
		return fmt.Errorf("%w; the rota daemon%s is still running and could not be stopped (%s), so %s is kept: %s; run rota serve uninstall again from %s",
			kept.supervisor, pidOf(kept.daemon), oneLine(kept.stop.Error()), oneLine(path), keptWhy, keptFrom)
	case err != nil:
		if _, lerr := os.Lstat(path); !errors.Is(lerr, fs.ErrNotExist) {
			return err
		}
		err = fmt.Errorf("%s is removed, but %w", oneLine(path), err)
		if stopped != nil {
			err = fmt.Errorf("%w; the rota daemon%s was stopped as rota serve stop stops one, and %s %s running", err, pidOf(stopped), sessionsN(n), verb)
		}
		return err
	}
	fmt.Fprintf(c.out, "rota daemon uninstalled for %s; %s %s running\n", oneLine(home), sessionsN(n), verb)
	return nil
}

// removeUnit takes the store's unit away: the supervisor stops it, which
// leaves every session running, and its file is deleted.
//
// A supervisor that cannot be asked — no user bus to reach, say — leaves
// the daemon running under the unit, and systemd, once it reloads a unit
// whose file is gone, no longer has its KillMode=process: the daemon
// stopping would then end every session the unit holds. So the daemon is
// stopped first, as serve stop stops one, while the unit is still loaded as
// it was written. One started by hand is stopped too: without the
// supervisor the two cannot be told apart. The supervisor is then not asked
// to reload, which could catch the daemon on its way out; it forgets the
// stopped unit at its next reload. With no daemon running there is nothing
// to end and nothing to wait for.
//
// A daemon that cannot be stopped keeps the unit's file, and its link,
// where they are: the error is a unitKept. Otherwise the daemon removeUnit
// stopped itself, if it did, is returned with whatever went wrong.
func removeUnit(home, terminals string) (stopped *holdwire.Daemon, err error) {
	serr := stopUnit(home)
	if serr == nil {
		return nil, deleteUnit(home, true)
	}
	d, ok := daemonOf(terminals)
	if !ok {
		return nil, errors.Join(serr, deleteUnit(home, true))
	}
	if err := endDaemon(terminals); err != nil {
		return nil, unitKept{supervisor: serr, daemon: d, stop: err}
	}
	return d, errors.Join(serr, deleteUnit(home, false))
}

// unitKept is a unit whose file uninstall kept, because the supervisor
// could not be asked to stop it and its daemon could not be stopped either.
type unitKept struct {
	supervisor, stop error
	daemon           *holdwire.Daemon
}

func (k unitKept) Error() string {
	return fmt.Sprintf("%v; the rota daemon could not be stopped: %v", k.supervisor, k.stop)
}

func (k unitKept) Unwrap() []error { return []error{k.supervisor, k.stop} }
