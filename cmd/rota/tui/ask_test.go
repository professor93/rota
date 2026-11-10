package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/wire"
)

// askListing is the design's three accounts: two claude accounts and a
// codex one that is dead. Account 1 has a folder of its own; the rotation
// would spend it next.
func askListing() Listing {
	return Listing{Default: 1, Accounts: []wire.Account{
		{ID: 1, Provider: "claude", Email: "work@example.com", Order: 1, Threshold: 75, Cwd: "/srv/work"},
		{ID: 2, Provider: "claude", Email: "home@example.com", Order: 2},
		{ID: 3, Provider: "codex", Email: "cdx@example.com", Order: 3, Status: "reauth"},
	}}
}

// askModel is a model with the Ask tab alone, over b, which lists
// askListing unless the test set its own, started at 80×24.
func askModel(t *testing.T, b *fakeBackend, with ...func(*Options)) *Model {
	t.Helper()
	shellRegistry(t)
	register(tabsAsk, askNew)
	if b.AccountsFn == nil {
		b.AccountsFn = func(context.Context, Refresh) (Listing, error) { return askListing(), nil }
	}
	m := shellModel(t, b, &fakeTicks{}, with...)
	shellStart(t, m)
	return m
}

// askOf is the model's Ask tab.
func askOf(t *testing.T, m *Model) *askTab {
	t.Helper()
	for _, tb := range m.tabs {
		if a, ok := tb.(*askTab); ok {
			return a
		}
	}
	t.Fatal("no Ask tab")
	return nil
}

// askView is the tab's own view as text, at w×h.
func askView(t *testing.T, m *Model, w, h int) string {
	t.Helper()
	return ansi.Strip(askOf(t, m).View(w, h))
}

// askAsk writes text into the prompt, as a paste, and sends it.
func askAsk(text string) []tea.Msg {
	return []tea.Msg{tea.PasteMsg{Content: text}, press("enter")[0]}
}

// askSettle waits for the answer in flight to end.
func askSettle(t *testing.T, m *Model) {
	t.Helper()
	for i := 0; askOf(t, m).run != nil; i++ {
		if i == 20 {
			t.Fatal("the answer did not end")
		}
		await(t, m)
	}
}

// askQuestions is every Question the backend was asked.
func askQuestions(b *fakeBackend) []Question {
	var qs []Question
	for _, c := range b.named("Ask") {
		qs = append(qs, c.Args[0].(Question))
	}
	return qs
}

// askLine is the line of view that holds s, or -1.
func askLine(view, s string) int {
	for i, l := range strings.Split(view, "\n") {
		if strings.Contains(l, s) {
			return i
		}
	}
	return -1
}

func TestAskStreamsTheAnswer(t *testing.T) {
	b := &fakeBackend{AskFn: func(_ context.Context, q Question, each func(message.Event)) (wire.End, error) {
		each(message.Event{Type: "init", Account: 1})
		each(message.Event{Type: "text", Text: "fo", Delta: true})
		each(message.Event{Type: "text", Text: "ur", Delta: true})
		each(message.Event{Type: "text", Text: "four"})
		each(message.Event{Type: "tool", Tool: "Bash"})
		each(message.Event{Type: "thinking", Text: "hidden thoughts"})
		return wire.End{Type: "done", Account: 1, DurationMS: 2300, CostUSD: 0.0123}, nil
	}}
	m := askModel(t, b)
	run(t, m, askAsk("2+2?")...)
	askSettle(t, m)
	view := askView(t, m, 80, 20)
	if n := strings.Count(view, "four"); n != 1 {
		t.Errorf("four is shown %d times, want once:\n%s", n, view)
	}
	if !strings.Contains(view, "· tool Bash") || strings.Contains(view, "hidden thoughts") {
		t.Errorf("want the tool line and no thinking:\n%s", view)
	}
	if !strings.Contains(view, "#1 · 2.3s · $0.0123 · exit 0") {
		t.Errorf("no end line:\n%s", view)
	}
	if strings.Contains(view, "{}") {
		t.Errorf("no reading was asked for, and an empty one is drawn:\n%s", view)
	}
	if !strings.Contains(view, "> 2+2?") {
		t.Errorf("the question is not above its answer:\n%s", view)
	}
	if four, tool, end := askLine(view, "four"), askLine(view, "· tool Bash"), askLine(view, "#1 ·"); !(four < tool && tool < end) {
		t.Errorf("the answer, the tool and the end are out of order (%d, %d, %d):\n%s", four, tool, end, view)
	}
	if qs := askQuestions(b); len(qs) != 1 || qs[0].Prompt != "2+2?" || qs[0].Input || len(qs[0].With) != 0 {
		t.Errorf("asked %+v", qs)
	}
	if busy := askOf(t, m).Busy(); busy != "" {
		t.Errorf("Busy after the end is %q", busy)
	}
	// A whole piece the fragments did not add up to is shown, as the CLI
	// shows it, and a later answer replaces this one.
	b.AskFn = func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		each(message.Event{Type: "text", Text: "thr", Delta: true})
		each(message.Event{Type: "text", Text: "three"})
		return wire.End{Type: "done", Account: 2, ExitCode: 3}, nil
	}
	run(t, m, askAsk("1+2?")...)
	askSettle(t, m)
	view = askView(t, m, 80, 20)
	if !strings.Contains(view, "thrthree") || strings.Contains(view, "four") || !strings.Contains(view, "#2 · exit 3") {
		t.Errorf("the second answer:\n%s", view)
	}
}

