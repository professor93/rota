package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/cmd/rota/client"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/wire"
)

// The answer as it builds up from the stream's events, and what the Ask
// tab draws: the answer above the prompt, and under the prompt the line
// that says who is asked and what for.

// askDot separates the parts of a line the tab says about a run.
const askDot = "·"

// askStripMin is the least room the readings are given on the bottom line
// before the account is said more briefly.
const askStripMin = 24

// askKind is what one piece of the answer is.
type askKind int

const (
	askQuestion askKind = iota // what was asked, or sent into an open run
	askText                    // what the answer said
	askTool                    // a tool it called
	askNote                    // a stop or an interrupt
	askFail                    // what went wrong
	askEnd                     // how the run ended
	askEndBad                  // how it ended, when that was a failure
	askJSON                    // the readings asked for
)

type askPiece struct {
	kind askKind
	text string
}

// event takes one message of the answer and waits for the next.
func (a *askTab) event(msg askEventMsg) tea.Cmd {
	r := a.run
	if r == nil || msg.id != r.id {
		return nil
	}
	if msg.last {
		a.ended(r, msg)
		return nil
	}
	ev := msg.event
	switch ev.Type {
	case "init":
		if ev.RunID != "" {
			r.open = ev.RunID
		}
		if ev.Account != 0 {
			r.account = ev.Account
		}
	case "text":
		a.text(ev)
	case "tool":
		a.say(askTool, "tool "+ev.Tool)
	case "interrupted":
		a.say(askNote, "interrupted")
	case "idle":
		r.answering = false
	case "input":
		if ev.State == "failed" {
			a.say(askFail, cmp.Or(ev.Reason, "a message was not taken"))
		}
	case "error":
		a.say(askFail, cmp.Or(ev.Text, "the CLI said the turn failed"))
	}
	return askWait(r)
}

// text is a text event, by eventStream's rule: a fragment is shown as it
// comes, and the whole piece after it is skipped when the fragments said
// it already.
func (a *askTab) text(ev message.Event) {
	if ev.Text == "" {
		return
	}
	switch rest, seen := strings.CutPrefix(a.shown, ev.Text); {
	case ev.Delta:
		a.shown += ev.Text
	case seen:
		a.shown = rest
		return
	default:
		a.shown = ""
	}
	if n := len(a.pieces); n > 0 && a.pieces[n-1].kind == askText {
		a.pieces[n-1].text += ev.Text
		a.dirty = true
		return
	}
	a.say(askText, ev.Text)
}

// ended is the stream's end: the end line and the readings, a stop, the
// server's refusal under the prompt, or why the stream was cut.
func (a *askTab) ended(r *askRun, msg askEventMsg) {
	a.run = nil
	r.cancel()
	var no *client.Refusal
	switch err := msg.err; {
	case err == nil:
		a.end(r, msg.end)
	case r.stopping && errors.Is(err, context.Canceled):
		a.say(askNote, "stopped")
	case errors.As(err, &no):
		a.pieces, a.dirty = nil, true
		a.refusal = err.Error()
		if strings.TrimSpace(a.area.Value()) == "" {
			a.area.SetValue(r.prompt)
		}
	default:
		a.say(askFail, err.Error())
	}
}

// end says how the run ended, #account · duration · $cost · exit code, then
// its error, then the readings asked for as indented JSON.
func (a *askTab) end(r *askRun, end wire.End) {
	var parts []string
	if acct := cmp.Or(end.Account, r.account); acct != 0 {
		parts = append(parts, fmt.Sprintf("#%d", acct))
	}
	if end.DurationMS > 0 {
		parts = append(parts, askDuration(end.DurationMS))
	}
	if end.CostUSD > 0 {
		parts = append(parts, askCost(end.CostUSD))
	}
	if end.Type == "done" || end.ExitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit %d", end.ExitCode))
	}
	kind := askEnd
	if end.Type != "done" || end.ExitCode != 0 || end.IsError {
		kind = askEndBad
	}
	if len(parts) > 0 {
		a.say(kind, strings.Join(parts, " "+askDot+" "))
	}
	if end.Error != "" {
		a.say(askFail, end.Error)
	}
	if j := askReadings(end.Readings); j != "" {
		a.say(askJSON, j)
	}
}

// say adds a piece to the answer.
func (a *askTab) say(kind askKind, text string) {
	a.pieces = append(a.pieces, askPiece{kind: kind, text: text})
	a.dirty = true
}

