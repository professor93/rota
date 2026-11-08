package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/lend"
	rota "github.com/professor93/rota/lib"
)

// Prepare readies an account to run its vendor CLI: it rotates an expired
// token, lets the provider stage credentials into the account's private
// home, and saves the store — refusing to go on if that save fails, because
// a rotated token that is not on disk is a token lost.
// It returns the resolved executable, the complete child environment, and
// the claim on the account; pass the first two to Exec.
//
// The claim is the same one a run takes, for the same reason: this stages a
// credential into a home whose CLI may already own it. It is returned rather
// than released here because the CLI is about to run on it: a caller that
// starts the CLI as a child — a terminal, a shared session — holds it until
// that child ends and then releases it; a caller that replaces this process
// with the CLI calls KeepClaimsAcrossExec immediately before, or lends the
// claim to a process of its own that holds it until the CLI exits
// (lentClaims), as the program does for codex, so the claim outlives the
// replacing; and a caller that does not go through with either calls
// release.
//
// The CLI is taken to start where this process is, which is where the
// handover starts it; one started anywhere else is prepared with PrepareIn.
func (s *Store) Prepare(ctx context.Context, a *rota.Account) (path string, env []string, release func(), err error) {
	return s.PrepareIn(ctx, a, "")
}

// PrepareIn is Prepare for a CLI that starts in dir, "" being this process's
// own directory. The directory is part of what the CLI is launched with:
// Claude Code reads the settings files of the project there, and those are
// among the files a launch warns about (see sayRiskySettings), so a
// terminal started somewhere else is warned about its own directory's.
func (s *Store) PrepareIn(ctx context.Context, a *rota.Account, dir string) (path string, env []string, release func(), err error) {
	cmd, release, err := s.prepare(ctx, a, true)
	if err != nil {
		return "", nil, nil, err
	}
	handed := false
	defer unlessHanded(&handed, release)
	path, err = exec.LookPath(cmd.Bin)
	if err != nil {
		return "", nil, nil, fmt.Errorf("%s not found in PATH: %w", cmd.Bin, err)
	}
	env = rota.Environ(HostEnv(), cmd)
	cwd, aerr := filepath.Abs(dir)
	if aerr != nil {
		cwd = ""
	}
	s.sayRiskySettings(a, env, cwd, nil, false)
	s.sayFederationProfile(a, env, cwd)
	handed = true
	return path, env, release, nil
}

// unlessHanded lets go of a claim unless the function that took it handed
// it on, and is deferred straight after the claim is taken. A launch's claim
// is handed to whatever runs the CLI; one abandoned on the way there — by an
// error, or by a panic — is let go of here, the panic going on as it was. A
// server recovers a handler's panic and goes on serving, and an account it
// left claimed would look busy until it restarted, or, with a claim that is
// exclusive, could not be run at all.
func unlessHanded(handed *bool, release func()) {
	if !*handed {
		release()
	}
}

// refreshForLaunch rotates the ordinary token, unless the launch is not going
// to use it.
//
// An account holding a long-lived token launches with that one, and the
// refresh lineage has nothing to do with the run: rotating it first would be
// work the run does not need, and a provider refusing it — a spent refresh
// token, a network that is down — would stop a run that had a perfectly good
// credential in hand. Usage still wants a fresh token, and still refreshes
// for itself: `rota list` and the server's background sweep are untouched.
func (s *Store) refreshForLaunch(ctx context.Context, a *rota.Account) (bool, error) {
	// A dead account is never refreshed: its refresh token was refused, and
	// presenting it again may cost the provider's patience with the login.
	if a.LongValid() || a.Dead {
		return false, nil
	}
	return s.refreshLogged(ctx, "launch", a)
}

