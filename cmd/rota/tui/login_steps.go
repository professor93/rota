package tui

// The login flow's own steps: the code a browser approval gives, and a
// secret pasted whole (an API key, a setup token). Each sends what it was
// given once, keeps it in its field whatever the answer, and ends with
// stepDone only on success or on a refusal nothing will put right.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/cmd/rota/loginpage"
	"github.com/professor93/rota/internal/logins"
)

// secretAsk is what the secret step asks for, and so where it sends it:
// with a Login (of kind apikey) it finishes that login, FinishLogin(l.ID,
// value); otherwise Token is a setup token for Account, PasteToken, and Key
// a Claude Console key, PasteKey. Label adds a field naming the account a
// key makes. A Key with an Account is that account's key again.
type secretAsk struct {
	Want    loginpage.Want
	Login   *backendLogin
	Account int
	Label   bool
}

// loginNotYet is what a login not approved yet is answered with, in the
// command line's words.
const loginNotYet = "Not approved yet; approve in the browser, then give it again."

// loginStepBindings are the keys of the code and secret steps.
type loginStepBindings struct {
	Open, Copy, Page, Focus, Submit, Next, Leave key.Binding
}

func loginStepKeys() loginStepBindings {
	return loginStepBindings{
		Open:   key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open")),
		Copy:   key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy")),
		Page:   key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "one-time page")),
		Focus:  key.NewBinding(key.WithKeys("enter", "tab"), key.WithHelp("⏎", "type it")),
		Submit: key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "send")),
		Next:   key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next field")),
		Leave:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "leave the field")),
	}
}

// loginAnswerMsg is the answer to what a step sent, for that step alone.
type loginAnswerMsg struct {
	to  step
	end LoginEnd
	err error
}

// loginPageMsg asks the flow to serve the one-time page in place of the
// step it came from.
type loginPageMsg struct {
	from step
	ask  pageAsk
}

// loginSaid is what a step says beside its field about the last answer:
// a wait, or why it failed.
type loginSaid struct {
	text string
	wait bool
}

// loginSending is what the code and secret steps share: what is awaiting
// its answer, and what was said of the last.
type loginSending struct {
	busy bool
	sent string
	said loginSaid
}

// sending says an answer is awaited. Until it comes the step is neither
// left nor replaced (no x, no close, no page), so the answer is always
// taken: one that finished the login is said, never lost.
func (s *loginSending) sending() bool { return s.busy }

// loginWaiting says s awaits an answer, when it can say.
func loginWaiting(s step) bool {
	w, ok := s.(interface{ sending() bool })
	return ok && w.sending()
}

// send sends value through f, once: while its answer is awaited nothing
// more is sent. The answer comes back as a loginAnswerMsg for to.
func (s *loginSending) send(e *env, to step, value string, f func(ctx context.Context, b Backend) (LoginEnd, error)) tea.Cmd {
	if s.busy || value == "" {
		return nil
	}
	s.busy, s.sent, s.said = true, value, loginSaid{}
	b := e.b
	return e.callOr(envFinishTimeout, func(ctx context.Context) tea.Msg {
		end, err := f(ctx, b)
		return loginAnswerMsg{to: to, end: end, err: err}
	}, func(err error) tea.Msg { return loginAnswerMsg{to: to, err: err} }, nil)
}

// answer takes the answer to what was sent: stepDone on success, and on a
// refusal no approval can put right (the login is gone, or no approval can
// finish it); otherwise it is said beside the field, which keeps its value.
// Whatever was sent is taken out of what is said.
func (s *loginSending) answer(msg loginAnswerMsg) tea.Cmd {
	s.busy = false
	switch {
	case msg.err != nil:
		text := loginScrub(msg.err.Error(), s.sent)
		var no *client.Refusal
		if errors.As(msg.err, &no) && (no.Status == 404 || no.Reason == "unfinishable") {
			err := errors.New(text)
			return func() tea.Msg { return stepDone{Err: err} }
		}
		s.said = loginSaid{text: text}
	case msg.end.Pending:
		s.said = loginSaid{text: loginNotYet, wait: true}
	default:
		end := msg.end
		return func() tea.Msg { return stepDone{End: end} }
	}
	return nil
}

