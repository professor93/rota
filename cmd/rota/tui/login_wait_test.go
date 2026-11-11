package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/wire"
)

// loginWaitHost stands where the login flow does: it shows one step, starts
// it by its Init when it has one, and hands it every message, as the flow
// hands its current step.
type loginWaitHost struct {
	s     step
	width int
}

func (h *loginWaitHost) Init() tea.Cmd {
	if i, ok := h.s.(interface{ Init() tea.Cmd }); ok {
		return i.Init()
	}
	return nil
}

func (h *loginWaitHost) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	h.s, cmd = h.s.Update(msg)
	return h, cmd
}

func (h *loginWaitHost) View() tea.View { return tea.NewView(h.s.View(h.width)) }

// loginWaitEnv is the env a step is made with: b, the fake clock, no tabs.
func loginWaitEnv(t *testing.T, b *fakeBackend, ticks *fakeTicks, with ...func(*Options)) *env {
	t.Helper()
	shellRegistry(t)
	return shellModel(t, b, ticks, with...).e
}

// loginWaitLogin is a device login begun three minutes before the fake
// clock's now, so twelve minutes are left.
func loginWaitLogin(ticks *fakeTicks) *backendLogin {
	return &backendLogin{ID: "dev-1", Provider: "acme", Kind: "device",
		URL:       "https://auth.example.com/activate?user_code=ABCD-EFGH",
		CreatedAt: ticks.now().Add(-3 * time.Minute).UnixMilli()}
}

// loginWaitFinishes answers FinishLogin from answers, one per call, and the
// last one for every call past them.
func loginWaitFinishes(answers ...func() (LoginEnd, error)) func(context.Context, string, string) (LoginEnd, error) {
	var n atomic.Int32
	return func(context.Context, string, string) (LoginEnd, error) {
		i := min(int(n.Add(1))-1, len(answers)-1)
		return answers[i]()
	}
}

func loginWaitPending() (LoginEnd, error) { return LoginEnd{Pending: true}, nil }

func loginWaitAdded() (LoginEnd, error) {
	return LoginEnd{Account: 14, Provider: "acme", Status: "added"}, nil
}

// loginWaitDone is the stepDone among msgs, and whether there is one.
func loginWaitDone(t *testing.T, msgs []tea.Msg) (stepDone, bool) {
	t.Helper()
	var got []stepDone
	for _, m := range msgs {
		if d, ok := m.(stepDone); ok {
			got = append(got, d)
		}
	}
	if len(got) > 1 {
		t.Errorf("the step said it was done %d times: %+v", len(got), got)
	}
	if len(got) == 0 {
		return stepDone{}, false
	}
	return got[0], true
}

// loginWaitOf is every message of type T among msgs.
func loginWaitOf[T any](msgs []tea.Msg) []T {
	var out []T
	for _, m := range msgs {
		if v, ok := m.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

// loginWaitLines is the step's view at width w as plain lines.
func loginWaitLines(s step, w int) []string {
	return strings.Split(ansi.Strip(s.View(w)), "\n")
}

// loginWaitTimerSet waits, at most five seconds, until a timer is set on
// ticks: a timer is set by a command, which a busy machine may run a moment
// after the round that started it has ended.
func loginWaitTimerSet(t *testing.T, ticks *fakeTicks) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		ticks.mu.Lock()
		n := len(ticks.timers)
		ticks.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no timer was set")
		}
	}
}

func loginWaitAsked(b *fakeBackend) []string {
	var out []string
	for _, c := range b.named("FinishLogin") {
		out = append(out, c.String())
	}
	return out
}

func TestADeviceLoginFinishesUnprompted(t *testing.T) {
	for _, start := range []struct {
		name  string
		first func(h *loginWaitHost) []tea.Msg
	}{
		// The flow runs a step's Init when it shows it.
		{"started by the flow", func(h *loginWaitHost) []tea.Msg { return []tea.Msg{tea.BatchMsg{h.Init()}} }},
		// A flow that does not still hands it every message the shell
		// hears, and the first one starts it.
		{"started by the first message it hears", func(*loginWaitHost) []tea.Msg { return []tea.Msg{daemonMsg{}} }},
	} {
		t.Run(start.name, func(t *testing.T) {
			ticks := &fakeTicks{}
			b := &fakeBackend{FinishLoginFn: loginWaitFinishes(loginWaitPending, loginWaitPending, loginWaitAdded)}
			e := loginWaitEnv(t, b, ticks)
			h := &loginWaitHost{s: newDeviceStep(e, loginWaitLogin(ticks), "work@example.com"), width: 80}
			_, seen := run(t, h, start.first(h)...)
			want := []string{"FinishLogin(dev-1, )"}
			if got := loginWaitAsked(b); !slices.Equal(got, want) {
				t.Fatalf("once started the step asked %q, want %q", got, want)
			}
			if _, done := loginWaitDone(t, seen); done {
				t.Fatal("a pending login was taken for done")
			}
			if !strings.Contains(plain(h), "Waiting for the approval (12m left)") {
				t.Errorf("the step does not say it waits:\n%s", plain(h))
			}
			// Nothing is asked before the interval is up.
			_, seen = run(t, h, ticks.advance(loginWaitEvery-time.Millisecond)...)
			if got := loginWaitAsked(b); len(got) != 1 || len(seen) != 0 {
				t.Fatalf("before the interval: asked %q, saw %v", got, seen)
			}
			_, seen = run(t, h, ticks.advance(time.Millisecond)...)
			if got := loginWaitAsked(b); len(got) != 2 {
				t.Fatalf("after one interval it asked %q, want twice", got)
			}
			if _, done := loginWaitDone(t, seen); done {
				t.Fatal("the second pending answer was taken for done")
			}
			_, seen = run(t, h, ticks.advance(loginWaitEvery)...)
			if got := loginWaitAsked(b); !slices.Equal(got, slices.Repeat(want, 3)) {
				t.Fatalf("after two intervals it asked %q", got)
			}
			d, done := loginWaitDone(t, seen)
			if !done || d.Err != nil || d.End != (LoginEnd{Account: 14, Provider: "acme", Status: "added"}) {
				t.Fatalf("an approved login gave %+v (done %v)", d, done)
			}
			// Done is done: no question follows, however long it is left.
			_, seen = run(t, h, ticks.advance(time.Minute)...)
			run(t, h, press("x")[0], daemonMsg{})
			if got := loginWaitAsked(b); len(got) != 3 || len(seen) != 0 {
				t.Errorf("after it was done the step asked %q and saw %v", got, seen)
			}
			if i, ok := h.s.(interface{ Init() tea.Cmd }); ok && i.Init() != nil {
				t.Error("a second Init starts a second wait")
			}
		})
	}
}

