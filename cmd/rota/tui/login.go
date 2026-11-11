package tui

// The login flow: n adds an account and l signs one in again, in a short
// guided flow over the tabs. What it asks first depends on the provider
// and the account; once a login is begun, its kind picks the step that
// finishes it (login_steps.go, login_wait.go, login_page.go). One flow at a
// time: put aside with esc, it keeps waiting, and n or l brings it back
// (R17).

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/cmd/rota/loginpage"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/wire"
)

// loginBindings are the flow's own keys. A step's are its own, and come
// first: the flow takes a key only when the step does not.
type loginBindings struct {
	Up, Down, Choose, Cancel, Aside, Close, Abandon, Again key.Binding
}

func loginKeys() loginBindings {
	return loginBindings{
		Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Choose:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "choose")),
		Cancel:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		Aside:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "put aside")),
		Close:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close")),
		Abandon: key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "abandon")),
		Again:   key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "begin another")),
	}
}

// loginStage is where the flow is.
type loginStage int

const (
	loginPicking   loginStage = iota // a pick is shown
	loginBeginning                   // a begin, or a finish with no code, is awaited
	loginStepping                    // a step is shown
	loginEnded                       // the flow ended, and says why
)

// loginPick is which pick is shown.
type loginPick int

const (
	loginPickProvider loginPick = iota // n: which provider
	loginPickKind                      // n: short, long or an API key
	loginPickHow                       // a key, or the provider signing itself in
	loginPickAgain                     // l on a claude login: short, long or a setup token
	loginConfirm                       // l on another provider: enter begins
)

// loginOption is one choice of a pick: its name, what it gives up, and the
// way of signing in it means.
type loginOption struct{ name, about, way string }

// The ways a login is made.
const (
	loginShort = "short" // the provider's ordinary login
	loginLong  = "long"  // a year-long token, inference only
	loginKey   = "key"   // a pasted Claude Console key
	loginToken = "token" // a pasted `claude setup-token` token
	loginSelf  = "self"  // the provider's own CLI signs it in
)

// loginMakers are the steps of other files the flow opens: the device and
// delegated steps (login_wait.go) and the one-time page (login_page.go). A
// test puts recorders in a flow's own.
type loginMakers struct {
	device    func(e *env, l *backendLogin, who string) step
	delegated func(e *env, account int) step
	page      func(e *env, p pageAsk) step
}

// Messages for one flow alone, by its pointer and generation: one an
// abandoned flow or a login since replaced was waiting for goes nowhere.
type (
	loginBegunMsg struct {
		f   *loginFlow
		gen int
		b   LoginBegin
		l   backendLogin
		err error
	}
	// loginFinishedMsg answers a finish with no code: the provider signing
	// the account in itself.
	loginFinishedMsg struct {
		f   *loginFlow
		gen int
		end LoginEnd
		err error
	}
	loginTickMsg struct {
		f   *loginFlow
		gen int
	}
)

// loginFlow is the flow: an asider, so it can be put aside and come back.
type loginFlow struct {
	e     *env
	keys  loginBindings
	steps loginMakers
	// again is the account l signs in again, nil for n.
	again *wire.Account

	stage    loginStage
	pick     loginPick
	opts     []loginOption
	at       int
	provider string
	way      string
	// said is why the last begin was refused, under the pick; ended why
	// the flow ended.
	said, ended string

	// login is the login parked on the server, ends when it runs out, and
	// step the step that finishes it. gen counts the logins begun, so an
	// answer or a tick for one replaced is told apart.
	login *backendLogin
	ends  time.Time
	step  step
	gen   int

	// longFor is the account a long token is meant for, to say so when it
	// went on another; longWho whom n long's second approval is made as.
	longFor int
	longWho string
	// fellBack is the refusal of a login begun for an account that runs on
	// a key of a provider whose login takes one: its key is asked for
	// instead, on a login of the provider's own (F40).
	fellBack   string
	delegating bool

	aside, closed bool
}

