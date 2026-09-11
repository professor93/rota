// Package fakecli makes a test binary stand in for a vendor CLI.
//
// Tests need a claude, codex or grok on PATH that answers the way the real
// one would, without a network or a real CLI. A shell script did that on
// unix and nowhere else. Here the test binary itself is installed under the
// vendor's name, next to a small spec saying what to print and how to exit,
// and TestMain hands the process to Maybe, which plays the part when it sees
// the spec. One mechanism, every platform, no shell.
package fakecli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Spec is what the fake does when run. Lines may carry {{stdin}} (what was
// read, trailing newlines trimmed, the way $(cat) trims), {{args}} (the
// arguments joined by spaces, like $*), {{env:NAME}} or {{env:NAME|fallback}},
// and {{cwd}} (the working directory with symlinks resolved, like pwd -P).
// Values are escaped as JSON string contents, since that is where they go.
// A line that is only {{sleep:1s}} pauses instead of printing.
type Spec struct {
	Stdout []string `json:"stdout,omitempty"`
	Stderr []string `json:"stderr,omitempty"`
	Exit   int      `json:"exit,omitempty"`
	// Sleep is a duration to wait before writing anything, for tests that
	// kill a run or bound its concurrency.
	Sleep string `json:"sleep,omitempty"`
	// Touch names a file to create before exiting, so a test can prove the
	// fake ran — or did not.
	Touch string `json:"touch,omitempty"`
	// KeepStdin leaves stdin unread. The default reads it to the end, as
	// every real CLI does with a piped prompt.
	KeepStdin bool `json:"keep_stdin,omitempty"`
	// Echo makes the fake a streaming-input CLI: it reads a message per line
	// for as long as stdin is open, answers each with an assistant line and a
	// result, and says how many more are waiting. Stdout is ignored in this
	// mode, and Sleep is how long a turn takes rather than a pause at the
	// start.
	Echo bool `json:"echo,omitempty"`
	// EchoHold holds the first turn, in echo mode, until this many further
	// lines have arrived on stdin. A test about messages that arrive
	// mid-turn can then prove what happened to them instead of racing a
	// timer. Later turns are answered as they come.
	EchoHold int `json:"echo_hold,omitempty"`
}

// Result is a Spec that answers as the claude CLI does in print mode: one
// terminal result event carrying the prompt and the argv, "fake-stderr" on
// stderr, and the exit code given.
func Result(exit int) Spec {
	return Spec{
		Stdout: []string{`{"type":"result","subtype":"success","is_error":false,"session_id":"s-fake","result":"STDIN={{stdin}} ARGS={{args}}","num_turns":1,"total_cost_usd":0.5}`},
		Stderr: []string{"fake-stderr"},
		Exit:   exit,
	}
}

// Lines is a Spec that prints exactly these lines and exits 0.
func Lines(lines ...string) Spec { return Spec{Stdout: lines} }

// Exe is name as an executable file is called on this platform.
func Exe(name string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") {
		return name + ".exe"
	}
	return name
}

