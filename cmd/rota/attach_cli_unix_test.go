//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/professor93/rota/cmd/rota/attach"
	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/pty"
	"github.com/professor93/rota/internal/vtscan"
)

// rota run --attach as a person types it, on the daemon rig: the command is
// this test binary acting as rota in a process of its own, at a terminal of
// its own, so is the daemon and each holder, and the CLI in a held session
// is the tty fake.

// attachProc is one `rota run --attach` at a pseudo-terminal: what the
// window shows, the terminal's two ends, and its standard error, which is a
// pipe of its own or the terminal too. The test keeps the slave open, so
// the terminal's settings are still there to read after the command ends.
type attachProc struct {
	t             *testing.T
	cmd           *exec.Cmd
	master, slave *os.File
	screen        *shownOn
	stderr        *attachErr // nil when standard error is the terminal
	exited        chan struct{}
	before        syscall.Termios
}

// attachErr is a standard error read once the command has ended.
type attachErr struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *attachErr) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *attachErr) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// attachHere starts `rota run --attach` and args at a cols by rows terminal
// it leads, as a shell starts a command; with pipe its standard error is a
// pipe rather than the terminal.
func (r *daemonRig) attachHere(cols, rows uint16, pipe bool, args ...string) *attachProc {
	r.t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		r.t.Fatal(err)
	}
	if err := pty.Resize(master, cols, rows); err != nil {
		r.t.Fatal(err)
	}
	a := &attachProc{t: r.t, master: master, slave: slave, exited: make(chan struct{})}
	a.before = attachTermios(r.t, master)
	a.cmd = r.cmd(append([]string{"run", "--attach"}, args...)...)
	a.cmd.Stdin, a.cmd.Stdout, a.cmd.Stderr = slave, slave, slave
	if pipe {
		a.stderr = &attachErr{}
		a.cmd.Stderr = a.stderr
	}
	a.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := a.cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	go func() { _ = a.cmd.Wait(); close(a.exited) }()
	a.screen = watchScreen(master)
	r.t.Cleanup(func() {
		_ = a.cmd.Process.Kill()
		<-a.exited
		master.Close()
		slave.Close()
	})
	return a
}

// typ types s at the terminal.
func (a *attachProc) typ(s string) {
	a.t.Helper()
	if _, err := a.master.Write([]byte(s)); err != nil {
		a.t.Fatal(err)
	}
}

// waitAfter waits until the window has shown want after its first mark
// bytes.
func (a *attachProc) waitAfter(mark int, want string, within time.Duration) {
	a.t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if strings.Contains(a.screen.text()[mark:], want) {
			return
		}
	}
	a.t.Fatalf("%q never appeared at the terminal within %s; it showed:\n%q", want, within, a.screen.text()[mark:])
}

// end waits for the command to end, and says its exit code.
func (a *attachProc) end() int {
	a.t.Helper()
	select {
	case <-a.exited:
	case <-time.After(60 * time.Second):
		a.t.Fatalf("rota run --attach never ended; the terminal showed:\n%q", a.screen.text())
	}
	return a.cmd.ProcessState.ExitCode()
}

// givenBack says the terminal's settings are the ones it had before the
// command: line editing and echo on, and everything else as it was.
func (a *attachProc) givenBack() {
	a.t.Helper()
	now := attachTermios(a.t, a.master)
	if now.Lflag&syscall.ICANON == 0 || now.Lflag&syscall.ECHO == 0 {
		a.t.Fatalf("the terminal was left with ICANON %v and ECHO %v", now.Lflag&syscall.ICANON != 0, now.Lflag&syscall.ECHO != 0)
	}
	if now.Iflag != a.before.Iflag || now.Oflag != a.before.Oflag || now.Lflag != a.before.Lflag || now.Cflag != a.before.Cflag {
		a.t.Fatalf("the terminal's settings were %+v and are left %+v", a.before, now)
	}
}