// newLogin is the login flow: for a new account when again is nil, else
// for signing the account again names in again. It does nothing until a
// key is pressed.
func newLogin(e *env, again *wire.Account) asider {
	f := &loginFlow{e: e, keys: loginKeys(), steps: loginMakers{device: newDeviceStep, delegated: newDelegatedStep, page: newPageStep}}
	if again != nil {
		a := *again
		f.again = &a
	}
	f.start()
	return f
}

// start is the flow from its beginning: n picks the provider; l on a key
// asks for the key again, on a claude login picks the kind, and on any
// other account says what enter begins.
func (f *loginFlow) start() {
	f.gen++
	f.stage, f.said, f.ended, f.at, f.opts, f.way = loginPicking, "", "", 0, nil, ""
	f.login, f.step, f.longFor, f.longWho, f.fellBack, f.delegating = nil, nil, 0, "", "", false
	a := f.again
	switch {
	case a == nil:
		f.pick, f.provider = loginPickProvider, ""
		for i, p := range wire.LoginProviders() {
			f.opts = append(f.opts, loginOption{name: p})
			if p == rota.DefaultProvider {
				f.at = i
			}
		}
	case a.Provider == rota.DefaultProvider && a.Credential == "apikey":
		f.provider, f.way = a.Provider, loginKey
		f.show(newSecretStep(f.e, secretAsk{Want: loginpage.Key, Account: a.ID}))
	case a.Provider == rota.DefaultProvider:
		f.pick, f.provider = loginPickAgain, a.Provider
		f.opts = loginAgainKinds()
	default:
		f.pick, f.provider = loginConfirm, a.Provider
	}
}

func (f *loginFlow) Update(msg tea.Msg) (overlay, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return f, f.key(msg)
	case loginBegunMsg:
		if msg.f == f && msg.gen == f.gen && f.stage == loginBeginning {
			return f, f.begun(msg)
		}
		return f, nil
	case loginFinishedMsg:
		if msg.f == f && msg.gen == f.gen && f.stage == loginBeginning {
			return f, f.finished(msg.end, msg.err)
		}
		return f, nil
	case loginTickMsg:
		if msg.f == f && msg.gen == f.gen {
			return f, f.tick()
		}
		return f, nil
	case loginPageMsg:
		if f.stage == loginStepping && msg.from == f.step {
			// The page goes back to the step it was opened from.
			msg.ask.Back = msg.from
			return f, f.show(f.steps.page(f.e, msg.ask))
		}
		return f, nil
	case stepDone:
		if f.stage != loginStepping {
			return f, nil
		}
		if msg.Err != nil {
			return f, f.end(msg.Err.Error())
		}
		return f, f.done(msg.End)
	}
	return f, f.toStep(msg)
}

// stepping says a step is shown.
func (f *loginFlow) stepping() bool { return f.stage == loginStepping && f.step != nil }

// toStep gives msg to the step shown, if one is.
func (f *loginFlow) toStep(msg tea.Msg) tea.Cmd {
	if !f.stepping() {
		return nil
	}
	s, cmd := f.step.Update(msg)
	if s != nil {
		f.step = s
	}
	return cmd
}

// show puts s in place of the step shown, and starts it: a step that runs
// by itself (a device login polls) has Init.
func (f *loginFlow) show(s step) tea.Cmd {
	f.step, f.stage = s, loginStepping
	if i, ok := s.(interface{ Init() tea.Cmd }); ok {
		return i.Init()
	}
	return nil
}

// arm sets which of the flow's keys work where it is.
func (f *loginFlow) arm() {
	k := &f.keys
	for _, b := range []*key.Binding{&k.Up, &k.Down, &k.Choose, &k.Cancel, &k.Aside, &k.Close, &k.Abandon, &k.Again} {
		b.SetEnabled(false)
	}
	switch f.stage {
	case loginPicking:
		k.Up.SetEnabled(len(f.opts) > 1)
		k.Down.SetEnabled(len(f.opts) > 1)
		k.Choose.SetEnabled(len(f.opts) > 0 || f.pick == loginConfirm)
		k.Cancel.SetEnabled(true)
	case loginBeginning:
		// A begin parks a login nobody has seen, so it may be left; a finish
		// with no code acts on the server, and its end must be said.
		k.Cancel.SetEnabled(f.login == nil)
	case loginStepping:
		// While the step awaits an answer it is not left: the answer, which
		// may have finished the login, is always taken.
		if f.stepping() && !f.step.Typing() {
			waiting := loginWaiting(f.step)
			k.Aside.SetEnabled(f.login != nil)
			k.Abandon.SetEnabled(f.login != nil && !waiting)
			k.Close.SetEnabled(f.login == nil && !waiting)
		}
	case loginEnded:
		k.Again.SetEnabled(true)
		k.Close.SetEnabled(true)
	}
}