// mayLaunch decides whether a run may start on this account, and says out
// loud what a person should know about it if it may.
//
// A dead lineage is refused as it always was, with one exception: an account
// holding a long-lived token can still run, because that token is not part
// of the lineage that died. The rotation never reaches here with a dead
// account — it skips them — so this only ever lets through one somebody
// named by id, which is the whole rule: you may run the account you asked
// for, and you are told what it can no longer tell you.
func (s *Store) mayLaunch(a *rota.Account) error {
	if !a.Dead {
		return nil
	}
	if !a.LongValid() {
		return rota.WrapReauth(a)
	}
	if s.Warn != nil {
		s.Warn(fmt.Sprintf("%s: its login is dead (%s), so usage is unknown and the rotation skips it; "+
			"running on its long-lived token, good until %s", a, a.WhyDead(), a.LongUntil().Format(time.DateOnly)))
	}
	return nil
}

// Run starts an account's CLI and waits for it.
//
// What the run changes is not written back here. A CLI that owns its own
// credential file rotates the token inside its home, and that is read back by
// the next run's adoption rather than saved by this one — see rota.Adopt.
// What this call does persist is what it changed itself, before the CLI
// started: a token it refreshed, and whatever staging adopted.
//
// One thing is written after the CLI has ended: on an account on an API key,
// what the run said about its credit (CreditPause), and only when it said
// something. That write takes the store's lock again, on the backend this
// store was opened on, so Run then waits for whoever holds it: a caller must
// not hold another store open on the same backend while it waits for Run.
//
// Cancelling ctx stops the CLI; lim caps what the spec may ask for, and may
// be nil.
//
// An account whose CLI owns that file may run only once at a time, and a
// second run is refused with rota.ErrBusy rather than made to wait. Two runs
// would be two processes each believing the home is theirs: the second
// staging overwrites the token the first has already rotated to, and a spent
// refresh token is refused for good by these providers. Refusing costs a
// caller one retry; not refusing costs the account.
func (s *Store) Run(ctx context.Context, a *rota.Account, spec rota.Spec, lim *rota.Limits, events io.Writer) (*rota.Result, error) {
	cmd, release, err := s.ready(ctx, a, !spec.Hermetic)
	if err != nil {
		return nil, err
	}
	defer release()
	s.sayRunSettings(a, cmd, spec)
	res, err := rota.Run(ctx, a, s.Home(a), cmd, spec, lim, events)
	if until, ok := s.settleCredit(a, res); ok {
		a.NoCreditUntil = until
	}
	return res, err
}

// Start is Run for a run that stays open: the CLI keeps its standard input
// and takes more messages until the session is closed. The spec must ask for
// both Input and Stream, as rota.Start requires, and only Claude Code has a
// streaming input to ask for.
//
// The claim on the account is held until the session ends rather than until
// this call returns, because the CLI is still running when it returns — so
// the goroutine that waits for the session is what lets the account go. A
// caller that never closes the session therefore keeps the account claimed,
// which is exactly what it is: a run in flight.
//
// That goroutine also writes what the session's end said about an API key's
// credit, as Run does, which is why a program about to exit calls Wait first:
// an exit would cut it short.
func (s *Store) Start(ctx context.Context, a *rota.Account, spec rota.Spec, lim *rota.Limits, events io.Writer) (*rota.Session, error) {
	cmd, release, err := s.ready(ctx, a, !spec.Hermetic)
	if err != nil {
		return nil, err
	}
	handed := false
	defer unlessHanded(&handed, release) // nothing started, so nothing is holding the home
	s.sayRunSettings(a, cmd, spec)
	sess, err := rota.Start(ctx, a, s.Home(a), cmd, spec, lim, events)
	if err != nil {
		return nil, err
	}
	handed = true
	s.ended.Add(1)
	go func() {
		defer s.ended.Done()
		// The account in hand is the caller's while the session runs, so
		// what the session's end says about its credit goes to the store
		// alone; the next run reads it from there.
		res, _ := sess.Wait()
		_, _ = s.settleCredit(a, res)
		release()
	}()
	return sess, nil
}

// Wait returns once every session Start began on this store has ended and
// what its end owes is done: the account let go, and what it said about an
// API key's credit written down. A program that is about to exit calls it
// after its sessions, because a goroutine does that work and an exit would
// cut it short; one that goes on running need not.
func (s *Store) Wait() { s.ended.Wait() }

