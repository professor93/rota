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
// away, its file deleted whatever the supervisor says. Every session goes on
// running, for the next daemon to take up. With no unit installed there is
// nothing to do, and the supervisor is not asked. Like serve stop, it is not
// run from inside a held session.
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
	if err := removeUnit(home); err != nil {
		if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
			return fmt.Errorf("%s is removed, but %w", oneLine(path), err)
		}
		return err
	}
	n := len(runningRecords(terminals))
	verb := "keep"
	if n == 1 {
		verb = "keeps"
	}
	fmt.Fprintf(c.out, "rota daemon uninstalled for %s; %s %s running\n", oneLine(home), sessionsN(n), verb)
	return nil
}