func TestTheDeviceCodeIsShown(t *testing.T) {
	ticks := &fakeTicks{}
	opened := false
	e := loginWaitEnv(t, &fakeBackend{}, ticks, func(o *Options) {
		o.OpenURL = func(string) error { opened = true; return nil }
	})
	l := loginWaitLogin(ticks)
	s := newDeviceStep(e, l, "work@example.com")
	lines := loginWaitLines(s, 80)
	text := strings.Join(lines, "\n")
	for _, want := range []string{"Approve as work@example.com at", l.URL, "o open in a browser", "c copy the link",
		"Waiting for the approval (12m left)"} {
		if !strings.Contains(text, want) {
			t.Errorf("the step does not say %q:\n%s", want, text)
		}
	}
	// The code has a line of its own, in bold, beside the URL that carries it.
	at := slices.IndexFunc(lines, func(s string) bool { return strings.TrimSpace(s) == "ABCD-EFGH" })
	if at < 0 {
		t.Fatalf("the code is not on a line of its own:\n%s", text)
	}
	if styled := strings.Split(s.View(80), "\n")[at]; !strings.Contains(styled, "\x1b[1") || !strings.HasPrefix(lines[at], "  ") {
		t.Errorf("the code's line is not drawn large and set in: %q", styled)
	}
	if opened {
		t.Error("drawing opened a browser")
	}

	// A URL with no code shows none; with no browser, o is not offered; and
	// with nobody named, it is approved by whoever opens it.
	l.URL = "https://auth.example.com/activate"
	e = loginWaitEnv(t, &fakeBackend{}, ticks)
	s = newDeviceStep(e, l, "")
	text = strings.Join(loginWaitLines(s, 80), "\n")
	if strings.Contains(text, "ABCD") || strings.Contains(text, "o open") || !strings.Contains(text, "Approve at\n") {
		t.Errorf("with no code, no browser and nobody named:\n%s", text)
	}
	if keys := s.Keys(); len(keys) != 1 || keys[0].Help().Key != "c" {
		t.Errorf("with no browser the keys are %v", keys)
	}
	// A login that says nothing of when it began was begun just now.
	l.CreatedAt = 0
	if got := plain(&loginWaitHost{s: newDeviceStep(e, l, ""), width: 80}); !strings.Contains(got, "(15m left)") {
		t.Errorf("a login with no time of its own:\n%s", got)
	}
}

// The login's reply carries no interval: its state stays on the server
// (F3). So the step asks every five seconds, the CLI's own devicePoll, and
// counts each interval from the last answer, so it never has two questions
// out at once.
func TestTheDeviceLoginIsAskedEveryFiveSeconds(t *testing.T) {
	if loginWaitEvery != 5*time.Second {
		t.Fatalf("a device login is asked every %v, not every 5s", loginWaitEvery)
	}
	ticks := &fakeTicks{}
	release := make(chan struct{})
	slow := func() (LoginEnd, error) { <-release; return LoginEnd{Pending: true}, nil }
	b := &fakeBackend{FinishLoginFn: loginWaitFinishes(slow, loginWaitPending)}
	h := &loginWaitHost{s: newDeviceStep(loginWaitEnv(t, b, ticks), loginWaitLogin(ticks), "w"), width: 80}
	run(t, h, tea.BatchMsg{h.Init()})
	// The first answer is slow: however long it takes, nothing else is sent.
	run(t, h, ticks.advance(30*time.Second)...)
	if got := loginWaitAsked(b); len(got) != 1 {
		t.Fatalf("while one question was out the step asked %q", got)
	}
	close(release)
	await(t, h)
	run(t, h, ticks.advance(loginWaitEvery-time.Millisecond)...)
	if got := loginWaitAsked(b); len(got) != 1 {
		t.Fatalf("the interval counted from the question, not the answer: %q", got)
	}
	run(t, h, ticks.advance(time.Millisecond)...)
	if got := loginWaitAsked(b); len(got) != 2 {
		t.Fatalf("an interval after the answer it asked %q", got)
	}
}

