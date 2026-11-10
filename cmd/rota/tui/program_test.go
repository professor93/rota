package tui

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// programOut is an output the program writes from its own goroutines.
type programOut struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *programOut) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *programOut) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// programRun runs o in the background, over a pipe it returns the writing
// end of, and gives back a wait for its end.
func programRun(t *testing.T, o Options) (in *io.PipeWriter, wait func() (Result, error)) {
	t.Helper()
	r, w := io.Pipe()
	o.In = r
	type end struct {
		res Result
		err error
	}
	ended := make(chan end, 1)
	go func() {
		res, err := Run(context.Background(), o)
		ended <- end{res, err}
	}()
	t.Cleanup(func() { w.Close() })
	return w, func() (Result, error) {
		t.Helper()
		select {
		case e := <-ended:
			w.Close()
			return e.res, e.err
		case <-time.After(20 * time.Second):
			if runtime.GOOS == "windows" {
				t.Skip("Bubble Tea does not read keys from an io.Pipe on Windows; the model tests cover the shell")
			}
			t.Fatal("Run did not end")
			return Result{}, nil
		}
	}
}

// programType writes keys to the program as a terminal would.
func programType(w io.Writer, s string) {
	go w.Write([]byte(s))
}

func TestRunHeadless(t *testing.T) {
	out := &programOut{}
	in, wait := programRun(t, Options{
		Backend: &fakeBackend{},
		Out:     out,
		Env:     []string{"TERM=xterm-256color"},
		Remote:  true,
		Cols:    80,
		Rows:    24,
	})
	// Two presses: the chrome may ask before it quits.
	programType(in, "qq")
	res, err := wait()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res != (Result{}) {
		t.Errorf("Run gave %+v, want nothing pending", res)
	}
	if !strings.Contains(out.String(), "rota") {
		t.Errorf("nothing was drawn: %q", out.String())
	}
}

// Run is the one reader of Options.Resize: each size reaches the model, and
// an attach holding the terminal meanwhile hears of it (F22). Each tab is
// left, with ten seconds to close what it has open.
func TestRunHandsEveryResizeOnAndLeavesEachTab(t *testing.T) {
	shellRegistry(t)
	sizes := make(chan [2]int, 8)
	left := make(chan time.Duration, 1)
	envs := make(chan *env, 1)
	held, hold := make(chan struct{}, 1), make(chan struct{})
	register(10, func(e *env) tab {
		envs <- e
		return &shellTab{title: "Ten", sizes: sizes, left: left, held: held, hold: hold}
	})
	resize := make(chan [2]int)
	in, wait := programRun(t, Options{
		Backend: &fakeBackend{},
		Out:     &programOut{},
		Env:     []string{"TERM=xterm"},
		Remote:  true,
		Cols:    80,
		Rows:    24,
		Resize:  resize,
	})
	e := <-envs
	programSize(t, sizes, [2]int{80, 24})
	size, resized, release := e.window()
	if c, r := size(); c != 80 || r != 24 {
		t.Errorf("the attach's size is %d×%d before any resize", c, r)
	}
	resize <- [2]int{100, 30}
	select {
	case <-resized:
	case <-time.After(5 * time.Second):
		t.Fatal("the attach did not hear of the resize")
	}
	if c, r := size(); c != 100 || r != 30 {
		t.Errorf("the attach's size is %d×%d, want 100×30", c, r)
	}
	programSize(t, sizes, [2]int{100, 30})
	// While the program is held, as it is while an attach has the
	// terminal, the attach still hears of every resize.
	programType(in, "b")
	<-held
	resize <- [2]int{110, 35}
	select {
	case <-resized:
	case <-time.After(5 * time.Second):
		t.Fatal("while the program was held the attach did not hear of the resize")
	}
	if c, r := size(); c != 110 || r != 35 {
		t.Errorf("while held the attach's size is %d×%d, want 110×35", c, r)
	}
	close(hold)
	programSize(t, sizes, [2]int{110, 35})
	release()
	resize <- [2]int{90, 30}
	programSize(t, sizes, [2]int{90, 30})
	select {
	case <-resized:
		t.Error("a released attach still hears of resizes")
	default:
	}
	programType(in, "qq")
	if _, err := wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case d := <-left:
		if d <= 0 || d > 10*time.Second {
			t.Errorf("Leave had %v to close what is open, want up to 10 s", d)
		}
	default:
		t.Error("the tab was never left")
	}
}

func programSize(t *testing.T, sizes <-chan [2]int, want [2]int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-sizes:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("the model never saw %d×%d", want[0], want[1])
		}
	}
}

func TestRunWantsABackend(t *testing.T) {
	if _, err := Run(context.Background(), Options{In: strings.NewReader(""), Out: io.Discard}); err == nil {
		t.Error("Run without a Backend did not refuse")
	}
}
