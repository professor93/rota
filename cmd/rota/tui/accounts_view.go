package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/wire"
)

// accountsViewTitles are the table's column titles, by the ladder's keys.
// The provider's column has none, as in the design.
var accountsViewTitles = map[string]string{
	"place": "#", "id": "id", "account": "account", "provider": "",
	"5h": "5h", "7d": "7d", "reset": "reset", "state": "state",
}

// The details pane: each fact is indented, its label in a column of its
// own, and its value after a gap, wrapped under itself.
const (
	accountsViewIndent = 2
	accountsViewLabel  = 14
	accountsViewGap    = 2
	accountsViewValue  = accountsViewIndent + accountsViewLabel + accountsViewGap
)

// accountsViewSoon is how close to its end a login has to be before the
// state says so: three days, as `rota list`'s status does.
const accountsViewSoon = 72 * time.Hour

// View draws the table, then the selected account's details in the rows
// the table leaves. The columns depend on width alone (accountsLayout), so
// nothing that happens elsewhere, a session starting, reflows them.
func (a *accountsTab) View(width, height int) string {
	a.shown = 0
	if width <= 0 || height <= 0 {
		return ""
	}
	th := a.e.th
	switch {
	case !a.seen && a.failed != nil:
		return th.Over.Render(accountsViewLine("the accounts could not be read: "+clean(a.failed.Error()), width))
	case !a.seen:
		return th.Muted.Render(accountsViewLine("reading the accounts…", width))
	case len(a.rows) == 0:
		say := "No accounts yet."
		if a.e.control() {
			say += " n adds one."
		}
		return accountsViewLine(say, width)
	}
	cols := accountsLayout(width)
	a.scroll(height)
	now := a.e.now()
	lines := []string{th.Muted.Render(accountsViewHeader(cols))}
	for i := a.top; i < a.top+a.shown; i++ {
		lines = append(lines, a.row(cols, a.rows[i], a.rows[i].ID == a.sel, now))
	}
	if acct, ok := a.selected(); ok {
		if room := height - len(lines) - 1; room > 0 {
			lines = append(lines, accountsViewRule(th, width))
			lines = append(lines, a.details(acct, width, room, now)...)
		}
	}
	return strings.Join(lines, "\n")
}

// scroll is how many rows the table draws at height, at most half of it
// so the details keep the rest, and from which row, so that the selected
// one is drawn. The window moves only when the selection leaves it.
func (a *accountsTab) scroll(height int) {
	n := min(len(a.rows), max((height-2)/2, 1), max(height-1, 0))
	i := max(a.index(), 0)
	if i < a.top {
		a.top = i
	}
	if i >= a.top+n {
		a.top = i - n + 1
	}
	a.top = min(max(a.top, 0), max(len(a.rows)-n, 0))
	a.shown = n
}

// accountsViewHeader is the titles over the columns.
func accountsViewHeader(cols []col) string {
	var cells []string
	for _, c := range cols {
		if c.Shown {
			cells = append(cells, fit(accountsViewTitles[c.Key], c.W))
		}
	}
	return strings.TrimRight(strings.Join(cells, strings.Repeat(" ", ladderGap)), " ")
}