func TestAskPicksAutoOrAnAccount(t *testing.T) {
	b := &fakeBackend{AskFn: func(_ context.Context, q Question, _ func(message.Event)) (wire.End, error) {
		if q.Account == 3 {
			return wire.End{}, &client.Refusal{Status: 409, Reason: "busy", Message: "account 3 is busy:\x1b[2J another codex run holds it"}
		}
		return wire.End{Type: "done", Account: max(q.Account, 1)}, nil
	}}
	m := askModel(t, b, func(o *Options) { o.Cwd = "/home/p" })
	bottom := func() string {
		lines := strings.Split(askView(t, m, 120, 20), "\n")
		return lines[len(lines)-1]
	}
	if got := bottom(); !strings.Contains(got, "account: auto (now #1 work@example.com)") {
		t.Errorf("auto does not name the account the rotation would spend: %q", got)
	}
	// Auto asks the rotation, in the folder of the account it would spend
	// (R25).
	run(t, m, askAsk("one")...)
	askSettle(t, m)
	// tab leaves the prompt for the account; → goes through every account
	// in listing order and back to auto, ← the other way.
	run(t, m, press("tab")...)
	want := []string{"account: #1 work@example.com", "account: #2 home@example.com", "account: #3 cdx@example.com", "account: auto (now #1"}
	for _, w := range want {
		run(t, m, press("right")...)
		if got := bottom(); !strings.Contains(got, w) {
			t.Errorf("after → the line is %q, want %q", got, w)
		}
	}
	run(t, m, press("left")...)
	if got := bottom(); !strings.Contains(got, "account: #3 cdx@example.com") {
		t.Errorf("← from auto is %q, want the last account", got)
	}
	run(t, m, press("left", "left")...)
	run(t, m, askAsk("two")...)
	askSettle(t, m)
	run(t, m, press("tab", "right")...)
	run(t, m, askAsk("three")...)
	askSettle(t, m)
	got := askQuestions(b)
	wantQ := []Question{
		{Account: 0, Prompt: "one", Cwd: "/srv/work"},
		{Account: 1, Prompt: "two", Cwd: "/srv/work"},
		{Account: 2, Prompt: "three", Cwd: "/home/p"},
	}
	if len(got) != len(wantQ) {
		t.Fatalf("asked %+v", got)
	}
	for i := range wantQ {
		if got[i].Account != wantQ[i].Account || got[i].Prompt != wantQ[i].Prompt || got[i].Cwd != wantQ[i].Cwd {
			t.Errorf("question %d is %+v, want %+v", i, got[i], wantQ[i])
		}
	}
	// A refusal is the server's own words, under the prompt, cleaned, and
	// the question is back in the field to send elsewhere.
	run(t, m, press("tab", "right")...)
	run(t, m, askAsk("four")...)
	askSettle(t, m)
	view := askView(t, m, 120, 20)
	refusal := askLine(view, "account 3 is busy:[2J another codex run holds it")
	if refusal < 0 || strings.Contains(view, "\x1b") {
		t.Fatalf("the refusal is not shown cleaned:\n%q", view)
	}
	if prompt := askLine(view, "> four"); prompt < 0 || prompt > refusal || refusal != len(strings.Split(view, "\n"))-2 {
		t.Errorf("want the question back in the field and the refusal under it:\n%s", view)
	}
}

func TestReadingsAreToggledAndShownAsJSON(t *testing.T) {
	b := &fakeBackend{AskFn: func(_ context.Context, q Question, each func(message.Event)) (wire.End, error) {
		each(message.Event{Type: "text", Text: "ok"})
		end := wire.End{Type: "done", Account: 1}
		end.Tools = map[string]int{"Bash": 1}
		return end, nil
	}}
	m := askModel(t, b)
	// The readings are message.Names, all off.
	if line := askView(t, m, 200, 20); !strings.Contains(line, "with: [ ] blocks [ ] ask [ ] raw") {
		t.Errorf("the readings line:\n%s", line)
	}
	// shift+tab from the prompt reaches the readings; space toggles the one
	// under the cursor, → and ← move it.
	run(t, m, press("shift+tab", "space")...)
	for range slices.Index(message.Names, "tools") {
		run(t, m, press("right")...)
	}
	run(t, m, press("space", "left", "space", "space")...)
	view := askView(t, m, 200, 20)
	if !strings.Contains(view, "[x] blocks") || !strings.Contains(view, "[x] tools") || !strings.Contains(view, "[ ] stats") || !strings.Contains(view, "[ ] quota") {
		t.Errorf("toggled:\n%s", view)
	}
	run(t, m, askAsk("hi")...)
	askSettle(t, m)
	if qs := askQuestions(b); len(qs) != 1 || !slices.Equal(qs[0].With, []string{"blocks", "tools"}) {
		t.Errorf("asked %+v", qs)
	}
	view = askView(t, m, 80, 24)
	want := "{\n  \"tools\": {\n    \"Bash\": 1\n  }\n}"
	if !strings.Contains(view, want) {
		t.Errorf("the readings are not indented JSON under the end:\n%s", view)
	}
	if askLine(view, "\"tools\"") < askLine(view, "#1 · exit 0") {
		t.Errorf("the readings are above the end line:\n%s", view)
	}
}

