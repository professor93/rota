package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/wire"
)

// accountsNow is the tests' clock: Tuesday 2026-11-10 20:17 UTC.
var accountsNow = time.Date(2026, 11, 10, 20, 17, 0, 0, time.UTC)

// accountsAt is accountsNow moved by d, as the API writes a time.
func accountsAt(d time.Duration) string { return accountsNow.Add(d).Format(time.RFC3339) }

// accountsThree are the design's three accounts: work at 62% and 21% under
// its own threshold of 75, home at 12% and 7%, and a dead codex account at
// 95% whose reading is three hours old.
func accountsThree() []wire.Account {
	return []wire.Account{
		{ID: 1, Provider: "claude", Email: "work@example.com", Status: "ok", Order: 1, Threshold: 75, Percent: 62,
			Metered: true, CheckedAt: accountsAt(-2 * time.Minute), LoginUntil: accountsAt(20 * 24 * time.Hour),
			Windows: []wire.Window{
				{Name: "5h", Percent: 62, ResetsAt: accountsAt(time.Hour + 3*time.Minute), Primary: true},
				{Name: "7d", Percent: 21, ResetsAt: accountsAt(4*24*time.Hour + 2*time.Hour)},
			}},
		{ID: 2, Provider: "claude", Email: "home@example.com", Status: "ok", Order: 2, Threshold: 100, Percent: 12,
			Metered: true, CheckedAt: accountsAt(-time.Minute),
			Windows: []wire.Window{
				{Name: "5h", Percent: 12, ResetsAt: accountsAt(4*time.Hour + 10*time.Minute), Primary: true},
				{Name: "7d", Percent: 7, ResetsAt: accountsAt(6*24*time.Hour + 3*time.Hour)},
			}},
		{ID: 3, Provider: "codex", Email: "cdx@example.com", Status: "reauth", Order: 3, Threshold: 100, Percent: 95,
			Metered: true, CheckedAt: accountsAt(-3 * time.Hour),
			DeadReason: "the provider refused the refresh token: invalid_grant",
			Windows: []wire.Window{
				{Name: "5h", Percent: 95, ResetsAt: accountsAt(2 * time.Hour), Primary: true},
			}},
	}
}

// accountsModel is a model with the Accounts tab alone, on the tests'
// clock, over a backend whose listing is rows.
func accountsModel(t *testing.T, rows []wire.Account, with ...func(*Options)) (*Model, *fakeBackend) {
	t.Helper()
	shellRegistry(t)
	register(tabsAccounts, newAccounts)
	b := &fakeBackend{AccountsFn: func(context.Context, Refresh) (Listing, error) {
		return Listing{Accounts: rows}, nil
	}}
	return shellModel(t, b, &fakeTicks{clock: accountsNow}, with...), b
}

// accountsWatch makes Who answer a watch principal.
func accountsWatch(b *fakeBackend) {
	b.WhoFn = func(context.Context) (Principal, error) {
		return Principal{Name: "w", Role: "watch", Via: "ssh"}, nil
	}
}

func accountsOf(m *Model) *accountsTab { return m.tabs[0].(*accountsTab) }

// accountsPlain is a view as text: escapes stripped, and each line's
// trailing spaces, which a golden would not keep, gone.
func accountsPlain(view string) string {
	lines := strings.Split(ansi.Strip(view), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n") + "\n"
}

// accountsMarked is the table's rows drawn in the Selected style, as text.
func accountsMarked(m *Model) []string {
	a := accountsOf(m)
	mark := m.e.th.Selected.Render("x")
	mark = mark[:strings.Index(mark, "x")]
	var out []string
	for _, l := range strings.Split(a.View(80, 22), "\n")[1 : 1+a.shown] {
		if strings.Contains(l, mark) {
			out = append(out, ansi.Strip(l))
		}
	}
	return out
}

// accountsListing is a listing as the shell hands it on.
func accountsListing(rows ...wire.Account) listingMsg {
	return listingMsg{Listing: Listing{Accounts: rows}, How: RefreshNone}
}

func TestTheTableAtThreeSizes(t *testing.T) {
	for _, sz := range [][2]int{{60, 20}, {80, 24}, {120, 40}} {
		t.Run(fmt.Sprintf("%dx%d", sz[0], sz[1]), func(t *testing.T) {
			m, _ := accountsModel(t, accountsThree())
			run(t, m, tea.BatchMsg{m.Init()}, size(sz[0], sz[1]))
			view := accountsOf(m).View(sz[0], sz[1])
			if n := strings.Count(view, "\n") + 1; n > sz[1] {
				t.Errorf("the view has %d lines in %d rows", n, sz[1])
			}
			golden(t, accountsGolden(sz[0], sz[1]), accountsPlain(view))
			// However few rows the tab is given, it draws no more.
			for h := 1; h <= sz[1]; h++ {
				if n := strings.Count(accountsOf(m).View(sz[0], h), "\n") + 1; n > h {
					t.Errorf("the view has %d lines in %d rows", n, h)
				}
			}
		})
	}
}

// accountsGolden is the golden of the table at w×h in this run's East Asian
// mode: where ambiguous characters are drawn wide (RUNEWIDTH_EASTASIAN=1)
// a bar, the rule and a cut label are drawn otherwise, so each mode has its
// own.
func accountsGolden(w, h int) string {
	name := fmt.Sprintf("accounts-%dx%d", w, h)
	if textEastAsian() {
		name += "-wide"
	}
	return name
}

// TestAccountsInBothEastAsianModes runs the Accounts tests again in the
// mode this run is not in, as TestTextBothEastAsianModes does for the text:
// so the table, the rule and the goldens hold where ambiguous characters
// are drawn wide as well as narrow.
func TestAccountsInBothEastAsianModes(t *testing.T) {
	if os.Getenv(textModeVar) != "" {
		t.Skip("a child run")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := "^(TestTextModeIsTheOneAsked|TestTheTableAtThreeSizes|TestALabelNeverEscapesItsCell|" +
		"TestSelectionFollowsTheAccountNotTheRow|TestDetailsShowEveryWindowAndSetting|TestStaleReadingsFade|" +
		"TestNoReflowWhenASessionStarts|TestKeysReachTheirActions|TestWatchSeesNoChangeKeys|TestManyAccountsScroll|" +
		"TestAClickSelectsARowAndTheWheelScrolls|TestAccountsUpdateDoesNoIO|TestAccountsRingsTheBellAfterTheFirstListing|" +
		"TestAccountsEmptyListingSaysHowToAdd|TestAccountsStateSaysWhy|TestAccountsStateIsNeverCut|" +
		"TestAccountsSaysWhyTheAccountsCannotBeRead|TestAccountsTheSameOverlayOpensOnce|TestAccountsALabelFromAUUIDIsCutAsItIsDrawn)$"
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
	// The child is started by its own path, and PATH is the empty directory
	// TestMain made.
	env = append(env, textModeVar+"="+mode, "PATH="+os.Getenv("PATH"))
	if mode == "wide" {
		env = append(env, "RUNEWIDTH_EASTASIAN=1")
	}
	cmd := exec.Command(exe, "-test.run", tests, "-test.count=1", "-test.v", "-test.timeout=10m")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the %s run failed: %v\n%s", mode, err, out)
	}
	for _, name := range []string{"TestTextModeIsTheOneAsked", "TestTheTableAtThreeSizes", "TestALabelNeverEscapesItsCell"} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("the %s run did not pass %s:\n%s", mode, name, out)
		}
	}
}

