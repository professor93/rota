package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/claudecode"
	"github.com/professor93/rota/internal/proc"
	rota "github.com/professor93/rota/lib"
)

// A claude account keeps a login of its own in its home: Claude Code's own
// credential store there holds the access token and the refresh token, and
// Claude Code refreshes it by itself, sharing it between every process it
// runs in that home — each window, its daemon, the background sessions the
// daemon hosts. lib says how (lib/claudehome.go); this file is the part that
// needs to know what is running, which lib cannot see.
//
// The rule it keeps is one owner per login. While any Claude Code process is
// alive in a home, that home owns the login: rota reads the store there and
// launches on what it finds, and does not write the store, refresh the
// login, or rewrite the home's .claude.json. Two refreshers of one login is
// how a lineage dies — the provider revokes a refresh token the moment it
// issues the next one, and may revoke the whole login when one it already
// replaced is presented again — and Claude Code already is one. When nothing
// is alive, rota may refresh, and a refresh is followed at once, after the
// store is saved, by writing the new login into the home, so nothing ever
// starts there on a spent token.
//
// After reading the home rota knows one of four things about it: it holds
// the account's current login (in sync); it holds nothing usable (empty);
// it holds a login the account has since replaced (behind); or it holds
// something rota cannot place — a login somebody made inside Claude Code
// that could not be confirmed yet, a store that could not be read while
// something runs there, or no login at all where the account's own was and
// Claude Code still runs. That last one is the hold, and in it rota
// refreshes nothing, writes nothing and stops nothing: whatever is in the
// home is used as it is, because Claude Code starting there is what makes it
// confirmable again, and presenting the account's own refresh token might
// present one that login already replaced.
//
// Two exceptions to the rule, because the rule alone deadlocks. rota is the
// owner of the login for the length of each one, and holds Claude Code's own
// refresh lock throughout, so one owner at a time still holds (see mend).
//
// The first is a stranded home. Claude Code's daemon refreshes the login every
// eight hours or so, and now and then the refresh fails. When the access token
// has run out by then the daemon writes auth_required into
// daemon-auth-status.json and the sessions it hosts say "Login expired · Please
// run /login". It goes on retrying every thirty seconds, without logging a
// failure, while rota, seeing something alive, waits for it to go away. The login
// is alive all that time. So a home that is stranded (see stranded) may be
// refreshed although it is alive, whatever else runs there: a window has had its
// chance by then, and an open one is exactly what kept such a home stuck for
// days. The login is written into the store under whatever is running. rota
// expects Claude Code to read it from there, as its re-check of the store
// suggests it does; no live daemon has been seen to recover that way yet.
//
// The second is a blanked store. When a refresh of its own comes back refused
// Claude Code empties the login in its store, keeping everything else, and marks
// the token dead in that process. An account whose current login that was, and
// which is not dead and holds its own refresh token, presents that token once
// before it is called dead, whether the home is alive or quiet. Nothing in the
// home is at risk, because the home holds no login any more. On success the
// login is written in and live processes can find it; on a refusal the account
// is dead and says why; on a transient failure it stays alive, says so, and the
// next look tries again. This does not break the rule below. Blanking is Claude
// Code's verdict and not the provider's word to rota, and it has been measured
// not to be final: a daemon reporting "no token found" for days on a login whose
// refresh then worked for rota at once. A blanked login that is not the
// account's current one is no such case: the home simply holds nothing usable.
//
// A refresh token the provider refused is never presented again. An account
// whose token was refused is dead, and a dead account is never refreshed
// anywhere; it comes back only by a fresh login — `rota login`, or /login
// inside one of its windows, which rota confirms and takes. The process keeps
// the refusal too, until a save gets it to the disk (see spentTokens).
//
// Every write that deletes the keychain item reads it again first and leaves
// it alone if it is not what rota read: /login writes it without the refresh
// lock, and a fresh login is the next look's to take.
//
// From the moment the provider answers a refresh of rota's, the home's login is
// spent, so the write that follows is owed until it is done, and owing it is on
// disk with the token: a write that fails is an error naming the account, and the
// next look finishes it with no other refresh (see owesWrite).
//
// Only the homes rota makes for its accounts hold a login of rota's. An
// account told a configuration directory of the person's choosing runs on a
// token in its environment, as every claude account did before: in that
// directory rota reads no credential store and writes none, stops no daemon
// and removes nothing.

// Names inside a Claude Code configuration directory that rota reads to
// tell whether anything is alive there, and the credential store it writes.
const (
	claudeCredentials = ".credentials.json"
	claudeDaemonLock  = "daemon.lock"
	claudeDaemonAuth  = "daemon-auth-status.json"
	claudeRefreshLock = ".oauth_refresh.lock"
	claudeSessionsDir = "sessions"
)

// routeKey is where an account remembers which route its last launch in its
// home took, so the next launch can tell when the home's daemon was started
// on the other one. seedOwedKey is where it remembers that its home is owed the
// login rota refreshed (see owesWrite). Both belong to the home, and are
// forgotten when the account is given another (MoveHome).
const (
	routeKey    = "route"
	routeStored = "stored"
	routeEnv    = "env"
	seedOwedKey = "seed_owed"
)

// How long the things this file waits for may take. Variables so a test
// need not wait for real.
var (
	daemonStopTimeout = 15 * time.Second
	keychainTimeout   = 10 * time.Second
	removalWait       = 5 * time.Second
	removalPoll       = 200 * time.Millisecond
	// unreadablePause is how long a quiet home's unreadable store is given
	// before it is read once more and, still unreadable, taken for empty: a
	// writer that was just finishing has finished.
	unreadablePause = 300 * time.Millisecond
	// strandedGrace is how long past its real expiry a token must be before a
	// daemon that gave up on it is taken at its word: long enough that a
	// daemon still able to refresh has done so.
	strandedGrace = 10 * time.Minute
	// tokenLife is how long an access token lasts, about: a status file older
	// than that, counted back from the expiry, was written about an earlier
	// token and says nothing about this one.
	tokenLife = 8 * time.Hour
	// refreshLockStale is how long Claude Code's refresh lock may go untouched
	// before Claude Code itself takes it for a dead holder's, and
	// refreshLockBeat how often a holder touches it to say it is not: the
	// numbers Claude Code gives its own lock.
	refreshLockStale = time.Minute
	refreshLockBeat  = 5 * time.Second
	// seedTries is how many times one write of a login into a home is tried, and
	// seedPause how long it gives the disk between tries. Once the keychain item
	// is gone there is no other copy of what stood beside the login.
	seedTries = 3
	seedPause = 100 * time.Millisecond
)

// errKeychain marks a keychain that could not be read, which is not the
// same as one that holds no item.
var errKeychain = errors.New("the keychain could not be read")

// errItemChanged marks a keychain item that changed between rota reading it and
// deleting it to write the login: /login writes the item without Claude Code's
// refresh lock, and a fresh login there is not rota's to delete.
var errItemChanged = errors.New("the keychain item changed while rota was writing the login, so it was left alone and nothing was written")

// dropItemAsJudged deletes the home's keychain item, after reading it once more
// and finding it is still what rota judged; an item that has gone needs no delete.
func dropItemAsJudged(svc string, judged []byte) error {
	ctx, cancel := keychainContext()
	defer cancel()
	now, found, err := claudecode.Read(ctx, svc)
	switch {
	case err != nil:
		return fmt.Errorf("%w: the item %q: %v", errKeychain, svc, err)
	case !found:
		return nil
	case !bytes.Equal(now, judged):
		return errItemChanged
	}
	return claudecode.Delete(ctx, svc)
}

