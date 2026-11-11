package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/cmd/rota/loginpage"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

// loginNow is the TUI's clock in these tests.
var loginNow = time.Date(2026, 11, 10, 20, 17, 0, 0, time.UTC)

const (
	// loginURL is where every login a fake begins is approved.
	loginURL = "https://claude.example/oauth/authorize?code=true&client_id=c&state=s"
	// loginWrong is the server's sentence for a code the provider turned
	// down: a 400 in the provider's words, the login still parked.
	loginWrong = "authorization code was rejected (expired, already used, or mismatched): Invalid authorization code; try the code again, or approve again at the same link"
	// The kinds' words, each saying what it gives up.
	loginShortText = "A login rota refreshes; usage readings and Remote Control need it."
	loginLongText  = "A year, inference only: no usage readings or Remote Control. Two approvals: the login that makes the account, then the year-long token."
	loginKeyText   = "A Claude Console key, paid per token: no usage readings or Remote Control; it starts out of the rotation."
)

// loginRows are the accounts these tests sign in again: two claude logins,
// a codex one, a claude API key and a grok one.
func loginRows() []wire.Account {
	return []wire.Account{
		{ID: 12, Provider: "claude", Email: "a@x", UUID: "u12", Order: 1, Threshold: 75, Metered: true},
		{ID: 13, Provider: "claude", Email: "b@x", UUID: "u13", Order: 2, Threshold: 75, Metered: true},
		{ID: 6, Provider: "codex", Email: "c@x", UUID: "u6", Order: 3, Threshold: 75, Metered: true},
		{ID: 9, Provider: "claude", Name: "console", UUID: "key-9", Credential: "apikey", Order: 4, Threshold: 75},
		{ID: 7, Provider: "grok", UUID: "grok-7", Order: 5, Threshold: 75},
	}
}

// loginModel is the TUI with the Accounts tab over a fake backend listing
// rows, started at 80×24 on the fake clock. Every login the fake begins is
// a code login named abc123, as the API answers one: no state, begun now.
func loginModel(t *testing.T, rows []wire.Account, with ...func(*Options)) (*Model, *fakeBackend, *fakeTicks) {
	t.Helper()
	shellRegistry(t)
	register(tabsAccounts, newAccounts)
	ticks := &fakeTicks{clock: loginNow}
	b := &fakeBackend{AccountsFn: func(context.Context, Refresh) (Listing, error) {
		return Listing{Accounts: rows}, nil
	}}
	b.BeginLoginFn = func(_ context.Context, lb LoginBegin) (backendLogin, error) {
		p := lb.Provider
		for _, r := range rows {
			if r.ID == lb.Account {
				p = r.Provider
			}
		}
		if p == "" {
			p = "claude"
		}
		return backendLogin{ID: "abc123", Provider: p, URL: loginURL, Kind: "code", Long: lb.Long,
			Account: lb.Account, CreatedAt: ticks.now().UnixMilli()}, nil
	}
	m := shellModel(t, b, ticks, with...)
	shellStart(t, m)
	return m, b, ticks
}

// loginPress sends each key in a run of its own, as a person types them:
// what a key opens is there for the next one.
func loginPress(t *testing.T, m *Model, keys ...string) []tea.Msg {
	t.Helper()
	var seen []tea.Msg
	for _, k := range keys {
		_, got := run(t, m, press(k)...)
		seen = append(seen, got...)
	}
	return seen
}

// loginSettle is run, then waits for every command still running, as a
// call to the real API under load may be.
func loginSettle(t *testing.T, m *Model, msgs ...tea.Msg) []tea.Msg {
	t.Helper()
	_, seen := run(t, m, msgs...)
	for len(harnessDriverOf(t).leftover()) > 0 {
		_, more := await(t, m)
		seen = append(seen, more...)
	}
	return seen
}

// loginOf is the login flow, open or put aside, or nil.
func loginOf(m *Model) *loginFlow {
	for i := len(m.stack) - 1; i >= 0; i-- {
		if f, ok := m.stack[i].(*loginFlow); ok {
			return f
		}
	}
	if f, ok := m.aside.(*loginFlow); ok {
		return f
	}
	return nil
}

func loginFlowOf(t *testing.T, m *Model) *loginFlow {
	t.Helper()
	f := loginOf(m)
	if f == nil {
		t.Fatalf("no login flow is open:\n%s", plain(m))
	}
	return f
}

func loginCodeOf(t *testing.T, f *loginFlow) *loginCode {
	t.Helper()
	s, ok := f.step.(*loginCode)
	if !ok {
		t.Fatalf("the flow shows %T, not the code step", f.step)
	}
	return s
}

func loginSecretOf(t *testing.T, f *loginFlow) *loginSecret {
	t.Helper()
	s, ok := f.step.(*loginSecret)
	if !ok {
		t.Fatalf("the flow shows %T, not the secret step", f.step)
	}
	return s
}

// loginText is the whole screen as words: escapes gone, and every run of
// spaces and line breaks one space, so a sentence wrapped across lines
// reads whole.
func loginText(m *Model) string {
	return strings.Join(strings.Fields(plain(m)), " ")
}

// loginToasts are the texts of every toast among msgs.
func loginToasts(msgs []tea.Msg) []string {
	var out []string
	for _, msg := range msgs {
		if t, ok := msg.(toastMsg); ok {
			out = append(out, t.Text)
		}
	}
	return out
}

func loginSaw(t *testing.T, msgs []tea.Msg, want tea.Msg) {
	t.Helper()
	for _, msg := range msgs {
		if msg == want {
			return
		}
	}
	t.Errorf("no %#v among %#v", want, msgs)
}

func loginToasted(t *testing.T, msgs []tea.Msg, want string) {
	t.Helper()
	if got := loginToasts(msgs); !slices.Contains(got, want) {
		t.Errorf("no toast %q among %q", want, got)
	}
}

// loginBegins is the LoginBegin of every BeginLogin the backend took.
func loginBegins(b *fakeBackend) []LoginBegin {
	var out []LoginBegin
	for _, c := range b.named("BeginLogin") {
		out = append(out, c.Args[0].(LoginBegin))
	}
	return out
}

// loginFinishes is every FinishLogin the backend took, as id and code.
func loginFinishes(b *fakeBackend) [][2]string {
	var out [][2]string
	for _, c := range b.named("FinishLogin") {
		out = append(out, [2]string{c.Args[0].(string), c.Args[1].(string)})
	}
	return out
}

// loginSelect selects an account in the Accounts tab.
func loginSelect(t *testing.T, m *Model, id int) {
	t.Helper()
	run(t, m, selectMsg{Account: id})
	if accountsOf(m).sel != id {
		t.Fatalf("account %d is not selected", id)
	}
}

func TestAddAClaudeAccountWithACode(t *testing.T) {
	m, b, _ := loginModel(t, nil)
	b.FinishLoginFn = func(_ context.Context, id, code string) (LoginEnd, error) {
		if code == "GOOD" {
			return LoginEnd{Account: 14, Provider: "claude", Email: "n@x", Status: "added"}, nil
		}
		return LoginEnd{}, &client.Refusal{Status: 400, Message: loginWrong}
	}
	loginPress(t, m, "n", "enter", "enter")
	if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Provider: "claude"}}) {
		t.Fatalf("begun %+v", got)
	}
	f := loginFlowOf(t, m)
	if txt := loginText(m); !strings.Contains(txt, "Open this and approve access as your claude account: "+loginURL) {
		t.Errorf("the step does not say what to open:\n%s", plain(m))
	}

	run(t, m, tea.PasteMsg{Content: "WRONG"})
	if v := loginCodeOf(t, f).in.Value(); v != "WRONG" {
		t.Errorf("the field holds %q after a refused code", v)
	}
	// The provider's words, right under the field the code is still in.
	if txt := loginText(m); !strings.Contains(txt, "code WRONG 15m left "+loginWrong) {
		t.Errorf("the refusal is not beside the field:\n%s", plain(m))
	}
	if loginOf(m) != f || f.Closed() {
		t.Fatal("a wrong code ended the flow")
	}

	_, seen := run(t, m, tea.PasteMsg{Content: "GOOD"})
	loginSaw(t, seen, selectMsg{Account: 14})
	loginSaw(t, seen, refreshMsg{How: RefreshStale})
	loginToasted(t, seen, "Added claude account 14")
	if got := loginFinishes(b); !slices.Equal(got, [][2]string{{"abc123", "WRONG"}, {"abc123", "GOOD"}}) {
		t.Errorf("finished %q", got)
	}
	if loginOf(m) != nil {
		t.Error("the flow is still open after the account was added")
	}
	if !strings.Contains(plain(m), "Added claude account 14") {
		t.Errorf("the toast is not shown:\n%s", plain(m))
	}
}

