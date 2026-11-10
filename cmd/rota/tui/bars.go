package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/professor93/rota/wire"
)

// barsFrame is the columns a bar takes beyond its cells: the two ends, a
// space, and the percent in four.
const barsFrame = 7

// barsGlyphs are what a bar is drawn with: its two ends, a filled cell, an
// empty one and the tick.
type barsGlyphs struct{ left, full, empty, tick, right string }

// barsBlocks are the glyphs of R33, each one column where ambiguous
// characters are drawn narrow; barsASCII are the ones drawn instead where
// they are not (RUNEWIDTH_EASTASIAN=1), so a bar keeps its width.
var (
	barsBlocks = barsGlyphs{"▕", "█", "░", "│", "▏"}
	barsASCII  = barsGlyphs{"[", "#", "-", "|", "]"}
)

// barsDrawn is the glyphs this terminal draws a bar with: the blocks while
// every one of them is one column to the measure, ASCII otherwise.
func barsDrawn() barsGlyphs {
	g := barsBlocks
	if textWidth(g.left+g.full+g.empty+g.tick+g.right) != 5 {
		return barsASCII
	}
	return g
}

// barsStale is how old a reading may be before it is drawn faint (R10):
// five of the daemon's sweeps.
const barsStale = 10 * time.Minute

// The levels a reading is at against its account's own threshold (R8).
const (
	lvGood = iota // below 0.8 of the threshold
	lvWarn        // from 0.8 of it
	lvOver        // at it or past it
)

// barsThreshold is the threshold an account is judged against: what the
// store holds, and 100 where it holds none.
func barsThreshold(threshold int) int {
	if threshold <= 0 || threshold > 100 {
		return 100
	}
	return threshold
}

// barsPercent is pct as a bar draws it: from 0 to 100, and 0 for a reading
// that is not a number.
func barsPercent(pct float64) float64 {
	if math.IsNaN(pct) {
		return 0
	}
	return min(max(pct, 0), 100)
}

// level is how close pct is to threshold: lvGood below 0.8 of it, lvWarn
// below it, lvOver at it or past it. The colour is relative to the account's
// own threshold, not a fixed 60/80, because that is rota's idea of a limit.
func level(pct float64, threshold int) int {
	switch r := barsPercent(pct) / float64(barsThreshold(threshold)); {
	case r >= 1:
		return lvOver
	case r >= 0.8:
		return lvWarn
	}
	return lvGood
}

// barsStyle is the style of a level.
func barsStyle(th Theme, lv int) lipgloss.Style {
	switch lv {
	case lvOver:
		return th.Over
	case lvWarn:
		return th.Warn
	}
	return th.Good
}

// bar draws pct in cells cells, with the account's threshold as a tick, and
// the percent after it: ▕██████████░░│░░░▏  62% (R8, R33). It takes
// cells+barsFrame columns, whatever the terminal: where ambiguous
// characters are drawn wide it draws [######--|---]  62% (barsDrawn).
//
// round(pct/100*cells) cells are filled with █ and the rest are ░, Faint.
// The tick │ sits at round(threshold/100*cells), kept inside the bar, and is
// left out at threshold 100, where the end of the bar is the limit. The
// filled cells and the percent take the level's style, and so does the tick
// once the reading has reached it, even where rounding left it just past
// the fill. A tick not reached is Muted. The percent is rounded down, as
// the level is judged on the reading itself: 74.6 against 75 reads 74%, not
// the threshold the colour says it is below.
func bar(th Theme, pct float64, threshold, cells int) string {
	cells = max(cells, 0)
	p := barsPercent(pct)
	t := barsThreshold(threshold)
	lv := level(p, t)
	style, reached := barsStyle(th, lv), lv == lvOver
	filled := int(math.Round(p / 100 * float64(cells)))
	tick := -1
	if t < 100 && cells > 0 {
		tick = min(max(int(math.Round(float64(t)/100*float64(cells))), 0), cells-1)
	}
	g := barsDrawn()
	var b strings.Builder
	b.WriteString(g.left)
	run := func(s lipgloss.Style, text string, n int) {
		if n > 0 {
			b.WriteString(s.Render(strings.Repeat(text, n)))
		}
	}
	for at := 0; at < cells; {
		switch {
		case at == tick:
			s := th.Muted
			if at < filled || reached {
				s = style
			}
			run(s, g.tick, 1)
			at++
		case at < filled:
			end := filled
			if tick > at && tick < end {
				end = tick
			}
			run(style, g.full, end-at)
			at = end
		default:
			end := cells
			if tick > at {
				end = tick
			}
			run(th.Faint, g.empty, end-at)
			at = end
		}
	}
	b.WriteString(g.right + " ")
	b.WriteString(style.Render(fmt.Sprintf("%3.0f%%", math.Floor(p))))
	return b.String()
}

// resetText is when at comes, seen from now: a countdown and the clock in
// loc, "1h 3m (21:20)", from a day on with the weekday, "2d 1h (Thu
// 21:20)", and under a minute "<1m (21:20)" (R9). short gives the countdown
// alone, "1h 3m", at most 8 columns. A time passed is "now", and the zero
// time "". A window resets within a week, so the longest is "6d 23h (Tue
// 19:16)", 18 columns, the reset column's width; anything longer is cut by
// fit. The TUI takes ambiguous characters as narrow (R33); the separator
// is not "·" by the Group A ruling.
func resetText(at, now time.Time, loc *time.Location, short bool) string {
	if at.IsZero() {
		return ""
	}
	d := at.Sub(now)
	if d <= 0 {
		return "now"
	}
	var in string
	switch days, hours, mins := int(d/(24*time.Hour)), int(d/time.Hour)%24, int(d/time.Minute)%60; {
	case days > 99:
		in = ">99d"
	case days > 0:
		in = fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		in = fmt.Sprintf("%dh %dm", hours, mins)
	case mins > 0:
		in = fmt.Sprintf("%dm", mins)
	default:
		in = "<1m"
	}
	if short {
		return in
	}
	if loc == nil {
		loc = time.Local
	}
	clock := "15:04"
	if d >= 24*time.Hour {
		clock = "Mon 15:04"
	}
	return in + " (" + at.In(loc).Format(clock) + ")"
}

// barsTime is an RFC 3339 time from the API, and the zero time for none.
func barsTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// deciding is the window that decides when the account is usable again
// (R9): the latest to reset of the windows at or over the account's
// threshold, and with none there, the highest. A scoped window covers one
// model and never decides, as it never holds the rotation back. ok is false
// when no window can decide.
func deciding(a wire.Account) (w wire.Window, ok bool) {
	t := float64(barsThreshold(a.Threshold))
	for _, c := range a.Windows {
		if c.Scoped {
			continue
		}
		if !ok {
			w, ok = c, true
			continue
		}
		cOver, wOver := c.Percent >= t, w.Percent >= t
		switch {
		case cOver && !wOver:
			w = c
		case cOver && wOver:
			if barsTime(c.ResetsAt).After(barsTime(w.ResetsAt)) {
				w = c
			}
		case !cOver && !wOver && c.Percent > w.Percent:
			w = c
		}
	}
	return w, ok
}

// stale is whether a's reading is too old to stand as it is (R10): it is
// older than ten minutes, was never taken, or the provider asked rota to
// wait, so the number shown is from before. An account with no usage to
// read has no reading to be stale.
func stale(a wire.Account, now time.Time) bool {
	if !a.Metered {
		return false
	}
	if a.UsageWaitUntil != "" {
		return true
	}
	at := barsTime(a.CheckedAt)
	return at.IsZero() || now.Sub(at) > barsStale
}