// accountsWidths is the width of s by both of the measures a terminal may
// lay it out by: rune by rune, and grapheme by grapheme.
func accountsWidths(s string) (wc, gr int) { return ansi.StringWidthWc(s), ansi.StringWidth(s) }

// accountsCut is s split at display column at: what comes before, which
// must be exactly at columns wide, and the rest. ok is false when a
// character straddles the column.
func accountsCut(s string, at int) (head, rest string, ok bool) {
	used, i := 0, 0
	for used < at && i < len(s) {
		c, w := ansi.FirstGraphemeCluster(s[i:], ansi.WcWidth)
		used += w
		i += len(c)
	}
	return s[:i], s[i:], used == at
}

func TestALabelNeverEscapesItsCell(t *testing.T) {
	const key = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	labels := []string{"\x1b[2Jevil", "日本語のアカウント", "👨‍👩‍👧 family",
		"\u009b31m" + strings.Repeat("a label far too long for any cell ", 4), "p"}
	rows := accountsThree()
	rows = append(rows, wire.Account{ID: 4, Provider: "claude\x1b[31m", Status: "reauth", Order: 4, Metered: true,
		Cwd: "/srv/\x1b]52;c;ZXZpbA==\x07" + strings.Repeat("deep/", 30), Note: "quota\r\nnote\x1b[1m " + key,
		DeadReason: "the server said: \x1b]0;title\x07refused " + key + "\n" + strings.Repeat("and more ", 20),
		Windows: []wire.Window{{Name: "\x1b]0;x\a" + strings.Repeat("Claude Opus ", 6), Percent: 50,
			ResetsAt: accountsAt(time.Hour), Scoped: true}}},
		wire.Account{ID: 5, Provider: strings.Repeat("provider", 20), Status: "ok", Order: 5})
	for i := range rows {
		rows[i].Name = labels[i]
	}
	for _, sz := range [][2]int{{60, 20}, {80, 24}, {120, 40}} {
		w, h := sz[0], sz[1]
		t.Run(fmt.Sprintf("%dx%d", w, h), func(t *testing.T) {
			m, _ := accountsModel(t, rows)
			run(t, m, tea.BatchMsg{m.Init()}, size(w, h))
			// With each account selected in turn, so its details are drawn
			// too.
			for sel := range rows {
				styled := accountsOf(m).View(w, h)
				// Without colour the theme writes no colour of its own, so
				// a 31m on the screen could only be the provider's.
				for _, bad := range []string{"\x1b[2J", "\x1b]", "\x1b[31m", "\u009b", "\a", "\r", key[len(key)-20:]} {
					if strings.Contains(styled, bad) {
						t.Errorf("with %d selected, %q reached the screen", sel+1, bad)
					}
				}
				lines := strings.Split(ansi.Strip(styled), "\n")
				// The bold the note asked for went whole, beside the space
				// that was there, so it was never taken as bold.
				if sel == 3 && w == 120 && !strings.Contains(ansi.Strip(styled), "note            quota note sk-ant-") {
					t.Errorf("the note is not shown as the text it holds:\n%s", ansi.Strip(styled))
				}
				// A label too long for the title is cut, not the provider
				// and the id after it.
				if sel == 3 && !strings.Contains(ansi.Strip(styled), "…  claude  id 4\n") {
					t.Errorf("the title of a long label lost its provider or id:\n%s", ansi.Strip(styled))
				}
				for _, l := range lines {
					if strings.ContainsRune(l, '\x1b') {
						t.Errorf("an ESC is left in %q", l)
					}
					if wc, gr := accountsWidths(l); wc > w || gr > w {
						t.Errorf("%q is %d columns rune by rune and %d by graphemes, in %d", l, wc, gr, w)
					}
				}
				if sel == 0 {
					accountsColumns(t, accountsLayout(w), lines[:len(rows)+1], labels)
				}
				run(t, m, press("down")...)
			}
		})
	}
}

// accountsColumns checks that every column of the table starts where the
// layout says, on the header and on every row, by both measures, with the
// gap before it blank, and that each row's account cell is its label.
func accountsColumns(t *testing.T, cols []col, lines, labels []string) {
	t.Helper()
	for r, line := range lines {
		at := 0
		for _, c := range cols {
			if !c.Shown {
				continue
			}
			head, rest, ok := accountsCut(line, at)
			if !ok {
				t.Fatalf("line %d: a character straddles column %s at %d: %q", r, c.Key, at, line)
			}
			if wc, gr := accountsWidths(head); wc != at || gr != at {
				t.Errorf("line %d: column %s starts at %d rune by rune and %d by graphemes, not %d", r, c.Key, wc, gr, at)
			}
			if at > 0 && !strings.HasSuffix(head, "  ") {
				t.Errorf("line %d: no gap before column %s: %q", r, c.Key, head)
			}
			cell, _, _ := accountsCut(rest, c.W)
			if r > 0 && c.Key == "account" {
				if got, want := strings.TrimRight(cell, " "), strings.TrimRight(fit(clean(labels[r-1]), c.W), " "); got != want {
					t.Errorf("line %d: the account cell is %q, not %q", r, got, want)
				}
			}
			at += c.W + ladderGap
		}
	}
}

