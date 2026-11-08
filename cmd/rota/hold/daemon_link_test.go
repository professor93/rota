//go:build linux || darwin

package hold

import (
	"os"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
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