// row is one account's line of the table. Every cell is exactly its
// column's width, so no label shifts the next column. The columns before
// the bars are reversed on the selected row. A stale reading's numbers
// are Faint (R10).
func (a *accountsTab) row(cols []col, acct wire.Account, selected bool, now time.Time) string {
	th := a.e.th
	nums, resetStyle := th, th.Text
	if stale(acct, now) {
		nums, resetStyle = accountsViewFaint(th), th.Faint
	}
	var lead, rest []string
	for _, c := range cols {
		if !c.Shown {
			continue
		}
		switch c.Key {
		case "place":
			lead = append(lead, fit(accountsViewPlace(acct), c.W))
		case "id":
			lead = append(lead, fit(strconv.Itoa(acct.ID), c.W))
		case "account":
			lead = append(lead, fit(accountsLabel(acct), c.W))
		case "provider":
			lead = append(lead, fit(clean(acct.Provider), c.W))
		case "5h", "7d":
			rest = append(rest, accountsViewUsage(nums, acct, c.Key, c.W))
		case "reset":
			rest = append(rest, accountsViewCell(resetStyle, a.resetOf(acct, c.W, now), c.W))
		case "state":
			words, style := accountsViewState(th, acct, now, a.e.control())
			rest = append(rest, accountsViewCell(style, accountsViewWord(words, c.W), c.W))
		}
	}
	gap := strings.Repeat(" ", ladderGap)
	head := strings.Join(lead, gap)
	if selected {
		head = th.Selected.Render(head)
	}
	return strings.TrimRight(strings.Join(append([]string{head}, rest...), gap), " ")
}

// resetOf is when the window that decides the account resets (R9), in w
// columns: the countdown and the clock, or the countdown alone where both
// do not fit.
func (a *accountsTab) resetOf(acct wire.Account, w int, now time.Time) string {
	win, ok := deciding(acct)
	if !ok || !acct.Metered {
		return ""
	}
	at := barsTime(win.ResetsAt)
	s := resetText(at, now, a.e.o.Loc, false)
	if textWidth(s) > w {
		s = resetText(at, now, a.e.o.Loc, true)
	}
	return s
}

// accountsViewUsage is the cell of the window called name, w columns wide:
// its bar against the account's threshold, or why there is none. An API
// key has no usage to read, nor a provider that publishes none; usage not
// read yet is "?", and a reading without this window leaves it blank.
func accountsViewUsage(th Theme, acct wire.Account, name string, w int) string {
	switch {
	case acct.Credential == "apikey":
		return accountsViewCell(th.Muted, "api key", w)
	case !acct.Metered:
		return accountsViewCell(th.Muted, "n/a", w)
	case len(acct.Windows) == 0:
		return accountsViewCell(th.Muted, "?", w)
	}
	for _, win := range acct.Windows {
		if win.Name == name && !win.Scoped {
			return bar(th, win.Percent, acct.Threshold, w-barsFrame)
		}
	}
	return strings.Repeat(" ", w)
}

// accountsViewState is the state column: why the rotation passes the
// account by, or what is about to need doing, else ok. words are the ways
// to say it, the longest first: the cell says the first that fits
// (accountsViewWord). The last fits in 8 columns, the narrowest the ladder
// gives the state, so no state is ever cut.
func accountsViewState(th Theme, acct wire.Account, now time.Time, control bool) (words []string, style lipgloss.Style) {
	threshold := barsThreshold(acct.Threshold)
	credit := barsTime(acct.NoCreditUntil)
	switch {
	case acct.Status == rota.StatusReauth:
		if control {
			return []string{"dead: l signs in", "dead"}, th.Dead
		}
		return []string{"dead"}, th.Dead
	case credit.After(now):
		return []string{"no credit (" + accountsViewSpan(credit.Sub(now)) + ")", "no credit", "0 credit"}, th.Over
	case acct.Status == rota.StatusLimited:
		return []string{"limited"}, th.Over
	case acct.Metered && acct.Percent >= float64(threshold):
		return []string{fmt.Sprintf("over its %d%%", threshold), fmt.Sprintf("over %d%%", threshold), "over"}, th.Over
	}
	if t := barsTime(acct.LoginUntil); !t.IsZero() {
		switch left := t.Sub(now); {
		case left <= 0:
			return []string{"login ended", "ended"}, th.Warn
		case left <= accountsViewSoon:
			in := accountsViewSpan(left)
			return []string{"login ends in " + in, "ends in " + in, "ends " + in}, th.Warn
		}
	}
	if acct.Order < 1 {
		return []string{"out of rotation", "out"}, th.Muted
	}
	return []string{"ok"}, th.Good
}

