package rota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// startEcho opens a session on an echoing fake CLI, which answers every
// message it is sent and reports how many are still waiting.
func startEcho(t *testing.T, ctx context.Context, name string, fake fakecli.Spec, spec Spec, events io.Writer) *Session {
	t.Helper()
	fake.Echo = true
	bin := fakecli.Install(t, t.TempDir(), "claude", fake)
	Register(&fakeProvider{name: name, launched: &Command{Bin: bin}})
	a := &Account{ID: 1, Provider: name}
	a.Token.Access = "tok"
	spec.Input, spec.Stream, spec.flavorOverride = true, true, "claude"
	s, err := Start(ctx, a, "", nil, spec, nil, events)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// nextNotice takes the next notice, optionally skipping the idle ones, and
// fails rather than hanging when none arrives.
func nextNotice(t *testing.T, s *Session, skipIdle bool) Notice {
	t.Helper()
	for {
		select {
		case n, ok := <-s.Notices():
			if !ok {
				t.Fatal("the session ended before the notice the test was waiting for")
			}
			if skipIdle && n.Kind == "idle" {
				continue
			}
			return n
		case <-time.After(20 * time.Second):
			t.Fatal("no notice arrived")
			return Notice{}
		}
	}
}

// resultsIn is the answer of every result event in a CLI's output, in order.
func resultsIn(t *testing.T, out string) []string {
	t.Helper()
	var answers []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var e struct {
			Type   string `json:"type"`
			Result string `json:"result"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Type == "result" {
			answers = append(answers, e.Result)
		}
	}
	return answers
}

func kinds(ns []Notice) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Kind
	}
	return out
}

// drain collects what is left on the notices channel, which is closed when
// the session ends.
func drain(s *Session) []Notice {
	var all []Notice
	for n := range s.Notices() {
		all = append(all, n)
	}
	return all
}

func has(ns []Notice, kind, id string) bool {
	return slices.ContainsFunc(ns, func(n Notice) bool { return n.Kind == kind && n.ID == id })
}

// A session is one CLI process answering several messages: the prompt it was
// started with and everything sent afterwards, all in the same run, with one
// result each and one result for the whole.
func TestASessionTakesMoreMessagesIntoOneRun(t *testing.T) {
	var out bytes.Buffer // written by the reader, read after Wait
	s := startEcho(t, context.Background(), "t-session-one", fakecli.Spec{}, Spec{Prompt: "one"}, &out)

	two, err := s.Send("two")
	if err != nil {
		t.Fatal(err)
	}
	three, err := s.Send("three")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := s.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := resultsIn(t, out.String()); !slices.Equal(got, []string{"echo: one", "echo: two", "echo: three"}) {
		t.Fatalf("one process answered each message in turn: %v", got)
	}
	if res.Result != "echo: three" || res.ExitCode != 0 {
		t.Fatalf("the run's result is its last answer: %+v", res)
	}
	all := drain(s)
	for _, id := range []string{two, three} {
		if !has(all, "accepted", id) || !has(all, "answered", id) {
			t.Fatalf("every message is accepted and then answered: %q in %v", id, all)
		}
	}
	if last := all[len(all)-1]; last.Kind != "idle" {
		t.Fatalf("a session with nothing pending ends idle: %v", kinds(all))
	}
}

// Delivery is read from the count the CLI reports, not guessed from the
// order of the lines: while messages are still queued, none of them has been
// answered, and each result hands back exactly the ones it got through.
func TestAnsweredFollowsQueuedTurnCount(t *testing.T) {
	// The fake holds its first turn until both messages are in, which is the
	// situation this is about: two messages queued behind a running turn. The
	// turn time is not what puts them there — the hold is — it only spaces
	// the turns out, so a count can be read between two of them.
	s := startEcho(t, context.Background(), "t-session-queued",
		fakecli.Spec{EchoHold: 2, Sleep: "300ms"}, Spec{Prompt: "slow"}, nil)
	defer s.Close()

	a, err := s.Send("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Send("b")
	if err != nil {
		t.Fatal(err)
	}
	if n := nextNotice(t, s, true); n.Kind != "accepted" || n.ID != a {
		t.Fatalf("first accepted is a: %+v", n)
	}
	if n := nextNotice(t, s, true); n.Kind != "accepted" || n.ID != b {
		t.Fatalf("second accepted is b: %+v", n)
	}
	// The prompt's own turn ends while both are queued, and a queued message
	// is not an answered one.
	if got := s.Pending(); got != 2 {
		t.Fatalf("two messages are waiting: %d", got)
	}
	if n := nextNotice(t, s, true); n.Kind != "answered" || n.ID != a {
		t.Fatalf("the first queued message is answered first: %+v", n)
	}
	if got := s.Pending(); got != 1 {
		t.Fatalf("one message is still waiting: %d", got)
	}
	if n := nextNotice(t, s, true); n.Kind != "answered" || n.ID != b {
		t.Fatalf("then the second: %+v", n)
	}
	if got := s.Pending(); got != 0 {
		t.Fatalf("nothing is waiting: %d", got)
	}
}

// An interrupt is acknowledged by the CLI whatever it was doing, and steering
// is that interrupt followed by a message, which then gets its own turn.
func TestInterruptIsAcknowledgedAndSteerStartsTheNextTurn(t *testing.T) {
	s := startEcho(t, context.Background(), "t-session-steer",
		fakecli.Spec{Sleep: "100ms"}, Spec{Prompt: "work"}, nil)
	defer s.Close()

	first, err := s.Interrupt()
	if err != nil {
		t.Fatal(err)
	}
	for {
		n := nextNotice(t, s, true)
		if n.Kind == "interrupted" {
			if n.ID != first {
				t.Fatalf("the acknowledgement carries the interrupt's own id: %+v, want %q", n, first)
			}
			break
		}
	}

	msg, err := s.Steer("now")
	if err != nil {
		t.Fatal(err)
	}
	// Steering interrupts under a fresh id and sends; the acknowledgement and
	// the accepted notice come from different ends of the run, so what is
	// pinned here is that all three arrive and that the message is answered
	// after it is accepted.
	var steerID string
	accepted := false
	for range 6 {
		n := nextNotice(t, s, true)
		switch {
		case n.Kind == "interrupted" && n.ID != first:
			steerID = n.ID
		case n.Kind == "accepted" && n.ID == msg:
			accepted = true
		case n.Kind == "answered" && n.ID == msg:
			if !accepted {
				t.Fatal("a message is accepted before it is answered")
			}
			if steerID == "" {
				t.Fatal("steering interrupts before it sends")
			}
			return
		}
	}
	t.Fatalf("the steered message was never answered (interrupt %q, accepted %v)", steerID, accepted)
}

// What a session cannot do it refuses by name, before anything is written.
func TestASessionRefusesWhatItCannotDo(t *testing.T) {
	ctx := context.Background()
	s := startEcho(t, ctx, "t-session-refuse", fakecli.Spec{}, Spec{Prompt: "p"}, nil)
	if _, err := s.Send(""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("an empty message is nothing to send: %v", err)
	}
	if _, err := s.Send(strings.Repeat("x", 65<<10)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a message past the cap is refused: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send("late"); !errors.Is(err, ErrClosed) {
		t.Fatalf("a closed session takes nothing more: %v", err)
	}
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}

	bin := fakecli.Install(t, t.TempDir(), "claude", fakecli.Spec{Echo: true})
	Register(&fakeProvider{name: "t-session-refuse-2", launched: &Command{Bin: bin}})
	a := &Account{ID: 1, Provider: "t-session-refuse-2"}
	a.Token.Access = "tok"

	_, err := Start(ctx, a, "", nil, Spec{Prompt: "p", Input: true, flavorOverride: "claude"}, nil, nil)
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "stream") {
		t.Fatalf("a session is a stream and must say so: %v", err)
	}
	_, err = Run(ctx, a, "", nil, Spec{Prompt: "p", Input: true, Stream: true, flavorOverride: "claude"}, nil, nil)
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "Start") {
		t.Fatalf("a one-shot run must name what to call instead: %v", err)
	}
	_, err = Start(ctx, a, "", nil, Spec{Prompt: "p", Input: true, Stream: true, flavorOverride: "codex"}, nil, nil)
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "input") {
		t.Fatalf("only claude has a streaming input, and the field is refused by name: %v", err)
	}
}

// Cancelling the context ends the session where it stands, however long the
// CLI meant to take.
func TestCancellingTheContextEndsTheSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := startEcho(t, ctx, "t-session-cancel", fakecli.Spec{Sleep: "10s"}, Spec{Prompt: "slow"}, nil)
	cancel()
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled session must end, not wait out the CLI")
	}
	if _, err := s.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("the verdict is the cancellation: %v", err)
	}
}

// A caller that keeps sending into an agent that is not keeping up is told
// so, rather than queueing without limit.
func TestTooManyPendingIsRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := startEcho(t, ctx, "t-session-busy", fakecli.Spec{Sleep: "10s"}, Spec{Prompt: "slow"}, nil)
	for i := range maxPending {
		if _, err := s.Send("m"); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	if _, err := s.Send("one too many"); !errors.Is(err, ErrBusy) {
		t.Fatalf("past the limit a send is refused: %v", err)
	}
	cancel()
	<-s.Done()
}

// The prompt travels as a JSON message like every message after it, so a
// quote, a newline or a character outside ASCII reaches the CLI as itself.
func TestTheFirstPromptTravelsAsAJSONLine(t *testing.T) {
	const prompt = "say \"hello\"\nover two lines — ünïcödé 日本語"
	var out bytes.Buffer
	s := startEcho(t, context.Background(), "t-session-quote", fakecli.Spec{}, Spec{Prompt: prompt}, &out)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	got := resultsIn(t, out.String())
	if len(got) != 1 || got[0] != "echo: "+prompt {
		t.Fatalf("the prompt must arrive as written: %q", got)
	}
}

// A session records its command line like any run, and the flag that makes it
// a session is in it.
func TestASessionRecordsItsInputFormat(t *testing.T) {
	s := startEcho(t, context.Background(), "t-session-argv", fakecli.Spec{},
		Spec{Prompt: "p", IncludeArgv: true}, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := s.Wait()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(res.Argv, "--input-format")
	if i < 0 || i+1 >= len(res.Argv) || res.Argv[i+1] != "stream-json" {
		t.Fatalf("a session is launched with the streaming input format: %v", res.Argv)
	}
}