// A question that outlives its deadline is answered for, and no other is
// sent until it has returned (Backend).
func TestADeviceQuestionThatHangsIsNotAskedTwice(t *testing.T) {
	ticks := &fakeTicks{}
	release := make(chan struct{})
	var once sync.Once
	b := &fakeBackend{FinishLoginFn: loginWaitFinishes(func() (LoginEnd, error) { <-release; return LoginEnd{Pending: true}, nil }, loginWaitPending)}
	e := loginWaitEnv(t, b, ticks)
	// A program that ends, ends its calls: the ignoring callee is then
	// answered for envCallGrace later.
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	h := &loginWaitHost{s: newDeviceStep(e, loginWaitLogin(ticks), "w"), width: 80}
	run(t, h, tea.BatchMsg{h.Init()})
	cancel()
	await(t, h)
	if !strings.Contains(plain(h), "no answer") {
		t.Errorf("a question answered for does not say so:\n%s", plain(h))
	}
	for range 3 {
		run(t, h, ticks.advance(loginWaitEvery)...)
	}
	if got := loginWaitAsked(b); len(got) != 1 {
		t.Fatalf("with its question still running the step asked again: %q", got)
	}
	once.Do(func() { close(release) })
	s := h.s.(*loginWaitDevice)
	for deadline := time.Now().Add(5 * time.Second); s.parked.Load() > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the question that hung is still counted after it returned")
		}
	}
	// The next turn of the timer asks. On a busy machine the timer may be
	// set a turn late, so a few turns are given, a moment apart, and never
	// enough to reach the login's end.
	for i := 0; i < 20 && len(loginWaitAsked(b)) < 2; i++ {
		run(t, h, ticks.advance(loginWaitEvery)...)
		time.Sleep(10 * time.Millisecond)
	}
	if got := loginWaitAsked(b); len(got) != 2 {
		t.Fatalf("once its question returned the step asked %q, want once more", got)
	}
}

// A question that hangs past its deadline holds the next one back, but not
// the login's end: the step still ends when the login runs out.
func TestADeviceLoginRunsOutWithAQuestionHanging(t *testing.T) {
	ticks := &fakeTicks{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	b := &fakeBackend{FinishLoginFn: loginWaitFinishes(func() (LoginEnd, error) { <-release; return LoginEnd{Pending: true}, nil })}
	e := loginWaitEnv(t, b, ticks)
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	h := &loginWaitHost{s: newDeviceStep(e, loginWaitLogin(ticks), "w"), width: 80}
	run(t, h, tea.BatchMsg{h.Init()})
	cancel()
	await(t, h)
	if h.s.(*loginWaitDevice).parked.Load() != 1 {
		t.Fatal("the question that hangs is not counted")
	}
	// The one timer the answer set fires past the login's end.
	loginWaitTimerSet(t, ticks)
	_, seen := run(t, h, ticks.advance(13*time.Minute)...)
	d, done := loginWaitDone(t, seen)
	if !done || d.Err == nil || d.Err.Error() != "the login dev-1 has expired" {
		t.Fatalf("a login run out with its question hanging gave %+v (done %v)", d, done)
	}
	if got := loginWaitAsked(b); len(got) != 1 {
		t.Errorf("with its question hanging the step asked %q", got)
	}
}

func TestADeviceRefusalIsSaidAndTheWaitGoesOn(t *testing.T) {
	ticks := &fakeTicks{}
	b := &fakeBackend{FinishLoginFn: loginWaitFinishes(
		func() (LoginEnd, error) {
			return LoginEnd{}, &client.Refusal{Status: 502, Message: "the provider answered 502\x1b[2J \x1b]0;x\a"}
		},
		func() (LoginEnd, error) {
			return LoginEnd{}, errors.New("the rota API did not answer POST /v1/login/dev-1")
		},
		func() (LoginEnd, error) {
			return LoginEnd{}, &client.Refusal{Status: 409, Message: "another rota holds the store"}
		},
		loginWaitPending,
		loginWaitAdded,
	)}
	h := &loginWaitHost{s: newDeviceStep(loginWaitEnv(t, b, ticks), loginWaitLogin(ticks), "w"), width: 80}
	_, seen := run(t, h, tea.BatchMsg{h.Init()})
	if _, done := loginWaitDone(t, seen); done {
		t.Fatal("a 502 ended the wait")
	}
	view := h.s.View(80)
	if !strings.Contains(ansi.Strip(view), "the provider answered 502") || strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\x1b]") {
		t.Errorf("the refusal is not said, or not cleaned:\n%q", view)
	}
	run(t, h, ticks.advance(loginWaitEvery)...)
	if !strings.Contains(plain(h), "did not answer") || strings.Contains(plain(h), "502") {
		t.Errorf("a later failure does not replace the first:\n%s", plain(h))
	}
	// A busy store is no verdict on the login: the wait goes on.
	_, seen = run(t, h, ticks.advance(loginWaitEvery)...)
	if _, done := loginWaitDone(t, seen); done || !strings.Contains(plain(h), "another rota holds the store") {
		t.Fatalf("a 409 ended the wait, or is not said:\n%s", plain(h))
	}
	run(t, h, ticks.advance(loginWaitEvery)...)
	if strings.Contains(plain(h), "another rota") {
		t.Errorf("a pending answer leaves the failure said:\n%s", plain(h))
	}
	_, seen = run(t, h, ticks.advance(loginWaitEvery)...)
	if d, done := loginWaitDone(t, seen); !done || d.End.Account != 14 {
		t.Errorf("after the refusals the approval gave %+v (done %v)", d, done)
	}
}

