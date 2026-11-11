package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/logins"
)

// The two steps of the login flow that take nothing typed. A device login
// is approved in a browser anywhere, and its step asks the server about it
// until it is, so it finishes by itself (R21). A delegated one is signed in
// by the provider's own CLI, which needs a real terminal on the machine rota
// runs on: locally the step hands it this one, and over SSH it says what to
// run there (R15).

// loginWaitEvery is how long a device step waits between two questions: the
// CLI's devicePoll, the interval RFC 8628 gives a client told none. The
// provider's own interval stays in the login's state on the server, which no
// reply carries (F3), and a provider asking to slow down is answered as
// pending.
const loginWaitEvery = 5 * time.Second

type loginWaitBindings struct {
	Open, Copy, Hand, CopyCommand key.Binding
}

func loginWaitKeys() loginWaitBindings {
	return loginWaitBindings{
		Open:        key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open in a browser")),
		Copy:        key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy the link")),
		Hand:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "hand it the terminal")),
		CopyCommand: key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy the command")),
	}
}

// What the steps' own commands say back. Each names the step it is for, so
// a step the flow has left behind never moves the one it shows now.
type (
	// loginWaitPoll is a device step's timer: time to ask again.
	loginWaitPoll struct{ s *loginWaitDevice }
	// loginWaitAnswer is what FinishLogin answered a device step.
	loginWaitAnswer struct {
		s   *loginWaitDevice
		End LoginEnd
		Err error
	}
	// loginWaitBack is the terminal back from rota login: how it ended, and
	// the last line it wrote to stderr.
	loginWaitBack struct {
		s    *loginWaitDelegated
		Err  error
		Last string
	}
)

// loginWaitEnded is how a device login ended without an account: text as
// the person reads it, cleaned, over the refusal it came from, which
// errors.As still finds.
type loginWaitEnded struct {
	text string
	err  error
}

func (e loginWaitEnded) Error() string { return e.text }
func (e loginWaitEnded) Unwrap() error { return e.err }

// newDeviceStep waits for l, a device login, to be approved as who. It
// shows where, with the code to check there, and asks the server about the
// login every loginWaitEvery until it is approved, it runs out, or no
// approval can finish it. The first question goes as soon as it starts: by
// Init, which the flow runs when it shows a step, or else by the first
// message it hears.
func newDeviceStep(e *env, l *backendLogin, who string) step {
	s := &loginWaitDevice{e: e, who: who, keys: loginWaitKeys()}
	// The step's own life, which Leave ends. It hangs from nothing, so a
	// step never left holds nothing of the program's.
	s.life, s.leave = context.WithCancel(context.Background())
	if l != nil {
		s.l = *l
	}
	// The reply's CreatedAt is the store's, in milliseconds, as the CLI
	// reads it; a login with none was begun just now.
	begun := e.now()
	if s.l.CreatedAt > 0 {
		begun = time.UnixMilli(s.l.CreatedAt)
	}
	s.ends = begun.Add(logins.TTL)
	if u, err := url.Parse(s.l.URL); err == nil {
		s.code = u.Query().Get("user_code")
	}
	return s
}

type loginWaitDevice struct {
	e    *env
	l    backendLogin
	who  string
	code string // the user_code the URL carries, to check on the page
	ends time.Time
	keys loginWaitBindings

	// started says the first question has gone. From then on exactly one
	// of the timer and a question is out at a time, until the step ends.
	started bool
	// said is the last failure, cleaned, until an answer replaces it.
	said string
	// parked counts questions answered for and still running (env.callOr):
	// none is sent beside one (Backend).
	parked atomic.Int32
	// life ends when the step is left: a question out then ends with it,
	// and no other is asked.
	life  context.Context
	leave context.CancelFunc
}

// Leave ends the step's wait: a question still out is cancelled, and none
// follows. The flow calls it when the person leaves the login, and Run when
// the program ends. It may be called from any goroutine, more than once.
func (s *loginWaitDevice) Leave(context.Context) { s.leave() }

// Init asks the first question, once.
func (s *loginWaitDevice) Init() tea.Cmd {
	if s.started {
		return nil
	}
	s.started = true
	return s.ask()
}

func (s *loginWaitDevice) Update(msg tea.Msg) (step, tea.Cmd) {
	start := s.Init()
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case loginWaitPoll:
		if msg.s == s {
			cmd = s.poll()
		}
	case loginWaitAnswer:
		if msg.s == s {
			cmd = s.answer(msg)
		}
	case tea.KeyPressMsg:
		switch {
		case s.e.o.OpenURL != nil && key.Matches(msg, s.keys.Open):
			cmd = openURL(s.e, s.l.URL)
		case key.Matches(msg, s.keys.Copy):
			cmd = tea.Batch(copyText(s.l.URL), toast(levelInfo, "the link was sent to your terminal's clipboard"))
		}
	}
	return s, tea.Batch(start, cmd)
}

