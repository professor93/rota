package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/pty"
)

// These are about LocalHandler: every route, served to one principal fixed
// by whoever mounts it — the control socket, which knows who is at the other
// end from the socket itself, and later an SSH server, from a key. Nothing a
// request carries can change who it is.

// The two principals the control socket serves: this machine's own user, and
// the same user from inside a held session, who may only watch.
var (
	localControl = Principal{Name: "local", Role: RoleControl, Via: "local"}
	localWatch   = Principal{Name: "local", Role: RoleWatch, Via: "local"}
)

// apiOnly is a server with the API group alone: no page, no sockets, no
// terminals, and no daemon.
func apiOnly(t *testing.T) *Server {
	t.Helper()
	s, err := New(Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1, Routes: &Routes{API: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s
}

// ask sends one request to a handler and answers the status and the body.
func ask(h http.Handler, method, path, body string, headers ...string) (int, string) {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// errorOf is the sentence an answer refused with.
func errorOf(body string) string {
	var doc struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &doc)
	return doc.Error
}

// LocalHandler is every group whatever the routes say — the terminals, the
// send and the sockets included — because what it is mounted on is this
// machine's own user, not a port somebody chose what to open on. Handler is
// unchanged: a group that is off is still a 404 there. Nothing a browser
// alone uses is served: no page, and no cookie sign-in.
func TestLocalHandlerHasEveryGroup(t *testing.T) {
	s := apiOnly(t)
	local := s.LocalHandler(&localControl)
	for _, path := range []string{"/v1/terminals", "/v1/health", "/v1/accounts"} {
		if code, body := ask(local, "GET", path, ""); code != http.StatusOK {
			t.Errorf("GET %s on LocalHandler answers %d %s, want 200", path, code, body)
		}
	}
	// A socket route is there: a plain GET is told it is not an upgrade by
	// the route itself, rather than 404 by the mux.
	if code, body := ask(local, "GET", "/v1/ws", ""); code != http.StatusBadRequest || errorOf(body) == "" {
		t.Errorf("GET /v1/ws on LocalHandler answers %d %s, want the socket's own 400", code, body)
	}
	if code, body := ask(local, "GET", "/v1/terminals/0123456789abcdef/messages", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("the send route is missing from LocalHandler: GET on it answers %d %s, want 405", code, body)
	}
	code, body := ask(local, "GET", "/v1/session", "")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/session on LocalHandler answers %d %s", code, body)
	}
	var who sessionView
	if err := json.Unmarshal([]byte(body), &who); err != nil || who.Name != "local" || who.Role != RoleControl || who.Via != "local" {
		t.Errorf("GET /v1/session on LocalHandler says %s, want local, control, local", body)
	}
	for _, path := range []string{"/", "/playground", "/terminal", "/assets/app.css", "/invite/abc"} {
		if code, _ := ask(local, "GET", path, ""); code != http.StatusNotFound {
			t.Errorf("GET %s on LocalHandler answers %d; nothing a browser alone uses is served there", path, code)
		}
	}
	if code, _ := ask(local, "POST", "/v1/session", `{"name":"a","password":"b"}`); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/session on LocalHandler answers %d; nobody signs in there", code)
	}

	if code, body := ask(s.Handler(), "GET", "/v1/terminals", "", "Authorization", "Bearer x"); code != http.StatusNotFound {
		t.Errorf("GET /v1/terminals on Handler with the terminal off answers %d %s, want 404 as before", code, body)
	}
}

// Every request on LocalHandler is its principal's, and only the role table
// decides: a cross-site Origin, a wrong bearer token and a cookie nobody
// issued are all ignored, and a wrong token is not counted as a guess.
func TestLocalNeedsNoTokenAndNoOrigin(t *testing.T) {
	s := apiOnly(t)
	local := s.LocalHandler(&localControl)
	for range 12 {
		code, body := ask(local, "POST", "/v1/terminals", "{",
			"Origin", "https://evil.example", "Authorization", "Bearer wrong", "Cookie", sessionCookie+"=nobodys")
		// The handler's own answer to a body that is not JSON: the guard let
		// it through.
		if code != http.StatusBadRequest {
			t.Fatalf("POST /v1/terminals from another origin answers %d %s, want the handler's 400", code, body)
		}
	}
	// A socket is held to the same: the route answers for itself.
	code, body := ask(local, "GET", "/v1/terminals/0123456789abcdef/ws", "", "Origin", "https://evil.example")
	if code != http.StatusNotFound || !strings.HasPrefix(errorOf(body), "no terminal") {
		t.Fatalf("a socket from another origin answers %d %s, want the route's own 404", code, body)
	}
	// Twelve wrong tokens on the local handler blocked nobody.
	if s.limit.blocked("192.0.2.1") {
		t.Fatal("a token LocalHandler never read was counted as a guess")
	}
}

