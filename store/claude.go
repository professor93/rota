package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
// A refresh token the provider refused is never presented again. An account
// whose token was refused is dead, and a dead account is never refreshed
// anywhere; it comes back only by a fresh login — `rota login`, or /login
// inside one of its windows, which rota confirms and takes.
//
// In a directory the person chose, which may be their own, rota never takes
// or replaces a login it cannot show is the account's: a login there it did
// not write is confirmed with the provider first, and one that belongs to
// somebody else is left exactly where it is.

// Names inside a Claude Code configuration directory that rota reads to
// tell whether anything is alive there, and the credential store it writes.
const (
	claudeCredentials = ".credentials.json"
	claudeDaemonLock  = "daemon.lock"
	claudeSessionsDir = "sessions"
)

// routeKey is where an account remembers which route its last launch in its
// home took, so the next launch can tell when the home's daemon was started
// on the other one. It belongs to the home, and is forgotten when the
// account is given another (MoveHome).
const (
	routeKey    = "route"
	routeStored = "stored"
	routeEnv    = "env"
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
)

// errKeychain marks a keychain that could not be read, which is not the
// same as one that holds no item.
var errKeychain = errors.New("the keychain could not be read")

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
	// foreign names the account a directory the person chose is signed in
	// as instead, when the provider has said so; "" otherwise. That login is
	// left exactly where it is.
	foreign string
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

// personalClaude reports whether an account's home is the person's own
// Claude Code directory: the directory the environment says is theirs (see
// personalWorld), or ~/.claude — compared by file identity, so a second
// spelling is the same directory. The login there is the person's own, and
// rota never reads or writes it: such an account keeps the environment
// route.
func (s *Store) personalClaude(a *rota.Account) bool {
	if a.ConfigDir == "" {
		return false // rota's own home for it, which is never the person's
	}
	if dir, _ := s.personalDir(); sameDir(dir, a.ConfigDir) {
		return true
	}
	return sameDir(defaultClaudeDir(), a.ConfigDir)
}

