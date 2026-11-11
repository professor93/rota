package tui

// The Accounts tab's own actions: an account moved in the rotation, out of
// it and back, its threshold stepped by five or typed (R36), and the bell
// when an account goes over its threshold (R11).

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/wire"
)

// accountsActBad is what the typed threshold says of a value it cannot take.
const accountsActBad = "a threshold is a whole percentage from 1 to 100"

// accountsActState is what the actions keep between a change sent and its
// answer being listed: each account's threshold, and the moves waiting
// behind the one out. It is the tab's (accountsTab.act), read and written
// in Update alone, and its zero value is ready.
type accountsActState struct {
	ids map[int]*accountsActFlight
	// moving says a move is out, and moves are the ones pressed since, in
	// order: each is sent once the one before it is answered.
	moving bool
	moves  []accountsActPlace
}

// accountsActPlace is a move waiting: the account and where to.
type accountsActPlace struct {
	acct  wire.Account
	place string
}

// accountsActFlight is one account's threshold change.
type accountsActFlight struct {
	// out is true while a change is in flight, and sent is its value. next
	// is the value pressed or typed since, each press clamped as it came,
	// 0 for none; it is sent once out is answered.
	out        bool
	sent, next int
	// now is what the last answer set, 0 for none. Until a listing comes,
	// a step is taken from it rather than from the table, which may still
	// show the value before.
	now int
}

// accountsActAnswer is how a change the actions sent was answered, which
// the tab hands back to them (answered). A move's names its place; a
// threshold's names none.
type accountsActAnswer struct {
	id    int
	place string
	v     int    // the threshold sent
	got   int    // the threshold the server answered with
	who   string // the account, as a refusal names it
	err   error
}

// accountsActSend is the command that sends c for the account ans is
// about, under an ordinary call's deadline, and answers with ans and how
// it went: an error for a refusal, a panic or no answer, so whatever waits
// on the answer always hears one.
func accountsActSend(e *env, c Change, ans accountsActAnswer) tea.Cmd {
	b := e.b
	return e.callOr(envCallTimeout, func(ctx context.Context) tea.Msg {
		r, err := b.Change(ctx, ans.id, c)
		out := ans
		out.got, out.err = r.Threshold, err
		return out
	}, func(err error) tea.Msg {
		out := ans
		out.err = err
		return out
	}, nil)
}

// answered takes the answer to a change the actions sent.
func (a *accountsTab) answered(m accountsActAnswer) tea.Cmd {
	if m.place != "" {
		return a.act.moved(a.e, m)
	}
	return a.act.set(a.e, m)
}

// move puts acct at place in the rotation, in the words `rota set --order`
// takes: up, down, first, last or out. Each press is a move of its own. One
// move of the tab's is out at a time, as moving one account shifts the
// others: the next waits for its answer, so the server takes them in the
// order pressed.
func (a *accountsTab) move(acct wire.Account, place string) tea.Cmd {
	if a.act.moving {
		a.act.moves = append(a.act.moves, accountsActPlace{acct, place})
		return nil
	}
	a.act.moving = true
	return accountsActMoveTo(a.e, acct, place)
}

func accountsActMoveTo(e *env, acct wire.Account, place string) tea.Cmd {
	return accountsActSend(e, Change{Order: &place}, accountsActAnswer{id: acct.ID, place: place, who: accountsActWho(acct)})
}

// moved takes a move's answer: the next move waiting is sent, and after
// the last the listing is read again, as a move shifts the others. A
// refusal is said, cleaned, the moves waiting are dropped, and the listing
// is read again.
func (s *accountsActState) moved(e *env, m accountsActAnswer) tea.Cmd {
	if !s.moving {
		return nil
	}
	if m.err != nil {
		s.moving, s.moves = false, nil
		return accountsActRefused(m.who+" could not "+accountsActMove(m.place), m.err)
	}
	if len(s.moves) > 0 {
		next := s.moves[0]
		s.moves = s.moves[1:]
		return accountsActMoveTo(e, next.acct, next.place)
	}
	s.moving = false
	return accountsActRefresh
}

// accountsActMove is what a move to place does, as a refusal says it.
func accountsActMove(place string) string {
	switch place {
	case "up":
		return "move up"
	case "down":
		return "move down"
	case "first":
		return "go first"
	case "last":
		return "go back into the rotation"
	case "out":
		return "leave the rotation"
	}
	return "move to " + place
}

// nudge moves acct's threshold by delta, within 1..100 (R36). One change
// of an account's threshold is out at a time: the presses while it is out
// make one value, clamped press by press as they would be one at a time,
// sent once it is answered, so a key held down sends two changes, not
// thirty.
func (a *accountsTab) nudge(acct wire.Account, delta int) tea.Cmd {
	return accountsActThreshold(a, acct, 0, delta)
}

// toggleOut takes acct out of the rotation, or puts it back last when it
// is out.
func (a *accountsTab) toggleOut(acct wire.Account) tea.Cmd {
	if acct.Order < 1 {
		return a.move(acct, "last")
	}
	return a.move(acct, "out")
}

