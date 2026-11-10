package sshd

import (
	"io"
	"testing"
	"time"
)

// One goroutine reads a channel's input, and a chunk it has read waits for
// whoever receives next: an attach on Input, or a TUI on Read. Nothing read
// is dropped when one receiver stops and another starts, and a chunk too
// big for a Read is given by the next.
func TestTheInputHasOneReader(t *testing.T) {
	pr, pw := io.Pipe()
	in := inputPump(pr)
	defer in.stop()
	send := func(s string) {
		t.Helper()
		done := make(chan struct{})
		go func() { defer close(done); _, _ = pw.Write([]byte(s)) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("writing %q: nobody read it", s)
		}
	}
	recv := func() string {
		t.Helper()
		select {
		case b, ok := <-in.Input():
			if !ok {
				t.Fatal("Input closed")
			}
			return string(b)
		case <-time.After(5 * time.Second):
			t.Fatal("nothing on Input")
		}
		return ""
	}
	// read is in.Read, given at most five seconds.
	read := func(p []byte) (int, error) {
		t.Helper()
		type got struct {
			n   int
			err error
		}
		c := make(chan got, 1)
		go func() {
			n, err := in.Read(p)
			c <- got{n, err}
		}()
		select {
		case g := <-c:
			return g.n, g.err
		case <-time.After(5 * time.Second):
			t.Fatal("Read gave nothing")
		}
		return 0, nil
	}

	send("ab")
	if got := recv(); got != "ab" {
		t.Fatalf("Input gave %q", got)
	}
	// Read while nobody receives: the pump holds it for the next receiver.
	send("cd")
	buf := make([]byte, 8)
	if n, err := read(buf); err != nil || string(buf[:n]) != "cd" {
		t.Fatalf("Read gave %q %v", buf[:n], err)
	}
	send("efg")
	small := make([]byte, 1)
	if n, err := read(small); n != 1 || err != nil || small[0] != 'e' {
		t.Fatalf("a small Read gave %q %v", small[:n], err)
	}
	if n, err := read(buf); err != nil || string(buf[:n]) != "fg" {
		t.Fatalf("the Read after a small one gave %q %v", buf[:n], err)
	}
	// An attach that gives up receiving takes nothing with it.
	quit, gone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(gone)
		select {
		case <-in.Input():
			t.Error("a receiver that gave up took a chunk")
		case <-quit:
		}
	}()
	close(quit)
	<-gone
	send("h")
	if got := recv(); got != "h" {
		t.Fatalf("Input gave %q", got)
	}
	// The input's end is the end of Input and of Read.
	pw.Close()
	select {
	case _, ok := <-in.Input():
		if ok {
			t.Fatal("Input gave a chunk after the end")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Input did not close at the end")
	}
	if n, err := read(buf); n != 0 || err != io.EOF {
		t.Fatalf("Read at the end: %d %v", n, err)
	}
}

// Once stopped, a pump drops what it holds and a Read waiting ends.
func TestAStoppedInputEnds(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	in := inputPump(pr)
	read := make(chan error, 1)
	go func() {
		_, err := in.Read(make([]byte, 4))
		read <- err
	}()
	in.stop()
	select {
	case err := <-read:
		if err != io.EOF {
			t.Fatalf("a Read waiting when the pump stopped: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a Read waiting when the pump stopped did not end")
	}
	in.stop()
}