func TestSelectionFollowsTheAccountNotTheRow(t *testing.T) {
	rows := accountsThree()
	m, _ := accountsModel(t, rows)
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	a := accountsOf(m)
	if a.sel != 1 {
		t.Fatalf("the first listing selects %d, not the first row", a.sel)
	}
	run(t, m, press("down")...)
	if a.sel != 2 {
		t.Fatalf("down selects %d", a.sel)
	}
	// Reordered elsewhere: the same account stays selected, and its row is
	// the one marked.
	run(t, m, accountsListing(rows[1], rows[2], rows[0]))
	if a.sel != 2 || !strings.Contains(plain(m), "home@example.com  claude  id 2") {
		t.Errorf("after a reorder %d is selected:\n%s", a.sel, plain(m))
	}
	if got := accountsMarked(m); len(got) != 1 || !strings.Contains(got[0], "home@") {
		t.Errorf("the rows marked selected are %q", got)
	}
	// Removed elsewhere: the account that took its place is selected.
	run(t, m, press("down")...)
	run(t, m, accountsListing(rows[1], rows[0]))
	if a.sel != 1 {
		t.Errorf("after the selected account went, %d is selected, not its neighbour 1", a.sel)
	}
	// The last one gone: the one before it.
	run(t, m, accountsListing(rows[1]))
	if a.sel != 2 {
		t.Errorf("after the last account went, %d is selected", a.sel)
	}
	// selectMsg picks an id, at once when it is listed, and once it is
	// when it is not yet.
	run(t, m, accountsListing(rows...), selectMsg{Account: 2})
	if a.sel != 2 {
		t.Errorf("selectMsg{2} selects %d", a.sel)
	}
	fresh := wire.Account{ID: 9, Provider: "claude", Email: "new@example.com", Status: "ok", Order: 4, Metered: true}
	run(t, m, selectMsg{Account: 9})
	if a.sel != 2 {
		t.Errorf("selectMsg for an account not listed yet moved the selection to %d", a.sel)
	}
	run(t, m, accountsListing(append(slices.Clone(rows), fresh)...))
	if a.sel != 9 {
		t.Errorf("the account asked for is not selected once listed: %d", a.sel)
	}
	// An empty listing selects nothing, and a listing after it the first.
	run(t, m, accountsListing())
	if _, ok := a.selected(); ok || a.sel != 0 {
		t.Errorf("an empty listing leaves %d selected", a.sel)
	}
	run(t, m, accountsListing(rows...))
	if a.sel != 1 {
		t.Errorf("a listing after an empty one selects %d", a.sel)
	}
	// A failed read changes nothing.
	run(t, m, listingMsg{Err: errors.New("down"), How: RefreshNone})
	if a.sel != 1 || len(a.rows) != 3 {
		t.Errorf("a failed read changed the table: %d selected, %d rows", a.sel, len(a.rows))
	}
	// An account asked for and not listed yet is forgotten once the person
	// moves the selection, by a key or by a click.
	later := func(id int) wire.Account {
		return wire.Account{ID: id, Provider: "claude", Email: fmt.Sprintf("n%d@example.com", id), Status: "ok", Order: id}
	}
	run(t, m, selectMsg{Account: 8}, press("down")[0])
	run(t, m, accountsListing(append(slices.Clone(rows), later(8))...))
	if a.sel != 2 {
		t.Errorf("an account asked for before a key took the selection to %d", a.sel)
	}
	run(t, m, selectMsg{Account: 10})
	m.View()
	top, _ := m.body()
	run(t, m, tea.MouseClickMsg{X: 5, Y: top + 3, Button: tea.MouseLeft})
	run(t, m, accountsListing(append(slices.Clone(rows), later(8), later(10))...))
	if a.sel != 3 {
		t.Errorf("an account asked for before a click took the selection to %d", a.sel)
	}
}

func TestDetailsShowEveryWindowAndSetting(t *testing.T) {
	acct := wire.Account{
		ID: 7, Provider: "claude", Name: "team", Email: "team@example.com", Status: "reauth", Order: 2, Threshold: 75,
		Percent: 80, Metered: true, Cwd: "/srv/team/project", ConfigDir: "/srv/team/.claude", Sessions: "own",
		RemoteControl: true, LoginUntil: accountsAt(15 * 24 * time.Hour), LongUntil: accountsAt(300 * 24 * time.Hour),
		NoCreditUntil: accountsAt(3 * time.Hour), CheckedAt: accountsAt(-4 * time.Minute),
		UsageWaitUntil: accountsAt(3 * time.Minute), Note: "extra usage 1.00 / 5.00 USD",
		DeadReason: "the provider refused the refresh token, which another sign-in replaced: invalid_grant",
		Windows: []wire.Window{
			{Name: "5h", Percent: 80, ResetsAt: accountsAt(time.Hour + 3*time.Minute), Primary: true},
			{Name: "7d", Percent: 40, ResetsAt: accountsAt(49 * time.Hour)},
			{Name: "Opus", Percent: 33, ResetsAt: accountsAt(5 * 24 * time.Hour), Scoped: true},
		},
	}
	m, _ := accountsModel(t, []wire.Account{acct})
	run(t, m, tea.BatchMsg{m.Init()}, size(120, 40))
	view := accountsPlain(accountsOf(m).View(120, 40))
	for _, want := range []string{
		"team  claude  id 7",
		"5h", " 80%", "1h 3m (21:20)",
		"7d", " 40%", "2d 1h (Thu 21:17)",
		"Opus", " 33%", "5d 0h (Sun 20:17)",
		"threshold       75%",
		"place           2",
		"folder          /srv/team/project",
		"config          /srv/team/.claude",
		"sessions        own",
		"remote control  on",
		"credential      login and long token",
		"login until     2026-11-25 20:17 (15d left)",
		"long until      2027-09-06 20:17 (300d left)",
		"no credit       3h 0m (23:17)",
		"status          dead: the provider refused the refresh token, which another sign-in replaced: invalid_grant",
		"checked         4m ago; read again in 3m (20:20), as the provider asked",
		"note            extra usage 1.00 / 5.00 USD",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the details lack %q:\n%s", want, view)
		}
	}
	// A scoped window has no threshold tick; the others have it.
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "Opus") && strings.Contains(l, barsDrawn().tick) {
			t.Errorf("the scoped window carries the threshold's tick: %q", l)
		}
	}
	// Narrow, the reason wraps under its value, and no line is too wide.
	view = accountsPlain(accountsOf(m).View(60, 40))
	lines := strings.Split(view, "\n")
	var status []string
	for i, l := range lines {
		if v, ok := strings.CutPrefix(l, "  status          "); ok {
			status = append(status, v)
			for _, more := range lines[i+1:] {
				v, ok := strings.CutPrefix(more, strings.Repeat(" ", 18))
				if !ok || strings.HasPrefix(v, " ") {
					break
				}
				status = append(status, v)
			}
		}
	}
	if len(status) < 2 || strings.Join(status, " ") != "dead: "+acct.DeadReason {
		t.Errorf("the reason does not wrap under its value at 60: %q\n%s", status, view)
	}
	for _, l := range lines {
		if textWidth(l) > 60 {
			t.Errorf("%q is wider than 60", l)
		}
	}
	// The defaults say what they are.
	plainAcct := wire.Account{ID: 4, Provider: "claude", Email: "p@example.com", Status: "ok", Order: 0, Threshold: 100, Metered: true,
		UsageWaitUntil: accountsAt(-time.Minute)}
	keyAcct := wire.Account{ID: 5, Provider: "claude", Credential: "apikey", Status: "ok", Order: 1, Threshold: 100}
	run(t, m, accountsListing(plainAcct, keyAcct))
	view = accountsPlain(accountsOf(m).View(120, 40))
	for _, want := range []string{
		"usage           not read yet", "place           out of rotation", "folder          not set",
		"config          not set", "sessions        shared", "remote control  off", "credential      login", "checked         never",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the details of an account with nothing set lack %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "read again") {
		t.Errorf("a wait that is over is still said:\n%s", view)
	}
	run(t, m, press("down")...)
	view = accountsPlain(accountsOf(m).View(120, 40))
	for _, want := range []string{"account-5  claude  id 5", "usage           none: an API key is paid per token", "credential      API key", "checked         n/a"} {
		if !strings.Contains(view, want) {
			t.Errorf("the details of an API key account lack %q:\n%s", want, view)
		}
	}
}

