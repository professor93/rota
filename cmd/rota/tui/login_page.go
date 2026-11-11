package tui

// w in the login flow: the one-time page of `rota login --web`, served by
// this process, which over SSH is the daemon, for a control principal only
// (O2). It asks where the page listens, 127.0.0.1 first, then shows the link,
// which c copies and nothing else repeats, how to reach or trust it, and how
// long it has left.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/cmd/rota/loginpage"
	"github.com/professor93/rota/internal/logins"
	rota "github.com/professor93/rota/lib"
)

// pageAsk is what the page is served for: what it takes, the login it
// finishes (nil for a key or a token for no login), the URL to approve at,
// the account and label of a token or a key, and Back, the step esc, five
// wrong tries and the page's time running out return to (none: the flow ends).
type pageAsk struct {
	Want    loginpage.Want
	Login   *backendLogin
	Approve string
	Account int
	Label   string
	Back    step
}

// Seams for tests: where the page may listen, how it is served, and the port
// an address with none is given. At loginpage's default the address is handed
// on as it is, so a busy 8788 falls back to the kernel's; tests set 0.
var (
	pageChoices    = loginpage.Choices
	loginPageServe = loginpage.Serve
	loginPagePort  = loginpage.DefaultPort
)

const (
	// loginPageBusy is what quitting ends while a page is up.
	loginPageBusy = "the one-time page"
	// loginPageLoopback is offered when this machine's addresses cannot be
	// listed, in loginpage's words for it.
	loginPageLoopback = "127.0.0.1"
	loginPageLoopWho  = "only this machine; from another one, through ssh -L"
)

// loginPageBindings are the step's keys.
type loginPageBindings struct {
	Up, Down, Serve, Copy, Back, Close key.Binding
}

func loginPageKeys() loginPageBindings {
	return loginPageBindings{
		Up:    key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:  key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Serve: key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "serve the page")),
		Copy:  key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy the link")),
		Back:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Close: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close the page")),
	}
}

// loginPageState is how far the page has got.
type loginPageState int

const (
	loginPageChoosing loginPageState = iota // an address is being chosen
	loginPageStarting                       // the page is starting to listen
	loginPageServing                        // the page is up
	loginPageClosing                        // the page is closing after esc
	loginPageOver                           // the page has ended
)

// The step's messages: this machine's addresses, or why they could not be
// listed; the page up, with the latest it ends and the ssh line that
// reaches it, or why it did not start; how it ended, with what the Backend
// said of the value it accepted; and the countdown's tick. ID is the step's
// or its page's, so what one says never reaches another.
type (
	loginPageChoicesMsg struct {
		ID      uint64
		Choices []loginpage.Choice
		Err     error
	}
	loginPageServedMsg struct {
		ID    uint64
		Page  *loginpage.Page
		Ends  time.Time
		Reach string
		Err   error
	}
	loginPageEndedMsg struct {
		ID    uint64
		End   loginpage.End
		Login LoginEnd
	}
	loginPageTickMsg struct{ ID uint64 }
)

// loginPageSeq numbers the steps and the pages they serve.
var loginPageSeq atomic.Uint64

// loginPageResult is what the Backend answered for the value the page
// accepted: written by the page's Submit, read once the page has ended.
type loginPageResult = atomic.Pointer[LoginEnd]

// loginPageStep is the step: the addresses offered once read (ready), the
// one selected, why fewer are offered (note) and why the last page did not
// start (failed); then the page, leaving if esc came while it started, stop
// for the context it is served under, the latest it ends, the login's own
// end, and the ssh line that reaches it on loopback.
type loginPageStep struct {
	e    *env
	ask  pageAsk
	keys loginPageBindings

	choices      []loginpage.Choice
	sel          int
	ready        bool
	note, failed string

	state           loginPageState
	leaving         bool
	id              uint64
	page            *loginpage.Page
	stop            context.CancelFunc
	got             *loginPageResult
	ends, loginEnds time.Time
	reach           string
}

// newPageStep serves the page p asks for until it is answered or closed.
// The flow runs its Init when it shows it.
func newPageStep(e *env, p pageAsk) step {
	return &loginPageStep{e: e, ask: p, keys: loginPageKeys(), id: loginPageSeq.Add(1)}
}

