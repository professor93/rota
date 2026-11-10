package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/wire"
)

// accountsActFake stands in for the server behind the Accounts tab's
// actions: its listing shows the thresholds it was sent, as the server's
// does, and a test can hold the next change or the next cached listing, or
// have changes refused.
type accountsActFake struct {
	mu       sync.Mutex
	rows     []wire.Account
	hold     chan struct{} // the next Change waits for it to close
	holdList chan struct{} // the next Accounts(none) waits for it to close
	no       error         // every Change is refused with it
	panics   string        // every Change panics with it
}

// accountsActModel is a model with the Accounts tab alone over an
// accountsActFake listing rows.
func accountsActModel(t *testing.T, rows []wire.Account) (*Model, *fakeBackend, *accountsActFake) {
	t.Helper()
	m, b := accountsModel(t, rows)
	f := &accountsActFake{rows: slices.Clone(rows)}
	b.AccountsFn, b.ChangeFn = f.accounts, f.change
	return m, b, f
}

// accountsActStart starts m at 80×24 with its first listing.
func accountsActStart(t *testing.T, m *Model) {
	t.Helper()
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	if len(accountsOf(m).rows) == 0 {
		t.Fatal("the first listing did not arrive")
	}
}

// holdNext makes the next Change wait until the channel it returns is
// closed.
func (f *accountsActFake) holdNext() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = make(chan struct{})
	return f.hold
}

// holdListing makes the next Accounts(none) wait until the channel it
// returns is closed.
func (f *accountsActFake) holdListing() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdList = make(chan struct{})
	return f.holdList
}

func (f *accountsActFake) refuse(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.no = err
}

func (f *accountsActFake) accounts(ctx context.Context, how Refresh) (Listing, error) {
	f.mu.Lock()
	hold := f.holdList
	if how == RefreshNone {
		f.holdList = nil
	} else {
		hold = nil
	}
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return Listing{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return Listing{Accounts: slices.Clone(f.rows)}, nil
}

func (f *accountsActFake) change(ctx context.Context, id int, c Change) (wire.Account, error) {
	f.mu.Lock()
	hold, no, panics := f.hold, f.no, f.panics
	f.hold = nil
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return wire.Account{}, ctx.Err()
		}
	}
	if panics != "" {
		panic(panics)
	}
	if no != nil {
		return wire.Account{}, no
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].ID != id {
			continue
		}
		if c.Threshold != nil {
			f.rows[i].Threshold = *c.Threshold
		}
		return f.rows[i], nil
	}
	return wire.Account{}, &client.Refusal{Status: 404, Message: fmt.Sprintf("no account %d", id)}
}

// accountsActChanges is every Change b took, as "1 order up" or
// "1 threshold 80", and "more" for any other field set.
func accountsActChanges(b *fakeBackend) []string {
	var out []string
	for _, c := range b.named("Change") {
		ch := c.Args[1].(Change)
		var said []string
		if ch.Order != nil {
			said = append(said, "order "+*ch.Order)
		}
		if ch.Threshold != nil {
			said = append(said, fmt.Sprintf("threshold %d", *ch.Threshold))
		}
		if ch.Cwd != nil || ch.ConfigDir != nil || ch.Sessions != nil || ch.RemoteControl != nil || ch.ForgetLong {
			said = append(said, "more")
		}
		out = append(out, fmt.Sprintf("%d %s", c.Args[0], strings.Join(said, ", ")))
	}
	return out
}

// accountsActListings is how the listings since the n-th were read.
func accountsActListings(b *fakeBackend, n int) []string {
	var out []string
	for _, c := range b.named("Accounts")[n:] {
		out = append(out, c.String())
	}
	return out
}

// accountsActSaid is the toasts among msgs, and how many bells.
func accountsActSaid(msgs []tea.Msg) (toasts []toastMsg, bells int) {
	for _, msg := range msgs {
		switch msg := msg.(type) {
		case toastMsg:
			toasts = append(toasts, msg)
		case bellMsg:
			bells++
		}
	}
	return toasts, bells
}

