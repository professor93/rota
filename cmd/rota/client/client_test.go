//go:build linux || darwin

package client

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// terminals is a terminals directory short enough for its socket.
func terminals(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := holdwire.SockFits(holdwire.CtlSock(dir)); err != nil {
		t.Skipf("no control socket fits here: %v", err)
	}
	return dir
}

// A held lock is a daemon, whatever its socket does (Review Focus 2): with
// the claim held and a dead socket file at ctl.sock, Dial waits for it to
// answer and then says the daemon is not answering — never that there is
// none. With the claim let go of, the same file is no daemon, at once.
func TestDialNeverTrustsAFailedConnect(t *testing.T) {
	dir := terminals(t)
	release, err := holdwire.ClaimDaemon(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: holdwire.CtlSock(dir), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()

	began := time.Now()
	_, err = Dial(dir)
	took := time.Since(began)
	want := fmt.Sprintf("the rota daemon (pid %d) is not answering on %s", os.Getpid(), holdwire.CtlSock(dir))
	if err == nil || errors.Is(err, ErrNoDaemon) || err.Error() != want {
		t.Fatalf("Dial to a held lock and a dead socket: %v, want %q", err, want)
	}
	if took < 5*time.Second-100*time.Millisecond {
		t.Fatalf("Dial gave up after %s rather than trying for 5s", took)
	}

	release()
	began = time.Now()
	if _, err := Dial(dir); !errors.Is(err, ErrNoDaemon) || time.Since(began) > time.Second {
		t.Fatalf("Dial with no claim: %v after %s, want ErrNoDaemon at once", err, time.Since(began))
	}
}

// daemon stands in for one: it holds the claim and answers on ctl.sock with
// the description documents a daemon writes, one session running and one
// stopped, and refuses to stop the stopped one the way a daemon does.
func daemon(t *testing.T) string {
	t.Helper()
	dir := terminals(t)
	release, err := holdwire.ClaimDaemon(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	ln, err := net.Listen("unix", holdwire.CtlSock(dir))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("GET /v1/terminals", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"terminals":[
			{"id":"00000000000000a1","held":true,"name":"api work","label":"api work","kind":"account",
			 "cwd":"/src/api","started":"2026-11-07T10:00:00Z","ended":false,"state":"live","status":"quiet",
			 "attachable":true,"reachable":true,"pid":4242,"title":"","conversation":"conv-1","resumable":false,
			 "account":{"id":2,"label":"work@x","provider":"claude"},"cols":120,"rows":40,"offset":10,"viewers":[]},
			{"id":"00000000000000b2","held":true,"name":"old","label":"","kind":"shell","cwd":"/tmp",
			 "started":"2026-11-06T09:00:00Z","ended":true,"ended_at":"2026-11-06T11:30:00Z","exit":3,
			 "state":"ended","status":"stopped","attachable":false,"reachable":false,"resumable":false,
			 "conversation":"","holder":null,"viewers":[],"recording":false}]}`)
	})
	mux.HandleFunc("DELETE /v1/terminals/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "00000000000000b2" {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"terminal 00000000000000b2 has ended"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return dir
}

// A session is read from the documents the daemon writes: ended a bool with
// ended_at beside it, the account an object (P7).
func TestSessionsAreReadFromTheDaemonsDocuments(t *testing.T) {
	c, err := Dial(daemon(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Health(); err != nil {
		t.Fatal(err)
	}
	all, err := c.List()
	if err != nil || len(all) != 2 {
		t.Fatalf("List: %v %+v", err, all)
	}
	live, old := all[0], all[1]
	started := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	if live.ID != "00000000000000a1" || live.Name != "api work" || live.Label != "api work" || live.Kind != "account" ||
		live.State != "live" || live.Status != "quiet" || live.Cwd != "/src/api" || live.Conversation != "conv-1" ||
		live.Account != 2 || live.AccountLabel != "work@x" || live.Provider != "claude" || live.PID != 4242 ||
		!live.Attachable || live.Resumable || !live.Started.Equal(started) || live.Ended != nil || live.Exit != nil {
		t.Fatalf("the running one: %+v", live)
	}
	ended := time.Date(2026, 11, 6, 11, 30, 0, 0, time.UTC)
	if old.ID != "00000000000000b2" || old.Kind != "shell" || old.State != "ended" || old.Status != "stopped" ||
		old.Account != 0 || old.Provider != "" || old.Attachable || old.Ended == nil || !old.Ended.Equal(ended) ||
		old.Exit == nil || *old.Exit != 3 {
		t.Fatalf("the stopped one: %+v", old)
	}

	if err := c.Stop("00000000000000a1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.Stop("00000000000000b2"); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("a refused Stop says why: %v", err)
	}
}
