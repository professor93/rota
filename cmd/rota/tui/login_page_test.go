package tui

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/cmd/rota/loginpage"
	"github.com/professor93/rota/internal/logins"
	"github.com/professor93/rota/wire"
)

// No test here takes the page's default port: an address with none gets
// the kernel's.
func init() { loginPagePort = 0 }

// loginPageClock is the TUI's clock in these tests.
var loginPageClock = time.Date(2026, 11, 10, 20, 17, 0, 0, time.UTC)

// loginPageLo is what pageChoices offers first on any machine.
var loginPageLo = loginpage.Choice{Addr: "127.0.0.1", Iface: "lo", Who: "only this machine; from another one, through ssh -L"}

// loginPageHost stands in for the login flow (Task 15): it shows the step it
// holds, takes the step each Update returns, as the flow does, and keeps
// every stepDone instead of closing.
type loginPageHost struct {
	st   step
	done []stepDone
}

func (h *loginPageHost) Update(msg tea.Msg) (overlay, tea.Cmd) {
	if d, ok := msg.(stepDone); ok {
		h.done = append(h.done, d)
		return h, nil
	}
	var cmd tea.Cmd
	h.st, cmd = h.st.Update(msg)
	return h, cmd
}

func (h *loginPageHost) View(width, _ int) string { return h.st.View(width) }
func (h *loginPageHost) Keys() []key.Binding      { return h.st.Keys() }
func (h *loginPageHost) Typing() bool             { return h.st.Typing() }
func (h *loginPageHost) Closed() bool             { return false }

// loginPageField is the step the page is opened from: a code field holding
// what was typed so far, which comes back as it was.
type loginPageField struct{ value string }

func (f *loginPageField) Update(tea.Msg) (step, tea.Cmd) { return f, nil }
func (f *loginPageField) View(int) string                { return "code field: " + f.value }
func (f *loginPageField) Keys() []key.Binding            { return nil }
func (f *loginPageField) Typing() bool                   { return false }

// loginPageOffer makes pageChoices answer choices, or err.
func loginPageOffer(t *testing.T, err error, choices ...loginpage.Choice) {
	t.Helper()
	saved := pageChoices
	pageChoices = func() ([]loginpage.Choice, error) { return slices.Clone(choices), err }
	t.Cleanup(func() { pageChoices = saved })
}

// loginPageReal serves real pages, on loopback in these tests, and closes
// any a failing test left up.
func loginPageReal(t *testing.T) {
	t.Helper()
	var mu sync.Mutex
	var pages []*loginpage.Page
	saved := loginPageServe
	loginPageServe = func(ctx context.Context, o loginpage.Options) (*loginpage.Page, error) {
		p, err := loginpage.Serve(ctx, o)
		if p != nil {
			mu.Lock()
			pages = append(pages, p)
			mu.Unlock()
		}
		return p, err
	}
	t.Cleanup(func() {
		loginPageServe = saved
		mu.Lock()
		defer mu.Unlock()
		for _, p := range pages {
			p.Close()
		}
	})
}

// loginPageFake stands in for loginpage.Serve: it keeps the options each
// page was asked with and listens nowhere. Its page ends when the test ends
// it, or as Stopped when it is closed.
type loginPageFake struct {
	mu      sync.Mutex
	opts    []loginpage.Options
	url, fp string
	err     error
	done    chan loginpage.End
}

func loginPageFaked(t *testing.T, url, fp string) *loginPageFake {
	t.Helper()
	f := &loginPageFake{url: url, fp: fp, done: make(chan loginpage.End, 1)}
	saved := loginPageServe
	loginPageServe = func(ctx context.Context, o loginpage.Options) (*loginpage.Page, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.opts = append(f.opts, o)
		if f.err != nil {
			return nil, f.err
		}
		return &loginpage.Page{URL: f.url, Fingerprint: f.fp, Done: f.done,
			Close: func() { f.end(loginpage.End{Why: loginpage.Stopped}) }}, nil
	}
	t.Cleanup(func() { loginPageServe = saved })
	return f
}

// end ends the page as e, once.
func (f *loginPageFake) end(e loginpage.End) {
	select {
	case f.done <- e:
	default:
	}
}

func (f *loginPageFake) asked() []loginpage.Options {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.opts)
}

// loginPageURL is where the login in these tests is approved.
const loginPageURL = "https://claude.ai/oauth/authorize?code=true"

// loginPageCode is the page asked from a code step for a login bound to
// account 12, with back under it, as the flow asks it: Approve is the URL.
func loginPageCode(back step) pageAsk {
	return pageAsk{
		Want:    loginpage.Code,
		Login:   &backendLogin{ID: "abc123", Provider: "claude", URL: loginPageURL, Kind: "code", Account: 12, CreatedAt: loginPageClock.UnixMilli()},
		Approve: loginPageURL,
		Account: 12,
		Back:    back,
	}
}

// loginPageSetup is the shell over b with one tab, and the page step asked
// for, held by a host overlay.
func loginPageSetup(t *testing.T, b *fakeBackend, ask pageAsk, with ...func(*Options)) (*Model, *loginPageHost, *fakeTicks) {
	t.Helper()
	shellTabs(t, 10)
	ticks := &fakeTicks{clock: loginPageClock}
	m := shellModel(t, b, ticks, append([]func(*Options){func(o *Options) { o.Host = "box" }}, with...)...)
	shellStart(t, m)
	host := &loginPageHost{st: newPageStep(m.e, ask)}
	run(t, m, openMsg{O: host})
	if len(m.stack) == 0 || m.stack[len(m.stack)-1] != overlay(host) {
		t.Fatal("the host overlay did not open")
	}
	return m, host, ticks
}