// accountsViewWord is the first of words that fits in w columns, or the
// last cut to fit.
func accountsViewWord(words []string, w int) string {
	for _, s := range words {
		if textWidth(s) <= w {
			return s
		}
	}
	return fit(words[len(words)-1], w)
}

// details are the selected account's every window and setting, at most
// room lines of width.
func (a *accountsTab) details(acct wire.Account, width, room int, now time.Time) []string {
	th, loc := a.e.th, a.e.o.Loc
	nums, readStyle := th, th.Text
	if stale(acct, now) {
		nums, readStyle = accountsViewFaint(th), th.Faint
	}
	vw := width - accountsViewValue
	pad := strings.Repeat(" ", accountsViewIndent)
	label := func(s string) string {
		return pad + th.Muted.Render(fit(s, accountsViewLabel)) + strings.Repeat(" ", accountsViewGap)
	}
	// The title names the account; a label too long for it is cut first,
	// so the provider and the id still show.
	name, rest := accountsLabel(acct), "  "+clean(acct.Provider)+"  id "+strconv.Itoa(acct.ID)
	if left := width - textWidth(rest); textWidth(name) > left && left >= width/2 {
		name = strings.TrimRight(fit(name, left), " ")
	}
	out := []string{th.Title.Render(accountsViewLine(name+rest, width))}
	add := func(name, value string, style lipgloss.Style) {
		for i, l := range wrap(clean(value), vw) {
			head := strings.Repeat(" ", accountsViewValue)
			if i == 0 {
				head = label(name)
			}
			out = append(out, head+style.Render(l))
		}
	}

	// Every window, with its bar, its percent and when it resets: up to 16
	// cells, leaving the 18 columns the longest reset takes. A scoped
	// window covers one model and is not held to the threshold.
	cells := min(max(vw-barsFrame-2-18, 4), 16)
	for _, win := range acct.Windows {
		threshold := acct.Threshold
		if win.Scoped {
			threshold = 100
		}
		line := label(clean(win.Name)) + bar(nums, win.Percent, threshold, cells)
		if left := vw - cells - barsFrame - 2; left > 0 {
			line += "  " + readStyle.Render(strings.TrimRight(fit(resetText(barsTime(win.ResetsAt), now, loc, false), left), " "))
		}
		out = append(out, strings.TrimRight(line, " "))
	}
	if len(acct.Windows) == 0 {
		switch {
		case acct.Credential == "apikey":
			add("usage", "none: an API key is paid per token", th.Muted)
		case !acct.Metered:
			add("usage", "not read for this provider", th.Muted)
		default:
			add("usage", "not read yet", th.Muted)
		}
	}

	switch acct.Status {
	case rota.StatusReauth:
		why := "dead"
		if acct.DeadReason != "" {
			why += ": " + acct.DeadReason
		}
		add("status", why, th.Dead)
	case rota.StatusLimited:
		add("status", "limited", th.Over)
	default:
		add("status", "ok", th.Good)
	}
	add("threshold", strconv.Itoa(barsThreshold(acct.Threshold))+"%", th.Text)
	place := accountsViewPlace(acct)
	if place == "-" {
		place = "out of rotation"
	}
	add("place", place, th.Text)

	credential := "login"
	switch {
	case acct.Credential == "apikey":
		credential = "API key"
	case acct.LongUntil != "":
		credential = "login and long token"
	}
	add("credential", credential, th.Text)
	if t := barsTime(acct.LoginUntil); !t.IsZero() {
		add("login until", accountsViewUntil(t, now, loc), th.Text)
	}
	if t := barsTime(acct.LongUntil); !t.IsZero() {
		add("long until", accountsViewUntil(t, now, loc), th.Text)
	}
	if t := barsTime(acct.NoCreditUntil); t.After(now) {
		add("no credit", resetText(t, now, loc, false), th.Over)
	}

	checked := "n/a"
	if acct.Metered {
		checked = "never"
		if t := barsTime(acct.CheckedAt); !t.IsZero() {
			checked = accountsViewAgo(now.Sub(t))
		}
	}
	if t := barsTime(acct.UsageWaitUntil); t.After(now) {
		checked += "; read again in " + resetText(t, now, loc, false) + ", as the provider asked"
	}
	add("checked", checked, readStyle)

	add("folder", accountsViewOr(acct.Cwd, "not set"), th.Text)
	add("config", accountsViewOr(acct.ConfigDir, "not set"), th.Text)
	add("sessions", accountsViewOr(acct.Sessions, "shared"), th.Text)
	remote := "off"
	if acct.RemoteControl {
		remote = "on"
	}
	add("remote control", remote, th.Text)
	if acct.Note != "" {
		add("note", acct.Note, th.Muted)
	}
	if len(out) > room && room > 1 {
		more := len(out) - room + 1
		out = append(out[:room-1], th.Muted.Render(accountsViewLine(pad+"… "+strconv.Itoa(more)+" more", width)))
	}
	if len(out) > room {
		out = out[:room]
	}
	return out
}