// loginInHome reports whether this account's login is one rota keeps in its
// home at all, whatever state the login is in: the platform keeps logins in
// homes (not Windows), the home is not the person's own, and on macOS its
// path names a keychain item rota can find. An account for which it is
// false runs on a token in its environment, as every claude account did
// before.
func (s *Store) loginInHome(a *rota.Account) bool {
	home := s.Home(a)
	if rota.JoinHome(a, home) == nil || s.personalClaude(a) {
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
// a refresh token and an expiry, and not dead.
func (s *Store) keepsLogin(a *rota.Account) bool {
	return s.loginInHome(a) && rota.StoresLogin(a, s.Home(a))
}

// whyNoLogin says why an account does not run on a login of its own in its
// home, in a few words a person can act on; "" when it does.
func (s *Store) whyNoLogin(a *rota.Account) string {
	home := s.Home(a)
	switch {
	case rota.JoinHome(a, home) == nil:
		return "this platform keeps Claude Code on a token in its environment"
	case s.personalClaude(a):
		return "its configuration directory is your own, whose login rota never touches"
	case keychainKept && !nameable(home):
		return "its home " + home + " is not a plain ASCII path, so the keychain item Claude Code would keep its login in cannot be found"
	case a.Dead:
		return "its login is dead"
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
// keychain item when there is one, and the file only when there is not. A
// keychain that cannot be read is errKeychain; a file that cannot be read
// is the read's own error, which adoption takes for an unreadable store.
func (h *claudeHome) readStore() (raw []byte, inKeychain bool, err error) {
	if svc, ok := h.service(); ok {
		ctx, cancel := keychainContext()
		defer cancel()
		raw, found, err := claudecode.Read(ctx, svc)
		if err != nil {
			return nil, false, fmt.Errorf("%w: the item %q that holds its login: %v", errKeychain, svc, err)
		}
		if found {
			return raw, true, nil
		}
	}
	raw, err = os.ReadFile(filepath.Join(h.home, claudeCredentials))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return raw, false, err
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
// In a directory the person chose, any usable login there that is not the
// account's current one nor the one rota recorded writing goes to the
// provider first, whatever Staged says: that directory may be the person's
// own, and their login must never be taken for a rotation of the account's,
// or written over as an older one.
func (h *claudeHome) adopt(ctx context.Context) error {
	h.hold, h.usable, h.spent, h.foreign = "", false, false, ""
	if err := h.s.oneHomeOneAccount(h.a); err != nil {
		return err
	}
	fsys, ok := h.read()
	if !ok {
		return nil
	}
	kind, nl := rota.ClaudeHomeLogin(h.a, fsys)
	h.spent = kind == rota.HomeLoginWritten
	if kind == rota.HomeLoginOther && !h.s.owns(h.a) {
		h.usable = true
		h.confirm(ctx, nl, true)
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
		if err = rota.AdoptFrom(h.a, fsys); errors.Is(err, rota.ErrUnreadableLogin) {
			h.a.StagedSuperseded() // empty, then: the next write replaces it
			err = nil
		}
	}
	if errors.As(err, &nl) {
		err = nil
		h.confirm(ctx, nl, false)
	}
	if err != nil {
		return err
	}
	h.usable = rota.HoldsLogin(h.a, fsys)
	return nil
}

// confirm settles a login found in the home that is not the account's own,
// by asking the provider whose it is with that login's own access token. The
// account's is taken — a dead account is alive again. Somebody else's is not
// taken: in a home rota made it is said so and replaced when nothing runs
// there; in a directory the person chose (chosen) it is left exactly where it
// is. When the provider cannot be asked, for whatever reason, a lapsed
// access token included, the home is in the hold.
func (h *claudeHome) confirm(ctx context.Context, nl *rota.NewLogin, chosen bool) {
	id, err := nl.Identify(ctx)
	switch {
	case err != nil:
		h.hold = fmt.Sprintf("Claude Code in its home holds a login made there that rota has not been able to confirm yet (%v)", err)
	case rota.MatchIdentity([]*rota.Account{h.a}, h.a.Provider, id) != nil:
		nl.Accept(h.a)
	case chosen:
		h.a.StagedSuperseded()
		h.foreign = id.Email
		if h.foreign == "" {
			h.foreign = id.UUID
		}
		if h.foreign == "" {
			h.foreign = "another account"
		}
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
// nothing remembered, a home rota made was last launched by a rota that knew
// only the environment; a directory the person chose may have been anybody's,
// and its route is unknown ("").
func (h *claudeHome) lastRoute() string {
	if r := h.a.Extra[routeKey]; r != "" {
		return r
	}
	if h.s.owns(h.a) {
		return routeEnv
	}
	return ""
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
	if last == "" || last == route {
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
// in the hold, the account is not dead and nothing is alive there, and puts
// the new login into the home at once — after the store has it on disk,
// because from the moment the provider answers, the refresh token the home
// holds is spent. That spent login is also why the write is made even if
// something started in the home meanwhile (see mayWrite).
func (h *claudeHome) renew(ctx context.Context) (bool, error) {
	a := h.a
	if !a.Expired() || a.Dead || h.hold != "" || !h.quiet() {
		return false, nil
	}
	changed, err := h.refresh(ctx)
	if err != nil || !changed || a.Dead || h.hold != "" {
		return changed, err
	}
	if err := h.s.Save(); err != nil {
		return true, fmt.Errorf("refusing to go on: the store could not be saved after a token change: %w", err)
	}
	return true, h.writeRefreshed(ctx)
}

// writeRefreshed puts a login rota has just refreshed and saved into the
// home, when it may.
func (h *claudeHome) writeRefreshed(ctx context.Context) error {
	if fsys, ok := h.read(); ok {
		kind, _ := rota.ClaudeHomeLogin(h.a, fsys)
		h.spent = kind == rota.HomeLoginWritten
	}
	if !h.mayWrite() {
		return nil
	}
	_, files, err := rota.StagePlan(ctx, h.a, h.home)
	if err != nil {
		return err
	}
	return h.seed(files)
}

// mayWrite reports whether rota may write the account's login into the home
// now: never in the hold or over another account's login in a directory the
// person chose; when nothing is alive there; and, the one exception to that,
// when the home still holds the very login rota itself refreshed away. That
// login is certainly spent — the provider issued its successor, which the
// account holds — so every window still on it is signed out at its next
// refresh anyway, and leaving it there only makes sure of that.
func (h *claudeHome) mayWrite() bool {
	if h.hold != "" || h.foreign != "" {
		return false
	}
	return h.quiet() || h.spent
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
	changed, err = rota.Refresh(ctx, a)
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

func nonEmpty(s, otherwise string) string {
	if s != "" {
		return s
	}
	return otherwise
}

// seed writes the account's login into its home. It is only ever called
// when nothing is alive there, and never in the hold.
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
	if h.hold != "" || h.foreign != "" {
		return nil
	}
	for _, f := range files {
		base, inKeychain, err := h.readStore()
		if err != nil {
			return fmt.Errorf("%s: %w", h.a, err)
		}
		merged, err := rota.MergeClaudeCredentials(base, f.Content)
		if err != nil {
			return err
		}
		svc, named := h.service()
		dropItem := func() error {
			if !named || !inKeychain {
				return nil
			}
			ctx, cancel := keychainContext()
			defer cancel()
			if err := claudecode.Delete(ctx, svc); err != nil {
				return fmt.Errorf("%s: could not remove the keychain item %q before writing its login: %w", h.a, svc, err)
			}
			return nil
		}
		if err := writeAtomicThen(filepath.Join(h.home, f.Path), merged, dropItem); err != nil {
			return err
		}
		h.a.StagedWritten()
	}
	return nil
}

// tidy removes a credential file left beside a keychain item. Claude Code
// reads the item and never the file while both exist, so the file is stale
// by definition — and a stale one has pinned an old token in live sessions
// before. Only when nothing is alive, and never in the hold.
func (h *claudeHome) tidy() {
	svc, ok := h.service()
	if !ok || h.hold != "" || h.foreign != "" {
		return
	}
	path := filepath.Join(h.home, claudeCredentials)
	if _, err := os.Lstat(path); err != nil {
		return
	}
	ctx, cancel := keychainContext()
	defer cancel()
	if _, found, err := claudecode.Read(ctx, svc); err == nil && found {
		_ = os.Remove(path)
	}
}

/* ------------------------------------------------------------- launching --- */

// launch is everything a run on a claude account does before Claude Code
// starts, in the order that keeps its login alive: read the home, decide the
// route, retire a daemon from the other route, refresh and write only when
// rota may, and build the account's world.
func (h *claudeHome) launch(ctx context.Context, mirror bool) (*rota.Command, error) {
	a := h.a
	if !h.s.loginInHome(a) {
		if keychainKept && rota.JoinHome(a, h.home) != nil && !h.s.personalClaude(a) && !nameable(h.home) {
			h.warn(fmt.Sprintf("%s: %s; it runs on a token in its environment, without Remote Control", a, h.s.whyNoLogin(a)))
		}
		if err := h.s.mayLaunch(a); err != nil {
			return nil, err
		}
		if !mirror {
			return tokenRun(ctx, a)
		}
		return h.launchOnToken(ctx)
	}
	if err := h.adopt(ctx); err != nil {
		return nil, err
	}
	// A dead account whose home holds a login nobody could confirm yet is
	// let through: Claude Code starting there is what makes it confirmable.
	if !h.heldDead() {
		if err := h.s.mayLaunch(a); err != nil {
			return nil, err
		}
	}
	switch {
	case h.foreign != "":
		return h.launchAside(ctx, mirror)
	case !mirror:
		return h.envRun(ctx)
	case h.hold != "":
		return h.launchHeld()
	case !rota.StoresLogin(a, h.home):
		return h.launchOnToken(ctx)
	}
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

// launchAside launches an account whose chosen directory is signed in as
// somebody else: that login is left exactly where it is, and the run goes on
// the account's own token in its environment.
func (h *claudeHome) launchAside(ctx context.Context, mirror bool) (*rota.Command, error) {
	a := h.a
	h.warn(fmt.Sprintf("%s: its configuration directory %s holds another account's login (%s), which rota leaves alone; "+
		"it runs on a token in its environment", a, h.home, h.foreign))
	if err := h.tokenNow(ctx); err != nil {
		return nil, err
	}
	cmd, err := rota.Stage(a, "")
	if err != nil || !mirror {
		return cmd, err
	}
	h.remember(routeEnv)
	// Not quiet, so nothing about the directory's own .claude.json changes:
	// it is somebody else's world as much as the login is.
	return h.s.mirrored(a, cmd, false)
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
// the person is told when the account's takes over.
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
		h.warn(fmt.Sprintf("%s: Claude Code is running in its home (%s) on another login than the account's current one; "+
			"the account's login takes over when those sessions end, or at once in any of them where you run /login",
			a, strings.Join(h.live(), ", ")))
	case h.quiet():
		h.tidy()
	}
	h.remember(routeStored)
	return h.s.mirrored(a, cmd, h.quiet())
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
	a := h.a
	h.retireFor(routeEnv)
	changed, err := refreshForLaunch(ctx, a)
	if changed {
		if serr := h.s.Save(); serr != nil {
			return nil, errors.Join(err, fmt.Errorf("refusing to run: store not saved after a token change: %w", serr))
		}
	}
	if err != nil {
		return nil, err
	}
	cmd, err := rota.Stage(a, "")
	if err != nil {
		return nil, err
	}
	h.remember(routeEnv)
	return h.s.mirrored(a, cmd, h.quiet())
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
	case h.usable && h.foreign == "":
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
// touches nothing where rota keeps no login (Windows, a home whose path
// names no keychain item, the person's own directory). In a home rota made
// the login there goes whatever it is; in a directory the person chose it
// goes only when it is provably this account's — its current refresh token,
// or the one rota recorded writing — because a login rota cannot show is
// the account's may be the person's own.
func (h *claudeHome) dropOwnLogin() error {
	if !h.s.loginInHome(h.a) {
		return nil
	}
	if !h.s.owns(h.a) {
		fsys, ok := h.read()
		if !ok {
			return fmt.Errorf("%s: %s, so whose login its directory holds cannot be told; nothing was removed", h.a, h.hold)
		}
		if kind, _ := rota.ClaudeHomeLogin(h.a, fsys); kind != rota.HomeLoginCurrent && kind != rota.HomeLoginWritten {
			return nil
		}
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

// MoveHome gives a claude account another home — a new configuration
// directory, or rota's own again when configDir is "" — and saves it. It
// does nothing when the home stays where it is or the provider keeps no
// login in a home; the caller sets ConfigDir then, as for any other account.
//
// Where rota keeps the account's login in its home, the home is read first:
// Claude Code may have rotated the login there since rota last looked, and
// that rotation would otherwise be deleted with the home's copy, leaving the
// account with a spent token. What the reading took is saved before anything
// is removed. A home in the hold, or one where Claude Code runs, is refused:
// its processes share the login there. Then the login is taken out of the
// old home — keychain item first, and in a directory the person chose only
// when it is provably the account's — so no second copy of one refresh token
// is left for anybody to present; the account forgets what it knew about
// that home, so the new one is written from nothing on its first launch and
// its daemon's route is its own. A step that fails leaves the account and
// the old home as they were before it.
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
	a.ConfigDir = configDir
	return s.Save()
}
