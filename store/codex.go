package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/professor93/rota/internal/proc"
	rota "github.com/professor93/rota/lib"
)

// An interactive codex session starts Codex's app-server daemon in its
// CODEX_HOME and leaves it running when the session ends: detached, with no
// idle exit, until `codex app-server daemon stop`, a signal or a reboot. It is
// on by default (the daemon_auto_start feature) in every release since 0.158.
// The daemon holds the account's login and refreshes it itself — it looks
// every four and a half minutes and refreshes once the access token is within
// five minutes of expiry, the margin rota's own ExpiryBuffer uses — and
// writes the rotated tokens to auth.json in the home. Before refreshing it
// reads auth.json again, and takes the login there instead if the file has
// changed; if it has not, it presents the refresh token it holds.
//
// So while the daemon is alive the home owns the login, as a Claude Code home
// does while something runs there: rota reads back what the daemon wrote and
// refreshes nothing itself. A refresh by rota would leave the daemon holding
// a refresh token already spent, which it presents the next time its token
// falls due, and codex refuses a reused refresh token for good. Each account
// has a home of its own, so each has a daemon of its own.
const codexDaemonDir = "app-server-daemon"

// codexDaemonRecords are the records Codex keeps for the daemon it runs in a
// home: one for a daemon of the package it installs there, and one for a
// daemon started from an older standalone install. The updater Codex may run
// beside the daemon holds no login and is not among them.
var codexDaemonRecords = []string{"daemon.pid", "app-server.pid"}

// codexDaemon reports whether Codex's daemon is alive in a home, and names
// it. A record nobody can read counts as alive, as a Claude Code one does: a
// home wrongly thought quiet is one rota refreshes underneath the process
// holding its login.
//
// Every mistake it can make is on that side. Codex also records when the
// process started, to tell its daemon from another process that has since
// been given the same pid; rota reads the pid alone, so a record left behind
// whose pid now belongs to something else, or an empty one from a start that
// crashed, reads as alive. That only leaves the refresh to the CLI, which
// makes it itself when it next starts. A record missing for the moment a
// restart takes is the other way round; what rota refreshes then goes into
// the home at once (see Maintain), and the daemon reads the file again
// before it refreshes.
func codexDaemon(home string) (string, bool) {
	for _, name := range codexDaemonRecords {
		raw, err := os.ReadFile(filepath.Join(home, codexDaemonDir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		pid, _, ok := readRecord(raw, err)
		switch {
		case !ok:
			return name + " (unreadable)", true
		case proc.Alive(pid):
			return "codex daemon " + strconv.Itoa(pid), true
		}
	}
	return "", false
}

// codexDue reports whether a is a codex login due for a refresh, which goes
// into its home and is saved before it is refreshed: by Maintain, and by a
// run.
func codexDue(a *rota.Account) bool {
	return rota.Flavor(a.Provider) == "codex" && a.Expired()
}

// holdDue makes a codex home hold the account's login that is due, before it
// is refreshed and the store saved, so that a token refreshed after that and
// saved nowhere but the home is read back by the next adoption. A login held
// as a refresh token alone has no access token to write, and staging would
// refresh it on the way: it goes into the home once refreshed. Until then a
// home that does not hold it — empty, or holding a login it superseded,
// which adoption never reads back — is emptied and recorded as holding it,
// so the refreshed token written there is read back even if no save after
// the refresh succeeds. A home whose daemon is alive is the daemon's, and
// nothing in it is removed.
func (s *Store) holdDue(a *rota.Account) error {
	home := s.Home(a)
	switch {
	case a.Token.Access != "":
		_, err := rota.Stage(a, home)
		return err
	case !a.StagedCurrent() && !s.heldByDaemon(a):
		if err := os.Remove(filepath.Join(home, "auth.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		a.StagedWritten()
	}
	return nil
}

// heldByDaemon reports whether the account's login is held by a daemon its
// CLI left running in its home, which then refreshes it in rota's place.
func (s *Store) heldByDaemon(a *rota.Account) bool {
	if rota.Flavor(a.Provider) != "codex" {
		return false
	}
	_, alive := codexDaemon(s.Home(a))
	return alive
}
