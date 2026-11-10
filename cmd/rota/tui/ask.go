package tui

import (
	"context"
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
	askAreaTyping = "enter asks, alt+enter adds a line, esc leaves"
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

// askRun is one Ask in flight. Update alone touches its plain fields; the
// stream's goroutine writes what it reads into ch, and runID under mu, so
// esc and Leave can close an open run that Update has not heard of yet.
// known is closed once that id is seen, or the stream has ended without
// one.
type askRun struct {
	id     int
	ctx    context.Context
	cancel context.CancelFunc
	ch     chan askEventMsg
	known  chan struct{}
	once   sync.Once
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

// seen is the run's id as the stream's goroutine read it, "" until then.
func (r *askRun) seen() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runID
}

// know says the run's id is known, or never will be.
func (r *askRun) know() { r.once.Do(func() { close(r.known) }) }

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

	// pieces is the answer; a text piece is one line of it, so text added
	// to the line being written lays out that line again and no other.
	pieces []askPiece
	// shown is what fragments have shown of a piece still to arrive whole
	// (eventStream's rule).
	shown string
	// lines is the answer laid out for linesW columns: ends[i] is how many
	// lines the pieces up to i make, and pieces from from on are to be laid
	// out again. work counts the bytes layout has looked at, which a test
	// holds to linear. blank is as many empty lines as there are lines,
	// all the viewport is told of them: it keeps the scroll, and View draws
	// the lines it says.
	lines  []string
	ends   []int
	linesW int
	from   int
	work   int
	blank  []string

	run     *askRun
	seq     int
	refusal string
}

func askNew(e *env) tab {
	a := &askTab{e: e, vp: viewport.New(), follow: true, with: make([]bool, len(message.Names))}
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

// Busy names what quitting would end, as the shell's quit ask lists it
// ("press q again to quit; the open answer ends too"): the run kept open,
// or the open answer, one still streaming.
func (a *askTab) Busy() string {
	switch r := a.run; {
	case r == nil:
		return ""
	case r.open != "":
		return "the run kept open"
	}
	return "the open answer"
}

// Leave stops an answer still streaming, and shuts a run kept open, which
// the server keeps until it is closed. Such a run's stream outlived the
// program's end for this: Leave waits, within ctx, until its id is known,
// interrupts it and closes it by that id, and only then ends the stream. A
// run still waiting to start when ctx ends is cancelled before it starts.
func (a *askTab) Leave(ctx context.Context) {
	r := a.run
	if r == nil {
		return
	}
	defer r.cancel()
	if !r.input {
		return
	}
	select {
	case <-r.known:
	case <-ctx.Done():
	}
	if id := r.seen(); id != "" {
		_ = askShut(ctx, a.e.b, id)
	}
}

// askShut ends a run kept open as the server ends one nobody reads: what
// it is doing is interrupted, then it is closed, so it finishes no turn for
// nobody.
func askShut(ctx context.Context, b Backend, id string) error {
	_ = b.InterruptRun(ctx, id)
	return b.CloseRun(ctx, id)
}

// shut is askShut as a command, for the run whose answer is ask.
func (a *askTab) shut(ask int, id string) tea.Cmd {
	return a.call(ask, "", func(ctx context.Context, b Backend) error { return askShut(ctx, b, id) })
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
		a.from = 0
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
	only(&k.KeepOpen, r == nil || !r.input)
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

// answering says esc would stop something: an answer still coming, whether
// a one-shot one or a run asked to stay open that has not opened yet, or an
// open run working on a message.
func (a *askTab) answering() bool {
	r := a.run
	if r == nil || r.stopping {
		return false
	}
	if r.input && r.open != "" {
		return r.answering
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

// stop is esc. It interrupts an open run that is working. An answer still
// coming is stopped: a run asked to stay open whose id the stream has
// already read is interrupted and closed by it, and any other request is
// cancelled, so one still waiting to start never starts. With nothing to
// stop, esc leaves the field.
func (a *askTab) stop() tea.Cmd {
	r := a.run
	switch {
	case !a.answering():
		if a.editing() {
			a.rest()
		}
		return nil
	case r.input && r.open != "":
		open := r.open
		return a.call(r.id, "", func(ctx context.Context, b Backend) error { return b.InterruptRun(ctx, open) })
	}
	r.stopping = true
	if id := r.seen(); r.input && id != "" {
		return a.shut(r.id, id)
	}
	r.cancel()
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
	// A run kept open goes on without its stream, so its stream outlives
	// the program's end until Leave has closed it by its id.
	parent := a.e.ctx
	if q.Input {
		parent = context.WithoutCancel(parent)
	}
	ctx, cancel := context.WithCancel(parent)
	r := &askRun{id: a.seq, ctx: ctx, cancel: cancel, ch: make(chan askEventMsg, askBuffer), known: make(chan struct{}),
		input: q.Input, answering: true, prompt: text}
	a.run = r
	a.pieces, a.shown, a.follow, a.from = nil, "", true, 0
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

// called is how a call into the open run went. A message not taken goes
// back to the field, as the person's own. For the run on the screen, a
// refusal is said under the prompt, and the message leaves the answer; a
// call about an earlier run has nothing more to say.
func (a *askTab) called(msg askCallMsg) {
	if msg.err == nil {
		return
	}
	if msg.text != "" && strings.TrimSpace(a.area.Value()) == "" {
		a.area.SetValue(msg.text)
	}
	r := a.run
	if r == nil || r.id != msg.id {
		return
	}
	a.refusal = msg.err.Error()
	if msg.text == "" {
		return
	}
	if n := len(a.pieces); n > 0 && a.pieces[n-1] == (askPiece{kind: askQuestion, text: msg.text}) {
		a.pieces = a.pieces[:n-1]
		a.touch(n - 1)
	}
	r.answering = false
}

// askStream starts r's answer on a goroutine of its own, which recovers
// (F31): every event goes into r.ch, then how the stream ended, and r.ch
// is closed after. The run's id is kept as soon as it is read. The command
// itself returns at once.
func askStream(b Backend, r *askRun, q Question) tea.Cmd {
	return func() tea.Msg {
		go func() {
			last := askEventMsg{id: r.id, last: true}
			defer r.know()
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
					r.know()
				}
				r.push(askEventMsg{id: r.id, event: ev})
			})
		}()
		return nil
	}
}