func TestAKeptOpenRunTakesMoreMessages(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once
	b := &fakeBackend{
		AskFn: func(ctx context.Context, q Question, each func(message.Event)) (wire.End, error) {
			each(message.Event{Type: "init", RunID: "r-1", Account: 1})
			each(message.Event{Type: "text", Text: "hello"})
			each(message.Event{Type: "idle"})
			select {
			case <-closed:
			case <-ctx.Done():
				return wire.End{}, ctx.Err()
			}
			return wire.End{Type: "done", Account: 1, NumTurns: 2}, nil
		},
		SendRunFn: func(_ context.Context, _, text string) error {
			if text == "nope" {
				return &client.Refusal{Status: 409, Message: "the run takes no more messages now"}
			}
			return nil
		},
		InterruptRunFn: func(context.Context, string) error { return nil },
		CloseRunFn: func(context.Context, string) error {
			once.Do(func() { close(closed) })
			return nil
		},
	}
	m := askModel(t, b)
	a := askOf(t, m)
	if !strings.Contains(askView(t, m, 80, 20), "keep open: off") {
		t.Fatal("keep open does not start off")
	}
	run(t, m, press("ctrl+s")...)
	if !strings.Contains(askView(t, m, 80, 20), "keep open: on") {
		t.Error("ctrl+s does not keep the next run open")
	}
	run(t, m, askAsk("hi")...)
	if qs := askQuestions(b); len(qs) != 1 || !qs[0].Input {
		t.Fatalf("asked %+v", qs)
	}
	if a.Busy() != "the run kept open" || !strings.Contains(askView(t, m, 80, 20), "run open") {
		t.Errorf("Busy is %q:\n%s", a.Busy(), askView(t, m, 80, 20))
	}
	// The run has answered and is idle: esc leaves the field, and
	// interrupts nothing.
	run(t, m, press("esc")...)
	if a.Typing() || len(b.named("InterruptRun")) != 0 {
		t.Errorf("esc on an idle run: typing %v, %q", a.Typing(), b.calls())
	}
	run(t, m, askAsk("again")...)
	if got := b.calls(); !slices.Contains(got, "SendRun(r-1, again)") || len(askQuestions(b)) != 1 {
		t.Fatalf("calls %q", got)
	}
	if view := askView(t, m, 80, 20); askLine(view, "> again") < askLine(view, "hello") {
		t.Errorf("the second message is not under the first answer:\n%s", view)
	}
	// While the run works on a message, esc interrupts it rather than
	// leaving the field.
	run(t, m, press("esc")...)
	if got := b.named("InterruptRun"); len(got) != 1 || got[0].Args[0] != "r-1" || !a.Typing() {
		t.Errorf("esc while answering: InterruptRun %v, typing %v", got, a.Typing())
	}
	// A message the run will not take is said in the server's words, and
	// goes back to the field.
	run(t, m, askAsk("nope")...)
	// Shown once: in the field, not in the conversation as if it was sent.
	if view := askView(t, m, 80, 20); askLine(view, "the run takes no more messages now") < 0 || a.area.Value() != "nope" || strings.Count(view, "> nope") != 1 {
		t.Errorf("the refused message: field %q\n%s", a.area.Value(), view)
	}
	// A message not taken leaves nothing being answered: esc leaves the
	// field.
	run(t, m, press("esc")...)
	if a.Typing() || len(b.named("InterruptRun")) != 1 {
		t.Errorf("esc after a refused send: typing %v, %q", a.Typing(), b.calls())
	}
	run(t, m, press("ctrl+x")...)
	if got := b.named("CloseRun"); len(got) != 1 || got[0].Args[0] != "r-1" {
		t.Errorf("CloseRun calls %v", got)
	}
	askSettle(t, m)
	if a.Busy() != "" || !strings.Contains(askView(t, m, 80, 20), "#1 · exit 0") {
		t.Errorf("after closing, Busy is %q:\n%s", a.Busy(), askView(t, m, 80, 20))
	}
	// Keep-open stays as set, for the next question.
	if !strings.Contains(askView(t, m, 80, 20), "keep open: on") {
		t.Error("keep open was forgotten")
	}
}

func TestEscStopsAnAnswer(t *testing.T) {
	saw := make(chan error, 1)
	b := &fakeBackend{AskFn: func(ctx context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		each(message.Event{Type: "text", Text: "the start of it", Delta: true})
		<-ctx.Done()
		saw <- ctx.Err()
		return wire.End{}, ctx.Err()
	}}
	m := askModel(t, b)
	a := askOf(t, m)
	run(t, m, askAsk("long one")...)
	if a.Busy() != "the open answer" {
		t.Fatalf("Busy is %q", a.Busy())
	}
	run(t, m, press("esc")...)
	askSettle(t, m)
	select {
	case err := <-saw:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the backend's context ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the backend never saw its context end")
	}
	view := askView(t, m, 80, 20)
	if !strings.Contains(view, "the start of it") || !strings.Contains(view, "· stopped") || a.Busy() != "" {
		t.Errorf("Busy %q:\n%s", a.Busy(), view)
	}
	// With nothing answering, esc leaves the field, as R6 says.
	if !a.Typing() {
		t.Fatal("the prompt lost focus when the answer was stopped")
	}
	run(t, m, press("esc")...)
	if a.Typing() {
		t.Error("esc with nothing answering does not leave the field")
	}
}

func TestLeavingClosesAnOpenRun(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once
	b := &fakeBackend{
		AskFn: func(ctx context.Context, _ Question, each func(message.Event)) (wire.End, error) {
			each(message.Event{Type: "init", RunID: "r-9"})
			each(message.Event{Type: "idle"})
			select {
			case <-closed:
				return wire.End{Type: "done"}, nil
			case <-ctx.Done():
				return wire.End{}, ctx.Err()
			}
		},
		InterruptRunFn: func(context.Context, string) error { return nil },
		CloseRunFn: func(context.Context, string) error {
			once.Do(func() { close(closed) })
			return nil
		},
	}
	m := askModel(t, b)
	run(t, m, press("ctrl+s")...)
	run(t, m, askAsk("stay")...)
	if err := programLeave(m.leavers()); err != nil {
		t.Fatal(err)
	}
	// Interrupted, then closed, as the server shuts a run nobody reads, so
	// no turn goes on for nobody (M2).
	if got := askShutCalls(b); !slices.Equal(got, []string{"InterruptRun(r-9)", "CloseRun(r-9)"}) {
		t.Errorf("leaving sent %q", got)
	}
	askSettle(t, m)
	// A tab with nothing open leaves nothing to close.
	if err := programLeave(m.leavers()); err != nil || len(b.named("CloseRun")) != 1 {
		t.Errorf("leaving again: %v, %v", err, b.named("CloseRun"))
	}
}

