// Package fakecli makes a test binary stand in for a vendor CLI.
//
// Tests need a claude, codex, grok or kimi on PATH that answers the way the
// real one would, without a network or a real CLI. A shell script did that on
// unix and nowhere else. Here the test binary itself is installed under the
// vendor's name, next to a small spec saying what to print and how to exit,
// and TestMain hands the process to Maybe, which plays the part when it sees
// the spec. One mechanism, every platform, no shell. When it sees none, Maybe
// leaves every directory holding a real vendor CLI — claude, codex, grok,
// kimi — out of PATH, so the only one a test can ever start is a fake.
package fakecli

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	// StdinTo names a file to write what was read on stdin to, byte for
	// byte, before exiting, so a test can see what a program was handed.
	// The Echo, Tty and Exec modes end before it is written.
	StdinTo string `json:"stdin_to,omitempty"`
	// KeepStdin leaves stdin unread. The default reads it to the end, as
	// every real CLI does with a piped prompt.
	KeepStdin bool `json:"keep_stdin,omitempty"`
	// Echo makes the fake a streaming-input CLI: it reads a message per line
	// for as long as stdin is open, answers each with an assistant line and a
	// result, and says how many more are waiting. Stdout is ignored in this
	// mode, and Sleep is how long a turn takes rather than a pause at the
	// start.
	Echo bool `json:"echo,omitempty"`
	// Tty makes the fake an interactive CLI sitting at a terminal, which is
	// what a session on a pseudo-terminal needs to be tested against: it
	// says it is ready and how big its window is, answers each line it reads
	// with a transformed copy — so a test can tell the terminal's own echo
	// from the program's output — says the new size whenever the window
	// changes, prints as much as it is asked to on "noise N", writes the
	// bytes "print <hex>" spells out, answers "env", "envhas" and
	// "registry" (see ask) and exits on "bye". Stdout is ignored in this
	// mode.
	Tty bool `json:"tty,omitempty"`
	// EchoHold holds the first turn, in echo mode, until this many further
	// lines have arrived on stdin. A test about messages that arrive
	// mid-turn can then prove what happened to them instead of racing a
	// timer. Later turns are answered as they come.
	EchoHold int `json:"echo_hold,omitempty"`
	// Term is what the fake does about SIGTERM. "exit" finishes as a CLI
	// shutting down cleanly does: it creates TermTouch and exits with Exit.
	// "ignore" takes no notice. Empty leaves it to the runtime, which dies.
	Term      string `json:"term,omitempty"`
	TermTouch string `json:"term_touch,omitempty"`
	// TermDelay is how long "exit" takes to finish, as a helper still closing
	// up does after the CLI that started it has gone.
	TermDelay string `json:"term_delay,omitempty"`
	// Spawn is the path of another fake to start and leave running, in the
	// fake's own process group, as a CLI starts a helper.
	Spawn string `json:"spawn,omitempty"`
	// Detach is the path of another fake to start in a process group of its
	// own, holding this fake's stdout and stderr, as a CLI that daemonises a
	// helper leaves it: the pipes stay open after the CLI is gone, and a
	// signal to the CLI's group does not reach it.
	Detach string `json:"detach,omitempty"`
	// Ready names a file the fake writes its pid to once Term, Spawn,
	// Detach and Linger are in force, so a test can signal it without racing
	// its start. Before anything else, the fake also creates Ready+".ran", so
	// a test can tell a fake that ran and failed from one that never ran at
	// all: see Up.
	Ready string `json:"ready,omitempty"`
	// Exec is a program the fake becomes once it has read stdin to its end,
	// by an exec from a thread other than its main one, as a Go CLI's
	// syscall.Exec usually is: Linux shows the process as a zombie for that
	// moment while the thread takes over the pid. Linux only; elsewhere the
	// fake says it cannot and exits 70.
	Exec string `json:"exec,omitempty"`
	// KilledRuns plays macOS killing a fake inside exec, for a test of what
	// copes with that: "N:file" makes each run add a byte to file and die of
	// a kill before it has done anything, while the file holds no more than
	// N. It sits in the spec because the code under test, not the test,
	// decides what environment a fake gets.
	KilledRuns string `json:"killed_runs,omitempty"`
	// Linger keeps a tty fake this long after its terminal hangs up or its
	// input ends, taking no notice of the hang-up, as a CLI finishing its
	// work does: a test can then see what outlives the process holding the
	// other end of its terminal.
	Linger string `json:"linger,omitempty"`
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
// Windows that a test runner does not have. On macOS the fake is then run
// once, doing nothing, before Link returns: see warm.
func Link(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Remove(path)
	if err := os.Link(self, path); err != nil {
		if err := copyTo(self, path); err != nil {
			return err
		}
	}
	return warm(path)
}

