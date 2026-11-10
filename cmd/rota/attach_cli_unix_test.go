//go:build linux || darwin

package main

import (
	"bytes"
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
// thing drawn is every mode reset and then the line saying how it ended.
func TestAttachGivesTheTerminalBack(t *testing.T) {
	r := newDaemonRig(t)
	// A CLI that exits 3 when it is told bye.
	bin := holdFake(t, "claude", fakecli.Spec{Exit: 3})
	r.env = append(r.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.start()

	// The line this attach was to end with, after the reset.
	endsWith := func(a *attachProc, line string) {
		t.Helper()
		a.waitAfter(0, line, 10*time.Second)
		text := a.screen.text()
		i := strings.LastIndex(text, string(vtscan.ResetAll()))
		if i < 0 || !strings.Contains(text[i:], line) {
			t.Fatalf("the terminal's last bytes are not every mode reset and then %q:\n%q", line, text[max(len(text)-400, 0):])
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
	// up, and ends as its record says.
	id := r.newSession("job")
	a = r.attachHere(100, 30, false, "job")
	a.waitAfter(0, "ready 100x30", 10*time.Second)
	if out, errb, code := r.rota("serve", "stop", "--all", "--yes"); code != 0 {
		t.Fatalf("serve stop --all --yes: exit %d %q %q", code, out, errb)
	}
	got := a.end()
	rec := holdwire.Judge(r.terminals, id)
	if rec.State != holdwire.StateEnded || rec.Meta == nil || rec.Meta.Exit == nil {
		t.Fatalf("after stop --all the record reads %+v %+v", rec, rec.Meta)
	}
	t.Logf("stop --all ended the session with %d, and the attach exited %d", *rec.Meta.Exit, got)
	ended := attach.Result{Why: attach.Ended, Session: rotaclient.Session{ID: id, Name: "job"}, Code: *rec.Meta.Exit}
	if got != ended.ExitCode() {
		t.Fatalf("the session ended %d, and rota run --attach exited %d, want %d; the terminal showed:\n%q",
			*rec.Meta.Exit, got, ended.ExitCode(), a.screen.text())
	}
	a.givenBack()
	endsWith(a, ended.Line(attach.Options{}))
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
	if got := a.stderr.String(); !strings.HasPrefix(got, "rota: lost api: ") || !strings.HasSuffix(got, "; rota run --attach api tries again\n") {
		t.Fatalf("stderr %q, want the lost line", got)
	}
	if s := r.session(id); s.State != "live" {
		t.Fatalf("after a signal the session is %q, want live", s.State)
	}
}