// lines is what is said beside the field, indented to it.
func (s *loginSending) lines(th Theme, indent, width int) []string {
	text, style := s.said.text, th.Over
	switch {
	case s.busy:
		text, style = "sent; waiting for the answer", th.Muted
	case s.said.wait:
		style = th.Warn
	}
	var out []string
	for _, l := range wrap(clean(text), width-indent) {
		out = append(out, strings.Repeat(" ", indent)+style.Render(l))
	}
	return out
}

// loginScrub is text with value taken out wherever it appears, as the
// Backend takes out what was typed: a value shorter than any real code or
// key is left, since taking out a few letters would garble the sentence.
func loginScrub(text, value string) string {
	if v := strings.TrimSpace(value); len(v) >= 8 {
		text = strings.ReplaceAll(text, v, "[not repeated]")
	}
	return text
}

// loginTrim is a paste as a field takes it: line breaks gone, and the
// spaces around it.
func loginTrim(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(s))
}

// loginEnds is when l can no longer be finished: the store's own bound,
// counted from when it began. A login that says no time began now.
func loginEnds(l *backendLogin, now time.Time) time.Time {
	if l.CreatedAt <= 0 {
		return now.Add(logins.TTL)
	}
	return time.UnixMilli(l.CreatedAt).Add(logins.TTL)
}

// loginField is in drawn after a label, then a countdown when ends is set:
// its value cleaned when it holds anything a terminal would act on or
// shaped like a key, so what was pasted is never drawn as it is.
func loginField(th Theme, label string, in textinput.Model, ends, now time.Time) string {
	if v := in.Value(); clean(v) != v {
		in.SetValue(clean(v))
	}
	line := th.Muted.Render(label) + in.View()
	if !ends.IsZero() {
		line += "  " + th.Muted.Render(timeLeft(ends, now)+" left")
	}
	return line
}

// loginLabelWidth is how wide a field's label is drawn, and so where what
// is said beside it starts.
const loginLabelWidth = 7

// loginInputWidth is how wide a field is in a terminal of width columns:
// what its label and its countdown leave, so the line never wraps.
func loginInputWidth(width int) int {
	return max(width-2-loginLabelWidth-len("  99m left")-1, 8)
}

// loginHints is the keys a step offers, as the footer draws them.
func loginHints(th Theme, bs ...key.Binding) string {
	var parts []string
	for _, b := range bs {
		if b.Enabled() {
			h := b.Help()
			parts = append(parts, th.Key.Render(h.Key)+" "+th.Muted.Render(h.Desc))
		}
	}
	return strings.Join(parts, "  ")
}

// loginPara is text cleaned and wrapped to width, in style.
func loginPara(style interface{ Render(...string) string }, text string, width int) []string {
	var out []string
	for _, l := range wrap(clean(text), width) {
		out = append(out, style.Render(l))
	}
	return out
}

// loginCode is the code step: the link to approve at, and the field the
// code that approval gives goes in. It starts blurred, so o, c and w work;
// tab or enter focuses it, and a paste focuses, fills and sends it (R16).
type loginCode struct {
	loginSending
	e    *env
	l    *backendLogin
	who  string
	keys loginStepBindings
	in   textinput.Model
	ends time.Time
}

// newCodeStep is the code step for l, approved as who.
func newCodeStep(e *env, l *backendLogin, who string) step {
	s := &loginCode{e: e, l: l, who: who, keys: loginStepKeys(), in: newInput(e.th), ends: loginEnds(l, e.now())}
	s.in.Prompt = ""
	s.size(e.width)
	return s
}

func (s *loginCode) size(width int) {
	s.in.SetWidth(loginInputWidth(width))
	s.in.SetValue(s.in.Value())
}

func (s *loginCode) Typing() bool { return s.in.Focused() }

