package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
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
	"github.com/professor93/rota/internal/fakecli"
)

// launchExec is Options.Exec over a fake rota: each run asked for is
// recorded, with the command it was given.
type launchExec struct {
	mu    sync.Mutex
	path  string // the fake
	asked [][]string
	made  []*exec.Cmd
	// make, when set, makes the command instead of the fake.
	make func(args ...string) *exec.Cmd
}

func (x *launchExec) exec(args ...string) *exec.Cmd {
	var c *exec.Cmd
	if x.make != nil {
		c = x.make(args...)
	} else {
		c = exec.Command(x.path, args...)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.asked = append(x.asked, slices.Clone(args))
	x.made = append(x.made, c)
	return c
}

func (x *launchExec) runs() [][]string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.asked)
}

// commands are the commands Exec made, in order.
func (x *launchExec) commands() []*exec.Cmd {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.made)
}

// launchFake installs a fake rota that does what spec says, on a PATH that
// holds it alone, and gives the Exec that runs it.
func launchFake(t *testing.T, spec fakecli.Spec) *launchExec {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	return &launchExec{path: fakecli.Install(t, bin, "rota", spec)}
}

// launchHandedOff is the one hand-off among what the commands produced.
func launchHandedOff(t *testing.T, seen []tea.Msg) handOffMsg {
	t.Helper()
	var got []handOffMsg
	for _, msg := range seen {
		if h, ok := msg.(handOffMsg); ok {
			got = append(got, h)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d hand-offs, want 1, among %v", len(got), seen)
	}
	if got[0].Cmd == nil || got[0].Done == nil {
		t.Fatalf("the hand-off is %+v", got[0])
	}
	return got[0]
}

// launchPlay does what Bubble Tea does with a hand-off: it gives the
// command the terminal's streams, runs it, and gives back what it wrote.
func launchPlay(h handOffMsg) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	h.Cmd.SetStdin(strings.NewReader(""))
	h.Cmd.SetStdout(&out)
	h.Cmd.SetStderr(&errOut)
	err = h.Cmd.Run()
	return out.String(), errOut.String(), err
}

// launchToasts are the toasts among what the commands produced.
func launchToasts(seen []tea.Msg) []toastMsg {
	var out []toastMsg
	for _, msg := range seen {
		if t, ok := msg.(toastMsg); ok {
			out = append(out, t)
		}
	}
	return out
}

// launchStale is how many reads of what is stale b has taken.
func launchStale(b *fakeBackend) int {
	n := 0
	for _, c := range b.named("Accounts") {
		if c.String() == "Accounts(stale)" {
			n++
		}
	}
	return n
}