// The ends the CLI's wait has too: a login no longer parked, the provider's
// verdict on the device code, and a login no approval can finish. Each ends
// the step at once, says so cleaned, and asks nothing more.
func TestADeviceLoginNothingCanFinishEndsTheWait(t *testing.T) {
	for _, c := range []struct {
		name    string
		no      *client.Refusal
		says    string
		refresh bool // the listing is read again, for what the login may have added
	}{
		// Another rota finished it, an answer lost on the way finished it
		// here, or it ran out.
		{"gone", &client.Refusal{Status: 404, Message: "no pending login dev-1"},
			"the login dev-1 is no longer parked: finished elsewhere, or run out", true},
		// The provider denied it, or its code is spent: approving again
		// cannot help, as lib answers it with a 400 in the provider's words.
		{"denied", &client.Refusal{Status: 400, Message: "login was denied\x1b[2J"}, "login was denied", false},
		{"unfinishable", &client.Refusal{Status: 400, Reason: "unfinishable", Message: "the account this login was begun for is gone"},
			"the account this login was begun for is gone", false},
		// The reason alone decides, whatever status carries it.
		{"unfinishable at any status", &client.Refusal{Status: 422, Reason: "unfinishable", Message: "approved by another account"},
			"approved by another account", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ticks := &fakeTicks{}
			b := &fakeBackend{FinishLoginFn: loginWaitFinishes(loginWaitPending,
				func() (LoginEnd, error) { return LoginEnd{}, c.no })}
			h := &loginWaitHost{s: newDeviceStep(loginWaitEnv(t, b, ticks), loginWaitLogin(ticks), "w"), width: 80}
			run(t, h, tea.BatchMsg{h.Init()})
			_, seen := run(t, h, ticks.advance(loginWaitEvery)...)
			d, done := loginWaitDone(t, seen)
			if !done || !errors.Is(d.Err, c.no) || d.Err.Error() != c.says {
				t.Fatalf("%v gave %+v (done %v), want an end saying %q over the refusal", c.no, d, done, c.says)
			}
			var want []refreshMsg
			if c.refresh {
				want = []refreshMsg{{How: RefreshStale}}
			}
			if got := loginWaitOf[refreshMsg](seen); !slices.Equal(got, want) {
				t.Errorf("the end asked for %v, want %v", got, want)
			}
			run(t, h, ticks.advance(time.Minute)...)
			if got := loginWaitAsked(b); len(got) != 2 {
				t.Errorf("after the end the step asked %q", got)
			}
		})
	}
}

// Every question carries the deadline of a login's finish (R29), and the
// step's own life: leaving the login ends a question still out, and none
// follows, whether it was out or the timer was set.
func TestADeviceQuestionEndsWithItsDeadlineOrTheStep(t *testing.T) {
	t.Run("the deadline", func(t *testing.T) {
		var left time.Duration
		b := &fakeBackend{FinishLoginFn: func(ctx context.Context, _, _ string) (LoginEnd, error) {
			if dl, ok := ctx.Deadline(); ok {
				left = time.Until(dl)
			}
			return LoginEnd{Pending: true}, nil
		}}
		ticks := &fakeTicks{}
		h := &loginWaitHost{s: newDeviceStep(loginWaitEnv(t, b, ticks), loginWaitLogin(ticks), "w"), width: 80}
		run(t, h, tea.BatchMsg{h.Init()})
		if left <= envFinishTimeout-10*time.Second || left > envFinishTimeout {
			t.Errorf("a question had %v left of its deadline, want about %v", left, envFinishTimeout)
		}
	})
	t.Run("left while a question is out", func(t *testing.T) {
		ended := make(chan error, 1)
		b := &fakeBackend{FinishLoginFn: func(ctx context.Context, _, _ string) (LoginEnd, error) {
			<-ctx.Done()
			ended <- ctx.Err()
			return LoginEnd{}, ctx.Err()
		}}
		ticks := &fakeTicks{}
		h := &loginWaitHost{s: newDeviceStep(loginWaitEnv(t, b, ticks), loginWaitLogin(ticks), "w"), width: 80}
		run(t, h, tea.BatchMsg{h.Init()})
		h.s.(interface{ Leave(context.Context) }).Leave(context.Background())
		select {
		case err := <-ended:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the question ended with %v, want it cancelled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("leaving the step left its question running")
		}
		_, seen := await(t, h)
		if _, done := loginWaitDone(t, seen); done || strings.Contains(plain(h), "canceled") {
			t.Errorf("a step left said %v:\n%s", seen, plain(h))
		}
		if due := ticks.advance(time.Minute); len(due) != 0 || len(loginWaitAsked(b)) != 1 {
			t.Errorf("a step left set a timer (%v) or asked again: %q", due, loginWaitAsked(b))
		}
	})
	t.Run("left while the timer is set", func(t *testing.T) {
		ticks := &fakeTicks{}
		b := &fakeBackend{FinishLoginFn: loginWaitFinishes(loginWaitPending)}
		h := &loginWaitHost{s: newDeviceStep(loginWaitEnv(t, b, ticks), loginWaitLogin(ticks), "w"), width: 80}
		run(t, h, tea.BatchMsg{h.Init()})
		leave := h.s.(interface{ Leave(context.Context) }).Leave
		leave(context.Background())
		leave(context.Background())
		_, seen := run(t, h, ticks.advance(time.Minute)...)
		if got := loginWaitAsked(b); len(got) != 1 || len(seen) != 0 {
			t.Errorf("a step left asked %q and said %v", got, seen)
		}
	})
}

