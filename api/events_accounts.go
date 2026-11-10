package api

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/events"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/rotation"
	"github.com/professor93/rota/store"
)

// The account events: what a background sweep read of the accounts, said
// against what the sweep before it read. Each is a latch, said once on the
// look where it changes and never for what was so before this server read
// it: a daemon that starts after a night off does not announce every
// window that came back meanwhile. A look follows every sweep (nextReset
// and maintain), so with the sweeps off there are none.
//
//   - account.threshold: rotation.Spent changing between two readings that
//     both exist. A failed reading is no reading, and changes no latch.
//   - account.reset: a window that had been used coming back, once.
//   - account.dead (reason login): the account coming to need a login,
//     marked dead or its login ended. A new login re-arms it silently.
//   - account.rotated: the account rotation.Pick takes changing, with why.
//     Busy accounts are not looked at: busyness is momentary.

// accountResetMoved is how far a window's reset must move for the window to
// count as back, and how near its old reset must be: less is a provider's
// clock jittering, not a new window.
const accountResetMoved = time.Minute

// accountSessionsNamed is how many of an account's sessions an
// account.threshold names; any more are counted. accountSessionName is the
// most of a name it repeats, so the sentence keeps its verb.
const (
	accountSessionsNamed = 3
	accountSessionName   = 64
)

// accountWatch is what the account events are computed against: the last
// look, nil until the first, which is the baseline and says nothing. since
// is when that first look was taken, which is as the server's sweeps begin,
// less two sweeps: a reading taken before it is not this server's, and
// counts as none. So a night with no daemon says nothing of what happened in
// it, and a daemon restarted for an upgrade still says a crossing its first
// sweep finds. Only the sweeps use it, one at a time, but under mu all the
// same.
type accountWatch struct {
	mu    sync.Mutex
	last  *accountsLook
	since time.Time
}

// accountLook is what one look read of one account, copied out of the
// store's account so that nothing is shared with it.
//
// spent, percent, window and windows come from a reading. An account with
// none this time carries them over from the look before (diffAccounts), and
// read says whether they come from a reading at all.
type accountLook struct {
	id, order int
	limit     int // rotation.Cutoff
	label     string
	provider  string
	// why is what the account needs a login for, while dead.
	why string
	// window is the window percent comes from: the unscoped one with the
	// highest reading, else the first unscoped one; "" when there is none.
	window string
	read   bool
	readAt int64 // QuotaAt, unix ms
	// dead is Dead or LoginEnded: it needs a login. marked is Dead alone,
	// which is all rotation.Pick passes an account by.
	dead      bool
	marked    bool
	spent     bool // rotation.Spent
	credit    bool // OutOfCredit
	queued    bool // rotation.InQueue
	available bool // rotation.Available
	percent   float64
	windows   map[string]windowLook // the unscoped windows, by name
}

// windowLook is one window as a look read it.
type windowLook struct {
	percent float64
	resets  time.Time
}

// accountsLook is one look at every account, and the account the rotation
// picks from them, 0 for none.
type accountsLook struct {
	byID  map[int]accountLook
	first int
}

// lookAccounts is one look at accounts, as a sweep has just left them.
func lookAccounts(accounts []*rota.Account) accountsLook {
	look := accountsLook{byID: make(map[int]accountLook, len(accounts))}
	for _, a := range accounts {
		l := accountLook{
			id: a.ID, order: a.Order, limit: rotation.Cutoff(a),
			label: a.Label(), provider: a.Provider,
			read: a.Quota != nil, readAt: a.QuotaAt,
			dead: a.Dead || a.LoginEnded(), marked: a.Dead,
			spent: rotation.Spent(a), credit: a.OutOfCredit(),
			queued: rotation.InQueue(a), available: rotation.Available(a),
			percent: a.Percent(),
		}
		switch {
		case a.Dead:
			l.why = a.WhyDead()
		case l.dead:
			l.why = fmt.Sprintf("its login ended on %s UTC; rota login %d makes a new one",
				a.LoginUntil().UTC().Format(time.DateOnly), a.ID)
		}
		if a.Quota != nil {
			l.windows = make(map[string]windowLook, len(a.Quota.Windows))
			top := math.Inf(-1)
			for _, w := range a.Quota.Windows {
				if w.Scoped {
					continue // Percent leaves them out, and so do the events
				}
				l.windows[w.Name] = windowLook{percent: w.Percent, resets: w.ResetsAt.Time}
				if w.Percent > top {
					top, l.window = w.Percent, w.Name
				}
			}
		}
		look.byID[a.ID] = l
	}
	if a, err := rotation.Pick(accounts); err == nil {
		look.first = a.ID
	}
	return look
}