// attachTermios reads f's terminal settings with the ioctl noecho_unix.go
// uses, through f's own descriptor so a read under way is not disturbed.
func attachTermios(t *testing.T, f *os.File) syscall.Termios {
	t.Helper()
	conn, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var (
		tio syscall.Termios
		ok  bool
	)
	if err := conn.Control(func(fd uintptr) { tio, ok = termios(fd) }); err != nil || !ok {
		t.Fatalf("reading the terminal's settings: %v", err)
	}
	return tio
}

// attachScreen is the window as a terminal would draw what it was sent,
// one string per row.
func attachScreen(t *testing.T, sent string, cols, rows int) []string {
	t.Helper()
	em := vt.NewEmulator(cols, rows)
	go func() { _, _ = io.Copy(io.Discard, em) }() // the replies it owes nobody
	// Ended by closing the replies' writer, as hold's drain is: Close
	// writes a field Read looks at without a lock.
	defer func() {
		if c, ok := em.InputPipe().(io.Closer); ok {
			_ = c.Close()
		}
	}()
	if _, err := em.Write([]byte(sent)); err != nil {
		t.Fatal(err)
	}
	return strings.Split(em.String(), "\n")
}

/* ---------------------------------------------------------------- the tests --- */

// A held session at this terminal: what it shows is here, what is typed
// reaches it, and Ctrl-\ d leaves with the line saying how to come back,
// the terminal as it was and the session still running.
func TestAttachRunsTheSessionHere(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	id := r.newSession("api")
	a := r.attachHere(100, 30, true, "api")
	a.waitAfter(0, "ready 100x30", 10*time.Second)
	a.typ("hi\r")
	a.waitAfter(0, "got:hi", 10*time.Second)
	a.typ("\x1cd")
	if code := a.end(); code != 0 {
		t.Fatalf("Ctrl-\\ d: exit %d, want 0; stderr %q", code, a.stderr.String())
	}
	if got, want := a.stderr.String(), "rota: detached from api; rota run --attach api returns to it\n"; got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
	a.givenBack()
	if s := r.session(id); s.State != "live" {
		t.Fatalf("after a detach the session is %q, want live", s.State)
	}
	if doc, code := r.status(); code != 0 || doc.Sessions.Live != 1 {
		t.Fatalf("serve status after a detach: exit %d, %d live", code, doc.Sessions.Live)
	}
}