func (f *loginFlow) Keys() []key.Binding {
	f.arm()
	var out []key.Binding
	if f.stepping() {
		out = append(out, f.step.Keys()...)
	}
	k := f.keys
	return append(out, loginEnabled(k.Choose, k.Up, k.Down, k.Again, k.Abandon, k.Aside, k.Close, k.Cancel)...)
}

func (f *loginFlow) key(msg tea.KeyPressMsg) tea.Cmd {
	f.arm()
	k := &f.keys
	switch f.stage {
	case loginPicking:
		switch {
		case key.Matches(msg, k.Up):
			f.at = max(f.at-1, 0)
		case key.Matches(msg, k.Down):
			f.at = min(f.at+1, len(f.opts)-1)
		case key.Matches(msg, k.Choose):
			return f.choose()
		case key.Matches(msg, k.Cancel):
			f.closed = true
		}
		return nil
	case loginBeginning:
		if key.Matches(msg, k.Cancel) {
			f.closed = true
		}
		return nil
	case loginEnded:
		switch {
		case key.Matches(msg, k.Again):
			f.start()
		case key.Matches(msg, k.Close):
			f.closed = true
		}
		return nil
	}
	// A step has the keys it offers, and every key while it is typing.
	if !f.stepping() {
		return nil
	}
	if f.step.Typing() || key.Matches(msg, f.step.Keys()...) {
		return f.toStep(msg)
	}
	switch {
	case key.Matches(msg, k.Aside):
		f.aside = true
		return nil
	case key.Matches(msg, k.Close):
		f.closed = true
		return f.leave()
	case key.Matches(msg, k.Abandon):
		f.closed = true
		return tea.Batch(f.leave(), toast(levelInfo, "the login %s stays parked until it runs out, in %s", f.login.ID, timeLeft(f.ends, f.e.now())))
	}
	return f.toStep(msg)
}

// choose takes the option picked.
func (f *loginFlow) choose() tea.Cmd {
	if f.pick == loginConfirm {
		f.way = loginShort
		return f.begin(LoginBegin{Account: f.again.ID})
	}
	o := f.opts[f.at]
	switch f.pick {
	case loginPickProvider:
		f.provider = o.name
		if loginKinded(f.provider) {
			f.pick, f.opts, f.at = loginPickKind, loginKinds(f.provider), 0
			return nil
		}
		f.way = loginShort
		return f.begin(LoginBegin{Provider: f.provider})
	case loginPickKind:
		// Long begins with the ordinary login, which makes the account.
		if f.way = o.way; f.way == loginKey {
			return f.show(newSecretStep(f.e, secretAsk{Want: loginpage.Key, Label: true}))
		}
		return f.begin(LoginBegin{Provider: f.provider})
	case loginPickHow:
		if o.way == loginSelf {
			return f.finishBare()
		}
		return f.show(newSecretStep(f.e, secretAsk{Want: loginpage.Key, Login: f.login, Account: f.againID()}))
	case loginPickAgain:
		switch f.way = o.way; f.way {
		case loginLong:
			f.longFor = f.again.ID
			return f.begin(LoginBegin{Provider: f.provider, Long: true})
		case loginToken:
			return f.show(newSecretStep(f.e, secretAsk{Want: loginpage.Token, Account: f.again.ID}))
		}
		return f.begin(LoginBegin{Account: f.again.ID})
	}
	return nil
}