func (s *loginCode) arm() {
	k, typing := &s.keys, s.in.Focused()
	k.Open.SetEnabled(!typing && s.e.o.OpenURL != nil)
	for _, b := range []*key.Binding{&k.Copy, &k.Focus} {
		b.SetEnabled(!typing)
	}
	k.Page.SetEnabled(!typing && !s.busy)
	k.Submit.SetEnabled(typing)
	k.Leave.SetEnabled(typing)
	k.Next.SetEnabled(false)
}

func (s *loginCode) Keys() []key.Binding {
	s.arm()
	k := s.keys
	return loginEnabled(k.Submit, k.Leave, k.Focus, k.Open, k.Copy, k.Page)
}

// loginEnabled is the bindings that are enabled, in order.
func loginEnabled(bs ...key.Binding) []key.Binding {
	var out []key.Binding
	for _, b := range bs {
		if b.Enabled() {
			out = append(out, b)
		}
	}
	return out
}

func (s *loginCode) Update(msg tea.Msg) (step, tea.Cmd) {
	switch msg := msg.(type) {
	case loginAnswerMsg:
		if msg.to == step(s) {
			return s, s.answer(msg)
		}
	case tea.PasteMsg:
		return s, s.paste(msg.Content)
	case tea.WindowSizeMsg:
		s.size(msg.Width)
	case tea.KeyPressMsg:
		return s, s.key(msg)
	}
	return s, nil
}

func (s *loginCode) key(msg tea.KeyPressMsg) tea.Cmd {
	s.arm()
	k := &s.keys
	switch {
	case key.Matches(msg, k.Submit):
		return s.submit()
	case key.Matches(msg, k.Leave):
		s.in.Blur()
		return nil
	case key.Matches(msg, k.Focus):
		return s.in.Focus()
	case key.Matches(msg, k.Open):
		return openURL(s.e, s.l.URL)
	case key.Matches(msg, k.Copy):
		return loginCopied(s.l.URL)
	case key.Matches(msg, k.Page):
		return loginPageFor(s, pageAsk{Want: loginpage.Code, Login: s.l, Approve: s.l.URL, Account: s.l.Account})
	}
	if !s.in.Focused() {
		return nil
	}
	var cmd tea.Cmd
	s.in, cmd = s.in.Update(msg)
	return cmd
}

// loginCopied puts text on the clipboard of the person's terminal, and says
// it was sent there: whether the terminal took it nobody can tell (R20).
func loginCopied(text string) tea.Cmd {
	return tea.Batch(copyText(text), toast(levelInfo, "the link was sent to your terminal's clipboard"))
}

// loginPageFor asks the flow for the one-time page in place of from.
func loginPageFor(from step, ask pageAsk) tea.Cmd {
	return func() tea.Msg { return loginPageMsg{from: from, ask: ask} }
}

// paste fills the field with what was pasted, focused, and sends it: once,
// so a paste while an answer is awaited is dropped and the field keeps what
// was sent.
func (s *loginCode) paste(content string) tea.Cmd {
	if s.busy {
		return nil
	}
	v := loginTrim(content)
	cmd := s.in.Focus()
	s.in.SetValue(v)
	s.in.CursorEnd()
	return tea.Batch(cmd, s.submit())
}

// submit sends the code, unless it looks like a key or a token: that would
// hand a credential to the provider's token endpoint as a code, so it stays
// in the field, drawn redacted, and nothing is sent.
func (s *loginCode) submit() tea.Cmd {
	id, v := s.l.ID, strings.TrimSpace(s.in.Value())
	if loginSecretLike(v) {
		if !s.busy {
			s.said = loginSaid{text: loginNoCode}
		}
		return nil
	}
	return s.send(s.e, s, v, func(ctx context.Context, b Backend) (LoginEnd, error) {
		return b.FinishLogin(ctx, id, v)
	})
}