// Warm is the run Link makes, for a path a test made to this test binary
// some other way — a symbolic link, say, that it then starts as rota: on
// macOS the path is run once, doing nothing, and waited for, so the look
// macOS takes at a new path is over before the test starts it for real.
// Elsewhere it does nothing.
func Warm(path string) error { return warm(path) }

func copyTo(self, path string) error {
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

// warmArg is the one argument a fake answers by exiting at once, whatever
// it was installed to do.
const warmArg = "-fakecli.warm"

// ranSuffix names, beside a fake's Ready file, the file it creates as soon
// as it runs.
const ranSuffix = ".ran"

// startTries is how many times Up starts a fake that never ran, as warm does
// a first run.
const startTries = 3

// upLimit is how long a fake has to say it is up while its run goes on, and
// upGrace how long one has to show it ran once its run has ended. A test of
// Up itself shortens the grace.
var (
	upLimit = 30 * time.Second
	upGrace = 5 * time.Second
)

// Up starts a fake through the code under test and returns its pid once the
// fake says it is up.
//
// start installs a fake with Spec.Ready set to ready, starts whatever runs
// it, and returns a channel closed when that run has ended. Each start is
// given a ready file of its own, so only the fake of the last start can
// prove it came up. Whatever else start makes for its run, it makes afresh
// each time, so that the test goes on with the last start's run and never
// with one Up gave up on:
//
//	var done chan error
//	fakecli.Up(t, func(ready string) <-chan struct{} {
//		bin := fakecli.Install(t, t.TempDir(), "claude", fakecli.Spec{Ready: ready, Sleep: "30s"})
//		done = make(chan error, 1)
//		ended := make(chan struct{})
//		go func() { _, err := rota.Run(ctx, a, "", &rota.Command{Bin: bin}, spec, nil, nil); done <- err; close(ended) }()
//		return ended
//	})
//
// A fake that never runs a line never says it ran, and its run ends. macOS
// killing it inside exec, a later run as well as a first one (see warm), is
// the usual cause and no fault of the code under test, so Up starts it
// again, up to startTries times. Anything else fails the test: a fake that
// ran but whose run ended without it saying it was up, which is the code
// under test or the fake, and a fake that neither comes up nor ends within
// upLimit. Once the run has ended a fake still has upGrace to say it is up,
// for a helper on its way. A fake that says it is up just as its run ends is
// up: what the run did next is for the test to judge.
//
// Up proves one fake per start, and learns that one died only from its run
// ending. A helper that a CLI spawns while the CLI goes on running fits
// neither: were macOS to kill the helper inside exec the run would not end,
// and Up would fail at upLimit rather than start it again. A test of such a
// group waits for each of its fakes with WaitReady.
func Up(t testing.TB, start func(ready string) (ended <-chan struct{})) int {
	t.Helper()
	for try := 1; ; try++ {
		ready := filepath.Join(t.TempDir(), "ready")
		pid, ran := upOrEnded(t, ready, start(ready))
		switch {
		case pid > 0:
			return pid
		case ran:
			t.Fatalf("the fake ran but its run ended before it said it was up at %s", ready)
		case try == startTries:
			t.Fatalf("the fake never ran a line, %d times in a row (macOS killing it inside exec is the usual cause)", startTries)
		}
		t.Logf("start %d: the fake never ran a line (macOS killing it inside exec is the usual cause); starting it again", try)
	}
}

// upOrEnded waits for the fake at ready to say it is up, or for its run to
// end. It reports the fake's pid once it is up, or 0 and whether it ran.
func upOrEnded(t testing.TB, ready string, ended <-chan struct{}) (pid int, ran bool) {
	t.Helper()
	for limit := time.Now().Add(upLimit); ; {
		if pid := readyPid(ready); pid > 0 {
			return pid, true
		}
		select {
		case <-ended:
			// The run is over. A fake started by something other than the
			// run itself, a helper, may still be on its way: it says it ran
			// as it starts, and that it is up only once what it starts first
			// is going. So whether it ran is asked only when the grace is
			// over. One killed inside exec never gets anywhere.
			for grace := time.Now().Add(upGrace); ; time.Sleep(5 * time.Millisecond) {
				if pid := readyPid(ready); pid > 0 {
					return pid, true
				}
				if time.Now().After(grace) {
					_, err := os.Stat(ready + ranSuffix)
					return 0, err == nil
				}
			}
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(limit) {
			t.Fatalf("the fake never said it was up at %s, and its run has not ended", ready)
		}
	}
}

// readyPid is the pid a fake wrote to its Ready file, or 0 while it has not.
func readyPid(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}

// playKill plays macOS killing a fake inside exec, as "N:file" asks (see
// Spec.KilledRuns), and returns only for a run it lets through.
func playKill(knob string) {
	n, file, ok := strings.Cut(knob, ":")
	limit, err := strconv.Atoi(n)
	if !ok || err != nil {
		return
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write([]byte{'x'})
	st, err := f.Stat()
	f.Close()
	if err == nil && st.Size() <= int64(limit) {
		if self, err := os.FindProcess(os.Getpid()); err == nil {
			_ = self.Kill()
		}
		os.Exit(137) // only if the kill did not land
	}
}

// WaitReady waits for a fake installed with Spec.Ready to say it is up, and
// returns its pid. A test that stops a running fake waits for this first: a
// signal that reaches a fake sooner can end it before it has run a line of
// its own, a handler for that very signal included.
func WaitReady(t testing.TB, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake cli never said it was up at %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func specPath(exe string) string { return exe + ".spec.json" }

// ownSpec is the spec beside this executable, if there is one: if this
// process is a fake.
func ownSpec() ([]byte, bool) {
	exe, err := os.Executable()
	if err != nil {
		return nil, false
	}
	raw, err := os.ReadFile(specPath(exe))
	return raw, err == nil
}

// Maybe plays the fake when this process is one — a spec sits beside the
// executable — and never returns in that case. TestMain calls it first;
// a real test binary has no spec and carries on, with every directory that
// holds a real vendor CLI left out of its PATH (see scrubPath), so that no
// test it runs can start one.
func Maybe() {
	if len(os.Args) == 2 && os.Args[1] == warmArg {
		refuseWarm()
		os.Exit(0)
	}
	raw, ok := ownSpec()
	if !ok {
		scrubPath()
		return
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "fake cli:", err)
		os.Exit(70)
	}
	playKill(spec.KilledRuns)
	if spec.Ready != "" {
		// The first thing a fake that runs at all does: see Up.
		_ = os.WriteFile(spec.Ready+ranSuffix, nil, 0o600)
	}
	os.Exit(run(spec, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(spec Spec, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch spec.Term {
	case "ignore":
		signal.Ignore(syscall.SIGTERM)
	case "exit":
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		go func() {
			<-term
			if d, err := time.ParseDuration(spec.TermDelay); err == nil {
				time.Sleep(d)
			}
			_ = os.WriteFile(spec.TermTouch, nil, 0o600)
			os.Exit(spec.Exit)
		}()
	}
	if spec.Spawn != "" {
		// Nothing of this fake's is passed on, so the helper holds no pipe of
		// the run's open: only the group keeps it within reach.
		if err := exec.Command(spec.Spawn).Start(); err != nil {
			fmt.Fprintln(stderr, "fake cli:", err)
			return 70
		}
	}
	if spec.Detach != "" {
		helper := exec.Command(spec.Detach)
		helper.Stdout, helper.Stderr = stdout, stderr
		detach(helper)
		if err := helper.Start(); err != nil {
			fmt.Fprintln(stderr, "fake cli:", err)
			return 70
		}
	}
	if spec.Linger != "" {
		signal.Ignore(syscall.SIGHUP)
	}
	if spec.Ready != "" {
		// Written whole and then named, so WaitReady never reads half a pid.
		_ = os.WriteFile(spec.Ready+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o600)
		_ = os.Rename(spec.Ready+".tmp", spec.Ready)
	}
	if spec.Tty {
		code := atty(spec, stdin, stdout)
		if d, err := time.ParseDuration(spec.Linger); err == nil {
			time.Sleep(d)
		}
		return code
	}
	if spec.Echo {
		return echo(spec, stdin, stdout)
	}
	in, read := "", []byte(nil)
	if !spec.KeepStdin {
		read, _ = io.ReadAll(stdin)
		in = strings.TrimRight(string(read), "\n")
	}
	if spec.Exec != "" {
		fmt.Fprintln(stderr, "fake cli:", execFromThread(spec.Exec))
		return 70
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
	if spec.StdinTo != "" {
		_ = os.WriteFile(spec.StdinTo, read, 0o600)
	}
	if spec.Touch != "" {
		_ = os.WriteFile(spec.Touch, nil, 0o600)
	}
	return spec.Exit
}

// atty plays a CLI that is being typed at rather than piped to.
//
// It says what a program at a terminal can say and a program on a pipe
// cannot: how big its window is, and when that changed. Every line it reads
// comes back transformed, because a terminal echoes what is typed into it
// all by itself — a test that saw "hello" could not otherwise tell whether
// the program read it or the kernel bounced it back.
//
// Lines end in carriage return and newline, as they do on a terminal in its
// ordinary mode: a bare newline moves down without returning to the left,
// and the output staircases across the screen.
func atty(spec Spec, stdin io.Reader, stdout io.Writer) int {
	var mu sync.Mutex
	say := func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(stdout, format+"\r\n", a...)
	}
	size := func(what string) {
		cols, rows := termSize()
		say("%s %dx%d", what, cols, rows)
	}
	// The window is watched before ready is said, because ready is what a
	// test waits for before it resizes: a SIGWINCH that arrives before
	// anybody is listening for it is dropped, and the size is never said.
	watchResize(func() { size("size") })
	size("ready")

	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		// "noise N" prints N lines of two hundred bytes and then says it is
		// over: what a test needs to make a reader that is not reading fall
		// behind whatever the kernel was willing to buffer for it.
		if n, ok := strings.CutPrefix(line, "noise "); ok {
			count, err := strconv.Atoi(strings.TrimSpace(n))
			if err != nil || count < 0 {
				count = 0
			}
			for i := range count {
				say("noise %06d %s", i, strings.Repeat("x", 185))
			}
			say("noise done")
			continue
		}
		// "print <hex>" writes exactly the bytes it spells, with no newline:
		// how a test makes the fake draw what a full-screen program draws,
		// escape sequences and all, without a full-screen program.
		if h, ok := strings.CutPrefix(line, "print "); ok {
			raw, err := hex.DecodeString(strings.TrimSpace(h))
			mu.Lock()
			if err != nil {
				fmt.Fprintf(stdout, "bad print: %s\r\n", h)
			} else {
				_, _ = stdout.Write(raw)
			}
			mu.Unlock()
			continue
		}
		if reply, ok := ask(line); ok {
			say("%s", reply)
			continue
		}
		switch line {
		case "size":
			size("size")
		case "bye":
			say("bye")
			return spec.Exit
		default:
			say("got:%s", visible(line))
		}
	}
	return spec.Exit
}

// visible spells out each control byte of what was read as \xHH. Echoed
// back raw, an escape sequence somebody typed would be acted on by the
// terminal showing it, and a test reading the screen would see its effect
// instead of the bytes that arrived.
func visible(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		if c := line[i]; c < 0x20 || c == 0x7f {
			fmt.Fprintf(&b, `\x%02x`, c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
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