func TestTheKindSaysWhatEachGivesUp(t *testing.T) {
	m, b, _ := loginModel(t, nil)
	loginPress(t, m, "n")
	if txt := loginText(m); !strings.Contains(txt, "> claude") {
		t.Errorf("claude is not preselected:\n%s", plain(m))
	}
	loginPress(t, m, "enter")
	txt := loginText(m)
	for _, s := range []string{"> short " + loginShortText, "long " + loginLongText, "API key " + loginKeyText} {
		if !strings.Contains(txt, s) {
			t.Errorf("the kind does not say %q:\n%s", s, plain(m))
		}
	}
	if n := len(b.named("BeginLogin")); n != 0 {
		t.Errorf("picking began %d logins", n)
	}
	loginPress(t, m, "esc")
	if loginOf(m) != nil {
		t.Fatal("esc on a pick left the flow open")
	}

	// codex has no long token and no key: its login begins once picked.
	loginPress(t, m, "n")
	at := slices.Index(wire.LoginProviders(), "codex") - slices.Index(wire.LoginProviders(), "claude")
	for range at {
		loginPress(t, m, "down")
	}
	loginPress(t, m, "enter")
	if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Provider: "codex"}}) {
		t.Fatalf("begun %+v", got)
	}
	txt = loginText(m)
	if strings.Contains(txt, loginShortText) || !strings.Contains(txt, "approve access as your codex account") {
		t.Errorf("codex was asked its kind, or shows no step:\n%s", plain(m))
	}
	loginCodeOf(t, loginFlowOf(t, m))
}

func TestAgainIsBoundToTheAccount(t *testing.T) {
	m, b, _ := loginModel(t, loginRows())
	loginSelect(t, m, 12)
	loginPress(t, m, "l")
	if txt := loginText(m); !strings.Contains(txt, "Sign claude account 12 (a@x) in again") || !strings.Contains(txt, "> short") {
		t.Errorf("l on a claude login does not ask its kind:\n%s", plain(m))
	}
	loginPress(t, m, "enter")
	if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Account: 12}}) {
		t.Fatalf("begun %+v", got)
	}
	if txt := loginText(m); !strings.Contains(txt,
		"Open this and approve access as a@x, which is claude account 12; no other account's approval will finish it: "+loginURL) {
		t.Errorf("the step does not say it is bound:\n%s", plain(m))
	}
	// c copies the link, and says only that it was sent; with no browser
	// here there is no o.
	seen := loginPress(t, m, "c", "o")
	loginSaw(t, seen, clipboardMsg{Text: loginURL})
	loginToasted(t, seen, "the link was sent to your terminal's clipboard")
	if strings.Contains(loginText(m), "o open") {
		t.Errorf("o is offered with no browser:\n%s", plain(m))
	}
	loginPress(t, m, "x")

	// Another provider's account has no kind to pick: enter begins its login.
	loginSelect(t, m, 6)
	loginPress(t, m, "l")
	if n := len(b.named("BeginLogin")); n != 1 {
		t.Fatalf("l alone began a login (%d)", n)
	}
	loginPress(t, m, "enter")
	if got := loginBegins(b); !slices.Equal(got[1:], []LoginBegin{{Account: 6}}) {
		t.Fatalf("begun %+v", got)
	}
	if txt := loginText(m); !strings.Contains(txt, "approve access as c@x, which is codex account 6; no other account's approval will finish it") {
		t.Errorf("the codex step is not bound:\n%s", plain(m))
	}

	// With a browser on this machine, o opens the link in it.
	var opened []string
	m, _, _ = loginModel(t, loginRows(), func(o *Options) {
		o.OpenURL = func(u string) error { opened = append(opened, u); return nil }
	})
	loginPress(t, m, "l", "enter")
	if !strings.Contains(loginText(m), "o open") {
		t.Errorf("o is not offered with a browser:\n%s", plain(m))
	}
	loginPress(t, m, "o")
	if !slices.Equal(opened, []string{loginURL}) {
		t.Errorf("opened %q", opened)
	}
}

func TestALongTokenThatWentElsewhereIsSaid(t *testing.T) {
	long := func(account int) LoginEnd {
		return LoginEnd{Account: account, Provider: "claude", Status: "long", LongUntil: "2027-11-10T20:17:00Z"}
	}
	finish := func(_ context.Context, id, code string) (LoginEnd, error) {
		switch code {
		case "LONG13":
			return long(13), nil
		case "LONG12":
			return long(12), nil
		case "LONG14":
			return long(14), nil
		case "GOOD":
			return LoginEnd{Account: 14, Provider: "claude", Email: "n@x", Status: "added"}, nil
		}
		return LoginEnd{}, &client.Refusal{Status: 400, Message: loginWrong}
	}
	const went = "the long token went on account 13, which approved it"

	t.Run("again", func(t *testing.T) {
		m, b, _ := loginModel(t, loginRows())
		b.FinishLoginFn = finish
		loginSelect(t, m, 12)
		loginPress(t, m, "l", "down", "enter")
		if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Provider: "claude", Long: true}}) {
			t.Fatalf("begun %+v", got)
		}
		if txt := loginText(m); !strings.Contains(txt, "approve access as a@x: "+loginURL) {
			t.Errorf("the long login does not say whom to approve as:\n%s", plain(m))
		}
		_, seen := run(t, m, tea.PasteMsg{Content: "LONG13"})
		loginToasted(t, seen, "Long-lived token stored for #13, until 2027-11-10")
		loginToasted(t, seen, went)
		loginSaw(t, seen, selectMsg{Account: 13})
		if loginOf(m) != nil {
			t.Error("the flow is still open")
		}

		loginSelect(t, m, 12)
		loginPress(t, m, "l", "down", "enter")
		_, seen = run(t, m, tea.PasteMsg{Content: "LONG12"})
		loginToasted(t, seen, "Long-lived token stored for #12, until 2027-11-10")
		if got := loginToasts(seen); slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "went on") }) {
			t.Errorf("a token that went where it was asked is said to have gone elsewhere: %q", got)
		}
	})

	t.Run("new", func(t *testing.T) {
		m, b, _ := loginModel(t, nil)
		b.FinishLoginFn = finish
		loginPress(t, m, "n", "enter", "down", "enter")
		if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Provider: "claude"}}) {
			t.Fatalf("n long began %+v first; the account comes first", got)
		}
		_, seen := run(t, m, tea.PasteMsg{Content: "GOOD"})
		loginToasted(t, seen, "Added claude account 14")
		loginSaw(t, seen, selectMsg{Account: 14})
		if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Provider: "claude"}, {Provider: "claude", Long: true}}) {
			t.Fatalf("the long login did not follow: %+v", got)
		}
		f := loginFlowOf(t, m)
		if txt := loginText(m); !strings.Contains(txt, "approve access as n@x: "+loginURL) || loginCodeOf(t, f).in.Value() != "" {
			t.Errorf("the second approval does not name the account just added, or keeps the first code:\n%s", plain(m))
		}
		_, seen = run(t, m, tea.PasteMsg{Content: "LONG14"})
		loginToasted(t, seen, "Long-lived token stored for #14, until 2027-11-10")
		if slices.Contains(loginToasts(seen), went) || loginOf(m) != nil {
			t.Errorf("toasts %q, flow open %v", loginToasts(seen), loginOf(m) != nil)
		}

		loginPress(t, m, "n", "enter", "down", "enter")
		run(t, m, tea.PasteMsg{Content: "GOOD"})
		_, seen = run(t, m, tea.PasteMsg{Content: "LONG13"})
		loginToasted(t, seen, "the long token went on account 13, which approved it")

		// The account is made when the long login cannot begin: the flow
		// ends saying why, rather than offering to add it again.
		begin := b.BeginLoginFn
		b.BeginLoginFn = func(ctx context.Context, lb LoginBegin) (backendLogin, error) {
			if lb.Long {
				return backendLogin{}, &client.Refusal{Status: 400, Message: "claude issues no long-lived token today"}
			}
			return begin(ctx, lb)
		}
		loginPress(t, m, "n", "enter", "down", "enter")
		_, seen = run(t, m, tea.PasteMsg{Content: "GOOD"})
		loginToasted(t, seen, "Added claude account 14")
		if txt := loginText(m); !strings.Contains(txt, "claude issues no long-lived token today; n begins another") {
			t.Errorf("a long login that cannot begin is not said:\n%s", plain(m))
		}
	})
}

