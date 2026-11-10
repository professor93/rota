package sshd

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"
)

// inputRig is a pump over a pipe the test types into, with each wait
// bounded, so a pump that loses a chunk fails a test rather than hanging it.
type inputRig struct {
	t  *testing.T
	in *input
	pw *io.PipeWriter
}

func inputStart(t *testing.T) *inputRig {
	pr, pw := io.Pipe()
	in := inputPump(pr)
	t.Cleanup(func() {
		in.stop()
		pw.Close()
	})
	return &inputRig{t: t, in: in, pw: pw}
}

// send types s, and waits until the pump has read it.
func (r *inputRig) send(s string) {
	r.t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.pw.Write([]byte(s)) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		r.t.Fatalf("writing %q: nobody read it", s)
	}
}

// recv is the next chunk on Input, as it was given.
func (r *inputRig) recv() []byte {
	r.t.Helper()
	select {
	case b, ok := <-r.in.Input():
		if !ok {
			r.t.Fatal("Input closed")
		}
		return b
	case <-time.After(5 * time.Second):
		r.t.Fatal("nothing on Input")
	}
	return nil
}

// read is one Read into a buffer of size bytes.
func (r *inputRig) read(size int) (string, error) {
	r.t.Helper()
	type got struct {
		s   string
		err error
	}
	c := make(chan got, 1)
	go func() {
		p := make([]byte, size)
		n, err := r.in.Read(p)
		c <- got{string(p[:n]), err}
	}()
	select {
	case g := <-c:
		return g.s, g.err
	case <-time.After(5 * time.Second):
		r.t.Fatal("Read gave nothing")
	}
	return "", nil
}

// One goroutine reads a channel's input, and a chunk it has read waits for
// whoever receives next: an attach on Input, or a TUI on Read. Nothing read
// is dropped when one receiver stops and another starts, and what one Read
// cannot fit stays at the front for the next receiver, of either kind.
func TestTheInputHasOneReader(t *testing.T) {
	r := inputStart(t)
	r.send("ab")
	if got := r.recv(); string(got) != "ab" {
		t.Fatalf("Input gave %q", got)
	}
	// Read while nobody receives: the pump holds it for the next receiver.
	r.send("cd")
	if got, err := r.read(8); err != nil || got != "cd" {
		t.Fatalf("Read gave %q %v", got, err)
	}
	r.send("efg")
	if got, err := r.read(1); err != nil || got != "e" {
		t.Fatalf("a small Read gave %q %v", got, err)
	}
	if got, err := r.read(8); err != nil || got != "fg" {
		t.Fatalf("the Read after a small one gave %q %v", got, err)
	}
	// What a Read left is the front for an attach as well.
	r.send("hij")
	if got, err := r.read(1); err != nil || got != "h" {
		t.Fatalf("a small Read gave %q %v", got, err)
	}
	if got := r.recv(); string(got) != "ij" {
		t.Fatalf("Input after a small Read gave %q, want ij", got)
	}
	// A chunk given on Input is the receiver's: what is typed next does not
	// change it.
	r.send("kl")
	first := r.recv()
	r.send("mn")
	if second := r.recv(); string(first) != "kl" || string(second) != "mn" {
		t.Fatalf("Input gave %q then %q, want kl then mn", first, second)
	}
	// An attach that gives up receiving takes nothing with it.
	quit, gone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(gone)
		select {
		case <-r.in.Input():
			t.Error("a receiver that gave up took a chunk")
		case <-quit:
		}
	}()
	close(quit)
	<-gone
	r.send("o")
	if got := r.recv(); string(got) != "o" {
		t.Fatalf("Input gave %q", got)
	}
	// The input's end is the end of Input and of Read.
	r.pw.Close()
	select {
	case _, ok := <-r.in.Input():
		if ok {
			t.Fatal("Input gave a chunk after the end")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Input did not close at the end")
	}
	if got, err := r.read(8); got != "" || err != io.EOF {
		t.Fatalf("Read at the end: %q %v", got, err)
	}
}

// Hold ends a waiting Read with io.EOF and takes nothing: what is typed next
// is the attach's, on Input. Reads stay refused until Resume, and then go on
// with what comes after.
func TestAHoldTakesNothing(t *testing.T) {
	r := inputStart(t)
	waiting := make(chan error, 1)
	go func() {
		n, err := r.in.Read(make([]byte, 8))
		if n != 0 {
			err = fmt.Errorf("took %d bytes", n)
		}
		waiting <- err
	}()
	// Hold once the Read is under way.
	for deadline := time.Now().Add(5 * time.Second); ; {
		r.in.mu.Lock()
		reading := r.in.reading
		r.in.mu.Unlock()
		if reading == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the Read never started")
		}
		time.Sleep(time.Millisecond)
	}
	held := make(chan struct{})
	go func() { defer close(held); r.in.Hold() }()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("Hold never returned")
	}
	select {
	case err := <-waiting:
		if err != io.EOF {
			t.Fatalf("the Read waiting at the hold: %v", err)
		}
	default:
		t.Fatal("Hold returned with a Read still waiting")
	}
	if got, err := r.read(8); got != "" || err != io.EOF {
		t.Fatalf("a Read while held: %q %v", got, err)
	}
	r.send("x")
	if got, err := r.read(8); got != "" || err != io.EOF {
		t.Fatalf("a Read while held, with x typed: %q %v", got, err)
	}
	if got := r.recv(); string(got) != "x" {
		t.Fatalf("the attach got %q, want x", got)
	}
	r.in.Hold() // a second does nothing
	r.in.Resume()
	r.send("y")
	if got, err := r.read(8); err != nil || got != "y" {
		t.Fatalf("a Read after Resume: %q %v", got, err)
	}
}

