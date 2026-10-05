package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
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
//
// A claude account whose login lives in its home is read first, every time:
// Claude Code may have rotated the login there, or somebody may have signed
// a dead account in again inside it. Its usage is then read with the access
// token it has, while Claude Code runs or not — reading usage spends
// nothing. Only an expired token needs a refresh, and that waits for the
// home to be quiet; until then the account keeps its last reading.
func (s *Store) Refresh(ctx context.Context, force bool, accounts ...*rota.Account) []error {
	if len(accounts) == 0 {
		accounts = s.Accounts
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		errs    []error
		dirty   bool
		said    []string
		renewed []*claudeHome
		held    []func()
	)
	report := func(changed bool, err error) {
		mu.Lock()
		defer mu.Unlock()
		dirty = dirty || changed
		if err != nil {
			errs = append(errs, err)
		}
	}
	// Remarks made beside the work, collected so the store's Warn is only
	// ever called from here, one at a time.
	collect := func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		said = append(said, msg)
	}
	// A provider panicking here would otherwise kill the process, and with
	// it every run in flight.
	guard := func(a *rota.Account) {
		if v := recover(); v != nil {
			report(false, fmt.Errorf("%s: panic while refreshing: %v", a, v))
		}
	}
	for _, a := range accounts {
		shared := rota.SharedHome(a.Provider)
		if shared {
			before := snapshot(a)
			if err := s.claudeHome(a, true).adopt(ctx); err != nil {
				errs = append(errs, err)
				continue
			}
			dirty = dirty || before.differs(a)
		}
		if a.Dead || !rota.Metered(a.Provider) {
			continue
		}
		stale := force || a.QuotaAt == 0 || time.Since(time.UnixMilli(a.QuotaAt)) > QuotaTTL
		if !stale {
			continue
		}
		if shared && s.keepsLogin(a) {
			if !a.Expired() {
				h := s.claudeHome(a, true)
				h.warn = collect
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer guard(a)
					q, err := h.usage(ctx)
					if err != nil {
						report(true, fmt.Errorf("%s: quota: %w", a, err))
						return
					}
					a.Quota, a.QuotaAt = q, rota.NowMS()
					report(true, nil)
				}()
				continue
			}
			// Expired: a refresh, which only a quiet home may have. The claim
			// is kept until the new login is in the home.
			release, idle := s.holdIdle(a)
			if !idle {
				continue
			}
			held = append(held, release)
			h := s.claudeHome(a, false)
			h.warn = collect
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer guard(a)
				changed, err := rota.Refresh(ctx, a)
				if err != nil && a.Dead {
					// Not believed until the home is read again: a sibling may
					// have refreshed first.
					if aerr := h.adopt(ctx); aerr == nil && !a.Dead {
						changed, err = true, nil
					}
				}
				if err != nil {
					report(changed, err)
					return
				}
				mu.Lock()
				renewed = append(renewed, h)
				mu.Unlock()
				q, err := rota.Usage(ctx, a)
				if err != nil {
					report(true, fmt.Errorf("%s: quota: %w", a, err))
					return
				}
				a.Quota, a.QuotaAt = q, rota.NowMS()
				report(true, nil)
			}()
			continue
		}
		// A running account is left alone: refreshing rotates the token its
		// CLI is still holding, and a reading is not worth that. A claude
		// account whose token rota alone holds — Windows, a dead login run on
		// a long token — has nothing in its home to protect, as ever.
		release, idle := func() {}, true
		if !shared {
			release, idle = s.holdIdle(a)
		}
		if !idle {
			continue
		}
		wg.Add(1)
		go func(a *rota.Account) {
			defer wg.Done()
			defer release()
			defer guard(a)
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
	defer func() {
		for _, release := range held {
			release()
		}
	}()
	for _, msg := range said {
		s.say(msg)
	}
	if !dirty {
		return errs
	}
	if err := s.Save(); err != nil {
		// The refreshed logins are not on disk, so none goes into a home.
		return append(errs, err)
	}
	// Each refreshed login goes into its home now that the store has it,
	// while the home is still claimed — unless something started there
	// meanwhile, in which case the next quiet launch writes it.
	again := false
	for _, h := range renewed {
		if !h.quiet() {
			continue
		}
		_, files, err := rota.StagePlan(ctx, h.a, h.home)
		if err == nil {
			err = h.seed(files)
		}
		if err != nil {
			errs = append(errs, err)
		}
		again = true
	}
	if again {
		if err := s.Save(); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// usage reads the account's usage with the access token it has. A 401 is
// read as a sign that Claude Code rotated the login since the home was last
// read: it is read again, and the reading tried once more with what is
// there.
func (h *claudeHome) usage(ctx context.Context) (*rota.Quota, error) {
	q, err := rota.Usage(ctx, h.a)
	var he *rota.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusUnauthorized {
		return q, err
	}
	before := h.a.Token.Access
	if aerr := h.adopt(ctx); aerr != nil || h.a.Token.Access == before || h.a.Expired() {
		return q, err
	}
	return rota.Usage(ctx, h.a)
}

// accountState is what adoption may change about an account, so a caller
// can tell whether there is anything to save.
type accountState struct {
	tok    rota.Token
	staged string
	dead   bool
	reason string
	extra  map[string]string
}

func snapshot(a *rota.Account) accountState {
	return accountState{tok: a.Token, staged: a.Staged, dead: a.Dead, reason: a.DeadReason, extra: maps.Clone(a.Extra)}
}

func (b accountState) differs(a *rota.Account) bool {
	t := a.Token
	return t.Access != b.tok.Access || t.Refresh != b.tok.Refresh || t.ExpiresAt != b.tok.ExpiresAt ||
		!slices.Equal(t.Scopes, b.tok.Scopes) || a.Staged != b.staged || a.Dead != b.dead ||
		a.DeadReason != b.reason || !maps.Equal(a.Extra, b.extra)
}

// QuotaTTL bounds how stale a cached quota reading may be. Usage endpoints
// are rate-limited per account and answer 429 when every invocation hits
// them, so a reading is reused until it is this old. It moved here from the
// SDK: lib itself never caches a reading — this store does, so the caching
// policy is this package's to own.
const QuotaTTL = 5 * time.Minute