// The Origin rule is skipped by LocalHandler itself, not by who its
// principal is: an SSH server will mount it with a user, and that user's
// request, with a cross-site Origin or none at all, is let through to the
// route exactly as local's is. Handler still holds a user to the rule.
func TestLocalHandlerNeverReadsOriginForAnyPrincipal(t *testing.T) {
	s := apiOnly(t)
	user := &Principal{Name: "alice", Role: RoleControl, Via: "user"}
	h := s.LocalHandler(user)
	for _, origin := range []string{"https://evil.example", ""} {
		var headers []string
		if origin != "" {
			headers = []string{"Origin", origin}
		}
		if code, body := ask(h, "POST", "/v1/terminals", "{", headers...); code != http.StatusBadRequest {
			t.Errorf("a user's POST through LocalHandler with Origin %q answers %d %s, want the route's own 400", origin, code, body)
		}
		code, body := ask(h, "GET", "/v1/terminals/0123456789abcdef/ws", "", headers...)
		if code != http.StatusNotFound || !strings.HasPrefix(errorOf(body), "no terminal") {
			t.Errorf("a user's socket through LocalHandler with Origin %q answers %d %s, want the route's own 404", origin, code, body)
		}
	}
	code, body := ask(h, "GET", "/v1/session", "")
	if code != http.StatusOK || !strings.Contains(body, `"via":"user"`) {
		t.Errorf("GET /v1/session through LocalHandler as a user says %d %s", code, body)
	}
}