// CreditPause is how long an account on an API key counts as out of credit
// once a run on it ended with the API refusing it for want of credit (see
// rota.Result.OutOfCredit): the rotation passes it by until then. An hour,
// because the credit is back only when somebody buys more, which nothing
// here can see, and a key tried again too soon fails a run that another
// account would have answered. A run on it by id that is answered, or the
// key pasted again, ends it sooner. A variable, like lib's ExpiryBuffer: how
// long a deployment waits is its own call.
var CreditPause = time.Hour

// settleCredit writes down what a run that has ended says about its key's
// credit: out of it, for CreditPause, or back, when an account thought to be
// out of it was answered. It returns the value written and whether one was.
//
// Only a key is judged, and only once its run is over, never in the middle of
// one. The store's lock went when the run began, so this takes it again for
// the one write, on a store of its own, and changes nothing else; a run that
// has nothing to say about credit — every run on a login, and almost every
// run on a key — takes no lock and writes nothing.
func (s *Store) settleCredit(a *rota.Account, res *rota.Result) (int64, bool) {
	if !a.APIKey() || res == nil {
		return 0, false
	}
	var until int64
	switch {
	case res.OutOfCredit():
		until = rota.Now().Add(CreditPause).UnixMilli()
	case a.NoCreditUntil != 0 && answered(res):
		until = 0
	default:
		return 0, false
	}
	fresh, err := NewStore(s.backend)
	if err != nil {
		s.say(fmt.Sprintf("%s: could not record what its run said about its credit: %v", a, err))
		return 0, false
	}
	defer fresh.Close()
	// The same account, not merely the same id: ids are never reused, but a
	// store an application keeps may be anything.
	b := fresh.Find(a.ID)
	if b == nil || b.UUID != a.UUID || !b.APIKey() {
		return 0, false
	}
	// An answer clears only the verdict this run started under, never a newer
	// one another process wrote while it ran: that is a later refusal, and
	// the key is out of credit again.
	if until == 0 && b.NoCreditUntil > a.NoCreditUntil {
		return 0, false
	}
	b.NoCreditUntil = until
	if err := fresh.Save(); err != nil {
		s.say(fmt.Sprintf("%s: could not record what its run said about its credit: %v", a, err))
		return 0, false
	}
	return until, true
}

// answered reports whether a run reached the API and was answered, which is
// what says a key has credit again: the run succeeded, and the API charged for
// it or wrote something. A run that never left the machine — a command Claude
// Code answers by itself — says nothing about credit.
func answered(res *rota.Result) bool {
	if res.IsError || res.ExitCode != 0 {
		return false
	}
	if res.CostUSD > 0 {
		return true
	}
	var usage struct {
		Output int64 `json:"output_tokens"`
	}
	return len(res.Usage) > 0 && rota.UnmarshalLenient(res.Usage, &usage) == nil && usage.Output > 0
}

// ready does everything that has to happen before an account's CLI starts,
// and hands back the command to launch and the claim on the account.
//
// Run and Start share it whole: the order of these steps is what keeps a
// credential-owning account alive, and two copies of it would be two chances
// to get that order wrong. The caller decides only when the claim is released
// — at the end of the call for a run, at the end of the session for a
// session.
//
// mirror says whether the account's own Claude Code world is worth building.
// A hermetic run is given a throwaway configuration directory instead, so
// building one for it would be work nobody reads.
func (s *Store) ready(ctx context.Context, a *rota.Account, mirror bool) (*rota.Command, func(), error) {
	cmd, release, err := s.prepare(ctx, a, mirror)
	if err != nil {
		return nil, nil, err
	}
	handed := false
	defer unlessHanded(&handed, release)
	// Everything a run needs from the store is on disk by now. The lock goes,
	// because the run that follows lasts as long as the agent does and
	// nothing else may be made to wait for it.
	_ = s.Release() // releasing a lock cannot fail in a way a caller can act on
	cmd.BaseEnv = HostEnv()
	handed = true
	return cmd, release, nil
}

