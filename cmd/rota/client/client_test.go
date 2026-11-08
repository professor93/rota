//go:build linux || darwin

package client

import (
	"encoding/json"
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

// DialNow is Dial without the wait: a held lock whose socket does not
// accept is the daemon not answering at once, a free one is no daemon, and
// one that answers is dialled.
func TestDialNowDoesNotWait(t *testing.T) {
	dir := terminals(t)
	release, err := holdwire.ClaimDaemon(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	_, err = DialNow(dir)
	want := fmt.Sprintf("the rota daemon (pid %d) is not answering on %s", os.Getpid(), holdwire.CtlSock(dir))
	if err == nil || err.Error() != want || time.Since(began) > time.Second {
		t.Fatalf("DialNow to a silent daemon: %v after %s, want %q at once", err, time.Since(began), want)
	}
	// A daemon.pid that names no process this rota can check: the lock
	// says a daemon runs, and nobody is named rather than pid 0.
	if err := os.WriteFile(holdwire.DaemonPIDFile(dir), []byte(`{"pid":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = DialNow(dir); err == nil || err.Error() != "the rota daemon is not answering on "+holdwire.CtlSock(dir) {
		t.Fatalf("DialNow to a daemon whose pid is unknown: %v", err)
	}
	release()
	if _, err := DialNow(dir); !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("DialNow with no claim: %v", err)
	}
	c, err := DialNow(daemon(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Health(); err != nil {
		t.Fatal(err)
	}
}

// daemon stands in for one: it holds the claim and answers on ctl.sock with
// the description documents a daemon writes, one session running and one
// stopped, and refuses to stop the stopped one the way a daemon does.
func daemon(t *testing.T) string {
	t.Helper()
	dir := terminals(t)
	daemonAt(t, dir, twoSessions, nil)
	return dir
}

// twoSessions is a listing as a daemon writes it: one session running and
// one stopped.
const twoSessions = `{"terminals":[
	{"id":"00000000000000a1","held":true,"name":"api work","label":"api work","kind":"account",
	 "cwd":"/src/api","started":"2026-11-07T10:00:00Z","ended":false,"state":"live","status":"quiet",
	 "attachable":true,"reachable":true,"pid":4242,"title":"","conversation":"conv-1","resumable":false,
	 "account":{"id":2,"label":"work@x","provider":"claude"},"cols":120,"rows":40,"offset":10,"viewers":[]},
	{"id":"00000000000000b2","held":true,"name":"old","label":"","kind":"shell","cwd":"/tmp",
	 "started":"2026-11-06T09:00:00Z","ended":true,"ended_at":"2026-11-06T11:30:00Z","exit":3,
	 "state":"ended","status":"stopped","attachable":false,"reachable":false,"resumable":false,
	 "conversation":"","holder":null,"viewers":[],"recording":false}]}`

// daemonAt is daemon on the terminals directory dir, listing listing, with
// whatever routes more adds.
func daemonAt(t *testing.T, dir, listing string, more func(*http.ServeMux)) {
	t.Helper()
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
		fmt.Fprint(w, listing)
	})
	mux.HandleFunc("DELETE /v1/terminals/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "00000000000000b2" {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"terminal 00000000000000b2 has ended"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	})
	if more != nil {
		more(mux)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
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

// A session is found by its whole id, by a prefix of four characters or
// more that begins one id alone, or by the label or name of a live session.
// Two of either is an error that names them both; a stopped session's name
// is nobody's, and three characters are not a prefix.
func TestResolveByIDPrefixAndLabel(t *testing.T) {
	dir := terminals(t)
	live := func(id, name, label string) string {
		return `{"id":"` + id + `","held":true,"name":"` + name + `","label":"` + label + `","kind":"account",
			"started":"2026-11-07T10:00:00Z","ended":false,"state":"live","status":"running",
			"account":{"id":1,"label":"a@x","provider":"claude"}}`
	}
	daemonAt(t, dir, `{"terminals":[`+strings.Join([]string{
		live("a1b2c3d4e5f60718", "refactor", "refactor"),
		live("a1b2ffffffffffff", "twin", "twin"),
		live("c3d4000000000001", "twin", ""),
		`{"id":"d4e5000000000002","held":true,"name":"old","label":"old","kind":"shell",
		  "started":"2026-11-06T09:00:00Z","ended":true,"ended_at":"2026-11-06T11:30:00Z","exit":0,
		  "state":"ended","status":"stopped"}`,
	}, ",")+`]}`, nil)
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"a1b2c3d4e5f60718": "a1b2c3d4e5f60718", // the whole id
		"d4e5000000000002": "d4e5000000000002", // a stopped one, by its whole id
		"c3d4":             "c3d4000000000001", // a prefix of four
		"a1b2c3":           "a1b2c3d4e5f60718", // a longer one
		"refactor":         "a1b2c3d4e5f60718", // a label
	} {
		s, err := c.Resolve(in)
		if err != nil || s.ID != want {
			t.Errorf("Resolve(%q) = %q, %v; want %s", in, s.ID, err, want)
		}
	}
	for in, ids := range map[string][]string{
		"a1b2": {"a1b2c3d4e5f60718", "a1b2ffffffffffff"},
		"twin": {"a1b2ffffffffffff", "c3d4000000000001"},
	} {
		_, err := c.Resolve(in)
		if err == nil || !strings.Contains(err.Error(), ids[0]) || !strings.Contains(err.Error(), ids[1]) {
			t.Errorf("Resolve(%q), which matches two, says %v; it names both ids", in, err)
		}
	}
	for _, in := range []string{"old", "c3d", "nobody", ""} {
		if s, err := c.Resolve(in); err == nil || !errors.Is(err, ErrNoSession) {
			t.Errorf("Resolve(%q) found %s, %v; it matches no live session", in, s.ID, err)
		}
	}
	if _, err := c.Resolve("twin"); errors.Is(err, ErrNoSession) {
		t.Errorf("two sessions called twin are not none: %v", err)
	}
}

// Ensure starts a daemon only where the lock is free (Review Focus 2): with
// the claim held and nothing answering, it returns Dial's own error and never
// calls start. With the lock free it starts one and dials it, and a start
// that fails is its error.
func TestEnsureNeverStartsASecondDaemon(t *testing.T) {
	dir := terminals(t)
	release, err := holdwire.ClaimDaemon(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	start := func() error { called++; return nil }
	// Dial's own wait, five seconds, is TestDialNeverTrustsAFailedConnect's.
	c, started, err := ensure(dir, 0, start)
	want := fmt.Sprintf("the rota daemon (pid %d) is not answering on %s", os.Getpid(), holdwire.CtlSock(dir))
	if c != nil || started || err == nil || err.Error() != want || called != 0 {
		t.Fatalf("Ensure with the lock held and nothing answering: %v %v %v, start called %d times; want %q and no start",
			c, started, err, called, want)
	}
	release()

	refused := errors.New("no daemon for you")
	if c, started, err := Ensure(dir, func() error { return refused }); c != nil || started || !errors.Is(err, refused) {
		t.Fatalf("Ensure whose start fails: %v %v %v", c, started, err)
	}
	c, started, err = Ensure(dir, func() error { daemonAt(t, dir, twoSessions, nil); return nil })
	if err != nil || !started || c == nil || c.Health() != nil {
		t.Fatalf("Ensure with the lock free: %v %v %v", c, started, err)
	}
	if c, started, err := Ensure(dir, start); err != nil || started || c == nil || called != 0 {
		t.Fatalf("Ensure with a daemon answering: %v %v %v, start called %d times", c, started, err, called)
	}
}

// Create sends what it was given, under the daemon's own names, and reads
// the session it answers with; Get reads one; a refused send is a Refusal
// carrying the status, the reason a program acts on, and the sentence.
func TestCreateGetAndARefusedSend(t *testing.T) {
	dir := terminals(t)
	got := make(chan map[string]any, 1)
	doc := `{"id":"a1b2c3d4e5f60718","held":true,"name":"refactor","label":"refactor","kind":"account",
		"cwd":"/src","started":"2026-11-07T10:00:00Z","ended":false,"state":"live","status":"running",
		"pid":77,"account":{"id":0,"label":"","provider":""}}`
	daemonAt(t, dir, twoSessions, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /v1/terminals", func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			got <- body
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, doc)
		})
		mux.HandleFunc("GET /v1/terminals/{id}", func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("id") != "a1b2c3d4e5f60718" {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":"no terminal `+r.PathValue("id")+`"}`)
				return
			}
			fmt.Fprint(w, doc)
		})
		mux.HandleFunc("POST /v1/terminals/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["text"] != "hi" || body["force"] != true {
				t.Errorf("the send carried %v", body)
			}
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"alice holds the keyboard of this session","reason":"keyboard","holder":"alice"}`)
		})
	})
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Create(Create{Args: []string{"--model", "opus"}, Cwd: "/src", Cols: 100, Rows: 30,
		Label: "refactor", Env: []string{"FOO=bar"}, RemoteControl: true, RemoteControlName: "laptop"})
	if err != nil || s.ID != "a1b2c3d4e5f60718" || s.Name != "refactor" || s.State != "live" || s.PID != 77 {
		t.Fatalf("Create: %+v %v", s, err)
	}
	want := `{"args":["--model","opus"],"cols":100,"cwd":"/src","env":["FOO=bar"],"label":"refactor","remote_control":"laptop","rows":30}`
	if raw, _ := json.Marshal(<-got); string(raw) != want {
		t.Fatalf("Create sent %s, want %s", raw, want)
	}
	if _, err := c.Create(Create{Account: 2, RemoteControl: true}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(<-got); string(raw) != `{"account":2,"remote_control":true}` {
		t.Fatalf("Create with an account and Remote Control unnamed sent %s", raw)
	}

	if s, err := c.Get("a1b2c3d4e5f60718"); err != nil || s.Name != "refactor" {
		t.Fatalf("Get: %+v %v", s, err)
	}
	var no *Refusal
	if _, err := c.Get("ffffffffffffffff"); !errors.As(err, &no) || no.Status != http.StatusNotFound || no.Message != "no terminal ffffffffffffffff" {
		t.Fatalf("Get of nobody: %v", err)
	}
	err = c.Send("a1b2c3d4e5f60718", SendReq{Text: "hi", Force: true})
	if !errors.As(err, &no) || no.Status != http.StatusConflict || no.Reason != "keyboard" ||
		no.Message != "alice holds the keyboard of this session" || no.Holder != "alice" || err.Error() != no.Message {
		t.Fatalf("a refused send: %#v", err)
	}
}