// The session ends while it is shown, by its CLI exiting and by `serve stop
// --all` from elsewhere: each time the command exits with the code the
// session ended with, the terminal has its settings back, and the last
// thing drawn is every mode reset and then, below the window's last row,
// the line saying how it ended, written once the terminal was given back.
func TestAttachGivesTheTerminalBack(t *testing.T) {
	r := newDaemonRig(t)
	// A CLI that exits 3 when it is told bye.
	bin := holdFake(t, "claude", fakecli.Spec{Exit: 3})
	r.env = append(r.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.start()

	// The line this attach was to end with, after the reset: a newline
	// first, so it is not written over the session's last row, and its own
	// newline drawn as a carriage return and a newline, which only a
	// terminal given back does.
	endsWith := func(a *attachProc, line string) {
		t.Helper()
		a.waitAfter(0, line, 10*time.Second)
		text := a.screen.text()
		i := strings.LastIndex(text, string(vtscan.ResetAll()))
		if i < 0 || !strings.Contains(text[i:], "\r\n"+line+"\r\n") {
			t.Fatalf("the terminal's last bytes are not every mode reset and then %q on a line of its own:\n%q", line, text[max(len(text)-400, 0):])
		}
	}

	// Its CLI exits, with a code of its own.
	r.newSession("api")
	a := r.attachHere(100, 30, false, "api")
	a.waitAfter(0, "ready 100x30", 10*time.Second)
	a.typ("bye\r")
	if code := a.end(); code != 3 {
		t.Fatalf("the session's CLI exited 3, and rota run --attach exited %d; the terminal showed:\n%q", code, a.screen.text())
	}
	a.givenBack()
	endsWith(a, "rota: api ended, exit 3")

	// The daemon and every session stopped from elsewhere: the CLI is hung
	// up, dies of it with no exit code, and the attach says so.
	id := r.newSession("job")
	a = r.attachHere(100, 30, false, "job")
	a.waitAfter(0, "ready 100x30", 10*time.Second)
	if out, errb, code := r.rota("serve", "stop", "--all", "--yes"); code != 0 {
		t.Fatalf("serve stop --all --yes: exit %d %q %q", code, out, errb)
	}
	got := a.end()
	rec := holdwire.Judge(r.terminals, id)
	if rec.State != holdwire.StateEnded || rec.Meta == nil || rec.Meta.Exit == nil || *rec.Meta.Exit != -1 {
		t.Fatalf("after stop --all the record reads %+v %+v; the tty fake dies of its hang-up, -1", rec, rec.Meta)
	}
	if got != 1 {
		t.Fatalf("the session ended with no exit code, and rota run --attach exited %d, want 1; the terminal showed:\n%q", got, a.screen.text())
	}
	a.givenBack()
	endsWith(a, "rota: job ended with no exit code: a signal stopped its CLI, or its holder went")
}

// A session that has ended, named by its id, is not attached to: the
// command says so and exits 1, as it does for a name that finds no session
// at all, and the terminal is left as it was.
func TestAttachRefusesAnEndedSession(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	id := r.newSession("api")
	p := r.attach(id)
	p.waitOut("ready 100x30")
	p.send(0x2, []byte("bye\r"))
	for deadline := time.Now().Add(10 * time.Second); r.session(id).State == "live"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the session never ended after bye")
		}
	}
	for _, c := range []struct{ name, want string }{
		{id, "rota: session api has ended; rota list --sessions shows it\n"},
		{"nosuch", `rota: no session is "nosuch": it is not an id, the start of one, or the name of a live session` + "\n"},
	} {
		a := r.attachHere(100, 30, true, c.name)
		if code := a.end(); code != 1 {
			t.Fatalf("rota run --attach %s: exit %d, want 1; stderr %q", c.name, code, a.stderr.String())
		}
		if got := a.stderr.String(); got != c.want {
			t.Fatalf("rota run --attach %s: stderr %q, want %q", c.name, got, c.want)
		}
		a.givenBack()
	}
}

// A panic while the attach has the terminal gives it back before the panic
// goes on: the session's modes off, the pen and scroll region reset and the
// cursor below the window, then the terminal's own settings. The attach is
// run in this process, so that it can be made to panic.
func TestAPanicGivesTheTerminalBack(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	r.newSession("api")
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	if err := pty.Resize(master, 100, 30); err != nil {
		t.Fatal(err)
	}
	before := attachTermios(t, master)
	screen := watchScreen(master)

	wasRun, wasIn := attachRun, stdin
	t.Cleanup(func() { attachRun, stdin = wasRun, wasIn })
	stdin = slave
	attachRun = func(_ context.Context, term attach.Terminal, _ *rotaclient.Client, _ string, _ attach.Options) attach.Result {
		// What a session leaves on: the alternate screen, the mouse, the
		// cursor hidden.
		_, _ = term.Write([]byte("\x1b[?1049h\x1b[?1000h\x1b[?25lsession"))
		panic("boom")
	}
	var errb bytes.Buffer
	got := func() (p any) {
		defer func() { p = recover() }()
		run([]string{"run", "--attach", "api"}, slave, &errb)
		return nil
	}()
	if got != "boom" {
		t.Fatalf("the panic went on as %v, want boom", got)
	}
	want := string(attach.PutBack(30)) + "\r\n"
	screen.wait(t, "session"+want)
	if now := attachTermios(t, master); now.Iflag != before.Iflag || now.Oflag != before.Oflag || now.Lflag != before.Lflag || now.Cflag != before.Cflag {
		t.Fatalf("after a panic the terminal's settings were %+v and are left %+v", before, now)
	}
}