// accountsActThreshold sets acct's threshold to to, or keeps it when to is
// 0, moved by by, within 1..100. While a change of it is out, this makes
// the value that follows it, from the one waiting or the one out: a typed
// value replaces the steps before it. Else it is sent now, from the
// threshold listed, or from the last answer when no listing has come
// since; one that changes nothing is not sent.
func accountsActThreshold(a *accountsTab, acct wire.Account, to, by int) tea.Cmd {
	if i := slices.IndexFunc(a.rows, func(r wire.Account) bool { return r.ID == acct.ID }); i >= 0 {
		acct = a.rows[i]
	}
	s := &a.act
	f := s.ids[acct.ID]
	if f != nil && f.out {
		from := f.next
		if from == 0 {
			from = f.sent
		}
		f.next = accountsActStep(from, to, by)
		return nil
	}
	from := barsThreshold(acct.Threshold)
	if f != nil {
		from = f.now
	}
	want := accountsActStep(from, to, by)
	if want == from {
		return nil
	}
	if s.ids == nil {
		s.ids = map[int]*accountsActFlight{}
	}
	if f == nil {
		f = &accountsActFlight{}
		s.ids[acct.ID] = f
	}
	f.out, f.sent = true, want
	return accountsActSetTo(a.e, acct.ID, accountsActWho(acct), want)
}

func accountsActSetTo(e *env, id int, who string, v int) tea.Cmd {
	return accountsActSend(e, Change{Threshold: &v}, accountsActAnswer{id: id, v: v, who: who})
}

// accountsActStep is to, or from when to is 0, moved by by, within 1..100.
func accountsActStep(from, to, by int) int {
	if to != 0 {
		from = to
	}
	return min(max(from+by, 1), 100)
}

// set takes a threshold's answer: the value that waited is sent, or, with
// none, the listing is read again so the table shows the threshold. A
// refusal is said, cleaned, and the value waiting goes with it; a value
// answered before it in the same run of presses still stands until a
// listing comes, as the server has it.
func (s *accountsActState) set(e *env, m accountsActAnswer) tea.Cmd {
	f := s.ids[m.id]
	if f == nil || !f.out {
		return nil
	}
	if m.err != nil {
		f.out, f.next = false, 0
		if f.now == 0 {
			delete(s.ids, m.id)
		}
		return accountsActRefused(fmt.Sprintf("the threshold of %s could not be set to %d%%", m.who, m.v), m.err)
	}
	f.now = m.v
	if m.got >= 1 && m.got <= 100 {
		f.now = m.got
	}
	if f.next != 0 && f.next != f.now {
		v := f.next
		f.sent, f.next = v, 0
		return accountsActSetTo(e, m.id, m.who, v)
	}
	f.out, f.next = false, 0
	return accountsActRefresh
}

// listed lets go, at a listing, of each answer that waited for one: from
// now on a step is taken from what is listed. A change still out is kept.
func (s *accountsActState) listed() {
	for id, f := range s.ids {
		if !f.out {
			delete(s.ids, id)
		}
	}
}

// accountsActRefused says what could not be done and why, cleaned, and
// asks for the listing as it now is, since a refusal may mean the account
// changed or went.
func accountsActRefused(what string, err error) tea.Cmd {
	return tea.Batch(toast(levelErr, "%s", clean(what+": "+err.Error())), accountsActRefresh)
}

func accountsActRefresh() tea.Msg { return refreshMsg{How: RefreshNone} }

// crossed rings the bell once, and says which in a toast each, when
// accounts went over their thresholds between two listings (R11): usage
// read below the threshold before and at it or past it now. The threshold
// is the one listed now, so a threshold lowered under the usage moves the
// usage past nothing and rings nothing; an account listed or read for the
// first time is no crossing, as the first listing is none. The bell is
// armed again once the account falls back below.
func (a *accountsTab) crossed(before, after []wire.Account) tea.Cmd {
	was := make(map[int]wire.Account, len(before))
	for _, r := range before {
		was[r.ID] = r
	}
	var said []tea.Cmd
	for _, r := range after {
		// An account not listed before has no reading before.
		b := was[r.ID]
		limit := float64(barsThreshold(r.Threshold))
		if b.Metered && b.CheckedAt != "" && b.Percent < limit && r.Metered && r.Percent >= limit {
			said = append(said, toast(levelWarn, "%s", accountsActCrossing(r)))
		}
	}
	if len(said) == 0 {
		return nil
	}
	return tea.Batch(append([]tea.Cmd{accountsActBell}, said...)...)
}

func accountsActBell() tea.Msg { return bellMsg{} }

// accountsActCrossing says that r crossed its threshold, and in which
// window: claude/a@b crossed its 80% (5h 82%). Percents are floored, as
// the bars draw them, and the whole is cleaned, the window's name with it.
func accountsActCrossing(r wire.Account) string {
	at := strconv.Itoa(int(barsPercent(r.Percent))) + "%"
	if w, ok := deciding(r); ok {
		at = w.Name + " " + strconv.Itoa(int(barsPercent(w.Percent))) + "%"
	}
	return clean(fmt.Sprintf("%s crossed its %d%% (%s)", accountsActWho(r), barsThreshold(r.Threshold), at))
}

