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
// just rotate it for nothing. A reading is kept unless force is set or it
// is due: older than QuotaTTL, or taken before a window it reports reset
// (see due). Failures are collected, never fatal, and anything that changed
// is saved.
//
// A claude account whose login lives in its home is read first, every time:
// Claude Code may have rotated the login there, or somebody may have signed
// a dead account in again inside it. Its usage is then read with the access
// token it has, while Claude Code runs or not — reading usage spends
// nothing. Only an expired token needs a refresh, and that waits for the
// home to be quiet and out of the hold; until then the account keeps its last
// reading. What a home is owed although something is alive there comes first
// and has nothing to do with readings: see Rescue.
//
// No login kept in a home is refreshed until the store is saved, each such home
// having been given the account's login first where it held none rota could
// lose (writeDue). A store that cannot be saved then spends none of those
// tokens, and a login refreshed after that whose own save fails still goes
// into its home, where the next look reads it back, rather than nowhere. A
// listing with no such refresh to make saves no more than it did.
//
// A store whose lock was released — a run has started since it was opened —
// rotates nothing at all: no refresh, no claim, nothing written into a home.
// It reads homes into memory and reads usage with tokens that have not
// expired, and keeps the readings in memory, because a refresh token it
// rotated there could never be saved, and the one it replaced would already
// be spent.
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
	// The claims taken for a refresh are let go when this returns, however
	// it returns: a worker that panics must not leave an account claimed.
	defer func() {
		for _, release := range held {
			release()
		}
	}()
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
	reading := func(a *rota.Account, q *rota.Quota, err error) {
		if err != nil {
			report(true, fmt.Errorf("%s: quota: %w", a, err))
			return
		}
		a.Quota, a.QuotaAt = q, rota.NowMS()
		report(true, nil)
	}
	// Before any worker starts, because a rescue saves the store, and a save
	// reads every account.
	homes := s.lookIn(ctx, accounts, collect, report)
	now := rota.Now()
	// The work is planned first, one account at a time, and set going only
	// once the plan is whole: the plan writes into homes and saves the store,
	// and a save reads every account, which no worker may be changing then.
	// renews are the refreshes of logins kept in homes, which wait for the
	// save before them; plain is everything else.
	type work struct {
		a  *rota.Account
		do func()
	}
	var plain, renews []work
	for _, a := range accounts {
		shared := rota.SharedHome(a.Provider)
		h, kept := homes[a]
		if kept && h == nil {
			continue // its home could not be read, which is reported
		}
		if !readsUsage(a) || (!force && !due(a, now)) {
			continue
		}
		switch {
		case s.released && !shared:
			// Nothing may rotate here, and whether a CLI that keeps its
			// credential file to one process is running is not asked: it is
			// left alone, as it would be while it runs.
		case shared && !a.Expired():
			// A reading spends nothing, so for a home many processes share it
			// is taken whatever runs there.
			plain = append(plain, work{a, func() {
				if h != nil {
					q, err := h.usage(ctx)
					reading(a, q, err)
					return
				}
				q, err := rota.Usage(ctx, a)
				reading(a, q, err)
			}})
		case s.released, h != nil && (h.hold != "" || h.blanked):
			// Nothing may rotate here; the account keeps its last reading.
		case h != nil:
			// Expired: a refresh, which only a quiet home may have. The claim
			// is kept until the new login is in the home. The home is given
			// the account's login first, as for a launch's refresh (see
			// renew), and the save below says so on disk.
			release, ok := s.holdIdle(a)
			if !ok {
				continue
			}
			held = append(held, release)
			h.others = false
			err := func() (err error) {
				// The provider plans what is written: see guard.
				defer func() {
					if v := recover(); v != nil {
						err = fmt.Errorf("%s: panic while writing its login into its home: %v", a, v)
					}
				}()
				return h.writeDue(ctx)
			}()
			if err != nil {
				report(true, err)
				continue
			}
			renews = append(renews, work{a, func() {
				changed, err := h.refresh(ctx)
				if err != nil {
					report(changed, err)
					return
				}
				if a.Dead || h.hold != "" {
					report(true, nil) // refused, with a login in the home waiting to be confirmed
					return
				}
				h.owe()
				mu.Lock()
				renewed = append(renewed, h)
				mu.Unlock()
				q, err := rota.Usage(ctx, a)
				reading(a, q, err)
			}})
		default:
			// A running account is left alone: refreshing rotates the token
			// its CLI is still holding, and a reading is not worth that. A
			// claude account whose token rota alone holds — Windows, a
			// directory the person chose — has nothing in its home to
			// protect, as ever.
			release, idle := func() {}, true
			if !shared {
				release, idle = s.holdIdle(a)
			}
			if !idle {
				continue
			}
			held = append(held, release)
			plain = append(plain, work{a, func() {
				changed, err := rota.Refresh(ctx, a)
				if err != nil {
					report(changed, err)
					return
				}
				q, err := rota.Usage(ctx, a)
				reading(a, q, err)
			}})
		}
	}
	// No login kept in a home is refreshed until the store on disk says what
	// each home holds, so that a login refreshed below and written into its
	// home alone, when the save after it fails, is read back by the next look.
	// A store that cannot be saved spends none of them. A listing that
	// refreshes no such login saves nothing here.
	if len(renews) > 0 {
		if err := s.Save(); err != nil {
			report(false, fmt.Errorf("no login in a home refreshed: the store could not be saved: %w", err))
			renews = nil
		}
	}
	for _, w := range slices.Concat(plain, renews) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer guard(w.a)
			w.do()
		}()
	}
	wg.Wait()
	for _, msg := range said {
		s.say(msg)
	}
	if !dirty || s.released {
		return errs
	}
	if err := s.Save(); err != nil {
		// The refreshed logins are not on disk, and the ones they replaced are
		// spent. Each still goes into its home, the one copy left that the
		// next look can read back.
		errs = append(errs, err)
		for _, h := range renewed {
			if werr := h.writeRefreshed(ctx, false); werr != nil {
				errs = append(errs, werr)
			}
		}
		return errs
	}
	// Each refreshed login goes into its home now that the store has it,
	// while the home is still claimed — when rota may write there (mayWrite),
	// which a home still holding the login just refreshed away always is.
	again := false
	for _, h := range renewed {
		if err := h.writeRefreshed(ctx, true); err != nil {
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

// Rescue looks at the homes of the given claude accounts (all when none are
// given) and does what they are owed although something may be alive there, and
// nothing else: it is the look every listing starts with, for the listing that
// reads no usage. A home that is owed nothing costs a read of its store and asks
// no provider anything. Anything that changed is saved.
//
// A rescue must not depend on what a person asked to see: --short, or a reading
// that is still fresh, is no reason to leave a home signed out.
func (s *Store) Rescue(ctx context.Context, accounts ...*rota.Account) []error {
	if len(accounts) == 0 {
		accounts = s.Accounts
	}
	var errs []error
	dirty := false
	s.lookIn(ctx, accounts, s.say, func(changed bool, err error) {
		dirty = dirty || changed
		if err != nil {
			errs = append(errs, err)
		}
	})
	if dirty && !s.released {
		if err := s.Save(); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// lookIn reads the home of every claude account whose login lives in one and
// mends what wants it (mend), one account at a time. It returns each such
// account's look, nil when its home could not be read; warn takes the looks'
// remarks and report what became of each.
func (s *Store) lookIn(ctx context.Context, accounts []*rota.Account, warn func(string), report func(changed bool, err error)) map[*rota.Account]*claudeHome {
	homes := make(map[*rota.Account]*claudeHome, len(accounts))
	for _, a := range accounts {
		if !rota.SharedHome(a.Provider) || !s.loginInHome(a) {
			continue
		}
		homes[a] = nil
		// A store without its lock tries no claim at all — even a moment's
		// exclusive try can refuse a launch's shared one — and takes the home
		// for busy instead.
		h := s.claudeHome(a, s.released || s.claimed(a))
		h.warn = warn
		before := snapshot(a)
		if err := h.adopt(ctx); err != nil {
			report(false, err)
			continue
		}
		report(before.differs(a), nil)
		func() {
			// A provider panicking here would otherwise kill the process, and
			// with it every run in flight. The lock has gone by now, by mend's
			// own defer.
			defer func() {
				if v := recover(); v != nil {
					report(false, fmt.Errorf("%s: panic while rescuing: %v", a, v))
				}
			}()
			report(h.mend(ctx))
		}()
		homes[a] = h
	}
	return homes
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

// ResetGrace is how long after a window's reset time a reading taken before
// it counts as out of date. A reading that predates a reset says nothing true
// about its window afterwards — an account spent until five is whole again
// at five — so it is read again then, however young it is. The grace is the
// provider's moment to roll the window over: a reading taken the instant the
// time passes could still say spent, and would then be kept for a whole
// QuotaTTL.
const ResetGrace = 5 * time.Second

// readsUsage reports whether Refresh reads this account's usage at all.
func readsUsage(a *rota.Account) bool {
	return !a.Dead && !a.APIKey() && rota.Metered(a.Provider)
}

// due reports whether an account's reading is out of date at now: there is
// none, it is older than QuotaTTL, or a window it reports has reset since it
// was taken. It says nothing about whether a read may be sent. A "not before"
// a provider asks for — a Retry-After — is a check of its own beside the
// gate in Refresh that asks this, not a part of it: due is not asked at all
// for a forced read, and the wait holds that one back as well as the reads
// a reset or the cache makes due. NextReset would then take the later of a
// reset and the wait's end.
func due(a *rota.Account, now time.Time) bool {
	if a.QuotaAt == 0 || now.Sub(time.UnixMilli(a.QuotaAt)) > QuotaTTL {
		return true
	}
	r := pendingReset(a)
	return !r.IsZero() && !now.Before(r.Add(ResetGrace))
}

// pendingReset is the soonest reset time, among the windows of an account's
// reading, that the reading was taken before; zero when there is none. A
// reset the provider reported as already past when it was read is no news
// about the reading, which is what keeps a provider that is slow to roll a
// window over from being asked again on every look.
func pendingReset(a *rota.Account) time.Time {
	if a.Quota == nil || a.QuotaAt == 0 {
		return time.Time{}
	}
	at := time.UnixMilli(a.QuotaAt)
	var first time.Time
	for _, w := range a.Quota.Windows {
		r := w.ResetsAt.Time
		if r.After(at) && (first.IsZero() || r.Before(first)) {
			first = r
		}
	}
	return first
}

// NextReset is when the soonest window reset this store knows of puts a
// reading out of date — ResetGrace after the reset itself — or zero when no
// reading is waiting on one. It is for a process that keeps readings current
// on its own, as rota serve does: it can look again then, rather than up to a
// whole sweep later, and Refresh reads what is due. A reset that has already
// put its reading out of date is left out, since any look reads it, and so
// is every reading Refresh would never read.
func (s *Store) NextReset() time.Time {
	now := rota.Now()
	var next time.Time
	for _, a := range s.Accounts {
		if !readsUsage(a) {
			continue
		}
		r := pendingReset(a)
		if r.IsZero() {
			continue
		}
		at := r.Add(ResetGrace)
		if at.After(now) && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	return next
}