// View draws the answer, a rule, the prompt, a refusal if there is one,
// and the bottom line, in width columns and at most height rows; when
// there is less room than that, the top of the answer goes first.
func (a *askTab) View(width, height int) string {
	th := a.e.th
	if width <= 0 || height <= 0 {
		return ""
	}
	if a.watching() {
		var out []string
		for _, l := range wrap(clean(askWatchWords), width) {
			out = append(out, th.Muted.Render(l))
		}
		return strings.Join(out, "\n")
	}
	var below []string
	for _, l := range strings.Split(a.area.View(), "\n") {
		below = append(below, ansi.TruncateWc(l, width, ""))
	}
	if a.refusal != "" {
		lines := wrap(clean(a.refusal), width)
		for _, l := range lines[:min(len(lines), 2)] {
			below = append(below, th.Over.Render(l))
		}
	}
	below = append(below, a.bottom(width))
	rule := th.Rule.Render(strings.Repeat("─", width/max(textWidth("─"), 1)))
	lines := append(a.answer(width, max(height-len(below)-1, 0)), rule)
	lines = append(lines, below...)
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	return strings.Join(lines, "\n")
}

// answer is the h rows of the answer the viewport shows: its end while it
// follows, or where the person paged to.
func (a *askTab) answer(w, h int) []string {
	lines := a.answerLines(w)
	if len(a.pieces) == 0 {
		lines = nil
		for _, l := range wrap(clean(askHint), w) {
			lines = append(lines, a.e.th.Faint.Render(l))
		}
	}
	a.vp.SetWidth(w)
	a.vp.SetHeight(h)
	a.vp.SetContentLines(lines)
	if a.follow {
		a.vp.GotoBottom()
	}
	out := make([]string, h)
	for i, off := 0, a.vp.YOffset(); i < h && off+i < len(lines); i++ {
		out[i] = lines[off+i]
	}
	return out
}

// answerLines is every piece of the answer laid out in w columns, kept
// until the answer or the width changes.
func (a *askTab) answerLines(w int) []string {
	if !a.dirty && a.linesW == w {
		return a.lines
	}
	th := a.e.th
	var out []string
	add := func(st lipgloss.Style, lines []string) {
		for _, l := range lines {
			out = append(out, st.Render(l))
		}
	}
	for i, p := range a.pieces {
		switch p.kind {
		case askQuestion:
			if i > 0 {
				out = append(out, "")
			}
			for j, l := range askWrap(p.text, w-2) {
				lead := "  "
				if j == 0 {
					lead = th.Accent.Render("> ")
				}
				out = append(out, lead+th.Text.Render(l))
			}
		case askText:
			add(th.Text, askWrap(p.text, w))
		case askTool, askNote:
			add(th.Muted, askWrap(askDot+" "+p.text, w))
		case askFail, askEndBad:
			add(th.Over, askWrap(p.text, w))
		case askEnd, askJSON:
			add(th.Muted, askWrap(p.text, w))
		}
	}
	a.lines, a.linesW, a.dirty = out, w, false
	return out
}

// bottom is the line under the prompt: the account asked, the readings
// asked for, and whether the run is kept open, in w columns. The account
// is said more briefly when the readings would have too little room.
func (a *askTab) bottom(w int) string {
	th := a.e.th
	keep, keepStyle := "keep open: off", th.Muted
	if a.keepOpen {
		keep = "keep open: on"
	}
	if r := a.run; r != nil && r.open != "" {
		keep, keepStyle = "run open", th.Accent
	}
	const label = "account: "
	says := a.accountSays()
	who := says[len(says)-1]
	for _, s := range says {
		if w-textWidth(label+s)-textWidth(keep)-4 >= askStripMin {
			who = s
			break
		}
	}
	whoStyle := th.Text
	if a.focus == askOnAccount {
		whoStyle = th.Selected
	}
	strip, stripW := a.strip(w - textWidth(label+who) - textWidth(keep) - 4)
	gap := max(w-textWidth(label+who)-2-stripW-textWidth(keep), 2)
	line := th.Muted.Render(label) + whoStyle.Render(who) + "  " + strip + strings.Repeat(" ", gap) + keepStyle.Render(keep)
	return ansi.TruncateWc(line, w, "")
}

