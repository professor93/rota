package notify

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/events"
	"github.com/professor93/rota/internal/fakecli"
)

// cmdFake installs a fake program called name with spec in a directory of
// its own, puts that directory alone on PATH for the test, and returns the
// fake's path.
func cmdFake(t *testing.T, name string, spec fakecli.Spec) string {
	t.Helper()
	dir := t.TempDir()
	path := fakecli.Install(t, dir, name, spec)
	t.Setenv("PATH", dir)
	return path
}

// cmdSink makes a command sink running argv, with hide as the variables the
// program must not see.
func cmdSink(t *testing.T, hide []string, argv ...string) sender {
	t.Helper()
	snd, err := newCommand(api.EventSink{Type: "command", Command: argv}, env{hide: hide})
	if err != nil {
		t.Fatalf("newCommand(%q): %v", argv, err)
	}
	return snd
}

// cmdEvent is a session.ended event with most of its fields set.
func cmdEvent() events.Event {
	exit := 0
	return events.Event{
		ID: 1754000000000001, Type: events.SessionEnded, Time: time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC),
		Account: 2, Session: "s-1", Name: "refactor", Text: "ended \"well\"", Reason: "exited", Exit: &exit,
	}
}

func cmdSend(t *testing.T, snd sender, e events.Event) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return snd.send(ctx, e)
}

// The program reads the event on its stdin as one line of JSON, the
// line the event stream carries, and nothing of it is in its arguments.
func TestACommandGetsTheEventOnStdin(t *testing.T) {
	got := filepath.Join(t.TempDir(), "stdin")
	cmdFake(t, "rota-cmd-in", fakecli.Spec{StdinTo: got, Stderr: []string{"{{args}}"}, Exit: 3})
	e := cmdEvent()
	err := cmdSend(t, cmdSink(t, nil, "rota-cmd-in", "one", "two"), e)
	if err == nil || !strings.HasSuffix(err.Error(), ": one two") {
		t.Fatalf("the program's arguments are not its own: %v", err)
	}
	raw, rerr := os.ReadFile(got)
	if rerr != nil {
		t.Fatal(rerr)
	}
	line := string(raw)
	if !strings.HasSuffix(line, "}\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("stdin is not one line of JSON and a newline: %q", line)
	}
	frame := string(events.Frame(e))
	if !strings.Contains(frame, "\ndata: "+line+"\n") {
		t.Fatalf("stdin %q is not the stream's data line in\n%s", line, frame)
	}
	var back events.Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != e.ID || back.Type != e.Type || back.Text != e.Text || back.Exit == nil || *back.Exit != 0 {
		t.Fatalf("read back %+v", back)
	}
}

// The program runs with the daemon's environment, without the token a
// client brings and without the variables the daemon was told to hide, and
// their values are among the sink's secrets.
func TestACommandGetsNoBearerToken(t *testing.T) {
	t.Setenv("ROTA_TOKEN", "xtokenvalue")
	t.Setenv("MYTOK", "ytokenvalue")
	t.Setenv("ROTA_CMD_KEPT", "kept")
	cmdFake(t, "rota-cmd-env", fakecli.Spec{
		Stderr: []string{"{{env:ROTA_CMD_KEPT|none}} {{env:ROTA_TOKEN|none}} {{env:MYTOK|none}}"}, Exit: 3,
	})
	snd := cmdSink(t, []string{"ROTA_TOKEN", "MYTOK"}, "rota-cmd-env")
	err := cmdSend(t, snd, cmdEvent())
	var d *drop
	if !errors.As(err, &d) || !strings.HasSuffix(err.Error(), "kept none none") {
		t.Fatalf("got %v", err)
	}
	keys := snd.secrets()
	for _, v := range []string{"xtokenvalue", "ytokenvalue"} {
		if !strings.Contains(strings.Join(keys, " "), v) {
			t.Fatalf("secrets %q lack %q", keys, v)
		}
	}
	// ROTA_TOKEN is hidden even when the daemon names nothing to hide.
	cmdFake(t, "rota-cmd-env2", fakecli.Spec{Stderr: []string{"{{env:ROTA_TOKEN|none}}"}, Exit: 3})
	if err := cmdSend(t, cmdSink(t, nil, "rota-cmd-env2"), cmdEvent()); err == nil || !strings.HasSuffix(err.Error(), ": none") {
		t.Fatalf("got %v", err)
	}
}