func TestMovesSendOnePlaceEach(t *testing.T) {
	m, b, _ := accountsActModel(t, accountsThree())
	accountsActStart(t, m)
	for _, c := range []struct{ key, want string }{
		{"K", "1 order up"}, {"shift+up", "1 order up"},
		{"J", "1 order down"}, {"shift+down", "1 order down"},
		{"u", "1 order first"},
	} {
		changes, lists := len(accountsActChanges(b)), len(b.named("Accounts"))
		_, seen := run(t, m, press(c.key)...)
		if got := accountsActChanges(b)[changes:]; !slices.Equal(got, []string{c.want}) {
			t.Errorf("%s sent %q, not %q", c.key, got, c.want)
		}
		// A move shifts the others, so the listing is read again, as it is.
		if got := accountsActListings(b, lists); !slices.Equal(got, []string{"Accounts(none)"}) {
			t.Errorf("after %s the listing was read %q", c.key, got)
		}
		if toasts, bells := accountsActSaid(seen); len(toasts) != 0 || bells != 0 {
			t.Errorf("%s said %v and rang %d", c.key, toasts, bells)
		}
	}
	// Moves are not coalesced: three presses are three places.
	changes := len(accountsActChanges(b))
	run(t, m, press("K", "K", "J")...)
	got := accountsActChanges(b)[changes:]
	slices.Sort(got)
	if !slices.Equal(got, []string{"1 order down", "1 order up", "1 order up"}) {
		t.Errorf("K K J sent %q", got)
	}
	// The selected account is the one moved.
	run(t, m, press("down", "u")...)
	if got := accountsActChanges(b); got[len(got)-1] != "2 order first" {
		t.Errorf("u on the second account sent %q", got[len(got)-1])
	}
	// last and out are moves too, the words x sends.
	a := accountsOf(m)
	changes = len(accountsActChanges(b))
	run(t, m, tea.BatchMsg{a.move(a.rows[2], "last"), a.move(a.rows[2], "out")})
	got = accountsActChanges(b)[changes:]
	slices.Sort(got)
	if !slices.Equal(got, []string{"3 order last", "3 order out"}) {
		t.Errorf("last and out sent %q", got)
	}
}