func TestStaleReadingsFade(t *testing.T) {
	rows := accountsThree()
	rows = append(rows, wire.Account{ID: 4, Provider: "claude", Credential: "apikey", Email: "key@example.com",
		Status: "ok", Order: 4, Threshold: 100})
	m, _ := accountsModel(t, rows, func(o *Options) { p := colorprofile.TrueColor; o.Profile = &p })
	run(t, m, tea.BatchMsg{m.Init()}, size(120, 40))
	th := m.e.th
	view := accountsOf(m).View(120, 40)
	lines := strings.Split(view, "\n")
	row := func(id string) string {
		for _, l := range lines[1:5] {
			if strings.Contains(ansi.Strip(l), "  "+id+"@") {
				return l
			}
		}
		t.Fatalf("no row for %s in\n%s", id, view)
		return ""
	}
	// cdx was read three hours ago: its percent and its reset are Faint.
	if cdx := row("cdx"); !strings.Contains(cdx, th.Faint.Render(" 95%")) || !strings.Contains(cdx, th.Faint.Render("2h 0m (22:17)")) {
		t.Errorf("the stale row's numbers are not Faint: %q", cdx)
	}
	// home was read a minute ago: its numbers keep their colour.
	if home := row("home"); !strings.Contains(home, th.Good.Render(" 12%")) || strings.Contains(home, th.Faint.Render(" 12%")) {
		t.Errorf("a fresh row is drawn Faint or without its colour: %q", home)
	}
	// An API key has no reading to be stale (R10, ruling n3).
	if key := row("key"); strings.Contains(key, "\x1b[2m") {
		t.Errorf("an API key row is drawn Faint: %q", key)
	}
	// The selected account's details fade as its row does.
	run(t, m, press("down", "down")...)
	lines = strings.Split(accountsOf(m).View(120, 40), "\n")
	details := strings.Join(lines[accountsOf(m).shown+2:], "\n")
	for _, faded := range []string{" 95%", "2h 0m (22:17)", "3h ago"} {
		if !strings.Contains(details, th.Faint.Render(faded)) {
			t.Errorf("the stale account's details do not fade %q:\n%q", faded, details)
		}
	}
	// A provider asking rota to wait makes a fresh reading stale too.
	rows[1].UsageWaitUntil = accountsAt(5 * time.Minute)
	run(t, m, accountsListing(rows...))
	lines = strings.Split(accountsOf(m).View(120, 40), "\n")
	if home := row("home"); !strings.Contains(home, th.Faint.Render(" 12%")) {
		t.Errorf("a reading the provider holds back is not Faint: %q", home)
	}
}

func TestNoReflowWhenASessionStarts(t *testing.T) {
	m, _ := accountsModel(t, accountsThree())
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	before := m.View().Content
	tab := accountsOf(m).View(80, 22)
	run(t, m, daemonMsg{Daemon: Daemon{Running: true}})
	run(t, m, daemonMsg{Daemon: Daemon{Running: true, Live: []Live{{ID: "a", Name: "one", Account: 1}, {ID: "b", Name: "two", Account: 3}}}})
	if got := accountsOf(m).View(80, 22); got != tab {
		t.Errorf("the table changed when sessions started:\n%s\n---\n%s", tab, got)
	}
	// The chrome may say what runs; the body never moves.
	bodyOf := func(s string) string {
		lines := strings.Split(s, "\n")
		top, h := m.body()
		return strings.Join(lines[top:top+h], "\n")
	}
	if got := m.View().Content; bodyOf(got) != bodyOf(before) {
		t.Errorf("the screen's body changed when sessions started")
	}
}