func (s *loginCode) View(width int) string {
	th, w := s.e.th, width-2
	head := "Open this and approve access as " + s.who
	if s.l.Account != 0 {
		head += ", which is " + s.l.Provider + " account " + strconv.Itoa(s.l.Account) + "; no other account's approval will finish it"
	}
	lines := loginPara(th.Text, head+":", w)
	lines = append(lines, "")
	for _, l := range wrap(clean(s.l.URL), w-2) {
		lines = append(lines, "  "+th.Accent.Render(l))
	}
	s.arm()
	k := s.keys
	lines = append(lines, "", "  "+loginHints(th, k.Open, k.Copy, k.Page, k.Focus, k.Submit, k.Leave), "")
	in := s.in
	if loginSecretLike(in.Value()) {
		// A key pasted here by mistake shows as stars, whatever its shape.
		in.EchoMode = textinput.EchoPassword
	}
	lines = append(lines, loginField(th, fit("code", loginLabelWidth), in, s.ends, s.e.now()))
	lines = append(lines, s.lines(th, loginLabelWidth, w)...)
	return strings.Join(lines, "\n")
}

// loginSecret is the secret step: a key or a token, pasted or typed into a
// field that draws only stars, and for a new key a label. Like the code
// step it starts blurred, and a paste focuses, fills and sends the secret,
// or with a label to give moves on to it.
type loginSecret struct {
	loginSending
	e            *env
	ask          secretAsk
	keys         loginStepBindings
	value, label textinput.Model
	ends         time.Time
}

// newSecretStep is the secret step asking for a.
func newSecretStep(e *env, a secretAsk) step {
	s := &loginSecret{e: e, ask: a, keys: loginStepKeys(), value: newInput(e.th), label: newInput(e.th)}
	s.value.Prompt, s.label.Prompt = "", ""
	s.value.EchoMode = textinput.EchoPassword
	if a.Login != nil {
		s.ends = loginEnds(a.Login, e.now())
	}
	s.size(e.width)
	return s
}

func (s *loginSecret) size(width int) {
	for _, in := range []*textinput.Model{&s.value, &s.label} {
		in.SetWidth(loginInputWidth(width))
		in.SetValue(in.Value())
	}
}

func (s *loginSecret) Typing() bool { return s.value.Focused() || s.label.Focused() }

// url is the page the secret is made at, when the login names one.
func (s *loginSecret) url() string {
	if s.ask.Login != nil {
		return s.ask.Login.URL
	}
	return ""
}

func (s *loginSecret) arm() {
	k, typing := &s.keys, s.Typing()
	k.Open.SetEnabled(!typing && s.url() != "" && s.e.o.OpenURL != nil)
	k.Copy.SetEnabled(!typing && s.url() != "")
	k.Page.SetEnabled(!typing && !s.busy)
	k.Focus.SetEnabled(!typing)
	k.Submit.SetEnabled(typing)
	k.Next.SetEnabled(typing && s.ask.Label)
	k.Leave.SetEnabled(typing)
}

func (s *loginSecret) Keys() []key.Binding {
	s.arm()
	k := s.keys
	return loginEnabled(k.Submit, k.Next, k.Leave, k.Focus, k.Open, k.Copy, k.Page)
}

func (s *loginSecret) Update(msg tea.Msg) (step, tea.Cmd) {
	switch msg := msg.(type) {
	case loginAnswerMsg:
		if msg.to == step(s) {
			return s, s.answer(msg)
		}
	case tea.PasteMsg:
		return s, s.paste(msg.Content)
	case tea.WindowSizeMsg:
		s.size(msg.Width)
	case tea.KeyPressMsg:
		return s, s.key(msg)
	}
	return s, nil
}

