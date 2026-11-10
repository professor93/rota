//go:build linux || darwin

package attach

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/fakecli"
)

// commandExec is what bubbletea's tea.Exec runs, spelt out here so that
// this package needs no bubbletea to say Cmd is one.
type commandExec interface {
	Run() error
	SetStdin(io.Reader)
	SetStdout(io.Writer)
	SetStderr(io.Writer)
}

var _ commandExec = (*Cmd)(nil)

// commandMe is the principal the in-process server answers.
var commandMe = api.Principal{Name: "me", Role: api.RoleControl, Via: "user"}

// commandRig is a real server with the terminal group on, reached in
// process as commandMe, over a store whose one account runs the tty fake as
// claude, and one session of it at cols by rows. It is Task 1's rig.
func commandRig(t *testing.T, cols, rows uint16) (c *client.Client, id string) {
	t.Helper()
	// Short, rather than t.TempDir: the store may hold a unix socket.
	home, err := os.MkdirTemp("", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	work, err := os.MkdirTemp("", "w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	store := `{"accounts":[{"id":1,"provider":"claude","email":"a@x","token":{"accessToken":"t"}}]}`
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROTA_HOME", home)
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Spec{Tty: true})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv, err := api.New(api.Options{Token: "x", Dir: home, Roots: []string{work}, RefreshEvery: -1,
		Routes: &api.Routes{API: true, WebSocket: true, Terminal: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	c, stop := client.InProcess(srv.LocalHandler(&commandMe))
	t.Cleanup(stop)
	s, err := c.Create(client.Create{Account: 1, Cols: cols, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	return c, s.ID
}

// commandBuf is a writer the attach writes to while the test reads it.
type commandBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *commandBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *commandBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// wait waits until what was written holds want.
func (w *commandBuf) wait(t *testing.T, want string) {
	t.Helper()
	w.waitAfter(t, 0, want)
}

// waitAfter waits until what was written after its first mark bytes holds
// want.
func (w *commandBuf) waitAfter(t *testing.T, mark int, want string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if strings.Contains(w.String()[mark:], want) {
			return
		}
	}
	t.Fatalf("%q never came; the screen has %q", want, w.String()[mark:])
}

// commandRun runs x in the background, and gives what Run returned.
func commandRun(x *Cmd) <-chan error {
	done := make(chan error, 1)
	go func() { done <- x.Run() }()
	return done
}

// commandEnd waits for Run to return.
func commandEnd(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned")
	}
	return nil
}

// An attach is a command bubbletea's tea.Exec can run: its streams set, a
// window given for a screen that is not this process's own terminal, and
// Run ending nil when the person leaves, with the line on stderr and the
// Result saying why. What is typed after it is the next reader's.
func TestCommandIsAnExecCommand(t *testing.T) {
	c, id := commandRig(t, 80, 24)
	in, typing, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); typing.Close() })
	var out, errb commandBuf
	x := Command(c, id, Options{})
	x.SetStdin(in)
	x.SetStdout(&out)
	x.SetStderr(&errb)
	x.SetWindow(func() (int, int) { return 80, 24 }, make(chan struct{}))
	done := make(chan error, 1)
	go func() { done <- x.Run() }()

	out.wait(t, "ready 80x24")
	if _, err := typing.Write([]byte("hi\r")); err != nil {
		t.Fatal(err)
	}
	out.wait(t, "got:hi")
	if _, err := typing.Write([]byte("\x1cd")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after Ctrl-\\ d: %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Run never returned after Ctrl-\\ d; the screen has %q", out.String())
	}
	r := x.Result()
	if r.Why != Detached || r.Session.ID != id {
		t.Fatalf("Result %+v, want Detached from %s", r, id)
	}
	if want := r.Line(Options{}) + "\r\n"; !strings.HasSuffix(errb.String(), want) {
		t.Fatalf("stderr %q, want it to end with the line %q", errb.String(), want)
	}
	if !strings.HasPrefix(errb.String(), "rota: detached from ") {
		t.Fatalf("stderr %q says something but the line", errb.String())
	}

	// Run read nothing after it ended: the next key is still in the pipe.
	if _, err := typing.Write([]byte("z")); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		b := make([]byte, 8)
		n, _ := in.Read(b)
		got <- string(b[:n])
	}()
	select {
	case s := <-got:
		if s != "z" {
			t.Fatalf("the next reader read %q, want z", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the key typed after the attach never reached the next reader")
	}
}

// The window SetWindow gives is the one the attach draws for: a 40x12
// window on an 80x24 session is a crop with its note, and once the window
// says it is 80x24 the crop goes and the session is passed through.
func TestCommandFitsTheWindowItIsGiven(t *testing.T) {
	c, id := commandRig(t, 80, 24)
	in, typing, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); typing.Close() })
	var (
		out, errb  commandBuf
		mu         sync.Mutex
		cols, rows = 40, 12
	)
	resized := make(chan struct{}, 1)
	x := Command(c, id, Options{})
	x.SetStdin(in)
	x.SetStdout(&out)
	x.SetStderr(&errb)
	x.SetWindow(func() (int, int) { mu.Lock(); defer mu.Unlock(); return cols, rows }, resized)
	done := commandRun(x)

	out.wait(t, "z fits")
	mark := len(out.String())
	mu.Lock()
	cols, rows = 80, 24
	mu.Unlock()
	resized <- struct{}{}
	out.waitAfter(t, mark, "\x1b[?1049l") // the crop's own screen left
	if _, err := typing.Write([]byte("x\r")); err != nil {
		t.Fatal(err)
	}
	out.waitAfter(t, mark, "got:x")
	after := out.String()[mark:]
	if i := strings.LastIndex(after, "\x1b[?1049l"); strings.Contains(after[i:], "z fits") {
		t.Fatalf("the crop's note is drawn on a window that holds the session: %q", after[i:])
	}
	if _, err := typing.Write([]byte("\x1cd")); err != nil {
		t.Fatal(err)
	}
	if err := commandEnd(t, done); err != nil {
		t.Fatalf("Run after Ctrl-\\ d: %v", err)
	}
}