// prepare claims the account, brings its credential up to date, stages it,
// saves the store, and returns the command with the claim still held. A
// handover and a run are the same launch, differing in who waits for it.
func (s *Store) prepare(ctx context.Context, a *rota.Account, mirror bool) (*rota.Command, func(), error) {
	// One home, one account: a home another account also has holds a
	// credential this one would read as its own, and write over.
	if err := s.oneHomeOneAccount(a); err != nil {
		return nil, nil, err
	}
	launch := s.prepareAlone
	if rota.SharedHome(a.Provider) {
		launch = s.prepareShared
	}
	cmd, release, err := launch(ctx, a, mirror)
	if err != nil {
		return nil, nil, err
	}
	handed := false
	defer unlessHanded(&handed, release)
	// Everything rota launches, whatever its CLI, is told whose Claude Code
	// directory is the person's, so a rota started anywhere inside it — in a
	// shell a codex session opened, say — knows without guessing.
	cmd.Env = append(cmd.Env, claudeHomeVar+"="+s.handDown())
	handed = true
	return cmd, release, nil
}

// prepareAlone is prepare for an account whose CLI keeps its credential file
// to one process at a time.
func (s *Store) prepareAlone(ctx context.Context, a *rota.Account, mirror bool) (*rota.Command, func(), error) {
	if err := s.mayLaunch(a); err != nil {
		return nil, nil, err
	}
	// Taken before adoption, because adoption reads the very file another
	// run would be rewriting. Not waited for: the store lock is still held
	// here, and blocking on it would stop every other command until an agent
	// finished.
	release, ok := s.holdRun(a)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s keeps its own credential file, and two runs would spend the same refresh token", rota.ErrBusy, a)
	}
	handed := false
	defer unlessHanded(&handed, release)
	// Adopt before refreshing: see rota.Adopt. Doing it the other way round
	// is how a codex or kimi account is permanently killed.
	if aerr := rota.Adopt(a, s.Home(a)); aerr != nil {
		return nil, nil, aerr
	}
	// A daemon Codex left running in the home refreshes this login itself,
	// and the CLI refreshes a token that is due when it starts; rota
	// refreshing it too would spend the daemon's copy (see codexDaemon).
	var (
		changed bool
		err     error
	)
	if !s.heldByDaemon(a) {
		// Before a codex login that is due is refreshed, the home holds it
		// and the store says so on disk, as Maintain leaves them, so a token
		// refreshed after that and saved nowhere but the home is read back by
		// the next run's adoption. A store that cannot be saved spends no
		// token, and runs nothing. A login held as a refresh token alone is
		// made to be held there as holdDue says.
		if codexDue(a) && !a.LongValid() {
			err = s.holdDue(a)
			if serr := s.Save(); serr != nil {
				return nil, nil, errors.Join(err, fmt.Errorf("refusing to run: not refreshed, the store could not be saved: %w", serr))
			}
			if err != nil {
				return nil, nil, err
			}
		}
		changed, err = s.refreshForLaunch(ctx, a)
	}
	if changed {
		if serr := s.Save(); serr != nil {
			// The token the refresh replaced is spent. The new one goes into
			// the home, where the next run reads it back, rather than nowhere.
			if rota.Flavor(a.Provider) == "codex" && !a.Dead {
				if _, werr := rota.Stage(a, s.Home(a)); werr != nil {
					serr = errors.Join(serr, werr)
				}
			}
			return nil, nil, errors.Join(err, fmt.Errorf("refusing to run: store not saved after a token change: %w", serr))
		}
	}
	if err != nil {
		return nil, nil, err
	}
	// Staging is the last thing that needs the store: it may adopt a token
	// the CLI rotated, and that must be on disk before anything runs.
	cmd, err := s.command(a, mirror)
	if serr := s.Save(); serr != nil {
		return nil, nil, errors.Join(err, fmt.Errorf("refusing to run: store not saved after staging: %w", serr))
	}
	if err != nil {
		return nil, nil, err
	}
	handed = true
	return cmd, release, nil
}

