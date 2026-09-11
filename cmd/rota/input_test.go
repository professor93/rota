package main

import (
	"bufio"
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// echoCLI is a fake with a streaming input: it answers every message it is
// sent for as long as its stdin is open, which is the whole point of a run
// that stays open.
func echoCLI(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Spec{Echo: true})
	t.Setenv("PATH", bin)
}

// typed is what somebody types into the run while it is going.
func typed(t *testing.T, lines string) {
	t.Helper()
	was := stdin
	stdin = strings.NewReader(lines)
	t.Cleanup(func() { stdin = was })
}

// texts is what the agent said, in order.
func texts(events []map[string]any) []string {
	var out []string
	for _, ev := range events {
		if ev["type"] == "text" {
			s, _ := ev["text"].(string)
			out = append(out, s)
		}
	}
	return out
}

// A run started with --input goes on taking messages from stdin: each one is
// answered in the same conversation, an interrupt is acknowledged, a steer
// starts the next turn, and the stream says what became of every one of them.
func TestInputReadsMoreMessagesFromStdin(t *testing.T) {
	oneAccount(t)
	echoCLI(t)
	typed(t, "two\n/interrupt\n/steer three\n/close\n")

	out, errOut, code := call(t, "--json", "run", "1", "one", "--input")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	events := eventsOf(t, out)
	if len(events) == 0 || events[0]["type"] != "init" {
		t.Fatalf("a stream rota opens: %v", kinds(events))
	}
	id, _ := events[0]["run_id"].(string)
	if id == "" {
		t.Fatalf("an open run is addressable, so it says its id: %v", events[0])
	}
	if got := texts(events); len(got) != 3 || got[0] != "echo: one" || got[1] != "echo: two" || got[2] != "echo: three" {
		t.Fatalf("every message is answered, in the order it was sent: %q", got)
	}

	// Every message reaches the CLI and then gets a turn, and the two are
	// separate events because the CLI never echoes an injected message back.
	accepted, answered := map[string]bool{}, map[string]bool{}
	interrupted, idle := 0, 0
	for _, ev := range events {
		evID, _ := ev["id"].(string)
		switch ev["type"] {
		case "input":
			switch ev["state"] {
			case "accepted":
				accepted[evID] = true
			case "answered":
				if !accepted[evID] {
					t.Fatalf("a message is accepted before it is answered: %v", ev)
				}
				answered[evID] = true
			default:
				t.Fatalf("no message failed: %v", ev)
			}
		case "interrupted":
			if evID == "" {
				t.Fatalf("an acknowledged interrupt carries its own id: %v", ev)
			}
			interrupted++
		case "idle":
			idle++
		}
	}
	if len(accepted) != 2 || len(answered) != 2 {
		t.Fatalf("two messages were sent into the run: accepted %v, answered %v", accepted, answered)
	}
	if interrupted < 2 {
		t.Fatalf("/interrupt and /steer each interrupt: %d acknowledgements", interrupted)
	}
	if idle == 0 {
		t.Fatal("a run with nothing left waiting says so")
	}
	last := events[len(events)-1]
	if last["type"] != "done" || last["exit_code"] != nil && last["exit_code"] != float64(0) {
		t.Fatalf("the stream ends by saying how the run ended: %v", last)
	}
}

// Text mode is text mode: the answers and nothing else. What became of each
// message is stream bookkeeping, and a person who asked for prose did not ask
// for it.
func TestInputTextModePrintsOnlyTheAnswers(t *testing.T) {
	oneAccount(t)
	echoCLI(t)
	typed(t, "two\n/interrupt\n/steer three\n/close\n")

	out, errOut, code := call(t, "run", "1", "one", "--input")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	// The answers are pieces of a stream and are printed as they arrive, so
	// this fake's — which end in no newline of their own — run together. What
	// is pinned is that they are all that reached stdout.
	if out != "echo: oneecho: twoecho: three\n" {
		t.Fatalf("the answers, and nothing about the messages behind them: %q", out)
	}
	if !strings.Contains(errOut, "rota: run ") || !strings.Contains(errOut, "rota send ") {
		t.Fatalf("an open run says how to send into it: %q", errOut)
	}
}

// With --input and no prompt, the first line of stdin is the opening message:
// `echo ... | rota run 1 --input` is a whole conversation from a pipe.
func TestInputTakesThePromptFromStdinWhenNoneIsGiven(t *testing.T) {
	oneAccount(t)
	echoCLI(t)
	typed(t, "hello\n/close\n")

	out, errOut, code := call(t, "--json", "run", "1", "--input")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if got := texts(eventsOf(t, out)); len(got) == 0 || got[0] != "echo: hello" {
		t.Fatalf("the first line is the prompt: %q", got)
	}
}