// accountsHooks puts a recorder in place of every action a key reaches on
// m's Accounts tab, and returns what they were called with.
func accountsHooks(m *Model) func() []string {
	do := &accountsOf(m).do
	var mu sync.Mutex
	var got []string
	say := func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf(format, a...))
	}
	do.launch = func(_ *accountsTab, a wire.Account) tea.Cmd { say("launch(%d)", a.ID); return nil }
	do.move = func(_ *accountsTab, a wire.Account, place string) tea.Cmd {
		say("move(%d, %s)", a.ID, place)
		return nil
	}
	do.nudge = func(_ *accountsTab, a wire.Account, d int) tea.Cmd { say("nudge(%d, %d)", a.ID, d); return nil }
	do.typed = func(_ *accountsTab, a wire.Account) overlay { say("typed(%d)", a.ID); return overlayClosed() }
	do.toggleOut = func(_ *accountsTab, a wire.Account) tea.Cmd { say("toggleOut(%d)", a.ID); return nil }
	do.login = func(_ *env, again *wire.Account) asider {
		if again == nil {
			say("login(new)")
		} else {
			say("login(%d)", again.ID)
		}
		return nil
	}
	do.edit = func(_ *env, a wire.Account) overlay { say("edit(%d)", a.ID); return &shellOverlay{} }
	do.remove = func(_ *env, a wire.Account) overlay { say("remove(%d)", a.ID); return overlayClosed() }
	do.crossed = func(_ *accountsTab, before, after []wire.Account) tea.Cmd {
		say("crossed(%d, %d)", len(before), len(after))
		return nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

func TestKeysReachTheirActions(t *testing.T) {
	m, b := accountsModel(t, accountsThree())
	got := accountsHooks(m)
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	a := accountsOf(m)
	cases := []struct {
		key, want string
	}{
		{"enter", "launch(1)"},
		{"shift+up", "move(1, up)"}, {"K", "move(1, up)"},
		{"shift+down", "move(1, down)"}, {"J", "move(1, down)"},
		{"u", "move(1, first)"},
		{"+", "nudge(1, 5)"}, {"=", "nudge(1, 5)"}, {"-", "nudge(1, -5)"},
		{"%", "typed(1)"},
		{"x", "toggleOut(1)"},
		{"n", "login(new)"},
		{"l", "login(1)"},
		{"d", "remove(1)"},
	}
	for _, c := range cases {
		before := len(got())
		run(t, m, press(c.key)...)
		if now := got(); len(now) != before+1 || now[before] != c.want {
			t.Errorf("%s reached %q, not %q", c.key, now[before:], c.want)
		}
	}
	// The overlay an action makes is opened.
	run(t, m, press("e")...)
	if g := got(); g[len(g)-1] != "edit(1)" || len(m.stack) != 1 {
		t.Errorf("e reached %q and opened %d overlays", g[len(g)-1], len(m.stack))
	}
	run(t, m, press("esc")...)
	// The selection moves, and the actions follow it.
	run(t, m, press("j")...)
	run(t, m, press("enter")...)
	run(t, m, press("down", "k", "l")...)
	if g := got(); !slices.Equal(g[len(g)-2:], []string{"launch(2)", "login(2)"}) {
		t.Errorf("after moving, the actions reached %q", g[len(g)-2:])
	}
	if a.sel != 2 {
		t.Errorf("down, then k, left %d selected", a.sel)
	}
	// r reads every account's usage now.
	run(t, m, press("r")...)
	if c := b.named("Accounts"); c[len(c)-1].String() != "Accounts(force)" {
		t.Errorf("r asked %s", c[len(c)-1])
	}
	// With nothing listed, only adding is offered.
	run(t, m, accountsListing())
	before := len(got())
	run(t, m, press("enter", "K", "+", "%", "x", "u", "l", "e", "d", "n")...)
	if g := got()[before:]; !slices.Equal(g, []string{"login(new)"}) {
		t.Errorf("with no account the keys reached %q", g)
	}
	var offered []string
	for _, k := range a.Keys() {
		offered = append(offered, k.Help().Key)
	}
	slices.Sort(offered)
	if !slices.Equal(offered, []string{"n", "r", "↑/k", "↓/j"}) {
		t.Errorf("with no account the tab offers %q", offered)
	}
}

func TestWatchSeesNoChangeKeys(t *testing.T) {
	m, b := accountsModel(t, accountsThree())
	got := accountsHooks(m)
	accountsWatch(b)
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	a := accountsOf(m)
	help := func() []string {
		var out []string
		for _, k := range a.Keys() {
			out = append(out, k.Help().Key)
		}
		slices.Sort(out)
		return out
	}
	if h := help(); !slices.Equal(h, []string{"r", "↑/k", "↓/j"}) {
		t.Errorf("watch is offered %q", h)
	}
	run(t, m, press("enter", "shift+up", "K", "J", "+", "=", "-", "%", "x", "u", "n", "l", "e", "d")...)
	if g := got(); len(g) != 0 {
		t.Errorf("watch reached %q", g)
	}
	if len(m.stack) != 0 {
		t.Errorf("watch opened %d overlays", len(m.stack))
	}
	run(t, m, press("down")...)
	if a.sel != 2 {
		t.Errorf("watch cannot move the selection: %d", a.sel)
	}
	// A control principal is offered every key.
	m, _ = accountsModel(t, accountsThree())
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	a = accountsOf(m)
	if n := len(a.Keys()); n != 15 {
		t.Errorf("control is offered %d keys, not 15: %q", n, help())
	}
}

func TestManyAccountsScroll(t *testing.T) {
	var rows []wire.Account
	for i := 1; i <= 30; i++ {
		rows = append(rows, wire.Account{ID: i, Provider: "claude", Email: fmt.Sprintf("acct%02d@example.com", i),
			Status: "ok", Order: i, Threshold: 100, Percent: float64(i), Metered: true, CheckedAt: accountsAt(-time.Minute),
			Windows: []wire.Window{{Name: "5h", Percent: float64(i), ResetsAt: accountsAt(time.Hour)}}})
	}
	m, _ := accountsModel(t, rows)
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	defer func() {
		if n := strings.Count(accountsOf(m).View(80, 22), "\n") + 1; n > 22 {
			t.Errorf("the tab draws %d lines in 22", n)
		}
	}()
	rowOf := func(n int) bool {
		prefix := fmt.Sprintf("%-2d  acct%02d@", n, n)
		for _, l := range strings.Split(plain(m), "\n") {
			if strings.HasPrefix(l, prefix) {
				return true
			}
		}
		return false
	}
	if !rowOf(1) || rowOf(30) {
		t.Fatalf("at the start the table shows the first row and not the last:\n%s", plain(m))
	}
	for i := 2; i <= 30; i++ {
		run(t, m, press("down")...)
		if !rowOf(i) {
			t.Fatalf("selected %d, which is not shown:\n%s", i, plain(m))
		}
	}
	if rowOf(1) || !strings.Contains(plain(m), "acct30@example.com  claude  id 30") {
		t.Errorf("at the bottom the table still shows the first row, or the details are not the last:\n%s", plain(m))
	}
	run(t, m, press("down")...)
	if a := accountsOf(m); a.sel != 30 {
		t.Errorf("down past the bottom selects %d", a.sel)
	}
	// The details are cut to the rows left, and say how much is not shown.
	view := strings.Split(accountsPlain(accountsOf(m).View(80, 22)), "\n")
	if last := view[len(view)-2]; last != "  … 2 more" {
		t.Errorf("the cut details end with %q:\n%s", last, strings.Join(view, "\n"))
	}
	for i := 29; i >= 1; i-- {
		run(t, m, press("up")...)
		if !rowOf(i) {
			t.Fatalf("selected %d on the way up, which is not shown:\n%s", i, plain(m))
		}
	}
	// Scrolled to the bottom, a listing of three draws them, with the one
	// now where the selected one was.
	run(t, m, selectMsg{Account: 30})
	m.View()
	run(t, m, accountsListing(rows[:3]...))
	if !rowOf(1) || !rowOf(3) || accountsOf(m).sel != 3 {
		t.Errorf("after the listing shrank, %d is selected:\n%s", accountsOf(m).sel, plain(m))
	}
}

func TestAccountsUpdateDoesNoIO(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	m, b := accountsModel(t, accountsThree())
	rows := accountsThree()
	msgs := []tea.Msg{size(80, 24), accountsListing(rows...), modelWhoMsg{Who: Principal{Role: "control"}},
		accountsListing(rows[2], rows[0]), selectMsg{Account: 3}, daemonMsg{Daemon: Daemon{Running: true}},
		tea.MouseClickMsg{X: 1, Y: 2, Button: tea.MouseLeft}, tea.MouseWheelMsg{X: 1, Y: 2, Button: tea.MouseWheelDown},
		listingMsg{Err: errors.New("x")}, tea.PasteMsg{Content: "p"}, accountsListing()}
	for _, k := range []string{"up", "down", "k", "j", "enter", "shift+up", "shift+down", "K", "J", "+", "=", "-", "%",
		"x", "u", "n", "l", "e", "d", "r", "esc"} {
		msgs = append(msgs, accountsListing(rows...), press(k)[0])
	}
	assertNoIO(t, b, func() {
		m.Init()
		for _, msg := range msgs {
			m.Update(msg)
			m.View()
		}
	})
}

func TestAClickSelectsARowAndTheWheelScrolls(t *testing.T) {
	m, _ := accountsModel(t, accountsThree())
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24), press("M")[0])
	a := accountsOf(m)
	top, _ := m.body()
	m.View()
	// The tab's first line is its header, then account 1, 2 and 3.
	run(t, m, tea.MouseClickMsg{X: 10, Y: top + 3, Button: tea.MouseLeft})
	if a.sel != 3 {
		t.Errorf("a click on the third row selects %d", a.sel)
	}
	run(t, m, tea.MouseClickMsg{X: 10, Y: top, Button: tea.MouseLeft}, tea.MouseClickMsg{X: 10, Y: top + 4, Button: tea.MouseLeft},
		tea.MouseClickMsg{X: 10, Y: top + 6, Button: tea.MouseLeft}, tea.MouseClickMsg{X: 10, Y: top + 1, Button: tea.MouseRight})
	if a.sel != 3 {
		t.Errorf("a click on the header, the rule, the details or with the right button moved the selection to %d", a.sel)
	}
	run(t, m, tea.MouseWheelMsg{X: 10, Y: top + 1, Button: tea.MouseWheelUp})
	if a.sel != 2 {
		t.Errorf("the wheel up selects %d", a.sel)
	}
	run(t, m, tea.MouseWheelMsg{X: 10, Y: top + 1, Button: tea.MouseWheelDown}, tea.MouseWheelMsg{X: 10, Y: top + 1, Button: tea.MouseWheelDown})
	if a.sel != 3 {
		t.Errorf("the wheel down past the end selects %d", a.sel)
	}
	// A table scrolled by the keys maps a click to the row drawn there.
	var rows []wire.Account
	for i := 1; i <= 30; i++ {
		rows = append(rows, wire.Account{ID: i, Provider: "claude", Email: fmt.Sprintf("a%02d@example.com", i), Status: "ok", Order: i})
	}
	run(t, m, accountsListing(rows...), selectMsg{Account: 30})
	m.View()
	run(t, m, tea.MouseClickMsg{X: 10, Y: top + 1, Button: tea.MouseLeft})
	if want := 30 - a.shown + 1; a.sel != want {
		t.Errorf("a click on the first row drawn selects %d, not %d", a.sel, want)
	}
	// Below the rows drawn are the details, whatever rows the listing has
	// beyond them.
	run(t, m, selectMsg{Account: 1})
	m.View()
	run(t, m, tea.MouseClickMsg{X: 10, Y: top + a.shown + 1, Button: tea.MouseLeft},
		tea.MouseClickMsg{X: 10, Y: top + a.shown + 3, Button: tea.MouseLeft})
	if a.sel != 1 {
		t.Errorf("a click on the rule or in the details selects %d", a.sel)
	}
}

