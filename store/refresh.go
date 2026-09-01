package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	rota "github.com/professor93/rota/lib"
)

// Refresh brings quota readings up to date, in parallel, for the given
// accounts (all when none are given). Only providers that publish a quota
// endpoint are touched — refreshing a token nobody is about to use would
// just rotate it for nothing. Readings younger than a minute are kept
// unless force is set. Failures are collected, never fatal, and anything
// that changed is saved.
func (s *Store) Refresh(ctx context.Context, force bool, accounts ...*rota.Account) []error {
	if len(accounts) == 0 {
		accounts = s.Accounts
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		errs  []error
		dirty bool
	)
	report := func(changed bool, err error) {
		mu.Lock()
		defer mu.Unlock()
		dirty = dirty || changed
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, a := range accounts {
		if a.Dead || !rota.Metered(a.Provider) {
			continue
		}
		stale := force || a.QuotaAt == 0 || time.Since(time.UnixMilli(a.QuotaAt)) > QuotaTTL
		if !stale {
			continue
		}
		// A running account is left alone: refreshing rotates the token its
		// CLI is still holding, and a reading is not worth that.
		release, idle := s.holdIdle(a)
		if !idle {
			continue
		}
		wg.Add(1)
		go func(a *rota.Account) {
			defer wg.Done()
			defer release()
			// A provider panicking here would otherwise kill the process,
			// and with it every run in flight.
			defer func() {
				if v := recover(); v != nil {
					report(false, fmt.Errorf("%s: panic while refreshing: %v", a, v))
				}
			}()
			changed, err := rota.Refresh(ctx, a)
			if err != nil {
				report(changed, err)
				return
			}
			q, err := rota.Usage(ctx, a)
			if err != nil {
				report(changed, fmt.Errorf("%s: quota: %w", a, err))
				return
			}
			a.Quota, a.QuotaAt = q, rota.NowMS()
			report(true, nil)
		}(a)
	}
	wg.Wait()
	if dirty {
		if err := s.Save(); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// QuotaTTL bounds how stale a cached quota reading may be. Usage endpoints
// are rate-limited per account and answer 429 when every invocation hits
// them, so a reading is reused until it is this old. It moved here from the
// SDK: lib itself never caches a reading — this store does, so the caching
// policy is this package's to own.
const QuotaTTL = 5 * time.Minute
