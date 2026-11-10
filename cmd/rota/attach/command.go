package attach

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/muesli/cancelreader"

	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/pty"
	"github.com/professor93/rota/internal/vtscan"
)

// Cmd is an attach as a command whose streams the caller sets and which it
// then runs to its end: the shape bubbletea's tea.Exec runs, so the TUI can
// hand its screen to a session and take it back after. Command makes one.
type Cmd struct {
	c  *client.Client
	id string
	o  Options

	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	size    func() (cols, rows int)
	resized <-chan struct{}

	result Result
}

// Command is an attach to the session id, the whole id, of the daemon c
// reaches, with o, to be run with Run once its streams are set.
func Command(c *client.Client, id string, o Options) *Cmd {
	return &Cmd{c: c, id: id, o: o}
}

// SetStdin sets what the person types. Nothing typed after Run returns may
// be taken by it, so r is one of three things, and Run refuses anything
// else:
//
//   - An *os.File that can be waited on: a terminal, a pipe or a socket.
//     Run reads it only while it runs, through a read that is cancelled
//     without taking a byte. (A regular file, which cannot be waited on, is
//     read on to its next chunk.)
//   - A pump: anything with a method Input() <-chan []byte. Run receives
//     from it only while it runs, and never reads r, nor closes the channel;
//     a chunk sent after Run returns goes to whoever receives next. This is
//     how a raw stream such as an SSH channel must be given: through the one
//     pump that reads it for the channel's whole life, never as the stream
//     itself.
//   - nil, for nothing typed.
//
// Any other reader, a raw stream above all, is refused: a reader Run
// started over it could not be stopped when Run returns, and would take and
// drop the next chunk typed after it.
func (x *Cmd) SetStdin(r io.Reader) { x.stdin = r }

// SetStdout sets the person's screen.
func (x *Cmd) SetStdout(w io.Writer) { x.stdout = w }

// SetStderr sets where the line the attach ends with is said.
func (x *Cmd) SetStderr(w io.Writer) { x.stderr = w }

// SetWindow says how big the person's screen is, in cells, and when that
// changes, for a screen that is not this process's own terminal: a TUI on
// an SSH channel. size is asked again after each receive from resized; a
// nil resized never fires.
func (x *Cmd) SetWindow(size func() (cols, rows int), resized <-chan struct{}) {
	x.size, x.resized = size, resized
}

// Run attaches until the person leaves, the session ends, or the attach is
// lost or refused, then says Result.Line on stderr. When stdin and stdout
// are this process's terminal and no window was set, it is that terminal
// raw, as Local makes it, and given back after; otherwise the three streams
// and the window are the terminal. Either way stdin is read only while the
// attach runs, as SetStdin says. It returns nil for a leave and for an end,
// and otherwise what went wrong: a stdin SetStdin does not take is an error
// before anything is attached.
func (x *Cmd) Run() error {
	out := x.stdout
	if out == nil {
		out = io.Discard
	}
	t, restore, local, err := x.terminal(out)
	if err != nil {
		x.result = Result{Why: Refused, Session: client.Session{ID: x.id}, Err: err}
	} else {
		defer restore()
		if local {
			defer commandRescue(t, out, restore)
		}
		x.result = Run(context.Background(), t, x.c, x.id, x.o)
		restore()
		// The attach left the cursor on the window's last row: the line
		// goes below it.
		_, _ = io.WriteString(out, "\r\n")
	}
	line := x.result.Line(x.o)
	if x.stderr != nil {
		// A terminal the attach wrapped is raw, as the person's own
		// terminal was made: a newline alone would not return the cursor.
		end := "\r\n"
		if local {
			end = "\n"
		}
		_, _ = io.WriteString(x.stderr, line+end)
	}
	switch x.result.Why {
	case Detached, Ended:
		return nil
	}
	if x.result.Err != nil {
		return x.result.Err
	}
	return errors.New(strings.TrimPrefix(line, "rota: "))
}

// commandRescue, deferred while this process's own terminal is attached,
// gives it back from a panic on Run's goroutine before the panic goes on:
// every mode the session set off, the pen and scroll region reset and the
// cursor below its last row, then the settings Local found. Without it the
// person would be left on the session's screen, its modes on.
func commandRescue(t Terminal, out io.Writer, restore func()) {
	p := recover()
	if p == nil {
		return
	}
	_, rows := t.Size()
	_, _ = out.Write(append(vtscan.ResetAll(), "\x1b[0m\x1b[r\x1b["+strconv.Itoa(max(rows, 1))+";1H\r\n"...))
	restore()
	panic(p)
}

// Result is how the attach ended, once Run has returned.
func (x *Cmd) Result() Result { return x.result }