// claudeHome is one look at a claude account's home, for the length of one
// launch, refresh or removal.
type claudeHome struct {
	s    *Store
	a    *rota.Account
	home string
	// others is whether another rota run held the account's claim when the
	// look began. It is asked before this launch takes a claim of its own,
	// which is the only moment the answer means anything: afterwards this
	// launch's own claim is in the way of asking.
	others bool
	// warn is where this look's remarks go: the store's Warn, unless the look
	// runs beside others and must not call it from more than one goroutine.
	warn func(string)
	// warnedPath keeps a remark about the home's path to once per look.
	warnedPath bool
	// hold says why the home holds something rota cannot place, or is ""
	// when it does not; set by adopt. See the comment at the top.
	hold string
	// usable is whether the home's store holds a login Claude Code can sign
	// a new process in with, as adopt last read it.
	usable bool
	// spent is whether the home held, when adopt read it, the very login
	// rota wrote there and has refreshed away since: certainly spent, its
	// successor in the account's hands.
	spent bool
	// blanked is whether Claude Code blanked the account's current login in the
	// home, which adopt then leaves for mend instead of calling the account dead.
	blanked bool
	// blankItem is the keychain item as readStore last found it, when Claude
	// Code blanked it and a file beside it holds a login.
	blankItem []byte
	// item is the keychain item as readStore last found it, nil when there was
	// none: what a write that deletes it has judged, and must find still there.
	item []byte
	// rescuing is whether this look is writing the login under the refresh
	// lock, which goes on deciding for the write that follows: once the refresh
	// has answered, the token is good and the home is no longer stranded.
	rescuing bool
}

func (s *Store) claudeHome(a *rota.Account, others bool) *claudeHome {
	return &claudeHome{s: s, a: a, home: s.Home(a), others: others, warn: s.say}
}

// say passes a remark to Warn, when there is a Warn to pass it to.
func (s *Store) say(msg string) {
	if s.Warn != nil {
		s.Warn(msg)
	}
}

/* ------------------------------------------------------- what is alive --- */

// liveThing is one thing alive in a home. hosted marks the daemon and the
// sessions it runs — kinds bg, daemon and daemon-worker — which stopping the
// daemon ends; a window somebody has open, a record nobody can read, or
// another rota run is not hosted, and outlives any such stop.
type liveThing struct {
	what   string
	hosted bool
}

// hostedKinds are the session records the daemon owns.
var hostedKinds = []string{"bg", "daemon", "daemon-worker"}

// claudeLiveThings is what is alive in a Claude Code home, by the records
// Claude Code keeps there itself: its daemon's lock, which names the daemon's
// pid and is removed when it stops, and one record per live session
// process, removed when that process exits. Both are the account's own,
// never shared with anybody's.
//
// A record naming a pid that is no longer running is a leftover and says
// nothing. A record that cannot be read could be anything, and counts as
// alive: the two mistakes do not cost the same, and a home wrongly thought
// quiet has its login written over underneath a running process.
func claudeLiveThings(home string) []liveThing {
	var live []liveThing
	if what, ok := daemonAlive(home); ok {
		live = append(live, liveThing{what: what, hosted: !strings.HasSuffix(what, "(unreadable)")})
	}
	dir := filepath.Join(home, claudeSessionsDir)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return append(live, liveThing{what: claudeSessionsDir + "/ (unreadable)"})
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue // ended between the listing and now
		}
		pid, kind, ok := readRecord(raw, err)
		switch {
		case !ok:
			live = append(live, liveThing{what: claudeSessionsDir + "/" + e.Name() + " (unreadable)"})
		case proc.Alive(pid):
			live = append(live, liveThing{what: "session " + strconv.Itoa(pid), hosted: slices.Contains(hostedKinds, kind)})
		}
	}
	return live
}

// claudeLive names what is alive in a home, for saying so.
func claudeLive(home string) []string { return names(claudeLiveThings(home)) }

func names(things []liveThing) []string {
	out := make([]string, len(things))
	for i, t := range things {
		out[i] = t.what
	}
	return out
}

// daemonAlive reports whether a home's daemon is running, and names it.
func daemonAlive(home string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(home, claudeDaemonLock))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	pid, _, ok := readRecord(raw, err)
	switch {
	case !ok:
		return claudeDaemonLock + " (unreadable)", true
	case proc.Alive(pid):
		return "daemon " + strconv.Itoa(pid), true
	}
	return "", false
}

// readRecord reads the pid and kind a liveness record names.
func readRecord(raw []byte, err error) (pid int, kind string, ok bool) {
	if err != nil {
		return 0, "", false
	}
	var rec struct {
		PID  float64 `json:"pid"`
		Kind string  `json:"kind"`
	}
	if rota.UnmarshalLenient(raw, &rec) != nil || rec.PID <= 0 {
		return 0, "", false
	}
	return int(rec.PID), rec.Kind, true
}

// things is everything alive in the home, a rota run holding the account
// included.
func (h *claudeHome) things() []liveThing {
	live := claudeLiveThings(h.home)
	if h.others {
		live = append([]liveThing{{what: "another rota run"}}, live...)
	}
	return live
}

func (h *claudeHome) live() []string { return names(h.things()) }

// quiet reports whether nothing at all is alive in the home.
func (h *claudeHome) quiet() bool { return len(h.things()) == 0 }

// daemonAlone reports whether the daemon is running and nothing outlives
// stopping it: every live thing is the daemon or a session it hosts.
func (h *claudeHome) daemonAlone() bool {
	things := h.things()
	if _, alive := daemonAlive(h.home); !alive || len(things) == 0 {
		return false
	}
	for _, t := range things {
		if !t.hosted {
			return false
		}
	}
	return true
}

// stranded reports whether Claude Code's daemon has given up on the login in
// this home, which is one reason a home with something alive in it may be
// refreshed. All of it has to hold, of a home adopt has just read: rota keeps
// the account's login here, it is alive, and the home holds that very login,
// out of the hold; the store's lock is still held; something is alive here,
// since a quiet home needs no exception; the daemon said auth_required, about
// the token that ran out; and that token truly expired at least strandedGrace
// ago, by its real expiry and not by Expired, which counts a margin before it.
// What else runs there does not matter, a window or a held claim included: by
// then it has had its chance.
//
// It sees only what the status file says. Claude Code keeps that file through a
// state layer of its own, and on its storage-v5 path the file may not be written
// where rota looks, or at all; such a home is not found stranded, and waits for
// its store to be blanked or for nothing to be alive there. A blanked store needs
// no status file (see mend).
//
// An account that is dead and in the hold with a lapsed access token, which is
// how a refresh token the profile cannot vouch for looks, is left to `rota run`
// on purpose: heldDead lets it launch, and starting Claude Code there is what
// makes its login confirmable. Nothing here presents that token.
func (h *claudeHome) stranded() bool {
	a := h.a
	if h.s.released || h.hold != "" || !h.usable || !h.s.keepsLogin(a) || !a.StagedCurrent() || h.quiet() {
		return false
	}
	since, ok := daemonGaveUp(h.home)
	expired := time.UnixMilli(a.Token.ExpiresAt)
	return ok && !rota.Now().Before(expired.Add(strandedGrace)) && since.Add(tokenLife).After(expired)
}

// daemonGaveUp reads when the daemon in a home last gave up on its login, from
// the status file it leaves there. A file that is missing, cannot be read, or
// says anything else says nothing.
func daemonGaveUp(home string) (time.Time, bool) {
	raw, err := os.ReadFile(filepath.Join(home, claudeDaemonAuth))
	if err != nil {
		return time.Time{}, false
	}
	var status struct {
		Status string  `json:"status"`
		Since  float64 `json:"since"`
	}
	if rota.UnmarshalLenient(raw, &status) != nil || status.Status != "auth_required" || status.Since <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(status.Since)), true
}

// wanting reports whether this look owes the home something although something
// may be alive there: a write left unfinished, a store Claude Code blanked, a
// stranded home, or a blanked keychain item standing in front of a good file.
func (h *claudeHome) wanting() bool {
	if h.s.released || !h.s.keepsLogin(h.a) {
		return false
	}
	return h.owesWrite() || h.blanked || h.stranded() || h.covered()
}

// owesWrite reports whether the account's home is owed a login rota refreshed
// and saved but could not write there, and may be given it now. The token is on
// disk, so this takes no refresh. A home where something runs on a login that is
// not one of the account's keeps it until nothing runs there, as ever.
func (h *claudeHome) owesWrite() bool {
	if h.a.Extra[seedOwedKey] == "" || h.hold != "" {
		return false
	}
	return h.spent || !h.usable || h.a.StagedCurrent() || h.quiet()
}

// owe records that the account's home has not got the login rota just
// refreshed, on the account, so it is saved with the token.
func (h *claudeHome) owe() {
	if h.a.Extra == nil {
		h.a.Extra = map[string]string{}
	}
	h.a.Extra[seedOwedKey] = "1"
}

// paid forgets what owe recorded, once the home holds the login.
func (h *claudeHome) paid() {
	delete(h.a.Extra, seedOwedKey)
	h.usable, h.blanked, h.spent, h.blankItem = true, false, false, nil
}