// What the program says on stderr is said without the hidden values in it,
// and cut to 200 bytes.
func TestACommandsStderrIsCleanAndShort(t *testing.T) {
	t.Setenv("MYTOK", "ytokenvalue")
	cmdFake(t, "rota-cmd-leak", fakecli.Spec{Stderr: []string{"", "  bad token ytokenvalue " + strings.Repeat("é", 300), "second line"}, Exit: 1})
	snd := cmdSink(t, []string{"MYTOK"}, "rota-cmd-leak")
	err := cmdSend(t, snd, cmdEvent())
	if err == nil {
		t.Fatal("no error")
	}
	msg := cleanErr(err, snd.secrets())
	if strings.Contains(err.Error(), "ytokenvalue") || strings.Contains(msg, "ytokenvalue") {
		t.Fatalf("the hidden value is said: %q", err)
	}
	if !strings.HasPrefix(err.Error(), "command exited 1: bad token [redacted] é") || strings.Contains(err.Error(), "second") {
		t.Fatalf("got %q", err)
	}
	line := strings.TrimPrefix(err.Error(), "command exited 1: ")
	if len(line) > 200 || !strings.HasSuffix(line, "…") {
		t.Fatalf("the line is %d bytes: %q", len(line), line)
	}
}

// Exit 0 is delivered, 75 (EX_TEMPFAIL) is tried again after backoff's own
// wait, and anything else is dropped with the first line of stderr.
func TestExitCodesDecideWhatHappens(t *testing.T) {
	for _, tc := range []struct {
		exit int
		want string // "", "again" or "dropped"
		msg  string
	}{
		{0, "", ""},
		{75, "again", "command exited 75: busy now"},
		{3, "dropped", "command exited 3: busy now"},
		{1, "dropped", "command exited 1: busy now"},
	} {
		cmdFake(t, "rota-cmd-exit", fakecli.Spec{Stderr: []string{"busy now", "and more"}, Exit: tc.exit})
		err := cmdSend(t, cmdSink(t, nil, "rota-cmd-exit"), cmdEvent())
		var r *retry
		var d *drop
		switch tc.want {
		case "":
			if err != nil {
				t.Fatalf("exit %d: %v", tc.exit, err)
			}
			continue
		case "again":
			if !errors.As(err, &r) || r.after != 0 {
				t.Fatalf("exit %d: %#v", tc.exit, err)
			}
		case "dropped":
			if !errors.As(err, &d) {
				t.Fatalf("exit %d: %#v", tc.exit, err)
			}
		}
		if err.Error() != tc.msg {
			t.Fatalf("exit %d: %q, want %q", tc.exit, err, tc.msg)
		}
	}
	// No stderr at all says the code alone.
	cmdFake(t, "rota-cmd-quiet", fakecli.Spec{Exit: 4})
	if err := cmdSend(t, cmdSink(t, nil, "rota-cmd-quiet"), cmdEvent()); err == nil || err.Error() != "command exited 4" {
		t.Fatalf("got %v", err)
	}
}

// A program that outlives its attempt is killed and its event dropped, well
// within the attempt's bound and WaitDelay.
func TestACommandThatNeverExitsIsKilled(t *testing.T) {
	cmdFake(t, "rota-cmd-hang", fakecli.Spec{Sleep: "10s"})
	snd := cmdSink(t, nil, "rota-cmd-hang")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := snd.send(ctx, cmdEvent())
	took := time.Since(start)
	var d *drop
	if !errors.As(err, &d) || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("got %#v", err)
	}
	if took > 3*time.Second {
		t.Fatalf("the attempt took %v", took)
	}
}

