//go:build linux || darwin

package hold

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/share"
)

// A holder says in its State whether it keeps a screen, so that the daemon
// can repaint its viewers when a retired one comes back.

func TestTheStateSaysWhetherTheScreenIsKept(t *testing.T) {
	renewing(t, time.Hour)
	r := newRig(t, 1024, newFakeChild())
	claim(t, r.terminals)
	p := r.dial(t)
	if kind, b := p.next(); kind != holdwire.Welcome {
		t.Fatalf("the holder answered %d %q, not a Welcome", kind, b)
	}
	var first holdwire.StateMsg
	if kind, b := p.next(); kind != holdwire.State || share.Unmarshal(b, &first) != nil || !first.Screen {
		t.Fatalf("the link's first State is %d %q, not one keeping a screen", kind, b)
	}
	p.since(0)

	// A flood the screen cannot keep up with overtakes it.
	r.s.em.lock.lock()
	var flood strings.Builder
	for i := 0; flood.Len() < 4096; i++ {
		fmt.Fprintf(&flood, "line %06d\r\n", i)
	}
	r.c.print(t, flood.String())
	r.grounded(t, int64(flood.Len()))
	r.s.em.lock.unlock()
	p.stateSays(false, time.Second)

	// Once the CLI is quiet the screen is kept again.
	r.s.mu.Lock()
	r.s.renewAfter = 50 * time.Millisecond
	r.s.mu.Unlock()
	nudge(r.s.emWake)
	p.stateSays(true, 5*time.Second)

	// So it is for a size past the budget, and for a fit back within it.
	p.json(holdwire.Resize, holdwire.SizeMsg{Cols: 500, Rows: 401})
	p.stateSays(false, time.Second)
	// Nothing can be kept before the CLI prints after the fit, so the
	// Resized is read past no State that matters.
	p.fit(80, 24)
	r.c.print(t, "back\r\n")
	p.stateSays(true, 5*time.Second)
}

// A screen whose emulator panicked is not kept, nor is one for a session
// that starts past the budget.
func TestTheStateSaysAFailedOrTooLargeScreenIsNotKept(t *testing.T) {
	orig := vtWrite
	t.Cleanup(func() { vtWrite = orig })
	vtWrite = func(em *vt.Emulator, p []byte) (int, error) {
		if bytes.Contains(p, []byte("boom")) {
			panic("x/vt fell over")
		}
		return orig(em, p)
	}
	keeps := func(s *Session) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stateLocked().Screen
	}
	s := newSession(Config{ID: testID}, newFakeChild()) // no emulator running
	s.renewAfter = time.Hour
	defer func() { s.screenNow().Close() }()
	if !keeps(s) {
		t.Fatal("a new session's State says no screen is kept")
	}
	s.took([]byte("boom\r\n"))
	if step(s); !retired(s) || keeps(s) {
		t.Fatalf("after a panic: retired %v, and the State says kept %v", retired(s), keeps(s))
	}

	big := newSession(Config{ID: testID, Cols: 500, Rows: 401}, newFakeChild())
	defer func() { big.screenNow().Close() }()
	if keeps(big) {
		t.Fatal("a session started past the budget says its screen is kept")
	}
}

// stateSays reads the link until a State says whether a screen is kept as
// want does, and fails the test when none has within d.
func (p *peer) stateSays(want bool, d time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(d)
	for {
		p.c.Deadline(deadline)
		kind, b, err := p.c.Read()
		if err != nil {
			p.t.Fatalf("no State said screen %t within %v: %v", want, d, err)
		}
		var m holdwire.StateMsg
		if kind == holdwire.State && share.Unmarshal(b, &m) == nil && m.Screen == want {
			return
		}
	}
}
