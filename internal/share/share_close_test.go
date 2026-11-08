package share

import (
	"io"
	"net"
	"testing"
	"time"
)

// A Close does not wait for a Send stuck writing to a peer that stopped
// reading: it is what frees that Send.
func TestCloseDoesNotWaitForAStuckSend(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewConn(a, 0)
	sent := make(chan error, 1)
	go func() { sent <- c.Send(Ping, []byte("x")) }()
	// The peer takes one byte of the frame and no more. A pipe keeps
	// nothing, so the Send is then inside its write, stuck on the rest.
	if _, err := io.ReadFull(b, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.Close(); c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close waited for a write the peer was not reading")
	}
	select {
	case err := <-sent:
		if err == nil {
			t.Fatal("the stuck Send reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("the stuck Send was not freed")
	}
	if err := c.Send(Ping, nil); err != ErrClosed {
		t.Fatalf("a Send after Close answered %v", err)
	}
}