func TestNudgesCoalesce(t *testing.T) {
	m, b, f := accountsActModel(t, accountsThree())
	accountsActStart(t, m)
	a := accountsOf(m)
	// work is at 75. Ten presses while the first change is out send it,
	// then one change with the nine after it summed: 75+50, at most 100.
	release := f.holdNext()
	run(t, m, press(slices.Repeat([]string{"+"}, 9)...)...)
	// A listing while it is out lets go of nothing: the tenth press waits
	// too.
	run(t, m, refreshMsg{How: RefreshNone})
	run(t, m, press("+")...)
	if got := accountsActChanges(b); !slices.Equal(got, []string{"1 threshold 80"}) {
		t.Fatalf("while the first change is out, ten presses sent %q", got)
	}
	lists := len(b.named("Accounts"))
	close(release)
	_, seen := await(t, m)
	if got := accountsActChanges(b); !slices.Equal(got, []string{"1 threshold 80", "1 threshold 100"}) {
		t.Errorf("ten presses sent %q", got)
	}
	if got := accountsActListings(b, lists); !slices.Equal(got, []string{"Accounts(none)"}) {
		t.Errorf("after the last answer the listing was read %q", got)
	}
	if a.rows[0].Threshold != 100 {
		t.Errorf("the table shows a threshold of %d", a.rows[0].Threshold)
	}
	if toasts, bells := accountsActSaid(seen); len(toasts) != 0 || bells != 0 {
		t.Errorf("the steps said %v and rang %d", toasts, bells)
	}

	// At 100, + sends nothing; - steps down from what is listed.
	run(t, m, press("+", "=")...)
	run(t, m, press("-")...)
	if got := accountsActChanges(b)[2:]; !slices.Equal(got, []string{"1 threshold 95"}) {
		t.Errorf("+ = - at 100 sent %q", got)
	}

	// A step taken after an answer, before a listing shows it, starts
	// from the answer and not from the table.
	listed := f.holdListing()
	run(t, m, press("-")...) // 90, answered; its listing held
	if a.rows[0].Threshold != 95 {
		t.Fatalf("the table moved on before its listing: %d", a.rows[0].Threshold)
	}
	run(t, m, press("-")...) // 85, from the answer of 90
	close(listed)
	await(t, m)
	run(t, m) // whatever the last answer asked
	if got := accountsActChanges(b)[3:]; !slices.Equal(got, []string{"1 threshold 90", "1 threshold 85"}) {
		t.Errorf("two steps around a listing held sent %q", got)
	}
	if a.rows[0].Threshold != 85 {
		t.Errorf("the table ends at %d, not 85", a.rows[0].Threshold)
	}
	// Once a listing has come, a step is taken from the table again: the
	// answer is not kept past it.
	f.mu.Lock()
	f.rows[0].Threshold = 40 // another terminal set it
	f.mu.Unlock()
	run(t, m, refreshMsg{How: RefreshNone})
	run(t, m, press("+")...)
	if got := accountsActChanges(b); got[len(got)-1] != "1 threshold 45" {
		t.Errorf("after another terminal set 40, + sent %q", got[len(got)-1])
	}

	// A threshold typed while a change is out replaces the steps pressed
	// before it, and the steps after it add to it.
	release = f.holdNext()
	run(t, m, press("+", "+", "%")...) // 50 out; one step waiting; the field opens
	run(t, m, press("3", "0", "enter", "+", "-", "+")...)
	close(release)
	await(t, m)
	run(t, m)
	n := len(accountsActChanges(b))
	if got := accountsActChanges(b)[n-2:]; !slices.Equal(got, []string{"1 threshold 50", "1 threshold 35"}) {
		t.Errorf("a typed 30 between steps sent %q", got)
	}

	// Steps stop at 1, and another account's steps go on beside them.
	f.mu.Lock()
	f.rows[0].Threshold = 3
	f.mu.Unlock()
	run(t, m, refreshMsg{How: RefreshNone})
	release = f.holdNext()
	run(t, m, press("-")...)               // 1 out, held
	run(t, m, press("down", "-", "up")...) // home, 100 to 95, beside it
	close(release)
	await(t, m)
	run(t, m)
	run(t, m, press("-")...) // at 1 already
	got := accountsActChanges(b)[n:]
	slices.Sort(got)
	if !slices.Equal(got, []string{"1 threshold 1", "2 threshold 95"}) {
		t.Errorf("steps at 3 and beside another account sent %q", got)
	}
	if left := accountsActLeft(a); len(left) != 0 {
		t.Errorf("after every answer was listed, changes are still kept for %v", left)
	}
}