// Through the notifier: the attempt's own bound kills the program, the sink
// is failing, and the event is dropped.
func TestAHangingCommandIsBoundedByTheAttempt(t *testing.T) {
	quick(t)
	attemptTimeout = 200 * time.Millisecond
	cmdFake(t, "rota-cmd-stuck", fakecli.Spec{Sleep: "10s"})
	d := &dialer{plan: []any{newSource(ev(1, events.AccountDead))}}
	sink := api.EventSink{Type: "command", Command: []string{"rota-cmd-stuck"}}
	start := time.Now()
	n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{sink}}, Dial: d.dial})
	eventually(t, "the event to be dropped", func() bool { return statusOf(t, n, "command").Dropped == 1 })
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("dropped after %v", took)
	}
	if s := statusOf(t, n, "command"); s.State != stateFailing || !strings.Contains(s.LastError, "killed") {
		t.Fatalf("%+v", s)
	}
}

// A program that leaves a helper holding its stderr does not hold the
// attempt past WaitDelay: it exited 0, so the event was delivered.
func TestAHelperHoldingStderrDoesNotHoldTheAttempt(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "helper.pid")
	helper := fakecli.Install(t, dir, "rota-cmd-helper", fakecli.Spec{Sleep: "20s", Ready: ready})
	cmdFake(t, "rota-cmd-detach", fakecli.Spec{Detach: helper})
	t.Cleanup(func() {
		if p, err := os.FindProcess(fakecli.WaitReady(t, ready)); err == nil {
			_ = p.Kill()
		}
	})
	start := time.Now()
	if err := cmdSend(t, cmdSink(t, nil, "rota-cmd-detach"), cmdEvent()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("the attempt took %v", took)
	}
}

// A program that is not there fails its sink when it is made, with exec's
// own words, and never the daemon.
func TestAMissingProgramFailsTheSinkNotTheDaemon(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := newCommand(api.EventSink{Type: "command", Command: []string{"rota-no-such-program"}}, env{})
	if err == nil || !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "rota-no-such-program") {
		t.Fatalf("the error does not name the program: %v", err)
	}
	// An empty command, or one relative to wherever the daemon runs, is
	// refused too.
	for _, argv := range [][]string{nil, {""}, {filepath.Join("bin", "x")}} {
		if _, err := newCommand(api.EventSink{Type: "command", Command: argv}, env{}); err == nil {
			t.Fatalf("%q was made", argv)
		}
	}
	// A name found only through a relative PATH entry is refused (exec.ErrDot).
	dir := t.TempDir()
	fakecli.Install(t, dir, "rota-cmd-dot", fakecli.Spec{})
	t.Chdir(filepath.Dir(dir))
	t.Setenv("PATH", filepath.Base(dir))
	if _, err := newCommand(api.EventSink{Type: "command", Command: []string{"rota-cmd-dot"}}, env{}); !errors.Is(err, exec.ErrDot) {
		t.Fatalf("got %v", err)
	}
	// Through the notifier the sink is failing and the others carry on.
	t.Setenv("PATH", t.TempDir())
	quick(t)
	other := &fakeSender{}
	useSenders(t, map[string]any{"webhook": other, "command": newCommand})
	d := &dialer{plan: []any{newSource(ev(1, events.AccountDead))}}
	n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{
		{Type: "command", Command: []string{"rota-no-such-program"}}, {Type: "webhook", URL: "https://h.example/x"},
	}}, Dial: d.dial})
	eventually(t, "the webhook to send", func() bool { return len(other.delivered()) == 1 })
	if s := statusOf(t, n, "command"); s.State != stateFailing || !strings.Contains(s.LastError, "rota-no-such-program") {
		t.Fatalf("%+v", s)
	}
}

// An absolute path is used as it is, with no PATH at all.
func TestAnAbsoluteProgramNeedsNoPath(t *testing.T) {
	got := filepath.Join(t.TempDir(), "stdin")
	path := fakecli.Install(t, t.TempDir(), "rota-cmd-abs", fakecli.Spec{StdinTo: got})
	t.Setenv("PATH", t.TempDir())
	if err := cmdSend(t, cmdSink(t, nil, path), cmdEvent()); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(got); err != nil || len(raw) == 0 {
		t.Fatalf("%q %v", raw, err)
	}
}