func TestACutStreamEndsTheAnswerAndSaysSo(t *testing.T) {
	b := &fakeBackend{AskFn: func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		each(message.Event{Type: "text", Text: "half of the"})
		return wire.End{}, fmt.Errorf("the answer was cut off before the run ended: %w", io.ErrUnexpectedEOF)
	}}
	m := askModel(t, b)
	run(t, m, askAsk("cut")...)
	askSettle(t, m)
	view := askView(t, m, 80, 20)
	part, cut := askLine(view, "half of the"), askLine(view, "the answer was cut off before the run ended: unexpected EOF")
	if part < 0 || cut <= part {
		t.Errorf("want the partial answer, then the error under it (%d, %d):\n%s", part, cut, view)
	}
	if askOf(t, m).Busy() != "" {
		t.Error("a cut answer is still streaming")
	}
	// The keys still work: the next question goes out.
	b.AskFn = func(context.Context, Question, func(message.Event)) (wire.End, error) {
		return wire.End{Type: "done"}, nil
	}
	run(t, m, askAsk("again")...)
	askSettle(t, m)
	if len(askQuestions(b)) != 2 {
		t.Errorf("the next question was not sent: %q", b.calls())
	}
}

func TestWatchCannotAsk(t *testing.T) {
	b := &fakeBackend{WhoFn: func(context.Context) (Principal, error) {
		return Principal{Name: "w", Role: "watch", Via: "ssh"}, nil
	}}
	m := askModel(t, b)
	a := askOf(t, m)
	if view := askView(t, m, 80, 20); !strings.Contains(view, "asking runs an account, which the watch role may not do") {
		t.Errorf("the watch view:\n%s", view)
	}
	k := a.keys()
	for _, binding := range a.Keys() {
		if !binding.Enabled() {
			continue
		}
		if !slices.Equal(binding.Keys(), k.Focus.Keys()) && !slices.Equal(binding.Keys(), k.BackFocus.Keys()) {
			t.Errorf("watch has %q enabled", binding.Keys())
		}
	}
	run(t, m, press("enter", "tab", "tab", "tab", "ctrl+s", "space", "right")...)
	run(t, m, askAsk("x")...)
	if a.Typing() || len(b.named("Ask")) != 0 {
		t.Errorf("watch typed (%v) or asked: %q", a.Typing(), b.calls())
	}
}

func TestAskOverTheRealAPI(t *testing.T) {
	backendHTTPClaude(t, fakecli.Lines(
		`{"type":"system","subtype":"init","session_id":"s-1","model":"m"}`,
		`{"type":"assistant","session_id":"s-1","message":{"content":[{"type":"text","text":"four"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s-1","result":"four","num_turns":1}`))
	r := backendHTTPServe(t, api.RoleControl, seedStore(t, ""))
	shellRegistry(t)
	register(tabsAsk, askNew)
	m := shellModel(t, r.b, &fakeTicks{})
	shellStart(t, m)
	run(t, m, askAsk("2+2?")...)
	askSettle(t, m)
	view := askView(t, m, 80, 20)
	if !strings.Contains(view, "four") || askLine(view, "#1 · ") < askLine(view, "four") {
		t.Errorf("want four, then an end line naming account 1:\n%s", view)
	}
	// What went out is the prompt alone: no Remote Control, no raw flag,
	// and no folder, since neither the account nor the TUI names one.
	if c := r.rec.last(t, "POST", "/v1/run"); c.Body != `{"prompt":"2+2?","stream":true}` {
		t.Errorf("sent %s", c.Body)
	}
}

func TestAskAt80x24(t *testing.T) {
	b := &fakeBackend{AskFn: func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		each(message.Event{Type: "init", Account: 1})
		each(message.Event{Type: "text", Text: "Let me check."})
		each(message.Event{Type: "tool", Tool: "Bash"})
		each(message.Event{Type: "text", Text: "2 + 2 is four.\n\n    four = 2 + 2"})
		end := wire.End{Type: "done", Account: 1, DurationMS: 4210, CostUSD: 0.0371}
		end.Tools = map[string]int{"Bash": 1}
		return end, nil
	}}
	m := askModel(t, b)
	run(t, m, press("shift+tab")...)
	for range slices.Index(message.Names, "tools") {
		run(t, m, press("right")...)
	}
	run(t, m, press("space")...)
	run(t, m, askAsk("what is 2+2? check it in a shell")...)
	askSettle(t, m)
	golden(t, "ask-80x24", askView(t, m, 80, 24))
}

func TestAskUpdateDoesNoIO(t *testing.T) {
	b := &fakeBackend{
		AskFn:          func(context.Context, Question, func(message.Event)) (wire.End, error) { return wire.End{}, nil },
		SendRunFn:      func(context.Context, string, string) error { return nil },
		InterruptRunFn: func(context.Context, string) error { return nil },
		CloseRunFn:     func(context.Context, string) error { return nil },
	}
	m := askModel(t, b)
	a := askOf(t, m)
	var msgs []tea.Msg
	msgs = append(msgs, press("ctrl+s", "enter")...)
	msgs = append(msgs, tea.PasteMsg{Content: "a\nb"}, press("alt+enter")[0], press("enter")[0])
	// What the stream would say, for the run just asked for.
	id := 1
	msgs = append(msgs,
		askEventMsg{id: id, event: message.Event{Type: "init", RunID: "r-1"}},
		askEventMsg{id: id, event: message.Event{Type: "text", Text: "x"}},
		askEventMsg{id: id, event: message.Event{Type: "tool", Tool: "Read"}})
	msgs = append(msgs, press("esc")[0], tea.PasteMsg{Content: "more"}, press("enter")[0])
	msgs = append(msgs, askEventMsg{id: id, event: message.Event{Type: "idle"}}, askCallMsg{id: id, err: errors.New("no")})
	msgs = append(msgs, press("pgup", "pgdown", "tab", "right", "left", "tab", "space", "right", "shift+tab", "ctrl+x", "esc")...)
	msgs = append(msgs, tea.MouseWheelMsg{Button: tea.MouseWheelUp}, size(100, 30), listingMsg{Listing: askListing()},
		tea.BackgroundColorMsg{}, askEventMsg{id: id, last: true, end: wire.End{Type: "done"}}, askEventMsg{id: 7, last: true})
	assertNoIO(t, b, func() {
		for _, msg := range msgs {
			m.Update(msg)
			m.View()
			a.View(80, 20)
		}
	})
	if a.run != nil {
		t.Errorf("the run did not end: %+v", a.run)
	}
}