// The watch principal reads and changes nothing, whatever it carries: even
// the server's own token on the request does not make it control.
func TestLocalWatchCannotChange(t *testing.T) {
	s := apiOnly(t)
	watch := s.LocalHandler(&localWatch)
	if code, body := ask(watch, "GET", "/v1/terminals", ""); code != http.StatusOK {
		t.Fatalf("the watch principal cannot list the terminals: %d %s", code, body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/terminals", `{"account":1}`},
		{"POST", "/v1/terminals/0123456789abcdef/messages", `{"text":"hi"}`},
		{"DELETE", "/v1/terminals/0123456789abcdef", ""},
		{"POST", "/v1/run", `{"prompt":"hi"}`},
		{"POST", "/v1/invites", `{}`},
	} {
		code, body := ask(watch, c.method, c.path, c.body, "Authorization", "Bearer x")
		if code != http.StatusForbidden || errorOf(body) != callerWatchOnly {
			t.Errorf("%s %s as the watch principal answers %d %s, want 403 %q", c.method, c.path, code, body, callerWatchOnly)
		}
	}
	// The socket that starts a run is control, and refused before any
	// upgrade.
	if code, body := ask(watch, "GET", "/v1/ws", ""); code != http.StatusForbidden || errorOf(body) != callerWatchOnly {
		t.Errorf("GET /v1/ws as the watch principal answers %d %s, want 403 %q", code, body, callerWatchOnly)
	}
}

// The name local is the control socket's own principal, and the person it
// types as when nothing else is said. A user called local would be that
// person too, and a token called local would read as the socket wherever a
// name is shown — who holds a keyboard, and every audit line — so on a
// server with a control socket, one with HoldCommand, neither may have that
// name. A server without one, foreground serve, has no principal called
// local, and a file that worked there before the daemon still does.
// ReservedName is the one sentence for all of them.
func TestNoUserOrTokenOfADaemonMayBeCalledLocal(t *testing.T) {
	const want = `the name "local" is reserved for the control socket's own principal; choose another`
	if err := ReservedName("local"); err == nil || err.Error() != want {
		t.Fatalf("ReservedName(local) is %v, want %q", err, want)
	}
	if err := ReservedName("alice"); err != nil {
		t.Fatalf("ReservedName(alice) is %v", err)
	}
	user := "[[users]]\nname = \"local\"\nrole = \"control\"\npassword = \"" + goodHash() + "\"\n"
	token := "[[tokens]]\nname = \"local\"\nrole = \"watch\"\nsha256 = \"" + strings.Repeat("ab", 32) + "\"\n"
	hold := func() *exec.Cmd { return exec.Command(os.DevNull) }
	for _, c := range []struct{ table, body string }{{"users", user}, {"tokens", token}} {
		cfg, err := LoadConfig(write(t, c.body))
		if err != nil {
			t.Fatalf("a %s entry called local in the file is refused: %v", c.table, err)
		}
		opts, _, err := cfg.Serve()
		if err != nil {
			t.Fatal(err)
		}
		opts.Token, opts.Dir, opts.RefreshEvery = "x", t.TempDir(), -1
		srv, err := New(opts)
		if err != nil {
			t.Fatalf("foreground serve with a %s entry called local: %v", c.table, err)
		}
		srv.Stop()
		at := `[[` + c.table + `]] "local": `
		if err := ReservedNames(opts.Users, opts.Tokens); err == nil || err.Error() != at+want {
			t.Errorf("a daemon's file with a %s entry called local answers %v, want %q", c.table, err, at+want)
		}
		// Where held sessions do not run, a daemon is refused before that.
		if pty.Supported {
			opts.HoldCommand = hold
			if _, err := New(opts); err == nil || err.Error() != "api: "+at+want {
				t.Errorf("a daemon with a %s entry called local answers %v, want %q", c.table, err, "api: "+at+want)
			}
		}
	}
}

// The environment a session starts from, and an account's Remote Control
// turned on for it, are this machine's own user's to hand a daemon (P30,
// P14): over the control socket, as local. A bearer token, or a user even
// through LocalHandler, is refused before anything starts, an empty env
// included. Over the socket both are read, remote_control is true, false or
// a name, and an empty env is refused.
func TestEnvIsTakenOnlyOverTheControlSocket(t *testing.T) {
	s, err := New(Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1, Routes: &Routes{API: true, WebSocket: true, Terminal: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	user := s.LocalHandler(&Principal{Name: "alice", Role: RoleControl, Via: "user"})
	const env, rc = "env is taken only over the control socket", "remote_control is taken only over the control socket"
	for _, c := range []struct{ body, want string }{
		{`{"account":1,"env":["FOO=bar"]}`, env},
		{`{"account":1,"env":[]}`, env},
		{`{"account":1,"remote_control":true}`, rc},
		{`{"account":1,"remote_control":"laptop"}`, rc},
	} {
		for name, h := range map[string]http.Handler{"a token": s.Handler(), "a user": user} {
			if code, body := ask(h, "POST", "/v1/terminals", c.body, "Authorization", "Bearer x"); code != http.StatusForbidden || errorOf(body) != c.want {
				t.Errorf("%s sending %s answers %d %s, want 403 %q", name, c.body, code, body, c.want)
			}
		}
	}
	if code, body := ask(s.Handler(), "POST", "/v1/terminals", `{"account":1,"env":null,"remote_control":false}`, "Authorization", "Bearer x"); code == http.StatusForbidden {
		t.Errorf("a token sending no env and remote_control false is refused: %s", body)
	}
	local := s.LocalHandler(&localControl)
	for _, body := range []string{`{"account":1,"env":["FOO=bar"]}`, `{"account":1,"remote_control":"laptop"}`} {
		// Past the guard: the store here has no account 1, or this platform
		// no pseudo-terminal, and either says so.
		if code, answer := ask(local, "POST", "/v1/terminals", body); code == http.StatusForbidden || code == http.StatusBadRequest {
			t.Errorf("local sending %s answers %d %s", body, code, answer)
		}
	}
	for _, body := range []string{`{"remote_control":""}`, `{"remote_control":"  "}`, `{"remote_control":1}`} {
		if code, answer := ask(local, "POST", "/v1/terminals", body); code != http.StatusBadRequest {
			t.Errorf("local sending %s answers %d %s, want 400", body, code, answer)
		}
	}
	// An empty env would start the session with nothing, not even a PATH.
	const empty = "env is empty, which would start the session with no PATH; leave it out for the daemon's own"
	if code, answer := ask(local, "POST", "/v1/terminals", `{"account":1,"env":[]}`); code != http.StatusBadRequest || errorOf(answer) != empty {
		t.Errorf("local sending an empty env answers %d %s, want 400 %q", code, answer, empty)
	}
}
