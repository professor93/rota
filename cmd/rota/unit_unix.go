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
// over; with one installed, it is rewritten and restarted, which is how an
// install takes up a new binary or PATH.
func (c *cli) installDaemon() error {
	home, terminals, err := daemonStore()
	if err != nil {
		return err
	}
	if err := holdwire.SockFits(holdwire.CtlSock(terminals)); err != nil {
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
	if !again {
		if d, ok := daemonOf(terminals); ok {
			return fmt.Errorf("a rota daemon is running for %s%s, started by hand; rota serve stop it, then install", home, pidOf(d))
		}
	}
	exe, err := invokedAs()
	if err != nil {
		return err
	}
	if written, err := writeUnit(exe, home, unitEnv(), again); err != nil {
		if written == "" {
			return err
		}
		return fmt.Errorf("%s is written, but %w; rota serve uninstall takes it away", written, err)
	}
	if again {
		fmt.Fprintf(c.out, "rota daemon reinstalled for %s and restarted: %s\n", home, path)
	} else {
		fmt.Fprintf(c.out, "rota daemon installed for %s: %s\n", home, path)
	}
	if hint := lingerHint(); hint != "" {
		fmt.Fprintln(c.err, hint)
	}
	return nil
}

// uninstallDaemon is `rota serve uninstall`: the unit stopped and taken
// away, its file deleted whatever the supervisor says. Every session goes on
// running, for the next daemon to take up. With no unit installed there is
// nothing to do, and the supervisor is not asked.
func (c *cli) uninstallDaemon() error {
	home, terminals, err := daemonStore()
	if err != nil {
		return err
	}
	path, err := unitFile(home)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(c.out, "no rota daemon is installed for %s\n", home)
		return nil
	}
	if err := removeUnit(home); err != nil {
		if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
			return fmt.Errorf("%s is removed, but %w", path, err)
		}
		return err
	}
	n := len(runningRecords(terminals))
	verb := "keep"
	if n == 1 {
		verb = "keeps"
	}
	fmt.Fprintf(c.out, "rota daemon uninstalled for %s; %s %s running\n", home, sessionsN(n), verb)
	return nil
}