// readSince is look with every reading taken before since counted as none.
// What the reading says still decides the rotation's pick, as it does Pick's,
// but it is no latch: the first reading this server takes itself is.
func (look accountsLook) readSince(since time.Time) accountsLook {
	out := accountsLook{byID: make(map[int]accountLook, len(look.byID)), first: look.first}
	for id, l := range look.byID {
		if l.read && l.readAt < since.UnixMilli() {
			l.read, l.windows = false, nil
		}
		out.byID[id] = l
	}
	return out
}

// pickable is rotation.Available said of a look: in the queue, not marked
// dead, not spent and not out of credit. It is how a look's available is
// made again once its spent latch has been carried or rebased.
func (l accountLook) pickable() bool { return l.queued && !l.marked && !l.spent && !l.credit }

// diffAccounts is what changed from prev to next, said as events, and next
// as the following look should be compared with: an account next has no
// reading for keeps prev's reading, so a reading that failed changes no
// latch, and the rotation's pick is made again over what was kept.
//
// What is compared is a baseline of its own, and says nothing, until it
// changes: an account prev has not seen, and the reading of one prev has
// no reading of. Each is taken as next has it, in prev too, for every
// event, the rotation's included. So diffAccounts(accountsLook{}, next, now)
// says nothing at all. The events come by account id, each account's in
// the order dead, reset, threshold, and the rotation's last.
func diffAccounts(prev, next accountsLook, now time.Time) ([]events.Event, accountsLook) {
	kept := accountsLook{byID: make(map[int]accountLook, len(next.byID)), first: next.first}
	carried := false
	for id, n := range next.byID {
		if p, seen := prev.byID[id]; seen && !n.read {
			n.read, n.spent, n.percent, n.window, n.windows = p.read, p.spent, p.percent, p.window, p.windows
			n.available = n.pickable()
			carried = true
		}
		kept.byID[id] = n
	}
	if carried {
		kept.first = accountFirst(kept.byID)
	}

	base := accountsLook{byID: maps.Clone(prev.byID), first: prev.first}
	if base.byID == nil {
		base.byID = make(map[int]accountLook, len(kept.byID))
	}
	rebased := false
	for id, n := range kept.byID {
		switch p, seen := prev.byID[id]; {
		case !seen:
			n.windows = accountResetsPassed(n.windows, now)
			base.byID[id], rebased = n, true
		case !p.read && n.read:
			p.read, p.spent, p.percent, p.window = n.read, n.spent, n.percent, n.window
			p.windows = accountResetsPassed(n.windows, now)
			p.available = p.pickable()
			base.byID[id], rebased = p, true
		}
	}
	if rebased {
		base.first = accountFirst(base.byID)
	}

	var out []events.Event
	ids := slices.Sorted(maps.Keys(kept.byID))
	for _, id := range ids {
		evs, n := accountChanges(base.byID[id], kept.byID[id], now)
		kept.byID[id] = n
		out = append(out, evs...)
	}
	if e, ok := accountRotated(base, kept); ok {
		out = append(out, e)
	}
	return out, kept
}

// accountResetsPassed is windows with every reset that has passed by now
// taken as none, for a reading compared with itself (diffAccounts): a reset
// that passed before the first comparison happened while nobody compared,
// and is not said when the provider catches up with it (accountChanges
// keeps it none until then).
func accountResetsPassed(windows map[string]windowLook, now time.Time) map[string]windowLook {
	out, cloned := windows, false
	for name, w := range windows {
		if w.resets.IsZero() || now.Before(w.resets) {
			continue
		}
		if !cloned {
			out, cloned = maps.Clone(windows), true
		}
		w.resets = time.Time{}
		out[name] = w
	}
	return out
}