func TestAPasteFillsTheFieldOnceAndShowsNoKey(t *testing.T) {
	t.Run("code", func(t *testing.T) {
		m, b, _ := loginModel(t, nil)
		release := make(chan struct{})
		b.FinishLoginFn = func(ctx context.Context, id, code string) (LoginEnd, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return LoginEnd{Pending: true}, nil
		}
		loginPress(t, m, "n", "enter", "enter")
		f := loginFlowOf(t, m)
		run(t, m, tea.PasteMsg{Content: "  CODE\r\n"})
		s := loginCodeOf(t, f)
		if v := s.in.Value(); v != "CODE" || !s.Typing() {
			t.Errorf("the paste left %q in the field, focused %v", v, s.Typing())
		}
		// Further presses and pastes wait for the answer.
		loginPress(t, m, "enter", "enter")
		run(t, m, tea.PasteMsg{Content: "OTHER"})
		close(release)
		await(t, m)
		if got := loginFinishes(b); !slices.Equal(got, [][2]string{{"abc123", "CODE"}}) {
			t.Errorf("sent %q, not CODE once", got)
		}
		if txt := loginText(m); !strings.Contains(txt, "code CODE 15m left Not approved yet; approve in the browser, then give it again.") {
			t.Errorf("pending is not said beside the field:\n%s", plain(m))
		}

		// A key or a token pasted into the code field is no code: it stays in
		// the field, drawn as stars, and is never sent to the provider's
		// token endpoint, by the paste or by enter. That holds wherever the
		// key stands in what was pasted, as an environment line pastes it.
		refused := func(what string, seen []tea.Msg) {
			t.Helper()
			if got := loginFinishes(b); len(got) != 1 {
				t.Errorf("%s was sent as a code: %q", what, got)
			}
			p := plain(m)
			if strings.Contains(p, "TESTONLY") || !strings.Contains(loginText(m), loginNoCode) {
				t.Errorf("%s is drawn, or not said to be no code:\n%s", what, p)
			}
			for _, s := range loginToasts(seen) {
				if strings.Contains(s, "TESTONLY") {
					t.Errorf("a toast holds %s: %q", what, s)
				}
			}
		}
		for i, v := range []string{backendHTTPKey, backendHTTPSetup, "sk-proj-TESTONLY-0000000000", "xai-TESTONLY-key-00000000",
			"ANTHROPIC_API_KEY=" + backendHTTPKey, "OPENAI_API_KEY=sk-proj-TESTONLY-0000000000",
			`export XAI_API_KEY="xai-TESTONLY-key-00000000"`} {
			_, seen := run(t, m, tea.PasteMsg{Content: "\n" + v + "\r\n"})
			seen = append(seen, loginPress(t, m, "enter")...)
			if s.in.Value() != v {
				t.Errorf("the field does not hold what was pasted")
			}
			refused(fmt.Sprintf("paste %d", i), seen)
		}
		// Nor is a key typed by hand after a space, which is sent trimmed.
		seen := loginPress(t, m, "ctrl+u", "space", "s", "k", "-", "p", "r", "o", "j", "-", "T", "E", "S", "T", "O", "N", "L", "Y")
		if s.in.Value() != " sk-proj-TESTONLY" {
			t.Fatalf("typed %q", s.in.Value())
		}
		if strings.Contains(plain(m), "TESTONLY") {
			t.Errorf("a key typed after a space is drawn:\n%s", plain(m))
		}
		seen = append(seen, loginPress(t, m, "enter")...)
		refused("a typed key", seen)
		// Nor anything clean would change: a bidi override would reorder the
		// line, and no code holds one.
		_, seen = run(t, m, tea.PasteMsg{Content: "CO‮DE"})
		refused("a bidi override", seen)
		// A code is still sent from the same field.
		run(t, m, tea.PasteMsg{Content: "CODE2"})
		if got := loginFinishes(b); len(got) != 2 || got[1][1] != "CODE2" {
			t.Errorf("sent %q", got)
		}
	})

	t.Run("key", func(t *testing.T) {
		m, b, _ := loginModel(t, loginRows())
		var tries atomic.Int32
		b.PasteKeyFn = func(_ context.Context, k, label string) (LoginEnd, error) {
			if tries.Add(1) == 1 {
				return LoginEnd{}, fmt.Errorf("the console refused %s just now", k)
			}
			return LoginEnd{Account: 9, Provider: "claude", Status: "updated"}, nil
		}
		loginSelect(t, m, 9)
		loginPress(t, m, "l")
		f := loginFlowOf(t, m)
		s := loginSecretOf(t, f)
		if txt := loginText(m); !strings.Contains(txt, "the same key again keeps this account; a different one makes an account of its own") {
			t.Errorf("l on a key account does not say what a key does:\n%s", plain(m))
		}
		var seen []tea.Msg
		_, got := run(t, m, tea.PasteMsg{Content: " " + backendHTTPKey + "\r\n"})
		seen = append(seen, got...)
		if c := b.named("PasteKey"); len(c) != 1 || c[0].Args[0] != backendHTTPKey || c[0].Args[1] != "" {
			t.Fatalf("pasted %v", c)
		}
		if s.value.Value() != backendHTTPKey {
			t.Error("a refused key is not kept in its field")
		}
		p := plain(m)
		if strings.Contains(p, "TESTONLY") || !strings.Contains(p, "the console refused [not repeated] just now") {
			t.Errorf("the key is drawn, or the refusal does not show it redacted:\n%s", p)
		}
		got = loginPress(t, m, "enter")
		seen = append(seen, got...)
		loginToasted(t, seen, "Updated claude account 9")
		for _, s := range loginToasts(seen) {
			if strings.Contains(s, "TESTONLY") {
				t.Errorf("a toast holds the key: %q", s)
			}
		}
		if strings.Contains(plain(m), "TESTONLY") {
			t.Error("the key is drawn after the flow")
		}
	})

	t.Run("token", func(t *testing.T) {
		m, b, _ := loginModel(t, loginRows())
		b.PasteTokenFn = func(_ context.Context, account int, token string) (LoginEnd, error) {
			return LoginEnd{Account: account, Provider: "claude", Status: "long", LongUntil: "2027-11-10T20:17:00Z",
				Warning: "rota cannot tell which claude.ai account a pasted token belongs to"}, nil
		}
		loginSelect(t, m, 12)
		// With no login parked there is nothing to put aside: esc closes.
		loginPress(t, m, "l", "down", "down", "enter", "esc")
		if loginOf(m) != nil || m.aside != nil {
			t.Fatal("esc on a step with no login parked does not close the flow")
		}
		loginPress(t, m, "l", "down", "down", "enter")
		loginSecretOf(t, loginFlowOf(t, m))
		if txt := loginText(m); !strings.Contains(txt, "it goes on account 12 as its long-lived token") {
			t.Errorf("the token step does not say where it goes:\n%s", plain(m))
		}
		_, seen := run(t, m, tea.PasteMsg{Content: backendHTTPSetup + "\n"})
		if c := b.named("PasteToken"); len(c) != 1 || c[0].Args[0] != 12 || c[0].Args[1] != backendHTTPSetup {
			t.Fatalf("pasted %v", c)
		}
		if n := len(b.named("BeginLogin")); n != 0 {
			t.Errorf("a setup token began a login (%d)", n)
		}
		loginToasted(t, seen, "Long-lived token stored for #12, until 2027-11-10")
		loginToasted(t, seen, "rota cannot tell which claude.ai account a pasted token belongs to")
		for _, s := range append(loginToasts(seen), plain(m)) {
			if strings.Contains(s, "TESTONLY") {
				t.Errorf("the token is drawn: %q", s)
			}
		}
	})

	t.Run("label", func(t *testing.T) {
		m, b, _ := loginModel(t, nil)
		b.PasteKeyFn = func(_ context.Context, k, label string) (LoginEnd, error) {
			return LoginEnd{Account: 15, Provider: "claude", Status: "added", Warning: "the key was taken unchecked"}, nil
		}
		loginPress(t, m, "n", "enter", "down", "down", "enter")
		s := loginSecretOf(t, loginFlowOf(t, m))
		// A paste fills the key and moves to the label, which enter sends.
		run(t, m, tea.PasteMsg{Content: backendHTTPKey + "\n"})
		if n := len(b.named("PasteKey")); n != 0 || !s.label.Focused() {
			t.Fatalf("a key with a label to type was sent at once (%d), label focused %v", n, s.label.Focused())
		}
		// A key pasted into the label by mistake is never drawn there.
		run(t, m, tea.PasteMsg{Content: backendHTTPSetup})
		if p := plain(m); s.label.Value() != backendHTTPSetup || strings.Contains(p, "TESTONLY") {
			t.Errorf("a key in the label is drawn:\n%s", p)
		}
		// The label takes what is typed, and a paste into it replaces that
		// and sends nothing.
		loginPress(t, m, "h", "o")
		run(t, m, tea.PasteMsg{Content: "  work\r\n"})
		if n := len(b.named("PasteKey")); n != 0 || s.label.Value() != "work" || s.value.Value() != backendHTTPKey {
			t.Fatalf("a paste into the label gave %q and sent %d", s.label.Value(), n)
		}
		seen := loginPress(t, m, "enter")
		if c := b.named("PasteKey"); len(c) != 1 || c[0].Args[0] != backendHTTPKey || c[0].Args[1] != "work" {
			t.Fatalf("pasted %v", c)
		}
		loginToasted(t, seen, "Added claude account 15")
		loginToasted(t, seen, "the key was taken unchecked")
		loginSaw(t, seen, selectMsg{Account: 15})
	})
}

