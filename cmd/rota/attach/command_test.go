//go:build linux || darwin

package attach

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if strings.Contains(w.String(), want) {
			return
		}
	}
	t.Fatalf("%q never came; the screen has %q", want, w.String())
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