func TestADeviceLoginRunsOutOnTheClock(t *testing.T) {
	ticks := &fakeTicks{}
	b := &fakeBackend{FinishLoginFn: loginWaitFinishes(loginWaitPending)}
	l := loginWaitLogin(ticks)
	// Seven seconds are left: one more question, then the end.
	l.CreatedAt = ticks.now().Add(-15*time.Minute + 7*time.Second).UnixMilli()
	h := &loginWaitHost{s: newDeviceStep(loginWaitEnv(t, b, ticks), l, "w"), width: 80}
	run(t, h, tea.BatchMsg{h.Init()})
	if !strings.Contains(plain(h), "(7s left)") {
		t.Errorf("the countdown does not say seconds:\n%s", plain(h))
	}
	_, seen := run(t, h, ticks.advance(loginWaitEvery)...)
	if _, done := loginWaitDone(t, seen); done || len(loginWaitAsked(b)) != 2 {
		t.Fatalf("with two seconds left the step ended, or did not ask: %q", loginWaitAsked(b))
	}
	_, seen = run(t, h, ticks.advance(loginWaitEvery)...)
	d, done := loginWaitDone(t, seen)
	if !done || d.Err == nil || d.Err.Error() != "the login dev-1 has expired" {
		t.Fatalf("a login past its time gave %+v (done %v)", d, done)
	}
	if got := loginWaitAsked(b); len(got) != 2 {
		t.Errorf("a login past its time was asked about: %q", got)
	}
}

// A step's own timer and answer reach only it: the flow may show another
// step for another login while what it left is still out.
func TestADeviceStepHearsOnlyItsOwnTimerAndAnswer(t *testing.T) {
	t.Run("a timer", func(t *testing.T) {
		ticks := &fakeTicks{}
		b := &fakeBackend{FinishLoginFn: loginWaitFinishes(loginWaitPending)}
		e := loginWaitEnv(t, b, ticks)
		other := loginWaitLogin(ticks)
		other.ID = "dev-2"
		h := &loginWaitHost{s: newDeviceStep(e, other, "w"), width: 80}
		run(t, h, tea.BatchMsg{h.Init()})
		// The flow moves on to another login, which starts at once; both
		// timers then fire, and only the new step's own one asks.
		h.s = newDeviceStep(e, loginWaitLogin(ticks), "w")
		run(t, h, tea.BatchMsg{h.Init()})
		run(t, h, ticks.advance(loginWaitEvery)...)
		want := []string{"FinishLogin(dev-2, )", "FinishLogin(dev-1, )", "FinishLogin(dev-1, )"}
		if got := loginWaitAsked(b); !slices.Equal(got, want) {
			t.Errorf("with another step's timer firing too the step asked %q, want %q", got, want)
		}
	})
	t.Run("an answer", func(t *testing.T) {
		ticks := &fakeTicks{}
		release := make(chan struct{})
		b := &fakeBackend{FinishLoginFn: func(_ context.Context, id, _ string) (LoginEnd, error) {
			if id == "dev-2" {
				<-release
				return LoginEnd{Account: 2, Status: "added"}, nil
			}
			return LoginEnd{Pending: true}, nil
		}}
		e := loginWaitEnv(t, b, ticks)
		other := loginWaitLogin(ticks)
		other.ID = "dev-2"
		h := &loginWaitHost{s: newDeviceStep(e, other, "w"), width: 80}
		run(t, h, tea.BatchMsg{h.Init()})
		h.s = newDeviceStep(e, loginWaitLogin(ticks), "w")
		run(t, h, tea.BatchMsg{h.Init()})
		close(release)
		_, seen := await(t, h)
		if _, done := loginWaitDone(t, seen); done {
			t.Fatal("another step's approval finished this one")
		}
		run(t, h, ticks.advance(loginWaitEvery)...)
		if got := loginWaitAsked(b); len(got) != 3 || got[2] != "FinishLogin(dev-1, )" {
			t.Errorf("after another step's answer the step asked %q", got)
		}
	})
}

func TestTheDeviceKeysOpenAndCopy(t *testing.T) {
	ticks := &fakeTicks{}
	var opened []string
	e := loginWaitEnv(t, &fakeBackend{FinishLoginFn: loginWaitFinishes(loginWaitPending)}, ticks, func(o *Options) {
		o.OpenURL = func(u string) error { opened = append(opened, u); return nil }
	})
	l := loginWaitLogin(ticks)
	h := &loginWaitHost{s: newDeviceStep(e, l, "w"), width: 80}
	run(t, h, tea.BatchMsg{h.Init()})
	_, seen := run(t, h, press("c")...)
	if got := loginWaitOf[clipboardMsg](seen); !slices.Equal(got, []clipboardMsg{{Text: l.URL}}) {
		t.Errorf("c copied %v", got)
	}
	if toasts := loginWaitOf[toastMsg](seen); len(toasts) != 1 || !strings.Contains(toasts[0].Text, "sent") {
		t.Errorf("a copy is said as sent: %v", toasts)
	}
	run(t, h, press("o")...)
	if !slices.Equal(opened, []string{l.URL}) {
		t.Errorf("o opened %q", opened)
	}
	// Over SSH there is no browser here, and o does nothing.
	e = loginWaitEnv(t, &fakeBackend{FinishLoginFn: loginWaitFinishes(loginWaitPending)}, ticks)
	h = &loginWaitHost{s: newDeviceStep(e, l, "w"), width: 80}
	run(t, h, tea.BatchMsg{h.Init()})
	if _, seen := run(t, h, press("o")...); len(seen) != 0 {
		t.Errorf("o with no browser gave %v", seen)
	}
}