func TestEnterRunsTheCLIOnTheTerminal(t *testing.T) {
	x := launchFake(t, fakecli.Spec{Stdout: []string{"ARGS={{args}}"}})
	// Attach is set, but no daemon runs here: the CLI runs on this terminal.
	m, b := accountsModel(t, accountsThree(), func(o *Options) {
		o.Exec = x.exec
		o.Attach = func(context.Context, int, func() (int, int), <-chan struct{}) (Attached, error) {
			t.Error("an attach was asked for with no daemon running here")
			return nil, errors.New("no daemon")
		}
	})
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	// Update starts nothing: the run is made by the command it returns.
	if _, cmd := m.Update(press("enter")[0]); cmd == nil || len(x.runs()) != 0 {
		t.Fatalf("Update gave %v and made %q", cmd, x.runs())
	}
	stale := launchStale(b)
	_, seen := run(t, m, press("enter")...)
	h := launchHandedOff(t, seen)
	// Natural by default: the run is rota run N and nothing more, made as
	// Exec made it.
	if got := x.runs(); len(got) != 1 || !slices.Equal(got[0], []string{"run", "1"}) {
		t.Fatalf("Exec was asked for %q, want one run 1", got)
	}
	tl, ok := h.Cmd.(*tailed)
	if !ok || tl.c != x.commands()[0] {
		t.Fatalf("the hand-off is %T, not the command Exec made, tailed", h.Cmd)
	}
	if c := tl.c; c.Env != nil || c.Dir != "" || !slices.Equal(c.Args[1:], []string{"run", "1"}) {
		t.Errorf("the command was changed: args %q, env %q, dir %q", c.Args, c.Env, c.Dir)
	}
	if n := launchStale(b); n != stale {
		t.Errorf("the listing was read while the terminal was handed over")
	}
	out, _, err := launchPlay(h)
	if err != nil || !strings.Contains(out, "ARGS=run 1") {
		t.Fatalf("the run ended %v, saying %q", err, out)
	}
	// A process the CLI leaves holding its stderr cannot keep the TUI away.
	if d := tl.c.WaitDelay; d != 2*time.Second {
		t.Errorf("the run waits %v for what it left behind, want 2s", d)
	}
	before := plain(m)
	_, seen = run(t, m, h.Done(nil))
	if n := launchStale(b); n != stale+1 {
		t.Errorf("after the run the stale reads went from %d to %d, want one more", stale, n)
	}
	if got := launchToasts(seen); len(got) != 0 {
		t.Errorf("a run that went well said %+v", got)
	}
	if after := plain(m); after != before {
		t.Errorf("the screen changed after a run that went well:\n%s\nthen\n%s", before, after)
	}
	// The selection, and with it what ⏎ runs, is where it was.
	_, seen = run(t, m, press("down", "enter")...)
	launchHandedOff(t, seen)
	if got := x.runs(); len(got) != 2 || !slices.Equal(got[1], []string{"run", "2"}) {
		t.Errorf("after moving down Exec was asked for %q", got)
	}
	// With a daemon running and Attach set, a live account takes the held
	// path. Until Task 19 that path runs the CLI here, and it does run: a
	// held path that did nothing would leave ⏎ dead wherever a daemon runs.
	t.Run("held", func(t *testing.T) {
		x := launchFake(t, fakecli.Spec{})
		var attached atomic.Bool
		m, b := accountsModel(t, accountsThree(), func(o *Options) {
			o.Exec = x.exec
			o.Daemon = func(context.Context) Daemon { return Daemon{Running: true} }
			o.Attach = func(context.Context, int, func() (int, int), <-chan struct{}) (Attached, error) {
				attached.Store(true)
				return nil, errors.New("not until Task 19")
			}
		})
		run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
		if !m.e.daemon.Running {
			t.Fatal("the daemon is not running")
		}
		_, seen := run(t, m, press("enter")...)
		h := launchHandedOff(t, seen)
		if got := x.runs(); len(got) != 1 || !slices.Equal(got[0], []string{"run", "1"}) {
			t.Fatalf("Exec was asked for %q, want one run 1", got)
		}
		if attached.Load() {
			t.Error("an attach was asked for before Task 19 makes one")
		}
		if _, _, err := launchPlay(h); err != nil {
			t.Fatalf("the run ended %v", err)
		}
		stale := launchStale(b)
		run(t, m, h.Done(nil))
		if n := launchStale(b); n != stale+1 {
			t.Errorf("after the run the stale reads went from %d to %d, want one more", stale, n)
		}
	})
}

