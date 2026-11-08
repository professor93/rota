//go:build linux || darwin

package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// A daemon outlives a step of the clock. On Linux the start daemon.pid
// keeps is read back through a boot time every setting of the clock moves,
// so here it is put an hour back, as a step of an hour leaves it. The daemon
// still starts sessions, since a holder takes its link by who the kernel
// says is at the other end; and where the system keeps a start in ticks,
// serve stop still stops it, telling it by those.
func TestADaemonOutlivesAClockStep(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	path := holdwire.DaemonPIDFile(r.terminals)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d holdwire.Daemon
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	d.ProcStart = d.ProcStart.Add(-time.Hour)
	stepped, err := json.Marshal(&d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, stepped, 0o600); err != nil {
		t.Fatal(err)
	}
	// Put back before the rig reaps, so the daemon can be told and killed
	// should this fail; a daemon that stopped took its file with it.
	t.Cleanup(func() {
		if _, err := os.Stat(path); err == nil {
			_ = os.WriteFile(path, raw, 0o600)
		}
	})

	p := r.attach(r.newSession("after the step"))
	p.waitOut("ready 100x30")

	if d.ProcStartTicks == 0 {
		t.Log("this system keeps no start in ticks, and its start times do not move with the clock")
		return
	}
	if out, errb, code := r.rota("serve", "stop"); code != 0 || out != "rota daemon stopped; 1 session keeps running\n" {
		t.Fatalf("serve stop after the clock was stepped: exit %d %q %q", code, out, errb)
	}
}