// accountsActWho names an account in what the actions say: provider and
// label, as claude/work@example.com, cleaned.
func accountsActWho(acct wire.Account) string {
	if p := clean(acct.Provider); strings.TrimSpace(p) != "" {
		return p + "/" + accountsLabel(acct)
	}
	return accountsLabel(acct)
}

// accountsActBindings are the typed threshold's keys.
type accountsActBindings struct{ Apply, Cancel key.Binding }

func accountsActKeys() accountsActBindings {
	return accountsActBindings{
		Apply:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "apply")),
		Cancel: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	}
}

// accountsActTyped is the field % opens: one account's threshold, typed.
// It takes digits alone, so all it ever draws of what it was given is
// digits, and says what it wants when given anything else.
type accountsActTyped struct {
	a      *accountsTab
	acct   wire.Account
	keys   accountsActBindings
	in     textinput.Model
	bad    bool
	closed bool
}

// typedThreshold is the overlay that takes acct's threshold as typed: a
// whole percentage from 1 to 100, applied on enter as a step is, and not
// sent when it is the threshold the account has.
func (a *accountsTab) typedThreshold(acct wire.Account) overlay {
	in := newInput(a.e.th)
	in.Prompt = ""
	in.CharLimit = 3
	in.Focus()
	return &accountsActTyped{a: a, acct: acct, keys: accountsActKeys(), in: in}
}

// About says the field is about acct's threshold, so a % pressed twice
// opens it once.
func (o *accountsActTyped) About() (kind string, account int) { return "threshold", o.acct.ID }
func (o *accountsActTyped) Keys() []key.Binding               { return []key.Binding{o.keys.Apply, o.keys.Cancel} }
func (o *accountsActTyped) Typing() bool                      { return true }
func (o *accountsActTyped) Closed() bool                      { return o.closed }

func (o *accountsActTyped) Update(msg tea.Msg) (overlay, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, o.keys.Apply):
			return o, o.apply()
		case key.Matches(msg, o.keys.Cancel):
			o.closed = true
			return o, nil
		case msg.Text != "" && !accountsActDigits(msg.Text):
			o.bad = true
			return o, nil
		}
		o.bad = false
		var cmd tea.Cmd
		o.in, cmd = o.in.Update(msg)
		return o, cmd
	case tea.PasteMsg:
		// A paste is the value, trimmed, and its % is understood.
		v := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(msg.Content), "%"))
		if len(v) > o.in.CharLimit || !accountsActDigits(v) {
			o.bad = true
			return o, nil
		}
		o.in.SetValue(v)
		o.in.CursorEnd()
		o.bad = false
	case listingMsg:
		// An account another terminal removed has no threshold to set.
		if msg.Err == nil && !slices.ContainsFunc(msg.Listing.Accounts, func(r wire.Account) bool { return r.ID == o.acct.ID }) {
			o.closed = true
			return o, toast(levelWarn, "%s", accountsActWho(o.acct)+" is no longer listed; its threshold was not set")
		}
	}
	return o, nil
}

// apply sets the threshold typed, or says what a threshold is.
func (o *accountsActTyped) apply() tea.Cmd {
	v, err := strconv.Atoi(o.in.Value())
	if err != nil || v < 1 || v > 100 {
		o.bad = true
		return nil
	}
	o.closed = true
	return accountsActThreshold(o.a, o.acct, v, 0)
}

// accountsActDigits says s is one or more of 0 to 9 and nothing else.
func accountsActDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// View draws whom the field is for, the threshold listed, the field and,
// after a value it cannot take, what it wants: each line cut to width.
func (o *accountsActTyped) View(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	th := o.a.e.th
	acct := o.acct
	if i := slices.IndexFunc(o.a.rows, func(r wire.Account) bool { return r.ID == acct.ID }); i >= 0 {
		acct = o.a.rows[i]
	}
	lines := []string{accountsActLine(th.Title, "Threshold of "+accountsActWho(acct), width), ""}
	say := func(style lipgloss.Style, text string) {
		for _, l := range wrap(text, width) {
			lines = append(lines, style.Render(l))
		}
	}
	say(th.Muted, fmt.Sprintf("the usage at which the rotation moves on from it, now %d%%", barsThreshold(acct.Threshold)))
	// The field holds digits alone; the line is cut to width as a whole.
	lines = append(lines, "", ansi.Truncate(th.Muted.Render("threshold: ")+o.in.View()+th.Muted.Render("%"), width, ""))
	if o.bad {
		lines = append(lines, "")
		say(th.Over, accountsActBad)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

// accountsActLine is text, which clean gave, cut to w columns in style.
func accountsActLine(style lipgloss.Style, text string, w int) string {
	return style.Render(strings.TrimRight(fit(text, w), " "))
}