// accountsLabel is what names an account: what a person called it, else
// its email, else the start of its uuid, else its id. It is cleaned, so
// whatever a provider put in it is drawn as text.
func accountsLabel(acct wire.Account) string {
	for _, s := range []string{acct.Name, acct.Email} {
		if c := clean(s); strings.TrimSpace(c) != "" {
			return c
		}
	}
	if u := clean(acct.UUID); u != "" {
		return accountsViewLine(u, 12)
	}
	return "account-" + strconv.Itoa(acct.ID)
}

// accountsViewPlace is the account's place in the rotation, or "-" when it
// is out of it: a 0 would read as first.
func accountsViewPlace(acct wire.Account) string {
	if acct.Order < 1 {
		return "-"
	}
	return strconv.Itoa(acct.Order)
}

// accountsViewCell is text in style, exactly w columns wide: cut to fit,
// and padded with spaces outside the style.
func accountsViewCell(style lipgloss.Style, text string, w int) string {
	f := fit(text, w)
	body := strings.TrimRight(f, " ")
	if body == "" {
		return f
	}
	return style.Render(body) + strings.Repeat(" ", textWidth(f)-textWidth(body))
}

// accountsViewLine is s on one line of at most w columns.
func accountsViewLine(s string, w int) string { return strings.TrimRight(fit(s, w), " ") }

// accountsViewRule is the line between the table and the details.
func accountsViewRule(th Theme, w int) string {
	g := "─"
	if textWidth(g) != 1 {
		g = "-"
	}
	return th.Rule.Render(strings.Repeat(g, w))
}

// accountsViewFaint is th with every level and the muted parts of a bar
// Faint: what a stale reading is drawn with.
func accountsViewFaint(th Theme) Theme {
	f := th
	f.Good, f.Warn, f.Over, f.Muted = th.Faint, th.Faint, th.Faint, th.Faint
	return f
}

// accountsViewSpan is d in its largest unit, rounded down: "2d", "3h",
// "40m", and never less than "1m".
func accountsViewSpan(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(max(int(d/time.Minute), 1)) + "m"
}

// accountsViewAgo is how long ago a reading was, as `rota list` says it.
func accountsViewAgo(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	return accountsViewSpan(d) + " ago"
}

// accountsViewUntil is when a login or a token ends, and how long is left.
func accountsViewUntil(t, now time.Time, loc *time.Location) string {
	at := t.In(loc).Format("2006-01-02 15:04")
	if left := t.Sub(now); left > 0 {
		return at + " (" + accountsViewSpan(left) + " left)"
	}
	return at + " (ended)"
}

func accountsViewOr(s, none string) string {
	if s == "" {
		return none
	}
	return s
}