func TestAProviderFailureKeepsTheField(t *testing.T) {
	m, b, _ := loginModel(t, nil)
	var calls atomic.Int32
	b.FinishLoginFn = func(_ context.Context, id, code string) (LoginEnd, error) {
		if calls.Add(1) == 1 {
			return LoginEnd{}, &client.Refusal{Status: 502, Message: "the provider answered 502 Bad Gateway"}
		}
		return LoginEnd{Account: 14, Provider: "claude", Status: "added"}, nil
	}
	loginPress(t, m, "n", "enter", "enter")
	f := loginFlowOf(t, m)
	run(t, m, tea.PasteMsg{Content: "CODE1"})
	if txt := loginText(m); !strings.Contains(txt, "code CODE1 15m left the provider answered 502 Bad Gateway") {
		t.Errorf("the failure is not beside the field:\n%s", plain(m))
	}
	if loginCodeOf(t, f).in.Value() != "CODE1" || f.Pending() == nil || f.Pending().ID != "abc123" {
		t.Errorf("the value or the login is gone: %q %+v", loginCodeOf(t, f).in.Value(), f.Pending())
	}
	_, seen := run(t, m, press("enter")...)
	if got := loginFinishes(b); !slices.Equal(got, [][2]string{{"abc123", "CODE1"}, {"abc123", "CODE1"}}) {
		t.Errorf("sent %q", got)
	}
	loginToasted(t, seen, "Added claude account 14")

	// A call that never answers is answered for at its deadline, and the
	// field takes enter again.
	loginPress(t, m, "n", "enter", "enter")
	f = loginFlowOf(t, m)
	b.FinishLoginFn = func(context.Context, string, string) (LoginEnd, error) { panic("the provider fell over") }
	run(t, m, tea.PasteMsg{Content: "CODE2"})
	if txt := loginText(m); !strings.Contains(txt, "code CODE2 15m left the call failed: the provider fell over") {
		t.Errorf("a call that failed is not said beside the field:\n%s", plain(m))
	}
	b.FinishLoginFn = func(context.Context, string, string) (LoginEnd, error) {
		return LoginEnd{Account: 15, Provider: "claude", Status: "added"}, nil
	}
	_, seen = run(t, m, press("enter")...)
	loginToasted(t, seen, "Added claude account 15")

	// A begin the server refuses is said under the pick, and enter tries
	// again.
	var begins atomic.Int32
	b.BeginLoginFn = func(_ context.Context, lb LoginBegin) (backendLogin, error) {
		if begins.Add(1) == 1 {
			return backendLogin{}, &client.Refusal{Status: 502, Message: "the provider answered 502 Bad Gateway"}
		}
		return backendLogin{ID: "abc124", Provider: "claude", URL: loginURL, Kind: "code", CreatedAt: loginNow.UnixMilli()}, nil
	}
	loginPress(t, m, "n", "enter", "enter")
	if txt := loginText(m); !strings.Contains(txt, "> short") || !strings.Contains(txt, "the provider answered 502 Bad Gateway") {
		t.Errorf("a refused begin is not said under the pick:\n%s", plain(m))
	}
	loginPress(t, m, "enter")
	if f := loginFlowOf(t, m); f.Pending() == nil || f.Pending().ID != "abc124" {
		t.Errorf("enter did not begin again: %+v", f.Pending())
	}
}

func TestUnfinishableAndExpiredEndTheFlow(t *testing.T) {
	const gone = "claude account 12, which this login was begun for, is no longer here; nothing was stored, and `rota login claude` starts a login of its own"
	for _, c := range []struct {
		name string
		no   *client.Refusal
	}{
		{"unfinishable", &client.Refusal{Status: 400, Reason: "unfinishable", Message: gone}},
		{"404", &client.Refusal{Status: 404, Message: `no pending login "abc123" (expired?)`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, b, _ := loginModel(t, loginRows())
			b.FinishLoginFn = func(context.Context, string, string) (LoginEnd, error) { return LoginEnd{}, c.no }
			loginSelect(t, m, 12)
			loginPress(t, m, "l", "enter")
			run(t, m, tea.PasteMsg{Content: "CODE"})
			f := loginFlowOf(t, m)
			if txt := loginText(m); !strings.Contains(txt, c.no.Message+"; n begins another") {
				t.Errorf("the flow does not end with the server's words:\n%s", plain(m))
			}
			if f.Pending() != nil || f.Typing() || f.step != nil {
				t.Errorf("an ended flow still holds its login or its field: %+v", f.Pending())
			}
			// n begins another, from the start.
			loginPress(t, m, "n")
			if txt := loginText(m); !strings.Contains(txt, "> short") {
				t.Errorf("n does not begin another:\n%s", plain(m))
			}
			loginPress(t, m, "enter")
			if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Account: 12}, {Account: 12}}) {
				t.Errorf("begun %+v", got)
			}
		})
	}

	t.Run("expired", func(t *testing.T) {
		m, _, ticks := loginModel(t, nil)
		loginPress(t, m, "n", "enter", "enter")
		f := loginFlowOf(t, m)
		run(t, m, ticks.advance(14*time.Minute)...)
		// The last minute is counted down by the second, so the screen
		// keeps up with it.
		for range 3 {
			due := ticks.advance(time.Second)
			if !slices.ContainsFunc(due, func(msg tea.Msg) bool { _, ok := msg.(loginTickMsg); return ok }) {
				t.Fatalf("no tick in the last minute's second: %#v", due)
			}
			run(t, m, due...)
		}
		run(t, m, ticks.advance(56*time.Second)...)
		if txt := loginText(m); !strings.Contains(txt, "code 1s left") {
			t.Errorf("the countdown is not kept:\n%s", plain(m))
		}
		run(t, m, ticks.advance(time.Second)...)
		if txt := loginText(m); !strings.Contains(txt, "the login abc123 has expired; n begins another") {
			t.Errorf("the flow does not end when its login runs out:\n%s", plain(m))
		}
		if f.Pending() != nil {
			t.Error("an expired login is still pending")
		}
	})

	t.Run("counted from when it began", func(t *testing.T) {
		m, b, ticks := loginModel(t, nil)
		b.BeginLoginFn = func(context.Context, LoginBegin) (backendLogin, error) {
			return backendLogin{ID: "abc123", Provider: "claude", URL: loginURL, Kind: "code",
				CreatedAt: loginNow.Add(-5 * time.Minute).UnixMilli()}, nil
		}
		loginPress(t, m, "n", "enter", "enter")
		if txt := loginText(m); !strings.Contains(txt, "code 10m left") {
			t.Errorf("the countdown is not counted from when the login began:\n%s", plain(m))
		}
		loginPress(t, m, "esc")
		if p := plain(m); !strings.Contains(p, "login abc123 · 10m left · n returns") {
			t.Errorf("the badge is not counted from when the login began:\n%s", p)
		}
		run(t, m, ticks.advance(10*time.Minute)...)
		if txt := loginText(m); !strings.Contains(txt, "the login abc123 has expired") {
			t.Errorf("the login does not end when the server lets it go:\n%s", plain(m))
		}
	})

	t.Run("expired aside", func(t *testing.T) {
		m, _, ticks := loginModel(t, nil)
		loginPress(t, m, "n", "enter", "enter", "esc")
		if m.aside == nil {
			t.Fatal("esc did not put the flow aside")
		}
		_, seen := run(t, m, ticks.advance(15*time.Minute)...)
		if m.aside != nil || loginOf(m) != nil {
			t.Error("an expired flow is still kept aside")
		}
		loginToasted(t, seen, "the login abc123 has expired; n begins another")
	})
}

func TestTheFlowGoesAsideAndComesBack(t *testing.T) {
	m, b, _ := loginModel(t, loginRows())
	loginPress(t, m, "n", "enter", "enter", "enter", "A", "B", "esc", "esc")
	f := loginFlowOf(t, m)
	if m.aside != asider(f) || !f.Aside() {
		t.Fatal("esc with the field blurred did not put the flow aside")
	}
	if p := plain(m); !strings.Contains(p, "login abc123 · 15m left · n returns") || strings.Contains(p, "approve access") {
		t.Errorf("the footer has no badge, or the flow still shows:\n%s", p)
	}
	if r := m.result(); r.Pending == nil || r.Pending.ID != "abc123" {
		t.Errorf("the parked login is not what the TUI leaves: %+v", r.Pending)
	}
	// Keys go to the tab meanwhile.
	loginPress(t, m, "down")
	if accountsOf(m).sel != 13 {
		t.Error("keys do not reach the tab while the flow is aside")
	}

	loginPress(t, m, "n")
	if loginOf(m) != f || m.aside != nil || f.Aside() || loginCodeOf(t, f).in.Value() != "AB" {
		t.Fatalf("n does not bring the same flow back with its field")
	}
	if n := len(b.named("BeginLogin")); n != 1 {
		t.Errorf("coming back began another login (%d)", n)
	}
	loginPress(t, m, "esc")
	loginPress(t, m, "l")
	if loginOf(m) != f || m.aside != nil {
		t.Fatal("l does not bring the flow back")
	}

	seen := loginPress(t, m, "x")
	if loginOf(m) != nil || m.aside != nil || len(m.stack) != 0 {
		t.Error("x does not abandon the flow")
	}
	loginToasted(t, seen, "the login abc123 stays parked until it runs out, in 15m")
	if r := m.result(); r.Pending != nil {
		t.Errorf("an abandoned flow is still left behind: %+v", r.Pending)
	}
}

func TestAResizeKeepsTheFlowAndItsField(t *testing.T) {
	m, _, ticks := loginModel(t, nil)
	loginPress(t, m, "n", "enter", "enter", "enter", "A", "B", "1", "2")
	run(t, m, ticks.advance(3*time.Minute)...)
	f := loginFlowOf(t, m)
	before := loginText(m)
	if !strings.Contains(before, "code AB12 12m left") {
		t.Fatalf("the field or the countdown is not drawn:\n%s", plain(m))
	}
	run(t, m, size(50, 10))
	if p := plain(m); !strings.Contains(p, "rota needs a terminal") || strings.Contains(p, "AB12") {
		t.Errorf("the terminal too small still draws the flow:\n%s", p)
	}
	run(t, m, size(0, 0))
	run(t, m, size(80, 24))
	if loginOf(m) != f || !f.Typing() || loginCodeOf(t, f).in.Value() != "AB12" {
		t.Fatal("the flow, its focus or its field did not survive the resize")
	}
	if after := loginText(m); after != before {
		t.Errorf("the screen differs after the resize:\n%s\n---\n%s", before, after)
	}
	loginPress(t, m, "3")
	if v := loginCodeOf(t, f).in.Value(); v != "AB123" {
		t.Errorf("typing after the resize gives %q", v)
	}
}