// poll is the timer's turn: nothing once the step is left, the end once the
// login has run out (a question still running beside it or not), nothing
// beside a question still running, and otherwise the next question.
func (s *loginWaitDevice) poll() tea.Cmd {
	switch {
	case s.life.Err() != nil:
		return nil
	case !s.e.now().Before(s.ends):
		return s.end(stepDone{Err: fmt.Errorf("the login %s has expired", s.l.ID)})
	case s.parked.Load() > 0:
		return s.wait()
	}
	return s.ask()
}

func (s *loginWaitDevice) ask() tea.Cmd {
	if s.l.ID == "" {
		return s.end(stepDone{Err: errors.New("there is no login to wait for")})
	}
	b, id, life := s.e.b, s.l.ID, s.life
	return s.e.callOr(envFinishTimeout, func(ctx context.Context) tea.Msg {
		// Under the program's life and the deadline, as every call, and
		// under the step's own: leaving the login ends the question too.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(life, cancel)()
		end, err := b.FinishLogin(ctx, id, "")
		return loginWaitAnswer{s: s, End: end, Err: err}
	}, func(err error) tea.Msg { return loginWaitAnswer{s: s, Err: err} }, &s.parked)
}

// answer takes what the server said. A 404 is a login no longer parked: one
// another rota finished, one an answer lost on the way finished here, or one
// that ran out; the listing is read again to show which. A 400 ends the step
// in the server's words. That is wider than the CLI's wait, which ends only
// on the provider's verdict on the device code (denied, expired, spent) and
// on a login no approval can finish: over HTTP a 400 carries no reason that
// tells those from the store's other refusals of an approval, which the CLI
// says and then asks again about. Those are an approval as an account the
// login is not for, one naming no account, and an exchange that returned no
// access token (store/login.go, lib/core.go). Ending on them costs nothing
// today: no provider in lib signs in by device, and a device code is spent
// once a token is issued for it. Any other failure (a busy store, a provider
// or a daemon that did not answer) is said, and the wait goes on, on the same
// login.
func (s *loginWaitDevice) answer(a loginWaitAnswer) tea.Cmd {
	if s.life.Err() != nil {
		return nil
	}
	var no *client.Refusal
	switch {
	case a.Err == nil && !a.End.Pending:
		return s.end(stepDone{End: a.End})
	case errors.As(a.Err, &no) && no.Status == http.StatusNotFound:
		gone := loginWaitEnded{clean("the login " + s.l.ID + " is no longer parked: finished elsewhere, or run out"), a.Err}
		return tea.Batch(s.end(stepDone{Err: gone}), func() tea.Msg { return refreshMsg{How: RefreshStale} })
	case errors.As(a.Err, &no) && (no.Status == http.StatusBadRequest || no.Reason == "unfinishable"):
		return s.end(stepDone{Err: loginWaitEnded{clean(a.Err.Error()), a.Err}})
	case a.Err != nil:
		s.said = clean(a.Err.Error())
	default:
		s.said = ""
	}
	return s.wait()
}

func (s *loginWaitDevice) wait() tea.Cmd {
	return s.e.tick(loginWaitEvery, func(time.Time) tea.Msg { return loginWaitPoll{s: s} })
}

// end is the step's end, after which it sets no timer and asks nothing.
func (*loginWaitDevice) end(d stepDone) tea.Cmd {
	return func() tea.Msg { return d }
}

func (s *loginWaitDevice) View(width int) string {
	if width <= 0 {
		return ""
	}
	th := s.e.th
	head := "Approve at"
	if who := clean(s.who); who != "" {
		head = "Approve as " + who + " at"
	}
	lines := loginWaitWrap(th.Text, head, width)
	lines = append(lines, loginWaitWrap(th.Accent, clean(s.l.URL), width)...)
	if code := clean(s.code); code != "" {
		// Large, as a terminal has it: bold, on a line of its own.
		lines = append(lines, "")
		lines = append(lines, loginWaitWrap(th.Title, code, width-2)...)
		for i := len(lines) - 1; i >= 0 && lines[i] != ""; i-- {
			lines[i] = "  " + lines[i]
		}
	}
	lines = append(lines, "")
	lines = append(lines, loginWaitKeyLines(th, s.Keys(), width)...)
	lines = append(lines, loginWaitWrap(th.Text, "Waiting for the approval ("+timeLeft(s.ends, s.e.now())+" left)", width)...)
	lines = append(lines, loginWaitWrap(th.Warn, s.said, width)...)
	return strings.Join(lines, "\n")
}

func (s *loginWaitDevice) Keys() []key.Binding {
	if s.e.o.OpenURL == nil {
		return []key.Binding{s.keys.Copy}
	}
	return []key.Binding{s.keys.Open, s.keys.Copy}
}

func (*loginWaitDevice) Typing() bool { return false }

// newDelegatedStep signs account in through its own CLI, by `rota login N`:
// locally enter hands it the terminal, and a clean exit finishes the step;
// over SSH, where this process's terminal is not the person's, the step says
// what to run on the host, and c copies it.
func newDelegatedStep(e *env, account int) step {
	return &loginWaitDelegated{e: e, account: account, keys: loginWaitKeys()}
}

type loginWaitDelegated struct {
	e       *env
	account int
	keys    loginWaitBindings
	// handing says the terminal is handed over, or about to be: a second
	// enter meanwhile hands it over once.
	handing bool
	done    bool
	// failed is how the last hand-off ended, cleaned, when it did not end
	// well.
	failed string
}