// A Read that saw no hold, and asks the pump only as the hold comes, is
// given nothing: the pump looks at the hold again, and the chunk it holds
// is the attach's. Which of the two ways the Read then ends is chance, so
// it is tried forty times.
func TestAReadAskingAsTheHoldComesTakesNothing(t *testing.T) {
	old := inputAsking
	t.Cleanup(func() { inputAsking = old })
	for i := range 40 {
		r := inputStart(t)
		r.send("x") // the pump holds x for whoever asks
		asking, goOn := make(chan struct{}), make(chan struct{})
		inputAsking = func() {
			close(asking)
			<-goOn
		}
		got := make(chan string, 1)
		go func() {
			p := make([]byte, 4)
			n, _ := r.in.Read(p)
			got <- string(p[:n])
		}()
		select {
		case <-asking:
		case <-time.After(5 * time.Second):
			t.Fatal("the Read never came to ask")
		}
		inputAsking = func() {}
		held := make(chan struct{})
		go func() { defer close(held); r.in.Hold() }()
		for deadline := time.Now().Add(5 * time.Second); !r.in.isHeld(); {
			if time.Now().After(deadline) {
				t.Fatal("Hold never held")
			}
			time.Sleep(100 * time.Microsecond)
		}
		close(goOn)
		select {
		case <-held:
		case <-time.After(5 * time.Second):
			t.Fatal("Hold never returned")
		}
		if s := <-got; s != "" {
			t.Fatalf("try %d: a Read asking as the hold came took %q", i, s)
		}
		if b := r.recv(); string(b) != "x" {
			t.Fatalf("try %d: the attach got %q, want x", i, b)
		}
	}
}

// The terminal goes from a reader (the TUI) to an attach and back while the
// person types without pause, two hundred times: nothing typed is lost,
// repeated or reordered, wherever the hold falls.
func TestAHandoverLosesNothing(t *testing.T) {
	for run := range 200 {
		var typed bytes.Buffer
		for i := range 40 {
			fmt.Fprintf(&typed, "%04d,", i)
		}
		want := typed.Bytes()
		pr, pw := io.Pipe()
		in := inputPump(pr)
		go func() {
			rnd := rand.New(rand.NewPCG(uint64(run), 7))
			for rest := want; len(rest) > 0; {
				k := min(1+rnd.IntN(7), len(rest))
				if _, err := pw.Write(rest[:k]); err != nil {
					return
				}
				rest = rest[k:]
			}
			pw.Close()
		}()

		// reader reads as a TUI does, five bytes at a time, until a Read
		// ends with EOF: the hold, or the end of the input.
		var got bytes.Buffer
		var far atomic.Int64
		reader := func() <-chan []byte {
			out := make(chan []byte, 1)
			go func() {
				var b bytes.Buffer
				p := make([]byte, 5)
				for {
					n, err := in.Read(p)
					b.Write(p[:n])
					far.Add(int64(n))
					if err != nil {
						out <- b.Bytes()
						return
					}
				}
			}()
			return out
		}
		wait := func(what string, c <-chan []byte) []byte {
			select {
			case b := <-c:
				return b
			case <-time.After(5 * time.Second):
				t.Fatalf("run %d: %s never ended (so far %q)", run, what, got.String())
			}
			return nil
		}

		// The TUI, until it has a third of it and somewhere after.
		tui := reader()
		for deadline := time.Now().Add(5 * time.Second); far.Load() < int64(len(want)/3); {
			if time.Now().After(deadline) {
				t.Fatalf("run %d: the TUI read %d bytes", run, far.Load())
			}
			time.Sleep(50 * time.Microsecond)
		}
		in.Hold()
		got.Write(wait("the TUI's reading", tui))

		// The attach, on Input, until two thirds.
		for got.Len() < 2*len(want)/3 {
			select {
			case b, ok := <-in.Input():
				if !ok {
					t.Fatalf("run %d: Input closed at %q", run, got.String())
				}
				got.Write(b)
			case <-time.After(5 * time.Second):
				t.Fatalf("run %d: the attach got nothing more after %q", run, got.String())
			}
		}

		// The TUI again, to the end.
		in.Resume()
		got.Write(wait("the TUI's reading again", reader()))
		in.stop()
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("run %d: received\n%s\nwant\n%s", run, got.Bytes(), want)
		}
	}
}

// Once stopped, a pump holds nothing for anybody, and a Read waiting ends.
func TestAStoppedInputEnds(t *testing.T) {
	r := inputStart(t)
	read := make(chan error, 1)
	go func() {
		_, err := r.in.Read(make([]byte, 4))
		read <- err
	}()
	r.in.stop()
	select {
	case err := <-read:
		if err != io.EOF {
			t.Fatalf("a Read waiting when the pump stopped: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a Read waiting when the pump stopped did not end")
	}
	r.in.stop()
}