// loginPageOf is the page step the host shows.
func loginPageOf(t *testing.T, h *loginPageHost) *loginPageStep {
	t.Helper()
	s, ok := h.st.(*loginPageStep)
	if !ok {
		t.Fatalf("the host shows %T, not the page step", h.st)
	}
	return s
}

// loginPagePost sends value on the page's form, as a browser does, and
// returns the status and the page it answered with.
func loginPagePost(t *testing.T, link, value string) (int, string) {
	t.Helper()
	code, page, err := loginPageSend(link, value)
	if err != nil {
		t.Fatalf("POST to the page: %v", err)
	}
	return code, page
}

// loginPageSend is loginPagePost for a goroutine of the test's own.
func loginPageSend(link, value string) (int, string, error) {
	tr := &http.Transport{Proxy: nil}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := c.PostForm(link, url.Values{"value": {value}})
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), err
}

// loginPageDrain runs m until no command is left running: a page that is
// ending says so through a command that waited for it, and another, closing
// it, may still be running beside it.
func loginPageDrain(t *testing.T, m tea.Model, msgs ...tea.Msg) []tea.Msg {
	t.Helper()
	_, seen := run(t, m, msgs...)
	for len(harnessDriverOf(t).leftover()) > 0 {
		_, more := await(t, m)
		seen = append(seen, more...)
	}
	return seen
}

// loginPageGone fails the test unless nothing listens where link pointed.
func loginPageGone(t *testing.T, link string) {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err == nil {
		c.Close()
		t.Errorf("something still listens on %s", u.Host)
	}
}

// loginPageToasts is the text of every toast among msgs.
func loginPageToasts(msgs []tea.Msg) []string {
	var out []string
	for _, m := range msgs {
		if t, ok := m.(toastMsg); ok {
			out = append(out, t.Text)
		}
	}
	return out
}