func (s *loginSecret) key(msg tea.KeyPressMsg) tea.Cmd {
	s.arm()
	k := &s.keys
	switch {
	case key.Matches(msg, k.Submit):
		if s.value.Focused() && s.ask.Label {
			return s.focus(&s.label)
		}
		return s.submit()
	case key.Matches(msg, k.Next):
		if s.value.Focused() {
			return s.focus(&s.label)
		}
		return s.focus(&s.value)
	case key.Matches(msg, k.Leave):
		s.value.Blur()
		s.label.Blur()
		return nil
	case key.Matches(msg, k.Focus):
		return s.focus(&s.value)
	case key.Matches(msg, k.Open):
		return openURL(s.e, s.url())
	case key.Matches(msg, k.Copy):
		return loginCopied(s.url())
	case key.Matches(msg, k.Page):
		a := s.ask
		return loginPageFor(s, pageAsk{Want: a.Want, Login: a.Login, Approve: s.url(), Account: a.Account, Label: strings.TrimSpace(s.label.Value())})
	}
	var cmd tea.Cmd
	switch {
	case s.value.Focused():
		s.value, cmd = s.value.Update(msg)
	case s.label.Focused():
		s.label, cmd = s.label.Update(msg)
	}
	return cmd
}

// focus gives in the focus, and takes it from the other field.
func (s *loginSecret) focus(in *textinput.Model) tea.Cmd {
	s.value.Blur()
	s.label.Blur()
	return in.Focus()
}

// paste fills the label when it has focus; otherwise the secret, which is
// then sent, or the label is moved on to when there is one to give.
func (s *loginSecret) paste(content string) tea.Cmd {
	if s.busy {
		return nil
	}
	v := loginTrim(content)
	if s.label.Focused() {
		s.label.SetValue(v)
		s.label.CursorEnd()
		return nil
	}
	cmd := s.focus(&s.value)
	s.value.SetValue(v)
	s.value.CursorEnd()
	if v != "" && s.ask.Label {
		return tea.Batch(cmd, s.focus(&s.label))
	}
	return tea.Batch(cmd, s.submit())
}

func (s *loginSecret) submit() tea.Cmd {
	v := strings.TrimSpace(s.value.Value())
	if v == "" {
		return s.focus(&s.value)
	}
	a, label, id := s.ask, strings.TrimSpace(s.label.Value()), ""
	if a.Login != nil {
		id = a.Login.ID
	}
	return s.send(s.e, s, v, func(ctx context.Context, b Backend) (LoginEnd, error) {
		switch {
		case id != "":
			return b.FinishLogin(ctx, id, v)
		case a.Want == loginpage.Token:
			return b.PasteToken(ctx, a.Account, v)
		}
		return b.PasteKey(ctx, v, label)
	})
}

// head is what the step asks for, and where it goes.
func (s *loginSecret) head() string {
	a := s.ask
	switch {
	case a.Login != nil:
		return "Make a " + a.Login.Provider + " API key at this page, then paste it here; it is never shown:"
	case a.Want == loginpage.Token:
		return "Paste the token `claude setup-token` printed; it goes on account " + strconv.Itoa(a.Account) +
			" as its long-lived token, and is never shown."
	}
	return "Paste the Claude Console API key; it is never shown."
}

func (s *loginSecret) View(width int) string {
	th, w := s.e.th, width-2
	lines := loginPara(th.Text, s.head(), w)
	if s.ask.Want == loginpage.Key && s.ask.Account != 0 {
		lines = append(lines, loginPara(th.Muted, "the same key again keeps this account; a different one makes an account of its own", w)...)
	}
	if u := s.url(); u != "" {
		lines = append(lines, "")
		for _, l := range wrap(clean(u), w-2) {
			lines = append(lines, "  "+th.Accent.Render(l))
		}
	}
	s.arm()
	k := s.keys
	lines = append(lines, "", "  "+loginHints(th, k.Open, k.Copy, k.Page, k.Focus, k.Submit, k.Next, k.Leave), "")
	what := "key"
	if s.ask.Want == loginpage.Token {
		what = "token"
	}
	lines = append(lines, loginField(th, fit(what, loginLabelWidth), s.value, s.ends, s.e.now()))
	if s.ask.Label {
		lines = append(lines, loginField(th, fit("label", loginLabelWidth), s.label, time.Time{}, s.e.now()))
	}
	lines = append(lines, s.lines(th, loginLabelWidth, w)...)
	return strings.Join(lines, "\n")
}