func TestAFailedRunSaysWhy(t *testing.T) {
	secret := "sk-ant-api03-" + strings.Repeat("C", 40)
	cases := []struct {
		name string
		spec fakecli.Spec
		want string
	}{
		{"its last words", fakecli.Spec{Stderr: []string{"first", "boom", ""}, Exit: 3}, "rota run 1 exited 3: boom"},
		{"nothing said", fakecli.Spec{Exit: 2}, "rota run 1 exited 2"},
		{"hostile words", fakecli.Spec{Stderr: []string{"boom \x1b[2J " + secret}, Exit: 3}, "rota run 1 exited 3: boom \x1b[2J " + secret},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := launchFake(t, c.spec)
			m, b := accountsModel(t, accountsThree(), func(o *Options) { o.Exec = x.exec })
			run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
			stale := launchStale(b)
			_, seen := run(t, m, press("enter")...)
			h := launchHandedOff(t, seen)
			_, errOut, err := launchPlay(h)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != c.spec.Exit {
				t.Fatalf("the run ended %v, want exit %d", err, c.spec.Exit)
			}
			// What the CLI said still reached the terminal.
			for _, line := range c.spec.Stderr {
				if !strings.Contains(errOut, line) {
					t.Errorf("the terminal did not get %q: %q", line, errOut)
				}
			}
			_, seen = run(t, m, h.Done(err))
			if got := launchToasts(seen); len(got) != 1 || got[0] != (toastMsg{Text: c.want, Level: levelErr}) {
				t.Errorf("the toasts are %+v, want one error %q", got, c.want)
			}
			if n := launchStale(b); n != stale+1 {
				t.Errorf("after a failed run the stale reads went from %d to %d, want one more", stale, n)
			}
			// It reaches the screen, made safe to draw.
			if v := m.View().Content; strings.Contains(v, "\x1b[2J") || strings.Contains(v, secret) {
				t.Errorf("the screen holds what the CLI wrote raw:\n%q", v)
			}
			if p := plain(m); !strings.Contains(p, strings.SplitN(c.want, " \x1b", 2)[0]) {
				t.Errorf("the screen does not say %q:\n%s", c.want, p)
			}
		})
	}
	t.Run("killed", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("a process killed on Windows exits 1; the exit cases cover it")
		}
		ready := filepath.Join(t.TempDir(), "ready")
		x := launchFake(t, fakecli.Spec{Ready: ready, Sleep: "30s"})
		m, b := accountsModel(t, accountsThree(), func(o *Options) { o.Exec = x.exec })
		run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
		stale := launchStale(b)
		_, seen := run(t, m, press("enter")...)
		h := launchHandedOff(t, seen)
		ended := make(chan error, 1)
		go func() { _, _, err := launchPlay(h); ended <- err }()
		p, err := os.FindProcess(fakecli.WaitReady(t, ready))
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Kill(); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-ended:
		case <-time.After(20 * time.Second):
			t.Fatal("the killed run never ended")
		}
		_, seen = run(t, m, h.Done(err))
		if got := launchToasts(seen); len(got) != 1 || got[0] != (toastMsg{Text: "rota run 1 ended (signal: killed)", Level: levelErr}) {
			t.Errorf("the toasts are %+v", got)
		}
		if n := launchStale(b); n != stale+1 {
			t.Errorf("after a killed run the stale reads went from %d to %d, want one more", stale, n)
		}
	})
	// A run that went well, whose terminal then did not come back, is said
	// by that error alone: the CLI's last words belong to a run that went
	// well, and are not added to it.
	t.Run("the terminal did not come back", func(t *testing.T) {
		x := launchFake(t, fakecli.Spec{Stderr: []string{"fine"}})
		m, b := accountsModel(t, accountsThree(), func(o *Options) { o.Exec = x.exec })
		run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
		stale := launchStale(b)
		_, seen := run(t, m, press("enter")...)
		h := launchHandedOff(t, seen)
		if _, _, err := launchPlay(h); err != nil {
			t.Fatalf("the run ended %v", err)
		}
		if last := h.Cmd.(*tailed).Last(); last != "fine" {
			t.Fatalf("the tail holds %q, want fine", last)
		}
		_, seen = run(t, m, h.Done(errors.New("the terminal did not come back")))
		want := toastMsg{Text: "rota run 1: the terminal did not come back", Level: levelErr}
		if got := launchToasts(seen); len(got) != 1 || got[0] != want {
			t.Errorf("the toasts are %+v, want %+v", got, want)
		}
		if n := launchStale(b); n != stale+1 {
			t.Errorf("the stale reads went from %d to %d, want one more", stale, n)
		}
	})
	t.Run("never started", func(t *testing.T) {
		x := launchFake(t, fakecli.Spec{})
		missing := filepath.Join(filepath.Dir(x.path), "missing")
		x.make = func(args ...string) *exec.Cmd { return exec.Command(missing, args...) }
		m, b := accountsModel(t, accountsThree(), func(o *Options) { o.Exec = x.exec })
		run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
		stale := launchStale(b)
		_, seen := run(t, m, press("enter")...)
		h := launchHandedOff(t, seen)
		_, _, err := launchPlay(h)
		if err == nil {
			t.Fatal("a run of nothing went well")
		}
		_, seen = run(t, m, h.Done(err))
		got := launchToasts(seen)
		if len(got) != 1 || got[0].Level != levelErr || got[0].Text != "rota run 1: "+err.Error() {
			t.Errorf("the toasts are %+v, want one error saying %v", got, err)
		}
		if n := launchStale(b); n != stale+1 {
			t.Errorf("the stale reads went from %d to %d, want one more", stale, n)
		}
	})
	t.Run("no command", func(t *testing.T) {
		x := launchFake(t, fakecli.Spec{})
		x.make = func(...string) *exec.Cmd { return nil }
		m, b := accountsModel(t, accountsThree(), func(o *Options) { o.Exec = x.exec })
		run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
		stale := launchStale(b)
		_, seen := run(t, m, press("enter")...)
		for _, msg := range seen {
			if _, ok := msg.(handOffMsg); ok {
				t.Errorf("nothing to run was handed the terminal")
			}
		}
		if got := launchToasts(seen); len(got) != 1 || got[0] != (toastMsg{Text: "rota run 1 could not be started", Level: levelErr}) {
			t.Errorf("the toasts are %+v", got)
		}
		if n := launchStale(b); n != stale {
			t.Errorf("the listing was read again though nothing ran")
		}
	})
}

