package store

import (
	"context"
	"fmt"

	rota "github.com/professor93/rota/lib"
)

// Maintain brings the whole store up to date: it rotates access tokens that
// are close to expiring and reads usage for the accounts whose reading has
// aged past the cache.
//
// It exists for a server. A command line does this work when it is asked to
// — a run refreshes the token it is about to use, a listing reads the limits
// it is about to print — but a long-lived process is asked at unpredictable
// moments, and the two things a request should never have to wait to
// discover are that its credential expired and that its quota reading is an
// hour old. The rotation in particular decides from stored numbers: stale
// ones send a request to an account that is already spent.
//
// Nothing here is fatal. A provider that cannot be reached leaves its
// account exactly as it was, which is also what makes this safe to run on a
// timer: the worst outcome is the state a store would have had anyway.
//
// The lock is held throughout, so a run starting at the same moment waits
// for it. That is the same trade `Refresh` already makes for a listing, and
// it is the right way round: a token half-rotated by two writers is not
// recoverable, while a wait of a second is.
func (s *Store) Maintain(ctx context.Context) []error {
	// Without the lock nothing may rotate, so all that is left is reading:
	// see Refresh.
	if s.released {
		return s.Refresh(ctx, false)
	}
	var errs []error
	changed := false
	for _, a := range s.Accounts {
		if rota.SharedHome(a.Provider) {
			did, err := s.maintainShared(ctx, a)
			changed = changed || did
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if a.Dead {
			continue // only a fresh login helps; asking again just spends requests
		}
		// Not while its CLI has it. Adopting reads the file that CLI is
		// rewriting, and refreshing rotates the token underneath it: the
		// provider invalidates the copy the CLI still holds, and the next
		// thing it does with that copy is refused for good. Maintenance can
		// wait two minutes; a killed lineage cannot be undone.
		//
		// Adopt before refreshing. The vendor CLI may have rotated the
		// refresh token during the last run, and presenting rota's older
		// copy is how a codex or kimi lineage is permanently killed.
		//
		// What adoption read back is saved, not only what a refresh changed:
		// while a daemon Codex left in the home refreshes the login (see
		// codexDaemon), adoption is how the store keeps up with it. What rota
		// refreshes goes into a codex home at once, as a launch's refresh
		// does, so a daemon started there again reads the token that is still
		// good rather than the one just spent; should the save below fail, the
		// home's copy is read back by the next adoption.
		//
		// Adopting, asking after the daemon, refreshing and that write happen
		// under one claim, so nothing the CLI does between them goes unseen.
		var (
			err error
			did bool
		)
		before := snapshot(a)
		if !s.whileIdle(a, func() {
			home := s.Home(a)
			if err = rota.Adopt(a, home); err != nil || s.heldByDaemon(a) {
				return
			}
			if did, err = rota.Refresh(ctx, a); err != nil || !did || a.Dead {
				return
			}
			if rota.Flavor(a.Provider) == "codex" {
				_, err = rota.Stage(a, home)
			}
		}) {
			continue
		}
		changed = changed || did || before.differs(a)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
		}
	}
	if changed {
		if err := s.Save(); err != nil {
			errs = append(errs, fmt.Errorf("tokens could not be saved: %w", err))
		}
	}
	// Usage honours the five-minute cache and saves whatever it changed.
	return append(errs, s.Refresh(ctx, false)...)
}

// maintainShared is Maintain for a claude account. Its home is read first,
// whatever state the account is in — a dead one may have been signed in
// again inside Claude Code, which is how rota learns of it. A login kept in
// the home is refreshed only while nothing is alive there and it is not in the
// hold, and goes back into the home at once; one rota alone holds is refreshed
// as it always was. A home owed something although something is alive there is
// left for the Refresh Maintain ends with, whose first step that is.
func (s *Store) maintainShared(ctx context.Context, a *rota.Account) (bool, error) {
	if !s.loginInHome(a) {
		if a.Dead {
			return false, nil
		}
		did, err := rota.Refresh(ctx, a)
		return did, wrapAccount(a, err)
	}
	before := snapshot(a)
	h := s.claudeHome(a, s.claimed(a))
	if err := h.adopt(ctx); err != nil {
		return false, err
	}
	changed := before.differs(a)
	if a.Dead || h.hold != "" {
		return changed, nil
	}
	if !rota.StoresLogin(a, h.home) {
		did, err := rota.Refresh(ctx, a)
		return changed || did, wrapAccount(a, err)
	}
	release, idle := s.holdIdle(a)
	if !idle {
		return changed, nil
	}
	defer release()
	h.others = false
	did, err := h.renew(ctx)
	return changed || did, wrapAccount(a, err)
}

func wrapAccount(a *rota.Account, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", a, err)
}