// Install puts a fake called name into dir and returns its path. PATH is
// the caller's to set: a test that hands rota a BaseEnv needs dir there too.
func Install(t testing.TB, dir, name string, spec Spec) string {
	t.Helper()
	path := filepath.Join(dir, Exe(name))
	if err := Link(path); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specPath(path), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Link makes path another name for this test binary: a hard link where the
// filesystem allows, a copy otherwise. A symlink would need a privilege on
// Windows that a test runner does not have.
func Link(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Remove(path)
	if err := os.Link(self, path); err == nil {
		return nil
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

func specPath(exe string) string { return exe + ".spec.json" }

// Maybe plays the fake when this process is one — a spec sits beside the
// executable — and never returns in that case. TestMain calls it first;
// a real test binary has no spec and carries on.
func Maybe() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	raw, err := os.ReadFile(specPath(exe))
	if err != nil {
		return
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "fake cli:", err)
		os.Exit(70)
	}
	os.Exit(run(spec, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(spec Spec, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if spec.Echo {
		return echo(spec, stdin, stdout)
	}
	in := ""
	if !spec.KeepStdin {
		raw, _ := io.ReadAll(stdin)
		in = strings.TrimRight(string(raw), "\n")
	}
	if spec.Sleep != "" {
		if d, err := time.ParseDuration(spec.Sleep); err == nil {
			time.Sleep(d)
		}
	}
	cwd, _ := os.Getwd()
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	// Every substitution lands inside a JSON string, and a Windows path or
	// a quoted argument would break the document it sits in; the value is
	// written the way a JSON string carries it, and read back as itself.
	quote := func(v string) string {
		raw, _ := json.Marshal(v)
		return string(raw[1 : len(raw)-1])
	}
	expand := func(line string) string {
		line = strings.ReplaceAll(line, "{{stdin}}", quote(in))
		line = strings.ReplaceAll(line, "{{args}}", quote(strings.Join(args, " ")))
		line = strings.ReplaceAll(line, "{{cwd}}", quote(cwd))
		for {
			start := strings.Index(line, "{{env:")
			if start < 0 {
				return line
			}
			end := strings.Index(line[start:], "}}")
			if end < 0 {
				return line
			}
			name := line[start+len("{{env:") : start+end]
			// {{env:NAME|fallback}} reads like ${NAME:-fallback}.
			fallback := ""
			if n, f, ok := strings.Cut(name, "|"); ok {
				name, fallback = n, f
			}
			value, set := os.LookupEnv(name)
			if !set || value == "" {
				value = fallback
			}
			line = line[:start] + quote(value) + line[start+end+2:]
		}
	}
	for _, l := range spec.Stdout {
		// A line of the form {{sleep:1s}} is a pause, not output, for a
		// fake that must still be running when something looks at it.
		if d, ok := strings.CutPrefix(l, "{{sleep:"); ok {
			if wait, err := time.ParseDuration(strings.TrimSuffix(d, "}}")); err == nil {
				time.Sleep(wait)
			}
			continue
		}
		fmt.Fprintln(stdout, expand(l))
	}
	for _, l := range spec.Stderr {
		fmt.Fprintln(stderr, expand(l))
	}
	if spec.Touch != "" {
		_ = os.WriteFile(spec.Touch, nil, 0o600)
	}
	return spec.Exit
}

// echo plays a CLI with a streaming input: one turn per message, for as long
// as stdin stays open.
//
// It is the shape of the real thing rather than an imitation of its wording.
// A control request is answered at once, whatever the turn is doing, because
// that is what makes an interrupt an interrupt. Every other line is a message
// and waits its turn, and each result says how many messages are still
// waiting — the one number a caller can learn delivery from.
func echo(spec Spec, stdin io.Reader, stdout io.Writer) int {
	var mu sync.Mutex
	say := func(v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintln(stdout, string(raw))
	}
	var turnTime time.Duration
	if spec.Sleep != "" {
		turnTime, _ = time.ParseDuration(spec.Sleep)
	}

	messages := make(chan string, 1024)
	// done says no further line will ever arrive, so a hold that will never
	// be satisfied ends instead of waiting for ever.
	var done atomic.Bool
	go func() {
		defer close(messages)
		defer done.Store(true)
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		for sc.Scan() {
			var in struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
				Message   struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &in) != nil {
				continue
			}
			if in.Type == "control_request" {
				say(map[string]any{"type": "control_response", "response": map[string]any{
					"subtype": "success", "request_id": in.RequestID,
					"response": map[string]any{"still_queued": []string{}},
				}})
				continue
			}
			text := ""
			if len(in.Message.Content) > 0 {
				text = in.Message.Content[0].Text
			}
			messages <- text
		}
	}()

	turn := 0
	for text := range messages {
		if turnTime > 0 {
			time.Sleep(turnTime)
		}
		// The first turn can be held open until the messages a test means to
		// send mid-turn are in, which is what makes such a test about the
		// queue rather than about how fast the machine is.
		for turn == 0 && len(messages) < spec.EchoHold && !done.Load() {
			time.Sleep(5 * time.Millisecond)
		}
		turn++
		answer := "echo: " + text
		say(map[string]any{"type": "assistant", "session_id": "s-fake",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": answer}}}})
		say(map[string]any{"type": "result", "subtype": "success", "is_error": false,
			"session_id": "s-fake", "result": answer, "num_turns": turn,
			"queued_turn_count": len(messages)})
	}
	return 0
}