func (s *loginWaitDelegated) command() string { return "rota login " + strconv.Itoa(s.account) }

func (s *loginWaitDelegated) Update(msg tea.Msg) (step, tea.Cmd) {
	switch msg := msg.(type) {
	case loginWaitBack:
		if msg.s == s {
			return s, s.back(msg)
		}
	case tea.KeyPressMsg:
		switch {
		case s.e.o.Exec != nil && key.Matches(msg, s.keys.Hand):
			return s, s.hand()
		case s.e.o.Exec == nil && key.Matches(msg, s.keys.CopyCommand):
			return s, tea.Batch(copyText(s.command()), toast(levelInfo, "the command was sent to your terminal's clipboard"))
		}
	}
	return s, nil
}

// hand is the command that hands the terminal to rota login N, made in the
// command rather than here, since making it may look on disk for rota.
func (s *loginWaitDelegated) hand() tea.Cmd {
	if s.handing || s.done {
		return nil
	}
	s.handing = true
	start, id := s.e.o.Exec, strconv.Itoa(s.account)
	return func() tea.Msg {
		var (
			t *tailed
			c tea.ExecCommand // nil when there is no command: the shell then tells done
		)
		if cmd := start("login", id); cmd != nil {
			t = execTail(cmd)
			c = t
		}
		return handOff(c, func(err error) tea.Msg {
			back := loginWaitBack{s: s, Err: err}
			if t != nil {
				back.Last = t.Last()
			}
			return back
		})()
	}
}

// back takes the terminal back. A clean exit is the account signed in, and
// the listing is read again for it; anything else says how it ended, with
// the last line the command wrote, and enter tries again.
func (s *loginWaitDelegated) back(b loginWaitBack) tea.Cmd {
	s.handing = false
	if b.Err == nil {
		s.done = true
		end := LoginEnd{Account: s.account, Provider: s.provider(), Status: "refreshed"}
		return tea.Batch(
			func() tea.Msg { return stepDone{End: end} },
			func() tea.Msg { return refreshMsg{How: RefreshStale} },
		)
	}
	var exit *exec.ExitError
	why := s.command() + " could not run: " + b.Err.Error()
	if errors.As(b.Err, &exit) {
		// "exit status 3", or "signal: interrupt" for one a signal ended.
		why = s.command() + " ended with " + exit.String()
		if b.Last != "" {
			why += ": " + b.Last
		}
	}
	s.failed = clean(why)
	return nil
}

// provider is the account's provider as the latest listing says, or "" for
// one it does not hold yet.
func (s *loginWaitDelegated) provider() string {
	for _, a := range s.e.list.Accounts {
		if a.ID == s.account {
			return a.Provider
		}
	}
	return ""
}

func (s *loginWaitDelegated) View(width int) string {
	th := s.e.th
	who := "The account's own CLI signs it in"
	if p := clean(s.provider()); p != "" {
		who = p + " signs this account in itself"
	}
	var lines []string
	if s.e.o.Exec != nil {
		lines = loginWaitWrap(th.Text, who+"; enter hands it the terminal", width)
	} else {
		host := clean(s.e.o.Host)
		if host == "" {
			host = "the host"
		}
		lines = loginWaitWrap(th.Text, who+"; on "+host+", run: "+s.command(), width)
		lines = append(lines, "")
		lines = append(lines, loginWaitKeyLines(th, s.Keys(), width)...)
	}
	if s.failed != "" {
		lines = append(lines, "")
		lines = append(lines, loginWaitWrap(th.Over, s.failed, width)...)
	}
	return strings.Join(lines, "\n")
}

func (s *loginWaitDelegated) Keys() []key.Binding {
	if s.e.o.Exec == nil {
		return []key.Binding{s.keys.CopyCommand}
	}
	return []key.Binding{s.keys.Hand}
}

func (*loginWaitDelegated) Typing() bool { return false }

// loginWaitWrap is text, which clean gave, wrapped to width and drawn in st.
func loginWaitWrap(st lipgloss.Style, text string, width int) []string {
	lines := wrap(text, width)
	for i, l := range lines {
		lines[i] = st.Render(l)
	}
	return lines
}

// loginWaitKeyLines draws bindings as the view's own hints, "o open in a
// browser   c copy the link", as many to a line as fit in width.
func loginWaitKeyLines(th Theme, bs []key.Binding, width int) []string {
	var lines []string
	line, used := "", 0
	for _, b := range bs {
		h := b.Help()
		plain := h.Key + " " + h.Desc
		w := textWidth(plain)
		drawn := th.Key.Render(h.Key) + " " + th.Muted.Render(h.Desc)
		if w > width {
			drawn, w = th.Muted.Render(fit(plain, width)), width
		}
		switch {
		case used == 0:
			line, used = drawn, w
		case used+3+w <= width:
			line, used = line+"   "+drawn, used+3+w
		default:
			lines = append(lines, line)
			line, used = drawn, w
		}
	}
	if used > 0 {
		lines = append(lines, line)
	}
	return lines
}