// loginWaitRota installs a fake rota that says its arguments on stdout and
// does what spec says, and is what Options.Exec runs.
func loginWaitRota(t *testing.T, spec fakecli.Spec) (start func(args ...string) *exec.Cmd, ran *[][]string) {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	spec.Stdout = append([]string{"ARGS={{args}}"}, spec.Stdout...)
	path := fakecli.Install(t, bin, "rota", spec)
	var mu sync.Mutex
	ran = &[][]string{}
	return func(args ...string) *exec.Cmd {
		mu.Lock()
		*ran = append(*ran, args)
		mu.Unlock()
		return exec.Command(path, args...)
	}, ran
}

// loginWaitHandOff runs the command a handOffMsg hands the terminal to, as
// Bubble Tea would, and gives what its Done makes of how it ended, and what
// it wrote to stdout.
func loginWaitHandOff(t *testing.T, seen []tea.Msg) (tea.Msg, string) {
	t.Helper()
	offs := loginWaitOf[handOffMsg](seen)
	if len(offs) != 1 || offs[0].Cmd == nil || offs[0].Done == nil {
		t.Fatalf("want one hand-off of a command, got %+v among %v", offs, seen)
	}
	var out bytes.Buffer
	c := offs[0].Cmd
	c.SetStdin(strings.NewReader(""))
	c.SetStdout(&out)
	c.SetStderr(io.Discard)
	return offs[0].Done(c.Run()), out.String()
}

func TestADelegatedLoginHandsOverTheTerminal(t *testing.T) {
	run0, ran := loginWaitRota(t, fakecli.Spec{})
	e := loginWaitEnv(t, &fakeBackend{}, &fakeTicks{}, func(o *Options) { o.Exec = run0 })
	e.list = Listing{Accounts: []wire.Account{{ID: 5, Provider: "acme"}, {ID: 6, Provider: "grok"}}}
	h := &loginWaitHost{s: newDelegatedStep(e, 6), width: 80}
	if got := plain(h); !strings.Contains(got, "grok signs this account in itself; enter hands it the terminal") {
		t.Errorf("the step says:\n%s", got)
	}
	if keys := h.s.Keys(); len(keys) != 1 || keys[0].Help().Key != "⏎" || !slices.Equal(keys[0].Keys(), []string{"enter"}) {
		t.Errorf("the keys are %v", keys)
	}
	_, seen := run(t, h, press("enter")...)
	back, out := loginWaitHandOff(t, seen)
	if !slices.EqualFunc(*ran, [][]string{{"login", "6"}}, slices.Equal) || !strings.Contains(out, "ARGS=login 6") {
		t.Fatalf("the terminal went to rota %q, which said %q", *ran, out)
	}
	// The terminal coming back is the step's own: another step for the same
	// account, one the flow shows later, takes nothing from it.
	if _, seen := run(t, &loginWaitHost{s: newDelegatedStep(e, 6), width: 80}, back); len(seen) != 0 {
		t.Errorf("another step's hand-off ended this one: %v", seen)
	}
	_, seen = run(t, h, back)
	d, done := loginWaitDone(t, seen)
	if !done || d.Err != nil || d.End != (LoginEnd{Account: 6, Provider: "grok", Status: "refreshed"}) {
		t.Errorf("a clean exit gave %+v (done %v)", d, done)
	}
	if got := loginWaitOf[refreshMsg](seen); !slices.Equal(got, []refreshMsg{{How: RefreshStale}}) {
		t.Errorf("a clean exit asked for %v, want a read of what is stale", got)
	}
	// Signed in, it hands nothing over again; and locally c copies nothing.
	if _, seen := run(t, h, press("enter")[0], press("c")[0]); len(seen) != 0 || len(*ran) != 1 {
		t.Errorf("after the sign-in enter and c gave %v, and rota ran %q", seen, *ran)
	}
	// An account the listing does not hold yet is named by its CLI.
	e.list = Listing{}
	if got := plain(&loginWaitHost{s: newDelegatedStep(e, 9), width: 80}); !strings.Contains(got, "own CLI") {
		t.Errorf("an account not listed yet:\n%s", got)
	}
}