// prepareShared is prepare for an account whose CLI runs many processes in
// one home on one stored login — Claude Code. Its claim is shared, so runs
// overlap as they are meant to; what decides whether rota may write or
// refresh is whether anything at all is alive in the home, and part of that
// answer — another rota run holding the account — has to be asked before
// this run takes its own claim. See claude.go for the rest.
func (s *Store) prepareShared(ctx context.Context, a *rota.Account, mirror bool) (*rota.Command, func(), error) {
	h := s.claudeHome(a, s.claimed(a))
	release, ok := s.holdRun(a)
	if !ok {
		return nil, nil, fmt.Errorf("%w: another rota process is changing %s's login right now; try again in a moment", rota.ErrBusy, a)
	}
	handed := false
	defer unlessHanded(&handed, release)
	cmd, err := h.launch(ctx, mirror)
	// Whatever happened, the account may have changed — a rotation adopted,
	// a refresh, a login found dead — and none of it may be lost.
	if serr := s.Save(); serr != nil {
		return nil, nil, errors.Join(err, fmt.Errorf("refusing to run: store not saved: %w", serr))
	}
	if err != nil {
		return nil, nil, err
	}
	handed = true
	return cmd, release, nil
}

// command stages an account's credentials and returns the command that
// starts its CLI, with the account's own Claude Code world added to the
// environment when there is one to add. A claude account goes the whole way
// a launch goes (claudeHome.launch), judging what is alive in its home now.
func (s *Store) command(a *rota.Account, mirror bool) (*rota.Command, error) {
	if rota.SharedHome(a.Provider) {
		return s.claudeHome(a, s.claimed(a)).launch(context.Background(), mirror)
	}
	return rota.Stage(a, s.Home(a))
}

// mirrored adds the account's own Claude Code world to a command.
//
// A mirror that cannot be built is not a reason to refuse the run. The run
// still works — it is billed correctly, it just lacks the person's settings
// or shares their daemon — so the application is told, if it left somewhere
// to tell, and the run goes ahead.
func (s *Store) mirrored(a *rota.Account, cmd *rota.Command, quiet bool) (*rota.Command, error) {
	dir, merr := s.mirrorClaude(a, quiet)
	pointed := false
	for _, e := range cmd.Env {
		if k, _, _ := strings.Cut(e, "="); k == "CLAUDE_CONFIG_DIR" {
			pointed = true
		}
	}
	switch {
	case merr != nil && pointed:
		s.say(fmt.Sprintf("could not mirror Claude Code's configuration into %s (%v); "+
			"running there without your settings, memory and skills", dir, merr))
	case merr != nil:
		s.say(fmt.Sprintf("could not mirror Claude Code's configuration into %s (%v); "+
			"running with Claude Code's own directory and daemon", dir, merr))
		// Claude Code's own directory is the person's — not whatever a rota
		// that launched this one handed down, an account's home or a
		// hermetic run's throwaway directory.
		if own := os.Getenv("CLAUDE_CONFIG_DIR"); own != "" {
			if dir, _ := s.personalDir(); !sameDir(own, dir) {
				cmd.Drop = append(cmd.Drop, "CLAUDE_CONFIG_DIR")
			}
		}
	case dir != "" && !pointed:
		// Appended rather than replacing: Environ drops every inherited
		// value a command sets, so the child sees this one and only this one.
		// A command lib already pointed somewhere — the home its login is
		// kept in, or the directory the account named — is not pointed twice.
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+dir)
	}
	return cmd, nil
}