func TestADelegatedAccountGoesToItsStep(t *testing.T) {
	// made records the delegated steps the flow opens, by account.
	var made []int
	recorded := func(m *Model) {
		accountsOf(m).do.login = func(e *env, again *wire.Account) asider {
			f := newLogin(e, again).(*loginFlow)
			f.steps.delegated = func(e *env, account int) step {
				made = append(made, account)
				return overlayNotYet()
			}
			return f
		}
	}

	t.Run("again", func(t *testing.T) {
		made = nil
		m, b, _ := loginModel(t, loginRows())
		recorded(m)
		b.BeginLoginFn = func(context.Context, LoginBegin) (backendLogin, error) {
			return backendLogin{}, &client.Refusal{Status: 400, Reason: "delegated",
				Message: "account 7 signs in through its own CLI, so rota has no sign-in to start for it"}
		}
		loginSelect(t, m, 7)
		loginPress(t, m, "l", "enter")
		if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Account: 7}}) {
			t.Fatalf("begun %+v", got)
		}
		if !slices.Equal(made, []int{7}) {
			t.Fatalf("the delegated step was made for %v", made)
		}
		if f := loginFlowOf(t, m); !strings.Contains(plain(m), "not yet") || f.Pending() != nil {
			t.Errorf("the flow does not show the delegated step:\n%s", plain(m))
		}
		// It ends as the step says. The step asks for the listing itself, so
		// the flow does not ask again.
		_, seen := run(t, m, stepDone{End: LoginEnd{Account: 7, Status: "refreshed"}})
		loginToasted(t, seen, "Refreshed grok account 7")
		loginSaw(t, seen, selectMsg{Account: 7})
		if slices.Contains(seen, tea.Msg(refreshMsg{How: RefreshStale})) {
			t.Error("the flow asks for the listing the delegated step already asks for")
		}
		if loginOf(m) != nil {
			t.Error("the flow is still open")
		}
	})

	t.Run("signs itself in", func(t *testing.T) {
		made = nil
		m, b, _ := loginModel(t, loginRows())
		recorded(m)
		b.BeginLoginFn = func(context.Context, LoginBegin) (backendLogin, error) {
			return backendLogin{ID: "def456", Provider: "grok", URL: "https://console.x.example/keys", Kind: "apikey",
				Delegated: true, CreatedAt: loginNow.UnixMilli()}, nil
		}
		b.FinishLoginFn = func(_ context.Context, id, code string) (LoginEnd, error) {
			if code == "" {
				return LoginEnd{Account: 16, Provider: "grok", Status: "added", Delegated: true, LoginCommand: "GROK_HOME=x grok login"}, nil
			}
			return LoginEnd{Account: 17, Provider: "grok", Status: "added"}, nil
		}
		loginPress(t, m, "n")
		for range slices.Index(wire.LoginProviders(), "grok") - slices.Index(wire.LoginProviders(), "claude") {
			loginPress(t, m, "down")
		}
		loginPress(t, m, "enter")
		if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Provider: "grok"}}) {
			t.Fatalf("begun %+v", got)
		}
		if txt := loginText(m); !strings.Contains(txt, "> API key") || !strings.Contains(txt, "let grok sign itself in") {
			t.Fatalf("no choice between a key and grok signing itself in:\n%s", plain(m))
		}
		_ = loginPress(t, m, "down")
		seen := loginPress(t, m, "enter")
		if got := loginFinishes(b); !slices.Equal(got, [][2]string{{"def456", ""}}) {
			t.Errorf("finished %q", got)
		}
		loginToasted(t, seen, "Added grok account 16")
		loginSaw(t, seen, selectMsg{Account: 16})
		// The account just registered is read in; the step's own end is
		// the step's to refresh.
		loginSaw(t, seen, refreshMsg{How: RefreshStale})
		if !slices.Equal(made, []int{16}) {
			t.Errorf("the delegated step was made for %v", made)
		}
		_, seen = run(t, m, stepDone{End: LoginEnd{Account: 16, Status: "refreshed"}})
		loginToasted(t, seen, "Refreshed grok account 16")
		if slices.Contains(seen, tea.Msg(refreshMsg{How: RefreshStale})) || loginOf(m) != nil {
			t.Error("the delegated step's end is refreshed twice, or the flow stays open")
		}

		// The key instead: the secret step finishes the same kind of login.
		loginPress(t, m, "esc")
		loginPress(t, m, "n")
		for range slices.Index(wire.LoginProviders(), "grok") - slices.Index(wire.LoginProviders(), "claude") {
			loginPress(t, m, "down")
		}
		loginPress(t, m, "enter", "enter")
		loginSecretOf(t, loginFlowOf(t, m))
		if txt := loginText(m); !strings.Contains(txt, "https://console.x.example/keys") {
			t.Errorf("the key step does not say where keys are made:\n%s", plain(m))
		}
		seen = loginSettle(t, m, tea.PasteMsg{Content: "xai-TESTONLY-key-00000000"})
		if got := loginFinishes(b); len(got) != 2 || got[1] != [2]string{"def456", "xai-TESTONLY-key-00000000"} {
			t.Errorf("finished %q", got)
		}
		loginToasted(t, seen, "Added grok account 17")
	})

	t.Run("a key account of a provider that takes keys", func(t *testing.T) {
		made = nil
		m, b, _ := loginModel(t, loginRows())
		recorded(m)
		b.BeginLoginFn = func(_ context.Context, lb LoginBegin) (backendLogin, error) {
			if lb.Account != 0 {
				return backendLogin{}, &client.Refusal{Status: 400, Message: "account 7 runs on a grok key, so it has no sign-in to start; `rota login grok` takes a key"}
			}
			return backendLogin{ID: "fed789", Provider: "grok", URL: "https://console.x.example/keys", Kind: "apikey",
				Delegated: true, CreatedAt: loginNow.UnixMilli()}, nil
		}
		var finishes atomic.Int32
		b.FinishLoginFn = func(context.Context, string, string) (LoginEnd, error) {
			if finishes.Add(1) == 1 {
				return LoginEnd{}, &client.Refusal{Status: 400, Message: "xAI did not take that key"}
			}
			return LoginEnd{Account: 7, Provider: "grok", Status: "refreshed"}, nil
		}
		loginSelect(t, m, 7)
		loginPress(t, m, "l", "enter")
		if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Account: 7}, {Provider: "grok"}}) {
			t.Fatalf("begun %+v", got)
		}
		s := loginSecretOf(t, loginFlowOf(t, m))
		if txt := loginText(m); !strings.Contains(txt, "the same key again keeps this account") || strings.Contains(txt, "sign itself in") {
			t.Errorf("the key step for a key account says the wrong things:\n%s", plain(m))
		}
		// A key of no shape clean knows is still never drawn: the field
		// draws stars.
		loginSettle(t, m, tea.PasteMsg{Content: "xai-TESTONLY-key-00000000"})
		if p := plain(m); s.value.Value() != "xai-TESTONLY-key-00000000" || strings.Contains(p, "TESTONLY") ||
			!strings.Contains(p, "xAI did not take that key") {
			t.Errorf("the refused key is not kept, or is drawn:\n%s", p)
		}
		seen := loginSettle(t, m, press("enter")...)
		loginToasted(t, seen, "Refreshed grok account 7")
		if len(made) != 0 {
			t.Errorf("a key account was sent to its CLI: %v", made)
		}
	})

	t.Run("a refusal of a provider whose login takes no key", func(t *testing.T) {
		made = nil
		m, b, _ := loginModel(t, loginRows())
		recorded(m)
		const no = "rota knows account 6 by no uuid or e-mail, so no approval could be matched to it"
		b.BeginLoginFn = func(_ context.Context, lb LoginBegin) (backendLogin, error) {
			if lb.Account != 0 {
				return backendLogin{}, &client.Refusal{Status: 400, Message: no}
			}
			return backendLogin{ID: "cdx123", Provider: "codex", URL: loginURL, Kind: "code", CreatedAt: loginNow.UnixMilli()}, nil
		}
		loginSelect(t, m, 6)
		loginPress(t, m, "l", "enter")
		f := loginFlowOf(t, m)
		if txt := loginText(m); !strings.Contains(txt, no+"; n begins another") || f.Pending() != nil || f.step != nil {
			t.Errorf("the first refusal does not stand:\n%s", plain(m))
		}
	})
}

