//go:build linux || darwin

package client

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// listening is a socket at ctl.sock that answers nothing and counts every
// byte anybody sends it.
type listening struct {
	sock   string
	mark   string        // what received sends to know it is last in line
	marked chan struct{} // closed once the mark has been read
	mu     sync.Mutex
	sent   int
	wg     sync.WaitGroup
}

func listen(t *testing.T, sock string) *listening {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l := &listening{sock: sock, mark: fmt.Sprintf("mark-%d-%d", os.Getpid(), time.Now().UnixNano()), marked: make(chan struct{})}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.wg.Add(1)
			go func() {
				defer l.wg.Done()
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				got, _ := io.ReadAll(c)
				if string(got) == l.mark {
					close(l.marked)
					return
				}
				l.mu.Lock()
				l.sent += len(got)
				l.mu.Unlock()
			}()
		}
	}()
	return l
}

// received is every byte sent so far, once each connection has ended. A
// listener accepts its connections in the order they were made, so received
// makes one of its own, sending the mark, and waits for it to be read: by
// then every connection made before it has been accepted and counted in wg,
// and wg is waited on only after that.
func (l *listening) received(t *testing.T) int {
	t.Helper()
	c, err := net.Dial("unix", l.sock)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(c, l.mark)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.marked:
	case <-time.After(10 * time.Second):
		t.Fatal("the listener never read the mark")
	}
	l.wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sent
}

// A request carries what only this user's daemon may have, so the client
// asks the kernel who listens at ctl.sock before it sends a thing. Here this
// test's own socket is made another user's, as one planted in a store
// somebody else can write to would be: Dial refuses it at once rather than
// waiting for it to answer, and a client already made refuses it at its next
// request. Neither sends a byte.
func TestTheClientSendsNothingToAnotherUsersSocket(t *testing.T) {
	dir := terminals(t)
	release, err := holdwire.ClaimDaemon(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	sock := holdwire.CtlSock(dir)
	l := listen(t, sock)
	c, err := Dial(dir)
	if err != nil {
		t.Fatalf("this user's own socket: %v", err)
	}

	was := owner
	t.Cleanup(func() { owner = was })
	owner = func() int { return os.Geteuid() + 1 }
	want := fmt.Sprintf("%s is listened on by user %d, not by this one (%d), so nothing was sent to it", sock, os.Geteuid(), os.Geteuid()+1)
	began := time.Now()
	if _, err := Dial(dir); err == nil || err.Error() != want || time.Since(began) > time.Second {
		t.Fatalf("Dial to another user's socket: %v after %s, want %q at once", err, time.Since(began), want)
	}
	if err := c.Health(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a request to another user's socket: %v, want %q", err, want)
	}
	if n := l.received(t); n != 0 {
		t.Fatalf("another user's socket was sent %d bytes", n)
	}
}
