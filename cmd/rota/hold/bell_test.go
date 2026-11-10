//go:build linux || darwin

package hold

import (
	"strings"
	"testing"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/share"
)

// A notification a CLI asks for, by OSC 9 or OSC 777, is a bell to the
// daemon: it is counted, and its words, which may be the chat's own, never
// cross the link.
func TestANotificationCountsAsABellAndSaysNothing(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	p := r.link(t)
	p.since(0)
	out := "\a" + "\x1b]9;done\x1b\\" + "\x1b]777;notify;T;B\a"
	r.c.print(t, out)
	r.grounded(t, int64(len(out)))
	// The State the session ends on is sent before its Exit, whatever the
	// quarter second between States says.
	r.c.exit(0)
	var last holdwire.StateMsg
	for {
		kind, b := p.next()
		if kind == holdwire.Exit {
			break
		}
		if kind != holdwire.State {
			continue
		}
		if strings.Contains(string(b), "done") || strings.Contains(string(b), "T: B") {
			t.Errorf("a State says what the notification said: %s", b)
		}
		if err := share.Unmarshal(b, &last); err != nil {
			t.Fatal(err)
		}
	}
	if last.Bells != 3 {
		t.Fatalf("the last State has Bells == %d, want 3", last.Bells)
	}
}
