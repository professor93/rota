//go:build linux || darwin

package hold

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/share"
	rota "github.com/professor93/rota/lib"
)

// A link is the daemon's when the kernel names the peer and daemon.pid
// names the same pid, while the daemon's lock is held: nothing is asked of
// the start the record keeps. On Linux that start is read back through a
// boot time every setting of the clock moves, so asking would cost a
// daemon every link — every new session and every relink — once the clock
// had been stepped. Here the record's start is an hour from the truth, by
// time and by ticks, and the link is taken all the same.
func TestTheDaemonLinksWhateverItsRecordedStart(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	path := holdwire.DaemonPIDFile(r.terminals)
	d, ok := holdwire.RunningDaemon(r.terminals)
	if !ok || d.PID != os.Getpid() {
		t.Fatalf("this test is not the daemon: %+v", d)
	}
	d.ProcStart = d.ProcStart.Add(-time.Hour)
	if d.ProcStartTicks != 0 {
		d.ProcStartTicks -= 3600 * 100
	}
	raw, err := rota.EncodeIndent(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if d, _ := holdwire.RunningDaemon(r.terminals); d.PID != 0 {
		t.Fatalf("the record still names a process that started then: %+v", d)
	}
	p := r.link(t)
	p.send(holdwire.Ping, nil)
	p.until(holdwire.Pong)
}

// A process that is not the daemon is refused before anything it sends is
// read: here it sends nothing at all, and is refused at once rather than
// after helloWithin.
func TestALinkIsRefusedBeforeItsHello(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	conn, err := net.Dial("unix", holdwire.SockPath(holdwire.Dir(r.terminals, testID)))
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{t: t, c: share.NewConn(conn, 0)}
	t.Cleanup(func() { p.c.Close() })
	began := time.Now()
	p.c.Deadline(began.Add(helloWithin / 2))
	kind, b, err := p.c.Read()
	if err != nil || kind != holdwire.Refused {
		t.Fatalf("a silent peer that is not the daemon got %d %q (%v) after %s", kind, b, err, time.Since(began))
	}
	var m holdwire.RefusedMsg
	if err := share.Unmarshal(b, &m); err != nil || m.Message != refusedNotDaemon {
		t.Fatalf("refused with %q (%v)", m.Message, err)
	}
}