// The answer is a viewport that follows its end, and pgup and pgdown page
// through it.
func TestAskPagesThroughALongAnswer(t *testing.T) {
	var long []string
	for i := range 100 {
		long = append(long, fmt.Sprintf("line %03d", i))
	}
	b := &fakeBackend{AskFn: func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		each(message.Event{Type: "text", Text: strings.Join(long, "\n")})
		return wire.End{Type: "done"}, nil
	}}
	m := askModel(t, b)
	run(t, m, askAsk("count")...)
	askSettle(t, m)
	if view := askView(t, m, 80, 24); !strings.Contains(view, "line 099") || strings.Contains(view, "line 050") {
		t.Fatalf("the view does not follow the end:\n%s", view)
	}
	run(t, m, press("pgup", "pgup")...)
	if view := askView(t, m, 80, 24); strings.Contains(view, "line 099") || !strings.Contains(view, "line 0") {
		t.Errorf("pgup did not page back:\n%s", view)
	}
	run(t, m, press("pgdown", "pgdown", "pgdown")...)
	if view := askView(t, m, 80, 24); !strings.Contains(view, "line 099") {
		t.Errorf("pgdown did not come back to the end:\n%s", view)
	}
}

// The prompt is three rows, grows to six with what is typed, and takes a
// new line on alt+enter, never on enter.
func TestAskThePromptGrowsFromThreeToSix(t *testing.T) {
	m := askModel(t, &fakeBackend{})
	a := askOf(t, m)
	rows := func() int { return a.area.Height() }
	if rows() != 3 {
		t.Errorf("the prompt starts %d rows high", rows())
	}
	run(t, m, tea.PasteMsg{Content: "1\n2\n3\n4\n5\n6\n7\n8"})
	if rows() != 6 || a.area.LineCount() != 8 {
		t.Errorf("eight lines: %d rows for %d lines", rows(), a.area.LineCount())
	}
	run(t, m, press("alt+enter")...)
	if a.area.LineCount() != 9 {
		t.Errorf("alt+enter gave %d lines", a.area.LineCount())
	}
	if len(askView(t, m, 80, 24)) == 0 {
		t.Error("no view")
	}
}

// tab and shift+tab move between the prompt, the account and the
// readings; enter on the prompt at rest takes it up, and q and the digits
// reach the shell everywhere but in the field.
func TestAskFocusMovesAndOnlyTheFieldTypes(t *testing.T) {
	m := askModel(t, &fakeBackend{})
	a := askOf(t, m)
	if a.Typing() || a.focus != askOnPrompt {
		t.Fatal("the tab opens typing, or away from the prompt")
	}
	run(t, m, press("enter")...)
	if !a.Typing() {
		t.Fatal("enter did not take the prompt up")
	}
	run(t, m, press("q", "2")...)
	if a.area.Value() != "q2" {
		t.Errorf("the field holds %q", a.area.Value())
	}
	run(t, m, press("tab")...)
	if a.Typing() || a.focus != askOnAccount {
		t.Errorf("tab: typing %v, focus %v", a.Typing(), a.focus)
	}
	run(t, m, press("tab")...)
	if a.focus != askOnReadings {
		t.Errorf("tab again: focus %v", a.focus)
	}
	run(t, m, press("tab")...)
	if !a.Typing() || a.focus != askOnPrompt {
		t.Errorf("tab around: typing %v, focus %v", a.Typing(), a.focus)
	}
	run(t, m, press("shift+tab")...)
	if a.Typing() || a.focus != askOnReadings {
		t.Errorf("shift+tab: typing %v, focus %v", a.Typing(), a.focus)
	}
	// Every key that works here is in the help, and none is shown that does
	// not.
	for _, b := range a.Keys() {
		if b.Enabled() && b.Help().Key == "" {
			t.Errorf("%q has no help", b.Keys())
		}
	}
	if !askEnabled(a.Keys(), "space") || askEnabled(a.Keys(), "alt+enter") {
		t.Error("on the readings, space should toggle and alt+enter do nothing")
	}
}

// A panic in the backend's stream is said where the answer is, and ends
// only that answer (F31).
func TestAskAPanickingStreamIsSaid(t *testing.T) {
	b := &fakeBackend{AskFn: func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		each(message.Event{Type: "text", Text: "so far"})
		panic("boom")
	}}
	m := askModel(t, b)
	run(t, m, askAsk("x")...)
	askSettle(t, m)
	view := askView(t, m, 80, 20)
	if askLine(view, "so far") < 0 || askLine(view, "the answer failed: boom") < askLine(view, "so far") {
		t.Errorf("the panic:\n%s", view)
	}
	if askOf(t, m).Busy() != "" {
		t.Error("the answer is still streaming")
	}
}