// Init reads this machine's addresses in a command: each interface is a
// question to the kernel, and a host may have hundreds of them.
func (s *loginPageStep) Init() tea.Cmd {
	id := s.id
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = loginPageChoicesMsg{ID: id, Err: fmt.Errorf("%v", r)}
			}
		}()
		choices, err := pageChoices()
		return loginPageChoicesMsg{ID: id, Choices: choices, Err: err}
	}
}

func (s *loginPageStep) Update(msg tea.Msg) (step, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return s.key(msg)
	case tea.PasteMsg:
		// Never the paste itself: it may be the very code the page takes.
		if s.state == loginPageServing {
			return s, toast(levelInfo, "the page takes it: paste it there, or esc closes the page and goes back")
		}
	case loginPageChoicesMsg:
		// What Init read, 127.0.0.1 selected wherever it is listed, or
		// 127.0.0.1 alone, said so, when nothing could be listed.
		if msg.ID != s.id || s.ready {
			break
		}
		choices := msg.Choices
		switch {
		case msg.Err != nil:
			choices, s.note = nil, "this machine's addresses cannot be listed ("+msg.Err.Error()+"), so only "+loginPageLoopback+" is offered"
		case len(choices) == 0:
			s.note = "this machine lists no address to offer, so only " + loginPageLoopback + " is offered"
		}
		if len(choices) == 0 {
			choices = []loginpage.Choice{{Addr: loginPageLoopback, Who: loginPageLoopWho}}
		}
		s.choices, s.ready = choices, true
		s.sel = max(slices.IndexFunc(choices, func(c loginpage.Choice) bool { return c.Addr == loginPageLoopback }), 0)
	case loginPageServedMsg:
		if msg.ID == s.id && s.state == loginPageStarting {
			return s.served(msg)
		}
	case loginPageEndedMsg:
		if msg.ID == s.id && (s.state == loginPageServing || s.state == loginPageClosing) {
			return s.ended(msg)
		}
	case loginPageTickMsg:
		if msg.ID == s.id && s.state == loginPageServing {
			return s, s.tick()
		}
	}
	return s, nil
}

func (s *loginPageStep) key(msg tea.KeyPressMsg) (step, tea.Cmd) {
	s.bindings()
	k := &s.keys
	switch {
	case key.Matches(msg, k.Up):
		s.sel = max(s.sel-1, 0)
	case key.Matches(msg, k.Down):
		s.sel = min(s.sel+1, len(s.choices)-1)
	case key.Matches(msg, k.Serve):
		return s, s.serve()
	case key.Matches(msg, k.Copy):
		return s, tea.Batch(copyText(s.page.URL), toast(levelInfo, "the link was sent to the clipboard"))
	case key.Matches(msg, k.Close):
		s.state = loginPageClosing
		return s, loginPageClose(s.page, s.stop)
	case key.Matches(msg, k.Back) && s.state == loginPageStarting:
		// The page stops as soon as it is up, and its served message then
		// closes it and goes back.
		s.leaving = true
		return s, loginPageClose(nil, s.stop)
	case key.Matches(msg, k.Back):
		return s.back("")
	}
	return s, nil
}

// bindings enables the keys that work where the step is. Only a control
// principal serves (F13: roleFor never sees this listener), and only while
// choosing: a second page would leave the first serving the link shown.
func (s *loginPageStep) bindings() {
	k, st := &s.keys, s.state
	choosing := st == loginPageChoosing
	k.Up.SetEnabled(choosing && len(s.choices) > 1)
	k.Down.SetEnabled(choosing && len(s.choices) > 1)
	s.e.gate(&k.Serve)
	k.Serve.SetEnabled(choosing && s.ready && k.Serve.Enabled())
	k.Copy.SetEnabled(st == loginPageServing)
	k.Close.SetEnabled(st == loginPageServing)
	k.Back.SetEnabled(choosing || st == loginPageStarting && !s.leaving)
}

