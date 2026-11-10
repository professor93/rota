// Package rotation decides which account a request that named none should
// run on.
//
// Every account holds a place in a queue — order 1 is tried first, then 2,
// and so on — and a threshold, the usage percentage at which the queue moves
// on to the next one. Order 0 keeps an account out of the queue without
// removing it: it can still be run by id, but nothing picks it.
//
// The rule is deliberately a queue and not a balancer. Spending one account
// to its threshold before touching the next keeps a spare account spare,
// which is the point of having several; spreading load evenly would arrive
// at every account being half-spent at once.
//
// It is outside lib on purpose. lib is the SDK: it authenticates accounts,
// builds command lines and runs them, and it takes no view on which account
// anyone ought to spend. That is a policy an application chooses — a
// different one might round-robin, or pick by price, or ask a human — so it
// lives with the applications. lib carries order and threshold as fields
// because a store has to persist them; what they mean is decided here.
package rotation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/professor93/rota/internal/askmodel"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// DefaultThreshold is the usage percentage at which the rotation moves on
// when an account names no threshold of its own: all of it.
const DefaultThreshold = 100

// ErrNone means a request named no account and none could be picked — the
// rotation is empty, or everything in it is spent, out of credit or needs
// re-auth.
var ErrNone = errors.New("no account available in the rotation")

// errExhausted is ErrNone for a rotation with accounts in it and none of them
// available.
var errExhausted = fmt.Errorf("%w: every account in the rotation is spent or needs re-auth, or is an API key "+
	"out of credit; run one by id, raise its threshold, or add an account", ErrNone)

// Cutoff is the usage percentage at which this account stops being picked.
// An unset or nonsensical threshold means all of it.
func Cutoff(a *rota.Account) int {
	if a.Threshold <= 0 || a.Threshold > 100 {
		return DefaultThreshold
	}
	return a.Threshold
}

// Spent reports whether usage has reached this account's cutoff, which is
// what makes the rotation move on.
func Spent(a *rota.Account) bool { return a.Percent() >= float64(Cutoff(a)) }

// spentOn names the window of model that this account has used up to its
// cutoff, or "" when there is none — or no model was named, which is every
// request but one that said which model it wants.
//
// Such a window covers one model, which is why Spent leaves it out: nobody
// knows which model a request that named none will use. One that names its
// model is a different matter. A Fable request on an account whose weekly
// Fable window is spent would be refused, however much room the account has
// otherwise, and a Team seat may have no weekly window but one per model.
//
// A window is the model's when the first word of its name, the provider's
// label for it ("Fable", "Opus 4.1"), is a word of the model the request
// named, whatever the case, a leading "Claude" set aside: "fable",
// "claude-fable-5" and "claude-fable-5[1m]" all name Fable's.
//
// Like Spent, it reads the figure and not when the window resets: a reading
// held back by a wait keeps a reset window spent until it is read again.
func spentOn(a *rota.Account, model string) string {
	if model == "" || a.Quota == nil {
		return ""
	}
	words := modelWords(model)
	for _, w := range a.Quota.Windows {
		if !w.Scoped || w.Percent < float64(Cutoff(a)) {
			continue
		}
		name := modelWords(w.Name)
		if len(name) > 1 && name[0] == "claude" {
			name = name[1:]
		}
		if len(name) > 0 && slices.Contains(words, name[0]) {
			return w.Name
		}
	}
	return ""
}

// modelWords is a model's name in lower case, in the words its letters and
// digits make: "claude-fable-5[1m]" is claude, fable, 5 and 1m.
func modelWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// InQueue reports whether this account is picked automatically.
func InQueue(a *rota.Account) bool { return a.Order >= 1 }

// Available reports whether the rotation may pick this account right now.
//
// An account on an API key has no usage to read and so is never spent; what
// takes it out instead is a run that found its credit gone, for as long as
// the store said that verdict stands (rota.Account.OutOfCredit). It is never
// taken out in the middle of a run: only a run that has ended says so.
func Available(a *rota.Account) bool {
	return InQueue(a) && !a.Dead && !Spent(a) && !a.OutOfCredit()
}

// orderKey sorts accounts the way both the rotation and every listing want
// them: the queue in its own order, then whatever was left out of it, and
// ids to break ties so the answer never depends on map iteration or on the
// order a store happened to write.
func orderKey(a, b *rota.Account) int {
	if (a.Order == 0) != (b.Order == 0) {
		if a.Order == 0 {
			return 1 // an unordered account sorts after every ordered one
		}
		return -1
	}
	if c := cmp.Compare(a.Order, b.Order); c != 0 {
		return c
	}
	return cmp.Compare(a.ID, b.ID)
}

// Sort orders accounts in place for display: the rotation first, lowest
// order first, then the accounts left out of it, by id.
func Sort(accounts []*rota.Account) { slices.SortStableFunc(accounts, orderKey) }

// Queue returns the accounts eligible for automatic selection, in the order
// they are tried. It never returns the input slice, so a caller may sort or
// truncate the result without disturbing a store's own order.
func Queue(accounts []*rota.Account) []*rota.Account {
	out := make([]*rota.Account, 0, len(accounts))
	for _, a := range accounts {
		if InQueue(a) {
			out = append(out, a)
		}
	}
	slices.SortStableFunc(out, orderKey)
	return out
}