func TestNeverDrawsTheLoginState(t *testing.T) {
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, ""))
	shellRegistry(t)
	register(tabsAccounts, newAccounts)
	ticks := &fakeTicks{clock: time.Now()}
	m := shellModel(t, r.b, ticks)
	loginSettle(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	var seen []tea.Msg
	for _, k := range []string{"n", "enter", "enter"} {
		seen = append(seen, loginSettle(t, m, press(k)...)...)
	}
	f := loginFlowOf(t, m)
	l := f.Pending()
	if l == nil {
		t.Fatalf("no login was begun:\n%s", plain(m))
	}
	st, err := store.Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	parked, err := st.PendingLogin(l.ID)
	st.Close()
	if err != nil || parked == nil || len(parked.State) == 0 {
		t.Fatalf("the login is not parked with its state: %+v %v", parked, err)
	}
	var screens []string
	screens = append(screens, plain(m))
	seen = append(seen, loginSettle(t, m, tea.PasteMsg{Content: "WRONG"})...)
	screens = append(screens, plain(m))
	seen = append(seen, loginSettle(t, m, press("esc")...)...)
	seen = append(seen, loginSettle(t, m, press("esc")...)...)
	screens = append(screens, plain(m))
	seen = append(seen, loginSettle(t, m, press("n")...)...)
	screens = append(screens, plain(m), accountsPlain(f.View(80, 22)))
	for k, v := range parked.State {
		// The URL carries the state a provider hands back (OAuth's own
		// "state"), which is no secret; everything else stays parked.
		if strings.Contains(parked.URL, v) {
			continue
		}
		for _, s := range append(screens, loginToasts(seen)...) {
			if strings.Contains(s, v) {
				t.Errorf("the login's %s is drawn:\n%s", k, s)
			}
		}
	}
	if _, ok := parked.State["verifier"]; !ok {
		t.Fatalf("no verifier to look for: %v", parked.State)
	}
}

func TestTheFlowOverTheRealAPI(t *testing.T) {
	approvals(t, map[string][2]string{"AS13": {"u13", "b@x"}, "AS12": {"u12", "a@x"}})
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, `{"accounts":[
	 {"id":12,"provider":"claude","email":"a@x","uuid":"u12","order":1,"token":{"accessToken":"tok-12"}},
	 {"id":13,"provider":"claude","email":"b@x","uuid":"u13","order":2,"token":{"accessToken":"tok-13"}}]}`))
	shellRegistry(t)
	register(tabsAccounts, newAccounts)
	m := shellModel(t, r.b, &fakeTicks{clock: time.Now()})
	loginSettle(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	loginSettle(t, m, selectMsg{Account: 12})
	loginSettle(t, m, press("l")...)
	loginSettle(t, m, press("enter")...)
	if c := r.rec.last(t, "POST", "/v1/login"); c.Body != `{"account":12}` {
		t.Fatalf("sent %s", c.Body)
	}
	f := loginFlowOf(t, m)
	id := f.Pending().ID
	loginSettle(t, m, tea.PasteMsg{Content: "AS13"})
	if txt := loginText(m); !strings.Contains(txt, "code AS13") || !strings.Contains(txt, "which is account 13") {
		t.Fatalf("approving as another account is not said beside the field:\n%s", plain(m))
	}
	if loginOf(m) != f || f.Pending() == nil {
		t.Fatal("approving as another account ended the flow")
	}
	seen := loginSettle(t, m, tea.PasteMsg{Content: "AS12"})
	loginToasted(t, seen, "Refreshed claude account 12")
	loginSaw(t, seen, selectMsg{Account: 12})
	if loginOf(m) != nil {
		t.Error("the flow is still open")
	}
	if c := r.rec.last(t, "POST", "/v1/login/"+id); c.Body != `{"code":"AS12"}` {
		t.Errorf("sent %s %s", c.Path, c.Body)
	}
}

func TestTheFlowAt80x24(t *testing.T) {
	m, b, _ := loginModel(t, loginRows())
	b.FinishLoginFn = func(context.Context, string, string) (LoginEnd, error) {
		return LoginEnd{}, &client.Refusal{Status: 400, Message: loginWrong}
	}
	var screens []string
	loginPress(t, m, "n", "enter")
	screens = append(screens, accountsPlain(loginFlowOf(t, m).View(80, 22)))
	loginPress(t, m, "esc")
	loginSelect(t, m, 12)
	loginPress(t, m, "l")
	screens = append(screens, accountsPlain(loginFlowOf(t, m).View(80, 22)))
	loginPress(t, m, "enter")
	run(t, m, tea.PasteMsg{Content: "WRONG"})
	screens = append(screens, accountsPlain(loginFlowOf(t, m).View(80, 22)))
	loginPress(t, m, "esc")
	screens = append(screens, accountsPlain(loginFlowOf(t, m).View(80, 22)))
	for _, s := range screens {
		for i, l := range strings.Split(s, "\n") {
			if w := textWidth(l); w > 80 {
				t.Errorf("line %d is %d wide: %q", i, w, l)
			}
		}
		if n := strings.Count(s, "\n"); n > 22 {
			t.Errorf("%d lines in 22", n)
		}
	}
	golden(t, "login-80x24", strings.Join(screens, "----\n"))
}

func TestLoginUpdateDoesNoIO(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	m, b, _ := loginModel(t, loginRows())
	rows := loginRows()
	assertNoIO(t, b, func() {
		f := newLogin(m.e, nil).(*loginFlow)
		m.Update(openMsg{O: f})
		for _, k := range []string{"down", "up", "enter", "down", "enter"} {
			m.Update(press(k)[0])
			m.View()
		}
		l := backendLogin{ID: "abc123", Provider: "claude", URL: loginURL, Kind: "code", CreatedAt: loginNow.UnixMilli()}
		m.Update(loginBegunMsg{f: f, gen: f.gen, l: l})
		s := f.step
		msgs := []tea.Msg{tea.PasteMsg{Content: "CODE"}, loginAnswerMsg{to: s, err: errors.New("no")}, press("esc")[0],
			press("o")[0], press("c")[0], press("w")[0], loginPageMsg{from: s, ask: pageAsk{Want: loginpage.Code}},
			size(50, 10), size(80, 24), loginTickMsg{f: f, gen: f.gen}, press("esc")[0], press("n")[0],
			stepDone{End: LoginEnd{Account: 14, Status: "added"}}}
		for _, msg := range msgs {
			m.Update(msg)
			m.View()
		}
		g := newLogin(m.e, &rows[3]).(*loginFlow)
		m.Update(openMsg{O: g})
		for _, msg := range []tea.Msg{tea.PasteMsg{Content: backendHTTPKey}, press("enter")[0], press("tab")[0], press("esc")[0],
			press("x")[0], press("esc")[0]} {
			m.Update(msg)
			m.View()
		}
		h := newLogin(m.e, &rows[0]).(*loginFlow)
		m.Update(openMsg{O: h})
		for _, k := range []string{"down", "down", "enter", "esc", "l", "enter", "esc"} {
			m.Update(press(k)[0])
			m.View()
		}
	})
}

// loginFakeStep stands in for another task's step: it offers esc, records
// the keys it gets, says what it keeps running, and says when it is
// started and when it is left.
type loginFakeStep struct {
	name string
	got  []string
	left atomic.Bool
	// back, when set, is the step esc goes back to, as Task 17's page goes
	// back to the step it was opened from.
	back step
}

