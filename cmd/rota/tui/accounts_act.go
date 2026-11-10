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
	"sync"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/wire"
)

// accountsActBad is what the typed threshold says of a value it cannot take.
const accountsActBad = "a threshold is a whole percentage from 1 to 100"

// move puts acct at place in the rotation, in the words `rota set --order`
// takes: up, down, first, last or out. Each press is a move of its own. A
// move shifts the others, so once it is answered the listing is read
// again as it is; a refusal is said, cleaned, in a toast.
func (a *accountsTab) move(acct wire.Account, place string) tea.Cmd {
	b, id := a.e.b, acct.ID
	what := accountsActWho(acct) + " could not " + accountsActMove(place)
	failed := func(err error) tea.Msg { return accountsActRefused(what, err) }
	return a.e.callOr(envCallTimeout, func(ctx context.Context) tea.Msg {
		if _, err := b.Change(ctx, id, Change{Order: &place}); err != nil {
			return failed(err)
		}
		return accountsActRefresh()
	}, failed, nil)
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
// are summed and sent as one change once it is answered, so a key held
// down sends two changes, not thirty.
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

// crossed rings the bell once, and says which in a toast each, when
// accounts went over their thresholds between two listings (R11): usage
// read below the threshold before and at it or past it now. The threshold
// is the one listed now, so a threshold lowered under the usage moves the
// usage past nothing and rings nothing; an account listed or read for the
// first time is no crossing, as the first listing is none. The bell is
// armed again once the account falls back below.
//
// The tab calls it at every listing after the first, which is also when a
// threshold answered stops standing in for the one listed
// (accountsActListed).
func (a *accountsTab) crossed(before, after []wire.Account) tea.Cmd {
	accountsActListed(a)
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

// accountsActRefused says what could not be done and why, cleaned, and
// asks for the listing as it now is, since a refusal may mean the account
// changed or went.
func accountsActRefused(what string, err error) tea.Msg {
	return tea.BatchMsg{toast(levelErr, "%s", clean(what+": "+err.Error())), accountsActRefresh}
}

func accountsActRefresh() tea.Msg { return refreshMsg{How: RefreshNone} }

// accountsActState is every account's threshold change that is out, or
// whose answer no listing has shown yet, by the tab it was asked in. Update
// and the change's own command both reach it, so it is locked. It is kept
// beside the tab, not in it, because the tab's struct is accounts.go's and
// the tab hands its actions back nothing but listings. A tab's entries go
// when the last is let go of, or when its program's context ends.
type accountsActState struct {
	sync.Mutex
	of map[*accountsTab]*accountsActTab
}

type accountsActTab struct {
	ids  map[int]*accountsActFlight
	stop func() bool
}

// accountsActFlight is one account's threshold change.
type accountsActFlight struct {
	// out is true while a change is in flight. to is a threshold typed
	// meanwhile, 0 for none, and by the steps pressed meanwhile, summed:
	// both go as one change once it is answered.
	out    bool
	to, by int
	// now is the threshold the last answer set. Until a listing comes, a
	// step is taken from it rather than from the table, which may still
	// show the one before.
	now int
}

var accountsActFlights accountsActState

// flight is id's change in a, or nil. s is locked.
func (s *accountsActState) flight(a *accountsTab, id int) *accountsActFlight {
	if t := s.of[a]; t != nil {
		return t.ids[id]
	}
	return nil
}

// hold is id's change in a, made when there is none. A tab's first one
// ties the tab's entries to its program's context. s is locked, and it
// runs in Update, which is where a.e may be read.
func (s *accountsActState) hold(a *accountsTab, id int) *accountsActFlight {
	t := s.of[a]
	if t == nil {
		if s.of == nil {
			s.of = map[*accountsTab]*accountsActTab{}
		}
		t = &accountsActTab{ids: map[int]*accountsActFlight{}}
		s.of[a] = t
		t.stop = context.AfterFunc(a.e.ctx, func() {
			s.Lock()
			defer s.Unlock()
			if s.of[a] == t {
				delete(s.of, a)
			}
		})
	}
	f := t.ids[id]
	if f == nil {
		f = &accountsActFlight{}
		t.ids[id] = f
	}
	return f
}

// drop lets go of f, id's change in a, and of a's entry once it holds
// none. s is locked.
func (s *accountsActState) drop(a *accountsTab, id int, f *accountsActFlight) {
	t := s.of[a]
	if t == nil || t.ids[id] != f {
		return
	}
	delete(t.ids, id)
	if len(t.ids) == 0 {
		t.stop()
		delete(s.of, a)
	}
}

// accountsActListed lets go, at a listing, of each of a's answers that
// waited for one: from now on a step is taken from what is listed. A
// change still out is kept.
func accountsActListed(a *accountsTab) {
	s := &accountsActFlights
	s.Lock()
	defer s.Unlock()
	if t := s.of[a]; t != nil {
		for id, f := range t.ids {
			if !f.out {
				s.drop(a, id, f)
			}
		}
	}
}

// accountsActThreshold sets acct's threshold to to, or keeps it when to is
// 0, moved by by, within 1..100. While a change of it is out, this is kept
// for when that one is answered, a typed value replacing the steps pressed
// before it. Else it is sent now, from the threshold listed, or from the
// last answer when no listing has come since; one that changes nothing is
// not sent.
func accountsActThreshold(a *accountsTab, acct wire.Account, to, by int) tea.Cmd {
	if i := slices.IndexFunc(a.rows, func(r wire.Account) bool { return r.ID == acct.ID }); i >= 0 {
		acct = a.rows[i]
	}
	s := &accountsActFlights
	s.Lock()
	defer s.Unlock()
	f := s.flight(a, acct.ID)
	if f != nil && f.out {
		if to != 0 {
			f.to, f.by = to, 0
		}
		f.by += by
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
	f = s.hold(a, acct.ID)
	f.out = true
	return accountsActSend(a, acct, f, want)
}

// accountsActStep is to, or from when to is 0, moved by by, within 1..100.
func accountsActStep(from, to, by int) int {
	if to != 0 {
		from = to
	}
	return min(max(from+by, 1), 100)
}

// accountsActAnswer is how one change of a threshold was answered: with
// the threshold sent, as the server keeps a value from 1 to 100 as it is,
// or with why not.
type accountsActAnswer struct{ err error }

// accountsActSend is the command that sets acct's threshold to want, then
// sends what was pressed while that was out, one change at a time, each
// under an ordinary call's deadline. Once nothing is left it asks for the
// listing. A refusal ends it, drops what was pressed meanwhile, and is said.
func accountsActSend(a *accountsTab, acct wire.Account, f *accountsActFlight, want int) tea.Cmd {
	// The command reads the shell's env only through this copy, made in
	// Update.
	e, b, id, who := *a.e, a.e.b, acct.ID, accountsActWho(acct)
	s := &accountsActFlights
	return func() tea.Msg {
		for {
			v := want
			// callOr answers with what either function made, an answer.
			got, _ := e.callOr(envCallTimeout, func(ctx context.Context) tea.Msg {
				_, err := b.Change(ctx, id, Change{Threshold: &v})
				return accountsActAnswer{err}
			}, func(err error) tea.Msg { return accountsActAnswer{err} }, nil)().(accountsActAnswer)
			s.Lock()
			if got.err != nil {
				s.drop(a, id, f)
				s.Unlock()
				return accountsActRefused(fmt.Sprintf("the threshold of %s could not be set to %d%%", who, v), got.err)
			}
			f.now, want = v, v
			if f.to != 0 || f.by != 0 {
				want, f.to, f.by = accountsActStep(f.now, f.to, f.by), 0, 0
			}
			if want == f.now {
				f.out = false
				s.Unlock()
				return accountsActRefresh()
			}
			s.Unlock()
		}
	}
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
