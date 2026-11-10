package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/wire"
)

// The Ask tab sends one request, through the rotation or to an account the
// person picks, and streams the answer above the prompt (R25-R27). A run
// kept open takes more messages until it is closed, and quitting closes it.

func init() { register(tabsAsk, askNew) }

// The prompt is three rows high and grows to six with what is typed; what
// is typed past that scrolls inside it, up to askAreaRows rows in all.
const (
	askAreaMin  = 3
	askAreaMax  = 6
	askAreaRows = 10000
	// askBuffer is how many events a stream may be ahead of the screen.
	askBuffer = 256
)

// The words the tab says of itself.
const (
	askWatchWords = "asking runs an account, which the watch role may not do"
	askHint       = "enter writes a question, and enter again asks it: of the account the rotation picks, or of the one chosen below"
	askAreaRest   = "enter to write"
	askAreaTyping = "a question; enter asks, alt+enter starts a new line, esc leaves"
)

// askKeys are the Ask tab's keys. Every one but Focus and BackFocus is the
// control role's; PageUp and PageDown page through the answer.
type askKeys struct {
	Send, Newline, Focus, BackFocus, Left, Right, Toggle, KeepOpen, Stop, CloseRun key.Binding
	PageUp, PageDown                                                               key.Binding
}

// askBaseKeys are the bindings as made, every one enabled.
func askBaseKeys() askKeys {
	return askKeys{
		Send:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "ask")),
		Newline:   key.NewBinding(key.WithKeys("alt+enter", "shift+enter"), key.WithHelp("alt+enter", "new line")),
		Focus:     key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next field")),
		BackFocus: key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "previous field")),
		Left:      key.NewBinding(key.WithKeys("left"), key.WithHelp("←", "previous")),
		Right:     key.NewBinding(key.WithKeys("right"), key.WithHelp("→", "next")),
		Toggle:    key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "toggle reading")),
		KeepOpen:  key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "keep open")),
		Stop:      key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "stop")),
		CloseRun:  key.NewBinding(key.WithKeys("ctrl+x"), key.WithHelp("ctrl+x", "close run")),
		PageUp:    key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "page up")),
		PageDown:  key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdown", "page down")),
	}
}

// askFocus is the part of the tab that has focus.
type askFocus int

const (
	askOnPrompt askFocus = iota
	askOnAccount
	askOnReadings
	askFocusParts
)

// askEventMsg is one event of the answer, or, with last, how the stream
// ended: end, or err when it did not end as a run ends.
type askEventMsg struct {
	id    int
	event message.Event
	last  bool
	end   wire.End
	err   error
}

// askCallMsg is how a call into an open run went. text is the message a
// send carried, given back to the field if the run would not take it.
type askCallMsg struct {
	id   int
	text string
	err  error
}

// askRun is one Ask in flight. Update alone touches its plain fields; the
// stream's goroutine writes what it reads into ch, and runID under mu, so
// Leave can close an open run that Update has not heard of yet.
type askRun struct {
	id     int
	ctx    context.Context
	cancel context.CancelFunc
	ch     chan askEventMsg
	// input says the run was asked to stay open, and open is its id once
	// its opening event said it.
	input bool
	open  string
	// account is the one the opening event named.
	account int
	// answering is true until the run is idle, or has ended; stopping says
	// esc cancelled it.
	answering, stopping bool
	// prompt is what was asked, given back to the field if it is refused.
	prompt string

	mu    sync.Mutex
	runID string
}

// push hands m to the screen, or drops it once nobody is reading: the
// answer was stopped, or the program has ended.
func (r *askRun) push(m askEventMsg) {
	select {
	case r.ch <- m:
		return
	default:
	}
	select {
	case r.ch <- m:
	case <-r.ctx.Done():
	}
}

// askTab is the Ask tab.
type askTab struct {
	e    *env
	area textarea.Model
	// vp is the answer's scroll: View sets its size and lines, Update pages
	// it. follow says it keeps to the end as the answer grows.
	vp     viewport.Model
	follow bool

	focus askFocus
	// account is the one chosen, 0 for the rotation's choice.
	account int
	// with is each of message.Names, asked for or not; cursor is the one
	// under the cursor.
	with     []bool
	cursor   int
	keepOpen bool

	pieces []askPiece
	// shown is what fragments have shown of a piece still to arrive whole
	// (eventStream's rule).
	shown string
	// lines is the answer laid out for linesW columns, until it changes.
	lines  []string
	linesW int
	dirty  bool

	run     *askRun
	seq     int
	refusal string
}