// covered reports whether a blanked keychain item stands in front of a file
// holding the account's current login: Claude Code reads the item and never the
// file while an item exists, so the home is signed out whatever the file holds.
func (h *claudeHome) covered() bool {
	return h.blankItem != nil && h.usable && h.a.StagedCurrent() && h.hold == ""
}

// notThrough marks a rescue the provider did not answer, which changes nothing
// about the login, so whatever asked goes on.
type notThrough struct{ error }

func (e notThrough) Unwrap() error { return e.error }

// mend does what wanting says this look owes the home, as one sequence for this
// account: the lock is taken, the home read again, the login refreshed, saved
// and written, and the lock let go, so that a slow account holds nobody else's
// home open. Every look comes through here straight after it reads the home,
// whatever it came for, and one at a time, because a save reads every account.
// changed says something on the account changed that the caller has yet to save.
//
// Something in the home may go on trying to refresh by itself all this while:
// the daemon's re-check does, silently, every thirty seconds, and any window
// that starts there will. All of them take one lock around a refresh, a
// directory beside the store, so rota takes the same one, and nothing of Claude
// Code's can present the refresh token beside it. A lock somebody holds means
// Claude Code is refreshing right now, and the look leaves it to them. Once it is
// held the home is read again, as Claude Code does, because a refresh may have
// finished a moment ago.
func (h *claudeHome) mend(ctx context.Context) (changed bool, err error) {
	if !h.wanting() {
		return false, nil
	}
	lock, ok := takeRefreshLock(h.home)
	if !ok {
		return false, nil
	}
	defer lock.release()
	before := snapshot(h.a)
	if err := h.adopt(ctx); err != nil {
		return false, err
	}
	changed = before.differs(h.a)
	switch {
	case !h.wanting():
		return changed, nil
	case h.owesWrite():
		h.rescuing = true
		return true, h.writeAndSave(ctx)
	case h.blanked, h.stranded():
		did, err := h.rescue(ctx)
		return changed || did, err
	}
	return changed, h.uncover()
}

// rescue presents the account's own refresh token for a home mend found stranded
// or blanked, once, saves what came of it, and writes the login in. Claude Code's
// blanking is about a token it was forced to refresh, so a token rota still
// counts as good is presented as well; rota.Refresh asks only for an expired one.
func (h *claudeHome) rescue(ctx context.Context) (bool, error) {
	a := h.a
	cleared := h.blanked
	what := "Claude Code's daemon has given up on refreshing the login in its home"
	if cleared {
		what = "Claude Code cleared the login in its home"
	}
	expires, due := a.Token.ExpiresAt, a.Expired()
	if !due {
		a.Token.ExpiresAt = rota.NowMS() - 1
	}
	forced := a.Token.ExpiresAt
	h.rescuing = true
	changed, err := h.refresh(ctx)
	if !due && a.Token.ExpiresAt == forced {
		a.Token.ExpiresAt = expires
	}
	if err != nil && !a.Dead {
		// Nothing has changed: the home is as it was, and the next look tries again.
		return changed, notThrough{fmt.Errorf("%s: %s, and rota could not refresh it just now, so the next look tries again: %w", a, what, err)}
	}
	if cleared && a.Dead && h.hold == "" {
		a.DeadReason = fmt.Sprintf("Claude Code cleared the login in its home, and the provider refused rota's own refresh token too (%s)", a.DeadReason)
	}
	if !changed {
		return false, err
	}
	if serr := h.s.saveFirmly(); serr != nil {
		// A refusal not saved is still kept by this process (see spentTokens),
		// so no later look of it presents the token again.
		return true, fmt.Errorf("%s: refusing to go on: the store could not be saved after a token change: %w", a, serr)
	}
	if err != nil || !h.rescuing {
		return true, err // refused, or a sibling got there first
	}
	if cleared {
		h.warn(fmt.Sprintf("%s: Claude Code had cleared the login in its home, so rota refreshed it", a))
	} else {
		h.warn(fmt.Sprintf("%s: Claude Code's daemon had given up on refreshing the login in its home, so rota refreshed it", a))
	}
	return true, h.writeAndSave(ctx)
}

// writeAndSave writes what the home is owed, and saves that it has it.
func (h *claudeHome) writeAndSave(ctx context.Context) error {
	if err := h.writeRefreshed(ctx); err != nil {
		return err
	}
	return h.s.Save()
}

// uncover takes a blanked keychain item out of the way of the login in the file
// beside it, so Claude Code reads the file. What the item kept beside the login
// goes into the file first: the item is what Claude Code has been reading, so
// its secrets are the current ones. As with any write, the new file is whole
// before the item goes.
func (h *claudeHome) uncover() error {
	file, _, err := h.readStore()
	if err != nil || h.blankItem == nil {
		return err
	}
	merged, err := rota.MergeClaudeCredentials(h.blankItem, file)
	if err != nil {
		return err
	}
	svc, _ := h.service()
	judged := h.blankItem
	err = writeAtomicThen(filepath.Join(h.home, claudeCredentials), merged, func() error {
		return dropItemAsJudged(svc, judged)
	})
	if err != nil {
		return fmt.Errorf("%s: could not take the keychain item Claude Code blanked out of the way of its login: %w", h.a, err)
	}
	h.blankItem = nil
	return nil
}

// refreshLock is Claude Code's own refresh lock, held by rota. Claude Code takes
// a lock for dead when its mtime has not moved for refreshLockStale, so it is
// touched every refreshLockBeat for as long as it is held, as its own holders do.
// With no owner record beside it Claude Code does not try to take it any sooner.
//
// A holder that stalled past that, a laptop going to sleep say, may find
// somebody else has taken it. Ownership is told the way Claude Code's own holders
// tell it, by the directory and the mtime last given it: a lock that is not what
// rota left is not rota's, and from then on is neither touched nor removed.
type refreshLock struct {
	path string
	// held is the directory as rota last left it. Only the beat reads and writes
	// it until the beat has ended, and only release after.
	held fs.FileInfo
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// takeRefreshLock takes the lock as Claude Code would: one left behind by a dead
// holder is cleared, and one somebody holds is not rota's to have.
func takeRefreshLock(home string) (*refreshLock, bool) {
	path := filepath.Join(home, claudeRefreshLock)
	if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > refreshLockStale {
		_ = os.Remove(path)
	}
	if os.Mkdir(path, 0o700) != nil {
		return nil, false
	}
	fi, err := os.Lstat(path)
	if err != nil {
		_ = os.Remove(path) // made a moment ago, and not to be left in Claude Code's way
		return nil, false
	}
	l := &refreshLock{path: path, held: fi, stop: make(chan struct{}), done: make(chan struct{})}
	go l.beat()
	return l, true
}

func (l *refreshLock) beat() {
	defer close(l.done)
	tick := time.NewTicker(refreshLockBeat)
	defer tick.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-tick.C:
			if !l.ours() {
				return
			}
			now := time.Now()
			_ = os.Chtimes(l.path, now, now) // a miss is made up for at the next beat
			if fi, err := os.Lstat(l.path); err == nil {
				l.held = fi
			}
		}
	}
}

// ours reports whether the lock on disk is still the one rota left there.
func (l *refreshLock) ours() bool {
	fi, err := os.Lstat(l.path)
	return err == nil && os.SameFile(fi, l.held) && fi.ModTime().Equal(l.held.ModTime())
}

// release ends the beat and removes the lock, if it is still rota's. It is safe
// to call more than once.
func (l *refreshLock) release() {
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		if l.ours() {
			_ = os.Remove(l.path)
		}
	})
}

// claimed reports whether a run holds this account's claim, shared or not:
// whether the claim can be taken exclusively right now, without waiting. A
// claim that cannot even be tried counts as held.
func (s *Store) claimed(a *rota.Account) bool {
	if !rota.OwnsCredentials(a.Provider) {
		return false
	}
	// No lock file, no run: every run makes it. Asking is not a reason to
	// leave one in a directory the person chose.
	lock := filepath.Join(s.Home(a), runLock)
	if _, err := os.Lstat(lock); err != nil {
		return false
	}
	f, got, err := tryExclusive(lock)
	if err != nil || !got {
		return true
	}
	_ = f.Close()
	return false
}

