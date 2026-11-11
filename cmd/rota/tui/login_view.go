package tui

// What the login flow says and draws: the choices it offers with what each
// gives up, whom a login is approved as, its title, its picks and its badge;
// and what the shell asks of it beside: what it starts as it opens, what it
// is about, the login it holds, and what its step keeps running.

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/wire"
)

// awaiting says the flow awaits an answer for its parked login: its step's
// to what it sent, or the server's to a finish with no code. Until it comes
// the flow does not end, since that answer may have finished the login.
func (f *loginFlow) awaiting() bool {
	return (f.stepping() && loginWaiting(f.step)) || (f.stage == loginBeginning && f.login != nil)
}

// loginKeyStart finds an API key's start anywhere in a value, at a word's
// start as clean finds an Anthropic key: an OpenAI or Anthropic "sk-", an
// xAI "xai-", after an "OPENAI_API_KEY=" or a quote as well.
var loginKeyStart = regexp.MustCompile(`(^|[^A-Za-z0-9])(sk|xai)-`)

// loginNoCode is what the code step says of a value loginSecretLike refuses.
const loginNoCode = "that is not a code: it looks like a key or a token, or holds characters no code has; nothing was sent"

// loginSecretLike says v, as it would be sent (trimmed), is no code: clean
// would change it (an Anthropic key or token anywhere in it, or anything a
// terminal acts on), or it holds the start of another provider's key.
func loginSecretLike(v string) bool {
	v = strings.TrimSpace(v)
	return clean(v) != v || loginKeyStart.MatchString(v)
}

// loginAgainKinds are l's kinds for a claude login: short, bound to the
// account; long, which goes on whichever account approves it; and a setup
// token pasted for it (R13).
func loginAgainKinds() []loginOption {
	return []loginOption{
		{"short", "A login rota refreshes, which only this account's approval finishes; usage readings and Remote Control need it.", loginShort},
		{"long", "A year, inference only: no usage readings or Remote Control. The token goes on whichever account approves it.", loginLong},
		{"setup token", "A token `claude setup-token` printed, pasted: a year, inference only, on this account.", loginToken},
	}
}

// loginHows are how an account of provider p, whose login takes a key and
// which can sign itself in, is made.
func loginHows(p string) []loginOption {
	return []loginOption{
		{"API key", "A key made at " + p + "'s page, pasted here.", loginKey},
		{"let " + p + " sign itself in", p + "'s own CLI signs the account in, in a folder rota gives it; rota holds no credential for it.", loginSelf},
	}
}

// loginKinds are n's kinds for provider p: short and long, and an API key
// for claude alone, whose keys rota takes (R14).
func loginKinds(p string) []loginOption {
	out := []loginOption{
		{"short", "A login rota refreshes; usage readings and Remote Control need it.", loginShort},
		{"long", "A year, inference only: no usage readings or Remote Control. Two approvals: the login that makes the account, then the year-long token.", loginLong},
	}
	if p == rota.DefaultProvider {
		out = append(out, loginOption{"API key", "A Claude Console key, paid per token: no usage readings or Remote Control; it starts out of the rotation.", loginKey})
	}
	return out
}

// loginKinded says whether n asks p's kind: claude, and a provider with a
// long-lived token.
func loginKinded(p string) bool {
	if p == rota.DefaultProvider {
		return true
	}
	prov, err := rota.Lookup(p)
	if err != nil {
		return false
	}
	_, ok := prov.(rota.LongLived)
	return ok
}

func (f *loginFlow) againID() int {
	if f.again == nil {
		return 0
	}
	return f.again.ID
}

// who is whom the login is approved as: the account signed in again, the
// account n long just made, or anyone's for a new account.
func (f *loginFlow) who() string {
	switch {
	case f.longWho != "":
		return f.longWho
	case f.again != nil:
		return loginWho(*f.again)
	}
	return "your " + clean(f.provider) + " account"
}

// loginWho names an account to approve as: its e-mail, as a browser shows
// whom it is signed in as, else its label.
func loginWho(a wire.Account) string {
	if e := clean(a.Email); strings.TrimSpace(e) != "" {
		return e
	}
	return accountsLabel(a)
}