func TestADeadAccountIsNotRun(t *testing.T) {
	x := launchFake(t, fakecli.Spec{})
	m, _ := accountsModel(t, accountsThree(), func(o *Options) {
		o.Exec = x.exec
		// Not even into a held session.
		o.Daemon = func(context.Context) Daemon { return Daemon{Running: true} }
		o.Attach = func(context.Context, int, func() (int, int), <-chan struct{}) (Attached, error) {
			t.Error("a dead account was attached")
			return nil, errors.New("dead")
		}
	})
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	if !m.e.daemon.Running {
		t.Fatal("the daemon is not running")
	}
	_, seen := run(t, m, selectMsg{Account: 3}, press("enter")[0])
	for _, msg := range seen {
		if _, ok := msg.(handOffMsg); ok {
			t.Errorf("a dead account was handed the terminal")
		}
	}
	want := toastMsg{Text: "account 3 is dead: l signs it in again", Level: levelWarn}
	if got := launchToasts(seen); len(got) != 1 || got[0] != want {
		t.Errorf("the toasts are %+v, want %+v", got, want)
	}
	if got := x.runs(); len(got) != 0 {
		t.Errorf("Exec was asked for %q", got)
	}
	if p := plain(m); !strings.Contains(p, want.Text) {
		t.Errorf("the screen does not say it:\n%s", p)
	}
}

func TestWithoutATerminalItSaysSo(t *testing.T) {
	const want = "no terminal here to hand over; Sessions (a later beta) attaches over SSH"
	// A toast has the width less two columns: at 80 it is said whole.
	if w := textWidth(want); w > 78 {
		t.Fatalf("the toast is %d columns, more than 80 gives it", w)
	}
	for _, remote := range []bool{true, false} {
		t.Run(map[bool]string{true: "over SSH", false: "no Exec"}[remote], func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			m, b := accountsModel(t, accountsThree(), func(o *Options) { o.Remote = remote })
			run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
			stale := launchStale(b)
			_, seen := run(t, m, press("enter")...)
			for _, msg := range seen {
				if _, ok := msg.(handOffMsg); ok {
					t.Errorf("the terminal was handed over with none to hand")
				}
			}
			if got := launchToasts(seen); len(got) != 1 || got[0] != (toastMsg{Text: want, Level: levelWarn}) {
				t.Errorf("the toasts are %+v, want a warning %q", got, want)
			}
			if p := plain(m); !strings.Contains(p, want) {
				t.Errorf("the screen does not say it:\n%s", p)
			}
			if n := launchStale(b); n != stale {
				t.Errorf("the listing was read again though nothing ran")
			}
		})
	}
}