func (s *loginFakeStep) Update(msg tea.Msg) (step, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		s.got = append(s.got, k.String())
		if s.back != nil && k.String() == "esc" {
			return s.back, nil
		}
	}
	return s, nil
}
func (s *loginFakeStep) View(int) string { return s.name }
func (s *loginFakeStep) Keys() []key.Binding {
	return []key.Binding{key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close "+s.name))}
}
func (s *loginFakeStep) Typing() bool              { return false }
func (s *loginFakeStep) Busy() string              { return s.name }
func (s *loginFakeStep) Leave(ctx context.Context) { s.left.Store(true) }
func (s *loginFakeStep) Init() tea.Cmd {
	name := s.name
	return func() tea.Msg { return shellPing("started " + name) }
}

func TestAStepKeepsTheKeysItOffers(t *testing.T) {
	m, b, _ := loginModel(t, loginRows())
	var asked []pageAsk
	var devices []string
	var page, device *loginFakeStep
	accountsOf(m).do.login = func(e *env, again *wire.Account) asider {
		f := newLogin(e, again).(*loginFlow)
		f.steps.page = func(e *env, p pageAsk) step {
			asked = append(asked, p)
			page = &loginFakeStep{name: "the one-time page"}
			return page
		}
		f.steps.device = func(e *env, l *backendLogin, who string) step {
			devices = append(devices, l.ID+" as "+who)
			device = &loginFakeStep{name: "the device"}
			return device
		}
		return f
	}
	loginPress(t, m, "n", "enter", "enter")
	f := loginFlowOf(t, m)
	code := f.step
	seen := loginPress(t, m, "w")
	if len(asked) != 1 || asked[0].Want != loginpage.Code || asked[0].Login == nil || asked[0].Login.ID != "abc123" ||
		asked[0].Approve != loginURL || asked[0].Account != 0 {
		t.Fatalf("the page was asked for %+v", asked)
	}
	loginSaw(t, seen, shellPing("started the one-time page"))
	if f.step != step(page) || f.Busy() != "the one-time page" || !strings.Contains(plain(m), "the one-time page") {
		t.Fatalf("the page is not shown, or its work not said: %T %q", f.step, f.Busy())
	}
	// esc is the page's while it offers it: the flow is not put aside.
	loginPress(t, m, "esc")
	if !slices.Equal(page.got, []string{"esc"}) || f.Aside() {
		t.Errorf("esc went to the flow, not the page: %q aside %v", page.got, f.Aside())
	}
	// The page answers for itself, as the code step would have.
	_, seen = run(t, m, stepDone{End: LoginEnd{Account: 14, Provider: "claude", Status: "added"}})
	loginToasted(t, seen, "Added claude account 14")
	if code == step(page) || loginOf(m) != nil {
		t.Error("the flow did not end on the page's answer")
	}

	// x leaves what the step holds open.
	loginPress(t, m, "n", "enter", "enter", "w", "x")
	if !page.left.Load() || loginOf(m) != nil {
		t.Errorf("x did not leave the page (%v) or close the flow", page.left.Load())
	}
	// So does Run, through the flow.
	loginPress(t, m, "n", "enter", "enter", "w")
	f = loginFlowOf(t, m)
	f.Leave(context.Background())
	if !page.left.Load() {
		t.Error("the flow does not leave its step when Run ends")
	}
	loginPress(t, m, "x")

	// A device login is the device step's, started as it opens.
	b.BeginLoginFn = func(context.Context, LoginBegin) (backendLogin, error) {
		return backendLogin{ID: "dev123", Provider: "claude", URL: loginURL, Kind: "device", CreatedAt: loginNow.UnixMilli()}, nil
	}
	seen = loginPress(t, m, "n", "enter", "enter")
	if !slices.Equal(devices, []string{"dev123 as your claude account"}) || loginFlowOf(t, m).step != step(device) {
		t.Fatalf("the device step was made for %q", devices)
	}
	loginSaw(t, seen, shellPing("started the device"))
}

func TestAnAnswerForAFlowGoneGoesNowhere(t *testing.T) {
	m, b, _ := loginModel(t, nil)
	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var calls atomic.Int32
	b.BeginLoginFn = func(ctx context.Context, lb LoginBegin) (backendLogin, error) {
		n := calls.Add(1)
		select {
		case <-gates[n-1]:
		case <-ctx.Done():
		}
		return backendLogin{ID: fmt.Sprintf("new%03d", n), Provider: "claude", URL: loginURL, Kind: "code", CreatedAt: loginNow.UnixMilli()}, nil
	}
	loginPress(t, m, "n", "enter", "enter")
	first := loginFlowOf(t, m)
	loginPress(t, m, "esc")
	if loginOf(m) != nil {
		t.Fatal("esc while a login begins does not close the flow")
	}
	loginPress(t, m, "n", "enter", "enter")
	second := loginFlowOf(t, m)
	if second == first {
		t.Fatal("a closed flow came back")
	}
	close(gates[0])
	await(t, m)
	if second.stage != loginBeginning || second.login != nil {
		t.Fatalf("the closed flow's login reached the new one: %+v", second.login)
	}
	close(gates[1])
	await(t, m)
	if second.Pending() == nil || second.Pending().ID != "new002" {
		t.Fatalf("the new flow did not get its own login: %+v", second.Pending())
	}

	// A login that runs out while the answer to its code is awaited waits
	// for that answer, which may have finished it, and only then ends.
	for _, finished := range []bool{true, false} {
		m, b, ticks := loginModel(t, nil)
		hold := make(chan struct{})
		b.FinishLoginFn = func(ctx context.Context, id, code string) (LoginEnd, error) {
			select {
			case <-hold:
			case <-ctx.Done():
			}
			if finished {
				return LoginEnd{Account: 14, Provider: "claude", Status: "added"}, nil
			}
			return LoginEnd{}, errors.New("the provider gave up")
		}
		loginPress(t, m, "n", "enter", "enter")
		f := loginFlowOf(t, m)
		run(t, m, tea.PasteMsg{Content: "CODE"})
		run(t, m, ticks.advance(15*time.Minute)...)
		if loginOf(m) != f || strings.Contains(loginText(m), "has expired") {
			t.Fatalf("the flow ended while its answer was awaited:\n%s", plain(m))
		}
		close(hold)
		_, seen := await(t, m)
		if finished {
			loginToasted(t, seen, "Added claude account 14")
			continue
		}
		run(t, m, ticks.advance(time.Second)...)
		if txt := loginText(m); !strings.Contains(txt, "the login abc123 has expired; n begins another") {
			t.Errorf("the flow does not end once the answer came:\n%s", plain(m))
		}
	}

	// Nor does a tick of a flow abandoned move the new one: one countdown
	// runs, not two.
	m, _, ticks := loginModel(t, nil)
	loginPress(t, m, "n", "enter", "enter", "x", "n", "enter", "enter")
	run(t, m, ticks.advance(time.Minute)...)
	n := 0
	for _, msg := range ticks.advance(time.Minute) {
		if _, ok := msg.(loginTickMsg); ok {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d countdowns run for one flow", n)
	}
}

// loginOffers is the keys a flow offers, as the footer and the help draw
// them.
func loginOffers(f *loginFlow) []string {
	var out []string
	for _, b := range f.Keys() {
		out = append(out, b.Help().Key)
	}
	return out
}

// loginRecorded makes the Accounts tab's n and l open flows whose page is a
// fake, recording what each page was asked for.
func loginRecorded(m *Model, asked *[]pageAsk, pages *[]*loginFakeStep) {
	accountsOf(m).do.login = func(e *env, again *wire.Account) asider {
		f := newLogin(e, again).(*loginFlow)
		f.steps.page = func(e *env, p pageAsk) step {
			*asked = append(*asked, p)
			page := &loginFakeStep{name: "the one-time page"}
			*pages = append(*pages, page)
			return page
		}
		return f
	}
}

func TestAnAnswerAwaitedIsNeverLost(t *testing.T) {
	t.Run("a code", func(t *testing.T) {
		m, b, _ := loginModel(t, nil)
		var asked []pageAsk
		var pages []*loginFakeStep
		loginRecorded(m, &asked, &pages)
		hold := make(chan struct{})
		b.FinishLoginFn = func(ctx context.Context, id, code string) (LoginEnd, error) {
			select {
			case <-hold:
			case <-ctx.Done():
			}
			return LoginEnd{Account: 14, Provider: "claude", Status: "added"}, nil
		}
		loginPress(t, m, "n", "enter", "enter")
		f := loginFlowOf(t, m)
		code := f.step
		run(t, m, tea.PasteMsg{Content: "CODE"})
		loginPress(t, m, "esc")
		// While the answer is awaited neither x nor w is offered, nor taken.
		if got := loginOffers(f); slices.Contains(got, "x") || slices.Contains(got, "w") || !slices.Contains(got, "esc") {
			t.Errorf("offered %q while an answer is awaited", got)
		}
		seen := loginPress(t, m, "x", "w")
		if loginOf(m) != f || f.step != code || len(asked) != 0 {
			t.Fatalf("x or w left the step whose answer is awaited (pages %d)", len(asked))
		}
		for _, s := range loginToasts(seen) {
			if strings.Contains(s, "stays parked") {
				t.Errorf("a login being finished is said to stay parked: %q", s)
			}
		}
		close(hold)
		_, seen = await(t, m)
		loginToasted(t, seen, "Added claude account 14")
		loginSaw(t, seen, selectMsg{Account: 14})
		if loginOf(m) != nil {
			t.Error("the flow is still open after its answer")
		}
	})

	t.Run("a key", func(t *testing.T) {
		m, b, _ := loginModel(t, loginRows())
		var asked []pageAsk
		var pages []*loginFakeStep
		loginRecorded(m, &asked, &pages)
		hold := make(chan struct{})
		b.PasteKeyFn = func(ctx context.Context, k, label string) (LoginEnd, error) {
			select {
			case <-hold:
			case <-ctx.Done():
			}
			return LoginEnd{Account: 9, Provider: "claude", Status: "updated"}, nil
		}
		loginSelect(t, m, 9)
		loginPress(t, m, "l")
		f := loginFlowOf(t, m)
		run(t, m, tea.PasteMsg{Content: backendHTTPKey})
		loginPress(t, m, "esc")
		if got := loginOffers(f); slices.Contains(got, "esc") || slices.Contains(got, "w") {
			t.Errorf("offered %q while an answer is awaited", got)
		}
		loginPress(t, m, "esc", "w", "x")
		if loginOf(m) != f || len(asked) != 0 {
			t.Fatal("esc, w or x left the step whose answer is awaited")
		}
		close(hold)
		_, seen := await(t, m)
		loginToasted(t, seen, "Updated claude account 9")
		if loginOf(m) != nil {
			t.Error("the flow is still open after its answer")
		}
	})

	t.Run("aside", func(t *testing.T) {
		// Put aside meanwhile, the flow still hears its answer.
		m, b, _ := loginModel(t, nil)
		hold := make(chan struct{})
		b.FinishLoginFn = func(ctx context.Context, id, code string) (LoginEnd, error) {
			select {
			case <-hold:
			case <-ctx.Done():
			}
			return LoginEnd{Account: 14, Provider: "claude", Status: "added"}, nil
		}
		loginPress(t, m, "n", "enter", "enter")
		run(t, m, tea.PasteMsg{Content: "CODE"})
		loginPress(t, m, "esc", "esc")
		if m.aside == nil {
			t.Fatal("esc did not put the flow aside")
		}
		close(hold)
		_, seen := await(t, m)
		loginToasted(t, seen, "Added claude account 14")
		if m.aside != nil || loginOf(m) != nil {
			t.Error("the flow is still kept after its answer")
		}
	})
}

func TestASignInByItselfIsNotCancelledUnsaid(t *testing.T) {
	m, b, ticks := loginModel(t, loginRows())
	var made []int
	accountsOf(m).do.login = func(e *env, again *wire.Account) asider {
		f := newLogin(e, again).(*loginFlow)
		f.steps.delegated = func(e *env, account int) step { made = append(made, account); return overlayNotYet() }
		return f
	}
	b.BeginLoginFn = func(context.Context, LoginBegin) (backendLogin, error) {
		return backendLogin{ID: "def456", Provider: "grok", URL: "https://console.x.example/keys", Kind: "apikey",
			Delegated: true, CreatedAt: loginNow.UnixMilli()}, nil
	}
	hold := make(chan struct{})
	b.FinishLoginFn = func(ctx context.Context, id, code string) (LoginEnd, error) {
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return LoginEnd{Account: 16, Provider: "grok", Status: "added", Delegated: true}, nil
	}
	loginPress(t, m, "n")
	for range slices.Index(wire.LoginProviders(), "grok") - slices.Index(wire.LoginProviders(), "claude") {
		loginPress(t, m, "down")
	}
	loginPress(t, m, "enter")
	// The person takes their time over the pick: a second is left.
	run(t, m, ticks.advance(14*time.Minute+59*time.Second)...)
	loginPress(t, m, "down", "enter")
	f := loginFlowOf(t, m)
	// The server registers the account whatever the TUI does now: esc is
	// not offered, and does not hide what happens.
	if got := loginOffers(f); slices.Contains(got, "esc") {
		t.Errorf("esc is offered while the server signs the account in: %q", got)
	}
	loginPress(t, m, "esc")
	if loginOf(m) != f {
		t.Fatal("esc closed a flow whose account the server is registering")
	}
	// Nor does the login running out meanwhile end it: the finish was sent
	// in time, and its answer is what happened.
	run(t, m, ticks.advance(2*time.Second)...)
	if loginOf(m) != f || strings.Contains(loginText(m), "has expired") {
		t.Fatalf("the flow ended while the server signed the account in:\n%s", plain(m))
	}
	close(hold)
	_, seen := await(t, m)
	loginToasted(t, seen, "Added grok account 16")
	if !slices.Equal(made, []int{16}) {
		t.Errorf("the delegated step was made for %v", made)
	}
}

func TestAnAccountKnownByNothingStraysNoLogin(t *testing.T) {
	const no = "rota knows account 8 by no uuid or e-mail, so no approval could be matched to it"
	rows := append(loginRows(), wire.Account{ID: 8, Provider: "codex", Order: 6, Threshold: 75})
	m, b, _ := loginModel(t, rows)
	b.BeginLoginFn = func(context.Context, LoginBegin) (backendLogin, error) {
		return backendLogin{}, &client.Refusal{Status: 400, Message: no}
	}
	loginSelect(t, m, 8)
	loginPress(t, m, "l", "enter")
	if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Account: 8}}) {
		t.Errorf("begun %+v: a login of the provider's own was left parked", got)
	}
	if txt := loginText(m); !strings.Contains(txt, no+"; n begins another") {
		t.Errorf("the refusal does not end the flow:\n%s", plain(m))
	}

	// Nor does a claude account, or a login begun for nobody, fall back.
	m, b, _ = loginModel(t, loginRows())
	b.BeginLoginFn = func(context.Context, LoginBegin) (backendLogin, error) {
		return backendLogin{}, &client.Refusal{Status: 400, Message: "no"}
	}
	loginSelect(t, m, 12)
	loginPress(t, m, "l", "enter", "esc", "n")
	for range slices.Index(wire.LoginProviders(), "codex") - slices.Index(wire.LoginProviders(), "claude") {
		loginPress(t, m, "down")
	}
	loginPress(t, m, "enter")
	if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Account: 12}, {Provider: "codex"}}) {
		t.Errorf("begun %+v", got)
	}

	// An account known by its e-mail alone is known: its refusal is not
	// one for having no identity, and F40's path is tried.
	rows = append(loginRows(), wire.Account{ID: 10, Provider: "codex", Email: "e@x", Order: 7, Threshold: 75})
	m, b, _ = loginModel(t, rows)
	b.BeginLoginFn = func(_ context.Context, lb LoginBegin) (backendLogin, error) {
		if lb.Account != 0 {
			return backendLogin{}, &client.Refusal{Status: 400, Message: "no"}
		}
		return backendLogin{ID: "cdx123", Provider: "codex", URL: loginURL, Kind: "code", CreatedAt: loginNow.UnixMilli()}, nil
	}
	loginSelect(t, m, 10)
	loginPress(t, m, "l", "enter")
	if got := loginBegins(b); !slices.Equal(got, []LoginBegin{{Account: 10}, {Provider: "codex"}}) {
		t.Errorf("an account with an e-mail is taken for one known by nothing: begun %+v", got)
	}
}