func askNew(e *env) tab {
	a := &askTab{e: e, vp: viewport.New(), follow: true, with: make([]bool, len(message.Names)), dirty: true}
	a.vp.KeyMap = viewport.KeyMap{}
	a.area = askArea(e.th)
	a.area.SetWidth(max(e.width, 20))
	return a
}

// askArea is the prompt: newArea's field, three rows high and growing to
// six, taking a new line on Newline's keys rather than on enter.
func askArea(th Theme) textarea.Model {
	a := newArea(th)
	a.ShowLineNumbers = false
	a.SetPromptFunc(2, func(p textarea.PromptInfo) string {
		if p.LineNumber == 0 {
			return "> "
		}
		return "  "
	})
	a.DynamicHeight = true
	a.MinHeight, a.MaxHeight, a.MaxContentHeight = askAreaMin, askAreaMax, askAreaRows
	a.CharLimit = 0
	a.KeyMap.InsertNewline.SetKeys(askBaseKeys().Newline.Keys()...)
	a.Placeholder = askAreaRest
	a.SetHeight(askAreaMin)
	return a
}

func (a *askTab) Title() string { return "Ask" }

// watching says the principal is known, and may not ask.
func (a *askTab) watching() bool { return a.e.who.Role != "" && !a.e.control() }

// editing says the prompt has focus and takes what is typed.
func (a *askTab) editing() bool { return a.area.Focused() }

func (a *askTab) Typing() bool { return a.editing() }

func (a *askTab) Busy() string {
	switch r := a.run; {
	case r == nil:
		return ""
	case r.open != "":
		return "a run is open"
	}
	return "an answer is streaming"
}

// Leave closes a run kept open, which outlives its stream until it is
// closed, and stops an answer still streaming.
func (a *askTab) Leave(ctx context.Context) {
	r := a.run
	if r == nil {
		return
	}
	r.mu.Lock()
	id := r.runID
	r.mu.Unlock()
	if id != "" {
		_ = a.e.b.CloseRun(ctx, id)
	}
	r.cancel()
}

func (a *askTab) Update(msg tea.Msg) (tab, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return a, a.key(msg)
	case tea.PasteMsg:
		if !a.e.control() {
			return a, nil
		}
		cmd := a.edit()
		var more tea.Cmd
		a.area, more = a.area.Update(msg)
		return a, tea.Batch(cmd, more)
	case tea.MouseWheelMsg:
		a.vp, _ = a.vp.Update(msg)
		a.follow = a.vp.AtBottom()
	case tea.WindowSizeMsg:
		a.area.SetWidth(msg.Width)
	case tea.BackgroundColorMsg:
		a.area.SetStyles(askArea(a.e.th).Styles())
	case askEventMsg:
		return a, a.event(msg)
	case askCallMsg:
		a.called(msg)
	}
	return a, nil
}

// keys are the bindings as they stand: the control role's, each enabled
// where it does something now.
func (a *askTab) keys() askKeys {
	k := askBaseKeys()
	a.e.gate(&k.Send, &k.Newline, &k.Left, &k.Right, &k.Toggle, &k.KeepOpen, &k.Stop, &k.CloseRun, &k.PageUp, &k.PageDown)
	r, editing := a.run, a.editing()
	only := func(b *key.Binding, now bool) {
		if !now {
			b.SetEnabled(false)
		}
	}
	switch {
	case !editing:
		k.Send.SetHelp("enter", "write")
	case r != nil && r.open != "":
		k.Send.SetHelp("enter", "send")
		only(&k.Send, !r.stopping)
	default:
		only(&k.Send, r == nil)
	}
	only(&k.Newline, editing)
	if a.focus == askOnAccount {
		k.Left.SetHelp("←", "account")
		k.Right.SetHelp("→", "account")
	} else {
		k.Left.SetHelp("←", "reading")
		k.Right.SetHelp("→", "reading")
	}
	only(&k.Left, a.focus != askOnPrompt)
	only(&k.Right, a.focus != askOnPrompt)
	only(&k.Toggle, a.focus == askOnReadings)
	only(&k.KeepOpen, r == nil || r.open == "")
	switch {
	case a.answering():
	case editing:
		k.Stop.SetHelp("esc", "leave field")
	default:
		k.Stop.SetEnabled(false)
	}
	only(&k.CloseRun, r != nil && r.open != "")
	only(&k.PageUp, len(a.pieces) > 0)
	only(&k.PageDown, len(a.pieces) > 0)
	return k
}