// InUse reports whether anything is using an account right now: a rota run
// holding it, or — for a CLI that runs many processes in one home — any of
// them alive there, rota's or not. Like Busy it is a glance.
func (s *Store) InUse(a *rota.Account) bool {
	if s.claimed(a) {
		return true
	}
	return rota.SharedHome(a.Provider) && len(claudeLive(s.Home(a))) > 0
}

/* ---------------------------------------------------- whose login, where --- */

// loginInHome reports whether this account's login is one rota keeps in its
// home at all, whatever state the login is in: the home is one rota made for
// the account — never a directory the person chose, the person's own among
// them — the platform keeps logins in homes (not Windows), and on macOS the
// home's path names a keychain item rota can find. An account for which it
// is false runs on a token in its environment, as every claude account did
// before — or, for an account that runs on an API key, on its key.
func (s *Store) loginInHome(a *rota.Account) bool {
	home := s.Home(a)
	if a.APIKey() || !s.owns(a) || rota.JoinHome(a, home) == nil {
		return false
	}
	if keychainKept {
		if _, ok := claudecode.Service(home); !ok {
			return false
		}
	}
	return true
}

// keepsLogin reports whether a launch keeps this account's login in its home
// now: rota keeps it there at all, and lib takes the stored route for it —
// an access token, a refresh token and an expiry, and not dead.
func (s *Store) keepsLogin(a *rota.Account) bool {
	return s.loginInHome(a) && rota.StoresLogin(a, s.Home(a))
}

// unrefreshed reports whether rota holds a living account's login as a
// refresh token alone: no access token to run on, and so no stored route,
// until a refresh gives it one. Such a login is refreshed the way an expired
// one kept in a home is, by renew in a quiet home, and never past the checks
// that keep rota from presenting a refresh token a live Claude Code may hold.
func unrefreshed(a *rota.Account) bool {
	return !a.Dead && a.Token.Access == "" && a.Token.Refresh != ""
}

// whyNoLogin says why an account does not run on a login of its own in its
// home, in a few words a person can act on; "" when it does.
func (s *Store) whyNoLogin(a *rota.Account) string {
	home := s.Home(a)
	switch {
	case a.APIKey():
		return "it runs on an API key, which Remote Control refuses"
	case !s.owns(a):
		return "it runs on a token because its configuration directory is one you chose"
	case rota.JoinHome(a, home) == nil:
		return "this platform keeps Claude Code on a token in its environment"
	case keychainKept && !nameable(home):
		return "its home " + home + " is not a plain ASCII path, so the keychain item Claude Code would keep its login in cannot be found"
	case a.Dead:
		return "its login is dead"
	case unrefreshed(a):
		return "rota holds its login as a refresh token alone until it is refreshed, which its next run or " +
			"`rota list` does while nothing runs in its home"
	case !rota.StoresLogin(a, home):
		return "its login has no refresh token or no expiry"
	}
	return ""
}

func nameable(home string) bool {
	_, ok := claudecode.Service(home)
	return ok
}

// sharer is another account told the same home as this one, or nil. A home
// holds one account's login: two accounts in one would each read the
// other's as a rotation of their own, and take it.
func (s *Store) sharer(a *rota.Account) *rota.Account {
	home := s.Home(a)
	for _, o := range s.Accounts {
		if o != a && o.ID != a.ID && sameDir(s.Home(o), home) {
			return o
		}
	}
	return nil
}

// oneHomeOneAccount refuses an account whose home another account has too.
func (s *Store) oneHomeOneAccount(a *rota.Account) error {
	o := s.sharer(a)
	if o == nil {
		return nil
	}
	return rota.Invalid("%s and %s are both told their home is %s, and a home holds one account's login; "+
		"neither is launched or written there until one of them is given another (`rota set <id> --config`)",
		a, o, s.Home(a))
}

// service is the keychain item Claude Code keeps this home's login in, on
// the platform that has one.
func (h *claudeHome) service() (string, bool) {
	if !keychainKept {
		return "", false
	}
	return claudecode.Service(h.home)
}

func keychainContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), keychainTimeout)
}

// readStore reads the home's credential store the way Claude Code does: the
// keychain item when there is one, and the file only when there is not —
// except that a login in the file newer than the item's is read instead. When
// the keychain refuses a write Claude Code keeps the login in the file, and an
// item it could not delete is left behind, older, holding a refresh token the
// provider has replaced. inKeychain says an item exists, whichever was read,
// because a write has to get it out of the way either way. A keychain that
// cannot be read is errKeychain; a file that cannot be read is the read's own
// error, which adoption takes for an unreadable store.
func (h *claudeHome) readStore() (raw []byte, inKeychain bool, err error) {
	h.blankItem, h.item = nil, nil
	if svc, ok := h.service(); ok {
		ctx, cancel := keychainContext()
		defer cancel()
		raw, found, err := claudecode.Read(ctx, svc)
		if err != nil {
			return nil, false, fmt.Errorf("%w: the item %q that holds its login: %v", errKeychain, svc, err)
		}
		if found {
			h.item = raw
			if file, err := os.ReadFile(filepath.Join(h.home, claudeCredentials)); err == nil && fileIsNewer(file, raw) {
				if l, ok := storedLogin(raw); ok && l.Refresh == "" {
					h.blankItem = raw
				}
				return file, true, nil
			}
			return raw, true, nil
		}
	}
	raw, err = os.ReadFile(filepath.Join(h.home, claudeCredentials))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return raw, false, err
}

// loginEntry is what this file needs to know of the login in a credential store.
type loginEntry struct {
	Refresh string  `json:"refreshToken"`
	Expires float64 `json:"expiresAt"`
}

// storedLogin reads the login entry out of a credential store, ok being false
// when the store cannot be read or has no such entry.
func storedLogin(raw []byte) (loginEntry, bool) {
	var store struct {
		Login *loginEntry `json:"claudeAiOauth"`
	}
	if rota.UnmarshalLenient(raw, &store) != nil || store.Login == nil {
		return loginEntry{}, false
	}
	return *store.Login, true
}

// fileIsNewer reports whether a credential file holds a later login than the
// keychain item beside it. Only a login can be: a file with no refresh token or
// no expiry has nothing to offer, a blanked one above all. An item Claude Code
// blanked is older than any file that does hold a login: it reads the item and
// never the file while an item exists, so that home is signed out whatever the
// file holds, until the item is out of the way.
func fileIsNewer(file, item []byte) bool {
	f, ok := storedLogin(file)
	if !ok || f.Refresh == "" || f.Expires <= 0 {
		return false
	}
	i, ok := storedLogin(item)
	if !ok {
		return false
	}
	return i.Refresh == "" || (i.Expires > 0 && f.Expires > i.Expires)
}

// read is the home as adoption sees it: its files, with the store answered
// by readStore. ok is false when the keychain could not be read, which puts
// the home in the hold.
func (h *claudeHome) read() (fsys storeFS, ok bool) {
	raw, _, err := h.readStore()
	if errors.Is(err, errKeychain) {
		h.hold = fmt.Sprintf("the macOS keychain could not be read (%v); unlock the login keychain for this session, "+
			"or keep a long-lived token for this account (`rota login --long`)", err)
		return storeFS{}, false
	}
	return storeFS{FS: os.DirFS(h.home), store: raw, err: err}, true
}

// adopt reads the home's login into the account, and works out what the
// home holds. A new login found there is settled by asking the provider
// whose it is (confirm). A store that cannot be read is the hold while
// anything runs there, and is read once more after a pause in a quiet home
// before it is taken for empty.
//
// The account's own login blanked is left out of adoption, which would call the
// account dead: that is for mend to find out, before anything is saved (see the
// comment at the top).
func (h *claudeHome) adopt(ctx context.Context) error {
	h.hold, h.usable, h.spent, h.blanked = "", false, false, false
	if err := h.s.oneHomeOneAccount(h.a); err != nil {
		return err
	}
	fsys, ok := h.read()
	if !ok {
		return nil
	}
	h.spent = rota.ClaudeHomeSpent(h.a, fsys)
	if h.blanked = h.clearedOwn(fsys); h.blanked {
		return nil
	}
	err := rota.AdoptFrom(h.a, fsys)
	if errors.Is(err, rota.ErrUnreadableLogin) {
		if !h.quiet() {
			h.hold = "its credential store cannot be read while Claude Code runs there"
			return nil
		}
		time.Sleep(unreadablePause)
		if fsys, ok = h.read(); !ok {
			return nil
		}
		if h.blanked = h.clearedOwn(fsys); h.blanked {
			return nil
		}
		if err = rota.AdoptFrom(h.a, fsys); errors.Is(err, rota.ErrUnreadableLogin) {
			h.a.StagedSuperseded() // empty, then: the next write replaces it
			err = nil
		}
	}
	var nl *rota.NewLogin
	if errors.As(err, &nl) {
		err = nil
		h.confirm(ctx, nl)
	}
	if err != nil {
		return err
	}
	h.usable = rota.HoldsLogin(h.a, fsys)
	if l, ok := storedLogin(fsys.store); ok && l.Refresh == h.a.Token.Refresh && int64(l.Expires) >= h.a.Token.ExpiresAt {
		delete(h.a.Extra, seedOwedKey) // the home holds it, however it got there
	}
	return nil
}