func TestAFailedHandOffShowsItsLastWords(t *testing.T) {
	run0, ran := loginWaitRota(t, fakecli.Spec{Stderr: []string{"grok: starting", "boom"}, Exit: 3})
	e := loginWaitEnv(t, &fakeBackend{}, &fakeTicks{}, func(o *Options) { o.Exec = run0 })
	h := &loginWaitHost{s: newDelegatedStep(e, 6), width: 80}
	_, seen := run(t, h, press("enter")...)
	// A second enter before the terminal is handed over hands it once.
	_, more := run(t, h, press("enter")...)
	seen = append(seen, more...)
	back, _ := loginWaitHandOff(t, seen)
	_, seen = run(t, h, back)
	if _, done := loginWaitDone(t, seen); done || len(loginWaitOf[refreshMsg](seen)) != 0 {
		t.Fatalf("a failed hand-off ended the step: %v", seen)
	}
	got := plain(h)
	if !strings.Contains(got, "rota login 6 ended with exit status 3: boom") || strings.Contains(got, "starting") {
		t.Errorf("the step does not say the last words and the exit:\n%s", got)
	}
	// enter tries again.
	_, seen = run(t, h, press("enter")...)
	offs := loginWaitOf[handOffMsg](seen)
	if len(offs) != 1 || len(*ran) != 2 {
		t.Fatalf("enter after a failure did not hand the terminal over again: %v, ran %q", seen, *ran)
	}
	// One a signal ended says the signal, not an exit of -1.
	if runtime.GOOS != "windows" {
		sleeper := exec.Command(fakecli.Install(t, t.TempDir(), "sleeper", fakecli.Spec{Sleep: "30s"}))
		if err := sleeper.Start(); err != nil {
			t.Fatal(err)
		}
		sleeper.Process.Kill()
		run(t, h, offs[0].Done(sleeper.Wait()))
		if got := plain(h); !strings.Contains(got, "rota login 6 ended with signal: killed") {
			t.Errorf("a hand-off a signal ended:\n%s", got)
		}
	}

	// A rota that cannot be started says why.
	e = loginWaitEnv(t, &fakeBackend{}, &fakeTicks{}, func(o *Options) {
		o.Exec = func(args ...string) *exec.Cmd { return exec.Command(filepath.Join(t.TempDir(), "gone"), args...) }
	})
	h = &loginWaitHost{s: newDelegatedStep(e, 6), width: 80}
	_, seen = run(t, h, press("enter")...)
	back, _ = loginWaitHandOff(t, seen)
	run(t, h, back)
	if got := plain(h); !strings.Contains(got, "could not run") {
		t.Errorf("a rota that never started:\n%s", got)
	}
	// Nor is a hand-off of nothing taken for a success.
	e = loginWaitEnv(t, &fakeBackend{}, &fakeTicks{}, func(o *Options) { o.Exec = func(...string) *exec.Cmd { return nil } })
	h = &loginWaitHost{s: newDelegatedStep(e, 6), width: 80}
	_, seen = run(t, h, press("enter")...)
	offs = loginWaitOf[handOffMsg](seen)
	if len(offs) != 1 || offs[0].Cmd != nil {
		t.Fatalf("no command gave %+v", offs)
	}
	_, seen = run(t, h, offs[0].Done(errors.New("there was nothing to hand the terminal to")))
	if _, done := loginWaitDone(t, seen); done || !strings.Contains(plain(h), "nothing to hand") {
		t.Errorf("a hand-off of nothing:\n%s", plain(h))
	}
}

func TestOverSSHTheCommandIsShownAndCopied(t *testing.T) {
	e := loginWaitEnv(t, &fakeBackend{}, &fakeTicks{}, func(o *Options) {
		o.Remote, o.Host = true, "box\x1b[2J"
	})
	e.list = Listing{Accounts: []wire.Account{{ID: 6, Provider: "grok"}}}
	h := &loginWaitHost{s: newDelegatedStep(e, 6), width: 80}
	view := h.s.View(80)
	if got := ansi.Strip(view); !strings.Contains(got, "grok signs this account in itself; on box, run: rota login 6") ||
		!strings.Contains(got, "c copy the command") || strings.Contains(view, "\x1b[2J") {
		t.Errorf("over SSH the step says:\n%q", view)
	}
	_, seen := run(t, h, press("enter")...)
	if len(seen) != 0 {
		t.Errorf("enter over SSH gave %v", seen)
	}
	_, seen = run(t, h, press("c")...)
	if got := loginWaitOf[clipboardMsg](seen); !slices.Equal(got, []clipboardMsg{{Text: "rota login 6"}}) {
		t.Errorf("c copied %v", got)
	}
	if keys := h.s.Keys(); len(keys) != 1 || keys[0].Help().Key != "c" {
		t.Errorf("the keys are %v", keys)
	}
	// With no host name, the host is still where it runs.
	e.o.Host = ""
	if got := plain(h); !strings.Contains(got, "; on the host, run: rota login 6") {
		t.Errorf("with no host name the step says:\n%s", got)
	}
}