// serve starts the page on the address selected, in a command, under a
// context of its own within the program's: it ends with the TUI, and Leave
// and esc end it even before it is up. The command reads nothing of s.
func (s *loginPageStep) serve() tea.Cmd {
	if !s.e.control() || s.state != loginPageChoosing || !s.ready {
		return nil
	}
	s.id = loginPageSeq.Add(1)
	s.state, s.leaving, s.failed = loginPageStarting, false, ""
	s.got = &loginPageResult{}
	o := s.options(s.choices[s.sel].Addr)
	s.loginEnds = o.Ends
	ctx, stop := context.WithCancel(s.e.ctx)
	id, host := s.id, s.e.o.Host
	s.stop = stop
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = loginPageServedMsg{ID: id, Err: fmt.Errorf("%v", r)}
			}
		}()
		ends := o.Now().Add(loginpage.Lifetime)
		if !o.Ends.IsZero() && o.Ends.Before(ends) {
			ends = o.Ends
		}
		p, err := loginPageServe(ctx, o)
		if err != nil {
			return loginPageServedMsg{ID: id, Err: err}
		}
		return loginPageServedMsg{ID: id, Page: p, Ends: ends, Reach: loginPageReach(p, host)}
	}
}

// options are the page's for addr: what it asks for and says, the login's
// own end, and the Backend call each value goes to.
func (s *loginPageStep) options(addr string) loginpage.Options {
	a := s.ask
	o := loginpage.Options{Addr: loginPageAddr(addr), Want: a.Want, Provider: rota.DefaultProvider,
		Now: s.e.o.Now, Submit: loginPageSubmit(s.e.b, a, s.got)}
	id := a.Account
	if l := a.Login; l != nil {
		o.Provider, o.LoginURL, id = l.Provider, cmp.Or(a.Approve, l.URL), cmp.Or(l.Account, id)
		// CreatedAt is Unix milliseconds, as rota.Login has it, and the
		// store keeps a parked login for logins.TTL from then.
		if l.CreatedAt > 0 {
			o.Ends = time.UnixMilli(l.CreatedAt).Add(logins.TTL)
		}
	}
	// Who to approve as is the account the login is bound to, as the CLI's
	// page says it; nobody for a login bound to none.
	for _, acct := range s.e.list.Accounts {
		if id != 0 && acct.ID == id {
			o.Approve = cmp.Or(strings.TrimSpace(clean(acct.Email)), accountsLabel(acct))
		}
	}
	return o
}

func (s *loginPageStep) served(msg loginPageServedMsg) (step, tea.Cmd) {
	if msg.Err != nil {
		s.stop() // no page listens under it: this only lets it go
		s.state, s.failed = loginPageChoosing, "the one-time page did not start: "+msg.Err.Error()
		if s.leaving {
			return s.back("")
		}
		return s, nil
	}
	s.page, s.ends, s.reach = msg.Page, msg.Ends, msg.Reach
	wait := loginPageWait(s.id, msg.Page, s.got)
	if s.leaving {
		s.state = loginPageClosing
		return s, tea.Batch(wait, loginPageClose(msg.Page, s.stop))
	}
	s.state = loginPageServing
	return s, tea.Batch(wait, s.tick())
}

// ended is the page's end: an accepted value finishes the flow, even while
// esc closed the page, a refusal ends it, and the rest go back, saying why.
func (s *loginPageStep) ended(msg loginPageEndedMsg) (step, tea.Cmd) {
	s.state = loginPageOver
	s.stop() // the page is over: this only lets its context go
	switch e := msg.End; e.Why {
	case loginpage.Accepted:
		end := msg.Login
		return s, func() tea.Msg { return stepDone{End: end} }
	case loginpage.Refused:
		err := errors.New(cmp.Or(e.Message, "this login cannot be finished any more"))
		return s, func() tea.Msg { return stepDone{Err: err} }
	case loginpage.Tries:
		return s.back(strings.TrimSuffix("five wrong tries closed the one-time page; the last: "+e.Message, "; the last: "))
	case loginpage.TimedOut:
		if !s.loginEnds.IsZero() && !s.ends.Before(s.loginEnds) {
			return s.back("the one-time page closed when the login ran out")
		}
		return s.back("the one-time page closed after 10 minutes")
	}
	return s.back("")
}

// back returns to the step the page was opened from, saying why when there
// is a reason; with none to return to, the flow ends.
func (s *loginPageStep) back(why string) (step, tea.Cmd) {
	s.state = loginPageOver
	if s.ask.Back == nil {
		err := errors.New(cmp.Or(why, "the one-time page was closed"))
		return s, func() tea.Msg { return stepDone{Err: err} }
	}
	if why == "" {
		return s.ask.Back, nil
	}
	return s.ask.Back, toast(levelWarn, "%s", why)
}