// What a server or a model wrote is drawn as the characters it holds:
// never a command to the terminal, never a key, and never past the edge
// (Review Focus 1).
func TestAskDrawsNoCommandsAndNoKeys(t *testing.T) {
	const key = "sk-ant-api03-TESTONLY-not-a-real-key-0000000000-WXYZ"
	b := &fakeBackend{
		AccountsFn: func(context.Context, Refresh) (Listing, error) {
			return Listing{Default: 1, Accounts: []wire.Account{{ID: 1, Provider: "claude", Email: "\x1b[2Jevil@x"}}}, nil
		},
		AskFn: func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
			each(message.Event{Type: "text", Text: "a\x1b]0;title\x07b and " + key + " 日本語のアカウント 👨‍👩‍👧 family"})
			each(message.Event{Type: "tool", Tool: "\x1b[31mBash"})
			return wire.End{Type: "done", Error: "\x1b[2J bad key " + key}, nil
		},
	}
	m := askModel(t, b)
	run(t, m, askAsk("q\x1b[2J")...)
	askSettle(t, m)
	for _, w := range []int{60, 80, 120} {
		raw := askOf(t, m).View(w, 20)
		for _, bad := range []string{"\x1b[2J", "\x1b]", "\x07", "\x1b[31m", "TESTONLY"} {
			if strings.Contains(raw, bad) {
				t.Errorf("at %d the view holds %q", w, bad)
			}
		}
		for i, l := range strings.Split(ansi.Strip(raw), "\n") {
			if n := textWidth(l); n > w {
				t.Errorf("at %d line %d is %d wide: %q", w, i, n, l)
			}
		}
	}
	view := askView(t, m, 120, 20)
	for _, want := range []string{"[2Jevil@x", "a]0;titleb", "· tool [31mBash", "日本語のアカウント"} {
		if !strings.Contains(view, want) {
			t.Errorf("%q is not shown as text:\n%s", want, view)
		}
	}
}

// askShutCalls is every InterruptRun and CloseRun the backend took, in
// order.
func askShutCalls(b *fakeBackend) []string {
	var out []string
	for _, c := range b.calls() {
		if strings.HasPrefix(c, "InterruptRun(") || strings.HasPrefix(c, "CloseRun(") {
			out = append(out, c)
		}
	}
	return out
}

// askOpening is a backend whose Ask is asked to stay open and says its id
// after sent is closed by the test, or at once when sent is nil; it says
// on opened that it has, ends when the run is closed, and records whether
// its context had ended by the time it said the id.
type askOpening struct {
	*fakeBackend
	sent, opened, closed chan struct{}
	once                 sync.Once
	ctxAtInit            chan error
}

func askOpeningBackend(id string, sent chan struct{}) *askOpening {
	o := &askOpening{fakeBackend: &fakeBackend{}, sent: sent, opened: make(chan struct{}), closed: make(chan struct{}), ctxAtInit: make(chan error, 1)}
	o.AskFn = func(ctx context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		if o.sent != nil {
			select {
			case <-o.sent:
			case <-ctx.Done():
				return wire.End{}, ctx.Err()
			}
		}
		o.ctxAtInit <- ctx.Err()
		each(message.Event{Type: "init", RunID: id})
		close(o.opened)
		select {
		case <-o.closed:
			return wire.End{Type: "done"}, nil
		case <-ctx.Done():
			return wire.End{}, ctx.Err()
		}
	}
	o.InterruptRunFn = func(context.Context, string) error { return nil }
	o.CloseRunFn = func(context.Context, string) error {
		o.once.Do(func() { close(o.closed) })
		return nil
	}
	return o
}

// askStartAlone sends what the field holds and starts the stream itself,
// not through the driver, so nothing reads it: what the stream's goroutine
// reads, Update never hears of.
func askStartAlone(t *testing.T, m *Model, text string) {
	t.Helper()
	a := askOf(t, m)
	run(t, m, tea.PasteMsg{Content: text})
	_, cmd := a.Update(press("enter")[0])
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("enter gave %#v, want the stream and its wait", batch)
	}
	batch[0]()
}

// A run asked to stay open can be stopped before it opens (I1): with no id
// known, esc cancels the request, so a run still waiting to start never
// does; once the stream has read the id, esc interrupts and closes it by
// that id, though the screen has not shown it yet.
func TestEscStopsAKeptOpenAskBeforeItOpens(t *testing.T) {
	t.Run("no id yet", func(t *testing.T) {
		b := askOpeningBackend("r-1", make(chan struct{}))
		m := askModel(t, b.fakeBackend)
		a := askOf(t, m)
		run(t, m, press("ctrl+s")...)
		run(t, m, askAsk("stay")...)
		if !askEnabled(a.Keys(), "esc") || askEnabled(a.Keys(), "ctrl+s") || a.Busy() != "the open answer" {
			t.Errorf("while it opens: esc %v, ctrl+s %v, Busy %q", askEnabled(a.Keys(), "esc"), askEnabled(a.Keys(), "ctrl+s"), a.Busy())
		}
		run(t, m, press("esc")...)
		askSettle(t, m)
		if view := askView(t, m, 80, 20); !strings.Contains(view, "· stopped") || a.Busy() != "" {
			t.Errorf("Busy %q:\n%s", a.Busy(), view)
		}
		if got := askShutCalls(b.fakeBackend); len(got) != 0 {
			t.Errorf("a run with no id was reached by %q", got)
		}
	})
	t.Run("the id read, not shown", func(t *testing.T) {
		b := askOpeningBackend("r-2", nil)
		m := askModel(t, b.fakeBackend)
		a := askOf(t, m)
		run(t, m, press("ctrl+s")...)
		askStartAlone(t, m, "stay")
		<-b.opened
		_, cmd := a.Update(press("esc")[0])
		if cmd == nil {
			t.Fatal("esc did nothing")
		}
		a.Update(cmd())
		if got := askShutCalls(b.fakeBackend); !slices.Equal(got, []string{"InterruptRun(r-2)", "CloseRun(r-2)"}) {
			t.Errorf("esc sent %q", got)
		}
	})
}

