package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/wire"
)

// barsAs is a bar written in R33's blocks as this binary must draw it: the
// same where ambiguous characters are measured narrow, and in ASCII where
// they are measured wide. The mode is read from the measure, not from the
// function under test.
func barsAs(s string) string {
	if !textEastAsian() {
		return s
	}
	return strings.NewReplacer("▕", "[", "█", "#", "░", "-", "│", "|", "▏", "]").Replace(s)
}

// barsNow is the clock every test here reads: Tuesday 2026-11-10 20:17 UTC.
var barsNow = time.Date(2026, 11, 10, 20, 17, 0, 0, time.UTC)

func TestTheBarTicksAtTheThreshold(t *testing.T) {
	th := NewTheme(colorprofile.TrueColor, true)
	for _, c := range []struct {
		pct              float64
		threshold, cells int
		want             string
	}{
		{62, 75, 16, "▕██████████░░│░░░▏  62%"},
		{62, 100, 16, "▕██████████░░░░░░▏  62%"},
		{0, 100, 16, "▕░░░░░░░░░░░░░░░░▏   0%"},
		{100, 100, 16, "▕████████████████▏ 100%"},
		{0, 75, 8, "▕░░░░░░│░▏   0%"},
		// Past the tick the tick still shows, among the filled cells.
		{90, 50, 8, "▕████│██░▏  90%"},
		// The tick never falls off the end.
		{10, 99, 4, "▕░░░│▏  10%"},
		{10, 1, 4, "▕│░░░▏  10%"},
		// A reading past the ends draws as the end it passed.
		{140, 100, 4, "▕████▏ 100%"},
		{-3, 100, 4, "▕░░░░▏   0%"},
		// No threshold the store set means none.
		{50, 0, 4, "▕██░░▏  50%"},
		// The percent is rounded down: 99.6 is not full, and 74.6 is below
		// the 75 its colour says it is below.
		{99.6, 100, 4, "▕████▏  99%"},
		{74.6, 75, 4, "▕███│▏  74%"},
	} {
		s := bar(th, c.pct, c.threshold, c.cells)
		if got, want := ansi.Strip(s), barsAs(c.want); got != want {
			t.Errorf("bar(%v, %d, %d) = %q, want %q", c.pct, c.threshold, c.cells, got, want)
		}
		if w := textWidth(s); w != c.cells+barsFrame {
			t.Errorf("bar(%v, %d, %d) is %d wide, want %d", c.pct, c.threshold, c.cells, w, c.cells+barsFrame)
		}
	}
	// The colour is the level's: over at the threshold, in the Over style.
	over := bar(th, 80, 75, 8)
	if !strings.Contains(over, th.Over.Render(barsAs("██████"))) {
		t.Errorf("a bar over its threshold is not drawn in Over: %q", over)
	}
	good := bar(th, 10, 75, 8)
	if !strings.Contains(good, th.Good.Render(barsAs("█"))) {
		t.Errorf("a bar well under its threshold is not drawn in Good: %q", good)
	}
	// A tick the reading has reached is in the level's style, even where
	// rounding left it just past the fill; one it has not reached is Muted.
	if !strings.Contains(over, th.Over.Render(barsAs("│"))) {
		t.Errorf("a reached tick is not drawn in Over: %q", over)
	}
	if !strings.Contains(good, th.Muted.Render(barsAs("│"))) {
		t.Errorf("a tick not reached is not drawn Muted: %q", good)
	}
	// The empty cells are Faint, and the percent is the level's colour.
	warn := bar(th, 62, 75, 16)
	if !strings.Contains(warn, th.Faint.Render(barsAs("░░"))) {
		t.Errorf("the empty cells are not Faint: %q", warn)
	}
	if !strings.Contains(warn, th.Warn.Render(" 62%")) {
		t.Errorf("the percent is not in the level's colour: %q", warn)
	}
}