// Only Claude Code has a streaming input, and a CLI that has none refuses the
// flag by name before the run costs anything.
func TestInputRefusesWhatCannotTakeIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"t-cli-fake","email":"a@x","order":1,"token":{"accessToken":"t"}}]}`)
	echoCLI(t)
	typed(t, "/close\n")

	out, errOut, code := call(t, "run", "1", "hi", "--input")
	if code == 0 {
		t.Fatalf("a CLI with no streaming input must not be asked for one: %q", out)
	}
	if !strings.Contains(errOut, "input") {
		t.Fatalf("and the refusal must name the field: %q", errOut)
	}
}

// shortHome is oneAccount in a directory short enough to hold a unix socket
// path: macOS caps one at about a hundred characters, and a test's own
// temporary directory is most of that on its own.
func shortHome(t *testing.T) {
	t.Helper()
	home, err := os.MkdirTemp("", "rota")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("ROTA_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}}]}`)
}

// `rota send` reaches a run someone else started: the message joins that
// conversation, an interrupt stops what it is doing, and a close ends it.
func TestSendReachesARunningInput(t *testing.T) {
	shortHome(t)
	// Where the run will actually listen, under the same rules: a platform
	// without unix sockets, or a path too long for one, has no send surface
	// and nothing here to test.
	probe := filepath.Join(os.Getenv("ROTA_HOME"), "probe.sock")
	if ln, err := net.Listen("unix", probe); err != nil {
		t.Skipf("a run cannot be sent into here: %v", err)
	} else {
		ln.Close()
	}
	echoCLI(t)
	in, typing := io.Pipe()
	was := stdin
	stdin = in
	t.Cleanup(func() { stdin = was; typing.Close() })

	lines, out := io.Pipe()
	var errOut bytes.Buffer
	code := make(chan int, 1)
	go func() {
		code <- run([]string{"--json", "run", "1", "one", "--input"}, out, &errOut)
		out.Close()
	}()

	// The events are read as they arrive, because the run has to be sent into
	// while it is still going.
	var mu sync.Mutex
	var events []map[string]any
	opened, read := make(chan string, 1), make(chan struct{})
	go func() {
		defer close(read)
		sc := bufio.NewScanner(lines)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			var ev map[string]any
			if jsonv2.Unmarshal(sc.Bytes(), &ev) != nil {
				continue
			}
			if id, ok := ev["run_id"].(string); ok && id != "" {
				select {
				case opened <- id:
				default:
				}
			}
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
	}()

	var id string
	select {
	case id = <-opened:
	case <-time.After(30 * time.Second):
		t.Fatalf("the run never said its id: %q", errOut.String())
	}

	if got, said, c := call(t, "send", id, "from outside"); c != 0 || !strings.HasPrefix(got, "accepted ") {
		t.Fatalf("a message from another terminal is accepted: %d %q %q", c, got, said)
	}
	if got, said, c := call(t, "send", id, "--interrupt"); c != 0 || !strings.HasPrefix(got, "interrupted ") {
		t.Fatalf("an interrupt is acknowledged: %d %q %q", c, got, said)
	}
	if got, said, c := call(t, "send", id, "--close"); c != 0 || strings.TrimSpace(got) != "closed" {
		t.Fatalf("a close ends the run: %d %q %q", c, got, said)
	}

	select {
	case got := <-code:
		if got != 0 {
			t.Fatalf("the run ended %d: %q", got, errOut.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("a closed run must end: %q", errOut.String())
	}
	<-read
	mu.Lock()
	defer mu.Unlock()
	if got := texts(events); !slices.Contains(got, "echo: from outside") {
		t.Fatalf("what was sent from outside was answered in the same run: %q", got)
	}
}

// Sending into a run that was never there says so, by name, rather than
// failing at a socket path nobody typed.
func TestSendToNoRunIsAnError(t *testing.T) {
	oneAccount(t)

	_, errOut, code := call(t, "send", "nope", "x")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut, "nope") {
		t.Fatalf("the message must name the run asked for: %q", errOut)
	}
}

// A killed run leaves its socket file behind, and a file is not a run. The
// next send says so and takes the file with it, so it stops standing in for
// something that ended.
func TestSendCleansAStaleSocket(t *testing.T) {
	oneAccount(t)
	dir := filepath.Join(os.Getenv("ROTA_HOME"), "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "gone.sock")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, errOut, code := call(t, "send", "gone", "x")
	if code != 1 || !strings.Contains(errOut, "not running") {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the leftover must go: %v", err)
	}
}
