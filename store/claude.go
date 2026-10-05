package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// issues the next one — and Claude Code already is one. When nothing is
// alive, rota may refresh, and a refresh is followed at once, after the
// store is saved, by writing the new login into the home, so nothing ever
// starts there on a spent token.

// Names inside a Claude Code configuration directory that rota reads to
// tell whether anything is alive there, and the credential store it writes.
const (
	claudeCredentials = ".credentials.json"
	claudeDaemonLock  = "daemon.lock"
	claudeSessionsDir = "sessions"
)

// routeKey is where an account remembers which route its last launch in its
// home took, so the next launch can tell when the home's daemon was started
// on the other one.
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
)

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

// claudeLive names what is alive in a Claude Code home, by the records Claude
// Code keeps there itself: its daemon's lock, which names the daemon's pid
// and is removed when it stops, and one record per live session process,
// removed when that process exits. Both are the account's own, never shared
// with anybody's.
//
// A record naming a pid that is no longer running is a leftover and says
// nothing. A record that cannot be read could be anything, and counts as
// alive: the two mistakes do not cost the same, and a home wrongly thought
// quiet has its login written over underneath a running process.
func claudeLive(home string) []string {
	var live []string
	if what, ok := daemonAlive(home); ok {
		live = append(live, what)
	}
	dir := filepath.Join(home, claudeSessionsDir)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return append(live, claudeSessionsDir+"/ (unreadable)")
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue // ended between the listing and now
		}
		pid, ok := recordPID(raw, err)
		switch {
		case !ok:
			live = append(live, claudeSessionsDir+"/"+e.Name()+" (unreadable)")
		case proc.Alive(pid):
			live = append(live, "session "+strconv.Itoa(pid))
		}
	}
	return live
}

// daemonAlive reports whether a home's daemon is running, and names it.
func daemonAlive(home string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(home, claudeDaemonLock))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	pid, ok := recordPID(raw, err)
	switch {
	case !ok:
		return claudeDaemonLock + " (unreadable)", true
	case proc.Alive(pid):
		return "daemon " + strconv.Itoa(pid), true
	}
	return "", false
}

// recordPID reads the pid a liveness record names.
func recordPID(raw []byte, err error) (int, bool) {
	if err != nil {
		return 0, false
	}
	var rec struct {
		PID float64 `json:"pid"`
	}
	if rota.UnmarshalLenient(raw, &rec) != nil || rec.PID <= 0 {
		return 0, false
	}
	return int(rec.PID), true
}

// live is everything alive in the home, a rota run holding the account
// included.
func (h *claudeHome) live() []string {
	live := claudeLive(h.home)
	if h.others {
		live = append([]string{"another rota run"}, live...)
	}
	return live
}

// quiet reports whether nothing at all is alive in the home.
func (h *claudeHome) quiet() bool { return len(h.live()) == 0 }

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
	f, got, err := tryLockFile(lock)
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
// Claude Code directory — an account told its configuration lives in
// ~/.claude, say. The login there is the person's own, and rota never reads
// or writes it: such an account keeps the environment route.
func (s *Store) personalClaude(a *rota.Account) bool {
	home := realDir(s.Home(a))
	if src, _ := claudeConfigSource(); src != "" && realDir(src) == home {
		return true
	}
	if u, err := os.UserHomeDir(); err == nil && realDir(filepath.Join(u, ".claude")) == home {
		return true
	}
	return false
}

// keepsLogin reports whether an account's login lives in its home: lib takes
// the stored route for it, and the home is not the person's own.
func (s *Store) keepsLogin(a *rota.Account) bool {
	return rota.StoresLogin(a, s.Home(a)) && !s.personalClaude(a)
}

// service is the keychain item Claude Code keeps this home's login in, on
// the platform that has one. A home whose path is not plain ASCII has no
// name rota can work out, and is said so once.
func (h *claudeHome) service() (string, bool) {
	if !keychainKept {
		return "", false
	}
	svc, ok := claudecode.Service(h.home)
	if !ok && !h.warnedPath {
		h.warnedPath = true
		h.warn(fmt.Sprintf("%s: its home %s is not a plain ASCII path, so the keychain item Claude Code keeps its login in "+
			"cannot be named; rota uses the file alone there, and rotations Claude Code keeps in the keychain cannot be "+
			"followed for this home", h.a, h.home))
	}
	return svc, ok
}

func keychainContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), keychainTimeout)
}