// accountChanges is what changed about one account from p to n, and n as
// the next look should compare with it.
func accountChanges(p, n accountLook, now time.Time) ([]events.Event, accountLook) {
	var out []events.Event
	if n.dead && !p.dead {
		out = append(out, events.Event{Type: events.AccountDead, Account: n.id, Name: n.label,
			Reason: "login", Text: "needs a login: " + n.why})
	}

	// A window that had been used is back when its reset has moved on from
	// a reset that was due, or that it reads lower than; or when its reset
	// has passed and it reads lower, or is gone. A reset that moves while
	// the window fills is a provider's clock, and one that moves earlier is
	// not a new window.
	//
	// A reset said before the provider moved it on is kept as no reset at
	// all, for as long as the provider still gives the one that passed: when
	// it does move on, it is the reset already said.
	cloned := false
	noReset := func(name string, w windowLook) {
		if !cloned {
			n.windows, cloned = maps.Clone(n.windows), true
		}
		w.resets = time.Time{}
		n.windows[name] = w
	}
	for _, name := range slices.Sorted(maps.Keys(p.windows)) {
		was := p.windows[name]
		is, still := n.windows[name]
		if was.resets.IsZero() {
			if still && !is.resets.IsZero() && !now.Before(is.resets) {
				noReset(name, is)
			}
			continue
		}
		if was.percent <= 0 {
			continue
		}
		fell := still && is.percent < was.percent
		due := !now.Before(was.resets.Add(-accountResetMoved))
		moved := still && !is.resets.IsZero() && is.resets.Sub(was.resets) > accountResetMoved && (due || fell)
		passed := !now.Before(was.resets) && (!still || fell)
		if !moved && !passed {
			continue
		}
		if still && !moved {
			noReset(name, is)
		}
		had := was.percent
		out = append(out, events.Event{Type: events.AccountReset, Account: n.id, Name: n.label,
			Window: name, Percent: &had, Limit: n.limit,
			Text: fmt.Sprintf("has its %s window back: it had reached %s", name, roundPercent(had))})
	}

	if p.read && n.read && p.spent != n.spent {
		at := "usage at " + roundPercent(n.percent)
		if n.window != "" {
			at = n.window + " at " + roundPercent(n.percent)
		}
		reading := n.percent
		e := events.Event{Type: events.AccountThreshold, Account: n.id, Name: n.label,
			Window: n.window, Percent: &reading, Limit: n.limit}
		if n.spent {
			e.Reason, e.Text = "up", fmt.Sprintf("reached its %d%% limit: %s", n.limit, at)
		} else {
			e.Reason, e.Text = "down", fmt.Sprintf("is under its %d%% limit again: %s", n.limit, at)
		}
		out = append(out, e)
	}
	return out, n
}

// accountRotated is the rotation's pick moving from prev's to next's, and
// why, or false when it has not moved. prev holds every account next does.
//
// When the new pick is ahead of the old one in the queue, or there was
// none, the new pick is what changed: it can be used again (back), or it
// was put ahead (order). Otherwise the old pick is what changed, and why it
// is passed by is said of it, in the order Pick's reasons are worth acting
// on: removed, left the rotation, marked dead, out of credit, spent.
func accountRotated(prev, next accountsLook) (events.Event, bool) {
	was, is := prev.first, next.first
	if was == is {
		return events.Event{}, false
	}
	if is == 0 {
		text := "No account is available in the rotation: no account is in it"
		for _, l := range next.byID {
			if l.queued {
				text = "No account is available in the rotation: every account in it is spent, needs a login or is out of credit"
				break
			}
		}
		return events.Event{Type: events.AccountRotated, Reason: "none", Text: text}, true
	}
	before, n := prev.byID[is], next.byID[is]
	old, stays := next.byID[was]
	var reason, why string
	switch {
	case was == 0 || stays && old.queued && accountAhead(n, old):
		reason, why = "order", "the rotation was reordered"
		if before.queued && !before.available {
			reason, why = "back", "it can be used again"
		}
	case !stays:
		reason, why = "removed", prev.byID[was].label+" was removed"
	case !old.queued:
		reason, why = "left", old.label+" left the rotation"
	case old.marked:
		reason, why = "dead", old.label+" needs a login"
	case old.credit:
		reason, why = "credit", old.label+" is out of credit"
	case old.spent:
		reason, why = "spent", fmt.Sprintf("%s reached its %d%% limit", old.label, old.limit)
	default:
		reason, why = "order", "the rotation was reordered"
	}
	return events.Event{Type: events.AccountRotated, Account: is, Name: n.label, Reason: reason,
		Text: "is first in the rotation now: " + why}, true
}