// A run kept open goes on when the program ends, so its stream does too
// until Leave has closed it (M1): Leave waits for an id still to come, and
// closes the run by it; and it closes by the id the stream read even when
// the screen never showed it.
func TestLeavingClosesARunThatHasNotOpenedOnTheScreen(t *testing.T) {
	t.Run("the id comes after the program ended", func(t *testing.T) {
		sent := make(chan struct{})
		b := askOpeningBackend("r-7", sent)
		m := askModel(t, b.fakeBackend)
		program, end := context.WithCancel(context.Background())
		m.e.ctx = program
		run(t, m, press("ctrl+s")...)
		run(t, m, askAsk("stay")...)
		end()
		time.AfterFunc(200*time.Millisecond, func() { close(sent) })
		if err := programLeave(m.leavers()); err != nil {
			t.Fatal(err)
		}
		if err := <-b.ctxAtInit; err != nil {
			t.Errorf("the stream ended with the program, before its run said its id: %v", err)
		}
		if got := askShutCalls(b.fakeBackend); !slices.Equal(got, []string{"InterruptRun(r-7)", "CloseRun(r-7)"}) {
			t.Errorf("leaving sent %q", got)
		}
		askSettle(t, m)
	})
	t.Run("the id read, not shown", func(t *testing.T) {
		b := askOpeningBackend("r-8", nil)
		m := askModel(t, b.fakeBackend)
		run(t, m, press("ctrl+s")...)
		askStartAlone(t, m, "stay")
		<-b.opened
		if err := programLeave(m.leavers()); err != nil {
			t.Fatal(err)
		}
		if got := askShutCalls(b.fakeBackend); !slices.Equal(got, []string{"InterruptRun(r-8)", "CloseRun(r-8)"}) {
			t.Errorf("leaving sent %q", got)
		}
	})
}

// A run kept open whose stream is cut goes on without anyone reading it,
// so it is shut by its id, and the answer says so (M3).
func TestACutOpenRunIsShut(t *testing.T) {
	b := &fakeBackend{
		AskFn: func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
			each(message.Event{Type: "init", RunID: "r-3"})
			each(message.Event{Type: "text", Text: "half"})
			return wire.End{}, fmt.Errorf("the answer was cut off before the run ended: %w", io.ErrUnexpectedEOF)
		},
		InterruptRunFn: func(context.Context, string) error { return nil },
		CloseRunFn:     func(context.Context, string) error { return nil },
	}
	m := askModel(t, b)
	run(t, m, press("ctrl+s")...)
	run(t, m, askAsk("x")...)
	askSettle(t, m)
	view := askView(t, m, 80, 20)
	if got := askShutCalls(b); !slices.Equal(got, []string{"InterruptRun(r-3)", "CloseRun(r-3)"}) {
		t.Errorf("after the cut: %q", got)
	}
	if askLine(view, "cut off") < 0 || askLine(view, "· closing the run kept open") < askLine(view, "cut off") {
		t.Errorf("the cut:\n%s", view)
	}
	// A one-shot answer that is cut has no run to shut.
	b.AskFn = func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		return wire.End{}, io.ErrUnexpectedEOF
	}
	run(t, m, press("ctrl+s")...)
	run(t, m, askAsk("y")...)
	askSettle(t, m)
	if got := askShutCalls(b); len(got) != 2 {
		t.Errorf("a one-shot cut reached a run: %q", got)
	}
}

// The run is answering again once the server says it took a message, even
// when the last turn's idle comes after the person sent it (M4).
func TestAMessageTakenAfterAnIdleIsBeingAnswered(t *testing.T) {
	evs := make(chan message.Event)
	var once sync.Once
	b := &fakeBackend{
		AskFn: func(ctx context.Context, _ Question, each func(message.Event)) (wire.End, error) {
			for {
				select {
				case ev, ok := <-evs:
					if !ok {
						return wire.End{Type: "done"}, nil
					}
					each(ev)
				case <-ctx.Done():
					return wire.End{}, ctx.Err()
				}
			}
		},
		SendRunFn:      func(context.Context, string, string) error { return nil },
		InterruptRunFn: func(context.Context, string) error { return nil },
		CloseRunFn: func(context.Context, string) error {
			once.Do(func() { close(evs) })
			return nil
		},
	}
	m := askModel(t, b)
	a := askOf(t, m)
	say := func(ev message.Event) {
		t.Helper()
		evs <- ev
		await(t, m)
	}
	run(t, m, press("ctrl+s")...)
	run(t, m, askAsk("one")...)
	say(message.Event{Type: "init", RunID: "r-1"})
	say(message.Event{Type: "text", Text: "1"})
	run(t, m, askAsk("two")...)
	say(message.Event{Type: "idle"})
	say(message.Event{Type: "input", ID: "m-2", State: "accepted"})
	if !a.answering() {
		t.Fatal("a message taken after the idle is not being answered")
	}
	run(t, m, press("esc")...)
	if got := b.named("InterruptRun"); len(got) != 1 || !a.Typing() {
		t.Errorf("esc during the second turn: %v, typing %v", got, a.Typing())
	}
	run(t, m, press("ctrl+x")...)
	askSettle(t, m)
}

// An Ask that ignores its context after esc is answered for at the same
// grace env.call gives any call (M6).
func TestAnAskThatIgnoresItsContextIsAnsweredFor(t *testing.T) {
	release := make(chan struct{})
	b := &fakeBackend{AskFn: func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		each(message.Event{Type: "text", Text: "so far"})
		<-release
		return wire.End{}, errors.New("late")
	}}
	m := askModel(t, b)
	t.Cleanup(func() { close(release) })
	a := askOf(t, m)
	run(t, m, askAsk("x")...)
	run(t, m, press("esc")...)
	askSettle(t, m)
	if view := askView(t, m, 80, 20); !strings.Contains(view, "· stopped") || a.Busy() != "" {
		t.Errorf("Busy %q:\n%s", a.Busy(), view)
	}
}