func TestAccountsRingsTheBellAfterTheFirstListing(t *testing.T) {
	m, _ := accountsModel(t, accountsThree())
	got := accountsHooks(m)
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	if g := got(); len(g) != 0 {
		t.Errorf("the first listing asked %q", g)
	}
	rows := accountsThree()
	run(t, m, accountsListing(rows[0]), listingMsg{Err: errors.New("x")}, accountsListing(rows...))
	if g := got(); !slices.Equal(g, []string{"crossed(3, 1)", "crossed(1, 3)"}) {
		t.Errorf("later listings asked %q", g)
	}
}

func TestAccountsEmptyListingSaysHowToAdd(t *testing.T) {
	m, b := accountsModel(t, nil)
	if got := accountsOf(m).View(80, 20); strings.Contains(got, "No accounts") {
		t.Errorf("before any listing the tab says %q", got)
	}
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	if got := accountsPlain(accountsOf(m).View(80, 20)); got != "No accounts yet. n adds one.\n" {
		t.Errorf("control is told %q", got)
	}
	m, b = accountsModel(t, nil)
	accountsWatch(b)
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	if got := accountsPlain(accountsOf(m).View(80, 20)); got != "No accounts yet.\n" {
		t.Errorf("watch is told %q", got)
	}
}

func TestAccountsStateSaysWhy(t *testing.T) {
	th := NewTheme(colorprofile.Ascii, true)
	base := wire.Account{ID: 1, Provider: "claude", Status: "ok", Order: 1, Threshold: 75, Percent: 10, Metered: true}
	with := func(f func(a *wire.Account)) wire.Account { a := base; f(&a); return a }
	cases := []struct {
		name    string
		acct    wire.Account
		control bool
		words   []string
	}{
		{"ok", base, true, []string{"ok"}},
		{"dead", with(func(a *wire.Account) { a.Status = "reauth"; a.Percent = 99 }), true, []string{"dead: l signs in", "dead"}},
		{"dead to watch", with(func(a *wire.Account) { a.Status = "reauth" }), false, []string{"dead"}},
		{"dead, and out of credit", with(func(a *wire.Account) {
			a.Status, a.Credential, a.Metered, a.NoCreditUntil = "reauth", "apikey", false, accountsAt(time.Hour)
		}), true, []string{"dead: l signs in", "dead"}},
		{"out of credit", with(func(a *wire.Account) {
			a.Status, a.Credential, a.Metered, a.NoCreditUntil = "limited", "apikey", false, accountsAt(3*time.Hour+20*time.Minute)
		}), true, []string{"no credit (3h)", "no credit", "0 credit"}},
		{"limited", with(func(a *wire.Account) { a.Status, a.Percent = "limited", 100 }), true, []string{"limited"}},
		{"over", with(func(a *wire.Account) { a.Percent = 75 }), true, []string{"over its 75%", "over 75%", "over"}},
		{"under", with(func(a *wire.Account) { a.Percent = 74.9 }), true, []string{"ok"}},
		{"an API key is never over", with(func(a *wire.Account) { a.Metered, a.Credential, a.Percent = false, "apikey", 100 }), true, []string{"ok"}},
		{"login ends", with(func(a *wire.Account) { a.LoginUntil = accountsAt(2*24*time.Hour + 5*time.Hour) }), true,
			[]string{"login ends in 2d", "ends in 2d", "ends 2d"}},
		{"login ends soon", with(func(a *wire.Account) { a.LoginUntil = accountsAt(40 * time.Minute) }), true,
			[]string{"login ends in 40m", "ends in 40m", "ends 40m"}},
		{"login far off", with(func(a *wire.Account) { a.LoginUntil = accountsAt(4 * 24 * time.Hour) }), true, []string{"ok"}},
		{"login ended", with(func(a *wire.Account) { a.LoginUntil = accountsAt(-time.Hour) }), true, []string{"login ended", "ended"}},
		{"out of rotation", with(func(a *wire.Account) { a.Order = 0 }), true, []string{"out of rotation", "out"}},
	}
	for _, c := range cases {
		if words, _ := accountsViewState(th, c.acct, accountsNow, c.control); !slices.Equal(words, c.words) {
			t.Errorf("%s: %q, not %q", c.name, words, c.words)
		}
	}
	// The usage cells say why there is no bar.
	for _, c := range []struct {
		acct wire.Account
		want string
	}{
		{with(func(a *wire.Account) { a.Credential, a.Metered = "apikey", false }), "api key"},
		{with(func(a *wire.Account) { a.Metered = false }), "n/a"},
		{base, "?"},
		{with(func(a *wire.Account) { a.Windows = []wire.Window{{Name: "7d", Percent: 3}} }), ""},
		{with(func(a *wire.Account) { a.Windows = []wire.Window{{Name: "5h", Percent: 3, Scoped: true}} }), ""},
	} {
		cell := ansi.Strip(accountsViewUsage(th, c.acct, "5h", 23))
		if got := strings.TrimSpace(cell); got != c.want {
			t.Errorf("the usage cell is %q, not %q", got, c.want)
		}
		if w := textWidth(cell); w != 23 {
			t.Errorf("the usage cell %q is %d columns, not 23", cell, w)
		}
	}
}