// The daemon stopped and started again under an attach: the attach waits
// for it, the window is repainted, what is typed then reaches the session
// once, and nothing typed while it was gone reaches it at all.
func TestAttachSurvivesADaemonRestart(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	r.newSession("api")
	a := r.attachHere(100, 30, true, "api")
	a.waitAfter(0, "ready 100x30", 10*time.Second)
	if _, errb, code := r.rota("serve", "stop"); code != 0 {
		t.Fatalf("serve stop: exit %d %q", code, errb)
	}
	a.waitAfter(0, "the rota daemon went away", 10*time.Second)
	mark := len(a.screen.text())
	a.typ("lost\r")
	time.Sleep(100 * time.Millisecond)
	r.start()
	a.waitAfter(mark, "ready 100x30", 10*time.Second) // the repaint
	a.typ("x\r")
	a.waitAfter(mark, "got:x", 10*time.Second)
	// Long enough for a second x, or a late lost, to have come back.
	time.Sleep(300 * time.Millisecond)
	text := a.screen.text()
	if strings.Contains(text, "got:lost") || strings.Contains(text, "lostx") {
		t.Fatalf("what was typed while the daemon was gone reached the session:\n%q", text)
	}
	xs := 0
	for _, row := range attachScreen(t, text, 100, 30) {
		if strings.TrimSpace(row) == "got:x" {
			xs++
		}
	}
	if xs != 1 {
		t.Fatalf("x reached the session %d times, want once; the window shows:\n%s", xs, strings.Join(attachScreen(t, text, 100, 30), "\n"))
	}
	a.typ("\x1cd")
	if code := a.end(); code != 0 {
		t.Fatalf("Ctrl-\\ d after the restart: exit %d; stderr %q", code, a.stderr.String())
	}
	a.givenBack()
}

// With no daemon there is no session to attach to: it says so and how to
// hold one, and leaves the terminal as it was.
func TestAttachWithNoDaemon(t *testing.T) {
	r := newDaemonRig(t)
	a := r.attachHere(100, 30, true, "api")
	if code := a.end(); code != 1 {
		t.Fatalf("with no daemon: exit %d, want 1; stderr %q", code, a.stderr.String())
	}
	want := "rota: no rota daemon is running for " + r.home + ", so there is no session to attach to; rota run <id> --detach starts one\n"
	if got := a.stderr.String(); got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
	a.givenBack()
	if _, ok := holdwire.RunningDaemon(r.terminals); ok {
		t.Fatal("--attach started a daemon")
	}
}

// The attach leaves on the daemon's key, Ctrl-\, and on no other: Ctrl-]
// and then d are typed through as any keys are, and the attach goes on;
// Ctrl-\ d leaves. (Ctrl-\ twice, which types Ctrl-\ itself, is the attach
// loop's to pin: the tty fake reads in cooked mode, where that byte is
// SIGQUIT.)
func TestAttachLeavesOnTheDaemonsKey(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	r.newSession("api")
	a := r.attachHere(100, 30, true, "api")
	a.waitAfter(0, "ready 100x30", 10*time.Second)
	a.typ("a\x1dd\r")
	a.waitAfter(0, `got:a\x1dd`, 10*time.Second)
	select {
	case <-a.exited:
		t.Fatalf("Ctrl-] d left the attach; stderr %q", a.stderr.String())
	default:
	}
	if attach.DaemonKey(r.terminals) != attach.DefaultKey {
		t.Fatalf("the daemon's key is %s", attach.KeyName(attach.DaemonKey(r.terminals)))
	}
	a.typ("\x1cd")
	if code := a.end(); code != 0 {
		t.Fatalf("Ctrl-\\ d: exit %d; stderr %q", code, a.stderr.String())
	}
	if !strings.HasPrefix(a.stderr.String(), "rota: detached from api;") {
		t.Fatalf("stderr %q", a.stderr.String())
	}
}