// clearedOwn reports whether the home's store is blanked where the account's
// current login was, the account being alive and holding its own refresh token.
// A file older than the account's own login, left beside a blanked keychain
// item, is a leftover that changes nothing about that.
func (h *claudeHome) clearedOwn(fsys storeFS) bool {
	a := h.a
	if a.Dead || !a.StagedCurrent() {
		return false
	}
	l, ok := storedLogin(fsys.store)
	return ok && (l.Refresh == "" || (h.blankItem != nil && int64(l.Expires) < a.Token.ExpiresAt))
}

// confirm settles a login found in the home that is not the account's own,
// by asking the provider whose it is with that login's own access token. The
// account's is taken — a dead account is alive again. Somebody else's is not
// taken: it is said so, and replaced when nothing runs there. When the
// provider cannot be asked, for whatever reason, a lapsed access token
// included, the home is in the hold.
func (h *claudeHome) confirm(ctx context.Context, nl *rota.NewLogin) {
	id, err := nl.Identify(ctx)
	switch {
	case err != nil:
		h.hold = fmt.Sprintf("Claude Code in its home holds a login made there that rota has not been able to confirm yet (%v)", err)
	case rota.MatchIdentity([]*rota.Account{h.a}, h.a.Provider, id) != nil:
		nl.Accept(h.a)
	default:
		h.warn(nl.Refuse(h.a, id).Error())
	}
}

// sayHold is the one line a launch in the hold owes the person.
func (h *claudeHome) sayHold() {
	h.warn(fmt.Sprintf("%s: %s; it is used as it is, and rota refreshes and writes nothing for this account until it can be",
		h.a, h.hold))
}

// storeFS is a home as Claude Code reads it: its files, with the credential
// store answered by what readStore found — the keychain item on macOS when
// there is one — or by the error reading it gave.
type storeFS struct {
	fs.FS
	store []byte
	err   error
}