func (a *askTab) Keys() []key.Binding {
	k := a.keys()
	return []key.Binding{k.Send, k.Newline, k.Focus, k.BackFocus, k.Left, k.Right, k.Toggle, k.KeepOpen, k.Stop, k.CloseRun, k.PageUp, k.PageDown}
}

// answering says esc would stop something: a one-shot answer still
// streaming, or an open run working on a message.
func (a *askTab) answering() bool {
	r := a.run
	if r == nil || r.stopping {
		return false
	}
	if r.input {
		return r.open != "" && r.answering
	}
	return true
}

func (a *askTab) key(msg tea.KeyPressMsg) tea.Cmd {
	k := a.keys()
	switch {
	case key.Matches(msg, k.Focus):
		return a.move(1)
	case key.Matches(msg, k.BackFocus):
		return a.move(-1)
	case key.Matches(msg, k.Stop):
		return a.stop()
	case key.Matches(msg, k.Send):
		return a.send()
	case key.Matches(msg, k.KeepOpen):
		a.keepOpen = !a.keepOpen
	case key.Matches(msg, k.CloseRun):
		open := a.run.open
		return a.call(a.run.id, "", func(ctx context.Context, b Backend) error { return b.CloseRun(ctx, open) })
	case key.Matches(msg, k.PageUp):
		a.vp.PageUp()
		a.follow = a.vp.AtBottom()
	case key.Matches(msg, k.PageDown):
		a.vp.PageDown()
		a.follow = a.vp.AtBottom()
	case key.Matches(msg, k.Left):
		a.step(-1)
	case key.Matches(msg, k.Right):
		a.step(1)
	case key.Matches(msg, k.Toggle):
		a.with[a.cursor] = !a.with[a.cursor]
	case a.editing():
		// enter is the Send key's even when nothing can be sent yet: it
		// never reaches the field.
		if key.Matches(msg, askBaseKeys().Send) {
			return nil
		}
		var cmd tea.Cmd
		a.area, cmd = a.area.Update(msg)
		return cmd
	}
	return nil
}

// move takes the focus to the next part, or the one before. Arriving at
// the prompt takes it up, for the control role alone.
func (a *askTab) move(d int) tea.Cmd {
	a.focus = (a.focus + askFocus(d) + askFocusParts) % askFocusParts
	if a.focus == askOnPrompt && a.e.control() {
		return a.edit()
	}
	a.rest()
	return nil
}

// edit takes the prompt up, wherever the focus was.
func (a *askTab) edit() tea.Cmd {
	a.focus = askOnPrompt
	a.area.Placeholder = askAreaTyping
	return a.area.Focus()
}

// rest puts the prompt down: what is typed reaches the shell again.
func (a *askTab) rest() {
	a.area.Blur()
	a.area.Placeholder = askAreaRest
}

// step moves the account or the reading under the cursor by d.
func (a *askTab) step(d int) {
	if a.focus == askOnReadings {
		a.cursor = min(max(a.cursor+d, 0), len(a.with)-1)
		return
	}
	ids := []int{0}
	for _, acct := range a.e.list.Accounts {
		ids = append(ids, acct.ID)
	}
	i := 0
	for j, id := range ids {
		if id == a.account {
			i = j
		}
	}
	a.account = ids[(i+d+len(ids))%len(ids)]
}

// stop is esc: it cancels a one-shot answer, interrupts an open run that
// is working, and otherwise leaves the field.
func (a *askTab) stop() tea.Cmd {
	r := a.run
	switch {
	case a.answering() && !r.input:
		r.stopping = true
		r.cancel()
	case a.answering():
		open := r.open
		return a.call(r.id, "", func(ctx context.Context, b Backend) error { return b.InterruptRun(ctx, open) })
	case a.editing():
		a.rest()
	}
	return nil
}

