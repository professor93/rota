package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if code, body := ask(local, "GET", "/v1/terminals/0123456789abcdef/send", ""); code != http.StatusMethodNotAllowed {
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
		{"POST", "/v1/terminals/0123456789abcdef/send", `{"text":"hi"}`},
		{"DELETE", "/v1/terminals/0123456789abcdef", ""},
		{"POST", "/v1/run", `{"prompt":"hi"}`},
		{"POST", "/v1/invites", `{}`},
	} {
		code, body := ask(watch, c.method, c.path, c.body, "Authorization", "Bearer x")
		if code != http.StatusForbidden || errorOf(body) != watchOnly {
			t.Errorf("%s %s as the watch principal answers %d %s, want 403 %q", c.method, c.path, code, body, watchOnly)
		}
	}
	// The socket that starts a run is control, and refused before any
	// upgrade.
	if code, body := ask(watch, "GET", "/v1/ws", ""); code != http.StatusForbidden || errorOf(body) != watchOnly {
		t.Errorf("GET /v1/ws as the watch principal answers %d %s, want 403 %q", code, body, watchOnly)
	}
}

// The name local is the control socket's own principal, and the person it
// types as when nothing else is said. A user called local would be that
// person too, and its log lines would read as the socket's, so no user may
// have that name — in the file, and in Options for whoever builds a server
// without one.
func TestNoUserMayBeCalledLocal(t *testing.T) {
	want := `[[users]] "local": the name "local" is reserved for the control socket's own principal; give this user another`
	body := "[[users]]\nname = \"local\"\nrole = \"control\"\npassword = \"" + goodHash() + "\"\n"
	if _, err := LoadConfig(write(t, body)); err == nil || !strings.HasSuffix(err.Error(), ": "+want) {
		t.Fatalf("a user called local in the file answers %v, want %q", err, want)
	}
	_, err := New(Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1,
		Users: []User{{Name: "local", Role: RoleWatch, Password: goodHash()}}})
	if err == nil || err.Error() != "api: "+want {
		t.Fatalf("a user called local in Options answers %v, want %q", err, "api: "+want)
	}
	// A token is never a person, and may be called anything.
	body = "[[tokens]]\nname = \"local\"\nrole = \"watch\"\nsha256 = \"" + strings.Repeat("ab", 32) + "\"\n"
	if _, err := LoadConfig(write(t, body)); err != nil {
		t.Fatalf("a token called local is refused: %v", err)
	}
}