func (f storeFS) ReadFile(name string) ([]byte, error) {
	if name != claudeCredentials {
		return fs.ReadFile(f.FS, name)
	}
	switch {
	case f.err != nil:
		return nil, &fs.PathError{Op: "read", Path: name, Err: f.err}
	case f.store == nil:
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return bytes.Clone(f.store), nil
}

func (f storeFS) Open(name string) (fs.File, error) {
	if name != claudeCredentials {
		return f.FS.Open(name)
	}
	switch {
	case f.err != nil:
		return nil, &fs.PathError{Op: "open", Path: name, Err: f.err}
	case f.store == nil:
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &memFile{Reader: bytes.NewReader(f.store), size: int64(len(f.store))}, nil
}

type memFile struct {
	*bytes.Reader
	size int64
}

func (m *memFile) Stat() (fs.FileInfo, error) { return memInfo(m.size), nil }
func (m *memFile) Close() error               { return nil }

type memInfo int64

func (memInfo) Name() string       { return claudeCredentials }
func (i memInfo) Size() int64      { return int64(i) }
func (memInfo) Mode() fs.FileMode  { return 0o600 }
func (memInfo) ModTime() time.Time { return time.Time{} }
func (memInfo) IsDir() bool        { return false }
func (memInfo) Sys() any           { return nil }

/* ------------------------------------------------------------- the route --- */

// remember records which route this launch in the home took.
func (h *claudeHome) remember(route string) {
	if h.a.Extra == nil {
		h.a.Extra = map[string]string{}
	}
	h.a.Extra[routeKey] = route
}

// lastRoute is the route the account's last launch in this home took. With
// nothing remembered, the home was last launched by a rota that knew only
// the environment.
func (h *claudeHome) lastRoute() string {
	if r := h.a.Extra[routeKey]; r != "" {
		return r
	}
	return routeEnv
}

// retireFor stops the home's daemon when this launch takes another route
// than the one the daemon was started on, and stopping it leaves the home
// quiet.
//
// A daemon keeps the credential it started with for life and serves it to
// every background session it starts. One started on a token in its
// environment goes on handing out that token after the account has moved to
// its stored login — until it is revoked, and every background session it
// hosts says so. One started on the stored login would go on serving it
// after the account has gone back to a token. But a window somebody has
// open, a record nobody can read, or another rota run is left alone: a stop
// would not make the home quiet, only take its background work away.
func (h *claudeHome) retireFor(route string) {
	last := h.lastRoute()
	if last == route {
		return
	}
	if what, alive := daemonAlive(h.home); alive && !h.daemonAlone() {
		if route == routeStored {
			h.warn(fmt.Sprintf("%s: the Claude Code daemon in its home (%s) was started on a token in its environment and "+
				"stays while other sessions run there; background sessions started meanwhile use that daemon's old "+
				"credential until the home goes quiet", h.a, what))
		}
		return
	}
	if !h.daemonAlone() {
		return
	}
	what, _ := daemonAlive(h.home)
	if err := h.stopDaemon(); err != nil {
		h.warn(fmt.Sprintf("%s: the Claude Code daemon in its home (%s) was started on %s and could not be stopped (%v); "+
			"its background sessions keep that credential until it ends", h.a, what, routeWords(last), err))
		return
	}
	h.warn(fmt.Sprintf("%s: stopped the Claude Code daemon in its home (%s): it was started on %s and would have gone on "+
		"handing that credential to every background session", h.a, what, routeWords(last)))
}

func routeWords(route string) string {
	if route == routeStored {
		return "the account's stored login"
	}
	return "a token in its environment"
}

// stopDaemon stops the home's daemon and the background sessions it hosts,
// with nothing in its environment that authenticates and a bound on how long
// it may take.
func (h *claudeHome) stopDaemon() error {
	ctx, cancel := context.WithTimeout(context.Background(), daemonStopTimeout)
	defer cancel()
	env := rota.Environ(HostEnv(), &rota.Command{
		Env:  []string{"CLAUDE_CONFIG_DIR=" + h.home},
		Drop: rota.ClaudeCredentialVars(),
	})
	return claudecode.StopDaemon(ctx, h.home, env)
}

/* ------------------------------------------------- refreshing and writing --- */

// renew refreshes the account's login when it has expired, the home is not
// in the hold, the account is not dead and nothing is alive there, and puts the
// new login into the home at once, after the store has it on disk, because from
// the moment the provider answers, the refresh token the home holds is spent.
// That spent login is also why the write is made even if something started in
// the home meanwhile (see mayWrite). What is owed a home with something alive in
// it is mend's, and a blanked store's token is presented there and nowhere else.
func (h *claudeHome) renew(ctx context.Context) (bool, error) {
	a := h.a
	if !a.Expired() || a.Dead || h.hold != "" || h.blanked || !h.quiet() {
		return false, nil
	}
	changed, err := h.refresh(ctx)
	if err != nil || !changed || a.Dead || h.hold != "" {
		return changed, err
	}
	h.owe()
	if err := h.s.Save(); err != nil {
		return true, fmt.Errorf("refusing to go on: the store could not be saved after a token change: %w", err)
	}
	return true, h.writeRefreshed(ctx)
}

// writeRefreshed puts a login rota has refreshed and saved into the home, when
// it may. From here on the provider has answered and the home's login is spent,
// so a home that cannot be read or written is an error for a person to see, and
// the account goes on owing the write (see owesWrite).
func (h *claudeHome) writeRefreshed(ctx context.Context) error {
	a := h.a
	fsys, ok := h.read()
	if !ok {
		return fmt.Errorf("%s: its login was refreshed and saved, but its home could not be read to write it there, "+
			"so Claude Code there still holds the one it replaced (%s); the next look writes it", a, h.hold)
	}
	h.spent = rota.ClaudeHomeSpent(a, fsys)
	if !h.mayWrite() {
		return nil
	}
	_, files, err := rota.StagePlan(ctx, a, h.home)
	if err == nil && len(files) == 0 && h.rescuing {
		// The refresh token did not rotate, so the home reads as current, but
		// the access token did, and that is what a home in trouble is short of.
		a.StagedSuperseded()
		_, files, err = rota.StagePlan(ctx, a, h.home)
	}
	if err == nil {
		err = h.seed(files)
	}
	if err != nil {
		return fmt.Errorf("%s: its login was refreshed and saved, but could not be written into its home, "+
			"so Claude Code there still holds the one it replaced; the next look writes it: %w", a, err)
	}
	h.paid()
	return nil
}

// mayWrite reports whether rota may write the account's login into the home
// now: never in the hold; when nothing is alive there; and, the two
// exceptions to that, when the home still holds the very login rota itself
// refreshed away, or this look is writing under the refresh lock (mend). The
// first login is certainly spent — the provider issued its successor, which the
// account holds — so every window still on it is signed out at its next refresh
// anyway, and leaving it there only makes sure of that. And a rescue is for
// nothing until the home holds what it was short of.
func (h *claudeHome) mayWrite() bool {
	if h.hold != "" {
		return false
	}
	return h.quiet() || h.spent || h.rescuing
}

// refresh asks the provider for a new token. It is the only way rota
// refreshes a claude account whose login lives in its home, and it never
// refreshes a dead one or one in the hold.
//
// A refusal is not believed until the home has been read again: a sibling
// may have refreshed first, between the last reading and this request. A
// sibling's login that can be confirmed is taken, and the account is alive.
// Otherwise the account is dead — its own refresh token refused for good,
// and never presented again — and if the home holds a login nobody can
// confirm yet, the account is dead and in the hold: still launchable in its
// home on that login, and alive again the moment it is confirmed.
func (h *claudeHome) refresh(ctx context.Context) (changed bool, err error) {
	a := h.a
	if a.Dead || h.hold != "" {
		return false, fmt.Errorf("%w: %s is not refreshed: %s", rota.ErrBusy, a, nonEmpty(h.hold, "its login is dead"))
	}
	presented := spentKey(h.home, a.Token.Refresh)
	if why, ok := spentTokens.Load(presented); ok {
		// Refused earlier in this process, and not on disk yet: it is not
		// presented again.
		a.Dead, a.DeadReason = true, why.(string)
		return true, rota.WrapReauth(a)
	}
	rescue := h.rescuing
	h.rescuing = false
	changed, err = rota.Refresh(ctx, a)
	if rescue && changed && err == nil {
		h.rescuing = true
		h.owe()
	}
	if a.Dead {
		spentTokens.Store(presented, a.DeadReason)
	}
	if err == nil || !a.Dead {
		return changed, err
	}
	refused := a.DeadReason
	if aerr := h.adopt(ctx); aerr != nil {
		return true, err
	}
	switch {
	case !a.Dead:
		return true, nil
	case h.hold != "" && h.usable:
		a.DeadReason = fmt.Sprintf("its own refresh token was refused (%s); a login made inside Claude Code in its home "+
			"is waiting to be confirmed", refused)
		return true, nil
	}
	return true, err
}

// spentTokens are the refresh tokens the provider refused in this process, by
// home and a hash of the token, with why. The account says so on disk once a
// save gets through; until then, and for the rest of the process, this does.
// A failure the provider did not answer is not one: like any refresh of rota's
// that does not get through, it is tried again by the next look.
var spentTokens sync.Map

func spentKey(home, refresh string) string {
	sum := sha256.Sum256([]byte(refresh))
	return home + "\x00" + hex.EncodeToString(sum[:])
}

// saveFirmly saves a change that must reach the disk, trying again a moment
// later when the first save fails.
func (s *Store) saveFirmly() error {
	var err error
	for try := 1; try <= seedTries; try++ {
		if err = s.Save(); err == nil {
			return nil
		}
		time.Sleep(seedPause)
	}
	return err
}

func nonEmpty(s, otherwise string) string {
	if s != "" {
		return s
	}
	return otherwise
}

// seed writes the account's login into its home. It is only ever called
// when rota may write there (mayWrite), and never in the hold.
//
// The login replaces only itself in the store: Claude Code keeps the OAuth
// tokens of MCP servers and secrets of its own beside it. The new file is
// written and synced first; on macOS the keychain item is deleted next, and
// only then is the file renamed into place — Claude Code reads the item
// before the file, and rota never writes an item, because a write through
// security resets who may read it. A delete that fails leaves everything as
// it was. Claude Code moves the login back into the keychain itself on its
// next refresh.
func (h *claudeHome) seed(files []rota.StagedFile) error {
	if h.hold != "" {
		return nil
	}
	for _, f := range files {
		base, inKeychain, err := h.readStore()
		if err != nil {
			return fmt.Errorf("%s: %w", h.a, err)
		}
		if h.blankItem != nil {
			base = h.blankItem // what Claude Code has been reading, secrets and all
		}
		merged, err := rota.MergeClaudeCredentials(base, f.Content)
		if err != nil {
			return err
		}
		svc, named := h.service()
		judged := h.item
		dropItem := func() error {
			if !named || !inKeychain {
				return nil
			}
			if err := dropItemAsJudged(svc, judged); err != nil {
				return fmt.Errorf("%s: could not remove the keychain item %q before writing its login: %w", h.a, svc, err)
			}
			return nil
		}
		// The item goes before the rename, so a rename that fails leaves the home
		// with no login and this the only copy of what stood beside it: it is
		// tried again from here rather than read back from a store that is gone.
		// An item somebody wrote meanwhile stops it at once.
		for try := 1; ; try++ {
			err = writeAtomicThen(filepath.Join(h.home, f.Path), merged, dropItem)
			if err == nil {
				break
			}
			if try == seedTries || errors.Is(err, errItemChanged) {
				return err
			}
			time.Sleep(seedPause)
		}
		h.a.StagedWritten()
	}
	return nil
}

// tidy leaves one copy where a credential file sits beside a keychain item.
// Claude Code reads the item and never the file while both exist, so a file no
// newer than the item is stale, and a stale one has pinned an old token in live
// sessions before: it goes. A newer file is the login, written there because
// the keychain refused it, so it stays and the item goes instead, or Claude
// Code would go on presenting a refresh token that was replaced; a blanked item
// hands what it kept beside the login to the file first (uncover). Only when
// nothing is alive, and never in the hold.
func (h *claudeHome) tidy() {
	svc, ok := h.service()
	if !ok || h.hold != "" {
		return
	}
	path := filepath.Join(h.home, claudeCredentials)
	if _, err := os.Lstat(path); err != nil {
		return
	}
	ctx, cancel := keychainContext()
	defer cancel()
	item, found, err := claudecode.Read(ctx, svc)
	if err != nil || !found {
		return
	}
	if file, err := os.ReadFile(path); err == nil && fileIsNewer(file, item) {
		if l, ok := storedLogin(item); ok && l.Refresh == "" {
			if err := h.uncover(); err != nil {
				h.warn(err.Error())
			}
			return
		}
		_ = claudecode.Delete(ctx, svc)
		return
	}
	_ = os.Remove(path)
}

/* ------------------------------------------------------------- launching --- */

// launch is everything a run on a claude account does before Claude Code
// starts, in the order that keeps its login alive: read the home, decide the
// route, retire a daemon from the other route, refresh and write only when
// rota may, and build the account's world.
func (h *claudeHome) launch(ctx context.Context, mirror bool) (*rota.Command, error) {
	a := h.a
	if !h.s.loginInHome(a) {
		if keychainKept && !a.APIKey() && h.s.owns(a) && rota.JoinHome(a, h.home) != nil && !nameable(h.home) {
			h.warn(fmt.Sprintf("%s: %s; it runs on a token in its environment, without Remote Control", a, h.s.whyNoLogin(a)))
		}
		if err := h.s.mayLaunch(a); err != nil {
			return nil, err
		}
		return h.launchUnkept(ctx, mirror)
	}
	if err := h.adopt(ctx); err != nil {
		return nil, err
	}
	if _, err := h.mend(ctx); err != nil && !a.Dead {
		var nt notThrough
		if !errors.As(err, &nt) {
			return nil, err
		}
		h.warn(err.Error()) // the launch goes on as it would have
	}
	// A dead account whose home holds a login nobody could confirm yet is
	// let through: Claude Code starting there is what makes it confirmable.
	if !h.heldDead() {
		if err := h.s.mayLaunch(a); err != nil {
			return nil, err
		}
	}
	switch {
	case h.blanked:
		return h.launchCleared(mirror)
	case !mirror:
		return h.envRun(ctx)
	case h.hold != "":
		return h.launchHeld()
	case !rota.StoresLogin(a, h.home) && !unrefreshed(a):
		return h.launchOnToken(ctx)
	}
	// A login held as a refresh token alone goes the stored way too: it is
	// refreshed there exactly when an expired one would be, in a quiet home,
	// and written in before the launch, which then takes the stored route on it.
	return h.launchStored(ctx)
}

// heldDead reports whether the account is dead and its home holds a usable
// login nobody could confirm yet — the one dead account rota launches, on
// that login and in that home.
func (h *claudeHome) heldDead() bool { return h.a.Dead && h.hold != "" && h.usable }

// Launchable says whether a run on this account would be let through, before
// anything is staged, so a server can refuse it before its reply is under
// way: a dead account is refused unless it holds a long-lived token, or its
// home holds a login made inside Claude Code that is waiting to be confirmed
// — which takes reading the home.
func (s *Store) Launchable(ctx context.Context, a *rota.Account) error {
	if !a.Dead || a.LongValid() {
		return nil
	}
	if rota.SharedHome(a.Provider) && s.loginInHome(a) {
		h := s.claudeHome(a, s.claimed(a))
		if err := h.adopt(ctx); err == nil && (!a.Dead || h.heldDead()) {
			return nil
		}
	}
	return rota.WrapReauth(a)
}

// launchCleared launches an account whose home Claude Code blanked and mend
// could not fill yet. Claude Code started there would not be signed in, so the
// run goes on a token that is good, or does not go, and the next look tries
// again.
func (h *claudeHome) launchCleared(mirror bool) (*rota.Command, error) {
	a := h.a
	if !a.LongValid() && a.Expired() {
		return nil, fmt.Errorf("%w: %s: Claude Code cleared the login in its home and rota could not refresh it just now; "+
			"try again in a moment, or a long-lived token (`rota login --long`) runs it meanwhile", rota.ErrBusy, a)
	}
	cmd, err := rota.Stage(a, "")
	if err != nil || !mirror {
		return cmd, err
	}
	return h.s.mirrored(a, cmd, h.quiet())
}

// launchHeld launches while the home holds something rota cannot place, and
// changes nothing: no refresh, no write, no daemon stopped, no route
// remembered. A usable login in the home is joined as it is; without one the
// run goes on a token, but only one that is good.
func (h *claudeHome) launchHeld() (*rota.Command, error) {
	a := h.a
	h.sayHold()
	if h.usable {
		return h.s.mirrored(a, rota.JoinHome(a, h.home), h.quiet())
	}
	if !a.LongValid() && a.Expired() {
		return nil, fmt.Errorf("%w: %s: %s, and its own token has expired, which nothing may refresh until that is settled; "+
			"a long-lived token (`rota login --long`) runs it meanwhile", rota.ErrBusy, a, h.hold)
	}
	cmd, err := rota.Stage(a, "")
	if err != nil {
		return nil, err
	}
	return h.s.mirrored(a, cmd, h.quiet())
}

// launchStored launches an account whose login lives in its home.
//
// The route is decided first. It is the stored route when nothing runs in
// the home, when the home holds a login Claude Code can use, or when all
// that runs there is a daemon started on the environment route and the
// sessions it hosts — stopping that daemon is then the change of route this
// launch makes, and leaves the home quiet. Otherwise Claude Code runs there
// with no login it could share: windows from before this version, on tokens
// in their environment, when the home was never on the stored route — this
// launch goes the same way — and the hold when it was, since a process there
// may still hold the account's refresh token.
//
// On the stored route nothing is written when the home already holds the
// account's current login. Otherwise the login goes in when rota may write
// (mayWrite); when it may not, the launch joins the login that is there and
// the person is told when the account's takes over. A stranded home was mended
// before this, and the launch joins the login just written; a refresh that did
// not get through leaves the launch to join the home as it found it.
func (h *claudeHome) launchStored(ctx context.Context) (*rota.Command, error) {
	a := h.a
	route := routeEnv
	switch {
	case h.quiet(), h.usable:
		route = routeStored
	case h.lastRoute() == routeEnv && h.daemonAlone():
		route = routeStored
	}
	if route == routeEnv && h.lastRoute() != routeEnv {
		h.hold = "the home was signed in on the account's login and shows none now while Claude Code runs there"
		return h.launchHeld()
	}
	h.retireFor(route)
	if route == routeStored && !h.quiet() && !h.usable {
		route = routeEnv // the daemon would not stop
	}
	if route == routeEnv {
		return h.launchBeside(ctx)
	}
	if _, err := h.renew(ctx); err != nil {
		// A login that died just now is dead like any other, with the one
		// exception mayLaunch allows: a long-lived token owes it nothing.
		if !a.Dead || !a.LongValid() {
			return nil, err
		}
		_ = h.s.mayLaunch(a) // says what the account can no longer tell
		return h.launchOnToken(ctx)
	}
	if h.hold != "" {
		return h.launchHeld() // a refused refresh found a login it could not place
	}
	if !rota.StoresLogin(a, h.home) {
		if a.Token.Access != "" {
			// Refreshed into a token with no expiry, which runs on the token
			// route as it always has.
			return h.launchOnToken(ctx)
		}
		// Still a refresh token alone: something runs here, so nothing may
		// refresh it, and the login the home holds is joined as it is.
		if !a.StagedCurrent() {
			h.sayAnotherLogin()
		}
		h.remember(routeStored)
		return h.s.mirrored(a, rota.JoinHome(a, h.home), h.quiet())
	}
	cmd, files, err := rota.StagePlan(ctx, a, h.home)
	if err != nil {
		return nil, err
	}
	switch {
	case len(files) > 0 && h.mayWrite():
		if err := h.seed(files); err != nil {
			return nil, err
		}
	case len(files) > 0:
		h.sayAnotherLogin()
	case h.quiet():
		h.tidy()
	}
	h.remember(routeStored)
	return h.s.mirrored(a, cmd, h.quiet())
}

// sayAnotherLogin is the one line a launch owes the person when it joins a
// login in the home that is not the account's current one.
func (h *claudeHome) sayAnotherLogin() {
	h.warn(fmt.Sprintf("%s: Claude Code is running in its home (%s) on another login than the account's current one; "+
		"the account's login takes over when those sessions end, or at once in any of them where you run /login",
		h.a, strings.Join(h.live(), ", ")))
}

// launchBeside launches on a token while Claude Code from before runs in a
// home that was never on the account's stored login and holds no login it
// could share. The account's refresh token then lives nowhere but in rota's
// store, so refreshing it is safe, as it always was.
func (h *claudeHome) launchBeside(ctx context.Context) (*rota.Command, error) {
	a := h.a
	if err := h.tokenNow(ctx); err != nil {
		return nil, err
	}
	h.warn(fmt.Sprintf("%s: Claude Code from before is still running in its home (%s) on a token in its environment, "+
		"so this run is too — without Remote Control — until those windows are closed", a, strings.Join(h.live(), ", ")))
	h.remember(routeEnv)
	cmd, err := rota.Stage(a, "")
	if err != nil {
		return nil, err
	}
	return h.s.mirrored(a, cmd, h.quiet())
}

// tokenNow makes the account's own token good for a run on the environment
// route: the long-lived one when it is worth using, else the access token,
// refreshed when it has expired — but only while nothing runs in the home,
// or while what runs there was never on the account's stored login, so that
// nothing alive can hold the refresh token being presented. The refresh goes
// through refresh, which reads the home again before it believes a refusal.
func (h *claudeHome) tokenNow(ctx context.Context) error {
	a := h.a
	if a.LongValid() || !a.Expired() {
		return nil
	}
	if !h.quiet() && h.lastRoute() != routeEnv {
		return fmt.Errorf("%w: %s's token has expired, and Claude Code runs in its home (%s), which was signed in on the "+
			"account's own login; nothing may refresh it while that runs — a long-lived token (`rota login --long`) "+
			"runs it meanwhile", rota.ErrBusy, a, strings.Join(h.live(), ", "))
	}
	changed, err := h.refresh(ctx)
	if changed {
		if serr := h.s.Save(); serr != nil {
			return errors.Join(err, fmt.Errorf("refusing to run: store not saved after a token change: %w", serr))
		}
	}
	if err == nil && h.hold != "" {
		return fmt.Errorf("%w: %s: %s, and its own token has expired", rota.ErrBusy, a, h.hold)
	}
	return err
}

// launchOnToken launches in the home on the environment route: a token rota
// holds, refreshed as it always was, with the home's daemon retired if it
// was started on the account's stored login.
func (h *claudeHome) launchOnToken(ctx context.Context) (*rota.Command, error) {
	h.retireFor(routeEnv)
	cmd, err := h.onToken(ctx)
	if err != nil {
		return nil, err
	}
	h.remember(routeEnv)
	return h.s.mirrored(h.a, cmd, h.quiet())
}

// launchUnkept launches an account whose login rota keeps in no home — a
// directory the person chose, Windows, a home whose path names no keychain
// item — on a token in its environment, refreshed as it always was. Nothing
// of rota's login was ever in that home, so nothing there changes for it:
// no daemon is stopped and no route remembered, and in a directory the
// person chose not even what runs there is asked.
func (h *claudeHome) launchUnkept(ctx context.Context, mirror bool) (*rota.Command, error) {
	if !mirror {
		return tokenRun(ctx, h.a)
	}
	cmd, err := h.onToken(ctx)
	if err != nil {
		return nil, err
	}
	return h.s.mirrored(h.a, cmd, h.s.owns(h.a) && h.quiet())
}

// onToken is the environment route's command for the account, its token
// refreshed first when the launch is going to use it — never a dead one's —
// and the store saved after any change.
func (h *claudeHome) onToken(ctx context.Context) (*rota.Command, error) {
	a := h.a
	changed, err := refreshForLaunch(ctx, a)
	if changed {
		if serr := h.s.Save(); serr != nil {
			return nil, errors.Join(err, fmt.Errorf("refusing to run: store not saved after a token change: %w", serr))
		}
	}
	if err != nil {
		return nil, err
	}
	return rota.Stage(a, "")
}

// envRun readies a run that takes no home — a hermetic one — on the
// environment route. Its token has to be good for the run: the long-lived
// one when there is one worth using, else the account's own access token,
// refreshed first if it has expired — while nothing runs in the home, or
// while what runs there was never on the account's stored login and holds
// no login of its own; never in the hold, and never for a dead account.
func (h *claudeHome) envRun(ctx context.Context) (*rota.Command, error) {
	a := h.a
	if a.LongValid() || !a.Expired() {
		return rota.Stage(a, "")
	}
	switch {
	case h.hold != "":
		return nil, fmt.Errorf("%w: %s: %s, and its access token has expired; a run without its home cannot be handed "+
			"a fresh one until that is settled — a long-lived token (`rota login --long`) is what such runs are for",
			rota.ErrBusy, a, h.hold)
	case h.quiet():
		if _, err := h.renew(ctx); err != nil {
			return nil, err
		}
		if a.Expired() {
			return nil, fmt.Errorf("%w: %s: %s, and its access token has expired; a long-lived token (`rota login --long`) "+
				"is what runs without the account's home are for", rota.ErrBusy, a, nonEmpty(h.hold, "it was not refreshed"))
		}
	case h.usable:
		return nil, fmt.Errorf("%w: %s's login is held by the Claude Code running in its home (%s), and its access token "+
			"has expired; a run without its home cannot be handed a fresh one while that runs — a long-lived token "+
			"(`rota login --long`) is what such runs are for", rota.ErrBusy, a, strings.Join(h.live(), ", "))
	default:
		if err := h.tokenNow(ctx); err != nil {
			return nil, err
		}
	}
	return rota.Stage(a, "")
}

// tokenRun is a run with no home for an account whose login rota does not
// keep in one: refreshed as it always was — never a dead one.
func tokenRun(ctx context.Context, a *rota.Account) (*rota.Command, error) {
	if !a.Dead && !a.LongValid() && a.Expired() {
		if _, err := rota.Refresh(ctx, a); err != nil {
			return nil, err
		}
	}
	return rota.Stage(a, "")
}

/* ------------------------------------------------- removing and moving --- */

// dropOwnLogin removes the account's login from its home, keychain item
// first: once the file is gone nothing here would name the item again. It
// touches nothing where rota keeps no login — a directory the person chose,
// Windows, a home whose path names no keychain item.
func (h *claudeHome) dropOwnLogin() error {
	if !h.s.loginInHome(h.a) {
		return nil
	}
	if svc, ok := h.service(); ok {
		ctx, cancel := keychainContext()
		err := claudecode.Delete(ctx, svc)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: could not remove the keychain item %q that holds its login: %w", h.a, svc, err)
		}
	}
	if err := os.Remove(filepath.Join(h.home, claudeCredentials)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// quiesce makes a claude account's home quiet before it is removed: its
// daemon is stopped, which ends the background sessions it hosts, and the
// home is given a few seconds to go quiet. Whatever is still alive then —
// a window somebody opened in that home — is named, and the removal refused.
func (h *claudeHome) quiesce() error {
	if len(claudeLive(h.home)) == 0 {
		return nil
	}
	if what, alive := daemonAlive(h.home); alive {
		if err := h.stopDaemon(); err != nil {
			h.warn(fmt.Sprintf("%s: could not stop the Claude Code daemon in its home (%s): %v", h.a, what, err))
		} else {
			h.warn(fmt.Sprintf("%s: stopped the Claude Code daemon in its home (%s) and the background sessions it hosted", h.a, what))
		}
	}
	deadline := time.Now().Add(removalWait)
	for {
		live := claudeLive(h.home)
		if len(live) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: Claude Code is still running in %s's home (%s); close it before removing the account",
				rota.ErrBusy, h.a, strings.Join(live, ", "))
		}
		time.Sleep(removalPoll)
	}
}

// MoveHome gives a claude account another home — a directory the person
// chose, or rota's own again when configDir is "" — and saves it. It does
// nothing when the home stays where it is or the provider keeps no login in
// a home; the caller sets ConfigDir then, as for any other account. It goes
// before every other change a command makes to the account, because what it
// saves would carry them.
//
// Leaving a home rota keeps the account's login in, that home is read first:
// Claude Code may have rotated the login there since rota last looked, and
// that rotation would otherwise be deleted with the home's copy, leaving the
// account with a spent token. What the reading took is saved before anything
// is removed. A home in the hold, or one where Claude Code runs, is refused:
// its processes share the login there. Then the login is taken out of that
// home, keychain item first, so no second copy of one refresh token is left
// for anybody to present; in a directory the person chose the account goes on
// with the tokens it holds, refreshed by rota as before. Leaving a directory
// the person chose reads and removes nothing: no login of rota's is there.
//
// Either way the account forgets what it knew about the old home, so a home
// rota keeps its login in is written from nothing on its first launch there
// and its daemon's route is its own. A step that fails leaves the account
// and the old home as they were before it.
func (s *Store) MoveHome(ctx context.Context, a *rota.Account, configDir string) error {
	if !rota.SharedHome(a.Provider) {
		return nil
	}
	from, to := s.Home(a), configDir
	if to == "" {
		to = s.ownHome(a)
	}
	if sameDir(from, to) {
		return nil
	}
	if s.loginInHome(a) {
		if s.InUse(a) {
			var live []string
			if s.claimed(a) {
				live = append(live, "a rota run")
			}
			live = append(live, claudeLive(from)...)
			return fmt.Errorf("%w: Claude Code is running in %s's home (%s); its configuration directory can change once that has stopped",
				rota.ErrBusy, a, strings.Join(live, ", "))
		}
		h := s.claudeHome(a, false)
		if err := h.adopt(ctx); err != nil {
			return err
		}
		if h.hold != "" {
			return fmt.Errorf("%w: %s: %s; start the account once in its home, or try again, before moving it",
				rota.ErrBusy, a, h.hold)
		}
		if err := s.Save(); err != nil {
			return err
		}
		if err := h.dropOwnLogin(); err != nil {
			return err
		}
	}
	a.StagedSuperseded()
	delete(a.Extra, routeKey)
	delete(a.Extra, seedOwedKey)
	a.ConfigDir = configDir
	return s.Save()
}