func (s *loginPageStep) tick() tea.Cmd {
	id := s.id
	return s.e.tick(time.Second, func(time.Time) tea.Msg { return loginPageTickMsg{ID: id} })
}

// loginPageWait is the command that waits for the page to end, as Bubble
// Tea runs every command, in a goroutine of its own.
func loginPageWait(id uint64, p *loginpage.Page, got *loginPageResult) tea.Cmd {
	return func() tea.Msg {
		m := loginPageEndedMsg{ID: id, End: <-p.Done}
		if e := got.Load(); e != nil {
			m.Login = *e
		}
		return m
	}
}

// loginPageClose is the command that stops a page: its context, which ends
// one still starting once it is up, then p, if up. loginPageWait says how.
func loginPageClose(p *loginpage.Page, stop context.CancelFunc) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = toastMsg{Text: fmt.Sprintf("closing the one-time page failed: %v", r), Level: levelErr}
			}
		}()
		stop()
		if p != nil {
			p.Close()
		}
		return nil
	}
}

// loginPageSubmit is the page's Submit for a: a value for a login finishes
// it, a token goes on its account and a key makes or updates its own; got
// keeps the Backend's answer to the one accepted. A check under way is let
// finish however the page ends, as rota login --web lets it, since the
// provider may have taken a code that works once: it runs under the page's
// values, not its end, for envFinishTimeout at most (R29). A 404 or the
// reason "unfinishable" ends the page; any other refusal, and a login not
// approved yet, counts a try. A panic is an error, never the daemon's end.
func loginPageSubmit(b Backend, a pageAsk, got *loginPageResult) func(ctx context.Context, value string) (string, error) {
	return func(ctx context.Context, value string) (done string, err error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), envFinishTimeout)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				done, err = "", fmt.Errorf("the check failed: %v", r)
			}
		}()
		var end LoginEnd
		switch {
		case a.Login != nil:
			end, err = b.FinishLogin(ctx, a.Login.ID, value)
		case a.Want == loginpage.Token:
			end, err = b.PasteToken(ctx, a.Account, value)
		case a.Want == loginpage.Key:
			end, err = b.PasteKey(ctx, value, a.Label)
		default:
			return "", loginpage.Final(errors.New("there is no login for this page to finish"))
		}
		var no *client.Refusal
		switch {
		case errors.As(err, &no) && (no.Status == http.StatusNotFound || no.Reason == "unfinishable"):
			return "", loginpage.Final(err)
		case err != nil:
			return "", err
		case end.Pending:
			return "", errors.New("not approved yet; approve at the sign-in page, then give it again")
		}
		got.Store(&end)
		return "", nil
	}
}

// loginPageAddr is addr with loginPagePort added when it names no port and
// loginPagePort is not the page's own default.
func loginPageAddr(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil || loginPagePort == loginpage.DefaultPort {
		return addr
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]"), strconv.Itoa(loginPagePort))
}

// loginPageReach is the ssh line that reaches p on loopback, the one place it
// speaks plain HTTP, naming this account and machine as rota login --web does.
func loginPageReach(p *loginpage.Page, host string) string {
	u, err := url.Parse(p.URL)
	if p.Fingerprint != "" || err != nil {
		return ""
	}
	who := "<user>"
	if me, err := user.Current(); err == nil && me.Username != "" && !strings.ContainsAny(me.Username, " \\") {
		who = me.Username
	}
	here, _ := os.Hostname()
	return "ssh -L " + u.Port() + ":" + net.JoinHostPort(u.Hostname(), u.Port()) + " " + who + "@" + cmp.Or(host, here, "<host>")
}

// Keys are the bindings that work where the step is: the flow hands the
// step only a key one of them matches.
func (s *loginPageStep) Keys() []key.Binding {
	s.bindings()
	k := s.keys
	return slices.DeleteFunc([]key.Binding{k.Serve, k.Copy, k.Up, k.Down, k.Back, k.Close},
		func(b key.Binding) bool { return !b.Enabled() })
}