// Every state is said whole at every width the screen draws at: the state
// column is 8 to 16 columns wide, and the longest way to say a state that
// fits is said, never one cut with "…" (m2).
func TestAccountsStateIsNeverCut(t *testing.T) {
	at := accountsAt
	base := wire.Account{Provider: "claude", Status: "ok", Threshold: 75, Percent: 10, Metered: true}
	type say struct {
		from int // the narrowest state column it is said in
		word string
	}
	states := []struct {
		acct wire.Account
		says []say // widest first
	}{
		{wire.Account{Status: "reauth"}, []say{{16, "dead: l signs in"}, {0, "dead"}}},
		{wire.Account{Status: "limited", Credential: "apikey", NoCreditUntil: at(3 * time.Hour)}, []say{{14, "no credit (3h)"}, {9, "no credit"}, {0, "0 credit"}}},
		{wire.Account{Status: "limited", Credential: "apikey", NoCreditUntil: at(23 * time.Hour)}, []say{{15, "no credit (23h)"}, {9, "no credit"}, {0, "0 credit"}}},
		{wire.Account{Status: "limited", Percent: 100}, []say{{0, "limited"}}},
		{wire.Account{Percent: 80}, []say{{12, "over its 75%"}, {0, "over 75%"}}},
		{wire.Account{Threshold: 100, Percent: 100}, []say{{13, "over its 100%"}, {9, "over 100%"}, {0, "over"}}},
		{wire.Account{LoginUntil: at(49 * time.Hour)}, []say{{16, "login ends in 2d"}, {10, "ends in 2d"}, {0, "ends 2d"}}},
		{wire.Account{LoginUntil: at(23 * time.Hour)}, []say{{17, "login ends in 23h"}, {11, "ends in 23h"}, {0, "ends 23h"}}},
		{wire.Account{LoginUntil: at(-time.Hour)}, []say{{11, "login ended"}, {0, "ended"}}},
		{wire.Account{Order: -1}, []say{{15, "out of rotation"}, {0, "out"}}},
		{wire.Account{}, []say{{0, "ok"}}},
	}
	var rows []wire.Account
	for i, s := range states {
		a := base
		a.ID, a.Order, a.Email = i+1, i+1, fmt.Sprintf("s%d@example.com", i+1)
		if s.acct.Status != "" {
			a.Status = s.acct.Status
		}
		if s.acct.Credential != "" {
			a.Credential, a.Metered = s.acct.Credential, false
		}
		if s.acct.Threshold != 0 {
			a.Threshold = s.acct.Threshold
		}
		if s.acct.Percent != 0 {
			a.Percent = s.acct.Percent
		}
		if s.acct.Order < 0 {
			a.Order = 0
		}
		a.NoCreditUntil, a.LoginUntil = s.acct.NoCreditUntil, s.acct.LoginUntil
		rows = append(rows, a)
	}
	m, _ := accountsModel(t, rows)
	run(t, m, tea.BatchMsg{m.Init()}, size(124, 40))
	widths := map[int]bool{}
	for w := 60; w <= 124; w++ {
		at := 0
		state := 0
		for _, c := range accountsLayout(w) {
			if c.Key == "state" {
				state = c.W
				break
			}
			if c.Shown {
				at += c.W + ladderGap
			}
		}
		widths[state] = true
		lines := strings.Split(ansi.Strip(accountsOf(m).View(w, 40)), "\n")
		for i, s := range states {
			_, rest, _ := accountsCut(lines[i+1], at)
			cell, _, _ := accountsCut(rest, state)
			want := ""
			for _, say := range s.says {
				if state >= say.from {
					want = say.word
					break
				}
			}
			if got := strings.TrimRight(cell, " "); got != want || strings.Contains(cell, "…") {
				t.Errorf("at %d columns (state %d) %s says %q, not %q", w, state, rows[i].Email, got, want)
			}
		}
	}
	for _, w := range []int{8, 15, 16} {
		if !widths[w] {
			t.Errorf("no width from 60 to 124 gives the state %d columns; the ladder moved, and this test with it", w)
		}
	}
}