func TestAStepIsLeftAndReplacedAsItSays(t *testing.T) {
	m, b, ticks := loginModel(t, loginRows())
	var asked []pageAsk
	var pages []*loginFakeStep
	loginRecorded(m, &asked, &pages)
	// A step that goes back gives the flow the step it came from, as it was.
	loginPress(t, m, "n", "enter", "enter", "enter", "A", "B", "esc")
	f := loginFlowOf(t, m)
	code := loginCodeOf(t, f)
	loginPress(t, m, "w")
	pages[0].back = code
	loginPress(t, m, "esc")
	if f.step != step(code) || code.in.Value() != "AB" {
		t.Fatalf("the step the page went back to is not shown: %T", f.step)
	}
	// The flow's end leaves what its step holds open.
	loginPress(t, m, "w")
	run(t, m, ticks.advance(15*time.Minute)...)
	if !pages[1].left.Load() || !strings.Contains(loginText(m), "has expired") {
		t.Error("the page is left open when the login runs out")
	}

	// What ends the flow is scrubbed of what was sent.
	b.FinishLoginFn = func(_ context.Context, id, code string) (LoginEnd, error) {
		return LoginEnd{}, &client.Refusal{Status: 400, Reason: "unfinishable", Message: "the code " + code + " was approved by another id"}
	}
	loginPress(t, m, "esc", "n", "enter", "enter")
	run(t, m, tea.PasteMsg{Content: "CODE-ABCDEFGH"})
	if txt := loginText(m); strings.Contains(txt, "ABCDEFGH") || !strings.Contains(txt, "the code [not repeated] was approved by another id; n begins another") {
		t.Errorf("the end repeats what was sent:\n%s", plain(m))
	}

	// x where no login is parked is no key of the flow's.
	loginPress(t, m, "esc")
	loginSelect(t, m, 9)
	loginPress(t, m, "l")
	g := loginFlowOf(t, m)
	if got := loginOffers(g); slices.Contains(got, "x") {
		t.Errorf("x is offered with no login parked: %q", got)
	}
	loginPress(t, m, "x")
	if loginOf(m) != g || g.Closed() {
		t.Error("x closed a flow with no login parked")
	}
}

func TestAPasteIsTakenWhole(t *testing.T) {
	m, b, _ := loginModel(t, nil)
	b.FinishLoginFn = func(context.Context, string, string) (LoginEnd, error) {
		return LoginEnd{}, &client.Refusal{Status: 400, Message: loginWrong}
	}
	loginPress(t, m, "n", "enter", "enter")
	s := loginCodeOf(t, loginFlowOf(t, m))
	// Line breaks inside go too, not only around it.
	run(t, m, tea.PasteMsg{Content: "  CO\r\nDE\n"})
	if got := loginFinishes(b); len(got) != 1 || got[0][1] != "CODE" {
		t.Errorf("sent %q", got)
	}
	// What was pasted replaces the field, and typing goes on after it.
	loginPress(t, m, "home")
	run(t, m, tea.PasteMsg{Content: "XYZ"})
	loginPress(t, m, "9")
	if v := s.in.Value(); v != "XYZ9" {
		t.Errorf("typing after a paste gives %q", v)
	}
	// A code typed with a space after it is a code, sent trimmed.
	loginPress(t, m, "ctrl+u", "C", "O", "D", "E", "space", "enter")
	if got := loginFinishes(b); len(got) != 3 || got[2][1] != "CODE" {
		t.Errorf("sent %q", got)
	}
}

func TestTheCountdownKeepsItsPhase(t *testing.T) {
	m, b, ticks := loginModel(t, nil)
	b.BeginLoginFn = func(context.Context, LoginBegin) (backendLogin, error) {
		return backendLogin{ID: "abc123", Provider: "claude", URL: loginURL, Kind: "code",
			CreatedAt: loginNow.Add(-30500 * time.Millisecond).UnixMilli()}, nil
	}
	loginPress(t, m, "n", "enter", "enter")
	if txt := loginText(m); !strings.Contains(txt, "code 15m left") {
		t.Fatalf("14m29.5s left reads otherwise:\n%s", plain(m))
	}
	tick := func(msgs []tea.Msg) bool {
		return slices.ContainsFunc(msgs, func(msg tea.Msg) bool { _, ok := msg.(loginTickMsg); return ok })
	}
	if due := ticks.advance(29 * time.Second); tick(due) {
		t.Errorf("the countdown woke before it read otherwise")
	} else {
		run(t, m, due...)
	}
	due := ticks.advance(500 * time.Millisecond)
	if !tick(due) {
		t.Fatalf("the countdown did not wake when it reads otherwise")
	}
	run(t, m, due...)
	if txt := loginText(m); !strings.Contains(txt, "code 14m left") {
		t.Errorf("the countdown reads:\n%s", plain(m))
	}
}