// begin begins a login as b says.
func (f *loginFlow) begin(b LoginBegin) tea.Cmd {
	f.gen++
	f.stage, f.said = loginBeginning, ""
	gen, be := f.gen, f.e.b
	return f.e.callOr(envCallTimeout, func(ctx context.Context) tea.Msg {
		l, err := be.BeginLogin(ctx, b)
		return loginBegunMsg{f: f, gen: gen, b: b, l: l, err: err}
	}, func(err error) tea.Msg { return loginBegunMsg{f: f, gen: gen, b: b, err: err} }, nil)
}

// begun takes a login begun: its kind picks the step that finishes it.
func (f *loginFlow) begun(msg loginBegunMsg) tea.Cmd {
	if msg.err != nil {
		return f.refused(msg.b, msg.err)
	}
	if f.fellBack != "" && msg.l.Kind != "apikey" {
		// The provider's login takes no key: the first refusal stands.
		return f.end(f.fellBack)
	}
	l := msg.l
	f.login, f.ends = &l, loginEnds(&l, f.e.now())
	cmds := []tea.Cmd{f.ticker()}
	switch l.Kind {
	case "device":
		cmds = append(cmds, f.show(f.steps.device(f.e, f.login, f.who())))
	case "delegated":
		cmds = append(cmds, f.finishBare())
	case "apikey":
		if l.Delegated && f.again == nil {
			f.stage, f.pick, f.at = loginPicking, loginPickHow, 0
			f.opts = loginHows(clean(l.Provider))
			break
		}
		cmds = append(cmds, f.show(newSecretStep(f.e, secretAsk{Want: loginpage.Key, Login: f.login, Account: f.againID()})))
	default:
		cmds = append(cmds, f.show(newCodeStep(f.e, f.login, f.who())))
	}
	return tea.Batch(cmds...)
}

// refused takes a begin the server refused. An account whose own CLI signs
// it in gets the delegated step. An account of another provider refused
// with no reason either has no uuid or e-mail, which ends the flow, or may
// run on a key of a provider whose login takes one: its key is asked for on
// a login of the provider's own, and when that login takes no key the
// first refusal stands. n long's second login refused
// ends the flow, since the account is made. Anything else is said under
// the pick, and enter tries again.
func (f *loginFlow) refused(b LoginBegin, err error) tea.Cmd {
	var no *client.Refusal
	isNo := errors.As(err, &no)
	switch {
	case isNo && no.Reason == "delegated" && f.again != nil:
		f.delegating = true
		return f.show(f.steps.delegated(f.e, f.again.ID))
	case isNo && no.Status == 400 && no.Reason == "" && b.Account != 0 && f.fellBack == "" && f.provider != rota.DefaultProvider:
		if f.again.UUID == "" && f.again.Email == "" {
			// Refused for having no uuid or e-mail, which no retry changes;
			// a login of the provider's own would only be left parked.
			return f.end(err.Error())
		}
		f.fellBack = err.Error()
		return f.begin(LoginBegin{Provider: f.provider})
	case f.fellBack != "":
		return f.end(f.fellBack)
	case f.longWho != "":
		return f.end(err.Error())
	}
	f.stage, f.said = loginPicking, err.Error()
	return nil
}

// finishBare finishes the parked login with no code: the provider's own
// CLI signs the account in.
func (f *loginFlow) finishBare() tea.Cmd {
	f.stage = loginBeginning
	gen, be, id := f.gen, f.e.b, f.login.ID
	return f.e.callOr(envFinishTimeout, func(ctx context.Context) tea.Msg {
		end, err := be.FinishLogin(ctx, id, "")
		return loginFinishedMsg{f: f, gen: gen, end: end, err: err}
	}, func(err error) tea.Msg { return loginFinishedMsg{f: f, gen: gen, err: err} }, nil)
}

func (f *loginFlow) finished(end LoginEnd, err error) tea.Cmd {
	switch {
	case err != nil:
		return f.end(err.Error())
	case end.Pending:
		return f.end("the login " + f.login.ID + " is not finished yet")
	}
	f.stage = loginStepping
	return f.done(end)
}