// Typing is never: the step has no field, so its keys are never text.
func (s *loginPageStep) Typing() bool { return false }

// Busy is what quitting ends: the page, while one is starting, up or
// closing.
func (s *loginPageStep) Busy() string {
	if s.state >= loginPageStarting && s.state <= loginPageClosing {
		return loginPageBusy
	}
	return ""
}

// Leave stops the page this step served, waiting within ctx for one that is
// up to close. The flow calls it when it drops the step (x, or an esc the
// step does not take), and Run once the program has ended. A page still
// starting is stopped through its context the moment it is up.
func (s *loginPageStep) Leave(ctx context.Context) {
	if s.stop == nil {
		return
	}
	s.stop()
	p := s.page
	if p == nil || s.state != loginPageServing && s.state != loginPageClosing {
		return
	}
	done := make(chan struct{})
	go func() { defer close(done); loginPageClose(p, s.stop)() }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (s *loginPageStep) View(width int) string {
	th := s.e.th
	var lines []string
	say := func(st lipgloss.Style, text string) {
		for _, l := range wrap(clean(text), width) {
			lines = append(lines, st.Render(l))
		}
	}
	switch s.state {
	case loginPageChoosing:
		what := map[loginpage.Want]string{loginpage.Code: "code", loginpage.Key: "API key", loginpage.Token: "setup token"}[s.ask.Want]
		say(th.Text, "A one-time page takes the "+what+" in a browser, on this machine or on another that can reach it.")
		lines = append(lines, "")
		if !s.ready {
			say(th.Muted, "Looking up this machine's addresses…")
			break
		}
		say(th.Muted, "Where should it listen?")
		lines = append(lines, s.viewChoices(width)...)
		if s.note != "" {
			say(th.Muted, s.note)
		}
		if s.failed != "" {
			say(th.Over, s.failed)
		}
		if !s.e.control() {
			say(th.Over, "the one-time page is for the control role")
		}
	case loginPageStarting:
		if s.leaving {
			say(th.Muted, "Closing the one-time page…")
			break
		}
		say(th.Muted, "Starting the one-time page on "+s.choices[s.sel].Addr+"…")
	case loginPageServing:
		say(th.Text, "Open the one-time page:")
		for _, l := range wrap(clean(s.page.URL), width-2) {
			lines = append(lines, "  "+th.Accent.Render(l))
		}
		if fp := s.page.Fingerprint; fp != "" {
			say(th.Text, "It is HTTPS with a certificate made for this run; its SHA-256 fingerprint is "+fp+": check that the browser shows the same.")
		}
		if s.reach != "" {
			say(th.Muted, "From another machine: "+s.reach+", then open the link there.")
		}
		say(th.Text, "It ends after one login, five wrong tries or 10 minutes; "+timeLeft(s.ends, s.e.now())+" left.")
	case loginPageClosing:
		say(th.Muted, "Closing the one-time page; a value being checked is let finish first…")
	default:
		say(th.Muted, "The one-time page has closed.")
	}
	return strings.Join(lines, "\n")
}

// viewChoices is one row per address, the selected one marked, with who can
// reach it beside it; a warning in words too, colour never the only sign.
func (s *loginPageStep) viewChoices(width int) []string {
	th := s.e.th
	aw := 0
	for _, c := range s.choices {
		aw = max(aw, textWidth(clean(c.Addr)))
	}
	aw = max(min(aw, width-4), 1)
	indent, ww := 2+aw+2, width-2-aw-2
	if ww < 16 { // too narrow beside the addresses: under them
		indent, ww = 4, width-4
	}
	var lines []string
	for i, c := range s.choices {
		st, mark, who := th.Text, "  ", c.Who
		if c.Warn {
			st, who = th.Warn, "warning: "+who
		}
		addr := st
		if i == s.sel {
			mark, addr = "> ", st.Inherit(th.Selected)
		}
		head, rest := mark+addr.Render(fit(clean(c.Addr), aw)), wrap(clean(who), ww)
		if indent > 4 && len(rest) > 0 {
			head, rest = head+"  "+st.Render(rest[0]), rest[1:]
		}
		lines = append(lines, head)
		for _, l := range rest {
			lines = append(lines, strings.Repeat(" ", indent)+st.Render(l))
		}
	}
	return lines
}