// With no window set and this process's own terminal for stdin and stdout,
// Cmd attaches it as Local does: raw while it runs, given back after, and
// the line said with a plain newline, since the terminal turns it into a
// carriage return and a newline again.
func TestCommandOnThisTerminalIsRaw(t *testing.T) {
	c, id := commandRig(t, 80, 24)
	master, slave := localPair(t, 80, 24)
	before := localMustTermios(t, slave)
	var screen, errb commandBuf
	go func() { _, _ = io.Copy(&screen, master) }()
	x := Command(c, id, Options{})
	x.SetStdin(slave)
	x.SetStdout(slave)
	x.SetStderr(&errb)
	done := commandRun(x)

	screen.wait(t, "ready 80x24")
	if raw := localMustTermios(t, slave); raw.Lflag&(syscall.ICANON|syscall.ECHO) != 0 {
		t.Fatalf("attached, the terminal still has line editing or echo: lflag %#x", raw.Lflag)
	}
	if _, err := master.Write([]byte("hi\r")); err != nil {
		t.Fatal(err)
	}
	screen.wait(t, "got:hi")
	if _, err := master.Write([]byte("\x1cd")); err != nil {
		t.Fatal(err)
	}
	if err := commandEnd(t, done); err != nil {
		t.Fatalf("Run after Ctrl-\\ d: %v", err)
	}
	now := localMustTermios(t, slave)
	if now.Iflag != before.Iflag || now.Oflag != before.Oflag || now.Lflag != before.Lflag || now.Cflag != before.Cflag {
		t.Fatalf("the terminal's settings were %+v and are left %+v", before, now)
	}
	if want := x.Result().Line(Options{}) + "\n"; !strings.HasSuffix(errb.String(), want) || strings.HasSuffix(errb.String(), "\r\n") {
		t.Fatalf("stderr %q, want it to end with %q", errb.String(), want)
	}
}

// When stdin ends, so does the attach: Lost, and Run says what went wrong.
func TestCommandEndsWhenItsInputEnds(t *testing.T) {
	c, id := commandRig(t, 80, 24)
	in, typing, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close() })
	var out, errb commandBuf
	x := Command(c, id, Options{})
	x.SetStdin(in)
	x.SetStdout(&out)
	x.SetStderr(&errb)
	x.SetWindow(func() (int, int) { return 80, 24 }, nil)
	done := commandRun(x)
	out.wait(t, "ready 80x24")
	typing.Close()
	err = commandEnd(t, done)
	if err == nil || x.Result().Why != Lost || !errors.Is(x.Result().Err, io.EOF) {
		t.Fatalf("Run with its input ended: %v, Result %+v; want an error and Lost", err, x.Result())
	}
	if !strings.HasPrefix(errb.String(), "rota: lost ") {
		t.Fatalf("stderr %q, want the lost line", errb.String())
	}
}

// A raw stream for stdin is refused before anything is attached: a reader
// Run started over it would take the next key after Run.
func TestCommandRefusesARawStream(t *testing.T) {
	var out, errb commandBuf
	x := Command(nil, "0123456789abcdef", Options{})
	x.SetStdin(strings.NewReader("typed"))
	x.SetStdout(&out)
	x.SetStderr(&errb)
	err := x.Run()
	if !errors.Is(err, commandRaw) || x.Result().Why != Refused {
		t.Fatalf("Run over a raw stream: %v, Result %+v; want the refusal", err, x.Result())
	}
	if out.String() != "" {
		t.Fatalf("a refused attach drew %q", out.String())
	}
	if want := "rota: " + commandRaw.Error() + "\r\n"; errb.String() != want {
		t.Fatalf("stderr %q, want %q", errb.String(), want)
	}
}

// commandFeed is a stdin that is already a pump, as an SSH channel's one
// reader gives it: never read, only received from.
type commandFeed struct{ ch chan []byte }

func (f commandFeed) Read([]byte) (int, error) { panic("a pump's stream is never read") }
func (f commandFeed) Input() <-chan []byte     { return f.ch }

// A pump for stdin is received from while Run runs, and never after: the
// next chunk sent is left for whoever receives next.
func TestCommandTakesAPump(t *testing.T) {
	c, id := commandRig(t, 80, 24)
	feed := commandFeed{ch: make(chan []byte)}
	var out, errb commandBuf
	x := Command(c, id, Options{})
	x.SetStdin(feed)
	x.SetStdout(&out)
	x.SetStderr(&errb)
	x.SetWindow(func() (int, int) { return 80, 24 }, nil)
	done := commandRun(x)
	out.wait(t, "ready 80x24")
	send := func(s string) {
		t.Helper()
		select {
		case feed.ch <- []byte(s):
		case <-time.After(5 * time.Second):
			t.Fatalf("nobody received %q", s)
		}
	}
	send("hi\r")
	out.wait(t, "got:hi")
	send("\x1cd")
	if err := commandEnd(t, done); err != nil {
		t.Fatalf("Run after Ctrl-\\ d: %v", err)
	}
	select {
	case feed.ch <- []byte("z"):
		t.Fatal("something received from the pump after Run returned")
	case <-time.After(300 * time.Millisecond):
	}
}
