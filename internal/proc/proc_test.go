//go:build linux || darwin

package proc

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The process at the other end of a unix socket is named by the kernel, not
// by anything it says: here both ends are this test.
func TestPeerPIDOfAUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "rota")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	dialed := make(chan error, 1)
	go func() {
		c, err := net.Dial("unix", path)
		if err == nil {
			defer c.Close()
			buf := make([]byte, 1)
			_, _ = c.Read(buf) // held open until the test closes its end
		}
		dialed <- err
	}()
	c, err := l.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := PeerPID(c)
	_ = c.Close()
	if derr := <-dialed; derr != nil {
		t.Fatal(derr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Fatalf("the peer is %d, want %d", pid, os.Getpid())
	}
}

// This process started a moment ago, the boot it runs in has an id, and a
// pid that names nothing has no start time.
func TestStartTimeAndBootIDOfThisProcess(t *testing.T) {
	at, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(at); d < -2*time.Second || d > time.Hour {
		t.Fatalf("this process started %v ago", d)
	}
	again, err := StartTime(os.Getpid())
	if err != nil || !again.Equal(at) {
		t.Fatalf("asked twice, %v then %v (%v)", at, again, err)
	}
	id, err := BootID()
	if err != nil || id == "" {
		t.Fatalf("boot id %q, %v", id, err)
	}
	if again, _ := BootID(); again != id {
		t.Fatalf("the boot id moved: %q then %q", id, again)
	}
	if _, err := StartTime(-1); err == nil {
		t.Fatal("pid -1 has a start time")
	}
	if _, err := StartTime(1 << 30); err == nil {
		t.Fatal("a pid nothing has has a start time")
	}
}