// accountAhead reports whether a comes before b in the rotation's queue:
// lower order first, then lower id, as rotation.Pick tries them.
func accountAhead(a, b accountLook) bool {
	return cmp.Or(cmp.Compare(a.order, b.order), cmp.Compare(a.id, b.id)) < 0
}

// accountFirst is the account rotation.Pick would take of looks: the first
// in the queue that is available, or 0 for none.
func accountFirst(byID map[int]accountLook) int {
	var first *accountLook
	for _, l := range byID {
		if l.available && (first == nil || accountAhead(l, *first)) {
			first = &l
		}
	}
	if first == nil {
		return 0
	}
	return first.id
}

// roundPercent is a reading in words: rounded to a whole number, "82%".
func roundPercent(p float64) string { return strconv.Itoa(int(math.Round(p))) + "%" }

// lookAtAccounts compares what a sweep has just read of the accounts with
// what the one before it read, and says what changed. The first look is a
// baseline and says nothing. It is called with the store a sweep has open,
// and a panic in it costs this look, not the sweep.
func (s *Server) lookAtAccounts(st *store.Store) {
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("panic while looking at the accounts for their events", "panic", v)
		}
	}()
	s.watchAccounts(lookAccounts(st.Accounts))
}

// watchAccounts is lookAtAccounts for a look already taken: the first is
// kept as the baseline, and each after it is said against the last. A
// reading taken more than two sweeps before the first look is none
// (accountWatch, readSince).
func (s *Server) watchAccounts(next accountsLook) {
	w := &s.accounts
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.since = s.now()
		if every := s.opts.RefreshEvery; every > 0 {
			w.since = w.since.Add(-2 * every)
		}
		next = next.readSince(w.since)
		w.last = &next
		return
	}
	evs, kept := diffAccounts(*w.last, next.readSince(w.since), s.now())
	w.last = &kept
	for _, e := range evs {
		if e.Type == events.AccountThreshold {
			e.Text += s.accountRunsOn(e.Account)
		}
		s.emit(e)
	}
}

// accountRunsOn is what an account.threshold adds about the sessions still
// running on account id, oldest first: "; refactor runs on it", "; refactor
// and api run on it", "; a, b, c and 2 more run on it"; "" for none. They
// are named as every event names a session (eventName), each cut to
// accountSessionName bytes.
func (s *Server) accountRunsOn(id int) string {
	var on []*termSession
	for _, ts := range s.everyTerminal() {
		if ts.account == id && !ts.over() {
			on = append(on, ts)
		}
	}
	if len(on) == 0 {
		return ""
	}
	slices.SortFunc(on, func(a, b *termSession) int {
		return cmp.Or(a.started.Compare(b.started), strings.Compare(a.id, b.id))
	})
	names := make([]string, 0, accountSessionsNamed)
	for _, ts := range on[:min(len(on), accountSessionsNamed)] {
		names = append(names, cleanEventString(ts.eventName(), accountSessionName))
	}
	switch more := len(on) - len(names); {
	case len(on) == 1:
		return "; " + names[0] + " runs on it"
	case more > 0:
		return "; " + strings.Join(names, ", ") + " and " + strconv.Itoa(more) + " more run on it"
	default:
		return "; " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " run on it"
	}
}