// loginPageFlat is a view with its lines joined and every run of spaces one
// space, so a sentence wrapped over lines, under an indent, reads whole.
func loginPageFlat(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func TestThePageFinishesTheFlow(t *testing.T) {
	loginPageReal(t)
	loginPageOffer(t, nil, loginPageLo, loginpage.Choice{Addr: "0.0.0.0", Who: "every interface", Warn: true})
	b := &fakeBackend{FinishLoginFn: func(ctx context.Context, id, code string) (LoginEnd, error) {
		if code == "GOOD" {
			return LoginEnd{Account: 14, Provider: "claude", Status: "added"}, nil
		}
		return LoginEnd{}, errors.New("that code was not accepted")
	}}
	back := &loginPageField{value: "AB"}
	m, host, _ := loginPageSetup(t, b, loginPageCode(back))

	if s := loginPageOf(t, host); s.Busy() != "" || s.Typing() {
		t.Errorf("before serving: Busy %q, Typing %v", s.Busy(), s.Typing())
	}
	run(t, m, press("enter")...)
	s := loginPageOf(t, host)
	if s.page == nil {
		t.Fatalf("enter served no page; the step says:\n%s", s.View(80))
	}
	link := s.page.URL
	if !strings.HasPrefix(link, "http://127.0.0.1:") {
		t.Errorf("the page is at %q, not on 127.0.0.1", link)
	}
	if !strings.Contains(loginPageFlat(plain(m)), link) {
		t.Errorf("the screen does not show the link %s:\n%s", link, plain(m))
	}
	if s.Busy() != "the one-time page" {
		t.Errorf("while serving, Busy is %q", s.Busy())
	}

	code, page := loginPagePost(t, link, "GOOD")
	if code != http.StatusOK || !strings.Contains(page, "Signed in.") {
		t.Errorf("the page answered %d:\n%s", code, page)
	}
	seen := loginPageDrain(t, m)
	if len(host.done) != 1 || host.done[0].Err != nil || host.done[0].End.Account != 14 {
		t.Fatalf("stepDone is %+v, want one with Account 14 and no error (seen %v)", host.done, seen)
	}
	if got := b.named("FinishLogin"); len(got) != 1 || got[0].Args[0] != "abc123" || got[0].Args[1] != "GOOD" {
		t.Errorf("FinishLogin took %v, want one call for abc123 with GOOD", got)
	}
	if s.Busy() != "" {
		t.Errorf("after the page was used, Busy is %q", s.Busy())
	}
	loginPageGone(t, link)
}

func TestTheChoicesSayWhoCanReachThem(t *testing.T) {
	f := loginPageFaked(t, "https://192.168.1.20:8788/SECRET", "AA:BB")
	choices := []loginpage.Choice{
		loginPageLo,
		{Addr: "192.168.1.20", Iface: "eth0", Who: "machines on the local network of eth0"},
		{Addr: "203.0.113.7", Iface: "eth1", Who: "anyone on the internet who can reach eth1", Warn: true},
		{Addr: "0.0.0.0", Who: "every interface: anyone who can reach this machine on any of them", Warn: true},
	}
	loginPageOffer(t, nil, choices...)
	colour := func(o *Options) { p := colorprofile.TrueColor; o.Profile = &p }
	b := &fakeBackend{AccountsFn: func(context.Context, Refresh) (Listing, error) {
		return Listing{Accounts: []wire.Account{{ID: 12, Provider: "claude", Email: "me@example.com"}, {ID: 13, Provider: "claude", Name: "home"}}}, nil
	}}
	m, host, _ := loginPageSetup(t, b, loginPageCode(&loginPageField{}), colour)

	s := loginPageOf(t, host)
	raw := s.View(120)
	text := plain(m)
	warn := strings.SplitN(m.e.th.Warn.Render("x"), "x", 2)[0]
	if warn == "" {
		t.Fatal("the Warn style sets nothing at TrueColor")
	}
	lines := strings.Split(raw, "\n")
	for _, c := range choices {
		if !strings.Contains(text, c.Addr) || !strings.Contains(loginPageFlat(text), c.Who) {
			t.Errorf("%s is not offered with %q:\n%s", c.Addr, c.Who, text)
		}
		var line string
		for _, l := range lines {
			if strings.Contains(l, c.Addr) {
				line = l
			}
		}
		if got := strings.Contains(line, warn); got != c.Warn {
			t.Errorf("%s drawn in Warn: %v, want %v: %q", c.Addr, got, c.Warn, line)
		}
		if got := strings.Contains(line, "warning"); got != c.Warn {
			t.Errorf("%s says warning: %v, want %v (colour is never the only signal): %q", c.Addr, got, c.Warn, line)
		}
	}
	if s.sel != 0 || !strings.Contains(loginPageLine(text, "127.0.0.1"), ">") {
		t.Errorf("127.0.0.1 is not the one selected:\n%s", text)
	}

	run(t, m, press("down", "down", "up", "enter")...)
	if got := f.asked(); len(got) != 1 || got[0].Addr != "192.168.1.20:0" || got[0].Want != loginpage.Code ||
		got[0].Provider != "claude" || got[0].Approve != "me@example.com" || got[0].LoginURL != loginPageURL {
		t.Fatalf("the page was asked for with %+v, want the second address", got)
	}
	loginPageDrain(t, m, press("esc")...)
	if _, ok := host.st.(*loginPageField); !ok {
		t.Errorf("esc left %T shown", host.st)
	}

	// The page links to the URL asked, else the login's own, and names the
	// account the login is bound to by its e-mail, else its name.
	for _, c := range []struct {
		approve      string
		account      int
		link, person string
	}{
		{"https://claude.ai/oauth/authorize?other", 12, "https://claude.ai/oauth/authorize?other", "me@example.com"},
		{"", 13, loginPageURL, "home"},
		{"", 0, loginPageURL, ""},
	} {
		ask := loginPageCode(nil)
		ask.Approve, ask.Login.Account, ask.Account = c.approve, c.account, 0
		if o := newPageStep(m.e, ask).(*loginPageStep).options("127.0.0.1"); o.LoginURL != c.link || o.Approve != c.person {
			t.Errorf("%+v: the page links to %q and names %q", c, o.LoginURL, o.Approve)
		}
	}

	// 127.0.0.1 is selected wherever it is listed.
	loginPageOffer(t, nil, choices[1], loginPageLo)
	if s := newPageStep(m.e, loginPageCode(nil)).(*loginPageStep); s.sel != 1 {
		t.Errorf("with 127.0.0.1 second, %d is selected", s.sel)
	}

	// Addresses that cannot be listed leave 127.0.0.1, said so.
	loginPageOffer(t, errors.New("no interfaces here"), choices[1])
	s = newPageStep(m.e, loginPageCode(nil)).(*loginPageStep)
	if len(s.choices) != 1 || s.choices[0].Addr != "127.0.0.1" ||
		!strings.Contains(loginPageFlat(s.View(80)), "this machine's addresses cannot be listed (no interfaces here)") {
		t.Errorf("with no addresses listed, the step offers %+v:\n%s", s.choices, s.View(80))
	}
}

// loginPageLine is the first line of s holding sub.
func loginPageLine(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

func TestWatchCannotServeThePage(t *testing.T) {
	f := loginPageFaked(t, "http://127.0.0.1:1/SECRET", "")
	loginPageOffer(t, nil, loginPageLo)
	b := &fakeBackend{WhoFn: func(context.Context) (Principal, error) { return Principal{Name: "eve", Role: "watch"}, nil }}
	m, host, _ := loginPageSetup(t, b, loginPageCode(&loginPageField{}))
	run(t, m, press("enter")...)
	if len(f.asked()) != 0 {
		t.Errorf("a watch principal served the page: %+v", f.asked())
	}
	s := loginPageOf(t, host)
	if !strings.Contains(plain(m), "the one-time page is for the control role") {
		t.Errorf("the step does not say why:\n%s", plain(m))
	}
	for _, k := range s.Keys() {
		if key.Matches(press("enter")[0].(tea.KeyPressMsg), k) {
			t.Errorf("enter is offered to a watch principal: %v", k.Help())
		}
	}
	// Nor does anything else reach the listener for one.
	if cmd := s.serve(); cmd != nil || s.Busy() != "" {
		t.Errorf("serve for a watch principal gave a command (%v) and Busy %q", cmd != nil, s.Busy())
	}
}

func TestEscClosesThePage(t *testing.T) {
	loginPageReal(t)
	loginPageOffer(t, nil, loginPageLo)
	back := &loginPageField{value: "AB"}

	// Before anything is served, esc goes straight back.
	m, host, _ := loginPageSetup(t, &fakeBackend{}, loginPageCode(back))
	run(t, m, press("esc")...)
	if host.st != step(back) {
		t.Fatalf("esc before serving left %T shown", host.st)
	}

	host.st = newPageStep(m.e, loginPageCode(back))
	run(t, m, press("enter")...)
	s := loginPageOf(t, host)
	if s.page == nil {
		t.Fatal("no page was served")
	}
	link := s.page.URL
	seen := loginPageDrain(t, m, press("esc")...)
	if host.st != step(back) || back.value != "AB" {
		t.Errorf("after esc %T is shown (%v)", host.st, host.st)
	}
	if len(host.done) != 0 {
		t.Errorf("closing the page ended the flow: %+v", host.done)
	}
	if s.Busy() != "" {
		t.Errorf("after esc the page step is still busy: %q", s.Busy())
	}
	loginPageGone(t, link)
	for _, msg := range seen {
		if _, ok := msg.(stepDone); ok {
			t.Errorf("esc sent %+v", msg)
		}
	}

	// Run leaves a page still up: its listener is closed.
	host.st = newPageStep(m.e, loginPageCode(back))
	run(t, m, press("enter")...)
	s = loginPageOf(t, host)
	link = s.page.URL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Leave(ctx)
	loginPageGone(t, link)
	loginPageDrain(t, m)
	if host.st != step(back) {
		t.Errorf("a page left by Run leaves %T shown", host.st)
	}

	// esc while the page starts closes it as soon as it is up.
	f := loginPageFaked(t, "http://127.0.0.1:1/SECRET", "")
	host.st = newPageStep(m.e, loginPageCode(back))
	s = loginPageOf(t, host)
	_, serve := s.Update(press("enter")[0])
	run(t, m, press("esc")...)
	if host.st != step(s) || s.Busy() == "" || !strings.Contains(plain(m), "Closing the one-time page") {
		t.Errorf("esc while starting left %T, busy %q:\n%s", host.st, s.Busy(), plain(m))
	}
	loginPageDrain(t, m, serve())
	if host.st != step(back) || len(f.asked()) != 1 {
		t.Errorf("a page started after esc leaves %T, served %d", host.st, len(f.asked()))
	}

	// With no step to go back to, esc ends the flow.
	host.st = newPageStep(m.e, loginPageCode(nil))
	run(t, m, press("esc")...)
	if len(host.done) != 1 || host.done[0].Err == nil || host.done[0].Err.Error() != "the one-time page was closed" {
		t.Errorf("esc with nothing to go back to sent %+v", host.done)
	}
}

// What a page served earlier says never reaches the page served now.
func TestAnotherPagesWordsAreIgnored(t *testing.T) {
	loginPageOffer(t, nil, loginPageLo)
	f := loginPageFaked(t, "http://127.0.0.1:1/SECRET", "")
	back := &loginPageField{}
	m, host, ticks := loginPageSetup(t, &fakeBackend{}, loginPageCode(back))
	s := loginPageOf(t, host)
	_, serve := s.Update(press("enter")[0])
	other := s.id + 1000
	s.Update(loginPageServedMsg{ID: other, Err: errors.New("not this one")})
	if s.state != loginPageStarting || s.failed != "" {
		t.Errorf("another page's start moved the step starting: state %d, failed %q", s.state, s.failed)
	}
	run(t, m, serve())
	_, seen := run(t, m,
		loginPageServedMsg{ID: other, Err: errors.New("not this one")},
		loginPageEndedMsg{ID: other, End: loginpage.End{Why: loginpage.Accepted}},
		loginPageEndedMsg{ID: other, End: loginpage.End{Why: loginpage.Tries}},
		loginPageTickMsg{ID: other})
	if host.st != step(s) || s.state != loginPageServing || s.failed != "" || len(host.done) != 0 || len(seen) != 0 {
		t.Errorf("another page's words moved the step: %T, state %d, failed %q, done %+v, seen %v", host.st, s.state, s.failed, host.done, seen)
	}

	// The countdown ticks every second while the page is up, and stops
	// with it.
	for i := 0; i < 2; i++ {
		due := ticks.advance(time.Second)
		if !slices.ContainsFunc(due, func(msg tea.Msg) bool { return msg == tea.Msg(loginPageTickMsg{ID: s.id}) }) {
			t.Fatalf("second %d: no tick of the page among %v", i+1, due)
		}
		run(t, m, due...)
	}
	f.end(loginpage.End{Why: loginpage.TimedOut})
	loginPageDrain(t, m)
	if _, cmd := s.Update(loginPageTickMsg{ID: s.id}); cmd != nil {
		t.Error("the countdown ticks on after the page ended")
	}
}

// A value being checked when esc is pressed is let finish, as the CLI's
// page lets it: the provider may have taken a code that works once, and
// its acceptance finishes the flow.
func TestAValueCheckedWhileThePageClosesStillFinishesIt(t *testing.T) {
	loginPageReal(t)
	loginPageOffer(t, nil, loginPageLo)
	checking, release := make(chan struct{}), make(chan struct{})
	b := &fakeBackend{FinishLoginFn: func(ctx context.Context, id, code string) (LoginEnd, error) {
		close(checking)
		<-release
		if err := ctx.Err(); err != nil {
			return LoginEnd{}, err
		}
		return LoginEnd{Account: 14, Provider: "claude", Status: "added"}, nil
	}}
	back := &loginPageField{}
	m, host, _ := loginPageSetup(t, b, loginPageCode(back))
	run(t, m, press("enter")...)
	link := loginPageOf(t, host).page.URL
	answered := make(chan int, 1)
	go func() {
		c, _, _ := loginPageSend(link, "GOOD")
		answered <- c
	}()
	<-checking
	run(t, m, press("esc")...)
	if host.st == step(back) {
		t.Fatal("esc went back while a value was being checked")
	}
	if !strings.Contains(plain(m), "Closing the one-time page") || loginPageOf(t, host).Busy() != "the one-time page" {
		t.Errorf("the step does not say it is closing, or is not busy (%q):\n%s", loginPageOf(t, host).Busy(), plain(m))
	}
	close(release)
	if c := <-answered; c != http.StatusOK {
		t.Errorf("the page answered the value %d", c)
	}
	loginPageDrain(t, m)
	if len(host.done) != 1 || host.done[0].End.Account != 14 {
		t.Errorf("stepDone is %+v, want Account 14", host.done)
	}
	loginPageGone(t, link)
}

func TestFiveWrongTriesReturnToTheField(t *testing.T) {
	loginPageReal(t)
	loginPageOffer(t, nil, loginPageLo)
	b := &fakeBackend{FinishLoginFn: func(ctx context.Context, id, code string) (LoginEnd, error) {
		return LoginEnd{}, errors.New("the code " + code + " was not accepted")
	}}
	back := &loginPageField{value: "AB"}
	m, host, _ := loginPageSetup(t, b, loginPageCode(back))
	run(t, m, press("enter")...)
	link := loginPageOf(t, host).page.URL
	for i := 1; i <= loginpage.MaxTries; i++ {
		want := http.StatusBadRequest
		if i == loginpage.MaxTries {
			want = http.StatusForbidden
		}
		if c, _ := loginPagePost(t, link, "WRONG-CODE-12345"); c != want {
			t.Fatalf("try %d answered %d, want %d", i, c, want)
		}
	}
	seen := loginPageDrain(t, m)
	if host.st != step(back) || back.value != "AB" {
		t.Fatalf("after five wrong tries %T is shown", host.st)
	}
	if len(host.done) != 0 {
		t.Errorf("five wrong tries ended the flow: %+v", host.done)
	}
	toasts := loginPageToasts(seen)
	if len(toasts) != 1 || !strings.Contains(toasts[0], "five wrong tries closed the one-time page") ||
		!strings.Contains(toasts[0], "was not accepted") || strings.Contains(toasts[0], "WRONG-CODE-12345") {
		t.Errorf("the toasts are %q, want the reason without the value", toasts)
	}
	loginPageGone(t, link)
}

func TestThePageURLIsCopiedNotLogged(t *testing.T) {
	loginPageReal(t)
	loginPageOffer(t, nil, loginPageLo)
	m, host, ticks := loginPageSetup(t, &fakeBackend{}, loginPageCode(&loginPageField{}))
	var seen []tea.Msg
	_, got := run(t, m, press("enter")...)
	seen = append(seen, got...)
	link := loginPageOf(t, host).page.URL
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimPrefix(u.Path, "/")
	if len(secret) != 43 {
		t.Fatalf("the link's secret is %q", secret)
	}
	_, got = run(t, m, press("c")...)
	seen = append(seen, got...)
	var copied []string
	for _, msg := range got {
		if c, ok := msg.(clipboardMsg); ok {
			copied = append(copied, c.Text)
		}
	}
	if !slices.Equal(copied, []string{link}) {
		t.Errorf("c copied %q, want the link", copied)
	}
	if toasts := loginPageToasts(got); len(toasts) != 1 {
		t.Errorf("c said %q, want one toast that the copy was sent", toasts)
	}
	_, got = run(t, m, ticks.advance(time.Second)...)
	seen = append(seen, got...)
	seen = append(seen, loginPageDrain(t, m, press("esc")...)...)
	for _, s := range loginPageToasts(seen) {
		if strings.Contains(s, secret) {
			t.Errorf("a toast holds the secret: %q", s)
		}
	}
	if strings.Contains(plain(m), secret) {
		t.Errorf("the screen still holds the secret after the page closed:\n%s", plain(m))
	}
}

func TestThePageSaysHowToReachIt(t *testing.T) {
	// On loopback: the ssh line, and the countdown.
	loginPageReal(t)
	loginPageOffer(t, nil, loginPageLo)
	m, host, ticks := loginPageSetup(t, &fakeBackend{}, loginPageCode(&loginPageField{}))
	run(t, m, press("enter")...)
	s := loginPageOf(t, host)
	u, _ := url.Parse(s.page.URL)
	view := loginPageFlat(s.View(80))
	if want := "ssh -L " + u.Port() + ":127.0.0.1:" + u.Port() + " "; !strings.Contains(view, want) || !strings.Contains(view, "@box") {
		t.Errorf("no %q…@box line:\n%s", want, s.View(80))
	}
	if !strings.Contains(view, "ends after one login, five wrong tries or 10 minutes") || !strings.Contains(view, "10m left") {
		t.Errorf("no end and countdown:\n%s", s.View(80))
	}
	if strings.Contains(view, "fingerprint") {
		t.Errorf("a plain HTTP page shows a fingerprint:\n%s", s.View(80))
	}
	run(t, m, ticks.advance(61*time.Second)...)
	if !strings.Contains(loginPageFlat(s.View(80)), "9m left") {
		t.Errorf("a minute later:\n%s", s.View(80))
	}
	for _, w := range []int{40, 60, 80, 120} {
		for _, l := range strings.Split(s.View(w), "\n") {
			if textWidth(l) > w {
				t.Errorf("at %d a line is %d wide: %q", w, textWidth(l), l)
			}
		}
	}
	loginPageDrain(t, m, press("esc")...)

	// Off loopback: the fingerprint, no ssh line, and a login that ends
	// sooner than the page would.
	f := loginPageFaked(t, "https://203.0.113.7:8788/SECRET", "AB:CD:EF")
	ask := loginPageCode(&loginPageField{})
	ask.Login.CreatedAt = ticks.now().Add(2*time.Minute - logins.TTL).UnixMilli()
	host.st = newPageStep(m.e, ask)
	run(t, m, press("enter")...)
	view = loginPageFlat(loginPageOf(t, host).View(80))
	if !strings.Contains(view, "AB:CD:EF") || strings.Contains(view, "ssh -L") || !strings.Contains(view, "2m left") {
		t.Errorf("off loopback the step says:\n%s", loginPageOf(t, host).View(80))
	}
	if got := f.asked(); len(got) != 1 || !got[0].Ends.Equal(ticks.now().Add(2*time.Minute)) {
		t.Errorf("the page was given its end as %+v", got)
	}
	loginPageDrain(t, m, press("esc")...)
}

func TestThePageEndsAsItWasUsed(t *testing.T) {
	loginPageOffer(t, nil, loginPageLo)
	f := loginPageFaked(t, "http://127.0.0.1:1/SECRET", "")
	back := &loginPageField{value: "AB"}
	m, host, _ := loginPageSetup(t, &fakeBackend{}, loginPageCode(back))

	// Its time runs out: back, saying so.
	run(t, m, press("enter")...)
	f.end(loginpage.End{Why: loginpage.TimedOut})
	seen := loginPageDrain(t, m)
	if host.st != step(back) || len(host.done) != 0 {
		t.Fatalf("after its time ran out %T is shown, done %+v", host.st, host.done)
	}
	if toasts := loginPageToasts(seen); len(toasts) != 1 || !strings.Contains(toasts[0], "the one-time page closed after 10 minutes") {
		t.Errorf("the toasts are %q", toasts)
	}

	// The login's own end came first: that is said instead.
	ask := loginPageCode(back)
	ask.Login.CreatedAt = loginPageClock.Add(2*time.Minute - logins.TTL).UnixMilli()
	host.st = newPageStep(m.e, ask)
	run(t, m, press("enter")...)
	f.end(loginpage.End{Why: loginpage.TimedOut})
	seen = loginPageDrain(t, m)
	if toasts := loginPageToasts(seen); host.st != step(back) || len(toasts) != 1 || !strings.Contains(toasts[0], "closed when the login ran out") {
		t.Errorf("a login that ran out leaves %T, says %q", host.st, toasts)
	}

	// Refused: the flow ends with why.
	host.st = newPageStep(m.e, loginPageCode(back))
	run(t, m, press("enter")...)
	f.end(loginpage.End{Why: loginpage.Refused, Message: "no approval can finish this login"})
	loginPageDrain(t, m)
	if len(host.done) != 1 || host.done[0].Err == nil || host.done[0].Err.Error() != "no approval can finish this login" {
		t.Errorf("a refusal sent %+v", host.done)
	}

	// With nothing to go back to, five wrong tries end the flow too.
	host.done = nil
	ask = loginPageCode(nil)
	host.st = newPageStep(m.e, ask)
	run(t, m, press("enter")...)
	f.end(loginpage.End{Why: loginpage.Tries, Message: "not accepted"})
	loginPageDrain(t, m)
	if len(host.done) != 1 || host.done[0].Err == nil || !strings.Contains(host.done[0].Err.Error(), "five wrong tries") {
		t.Errorf("five wrong tries with no step to go back to sent %+v", host.done)
	}

	// A page that cannot listen leaves the choice, saying why.
	f.err = errors.New("listen tcp 127.0.0.1:0: address already in use")
	host.done = nil
	host.st = newPageStep(m.e, loginPageCode(back))
	run(t, m, press("enter")...)
	s := loginPageOf(t, host)
	if s.page != nil || s.Busy() != "" || !strings.Contains(loginPageFlat(s.View(80)), "address already in use") {
		t.Errorf("a page that did not start leaves:\n%s", s.View(80))
	}
	run(t, m, press("esc")...)
	if host.st != step(back) {
		t.Errorf("esc after a failed start left %T", host.st)
	}
}

func TestARefusalNoApprovalCanMendEndsTheFlow(t *testing.T) {
	loginPageReal(t)
	loginPageOffer(t, nil, loginPageLo)
	for _, no := range []*client.Refusal{
		{Status: http.StatusBadRequest, Reason: "unfinishable", Message: "account 12 is gone"},
		{Status: http.StatusNotFound, Message: "no login abc123"},
	} {
		t.Run(strconv.Itoa(no.Status), func(t *testing.T) { loginPageRefused(t, no) })
	}
}

func loginPageRefused(t *testing.T, no *client.Refusal) {
	b := &fakeBackend{FinishLoginFn: func(ctx context.Context, id, code string) (LoginEnd, error) {
		switch code {
		case "NOT-YET":
			return LoginEnd{Pending: true}, nil
		case "PLAIN":
			return LoginEnd{}, &client.Refusal{Status: http.StatusBadRequest, Message: "try the code again"}
		}
		return LoginEnd{}, no
	}}
	m, host, _ := loginPageSetup(t, b, loginPageCode(&loginPageField{}))
	run(t, m, press("enter")...)
	link := loginPageOf(t, host).page.URL
	if c, page := loginPagePost(t, link, "NOT-YET"); c != http.StatusBadRequest || !strings.Contains(page, "not approved yet") {
		t.Errorf("a login not approved yet answered %d, want a try counted:\n%s", c, page)
	}
	if c, _ := loginPagePost(t, link, "PLAIN"); c != http.StatusBadRequest {
		t.Errorf("a plain refusal answered %d, want a try counted", c)
	}
	if c, _ := loginPagePost(t, link, "CODE"); c != http.StatusForbidden {
		t.Errorf("%+v answered %d", no, c)
	}
	loginPageDrain(t, m)
	if len(host.done) != 1 || host.done[0].Err == nil || host.done[0].Err.Error() != no.Message {
		t.Errorf("%+v sent %+v", no, host.done)
	}
	loginPageGone(t, link)
}

func TestTheValueGoesWhereTheAskSays(t *testing.T) {
	b := &fakeBackend{
		FinishLoginFn: func(ctx context.Context, id, code string) (LoginEnd, error) {
			if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > envFinishTimeout || time.Until(dl) < envFinishTimeout-10*time.Second {
				t.Errorf("FinishLogin's deadline is %v, %v", dl, ok)
			}
			if ctx.Err() != nil {
				t.Errorf("the page's end cancelled the check: %v", ctx.Err())
			}
			return LoginEnd{Account: 3}, nil
		},
		PasteKeyFn: func(ctx context.Context, k, label string) (LoginEnd, error) { return LoginEnd{Account: 4}, nil },
		PasteTokenFn: func(ctx context.Context, account int, token string) (LoginEnd, error) {
			return LoginEnd{Account: account}, nil
		},
	}
	ended, end := context.WithCancel(context.Background())
	end() // the page has begun to close: a check under way is still let finish
	for _, c := range []struct {
		ask  pageAsk
		call string
		args []any
		acct int
	}{
		{pageAsk{Want: loginpage.Code, Login: &backendLogin{ID: "l1", Kind: "code"}}, "FinishLogin", []any{"l1", "V1"}, 3},
		{pageAsk{Want: loginpage.Key, Login: &backendLogin{ID: "l2", Kind: "apikey"}}, "FinishLogin", []any{"l2", "V1"}, 3},
		{pageAsk{Want: loginpage.Key, Label: "work"}, "PasteKey", []any{"V1", "work"}, 4},
		{pageAsk{Want: loginpage.Token, Account: 7}, "PasteToken", []any{7, "V1"}, 7},
	} {
		got := &loginPageResult{}
		done, err := loginPageSubmit(b, c.ask, got)(ended, "V1")
		calls := b.named(c.call)
		if err != nil || done != "" || len(calls) == 0 || !slices.Equal(calls[len(calls)-1].Args, c.args) || got.get().Account != c.acct {
			t.Errorf("%+v: done %q, err %v, %s took %v, kept %+v", c.ask, done, err, c.call, calls, got.get())
		}
	}
	if n := len(b.calls()); n != 4 {
		t.Errorf("the four values took %d calls: %q", n, b.calls())
	}

	// Not approved yet is an error, which counts a try; a panic is an error,
	// not a crash of the process, which over SSH is the daemon.
	b.FinishLoginFn = func(ctx context.Context, id, code string) (LoginEnd, error) { return LoginEnd{Pending: true}, nil }
	got := &loginPageResult{}
	if _, err := loginPageSubmit(b, loginPageCode(nil), got)(context.Background(), "V"); err == nil ||
		!strings.Contains(err.Error(), "not approved yet") || got.get() != (LoginEnd{}) {
		t.Errorf("a login not approved yet gives %v and keeps %+v", err, got.get())
	}
	b.FinishLoginFn = func(ctx context.Context, id, code string) (LoginEnd, error) { panic("boom") }
	if _, err := loginPageSubmit(b, loginPageCode(nil), &loginPageResult{})(context.Background(), "V"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a panicking check gives %v", err)
	}
}

func TestThePageAddressGetsThePortItShould(t *testing.T) {
	saved := loginPagePort
	defer func() { loginPagePort = saved }()
	loginPagePort = loginpage.DefaultPort
	for _, a := range []string{"127.0.0.1", "0.0.0.0", "fc00::1"} {
		if got := loginPageAddr(a); got != a {
			t.Errorf("at the default port %q is %q: a busy default port must still fall back", a, got)
		}
	}
	loginPagePort = 0
	if got := loginPageAddr("127.0.0.1"); got != "127.0.0.1:0" {
		t.Errorf("127.0.0.1 is %q", got)
	}
	for _, a := range []string{"fc00::1", "[fc00::1]"} {
		if got := loginPageAddr(a); got != "[fc00::1]:0" {
			t.Errorf("%s is %q", a, got)
		}
	}
}

func TestPageUpdateDoesNoIO(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	loginPageOffer(t, nil, loginPageLo)
	f := loginPageFaked(t, "http://127.0.0.1:1/SECRET", "")
	b := &fakeBackend{}
	_, host, _ := loginPageSetup(t, b, loginPageCode(&loginPageField{}))
	s := loginPageOf(t, host)
	page := &loginpage.Page{URL: "http://127.0.0.1:1/SECRET", Done: make(chan loginpage.End),
		Close: func() { t.Error("Update closed the page itself") }}
	assertNoIO(t, b, func() {
		var st step = s
		feed := func(msgs ...tea.Msg) {
			for _, msg := range msgs {
				st, _ = st.Update(msg)
				st.View(80)
				st.Keys()
				s.Busy()
			}
		}
		feed(press("down", "up", "enter", "enter")...)
		id := s.id
		feed(loginPageServedMsg{ID: id, Page: page, Ends: loginPageClock.Add(time.Minute)},
			loginPageTickMsg{ID: id}, press("c")[0], tea.PasteMsg{Content: "x"}, size(80, 24), press("esc")[0],
			loginPageEndedMsg{ID: id, End: loginpage.End{Why: loginpage.Accepted}})
	})
	if len(f.asked()) != 0 {
		t.Errorf("Update served a page itself: %+v", f.asked())
	}
}

// Every state of the step draws within its width, and what it draws from
// outside it, an address, who reaches it, an error, the link and the
// fingerprint, is cleaned: no escape sequence reaches the terminal and no
// key is shown.
func TestThePageFitsItsWidth(t *testing.T) {
	secret := "sk-ant-api03-" + strings.Repeat("x", 30)
	loginPageOffer(t, nil, loginPageLo,
		loginpage.Choice{Addr: "fd7a:115c:a1e0:ab12:4843:cd96:6258:b240", Iface: "tailscale0", Who: "machines on the carrier-grade NAT or Tailscale network of tailscale0"},
		loginpage.Choice{Addr: "10.0.0.1\x1b[2J", Iface: "evil", Who: "\x1b]52;c;eA==\x07machines 日本語の on " + secret + " eth0\r\n", Warn: true},
		loginpage.Choice{Addr: "0.0.0.0", Who: "every interface: anyone who can reach this machine on any of them", Warn: true})
	f := loginPageFaked(t, "https://[fd7a:115c:a1e0:ab12:4843:cd96:6258:b240]:8788/\x1b[2J"+strings.Repeat("A", 43), "AB:"+strings.Repeat("CD:", 30)+"EF\a")
	_, host, _ := loginPageSetup(t, &fakeBackend{}, loginPageCode(&loginPageField{}))
	s := loginPageOf(t, host)
	check := func(what string) {
		t.Helper()
		for w := 30; w <= 140; w++ {
			v := s.View(w)
			if strings.ContainsAny(v, "\a\r") || strings.Contains(v, "\x1b]") || strings.Contains(v, "\x1b[2J") || strings.Contains(v, "api03") {
				t.Fatalf("%s at %d draws what it should not: %q", what, w, v)
			}
			for _, l := range strings.Split(v, "\n") {
				if n := textWidth(ansi.Strip(l)); n > w {
					t.Errorf("%s at %d: a line is %d wide: %q", what, w, n, ansi.Strip(l))
				}
			}
		}
	}
	check("the choice")
	s.note = "this machine's addresses cannot be listed (" + secret + "\x1b[2J), so only 127.0.0.1 is offered"
	check("a note")

	f.err = errors.New("listen tcp 10.0.0.1:0: bind: can't assign requested address " + secret)
	st, cmd := s.Update(press("enter")[0])
	check("starting")
	st.Update(cmd())
	check("a failed start")
	f.err = nil
	st, cmd = s.Update(press("enter")[0])
	st.Update(press("esc")[0])
	check("leaving while starting")
	s.leaving = false
	st.Update(cmd())
	check("serving")
	if !strings.Contains(loginPageFlat(s.View(140)), strings.Repeat("A", 43)) {
		t.Errorf("the link is not drawn whole:\n%s", s.View(140))
	}
	st.Update(press("esc")[0])
	check("closing")
	st.Update(loginPageEndedMsg{ID: s.id, End: loginpage.End{Why: loginpage.Stopped}})
	check("closed")
}

// The widths again, in a test binary of its own measuring ambiguous
// characters the other way, as TestTextBothEastAsianModes does.
func TestThePageBothEastAsianModes(t *testing.T) {
	if os.Getenv(textModeVar) != "" {
		t.Skip("a child run")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mode := "wide"
	if textEastAsian() {
		mode = "narrow"
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "RUNEWIDTH_EASTASIAN=") && !strings.HasPrefix(kv, textModeVar+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, textModeVar+"="+mode)
	if mode == "wide" {
		env = append(env, "RUNEWIDTH_EASTASIAN=1")
	}
	cmd := exec.Command(exe, "-test.count=1", "-test.v", "-test.run", "^(TestTextModeIsTheOneAsked|TestThePageFitsItsWidth)$")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the %s run failed: %v\n%s", mode, err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestTextModeIsTheOneAsked") || !strings.Contains(string(out), "--- PASS: TestThePageFitsItsWidth") {
		t.Errorf("the %s run did not run in its mode:\n%s", mode, out)
	}
}