// accountSays is the account asked, from the longest way of saying it to
// the shortest: "auto (now #1 work@example.com)", with the label cut
// shorter, then without it, then "auto".
func (a *askTab) accountSays() []string {
	id, lead := a.account, ""
	if id == 0 {
		id, lead = a.e.list.Default, "auto"
		if id == 0 {
			return []string{lead}
		}
	}
	num := "#" + strconv.Itoa(id)
	var says []string
	for _, acct := range a.e.list.Accounts {
		if label := clean(cmp.Or(acct.Name, acct.Email)); acct.ID == id && label != "" {
			says = append(says, num+" "+askCut(label, 16), num+" "+askCut(label, 10))
		}
	}
	says = append(says, num)
	if lead == "" {
		return says
	}
	for i, s := range says {
		says[i] = lead + " (now " + s + ")"
	}
	return append(says, lead)
}

// strip is the readings, as "with: [x] blocks [ ] ask …", in at most w
// columns, the one under the cursor among them; and how wide it is.
func (a *askTab) strip(w int) (string, int) {
	th := a.e.th
	const head, more = "with: ", "…"
	room := w - textWidth(head)
	items := make([]string, len(message.Names))
	for i, n := range message.Names {
		mark := "[ ] "
		if a.with[i] {
			mark = "[x] "
		}
		items[i] = mark + n
	}
	start := 0
	end := askFit(items, start, room, textWidth(more))
	for a.cursor >= end && start < a.cursor {
		start++
		end = askFit(items, start, room, textWidth(more))
	}
	if end <= start {
		return "", 0
	}
	var b strings.Builder
	used := 0
	write := func(s string, st lipgloss.Style) {
		b.WriteString(st.Render(s))
		used += textWidth(s)
	}
	write(head, th.Muted)
	if start > 0 {
		write(more+" ", th.Muted)
	}
	for i := start; i < end; i++ {
		if i > start {
			write(" ", th.Text)
		}
		st := th.Text
		if a.focus == askOnReadings && i == a.cursor {
			st = th.Selected
		}
		write(items[i], st)
	}
	if end < len(items) {
		write(" "+more, th.Muted)
	}
	return b.String(), used
}

// askFit is where the items from start stop fitting in room columns, with
// room kept for the marks that say more are hidden on either side.
func askFit(items []string, start, room, mark int) int {
	used := 0
	if start > 0 {
		used = mark + 1
	}
	i := start
	for ; i < len(items); i++ {
		need := textWidth(items[i])
		if i > start {
			need++
		}
		tail := 0
		if i < len(items)-1 {
			tail = 1 + mark
		}
		if used+need+tail > room {
			break
		}
		used += need
	}
	return i
}

// askWrap is text laid out in w columns, cleaned line by line so its own
// line breaks stay: a line keeps its indentation, on what it wraps onto
// too, and the spaces inside it.
func askWrap(text string, w int) []string {
	w = max(w, 1)
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(clean(strings.ReplaceAll(line, "\t", "    ")), " ")
		indent := min(len(line)-len(strings.TrimLeft(line, " ")), w/2)
		for _, l := range askBreak(line[indent:], w-indent) {
			out = append(out, line[:indent]+l)
		}
	}
	return out
}

// askBreak is one line cut into pieces of at most w columns, after the last
// space that fits, or inside a word longer than a line.
func askBreak(line string, w int) []string {
	var out []string
	for textWidth(line) > w {
		head, _ := textHead(line, w)
		if rest := line[len(head):]; rest != "" && rest[0] != ' ' {
			if sp := strings.LastIndexByte(head, ' '); sp > 0 && strings.TrimSpace(head[:sp]) != "" {
				head = head[:sp]
			}
		}
		out = append(out, strings.TrimRight(head, " "))
		line = strings.TrimLeft(line[len(head):], " ")
		if line == "" {
			return out
		}
	}
	return append(out, line)
}

// askCut is s cut to at most n columns, with … where it was cut.
func askCut(s string, n int) string {
	if textWidth(s) <= n {
		return s
	}
	return strings.TrimRight(fit(s, n), " ")
}

// askDuration is how long a run took: 2.3s under a minute, 1m3s from there.
func askDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	}
	return d.Round(time.Second).String()
}

// askCost is what a run cost, in dollars: to the cent from a dollar up,
// and to a hundredth of a cent below.
func askCost(usd float64) string {
	if usd >= 1 {
		return fmt.Sprintf("$%.2f", usd)
	}
	return fmt.Sprintf("$%.4f", usd)
}

// askReadings is the readings asked for, as indented JSON, or "" when none
// came.
func askReadings(r message.Readings) string {
	raw, err := rota.EncodeIndent(r)
	if err != nil || string(raw) == "{}" {
		return ""
	}
	return string(raw)
}