// accountsActLeft is the ids a's kept changes are for.
func accountsActLeft(a *accountsTab) []int {
	s := &accountsActFlights
	s.Lock()
	defer s.Unlock()
	var ids []int
	if t := s.of[a]; t != nil {
		for id := range t.ids {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func TestTypedThresholdChecksItsValue(t *testing.T) {
	m, b, f := accountsActModel(t, accountsThree())
	accountsActStart(t, m)
	const bad = "a threshold is a whole percentage from 1 to 100"
	open := func() *accountsActTyped {
		t.Helper()
		run(t, m, press("%")...)
		if len(m.stack) != 1 {
			t.Fatalf("%% opened %d overlays", len(m.stack))
		}
		o, ok := m.stack[0].(*accountsActTyped)
		if !ok {
			t.Fatalf("%% opened a %T", m.stack[0])
		}
		return o
	}
	o := open()
	if kind, id := o.About(); kind != "threshold" || id != 1 || !o.Typing() {
		t.Errorf("the field is about %s %d, typing %v", kind, id, o.Typing())
	}
	view := plain(m)
	for _, want := range []string{"Threshold of claude/work@example.com", "now 75%", "threshold:", "⏎ apply", "esc cancel"} {
		if !strings.Contains(view, want) {
			t.Errorf("the field does not say %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, bad) {
		t.Errorf("the field complains before anything was typed:\n%s", view)
	}
	// Nothing, 0, 101 and letters are refused where they are typed, and
	// nothing is sent.
	for _, keys := range [][]string{
		{"enter"},
		{"0", "enter"},
		{"backspace", "1", "0", "1", "enter"},
		{"backspace", "backspace", "backspace", "q"},
		{"?"},
		{"space"},
	} {
		run(t, m, press(keys...)...)
		if !strings.Contains(plain(m), bad) || o.Closed() {
			t.Errorf("after %q the field says nothing, or closed:\n%s", keys, plain(m))
		}
	}
	// A digit typed puts the complaint away.
	run(t, m, press("8")...)
	if strings.Contains(plain(m), bad) {
		t.Errorf("a digit did not put the complaint away:\n%s", plain(m))
	}
	// A paste takes the place of what was typed, trimmed and without its %,
	// and a paste that is no number, a key above all, changes nothing and
	// is never drawn.
	const key = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	run(t, m, tea.PasteMsg{Content: key})
	if o.in.Value() != "8" || !strings.Contains(plain(m), bad) || strings.Contains(m.View().Content, "AAAA") {
		t.Errorf("a key pasted made the field %q:\n%s", o.in.Value(), plain(m))
	}
	run(t, m, press("8")...)
	run(t, m, tea.PasteMsg{Content: " %\r\n"})
	if o.in.Value() != "88" || !strings.Contains(plain(m), bad) {
		t.Errorf("a paste of nothing made the field %q:\n%s", o.in.Value(), plain(m))
	}
	run(t, m, tea.PasteMsg{Content: " 85%\r\n"})
	if o.in.Value() != "85" || strings.Contains(plain(m), bad) {
		t.Errorf("a pasted 85%% made the field %q:\n%s", o.in.Value(), plain(m))
	}
	if got := accountsActChanges(b); len(got) != 0 {
		t.Fatalf("the field sent %q before enter", got)
	}
	_, seen := run(t, m, press("enter")...)
	if got := accountsActChanges(b); !slices.Equal(got, []string{"1 threshold 85"}) || !o.Closed() || len(m.stack) != 0 {
		t.Errorf("enter sent %q, closed %v", got, o.Closed())
	}
	if toasts, _ := accountsActSaid(seen); len(toasts) != 0 {
		t.Errorf("a threshold taken said %v", toasts)
	}
	// esc closes it, sending nothing; the value the account has already
	// is not sent again.
	open()
	run(t, m, press("5", "esc")...)
	o = open()
	if !strings.Contains(plain(m), "now 85%") {
		t.Errorf("the field does not say the listed 85%%:\n%s", plain(m))
	}
	run(t, m, press("8", "5", "enter")...)
	if got := accountsActChanges(b); len(got) != 1 || !o.Closed() {
		t.Errorf("esc and the same value sent %q", got)
	}
	// The field follows the listing while it is open: what another
	// terminal set is said, and a value is judged against it.
	o = open()
	f.mu.Lock()
	f.rows[0].Threshold = 60
	f.mu.Unlock()
	run(t, m, refreshMsg{How: RefreshNone})
	if !strings.Contains(plain(m), "now 60%") {
		t.Errorf("the field does not say the 60%% listed since it opened:\n%s", plain(m))
	}
	run(t, m, press("8", "5", "enter")...)
	if got := accountsActChanges(b); !slices.Equal(got, []string{"1 threshold 85", "1 threshold 85"}) || !o.Closed() {
		t.Errorf("85 typed over a listed 60 sent %q", got)
	}
	// 100 and 1 are taken, and so is 007, which is 7.
	for _, typed := range []string{"100", "1", "007"} {
		open()
		run(t, m, press(strings.Split(typed, "")...)...)
		run(t, m, press("enter")...)
	}
	if got := accountsActChanges(b)[2:]; !slices.Equal(got, []string{"1 threshold 100", "1 threshold 1", "1 threshold 7"}) {
		t.Errorf("100, 1 and 007 sent %q", got)
	}
	// Opened twice before the first arrives, it opens once.
	run(t, m, press("%", "%")...)
	if len(m.stack) != 1 {
		t.Errorf("%% %% opened %d fields", len(m.stack))
	}
}

// The field is drawn within its width and height in either East Asian
// mode, whatever the account is called.
func TestAccountsActTheFieldFitsItsSpace(t *testing.T) {
	rows := accountsThree()
	rows[0].Provider = "claude\x1b]0;x\a"
	rows[0].Email = "\x1b[2J" + strings.Repeat("日本語のアカウント", 12) + "@example.com"
	m, _, _ := accountsActModel(t, rows)
	accountsActStart(t, m)
	o := accountsOf(m).typedThreshold(rows[0]).(*accountsActTyped)
	o.bad = true
	if view := o.View(0, 10); view != "" {
		t.Errorf("with no width the field drew %q", view)
	}
	for w := 1; w <= 130; w++ {
		for _, h := range []int{0, 1, 3, 8, 30} {
			view := o.View(w, h)
			lines := strings.Split(view, "\n")
			if h > 0 && len(lines) > h || h == 0 && view != "" {
				t.Fatalf("at %d×%d the field drew %d lines", w, h, len(lines))
			}
			for _, l := range lines {
				if wc, gr := accountsWidths(l); wc > w || gr > w {
					t.Fatalf("at %d×%d a line is %d (%d) wide: %q", w, h, wc, gr, ansi.Strip(l))
				}
				if s := ansi.Strip(l); strings.ContainsAny(s, "\x1b\a") || strings.Contains(s, "]0;") {
					t.Fatalf("at %d×%d a line holds a control: %q", w, h, s)
				}
			}
		}
	}
	if view := ansi.Strip(o.View(80, 10)); !strings.Contains(view, "Threshold of claude/日本語") || !strings.Contains(view, "…") {
		t.Errorf("the title is not the cleaned label, cut:\n%s", view)
	}
}

// TestAccountsActInBothEastAsianModes runs the field's fit again in the
// mode this run is not in, as TestAccountsInBothEastAsianModes does.
func TestAccountsActInBothEastAsianModes(t *testing.T) {
	if os.Getenv(textModeVar) != "" {
		t.Skip("a child run")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mode := "wide"
	if textEastAsian() {
		mode = "narrow"
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "RUNEWIDTH_EASTASIAN=") && !strings.HasPrefix(kv, textModeVar+"=") && !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	env = append(env, textModeVar+"="+mode, "PATH="+os.Getenv("PATH"))
	if mode == "wide" {
		env = append(env, "RUNEWIDTH_EASTASIAN=1")
	}
	tests := "^(TestTextModeIsTheOneAsked|TestAccountsActTheFieldFitsItsSpace|TestTheBellRingsOnceAndRearms)$"
	cmd := exec.Command(exe, "-test.run", tests, "-test.count=1", "-test.v", "-test.timeout=5m")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the %s run failed: %v\n%s", mode, err, out)
	}
	for _, name := range []string{"TestTextModeIsTheOneAsked", "TestAccountsActTheFieldFitsItsSpace", "TestTheBellRingsOnceAndRearms"} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("the %s run did not pass %s:\n%s", mode, name, out)
		}
	}
}

func TestExcludeAndPutBack(t *testing.T) {
	m, b, _ := accountsActModel(t, accountsThree())
	accountsActStart(t, m)
	lists := len(b.named("Accounts"))
	run(t, m, press("x")...)
	if got := accountsActChanges(b); !slices.Equal(got, []string{"1 order out"}) {
		t.Errorf("x on an account in the rotation sent %q", got)
	}
	if got := accountsActListings(b, lists); !slices.Equal(got, []string{"Accounts(none)"}) {
		t.Errorf("after x the listing was read %q", got)
	}
	rows := accountsThree()
	rows[0].Order = 0
	run(t, m, accountsListing(rows...))
	run(t, m, press("x")...)
	if got := accountsActChanges(b); !slices.Equal(got, []string{"1 order out", "1 order last"}) {
		t.Errorf("x on an account out of the rotation sent %q", got)
	}
	// u puts one that is out first, by the same move.
	run(t, m, press("u")...)
	if got := accountsActChanges(b); got[len(got)-1] != "1 order first" {
		t.Errorf("u on an account out of the rotation sent %q", got[len(got)-1])
	}
}

func TestTheBellRingsOnceAndRearms(t *testing.T) {
	at := func(id int, email string, threshold int, pct float64) wire.Account {
		return wire.Account{ID: id, Provider: "claude", Email: email, Status: "ok", Order: id, Threshold: threshold,
			Percent: pct, Metered: true, CheckedAt: accountsAt(-time.Minute),
			Windows: []wire.Window{
				{Name: "5h", Percent: pct, ResetsAt: accountsAt(time.Hour), Primary: true},
				{Name: "7d", Percent: pct / 3, ResetsAt: accountsAt(48 * time.Hour)},
			}}
	}
	listings := func(t *testing.T, ls ...[]wire.Account) (bells []int, toasts [][]toastMsg) {
		t.Helper()
		m, _ := accountsModel(t, nil)
		for _, rows := range ls {
			_, seen := run(t, m, accountsListing(rows...))
			said, rang := accountsActSaid(seen)
			bells, toasts = append(bells, rang), append(toasts, said)
		}
		return bells, toasts
	}
	t.Run("once, and again after it falls back", func(t *testing.T) {
		var ls [][]wire.Account
		for _, pct := range []float64{70, 85, 90, 70, 85} {
			ls = append(ls, []wire.Account{at(1, "a@b", 80, pct)})
		}
		bells, toasts := listings(t, ls...)
		if !slices.Equal(bells, []int{0, 1, 0, 0, 1}) {
			t.Errorf("listings at 70, 85, 90, 70, 85 against 80 rang %v", bells)
		}
		for _, i := range []int{1, 4} {
			want := toastMsg{Text: "claude/a@b crossed its 80% (5h 85%)", Level: levelWarn}
			if len(toasts[i]) != 1 || toasts[i][0] != want {
				t.Errorf("listing %d said %v, not %v", i+1, toasts[i], want)
			}
		}
		for _, i := range []int{0, 2, 3} {
			if len(toasts[i]) != 0 {
				t.Errorf("listing %d said %v", i+1, toasts[i])
			}
		}
	})
	t.Run("the first listing is no crossing", func(t *testing.T) {
		bells, _ := listings(t, []wire.Account{at(1, "a@b", 80, 85)}, []wire.Account{at(1, "a@b", 80, 90)})
		if !slices.Equal(bells, []int{0, 0}) {
			t.Errorf("listings at 85 and 90 rang %v", bells)
		}
	})
	t.Run("at the threshold is over it, and the percent is floored", func(t *testing.T) {
		var ls [][]wire.Account
		for _, pct := range []float64{79.99, 80, 85, 79.5, 99.9} {
			ls = append(ls, []wire.Account{at(1, "a@b", 80, pct)})
		}
		bells, toasts := listings(t, ls...)
		if !slices.Equal(bells, []int{0, 1, 0, 0, 1}) {
			t.Errorf("79.99, 80, 85, 79.5, 99.9 rang %v", bells)
		}
		if len(toasts[4]) != 1 || toasts[4][0].Text != "claude/a@b crossed its 80% (5h 99%)" {
			t.Errorf("at 99.9 it said %v", toasts[4])
		}
	})
	t.Run("two at once ring one bell and say both", func(t *testing.T) {
		bells, toasts := listings(t,
			[]wire.Account{at(1, "a@b", 80, 10), at(2, "c@d", 50, 40), at(3, "e@f", 90, 10)},
			[]wire.Account{at(1, "a@b", 80, 81), at(2, "c@d", 50, 60), at(3, "e@f", 90, 89)})
		if !slices.Equal(bells, []int{0, 1}) || len(toasts[1]) != 2 {
			t.Fatalf("two crossings rang %v and said %v", bells, toasts[1])
		}
		if toasts[1][0].Text != "claude/a@b crossed its 80% (5h 81%)" || toasts[1][1].Text != "claude/c@d crossed its 50% (5h 60%)" {
			t.Errorf("two crossings said %v", toasts[1])
		}
	})
	t.Run("what is no crossing rings nothing", func(t *testing.T) {
		unread := at(2, "c@d", 80, 0)
		unread.CheckedAt = ""
		key := at(3, "k@k", 80, 0)
		key.Metered, key.Credential = false, "apikey"
		keyOver := key
		keyOver.Percent = 95
		lowered := at(4, "g@h", 90, 85)
		loweredAfter := at(4, "g@h", 80, 85)
		// An account read before and not metered now, and one not metered
		// before (its reading is no usage) and metered now.
		unmetered := at(6, "u@u", 80, 95)
		unmetered.Metered = false
		wasUnmetered := at(7, "w@w", 80, 0)
		wasUnmetered.Metered = false
		bells, toasts := listings(t,
			[]wire.Account{unread, key, lowered, at(6, "u@u", 80, 10), wasUnmetered},
			// a first reading, an account first listed now, an API key, and
			// a threshold lowered under the usage: the usage moved past nothing.
			[]wire.Account{at(2, "c@d", 80, 95), at(5, "n@n", 80, 95), keyOver, loweredAfter, unmetered, at(7, "w@w", 80, 95)})
		if !slices.Equal(bells, []int{0, 0}) {
			t.Errorf("rang %v and said %v", bells, toasts)
		}
	})
	t.Run("what it says is cleaned", func(t *testing.T) {
		const key = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		hostile := func(pct float64) wire.Account {
			a := at(1, "\x1b[2Jevil "+key+"@x", 80, pct)
			a.Provider = "claude\x1b]0;t\a"
			a.Windows[0].Name = "5h\x1b[31m"
			return a
		}
		_, toasts := listings(t, []wire.Account{hostile(10)}, []wire.Account{hostile(90)})
		if len(toasts[1]) != 1 {
			t.Fatalf("a hostile crossing said %v", toasts[1])
		}
		if s := toasts[1][0].Text; strings.ContainsAny(s, "\x1b\a") || strings.Contains(s, "AAAA") || !strings.Contains(s, "crossed its 80% (5h 90%)") {
			t.Errorf("a hostile crossing said %q", s)
		}
	})
}

func TestARefusedMoveIsAToast(t *testing.T) {
	const key = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	m, b, f := accountsActModel(t, accountsThree())
	accountsActStart(t, m)
	saidOnce := func(t *testing.T, seen []tea.Msg, prefix string) {
		t.Helper()
		toasts, _ := accountsActSaid(seen)
		if len(toasts) != 1 {
			t.Fatalf("said %v, not one toast", toasts)
		}
		s := toasts[0]
		if s.Level != levelErr || !strings.HasPrefix(s.Text, prefix) || strings.ContainsAny(s.Text, "\x1b\a\r\n") || strings.Contains(s.Text, "AAAA") {
			t.Errorf("said %+v, not a cleaned error after %q", s, prefix)
		}
	}
	f.refuse(&client.Refusal{Status: 400, Reason: "invalid", Message: "\x1b[2Jaccount 1 is out of the rotation;\r\ngive it a place first " + key})
	lists := len(b.named("Accounts"))
	_, seen := run(t, m, press("K")...)
	saidOnce(t, seen, "claude/work@example.com could not move up: account 1 is out of the rotation; give it a place first sk-ant-")
	// The listing is read again, as it now is.
	if got := accountsActListings(b, lists); !slices.Equal(got, []string{"Accounts(none)"}) {
		t.Errorf("after a refusal the listing was read %q", got)
	}
	for _, c := range []struct{ key, what string }{
		{"J", "could not move down"}, {"u", "could not go first"}, {"x", "could not leave the rotation"},
	} {
		_, seen := run(t, m, press(c.key)...)
		saidOnce(t, seen, "claude/work@example.com "+c.what+": ")
	}

	// A refused step is said, and the steps pressed while it was out go
	// with it: the next press starts again from what is listed.
	release := f.holdNext()
	run(t, m, press("+", "+", "+")...)
	close(release)
	_, seen = await(t, m)
	saidOnce(t, seen, "the threshold of claude/work@example.com could not be set to 80%: account 1 is out")
	changes := len(accountsActChanges(b))
	f.refuse(nil)
	run(t, m, press("+")...)
	if got := accountsActChanges(b)[changes:]; !slices.Equal(got, []string{"1 threshold 80"}) {
		t.Errorf("after a refused step, + sent %q", got)
	}
	if left := accountsActLeft(accountsOf(m)); len(left) != 0 {
		t.Errorf("changes are still kept for %v", left)
	}

	// A call that fails without an answer is said too, and so is an
	// account gone from the server.
	f.mu.Lock()
	f.panics = "boom"
	f.mu.Unlock()
	_, seen = run(t, m, press("J")...)
	saidOnce(t, seen, "claude/work@example.com could not move down: the call failed: boom")
	f.mu.Lock()
	f.panics, f.rows = "", f.rows[1:]
	f.mu.Unlock()
	_, seen = run(t, m, press("-")...)
	saidOnce(t, seen, "the threshold of claude/work@example.com could not be set to 75%: no account 1")
}

// Whatever is kept for a tab's changes goes when its program ends, even
// with an answer no listing has shown yet.
func TestAccountsActForgetsWithTheProgram(t *testing.T) {
	m, _, f := accountsActModel(t, accountsThree())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.e.ctx = ctx
	accountsActStart(t, m)
	a := accountsOf(m)
	listed := f.holdListing()
	run(t, m, press("+")...)
	if left := accountsActLeft(a); !slices.Equal(left, []int{1}) {
		t.Fatalf("an answer not yet listed is kept for %v", left)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for len(accountsActLeft(a)) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if left := accountsActLeft(a); len(left) != 0 {
		t.Errorf("after the program ended, changes are kept for %v", left)
	}
	close(listed)
	await(t, m)
}

func TestActUpdateDoesNoIO(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	m, b, _ := accountsActModel(t, accountsThree())
	rows := accountsThree()
	over := slices.Clone(rows)
	over[1].Percent, over[1].Windows[0].Percent = 100, 100
	a := accountsOf(m)
	msgs := []tea.Msg{size(80, 24), accountsListing(rows...), modelWhoMsg{Who: Principal{Role: "control"}},
		accountsListing(over...), accountsListing(rows...)}
	for _, k := range []string{"K", "shift+up", "J", "shift+down", "u", "x", "+", "=", "-", "+", "%"} {
		msgs = append(msgs, press(k)[0])
	}
	assertNoIO(t, b, func() {
		m.Init()
		for _, msg := range msgs {
			m.Update(msg)
			m.View()
		}
		// The typed field, opened as the shell opens it.
		m.Update(openMsg{O: a.typedThreshold(rows[1])})
		for _, msg := range append(press("4", "x", "backspace", "0", "enter"), tea.PasteMsg{Content: "55"}) {
			m.Update(msg)
			m.View()
		}
		m.Update(openMsg{O: a.typedThreshold(rows[2])})
		for _, msg := range append([]tea.Msg{tea.PasteMsg{Content: "5"}}, press("enter", "esc")...) {
			m.Update(msg)
			m.View()
		}
		if cmd := a.crossed(rows, over); cmd == nil {
			t.Error("a crossing gave no command")
		}
	})
}