// What the accounts could not be read for is said in place of the table
// until a listing comes, and the next tick asks again.
func TestAccountsSaysWhyTheAccountsCannotBeRead(t *testing.T) {
	shellRegistry(t)
	register(tabsAccounts, newAccounts)
	var mu sync.Mutex
	fail := true
	failing := func(f bool) { mu.Lock(); fail = f; mu.Unlock() }
	b := &fakeBackend{AccountsFn: func(context.Context, Refresh) (Listing, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return Listing{}, errors.New("the store is\x1b[2J locked")
		}
		return Listing{Accounts: accountsThree()}, nil
	}}
	ticks := &fakeTicks{clock: accountsNow}
	m := shellModel(t, b, ticks)
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	const said = "the accounts could not be read: the store is locked\n"
	if got := accountsPlain(accountsOf(m).View(80, 20)); got != said {
		t.Errorf("a failed first read says %q", got)
	}
	run(t, m, ticks.advance(15*time.Second)...)
	if n := len(b.named("Accounts")); n < 2 {
		t.Errorf("the accounts were asked %d times", n)
	}
	if got := accountsPlain(accountsOf(m).View(80, 20)); got != said {
		t.Errorf("a second failed read says %q", got)
	}
	failing(false)
	run(t, m, ticks.advance(15*time.Second)...)
	if got := accountsPlain(accountsOf(m).View(80, 20)); !strings.Contains(got, "work@example") {
		t.Errorf("a read that worked shows no table:\n%s", got)
	}
	// Once there is a table, a failed read keeps it.
	failing(true)
	run(t, m, ticks.advance(15*time.Second)...)
	if got := accountsPlain(accountsOf(m).View(80, 20)); !strings.Contains(got, "work@example") {
		t.Errorf("a failed read after a listing dropped the table:\n%s", got)
	}
}

// accountsAbout is an overlay that says what it is about.
type accountsAbout struct {
	*shellOverlay
	kind    string
	account int
}

func (o accountsAbout) Update(msg tea.Msg) (overlay, tea.Cmd) {
	o.shellOverlay.Update(msg)
	return o, nil
}

func (o accountsAbout) About() (string, int) { return o.kind, o.account }

// A key that opens an overlay opens it as its Update returns, so the keys
// a terminal sends with it reach the overlay: a double d opens one. And the
// shell opens no overlay about what the one on top is about (m4).
func TestAccountsTheSameOverlayOpensOnce(t *testing.T) {
	m, _ := accountsModel(t, accountsThree())
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	a := accountsOf(m)
	var made []*shellOverlay
	about := func(kind string, id int) overlay {
		o := &shellOverlay{}
		made = append(made, o)
		return accountsAbout{o, kind, id}
	}
	a.do.remove = func(_ *env, acct wire.Account) overlay { return about("remove", acct.ID) }
	a.do.edit = func(_ *env, acct wire.Account) overlay { return about("edit", acct.ID) }
	a.do.typed = func(*accountsTab, wire.Account) overlay { o := &shellOverlay{}; made = append(made, o); return o }
	for _, c := range [][]string{{"d", "d"}, {"e", "e", "e"}, {"d", "down", "d"}, {"e", "d"}, {"%", "%"}} {
		made = nil
		run(t, m, press(c...)...)
		if len(m.stack) != 1 || len(made) != 1 {
			t.Errorf("%q opened %d overlays and made %d", c, len(m.stack), len(made))
		} else if !slices.Equal(made[0].keys, c[1:]) {
			t.Errorf("%q: the overlay took %q", c, made[0].keys)
		}
		for len(m.stack) > 0 {
			run(t, m, press("esc")...)
		}
		run(t, m, selectMsg{Account: 1})
	}
	// Opened together, the same overlay opens once, and two others both.
	for _, c := range []struct {
		open []overlay
		want int
	}{
		{[]overlay{about("remove", 1), about("remove", 1)}, 1}, // the same remove twice
		{[]overlay{about("remove", 1), about("remove", 2)}, 2}, // removes of two accounts
		{[]overlay{about("edit", 1), about("remove", 1)}, 2},   // two kinds, one account
		{[]overlay{&shellOverlay{}, &shellOverlay{}}, 2},       // overlays that do not say
	} {
		for _, o := range c.open {
			run(t, m, openMsg{O: o})
		}
		if len(m.stack) != c.want {
			t.Errorf("opened %d overlays, not %d", len(m.stack), c.want)
		}
		for len(m.stack) > 0 {
			run(t, m, press("esc")...)
		}
	}
}

// A label made from a uuid is cut as it is drawn, after clean: never in the
// middle of a character a terminal draws as one.
func TestAccountsALabelFromAUUIDIsCutAsItIsDrawn(t *testing.T) {
	for _, uuid := range []string{"aaaaaaaaaaa🇺🇸xyz", "0123456789abcdef-0123", "a\x1b[2Jbcdefghijklmnop"} {
		got := accountsLabel(wire.Account{ID: 1, UUID: uuid})
		if wc, gr := accountsWidths(got); wc != gr || wc > 12 {
			t.Errorf("the label of %q is %q, %d columns rune by rune and %d by graphemes", uuid, got, wc, gr)
		}
		if !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '\x1b') {
			t.Errorf("the label of %q is %q", uuid, got)
		}
		for _, r := range got {
			if r >= 0x1f1e6 && r <= 0x1f1ff {
				t.Errorf("the label of %q keeps half a flag: %q", uuid, got)
			}
		}
	}
	if got := accountsLabel(wire.Account{ID: 1, UUID: "short"}); got != "short" {
		t.Errorf("a short uuid is %q", got)
	}
	if got := accountsLabel(wire.Account{ID: 7}); got != "account-7" {
		t.Errorf("an account with no name is %q", got)
	}
}
