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
// home to be quiet and out of the hold; until then the account keeps its last
// reading. What a home is owed although something is alive there comes first
// and has nothing to do with readings: see Rescue.
//
// A store whose lock was released — a run has started since it was opened —
// rotates nothing at all: no refresh, no claim, nothing written into a home.
// It reads homes into memory and reads usage with tokens that have not
// expired, because a refresh token it rotated there could never be saved, and
// the one it replaced would already be spent. Before it asks, it reads each
// account's reading and wait from the store again (catchUp); what it reads,
// and any wait a provider asks for, it writes into the store under the lock
// taken again for that one write, and nothing else (keepReadings).
//
// A usage endpoint that turns a reading away — 429, or a failure on its
// side — is left alone for as long as it says, or for a backoff when it does
// not (QuotaWait), by this store and every other opened on the same
// directory, forced or not. The account keeps its last reading and its place:
// a refusal is never a dead account, nor a reason to refuse a run. Each
// reading not taken for it is one error saying so.
func (s *Store) Refresh(ctx context.Context, force bool, accounts ...*rota.Account) []error {
	return s.refresh(ctx, force, true, accounts)
}

// refresh is Refresh; sayWaits says whether an account left alone for a wait it
// already knew is said, which a sweep repeating itself every few minutes does
// not want. The refusal that starts a wait is said either way.
func (s *Store) refresh(ctx context.Context, force, sayWaits bool, accounts []*rota.Account) []error {
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
		read    []*rota.Account
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
		if err != nil && !turnedAway(a, err) {
			report(true, fmt.Errorf("%s: quota: %w", a, err))
			return
		}
		if err == nil {
			a.Quota, a.QuotaAt, a.QuotaWait = q, rota.NowMS(), rota.QuotaWait{}
		}
		mu.Lock()
		read = append(read, a)
		mu.Unlock()
		if err != nil {
			report(true, waitNote(a, err))
			return
		}
		report(true, nil)
	}
	// A store without its lock was opened before the run it served, which may
	// have lasted hours: a wait another process has recorded since, and a newer
	// reading, are on disk and not here. They are read in before anything is
	// asked.
	if s.released {
		if err := s.catchUp(accounts); err != nil {
			report(false, fmt.Errorf("usage readings could not be read from the store: %w", err))
		}
	}
	// Before any worker starts, because a rescue saves the store, and a save
	// reads every account.
	homes := s.lookIn(ctx, accounts, collect, report)
	for _, a := range accounts {
		shared := rota.SharedHome(a.Provider)
		h, kept := homes[a]
		if kept && h == nil {
			continue // its home could not be read, which is reported
		}
		if a.Dead || a.APIKey() || !rota.Metered(a.Provider) {
			continue
		}
		stale := force || a.QuotaAt == 0 || rota.Now().Sub(time.UnixMilli(a.QuotaAt)) > QuotaTTL
		if !stale {
			continue
		}
		// The provider's "not before" is a check of its own, after the question
		// of whether a reading is due and never folded into it: whatever makes
		// a reading due — force, the cache's age, a window that has reset — the
		// wait holds it back, and before anything else, a refresh included,
		// since a token is refreshed here only to read with it. Whatever
		// schedules a look for a reading looks no sooner than QuotaWait.Until.
		if a.QuotaWait.Waiting() {
			if sayWaits {
				report(false, waitNote(a, nil))
			}
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
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer guard(a)
				if h != nil {
					q, err := h.usage(ctx)
					reading(a, q, err)
					return
				}
				q, err := rota.Usage(ctx, a)
				reading(a, q, err)
			}()
		case s.released, h != nil && (h.hold != "" || h.blanked):
			// Nothing may rotate here; the account keeps its last reading.
		case h != nil:
			// Expired: a refresh, which only a quiet home may have. The claim
			// is kept until the new login is in the home.
			release, ok := s.holdIdle(a)
			if !ok {
				continue
			}
			held = append(held, release)
			h.others = false
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer guard(a)
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
			}()
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
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer guard(a)
				changed, err := rota.Refresh(ctx, a)
				if err != nil {
					report(changed, err)
					return
				}
				q, err := rota.Usage(ctx, a)
				reading(a, q, err)
			}()
		}
	}
	wg.Wait()
	for _, msg := range said {
		s.say(msg)
	}
	if s.released {
		if err := s.keepReadings(read); err != nil {
			errs = append(errs, fmt.Errorf("usage readings could not be kept: %w", err))
		}
		return errs
	}
	if !dirty {
		return errs
	}
	if err := s.Save(); err != nil {
		// The refreshed logins are not on disk, so none goes into a home.
		return append(errs, err)
	}
	// Each refreshed login goes into its home now that the store has it,
	// while the home is still claimed — when rota may write there (mayWrite),
	// which a home still holding the login just refreshed away always is.
	again := false
	for _, h := range renewed {
		if err := h.writeRefreshed(ctx); err != nil {
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

// QuotaWaitMax is the longest a usage endpoint is left alone after it turned
// a reading away, whatever it said: a Retry-After of a day, or a year, from
// a provider having a bad day must not leave every listing on an old number
// until then. Asked again after an hour, it can say so again.
const QuotaWaitMax = time.Hour

// quotaBackoff is the wait after a refusal that says nothing of how long:
// a minute, twice that for each refusal in a row before it, up to
// QuotaWaitMax.
const quotaBackoff = time.Minute

// turnedAway reports whether err is the usage endpoint turning a reading away
// for now — rate-limited, or failing on its side — and, when it is, records
// on a when it may be asked again: when it said, or after the backoff.
func turnedAway(a *rota.Account, err error) bool {
	var he *rota.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusTooManyRequests && he.Status < 500 {
		return false
	}
	now := rota.Now()
	count := a.QuotaWait.Count + 1
	if now.Sub(time.UnixMilli(a.QuotaWait.Until)) > QuotaWaitMax {
		count = 1 // long after the last wait ran out: a new run of refusals
	}
	wait := quotaBackoff << min(count-1, 12)
	// A Retry-After that names no moment still to come — 0, or a date already
	// past — says nothing of how long, and the backoff stands.
	if he.RetryAfter.After(now) {
		wait = he.RetryAfter.Sub(now)
	}
	wait = min(wait, QuotaWaitMax)
	a.QuotaWait = rota.QuotaWait{Until: now.Add(wait).UnixMilli(), Status: he.Status, Count: count}
	return true
}

// waitNote is what is said of a reading not taken for an account's wait:
// until when, why, and how old the reading it keeps is. cause is the refusal
// that started the wait, when this is it, kept for errors.As.
func waitNote(a *rota.Account, cause error) error {
	w := a.QuotaWait
	why := fmt.Sprintf("the provider asked rota to wait (http %d)", w.Status)
	if w.Status != http.StatusTooManyRequests {
		why = fmt.Sprintf("the provider is failing (http %d)", w.Status)
	}
	kept := "there is no reading yet"
	if a.QuotaAt > 0 {
		kept = "the reading shown was taken at " + clock(time.UnixMilli(a.QuotaAt))
	}
	return &waitError{
		msg:   fmt.Sprintf("%s: usage is not asked again until %s: %s; %s", a, clock(time.UnixMilli(w.Until)), why, kept),
		cause: cause,
	}
}

// clock is a moment as a person near it reads one: the time of day for
// today, and the date too for any other day.
func clock(t time.Time) string {
	now := rota.Now()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04:05")
	}
	return t.Format("2006-01-02 15:04:05")
}

type waitError struct {
	msg   string
	cause error
}

func (e *waitError) Error() string { return e.msg }
func (e *waitError) Unwrap() error { return e.cause }

// catchUp brings what a store without its lock holds of each account's
// reading and wait up to what is on disk: a newer reading, and a wait that
// runs later — or whatever wait goes with a newer reading, which may have
// ended one. It reads under the lock, taken for the read alone, and only when
// one of the accounts has its usage read at all.
func (s *Store) catchUp(accounts []*rota.Account) error {
	if !slices.ContainsFunc(accounts, func(a *rota.Account) bool {
		return !a.Dead && !a.APIKey() && rota.Metered(a.Provider)
	}) {
		return nil
	}
	disk, err := NewStore(s.backend)
	if err != nil {
		return err
	}
	defer disk.Close()
	for _, a := range accounts {
		d := disk.Find(a.ID)
		if d == nil || d.Provider != a.Provider {
			continue
		}
		if d.QuotaWait.Until > a.QuotaWait.Until || d.QuotaAt > a.QuotaAt {
			a.QuotaWait = d.QuotaWait
		}
		if d.QuotaAt > a.QuotaAt {
			a.Quota, a.QuotaAt = d.Quota, d.QuotaAt
		}
	}
	return nil
}

// keepReadings writes what a store without its lock read — each account's
// reading, or the wait its provider asked for — into the store on disk,
// under the lock taken again for this one write. Nothing else of any account
// is touched: a token this store holds may be older than the one on disk.
// The newer reading wins; a wait on disk that has not run out gives way only
// to a longer one, because the refusal behind it may be newer than this
// reading; and a refusal keeps the longer run of refusals, so a store that
// knew of fewer does not start the backoff over.
func (s *Store) keepReadings(read []*rota.Account) error {
	if len(read) == 0 {
		return nil
	}
	disk, err := NewStore(s.backend)
	if err != nil {
		return err
	}
	defer disk.Close()
	changed := false
	for _, a := range read {
		d := disk.Find(a.ID)
		if d == nil || d.Provider != a.Provider {
			continue // removed, or its id given to another, since
		}
		if a.QuotaAt > d.QuotaAt {
			d.Quota, d.QuotaAt, changed = a.Quota, a.QuotaAt, true
		}
		w := d.QuotaWait
		if a.QuotaWait.Until > w.Until || !w.Waiting() {
			w = a.QuotaWait
		}
		if a.QuotaWait.Until > 0 {
			w.Count = max(w.Count, a.QuotaWait.Count, d.QuotaWait.Count)
		}
		if w != d.QuotaWait {
			d.QuotaWait, changed = w, true
		}
	}
	if !changed {
		return nil
	}
	return disk.Save()
}