// terminal is the Terminal Run attaches: this process's own terminal when
// stdin and stdout are one and no window was set, and otherwise the
// streams wrapped. local says which. restore puts it back.
func (x *Cmd) terminal(out io.Writer) (t Terminal, restore func(), local bool, err error) {
	if x.size == nil {
		in, inOK := x.stdin.(*os.File)
		o, outOK := out.(*os.File)
		if inOK && outOK && commandIsTerminal(in) && commandIsTerminal(o) {
			t, restore, err := Local(in, o)
			return t, restore, true, err
		}
	}
	w, err := commandWrap(x.stdin, out, x.size, x.resized)
	if err != nil {
		return nil, nil, false, err
	}
	return w, w.stop, false, nil
}

// commandIsTerminal says whether f is a terminal, as one that can say how
// big it is. Where rota has no pseudo-terminals, nothing is.
func commandIsTerminal(f *os.File) bool {
	_, _, err := pty.Size(f)
	return err == nil
}

// commandPump is a stdin that is already a pump: Run receives its chunks
// and never reads it.
type commandPump interface {
	Input() <-chan []byte
}

// commandRaw is a stdin that is neither a file nor a pump.
var commandRaw = errors.New("attach: stdin must be a file or a pump with Input() <-chan []byte; " +
	"over any other reader the attach would take and drop the next key typed after it")

// commandTerm is the streams Run was given, as a Terminal: Input pumped from
// stdin or stdin's own pump, the screen stdout, and the window SetWindow's,
// or 80x24.
type commandTerm struct {
	out     io.Writer
	size    func() (cols, rows int)
	resized <-chan struct{}

	from   <-chan []byte             // stdin's own pump, when it is one
	reader cancelreader.CancelReader // the file's, when it is one
	input  chan []byte
	pumped chan struct{} // closed once pump has stopped
	done   chan struct{}
	once   sync.Once
	inErr  atomic.Value // error: why the pump stopped, when it panicked

	mu         sync.Mutex
	cols, rows int // the last good size
}

// inputErr is why Input was closed, when it was not stdin's own end: the
// pump's panic. Nil otherwise.
func (w *commandTerm) inputErr() error {
	err, _ := w.inErr.Load().(error)
	return err
}

// commandWrap makes the Terminal of in, out and the window, and, for a
// file, starts reading it. A file whose read cannot be cancelled, a
// regular file on linux, is still stopped at the end, but a read it has
// under way then takes the next chunk and drops it. Any in that is not a
// file, a pump or nil is commandRaw.
func commandWrap(in io.Reader, out io.Writer, size func() (int, int), resized <-chan struct{}) (*commandTerm, error) {
	w := &commandTerm{out: out, size: size, resized: resized, input: make(chan []byte),
		pumped: make(chan struct{}), done: make(chan struct{}), cols: 80, rows: 24}
	switch in := in.(type) {
	case nil:
		// Nothing to read: Input never sends, and never ends.
		close(w.pumped)
	case commandPump:
		w.from = in.Input()
		close(w.pumped)
	case *os.File:
		r, err := commandNewReader(in)
		if err != nil {
			r = commandPlain{in}
		}
		w.reader = r
		go w.pump()
	default:
		return nil, commandRaw
	}
	return w, nil
}

// commandNewReader is cancelreader.NewReader, and a variable only so that a
// test can give the pump a read that panics.
var commandNewReader = cancelreader.NewReader

// pump reads stdin and hands each chunk to Input, until the read fails or
// stop cancels it. Input is closed as it stops, a panic in the read
// included, which inputErr then says: the attach ends Lost with it, rather
// than the process with the attach's terminal as the session left it.
func (w *commandTerm) pump() {
	defer close(w.pumped)
	defer close(w.input)
	defer func() {
		if p := recover(); p != nil {
			w.inErr.Store(fmt.Errorf("input: panic: %v", p))
		}
	}()
	buf := make([]byte, 4096)
	for {
		n, err := w.reader.Read(buf)
		if n > 0 {
			select {
			case w.input <- bytes.Clone(buf[:n]):
			case <-w.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// stop stops reading stdin, and waits for the reader when it could be
// woken; one that could not be, commandPlain's, ends at its next chunk.
// A pump stdin gave is left as it is. It is safe to call twice.
func (w *commandTerm) stop() {
	w.once.Do(func() {
		close(w.done)
		if w.reader == nil {
			return
		}
		if w.reader.Cancel() {
			<-w.pumped
		}
		_ = w.reader.Close()
	})
}

func (w *commandTerm) Input() <-chan []byte {
	if w.from != nil {
		return w.from
	}
	return w.input
}

func (w *commandTerm) Write(p []byte) (int, error) { return w.out.Write(p) }
func (w *commandTerm) Resized() <-chan struct{}    { return w.resized }

// Size asks the window, and keeps the last answer for when it says 0: 80x24
// until it has given a size once, or always when no window was set.
func (w *commandTerm) Size() (cols, rows int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size != nil {
		if c, r := w.size(); c > 0 && r > 0 {
			w.cols, w.rows = c, r
		}
	}
	return w.cols, w.rows
}

// commandPlain is a reader as a CancelReader that cannot cancel a read under
// way: Cancel says so, and the pump stops at its next chunk.
type commandPlain struct{ io.Reader }

func (commandPlain) Cancel() bool { return false }
func (commandPlain) Close() error { return nil }
