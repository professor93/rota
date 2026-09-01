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
	var errs []error
	changed := false
	for _, a := range s.Accounts {
		if a.Dead {
			continue // only a fresh login helps; asking again just spends requests
		}
		// Not while its CLI has it. Adopting reads the file that CLI is
		// rewriting, and refreshing rotates the token underneath it: the
		// provider invalidates the copy the CLI still holds, and the next
		// thing it does with that copy is refused for good. Maintenance can
		// wait two minutes; a killed lineage cannot be undone.
		release, idle := s.holdIdle(a)
		if !idle {
			continue
		}
		// Adopt before refreshing. The vendor CLI may have rotated the
		// refresh token during the last run, and presenting rota's older
		// copy is how a codex or kimi lineage is permanently killed.
		err := rota.Adopt(a, s.Home(a))
		release()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
			continue
		}
		release, idle = s.holdIdle(a)
		if !idle {
			continue
		}
		did, err := rota.Refresh(ctx, a)
		release()
		changed = changed || did
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
		}
	}
	if changed {
		if err := s.Save(); err != nil {
			errs = append(errs, fmt.Errorf("refreshed tokens could not be saved: %w", err))
		}
	}
	// Usage honours the five-minute cache and saves whatever it changed.
	return append(errs, s.Refresh(ctx, false)...)
}