// A long line is laid out once, in time its length bounds, and what comes
// after it lays out only itself (M5).
func TestAskLaysOutALongLineOnce(t *testing.T) {
	b := &fakeBackend{AskFn: func(ctx context.Context, _ Question, _ func(message.Event)) (wire.End, error) {
		<-ctx.Done()
		return wire.End{}, ctx.Err()
	}}
	m := askModel(t, b)
	a := askOf(t, m)
	run(t, m, askAsk("long")...)
	id := a.run.id
	long := strings.Repeat("lorem ipsum ", (180<<10)/12) + strings.Repeat("x", 20<<10)
	before := a.work
	a.Update(askEventMsg{id: id, event: message.Event{Type: "text", Text: long}})
	a.View(80, 24)
	if work := a.work - before; work > 5*len(long) {
		t.Errorf("one %d-byte line took %d bytes of work to lay out", len(long), work)
	}
	for i, l := range a.lines {
		if n := textWidth(ansi.Strip(l)); n > 80 {
			t.Fatalf("line %d is %d wide", i, n)
		}
	}
	if got := strings.Join(a.lines, ""); strings.Count(got, "x") != 20<<10 {
		t.Errorf("the long word lost letters: %d of %d", strings.Count(got, "x"), 20<<10)
	}
	mid := a.work
	for range 100 {
		a.Update(askEventMsg{id: id, event: message.Event{Type: "tool", Tool: "Bash"}})
		a.View(80, 24)
	}
	if work := a.work - mid; work > 100*64 {
		t.Errorf("a hundred tool lines after it took %d bytes of work: the long line was laid out again", work)
	}
	run(t, m, press("esc")...)
	askSettle(t, m)
}

// No event is lost when the screen falls behind the stream: more than the
// channel holds arrive, in order.
func TestAskLosesNoEventsWhenTheScreenIsBehind(t *testing.T) {
	const n = 3 * askBuffer
	b := &fakeBackend{AskFn: func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		for i := range n {
			each(message.Event{Type: "text", Text: fmt.Sprintf("%d\n", i)})
		}
		return wire.End{Type: "done"}, nil
	}}
	m := askModel(t, b)
	a := askOf(t, m)
	run(t, m, askAsk("count")...)
	askSettle(t, m)
	var got []string
	for _, p := range a.pieces {
		if p.kind == askText && p.text != "" {
			got = append(got, p.text)
		}
	}
	for i := range n {
		if i >= len(got) || got[i] != strconv.Itoa(i) {
			t.Fatalf("event %d is missing or out of order: %d lines, from %q", i, len(got), got[max(i-2, 0):min(i+2, len(got))])
		}
	}
}

// What the run says along the way is shown where it happened: the CLI's
// own error, a message that was not taken, an interrupt.
func TestAskSaysWhatTheRunSaysAlongTheWay(t *testing.T) {
	b := &fakeBackend{AskFn: func(_ context.Context, _ Question, each func(message.Event)) (wire.End, error) {
		each(message.Event{Type: "text", Text: "first"})
		each(message.Event{Type: "error", Text: "rate limited for a moment"})
		each(message.Event{Type: "input", ID: "m-1", State: "failed", Reason: "the CLI took no input"})
		each(message.Event{Type: "interrupted", ID: "i-1"})
		each(message.Event{Type: "text", Text: "last"})
		return wire.End{Type: "done"}, nil
	}}
	m := askModel(t, b)
	run(t, m, askAsk("x")...)
	askSettle(t, m)
	view := askView(t, m, 80, 24)
	order := []string{"first", "rate limited for a moment", "the CLI took no input", "· interrupted", "last"}
	for i := 1; i < len(order); i++ {
		if askLine(view, order[i-1]) < 0 || askLine(view, order[i]) <= askLine(view, order[i-1]) {
			t.Errorf("%q is not shown after %q:\n%s", order[i], order[i-1], view)
		}
	}
}

// A stream that closed without saying how it ended ends all the same, as
// its context did, or with words saying so (M7).
func TestAskWaitEndsAStreamThatClosedWithoutAWord(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &askRun{id: 4, ctx: ctx, cancel: cancel, ch: make(chan askEventMsg)}
	close(r.ch)
	if got := askWait(r)().(askEventMsg); !got.last || got.id != 4 || got.err == nil || errors.Is(got.err, context.Canceled) {
		t.Errorf("an open context: %+v", got)
	}
	cancel()
	if got := askWait(r)().(askEventMsg); !got.last || !errors.Is(got.err, context.Canceled) {
		t.Errorf("an ended context: %+v", got)
	}
}

// A call about an earlier run says nothing under the new answer; a message
// it carried still goes back to the field, as the person's own (N2).
func TestAskACallAboutAnEarlierRunSaysNothing(t *testing.T) {
	m := askModel(t, &fakeBackend{})
	a := askOf(t, m)
	a.Update(askCallMsg{id: 99, text: "mine", err: &client.Refusal{Status: 409, Message: "the run has ended"}})
	if view := askView(t, m, 80, 20); strings.Contains(view, "the run has ended") || a.area.Value() != "mine" {
		t.Errorf("field %q:\n%s", a.area.Value(), view)
	}
}

// askEnabled says whether a binding for k is enabled among bs.
func askEnabled(bs []key.Binding, k string) bool {
	for _, b := range bs {
		if b.Enabled() && slices.Contains(b.Keys(), k) {
			return true
		}
	}
	return false
}