// RunDir is where a run that stays open puts the socket another terminal
// sends into: beside the accounts, not inside an account's private home,
// because an open run belongs to rota rather than to the CLI it launched.
func (s *Store) RunDir() (string, error) {
	dir := filepath.Join(filepath.Dir(s.homeRoot), "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// TerminalDir is where a recorded terminal's output is kept, beside the runs
// and for the same reason: what a terminal printed belongs to rota rather
// than to the CLI that printed it, and no CLI should be able to read it.
//
// It is made readable by its owner alone. A recording is a transcript of
// somebody's working session — file names, error messages, whatever scrolled
// past — and the directory it sits in is the last place to be relaxed about.
func (s *Store) TerminalDir() (string, error) {
	dir := filepath.Join(filepath.Dir(s.homeRoot), "terminals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// runLock is held for as long as an account's CLI is running, inside the home
// that CLI owns, so the answer survives across rota processes as well as
// within one.
const runLock = ".rota-run.lock"

// keepingAcrossExec is keepAcrossExec, as a variable so a test can see that
// the handover arranges for the claims to survive it rather than take the
// comment's word for it.
var keepingAcrossExec = keepAcrossExec

// tryExclusive and tryShared are the two ways a claim is tried, as variables
// so a test can see that a store which must try none tries none: a moment's
// exclusive try is enough to refuse a launch's shared claim.
var (
	tryExclusive = tryLockFile
	tryShared    = tryLockShared
)

// heldClaims are the claim files this process holds right now, so that the
// one moment they must outlive it — the handover — can find them.
var (
	heldMu     sync.Mutex
	heldClaims = map[*os.File]bool{}
)

// KeepClaimsAcrossExec arranges for every claim this process holds to
// survive the exec it is about to make, and is called by nothing but a
// handover that does not lend its claims (lentClaims), immediately before it
// replaces this process with the vendor CLI.
//
// Go opens every file close-on-exec, which is right for every other moment:
// a child started while a claim is held — another account's CLI, a shell on
// the server's terminal page — must not inherit the claim and keep the
// account looking busy long after its run ended. Here the CLI is the run,
// and the claim has to be held by whatever is running rather than by the
// process image that took it. The kernel releases it when the last process
// holding it exits — the CLI, and anything the CLI started that inherited
// it, a daemon it leaves behind included; that is why a codex handover lends
// its claims instead, Codex's CLI leaving its app-server daemon running.
func KeepClaimsAcrossExec() {
	heldMu.Lock()
	defer heldMu.Unlock()
	for f := range heldClaims {
		_ = keepingAcrossExec(f)
	}
}

// lentClaims are copies of every claim this process holds, for a handover
// that lends them to a process of its own rather than keeping them across
// its exec (see internal/lend, which is how the program reaches it without
// its being any part of this package's API): each a close-on-exec duplicate
// of the claim's descriptor, the caller's to close. A flock belongs to the
// open file, so a duplicate holds the same claim, and closing it lets go of
// nothing while this process's own is open.
func lentClaims() ([]*os.File, error) {
	heldMu.Lock()
	defer heldMu.Unlock()
	files := make([]*os.File, 0, len(heldClaims))
	for f := range heldClaims {
		d, err := dupClaim(f)
		if err != nil {
			for _, c := range files {
				_ = c.Close()
			}
			return nil, err
		}
		if d != nil {
			files = append(files, d)
		}
	}
	return files, nil
}

func init() { lend.Register(lentClaims) }

// claimFile claims an account whose CLI owns its credential file: shared or
// exclusively. The claim stays close-on-exec, so no child of this process
// holds it; a handover lends it (lentClaims), or keeps it across its exec
// (KeepClaimsAcrossExec), as its own exception.
//
// ok is false when someone else holds it in a way that excludes this claim.
// That is an answer rather than a failure — the caller wants to know whether
// it may proceed, not to queue behind an agent — and release is always safe
// to call.
//
// An account whose credential rota holds is never claimed: its token reaches
// the CLI in its environment, so two runs share nothing and holding them apart
// would cost the rotation its whole point.
func (s *Store) claimFile(a *rota.Account, shared bool) (release func(), ok bool) {
	if !rota.OwnsCredentials(a.Provider) {
		return func() {}, true
	}
	home := s.Home(a)
	if err := os.MkdirAll(home, 0o700); err != nil {
		// Nowhere to put the lock is nowhere to stage a credential either,
		// so let the caller fail on the real thing rather than on this.
		return func() {}, true
	}
	try := tryExclusive
	if shared {
		try = tryShared
	}
	held, got, err := try(filepath.Join(home, runLock))
	if err != nil || !got {
		return func() {}, false
	}
	heldMu.Lock()
	heldClaims[held] = true
	heldMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			heldMu.Lock()
			delete(heldClaims, held)
			heldMu.Unlock()
			_ = held.Close()
		})
	}, true
}

// holdRun is the claim a run takes, held by this process until release —
// and past it, by whatever a handover lent it to or kept it for.
//
// It is exclusive for a CLI that keeps its credential file to one process at
// a time, and shared for one whose home is shared (rota.SharedHome): many
// windows and runs on one Claude Code account at once are the whole point,
// and the rotation must never pass such an account over because it is busy.
func (s *Store) holdRun(a *rota.Account) (release func(), ok bool) {
	return s.claimFile(a, rota.SharedHome(a.Provider))
}

// holdIdle is the claim that means nothing else is using the account: taken
// exclusively, and for a shared home only while nothing is alive there
// either — not a rota run, not a window somebody opened in that home
// themselves, not the daemon. It is what a refresh, maintenance and a login
// written into the home ask for, and it is held for as long as this process
// runs.
func (s *Store) holdIdle(a *rota.Account) (func(), bool) {
	release, ok := s.claimFile(a, false)
	if !ok || !rota.SharedHome(a.Provider) {
		return release, ok
	}
	handed := false
	defer unlessHanded(&handed, release)
	if len(claudeLive(s.Home(a))) > 0 {
		return func() {}, false
	}
	handed = true
	return release, true
}

// whileIdle does do with the account's idle claim held, and lets go of the
// claim however do ends, a panic included. It reports false, having done
// nothing, when the account is in use.
func (s *Store) whileIdle(a *rota.Account, do func()) bool {
	release, idle := s.holdIdle(a)
	if !idle {
		return false
	}
	defer release()
	do()
	return true
}

// Hold claims an account while something outside this package writes into its
// private home — the vendor CLI's own login, most of all, which replaces the
// credential file wholesale.
//
// It is the idle claim — nothing else may be using the account while it is
// held, runs included — exported because signing an account in is the one
// write rota hands to another program entirely. ok is false when anything
// else is using it, and release is always safe to call.
func (s *Store) Hold(a *rota.Account) (release func(), ok bool) { return s.holdIdle(a) }

// Busy reports whether a run has this account in a way that would refuse
// another, so a caller can choose another one instead of being refused. An
// account whose home is shared is never busy in that sense: its runs share
// it. It is a glance rather than a promise: the answer can be out of date the
// moment it is given, and Run is what actually decides.
func (s *Store) Busy(a *rota.Account) bool {
	release, ok := s.holdRun(a)
	release()
	return !ok
}

// Removable says why an account cannot be removed right now, or nil, and
// changes nothing: a command removing several accounts asks it of every one
// of them before any is touched. Deleting the directory a running agent
// works from is worse than making somebody wait, and nothing is undoable
// once the files are gone.
//
// A claude account in a home rota made is refused while a window is open
// there — an interactive session, or a record nobody can read — which no
// removal may close; and while a run holds its claim with no daemon alive
// that could be what holds it. Its daemon and the background sessions the
// daemon hosts are no reason to refuse: Remove stops them. A claude account
// in a directory the person chose is never refused: rota keeps no login
// there and removes nothing, so nothing running there is harmed.
func (s *Store) Removable(a *rota.Account) error {
	if rota.SharedHome(a.Provider) {
		if !s.owns(a) {
			return nil
		}
		home := s.Home(a)
		var open []string
		for _, t := range claudeLiveThings(home) {
			if !t.hosted {
				open = append(open, t.what)
			}
		}
		if len(open) > 0 {
			return fmt.Errorf("%w: Claude Code is running in %s's home (%s); close it before removing the account",
				rota.ErrBusy, a, strings.Join(open, ", "))
		}
		if _, daemon := daemonAlive(home); daemon {
			return nil // what holds the claim may be the daemon, which Remove stops
		}
	}
	if s.claimed(a) {
		return errRunning(a)
	}
	return nil
}

func errRunning(a *rota.Account) error {
	return fmt.Errorf("%w: %s is running; stop it before removing the account", rota.ErrBusy, a)
}