func TestColourIsRelativeToTheThreshold(t *testing.T) {
	for _, c := range []struct {
		pct       float64
		threshold int
		want      int
	}{
		{59, 75, lvGood},
		{61, 75, lvWarn},
		{75, 75, lvOver},
		{79, 100, lvGood},
		{80, 100, lvWarn},
		{99.9, 100, lvWarn},
		{100, 100, lvOver},
		{30, 0, lvGood}, // no threshold means 100
		{0, 10, lvGood},
	} {
		if got := level(c.pct, c.threshold); got != c.want {
			t.Errorf("level(%v, %d) = %d, want %d", c.pct, c.threshold, got, c.want)
		}
	}
}

func TestResetReadsAsCountdownAndClock(t *testing.T) {
	for _, c := range []struct {
		at    time.Time
		short bool
		want  string
	}{
		{barsNow.Add(time.Hour + 3*time.Minute), false, "1h 3m (21:20)"},
		{barsNow.Add(time.Hour + 3*time.Minute), true, "1h 3m"},
		{barsNow.Add(49 * time.Hour), false, "2d 1h (Thu 21:17)"},
		{barsNow.Add(49 * time.Hour), true, "2d 1h"},
		{barsNow.Add(4*time.Hour + 10*time.Minute), false, "4h 10m (00:27)"},
		{barsNow.Add(30 * time.Second), false, "<1m (20:17)"},
		{barsNow.Add(30 * time.Second), true, "<1m"},
		{barsNow.Add(45 * time.Minute), false, "45m (21:02)"},
		{barsNow.Add(24 * time.Hour), false, "1d 0h (Wed 20:17)"},
		{barsNow.Add(-time.Minute), false, "now"},
		{barsNow, true, "now"},
		{time.Time{}, false, ""},
	} {
		got := resetText(c.at, barsNow, time.UTC, c.short)
		if got != c.want {
			t.Errorf("resetText(now%+v, short %v) = %q, want %q", c.at.Sub(barsNow), c.short, got, c.want)
		}
		// Every reading fits its column: 18 wide, and 8 when short.
		limit := 18
		if c.short {
			limit = 8
		}
		if textWidth(got) > limit {
			t.Errorf("resetText(now%+v, short %v) = %q is wider than %d", c.at.Sub(barsNow), c.short, got, limit)
		}
	}
	// The clock is the person's own.
	tokyo := time.FixedZone("JST", 9*3600)
	if got := resetText(barsNow.Add(time.Hour+3*time.Minute), barsNow, tokyo, false); got != "1h 3m (06:20)" {
		t.Errorf("in Tokyo the reset reads %q", got)
	}
	// Each form at its widest. A window resets within a week, so the long
	// form fits the column's 18 for every minute of one; past that it is
	// longer, and fit cuts it. The short form fits its 8 however far.
	for _, c := range []struct {
		in          time.Duration
		long, short string
	}{
		{23*time.Hour + 59*time.Minute, "23h 59m (20:16)", "23h 59m"},
		{6*24*time.Hour + 23*time.Hour + 59*time.Minute, "6d 23h (Tue 20:16)", "6d 23h"},
		{9*24*time.Hour + 23*time.Hour, "9d 23h (Fri 19:17)", "9d 23h"},
		{99*24*time.Hour + 23*time.Hour, "99d 23h (Thu 19:17)", "99d 23h"},
		{100 * 24 * time.Hour, ">99d (Thu 20:17)", ">99d"},
	} {
		if got := resetText(barsNow.Add(c.in), barsNow, time.UTC, false); got != c.long {
			t.Errorf("resetText(now+%v) = %q, want %q", c.in, got, c.long)
		}
		if got := resetText(barsNow.Add(c.in), barsNow, time.UTC, true); got != c.short || textWidth(got) > 8 {
			t.Errorf("resetText(now+%v, short) = %q, want %q", c.in, got, c.short)
		}
	}
	for d := time.Minute; d < 7*24*time.Hour; d += 7 * time.Minute {
		if got := resetText(barsNow.Add(d), barsNow, time.UTC, false); textWidth(got) > 18 {
			t.Fatalf("resetText(now+%v) = %q is wider than 18", d, got)
		}
	}
}

