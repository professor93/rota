//go:build linux || darwin

package attach

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/pty"
)

// localPair is a pseudo-terminal standing in for the person's own terminal,
// at cols by rows, so no test touches the terminal of whoever runs it. The
// test types on the master and reads what was drawn from it.
func localPair(t *testing.T, cols, rows uint16) (master, slave *os.File) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	if err := pty.Resize(master, cols, rows); err != nil {
		t.Fatal(err)
	}
	return master, slave
}

// localReadWithin reads once from r, failing the test if nothing comes
// within five seconds.
func localReadWithin(t *testing.T, what string, r interface{ Read([]byte) (int, error) }, n int) string {
	t.Helper()
	type got struct {
		s   string
		err error
	}
	ch := make(chan got, 1)
	go func() {
		buf := make([]byte, n)
		k, err := r.Read(buf)
		ch <- got{string(buf[:k]), err}
	}()
	select {
	case g := <-ch:
		if g.err != nil {
			t.Fatalf("reading %s: %v", what, g.err)
		}
		return g.s
	case <-time.After(5 * time.Second):
		t.Fatalf("reading %s: nothing within 5s", what)
	}
	return ""
}

func localMustTermios(t *testing.T, f *os.File) syscall.Termios {
	t.Helper()
	tio, err := localTermios(f)
	if err != nil {
		t.Fatal(err)
	}
	return tio
}

// TestLocalRestoresWhatItChanged (Review Focus 1): Local takes the terminal
// out of line editing, echo and signals, passes keys and output through as
// they are, and follows the window; restore gives back exactly the settings
// it found, drops whatever was typed but not read, and a second restore does
// nothing.
func TestLocalRestoresWhatItChanged(t *testing.T) {
	master, slave := localPair(t, 100, 30)
	before := localMustTermios(t, slave)
	if before.Lflag&syscall.ICANON == 0 || before.Lflag&syscall.ECHO == 0 {
		t.Fatalf("a fresh pseudo-terminal is not cooked: lflag %#x", before.Lflag)
	}

	term, restore, err := Local(slave, slave)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()

	raw := localMustTermios(t, slave)
	for _, f := range []struct {
		name string
		bit  uint64
	}{{"ICANON", uint64(syscall.ICANON)}, {"ECHO", uint64(syscall.ECHO)}, {"ISIG", uint64(syscall.ISIG)}, {"IEXTEN", uint64(syscall.IEXTEN)}} {
		if uint64(raw.Lflag)&f.bit != 0 {
			t.Errorf("raw mode left %s on", f.name)
		}
	}
	if raw.Iflag&syscall.ICRNL != 0 || raw.Iflag&syscall.IXON != 0 {
		t.Errorf("raw mode left ICRNL or IXON on: iflag %#x", raw.Iflag)
	}
	if raw.Oflag&syscall.OPOST != 0 {
		t.Errorf("raw mode left OPOST on: oflag %#x", raw.Oflag)
	}

	if c, r := term.Size(); c != 100 || r != 30 {
		t.Errorf("Size = %dx%d, want 100x30", c, r)
	}

	// A key reaches Read as typed, with no line to wait for, and Ctrl-\ is a
	// byte rather than a signal.
	if _, err := master.Write([]byte("a\x1c")); err != nil {
		t.Fatal(err)
	}
	if got := localReadWithin(t, "a key", term, 2); got != "a" && got != "a\x1c" {
		t.Errorf("Read = %q, want the keys as typed", got)
	}
	// Output is written as it is, with no newline turned into CR LF.
	if _, err := term.Write([]byte("out\n")); err != nil {
		t.Fatal(err)
	}
	if got := localReadWithin(t, "the output", master, 64); got != "out\n" {
		t.Errorf("the screen got %q, want %q", got, "out\n")
	}

	// The window is dragged: the kernel tells the terminal's foreground
	// process with SIGWINCH, which this test sends itself, since the pair is
	// not its controlling terminal.
	if err := pty.Resize(master, 40, 12); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case <-term.Resized():
	case <-time.After(5 * time.Second):
		t.Fatal("Resized did not fire after SIGWINCH")
	}
	if c, r := term.Size(); c != 40 || r != 12 {
		t.Errorf("Size after the resize = %dx%d, want 40x12", c, r)
	}

	// Something typed but not yet read when the attach ends, such as a key
	// release a kitty terminal reports, must not reach the shell after it.
	if _, err := master.Write([]byte("x!")); err != nil {
		t.Fatal(err)
	}
	// Reading the x proves the ! has arrived beside it, written together.
	for {
		got := localReadWithin(t, "the typed x", term, 1)
		if got == "x" {
			break
		}
		if got != "\x1c" {
			t.Fatalf("read %q before the x", got)
		}
	}
	restore()

	after := localMustTermios(t, slave)
	if after.Lflag != before.Lflag || after.Iflag != before.Iflag || after.Oflag != before.Oflag || after.Cflag != before.Cflag {
		t.Errorf("restore gave back lflag %#x iflag %#x oflag %#x cflag %#x, want %#x %#x %#x %#x",
			after.Lflag, after.Iflag, after.Oflag, after.Cflag, before.Lflag, before.Iflag, before.Oflag, before.Cflag)
	}
	if _, err := master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	if got := localReadWithin(t, "the line after restore", slave, 64); got != "y\n" {
		t.Errorf("the shell read %q after the attach, want %q: what was typed during it was kept", got, "y\n")
	}

	// A second restore does nothing: the settings someone made since stay.
	quiet := after
	quiet.Lflag &^= syscall.ECHO
	if err := localSetTermios(slave, quiet); err != nil {
		t.Fatal(err)
	}
	restore()
	if again := localMustTermios(t, slave); again.Lflag&syscall.ECHO != 0 {
		t.Error("a second restore set the terminal again")
	}

	// And the window is no longer followed.
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case <-term.Resized():
		t.Error("Resized fired after restore")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestLocalRefusesAPipe: standard input that is not a terminal is refused in
// words, and nothing is handed back to restore.
func TestLocalRefusesAPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	term, restore, err := Local(r, w)
	if err == nil || err.Error() != "attach: standard input is not a terminal" {
		t.Fatalf("Local on a pipe: err %v, want attach: standard input is not a terminal", err)
	}
	if term != nil || restore != nil {
		t.Error("Local on a pipe handed back a terminal or a restore")
	}
}