// Pick returns the account a request that named none should run on: the
// first in the queue that is neither dead nor past its threshold.
//
// The quota it reads is whatever the caller last stored. Pick does no
// network calls of its own — deciding which account to spend must not depend
// on a provider being reachable — so a caller that wants a fresh reading
// refreshes before asking. Choose does exactly that.
func Pick(accounts []*rota.Account) (*rota.Account, error) {
	queue := Queue(accounts)
	if len(queue) == 0 {
		return nil, fmt.Errorf("%w: no account is in the rotation; give one an order", ErrNone)
	}
	for _, a := range queue {
		if Available(a) {
			return a, nil
		}
	}
	return nil, errExhausted
}

// Next is the place at the end of the queue.
func Next(accounts []*rota.Account) int {
	next := 1
	for _, a := range accounts {
		if a.Order >= next {
			next = a.Order + 1
		}
	}
	return next
}

// Choose resolves what a caller named: the account with this id, or the
// rotation's own choice when id is zero.
//
// Usage is read first, honouring the quota cache, because the decision is
// only as good as the numbers behind it and a five-minute-old reading is
// what the cache is for. A provider that cannot be reached is not fatal: its
// account keeps whatever reading it had, and an account nobody can get a
// number for is treated as unspent rather than quietly dropped.
//
// A request rota's own program or server makes may carry the model it named
// in ctx; an account that has used that model's own window up to its
// threshold is then passed by too (see spentOn), and so is one whose
// provider has no such model, while another has it. Nothing else carries
// one, and a choice without one is the one described above.
func Choose(ctx context.Context, st *store.Store, id int) (*rota.Account, error) {
	if id != 0 {
		a := st.Find(id)
		if a == nil {
			return nil, rota.WrapNoAccount(id)
		}
		return a, nil
	}
	if queue := Queue(st.Accounts); len(queue) > 0 {
		st.Refresh(store.WithCaller(ctx, "choose"), false, queue...)
	}
	return pickIdle(st, askmodel.From(ctx))
}

// pickIdle is Pick, stepping past an account that is already running.
//
// Some providers hand their CLI a private home and let it rewrite the
// credential file there, so rota refuses a second run on one of those rather
// than let two processes spend the same refresh token. Handing out such an
// account while it is busy would mean every request after the first is
// refused, when there is very often another account sitting right behind it.
//
// The check is a glance, not a reservation: an account can become busy
// between the choice and the run. Run is what actually decides, and a caller
// that loses the race still gets a clean refusal rather than a damaged
// account.
//
// model is the one the request named, or "": an account that has spent that
// model's window is passed by like a spent one, and so is one whose
// provider's catalog has no such model, unless no account has it.
func pickIdle(st *store.Store, model string) (*rota.Account, error) {
	queue := Queue(st.Accounts)
	if len(queue) == 0 {
		return nil, fmt.Errorf("%w: no account is in the rotation; give one an order", ErrNone)
	}
	busy := 0
	var (
		windows []string
		// refused is the first account whose provider has no such model: the
		// answer when no account has it, whose own run then says so, as it
		// did before a request's model counted here at all.
		refused *rota.Account
	)
	for _, a := range queue {
		if !Available(a) {
			continue
		}
		if model != "" {
			if _, err := rota.ResolveModel(a.Provider, model); err != nil {
				if refused == nil && !st.Busy(a) {
					refused = a
				}
				continue
			}
		}
		if w := spentOn(a, model); w != "" {
			if !slices.Contains(windows, w) {
				windows = append(windows, w)
			}
			continue
		}
		if st.Busy(a) {
			busy++
			continue
		}
		return a, nil
	}
	if busy > 0 {
		return nil, fmt.Errorf("%w: every account that could answer is already running; "+
			"these providers keep their own credential file and cannot run twice at once, so wait or add an account", ErrNone)
	}
	if len(windows) > 0 {
		return nil, fmt.Errorf("%w: every account in the rotation is spent or needs re-auth, or is an API key out of credit, "+
			"or has used its %s window up to its threshold; ask for another model, run one by id, raise its threshold, or add an account",
			ErrNone, strings.Join(windows, " or "))
	}
	if refused != nil {
		return refused, nil
	}
	return nil, errExhausted
}

// Backfill puts every account of a store written before rotation existed
// into the queue, by id, so the feature works the moment it arrives instead
// of after somebody numbers five accounts by hand.
//
// It runs once. Afterwards an account left at order 0 is a decision, and
// this must not reverse it — which is what the store's Ordered flag records.
// The result is written straight back, because a store renumbered in memory
// on every load and never saved would keep renumbering over later choices.
//
// Best-effort: a read-only store still works, it just backfills again next
// time. Failing to open a store over this would be worse than the repetition.
func Backfill(st *store.Store) {
	// An empty store has nothing to decide, and writing one out just to
	// record that would create an account file for someone who has not added
	// an account yet.
	if st.Ordered || len(st.Accounts) == 0 {
		return
	}
	st.Ordered = true
	byID := append([]*rota.Account(nil), st.Accounts...)
	slices.SortFunc(byID, func(a, b *rota.Account) int { return a.ID - b.ID })
	for _, a := range byID {
		if a.Order == 0 {
			a.Order = Next(st.Accounts)
		}
	}
	_ = st.Save()
}