// Every line a step draws fits the width, whatever the server or the
// person's terminal sent, and nothing in it is a command to the terminal.
func TestTheWaitStepsFitAndShowNoControls(t *testing.T) {
	ticks := &fakeTicks{}
	hostile := "\x1b]0;x\a\x1b[2J" + strings.Repeat("長い", 50)
	b := &fakeBackend{FinishLoginFn: loginWaitFinishes(func() (LoginEnd, error) {
		return LoginEnd{}, &client.Refusal{Status: 502, Message: hostile}
	})}
	run0, _ := loginWaitRota(t, fakecli.Spec{Stderr: []string{hostile}, Exit: 1})
	l := loginWaitLogin(ticks)
	// The code arrives escaped, as a URL carries it, and is drawn decoded.
	l.URL = "https://auth.example.com/" + strings.Repeat("x", 200) + "?user_code=" + url.QueryEscape(hostile)
	steps := func(remote bool) []*loginWaitHost {
		e := loginWaitEnv(t, b, ticks, func(o *Options) {
			o.Host = hostile
			o.OpenURL = func(string) error { return nil }
			if !remote {
				o.Exec = run0
			}
		})
		e.list = Listing{Accounts: []wire.Account{{ID: 6, Provider: hostile}}}
		// And a URL with raw controls in it, which no parse takes a code
		// from, drawn cleaned all the same.
		raw := *l
		raw.URL = "https://auth.example.com/" + hostile
		return []*loginWaitHost{
			{s: newDeviceStep(e, l, hostile)},
			{s: newDeviceStep(e, &raw, hostile)},
			{s: newDelegatedStep(e, 6)},
		}
	}
	all := append(steps(false), steps(true)...)
	for _, h := range all {
		h.width = 80
		_, seen := run(t, h, tea.BatchMsg{h.Init()}, press("enter")[0])
		if offs := loginWaitOf[handOffMsg](seen); len(offs) == 1 {
			back, _ := loginWaitHandOff(t, seen)
			run(t, h, back)
		}
	}
	for _, h := range all {
		// Each draws the hostile text, cleaned; and from a wide character's
		// own width up, nothing wider than it is given (a narrower width
		// still takes the character whole, on a line of its own).
		if got := ansi.Strip(h.s.View(120)); strings.Count(got, "長い長い") < 2 {
			t.Errorf("%T does not draw what it was sent:\n%s", h.s, got)
		}
		for _, w := range []int{2, 12, 60, 80, 120} {
			view := h.s.View(w)
			for _, bad := range []string{"\x1b]", "\x1b[2J", "\a"} {
				if strings.Contains(view, bad) {
					t.Errorf("%T at %d draws %q", h.s, w, bad)
				}
			}
			for _, line := range strings.Split(ansi.Strip(view), "\n") {
				if n := textWidth(line); n > w {
					t.Errorf("%T at %d draws a line %d wide: %q", h.s, w, n, line)
				}
				if strings.ContainsFunc(line, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
					t.Errorf("%T at %d draws a control: %q", h.s, w, line)
				}
			}
		}
	}
	for _, h := range all {
		if got := h.s.View(0); got != "" {
			t.Errorf("%T drew %q into no width", h.s, got)
		}
	}
}

// A device step with no login ends at once, rather than asking about none.
func TestADeviceStepWithNoLoginEnds(t *testing.T) {
	ticks := &fakeTicks{}
	b := &fakeBackend{}
	h := &loginWaitHost{s: newDeviceStep(loginWaitEnv(t, b, ticks), nil, "w"), width: 80}
	_, seen := run(t, h, tea.BatchMsg{h.Init()})
	if d, done := loginWaitDone(t, seen); !done || d.Err == nil || b.count() != 0 {
		t.Errorf("no login gave %+v (done %v), calls %q", d, done, b.calls())
	}
}

func TestWaitUpdateDoesNoIO(t *testing.T) {
	ticks := &fakeTicks{}
	b := &fakeBackend{FinishLoginFn: loginWaitFinishes(loginWaitPending)}
	var mu sync.Mutex
	execs, opens := 0, 0
	counts := func() (int, int) { mu.Lock(); defer mu.Unlock(); return execs, opens }
	run0, _ := loginWaitRota(t, fakecli.Spec{})
	e := loginWaitEnv(t, b, ticks, func(o *Options) {
		o.Exec = func(args ...string) *exec.Cmd { mu.Lock(); execs++; mu.Unlock(); return run0(args...) }
		o.OpenURL = func(string) error { mu.Lock(); opens++; mu.Unlock(); return nil }
	})
	ssh := loginWaitEnv(t, b, ticks, func(o *Options) { o.Host = "box" })
	steps := []step{newDeviceStep(e, loginWaitLogin(ticks), "w"), newDelegatedStep(e, 6), newDelegatedStep(ssh, 6)}
	var cmds []tea.Cmd
	updates := func(msgs []tea.Msg) {
		t.Helper()
		assertNoIO(t, b, func() {
			for _, s := range steps {
				if i, ok := s.(interface{ Init() tea.Cmd }); ok {
					cmds = append(cmds, i.Init())
				}
				for _, msg := range msgs {
					_, cmd := s.Update(msg)
					cmds = append(cmds, cmd)
					s.View(80)
				}
			}
		})
		if x, o := counts(); x != 0 || o != 0 {
			t.Errorf("Update ran rota %d times and opened %d pages", x, o)
		}
	}
	updates([]tea.Msg{daemonMsg{}, size(80, 24), press("o")[0], press("c")[0], press("enter")[0],
		tea.PasteMsg{Content: "x"}, refreshMsg{}, stepDone{}, press("esc")[0]})
	// The commands do the work, and what they say back is taken with none
	// again: the answers, the timers, and the terminal coming back.
	var back []tea.Msg
	for _, cmd := range cmds {
		back = append(back, loginWaitExpand(cmd)...)
	}
	back = append(back, ticks.advance(loginWaitEvery)...)
	for _, msg := range back {
		if h, ok := msg.(handOffMsg); ok && h.Done != nil {
			back = append(back, h.Done(errors.New("exit status 3")), h.Done(nil))
		}
	}
	if x, o := counts(); len(b.named("FinishLogin")) == 0 || x == 0 || o == 0 {
		t.Fatalf("the commands asked %q, ran rota %d times and opened %d pages", b.calls(), x, o)
	}
	mu.Lock()
	execs, opens = 0, 0
	mu.Unlock()
	cmds = nil
	updates(back)
}

// loginWaitExpand runs cmd, and every command of a batch it gives, and
// returns what they said.
func loginWaitExpand(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, loginWaitExpand(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}