// readStore reads the home's credential store the way Claude Code does: the
// keychain item when there is one, and the file only when there is not.
func (h *claudeHome) readStore() (raw []byte, inKeychain bool, err error) {
	if svc, ok := h.service(); ok {
		ctx, cancel := keychainContext()
		defer cancel()
		raw, found, err := claudecode.Read(ctx, svc)
		if err != nil {
			return nil, false, fmt.Errorf("%s: could not read the keychain item %q that holds its login: %w", h.a, svc, err)
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

// adopt reads the home's login into the account, and settles a new login
// found there by asking the provider whose it is: the same account's is
// taken, and a dead account is alive again; somebody else's is refused and
// said so; and when the provider cannot be asked right now nothing changes,
// and the next adoption asks again. The home keeps working meanwhile — the
// login there is Claude Code's.
func (h *claudeHome) adopt(ctx context.Context) error {
	if h.s.personalClaude(h.a) {
		return nil
	}
	raw, _, err := h.readStore()
	if err != nil {
		return err
	}
	err = rota.AdoptFrom(h.a, storeFS{FS: os.DirFS(h.home), store: raw})
	var nl *rota.NewLogin
	if !errors.As(err, &nl) {
		return err
	}
	id, ierr := nl.Identify(ctx)
	switch {
	case ierr != nil:
	case rota.MatchIdentity([]*rota.Account{h.a}, h.a.Provider, id) != nil:
		nl.Accept(h.a)
	default:
		h.warn(nl.Refuse(h.a, id).Error())
	}
	return nil
}

// storeFS is a home as Claude Code reads it: its files, with the credential
// store answered by what readStore found — the keychain item on macOS when
// there is one.
type storeFS struct {
	fs.FS
	store []byte
}

func (f storeFS) ReadFile(name string) ([]byte, error) {
	if name != claudeCredentials {
		return fs.ReadFile(f.FS, name)
	}
	if f.store == nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return bytes.Clone(f.store), nil
}

func (f storeFS) Open(name string) (fs.File, error) {
	if name != claudeCredentials {
		return f.FS.Open(name)
	}
	if f.store == nil {
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

// retire stops the home's daemon when it was started on the other route.
//
// A daemon keeps the credential it started with for life and serves it to
// every background session it starts. One started on a token in its
// environment goes on handing out that token after the account has moved to
// its stored login — until it is revoked, and every background session it
// hosts says so. One started on the stored login would go on serving it
// after the account has gone back to a token. An account with no route
// remembered was last launched by a rota that knew only the environment.
//
// It runs before anything is decided about writing, because a daemon that
// stops may leave the home with nothing alive in it.
func (h *claudeHome) retire(route string) {
	last := h.a.Extra[routeKey]
	if last == "" {
		last = routeEnv
	}
	if last == route {
		return
	}
	what, alive := daemonAlive(h.home)
	if !alive {
		return
	}
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

// renew refreshes the account's login when it has expired and nothing is
// alive in its home, and puts the new login into the home at once — after
// the store has it on disk, because from the moment the provider answers,
// the refresh token the home holds is spent.
//
// A refusal is not believed until the home has been read again: a sibling
// may have refreshed first, between the last reading and this request, and
// what looks like a dead login is then a login rota had simply not caught up
// with.
func (h *claudeHome) renew(ctx context.Context) (bool, error) {
	if !h.a.Expired() || !h.quiet() {
		return false, nil
	}
	changed, err := rota.Refresh(ctx, h.a)
	if err != nil && h.a.Dead {
		if aerr := h.adopt(ctx); aerr == nil && !h.a.Dead {
			return true, nil
		}
	}
	if err != nil || !changed {
		return changed, err
	}
	if err := h.s.Save(); err != nil {
		return true, fmt.Errorf("refusing to go on: the store could not be saved after a token change: %w", err)
	}
	_, files, err := rota.StagePlan(ctx, h.a, h.home)
	if err != nil {
		return true, err
	}
	return true, h.seed(files)
}

// seed writes the account's login into its home. It is only ever called
// when nothing is alive there.
//
// The login replaces only itself in the store: Claude Code keeps the OAuth
// tokens of MCP servers and secrets of its own beside it. On macOS the
// keychain item goes first and the file is written after — Claude Code reads
// the item before the file, and rota never writes an item: a write through
// security resets who may read it. Claude Code moves the login back into the
// keychain itself on its next refresh.
func (h *claudeHome) seed(files []rota.StagedFile) error {
	for _, f := range files {
		base, inKeychain, err := h.readStore()
		if err != nil {
			return err
		}
		merged, err := rota.MergeClaudeCredentials(base, f.Content)
		if err != nil {
			return err
		}
		if svc, ok := h.service(); ok && inKeychain {
			ctx, cancel := keychainContext()
			err := claudecode.Delete(ctx, svc)
			cancel()
			if err != nil {
				return fmt.Errorf("%s: could not remove the keychain item %q before writing its login: %w", h.a, svc, err)
			}
		}
		if err := writeAtomic(filepath.Join(h.home, f.Path), merged); err != nil {
			return err
		}
		h.a.StagedWritten()
	}
	return nil
}

// tidy removes a credential file left beside a keychain item. Claude Code
// reads the item and never the file while both exist, so the file is stale
// by definition — and a stale one has pinned an old token in live sessions
// before. Only when nothing is alive.
func (h *claudeHome) tidy() {
	svc, ok := h.service()
	if !ok {
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

// stage decides how this launch starts, on an account whose login lives in
// its home, and writes the login there when it may.
//
// Nothing is written when the home already holds the account's current
// login. Otherwise a write is wanted, and what happens depends on whether
// anything is alive there:
//   - nothing: the login is written, and the launch joins it;
//   - something, on a login the home can still use — an earlier one, from
//     before rota was logged in again: nothing is written, the launch joins
//     what is there, and the person is told when the new login takes over;
//   - something, with no usable login in the home — windows from before
//     this rota, on tokens in their environment: nothing is written, and
//     this launch goes the same way, because a launch must never produce a
//     Claude Code that is not signed in.
func (h *claudeHome) stage(ctx context.Context) (*rota.Command, error) {
	a := h.a
	cmd, files, err := rota.StagePlan(ctx, a, h.home)
	if err != nil {
		return nil, err
	}
	live := h.live()
	switch {
	case len(files) == 0:
		if len(live) == 0 {
			h.tidy()
		}
		h.remember(routeStored)
		return cmd, nil
	case len(live) == 0:
		if err := h.seed(files); err != nil {
			return nil, err
		}
		h.remember(routeStored)
		return cmd, nil
	}
	who := strings.Join(live, ", ")
	if raw, _, err := h.readStore(); err == nil && rota.HoldsLogin(a, storeFS{FS: os.DirFS(h.home), store: raw}) {
		h.warn(fmt.Sprintf("%s: Claude Code is running in its home (%s) on another login than the account's current one; "+
			"the account's login takes over when those sessions end, or at once in any of them where you run /login", a, who))
		h.remember(routeStored)
		return cmd, nil
	}
	if a.Expired() && !a.LongValid() {
		return nil, fmt.Errorf("%w: Claude Code from before is still running in %s's home (%s) on a token in its environment, "+
			"and the account's own token has expired, which nothing may refresh while those run; close them, or keep a "+
			"long-lived token for times like this (`rota login --long`)", rota.ErrBusy, a, who)
	}
	h.warn(fmt.Sprintf("%s: Claude Code from before is still running in its home (%s) on a token in its environment, "+
		"so this run is too — without Remote Control — until those windows are closed", a, who))
	h.remember(routeEnv)
	return rota.Stage(a, "")
}

// envRun readies a run that takes no home — a hermetic one — on the
// environment route. Its token has to be good for the run: the long-lived
// one when there is one worth using, else the account's own access token,
// refreshed first if it has expired. An account whose login lives in its
// home may only be refreshed while nothing runs there.
func (h *claudeHome) envRun(ctx context.Context) (*rota.Command, error) {
	a := h.a
	if !a.LongValid() && a.Expired() {
		switch {
		case !h.s.keepsLogin(a):
			if _, err := rota.Refresh(ctx, a); err != nil {
				return nil, err
			}
		case !h.quiet():
			return nil, fmt.Errorf("%w: %s's login is held by the Claude Code running in its home (%s), and its access token "+
				"has expired; a run without its home cannot be handed a fresh one while that runs — a long-lived token "+
				"(`rota login --long`) is what such runs are for", rota.ErrBusy, a, strings.Join(h.live(), ", "))
		default:
			if _, err := h.renew(ctx); err != nil {
				return nil, err
			}
		}
	}
	return rota.Stage(a, "")
}

// launch is everything a run on a claude account does before Claude Code
// starts, in the order that keeps its login alive: read the home, retire a
// daemon from the other route, refresh only when nothing is alive, write the
// login only when nothing is alive, and build the account's world.
func (h *claudeHome) launch(ctx context.Context, mirror bool) (*rota.Command, error) {
	a := h.a
	if err := h.adopt(ctx); err != nil {
		return nil, err
	}
	// After adoption, because a login found in the home may have revived a
	// dead account.
	if err := h.s.mayLaunch(a); err != nil {
		return nil, err
	}
	if !mirror {
		return h.envRun(ctx)
	}
	if !h.s.keepsLogin(a) {
		return h.launchOnToken(ctx)
	}
	h.retire(routeStored)
	if _, err := h.renew(ctx); err != nil {
		// A login that died just now is dead like any other, with the one
		// exception mayLaunch allows: a long-lived token owes it nothing.
		if !a.Dead || !a.LongValid() {
			return nil, err
		}
		_ = h.s.mayLaunch(a) // says what the account can no longer tell
		return h.launchOnToken(ctx)
	}
	cmd, err := h.stage(ctx)
	if err != nil {
		return nil, err
	}
	return h.s.mirrored(a, cmd, h.quiet())
}

// launchOnToken launches in the home on the environment route: a token rota
// holds, refreshed as it always was, with the home's daemon retired if it
// was started on the account's stored login.
func (h *claudeHome) launchOnToken(ctx context.Context) (*rota.Command, error) {
	a := h.a
	h.retire(routeEnv)
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

/* ------------------------------------------------------------- removing --- */

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

// forget removes the account's login from its home before the home goes,
// keychain item first: once the directory is gone nothing would name the
// item again. In a directory the person chose only the login goes.
func (h *claudeHome) forget() error {
	if h.s.personalClaude(h.a) {
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
	if !h.s.owns(h.a) {
		if err := os.Remove(filepath.Join(h.home, claudeCredentials)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
