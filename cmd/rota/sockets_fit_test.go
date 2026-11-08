//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/termenv"
)

// A store with room for ctl.sock and none for a held session's socket,
// thirteen bytes longer, would get a daemon that answers every session it
// starts with an internal error, the reason only in its log. So serve
// start, the daemon itself and install each refuse it, naming the session's
// socket and the fix, and nothing is started or installed.
func TestAStoreWithNoRoomForASessionsSocketIsRefused(t *testing.T) {
	r := newDaemonRig(t)
	// The session's socket is 101 bytes, ctl.sock 88.
	const homeLen = 101 - len("/terminals/0123456789abcdef/sock")
	base, err := os.MkdirTemp("", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if len(base)+2 > homeLen {
		t.Skipf("the temporary directory %s is too deep to make one", base)
	}
	home := filepath.Join(base, strings.Repeat("h", homeLen-len(base)-1))
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}}]}`)
	terminals := filepath.Join(home, "terminals")
	if holdwire.SockFits(holdwire.CtlSock(terminals)) != nil || holdwire.SockFits(holdwire.SockPath(holdwire.Dir(terminals, "0123456789abcdef"))) == nil {
		t.Fatalf("the store %s is not the one meant", home)
	}
	long := &daemonRig{t: t, home: home, terminals: terminals, sock: holdwire.CtlSock(terminals),
		env: termenv.Set(r.env, "ROTA_HOME", home)}
	t.Cleanup(long.reap)
	said := "a held session's socket, " + filepath.Join(terminals, "<id>", "sock") + ", is 101 bytes, longer than the 100 a unix socket can have; a shorter ROTA_HOME fixes it"

	if out, errb, code := long.rota("serve", "start"); code != 1 || out != "" || errb != "error: "+said+"\n" {
		t.Fatalf("serve start: exit %d %q %q, want %q", code, out, errb, said)
	}
	if out, errb, code := long.rota("serve", "start", "--foreground"); code != 1 || out != "" || errb != "" || !strings.Contains(long.log(), said) {
		t.Fatalf("serve start --foreground: exit %d %q %q\nlog:\n%s", code, out, errb, long.log())
	}
	if _, ok := holdwire.RunningDaemon(terminals); ok {
		t.Fatal("a daemon serves a store none of whose sessions can start")
	}

	unitHome(t, true)
	calls := recordUnits(t, nil)
	t.Setenv("ROTA_HOME", home)
	if out, errb, code := call(t, "serve", "install"); code != 1 || out != "" || errb != "error: "+said+"\n" || len(*calls) != 0 {
		t.Fatalf("serve install: exit %d %q %q, the supervisor asked %q", code, out, errb, *calls)
	}
}