func barsWindow(name string, pct float64, resets time.Duration, scoped bool) wire.Window {
	return wire.Window{Name: name, Percent: pct, ResetsAt: barsNow.Add(resets).Format(time.RFC3339), Scoped: scoped}
}

func TestTheDecidingWindowIsTheOneThatHoldsItBack(t *testing.T) {
	a := wire.Account{Threshold: 80, Windows: []wire.Window{
		barsWindow("5h", 40, time.Hour, false),
		barsWindow("7d", 90, 50*time.Hour, false),
	}}
	if w, ok := deciding(a); !ok || w.Name != "7d" {
		t.Errorf("5h at 40 and 7d at 90 under 80: %v %v decides", w.Name, ok)
	}
	// Under the threshold, the highest decides.
	a = wire.Account{Threshold: 80, Windows: []wire.Window{
		barsWindow("5h", 60, time.Hour, false),
		barsWindow("7d", 30, 50*time.Hour, false),
	}}
	if w, ok := deciding(a); !ok || w.Name != "5h" {
		t.Errorf("5h at 60 and 7d at 30 under 80: %v %v decides", w.Name, ok)
	}
	// Both at 100: the account is back when the later one resets.
	a = wire.Account{Threshold: 100, Windows: []wire.Window{
		barsWindow("7d", 100, 50*time.Hour, false),
		barsWindow("5h", 100, time.Hour, false),
	}}
	if w, ok := deciding(a); !ok || w.Name != "7d" {
		t.Errorf("both at 100: %v %v decides", w.Name, ok)
	}
	a.Windows[0], a.Windows[1] = a.Windows[1], a.Windows[0]
	if w, ok := deciding(a); !ok || w.Name != "7d" {
		t.Errorf("both at 100, the other order: %v %v decides", w.Name, ok)
	}
	// Scoped windows never decide, however full.
	a = wire.Account{Threshold: 80, Windows: []wire.Window{
		barsWindow("Opus", 100, 90*time.Hour, true),
		barsWindow("5h", 20, time.Hour, false),
	}}
	if w, ok := deciding(a); !ok || w.Name != "5h" {
		t.Errorf("a full scoped window: %v %v decides", w.Name, ok)
	}
	a.Windows = a.Windows[:1]
	if w, ok := deciding(a); ok {
		t.Errorf("only a scoped window: %v decides", w.Name)
	}
	if _, ok := deciding(wire.Account{}); ok {
		t.Error("an account with no windows has a deciding one")
	}
	// No threshold set means 100.
	a = wire.Account{Windows: []wire.Window{
		barsWindow("5h", 95, time.Hour, false),
		barsWindow("7d", 90, 50*time.Hour, false),
	}}
	if w, ok := deciding(a); !ok || w.Name != "5h" {
		t.Errorf("no threshold, 95 and 90: %v %v decides", w.Name, ok)
	}
}

func TestStaleAfterTenMinutesOrAWait(t *testing.T) {
	at := func(d time.Duration) string { return barsNow.Add(-d).Format(time.RFC3339) }
	for _, c := range []struct {
		name string
		a    wire.Account
		want bool
	}{
		{"just read", wire.Account{Metered: true, CheckedAt: at(time.Minute)}, false},
		{"ten minutes", wire.Account{Metered: true, CheckedAt: at(10 * time.Minute)}, false},
		{"past ten minutes", wire.Account{Metered: true, CheckedAt: at(10*time.Minute + time.Second)}, true},
		{"never read", wire.Account{Metered: true}, true},
		{"unreadable date", wire.Account{Metered: true, CheckedAt: "yesterday"}, true},
		{"a wait", wire.Account{Metered: true, CheckedAt: at(time.Minute), UsageWaitUntil: barsNow.Add(time.Minute).Format(time.RFC3339)}, true},
		// An account with no usage to read has no reading to be old.
		{"unmetered", wire.Account{}, false},
	} {
		if got := stale(c.a, barsNow); got != c.want {
			t.Errorf("%s: stale = %v, want %v", c.name, got, c.want)
		}
	}
}