// send is enter: it takes the prompt up when it is down, sends what it
// holds into an open run, or asks it.
func (a *askTab) send() tea.Cmd {
	if !a.editing() {
		return a.edit()
	}
	text := strings.TrimSpace(a.area.Value())
	if text == "" || a.run != nil && a.run.open == "" {
		return nil
	}
	a.area.Reset()
	a.refusal = ""
	if r := a.run; r != nil {
		r.answering = true
		a.say(askQuestion, text)
		open := r.open
		return a.call(r.id, text, func(ctx context.Context, b Backend) error { return b.SendRun(ctx, open, text) })
	}
	q := Question{Account: a.account, Prompt: text, Cwd: a.cwd(), With: a.chosen(), Input: a.keepOpen}
	a.seq++
	ctx, cancel := context.WithCancel(a.e.ctx)
	r := &askRun{id: a.seq, ctx: ctx, cancel: cancel, ch: make(chan askEventMsg, askBuffer),
		input: q.Input, answering: true, prompt: text}
	a.run = r
	a.pieces, a.shown, a.follow = nil, "", true
	a.say(askQuestion, text)
	return tea.Batch(askStream(a.e.b, r, q), askWait(r))
}

// cwd is the folder an Ask runs in (R25): the chosen account's, or for the
// rotation's choice the account it would spend next, when that account has
// one; else the TUI's own, which is empty locally and the home over SSH.
func (a *askTab) cwd() string {
	id := a.account
	if id == 0 {
		id = a.e.list.Default
	}
	for _, acct := range a.e.list.Accounts {
		if acct.ID == id && acct.Cwd != "" {
			return acct.Cwd
		}
	}
	return a.e.o.Cwd
}

// chosen is the readings asked for, in message.Names' order.
func (a *askTab) chosen() []string {
	var out []string
	for i, on := range a.with {
		if on {
			out = append(out, message.Names[i])
		}
	}
	return out
}

// call reaches the open run under the ordinary deadline (R29).
func (a *askTab) call(id int, text string, f func(context.Context, Backend) error) tea.Cmd {
	b := a.e.b
	return a.e.callOr(envCallTimeout, func(ctx context.Context) tea.Msg {
		return askCallMsg{id: id, text: text, err: f(ctx, b)}
	}, func(err error) tea.Msg { return askCallMsg{id: id, text: text, err: err} }, nil)
}

// called is how a call into the open run went: a refusal is said under
// the prompt, and a message not taken leaves the answer and goes back to
// the field.
func (a *askTab) called(msg askCallMsg) {
	if msg.err == nil {
		return
	}
	a.refusal = msg.err.Error()
	if msg.text == "" {
		return
	}
	if strings.TrimSpace(a.area.Value()) == "" {
		a.area.SetValue(msg.text)
	}
	if n := len(a.pieces); n > 0 && a.pieces[n-1] == (askPiece{kind: askQuestion, text: msg.text}) {
		a.pieces, a.dirty = a.pieces[:n-1], true
	}
	if r := a.run; r != nil && r.id == msg.id {
		r.answering = false
	}
}

// askStream starts r's answer on a goroutine of its own, which recovers
// (F31): every event goes into r.ch, then how the stream ended, and r.ch
// is closed after. The command itself returns at once.
func askStream(b Backend, r *askRun, q Question) tea.Cmd {
	return func() tea.Msg {
		go func() {
			last := askEventMsg{id: r.id, last: true}
			defer close(r.ch)
			defer func() {
				if p := recover(); p != nil {
					last.end, last.err = wire.End{}, fmt.Errorf("the answer failed: %v", p)
				}
				r.push(last)
			}()
			last.end, last.err = b.Ask(r.ctx, q, func(ev message.Event) {
				if ev.Type == "init" && ev.RunID != "" {
					r.mu.Lock()
					r.runID = ev.RunID
					r.mu.Unlock()
				}
				r.push(askEventMsg{id: r.id, event: ev})
			})
		}()
		return nil
	}
}

// askWait delivers the next of r's events. A stream that closed without
// saying how it ended, which happens only once its context had ended and
// the screen was behind, ends here, as its context did.
func askWait(r *askRun) tea.Cmd {
	return func() tea.Msg {
		m, ok := <-r.ch
		if !ok {
			err := r.ctx.Err()
			if err == nil {
				err = errors.New("the answer ended without saying how")
			}
			return askEventMsg{id: r.id, last: true, err: err}
		}
		return m
	}
}