// SIGTERM to an attach ends it, with its terminal given back and a line
// saying the session was lost, not ended; the session goes on.
func TestASignalGivesTheTerminalBack(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	id := r.newSession("api")
	a := r.attachHere(100, 30, true, "api")
	a.waitAfter(0, "ready 100x30", 10*time.Second)
	if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := a.end(); code != 1 {
		t.Fatalf("SIGTERM: exit %d, want 1; stderr %q", code, a.stderr.String())
	}
	a.givenBack()
	if got, want := a.stderr.String(), "rota: lost api: rota was signalled to stop; rota run --attach api tries again\n"; got != want {
		t.Fatalf("stderr %q, want %q", got, want)
	}
	if s := r.session(id); s.State != "live" {
		t.Fatalf("after a signal the session is %q, want live", s.State)
	}
}

// An attach run inside the held session it names would show that session
// its own output, which it prints again, without end: it is refused, by the
// session's id, its name or the start of its id, as a session that is not
// there is, exit 1 and the terminal untouched. Another session is attached
// to as ever, told which one this terminal is inside, so that n and p pass
// it over. The attach runs in this process, put inside the session by the
// parent its walk up the processes is told.
func TestAttachRefusesTheSessionItIsIn(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	id := r.newSession("api")
	other := r.newSession("job")
	rec := holdwire.Judge(r.terminals, id)
	if rec.Meta == nil || rec.Meta.HolderPID <= 0 {
		t.Fatalf("session api's record reads %+v", rec)
	}
	self := os.Getpid()
	wasParent := parentOf
	t.Cleanup(func() { parentOf = wasParent })
	parentOf = func(pid int) (int, error) {
		if pid == self {
			return rec.Meta.HolderPID, nil
		}
		return wasParent(pid)
	}

	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	if err := pty.Resize(master, 100, 30); err != nil {
		t.Fatal(err)
	}
	before := attachTermios(t, master)
	screen := watchScreen(master)

	wasRun, wasIn := attachRun, stdin
	t.Cleanup(func() { attachRun, stdin = wasRun, wasIn })
	stdin = slave
	var (
		ran     []string
		options attach.Options
	)
	attachRun = func(_ context.Context, _ attach.Terminal, _ *rotaclient.Client, to string, o attach.Options) attach.Result {
		ran, options = append(ran, to), o
		return attach.Result{Why: attach.Detached}
	}

	for _, name := range []string{id, "api", id[:6]} {
		var errb bytes.Buffer
		if code := run([]string{"run", "--attach", name}, slave, &errb); code != 1 {
			t.Fatalf("rota run --attach %s from inside it: exit %d, want 1; stderr %q", name, code, errb.String())
		}
		if got, want := errb.String(), "rota: you are inside api; attaching to it would feed it its own output\n"; got != want {
			t.Fatalf("rota run --attach %s from inside it: stderr %q, want %q", name, got, want)
		}
	}
	if len(ran) != 0 {
		t.Fatalf("the session this terminal is in was attached to: %v", ran)
	}
	time.Sleep(100 * time.Millisecond) // for a late write to show
	if text := screen.text(); text != "" {
		t.Fatalf("a refused attach wrote to the terminal: %q", text)
	}
	if now := attachTermios(t, master); now.Iflag != before.Iflag || now.Oflag != before.Oflag || now.Lflag != before.Lflag || now.Cflag != before.Cflag {
		t.Fatalf("a refused attach left the terminal's settings %+v; they were %+v", now, before)
	}

	var errb bytes.Buffer
	if code := run([]string{"run", "--attach", "job"}, slave, &errb); code != 0 {
		t.Fatalf("rota run --attach job from inside api: exit %d; stderr %q", code, errb.String())
	}
	if len(ran) != 1 || ran[0] != other || options.Inside != id {
		t.Fatalf("from inside api, job was attached as %v with Inside %q; want %s inside %s", ran, options.Inside, other, id)
	}
}