// date is an RFC 3339 time as a day in the TUI's zone.
func (f *loginFlow) date(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.In(f.e.o.Loc).Format(time.DateOnly)
}

// Badge is the flow put aside, in the footer: its login, how long it stays
// open, and the key that brings it back.
func (f *loginFlow) Badge(now time.Time) string {
	if f.login == nil {
		return "login · n returns"
	}
	id := f.login.ID
	if len(id) > 6 {
		id = id[:6]
	}
	return "login " + id + " · " + timeLeft(f.ends, now) + " left · n returns"
}

// title is what the flow does, for which account.
func (f *loginFlow) title() string {
	switch {
	case f.again != nil:
		a := f.again
		return "Sign " + a.Provider + " account " + strconv.Itoa(a.ID) + " (" + accountsLabel(*a) + ") in again"
	case f.provider != "":
		return "Add a " + f.provider + " account"
	}
	return "Add an account"
}

func (f *loginFlow) View(width, height int) string {
	th, w := f.e.th, width-2
	lines := []string{th.Title.Render(strings.TrimRight(fit(clean(f.title()), w), " ")), ""}
	switch f.stage {
	case loginPicking:
		lines = append(lines, f.pickView(w)...)
	case loginBeginning:
		lines = append(lines, th.Muted.Render("asking the provider"))
	case loginStepping:
		if f.step != nil {
			lines = append(lines, strings.Split(f.step.View(width), "\n")...)
		}
	case loginEnded:
		lines = append(lines, loginPara(th.Over, f.ended+"; n begins another", w)...)
	}
	for i, l := range lines {
		if l != "" {
			lines[i] = " " + l
		}
	}
	if len(lines) > height {
		lines = lines[:max(height, 0)]
	}
	return strings.Join(lines, "\n")
}

// pickView is the pick shown: its question, its options with the one
// picked marked, each with what it gives up, and why the last begin was
// refused.
func (f *loginFlow) pickView(w int) []string {
	th := f.e.th
	var lines []string
	switch f.pick {
	case loginPickProvider:
		lines = append(lines, th.Muted.Render("Which provider?"), "")
	case loginPickKind, loginPickAgain:
		lines = append(lines, th.Muted.Render("What kind of login?"), "")
	case loginPickHow:
		lines = append(lines, th.Muted.Render("How does it sign in?"), "")
	case loginConfirm:
		lines = append(lines, loginPara(th.Text, "A login only this account's approval finishes; ⏎ begins it.", w)...)
	}
	for i, o := range f.opts {
		mark, style := "  ", th.Text
		if i == f.at {
			mark, style = "> ", th.Accent
		}
		lines = append(lines, style.Render(strings.TrimRight(fit(mark+clean(o.name), w), " ")))
		for _, l := range wrap(clean(o.about), w-4) {
			lines = append(lines, "    "+th.Muted.Render(l))
		}
	}
	if f.said != "" {
		lines = append(lines, "")
		lines = append(lines, loginPara(th.Over, f.said, w)...)
	}
	return lines
}

// Init starts the flow as the shell opens it, and as n begins it again:
// l on an account whose login is begun one way only (neither claude's
// kinds nor a key to paste) begins it at once, with no enter, and a step
// shown from the start is started.
func (f *loginFlow) Init() tea.Cmd {
	switch {
	case f.stage == loginPicking && f.pick == loginConfirm:
		return f.choose()
	case f.stepping():
		return f.step.Init()
	}
	return nil
}

// About is the flow and the account it signs in again.
func (f *loginFlow) About() (string, int) { return "login", f.againID() }

// Pending is the login parked on the server, while the flow still waits
// for it.
func (f *loginFlow) Pending() *backendLogin { return f.login }

// Busy is what the step keeps running, when it says: the one-time page.
func (f *loginFlow) Busy() string {
	if b, ok := f.step.(interface{ Busy() string }); ok {
		return b.Busy()
	}
	return ""
}

// Leave closes what the step holds open, when Run ends.
func (f *loginFlow) Leave(ctx context.Context) {
	if l, ok := f.step.(programLeaver); ok {
		l.Leave(ctx)
	}
}
