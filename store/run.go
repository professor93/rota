package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

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
// than released here because the caller is about to replace this process with
// that CLI, and the claim has to outlive the replacing — a caller that does
// not go through with the handover calls release instead.
func (s *Store) Prepare(ctx context.Context, a *rota.Account) (path string, env []string, release func(), err error) {
	if a.Dead {
		return "", nil, nil, rota.WrapReauth(a)
	}
	release, ok := s.holdForExec(a)
	if !ok {
		return "", nil, nil, fmt.Errorf("%w: %s keeps its own credential file, and two runs would spend the same refresh token", rota.ErrBusy, a)
	}
	fail := func(err error) (string, []string, func(), error) {
		release()
		return "", nil, nil, err
	}
	// Adopt first: the CLI may have rotated its refresh token during the
	// last run, and refreshing from rota's older copy would present a spent
	// one — which these providers reject for good.
	adopted := rota.Adopt(a, s.Home(a)) == nil
	changed, err := rota.Refresh(ctx, a)
	changed = changed || adopted
	var cmd *rota.Command
	if err == nil {
		// Stage may adopt a token the CLI rotated, or mark the account dead
		// on its way to an error; either way the account must be saved.
		cmd, err = rota.Stage(a, s.Home(a))
		changed = true
	}
	if changed {
		if serr := s.Save(); serr != nil {
			return fail(errors.Join(err, fmt.Errorf("refusing to run: store not saved after a token change: %w", serr)))
		}
	}
	if err != nil {
		return fail(err)
	}
	path, err = exec.LookPath(cmd.Bin)
	if err != nil {
		return fail(fmt.Errorf("%s not found in PATH: %w", cmd.Bin, err))
	}
	return path, rota.Environ(HostEnv(), cmd), release, nil
}

// Run starts an account's CLI and waits for it.
//
// What the run changes is not written back here. A CLI that owns its own
// credential file rotates the token inside its home, and that is read back by
// the next run's adoption rather than saved by this one — see rota.Adopt.
// What this call does persist is what it changed itself, before the CLI
// started: a token it refreshed, and whatever staging adopted.
//
// Cancelling ctx kills the CLI; lim caps what the spec may ask for, and may
// be nil.
//
// An account whose CLI owns that file may run only once at a time, and a
// second run is refused with rota.ErrBusy rather than made to wait. Two runs
// would be two processes each believing the home is theirs: the second
// staging overwrites the token the first has already rotated to, and a spent
// refresh token is refused for good by these providers. Refusing costs a
// caller one retry; not refusing costs the account.
func (s *Store) Run(ctx context.Context, a *rota.Account, spec rota.Spec, lim *rota.Limits, events io.Writer) (*rota.Result, error) {
	if a.Dead {
		return nil, rota.WrapReauth(a)
	}
	// Taken before adoption, because adoption reads the very file another
	// run would be rewriting. Not waited for: the store lock is still held
	// here, and blocking on it would stop every other command until an agent
	// finished.
	release, ok := s.holdForExec(a)
	if !ok {
		return nil, fmt.Errorf("%w: %s keeps its own credential file, and two runs would spend the same refresh token", rota.ErrBusy, a)
	}
	defer release()
	// Adopt before refreshing: see rota.Adopt. Doing it the other way round
	// is how a codex or kimi account is permanently killed.
	if aerr := rota.Adopt(a, s.Home(a)); aerr != nil {
		return nil, aerr
	}
	changed, err := rota.Refresh(ctx, a)
	if changed {
		if serr := s.Save(); serr != nil {
			return nil, errors.Join(err, fmt.Errorf("refusing to run: store not saved after a token change: %w", serr))
		}
	}
	if err != nil {
		return nil, err
	}
	// Staging is the last thing that needs the store: it may adopt a token
	// the CLI rotated, and that must be on disk before anything runs. Once
	// it is saved the lock is released, because the run that follows lasts
	// as long as the agent does and nothing else may be made to wait for it.
	cmd, err := rota.Stage(a, s.Home(a))
	if err != nil {
		if serr := s.Save(); serr != nil {
			return nil, errors.Join(err, fmt.Errorf("the store could not be saved: %w", serr))
		}
		return nil, err
	}
	if err := s.Save(); err != nil {
		return nil, fmt.Errorf("refusing to run: the store could not be saved after staging: %w", err)
	}
	_ = s.Release() // releasing a lock cannot fail in a way a caller can act on
	cmd.BaseEnv = HostEnv()
	return rota.Run(ctx, a, s.Home(a), cmd, spec, lim, events)
}

// runLock is held for as long as an account's CLI is running, inside the home
// that CLI owns, so the answer survives across rota processes as well as
// within one.
const runLock = ".rota-run.lock"

// keepingAcrossExec is keepAcrossExec, as a variable so a test can see that a
// claim is arranged to survive the handover rather than take the comment's
// word for it. Testing the call and testing that it is made are two things,
// and only the second one notices when it stops being made.
var keepingAcrossExec = keepAcrossExec

// holdIdle claims an account whose CLI owns its credential file, so nothing
// touches that file while the CLI has it.
//
// ok is false when someone else holds it. That is an answer rather than a
// failure — the caller wants to know whether it may proceed, not to queue
// behind an agent — and release is always safe to call.
//
// An account whose credential rota holds is never claimed: its token reaches
// the CLI in its environment, so two runs share nothing and holding them apart
// would cost the rotation its whole point.
func (s *Store) holdIdleFile(a *rota.Account) (release func(), ok bool, held *os.File) {
	if !rota.OwnsCredentials(a.Provider) {
		return func() {}, true, nil
	}
	home := s.Home(a)
	if err := os.MkdirAll(home, 0o700); err != nil {
		// Nowhere to put the lock is nowhere to stage a credential either,
		// so let the caller fail on the real thing rather than on this.
		return func() {}, true, nil
	}
	held, got, err := tryLockFile(filepath.Join(home, runLock))
	if err != nil || !got {
		return func() {}, false, nil
	}
	return func() { held.Close() }, true, held
}

// holdForExec is holdIdle for the one caller about to replace this process
// with the vendor CLI: the claim's close-on-exec flag is cleared so it
// survives the handover. Every other hold leaves the flag set — a child
// spawned while a hold is live must not inherit another account's lock and
// keep it long after this process released its own copy.
func (s *Store) holdForExec(a *rota.Account) (release func(), ok bool) {
	release, ok, held := s.holdIdleFile(a)
	if ok && held != nil {
		_ = keepingAcrossExec(held)
	}
	return release, ok
}

// holdIdle is the in-process claim: held for as long as this process runs.
func (s *Store) holdIdle(a *rota.Account) (release func(), ok bool) {
	release, ok, _ = s.holdIdleFile(a)
	return release, ok
}

// Hold claims an account while something outside this package writes into its
// private home — the vendor CLI's own login, most of all, which replaces the
// credential file wholesale.
//
// It is the same claim Run and Prepare take, exported because signing an
// account in is the one write rota hands to another program entirely. ok is
// false when somebody else holds it, and release is always safe to call.
func (s *Store) Hold(a *rota.Account) (release func(), ok bool) { return s.holdIdle(a) }

// Busy reports whether a run has this account, so a caller can choose another
// one instead of being refused. It is a glance rather than a promise: the
// answer can be out of date the moment it is given, and Run is what actually
// decides.
func (s *Store) Busy(a *rota.Account) bool {
	release, ok := s.holdForExec(a)
	release()
	return !ok
}