// done takes a login that ended in an account: it is selected, the listing
// read again and the end said. n long then begins the long token's login
// for that account, and an account rota holds no credential for gets the
// step its CLI signs it in by; otherwise the flow closes. The delegated
// step asks for the listing itself when its CLI is done, so its end is
// not asked for twice.
func (f *loginFlow) done(end LoginEnd) tea.Cmd {
	p, n := clean(end.Provider), end.Account
	if p == "" {
		p = clean(f.provider)
	}
	cmds := []tea.Cmd{func() tea.Msg { return selectMsg{Account: n} }}
	if !f.delegating {
		cmds = append(cmds, func() tea.Msg { return refreshMsg{How: RefreshStale} })
	}
	switch end.Status {
	case "long":
		cmds = append(cmds, toast(levelInfo, "Long-lived token stored for #%d, until %s", n, f.date(end.LongUntil)))
		if f.longFor != 0 && n != f.longFor {
			cmds = append(cmds, toast(levelWarn, "the long token went on account %d, which approved it", n))
		}
	case "added":
		cmds = append(cmds, toast(levelInfo, "Added %s account %d", p, n))
	case "refreshed":
		cmds = append(cmds, toast(levelInfo, "Refreshed %s account %d", p, n))
	case "updated":
		cmds = append(cmds, toast(levelInfo, "Updated %s account %d", p, n))
	default:
		cmds = append(cmds, toast(levelInfo, "Signed in %s account %d", p, n))
	}
	if end.Warning != "" {
		cmds = append(cmds, toast(levelWarn, "%s", end.Warning))
	}
	f.login, f.step = nil, nil
	f.gen++
	switch {
	case f.way == loginLong && f.again == nil && f.longFor == 0 && end.Status != "long":
		f.longFor, f.longWho = n, p+" account "+strconv.Itoa(n)
		if e := clean(end.Email); strings.TrimSpace(e) != "" {
			f.longWho = e
		}
		cmds = append(cmds, f.begin(LoginBegin{Provider: f.provider, Long: true}))
	case end.Delegated && !f.delegating:
		f.delegating = true
		cmds = append(cmds, f.show(f.steps.delegated(f.e, n)))
	default:
		f.closed = true
	}
	return tea.Batch(cmds...)
}

// end ends the flow with why, which it then says, with n beginning
// another. Put aside, it closes and says why in a toast instead of coming
// back over whatever the person is doing.
func (f *loginFlow) end(why string) tea.Cmd {
	leave := f.leave()
	f.login, f.step = nil, nil
	f.gen++
	f.stage, f.ended = loginEnded, why
	if f.aside {
		f.closed = true
		return tea.Batch(leave, toast(levelErr, "%s; n begins another", why))
	}
	return leave
}

// leave closes what the step holds open, a page's listener, once the
// flow no longer shows it.
func (f *loginFlow) leave() tea.Cmd {
	l, ok := f.step.(programLeaver)
	if !ok {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), programLeaveTimeout)
		defer cancel()
		l.Leave(ctx)
		return nil
	}
}

// ticker wakes the flow when its countdown next reads differently: each
// minute, then each second of the last, and at the end.
func (f *loginFlow) ticker() tea.Cmd {
	d := f.ends.Sub(f.e.now())
	unit := time.Minute
	if d <= time.Minute {
		unit = time.Second
	}
	if d = d % unit; d <= 0 {
		d = unit
	}
	gen := f.gen
	return f.e.tick(d, func(time.Time) tea.Msg { return loginTickMsg{f: f, gen: gen} })
}

// tick ends the flow once its login has run out (the server keeps it no
// longer), and otherwise waits for the next change.
func (f *loginFlow) tick() tea.Cmd {
	if f.login == nil {
		return nil
	}
	// An answer awaited is taken first: what was sent in time may have
	// finished the login.
	if !f.e.now().Before(f.ends) && !f.awaiting() {
		return f.end("the login " + f.login.ID + " has expired")
	}
	return f.ticker()
}

func (f *loginFlow) Typing() bool { return f.stepping() && f.step.Typing() }
func (f *loginFlow) Closed() bool { return f.closed }
func (f *loginFlow) Aside() bool  { return f.aside }
func (f *loginFlow) Return()      { f.aside = false }
