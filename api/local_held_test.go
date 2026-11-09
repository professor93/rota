//go:build linux || darwin

package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/holdwire"
)

// These are about the local principal at a held session's keyboard. local is
// a person like any user signed in on the page, so its connections share the
// keyboard the way one person's devices do; and the control socket's other
// principal, local from inside a held session, only ever watches.

/* ------------------------------------------------------------ the harness --- */

// door is the same server reached through LocalHandler as pr, on a listener
// of its own: what the control socket is, without the socket.
func (p *terms) door(pr *Principal) *terms {
	p.tt.Helper()
	h := *p.harness
	h.srv = httptest.NewServer(p.api.LocalHandler(pr))
	p.tt.Cleanup(h.srv.Close)
	return &terms{harness: &h, tt: p.tt, watch: p.watch, alice: p.alice, bob: p.bob}
}

// attachHere attaches carrying nothing at all, which is how everything
// reaches LocalHandler.
func (p *terms) attachHere(id, query string) *term {
	p.tt.Helper()
	path := "/v1/terminals/" + id + "/ws"
	if query != "" {
		path += "?" + query
	}
	c, resp := dialWS(p.tt, p.harness, path)
	if c == nil {
		p.tt.Fatalf("the handshake was refused: %d", resp.StatusCode)
	}
	return &term{t: p.tt, c: c}
}

/* ---------------------------------------------------------- the keyboard --- */

// Two clients of the control socket are one person, local, on two devices:
// whichever of them types holds the keyboard from then on, without asking,
// and a send from the socket types while either holds it. A bearer token is
// still nobody, and is refused while local holds the keyboard.
func TestTwoLocalClientsShareTheKeyboard(t *testing.T) {
	p := withHeld(t, nil)
	ctl := p.door(&localControl)
	id := ctl.start(nil)["id"].(string)

	a := ctl.attachHere(id, "")
	helloA := a.waitType("hello")
	if helloA["holder"] != "local" || helloA["holder_conn"] != connOf(helloA) {
		t.Fatalf("the first local client to attach holds the keyboard: %v", helloA)
	}
	b := ctl.attachHere(id, "")
	helloB := b.waitType("hello")
	if helloB["holder_conn"] != connOf(helloA) {
		t.Fatalf("a second local client attaching moved the keyboard: %v", helloB)
	}

	b.typeIn("from b\r")
	b.waitOut("got:from b")
	if errs := b.errors(); len(errs) > 0 {
		t.Fatalf("the second local client was refused: %q", errs)
	}
	a.waitDoc("the keyboard on the second client", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder"] == "local" && d["holder_conn"] == connOf(helloB)
	})
	a.typeIn("from a\r")
	a.waitOut("got:from a")
	b.waitDoc("the keyboard back on the first client", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder_conn"] == connOf(helloA)
	})

	resp, raw := ctl.do("POST", "/v1/terminals/"+id+"/messages", map[string]any{"text": "sent"})
	if resp.StatusCode != 200 {
		t.Fatalf("a send from the control socket while local holds the keyboard answered %d %s", resp.StatusCode, raw)
	}
	a.waitOut("got:sent")
	if code, doc := p.send(p.alice, id, map[string]any{"text": "x"}); code != 409 || doc["holder"] != "local" {
		t.Fatalf("a token's send while local holds the keyboard answered %d %v", code, doc)
	}
}

// The watch principal never holds the keyboard: not by attaching to a
// session nobody holds, not by typing or claiming, not in the grace its own
// person's connection left, and not by a send. What it types never reaches
// the CLI.
func TestALocalWatcherNeverTakesTheKeyboard(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "record")
	p := withHeld(t, []string{"FAKE_HOLDER_RECORD=" + rec})
	ctl, look := p.door(&localControl), p.door(&localWatch)
	id := ctl.start(nil)["id"].(string)

	w := look.attachHere(id, "")
	if hello := w.waitType("hello"); hello["holder"] != nil {
		t.Fatalf("the watch principal attaching to a free keyboard took it: %v", hello)
	}
	w.typeIn("peek\r")
	if msg := w.waitType("error")["message"]; msg != callerWatchOnly {
		t.Fatalf("the watch principal typing is refused with %q, want %q", msg, callerWatchOnly)
	}
	w.say(map[string]any{"type": "claim"})
	if msg := w.waitType("error")["message"]; msg != callerWatchOnly {
		t.Fatalf("the watch principal claiming is refused with %q, want %q", msg, callerWatchOnly)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/terminals/" + id + "/messages", map[string]any{"text": "peek"}},
		{"DELETE", "/v1/terminals/" + id, nil},
		{"POST", "/v1/terminals", map[string]any{"account": 1}},
	} {
		resp, raw := look.do(c.method, c.path, c.body)
		if resp.StatusCode != 403 || errorOf(string(raw)) != callerWatchOnly {
			t.Fatalf("%s %s as the watch principal answered %d %s", c.method, c.path, resp.StatusCode, raw)
		}
	}

	// Its person's control client holds the keyboard, and drops: the grace is
	// local's, and the watcher, local too, still may not have it.
	a := ctl.attachHere(id, "")
	if hello := a.waitType("hello"); hello["holder_conn"] != connOf(hello) {
		t.Fatalf("the control client did not take the free keyboard: %v", hello)
	}
	a.typeIn("ok\r")
	a.waitOut("got:ok")
	a.c.conn.Close()
	p.waitViewers(id, 1)
	w.typeIn("now\r")
	if msg := w.waitType("error")["message"]; msg != callerWatchOnly {
		t.Fatalf("the watch principal typing in local's grace is refused with %q, want %q", msg, callerWatchOnly)
	}
	w.say(map[string]any{"type": "claim"})
	if msg := w.waitType("error")["message"]; msg != callerWatchOnly {
		t.Fatalf("the watch principal claiming in local's grace is refused with %q, want %q", msg, callerWatchOnly)
	}
	again := look.attachHere(id, "")
	again.waitType("hello")
	again.settled()
	w.settled()
	for _, tm := range []*term{w, again} {
		for _, d := range tm.c.log {
			if d["type"] == "keyboard" && d["holder_conn"] != nil && d["holder_conn"] != connOfAny(a) {
				t.Fatalf("the keyboard went to a watch connection: %v", d)
			}
		}
	}
	if got := typedInto(rec); strings.Contains(got, "peek") || strings.Contains(got, "now") {
		t.Fatalf("the watch principal's typing reached the CLI: %q", got)
	}
}

// connOfAny is a connection's id from the hello it was sent.
func connOfAny(tm *term) string {
	for _, d := range tm.c.log {
		if d["type"] == "hello" {
			return connOf(d)
		}
	}
	return ""
}

// local counts as the person auth.local_user names, so it is that person's
// device: it takes the keyboard from their page by typing, and gets it back
// by coming back within the grace that person's own connection left.
func TestLocalIsTheLocalUsersDevice(t *testing.T) {
	p := withHeld(t, nil, withPersons, func(o *Options) { o.LocalUser = "alice" })
	ctl := p.door(&localControl)
	id := p.start(nil)["id"].(string)

	page := p.attachAs(id, p.signIn("alice"), "")
	if hello := page.waitType("hello"); hello["holder"] != "alice" {
		t.Fatalf("alice's page did not take the keyboard: %v", hello)
	}
	here := ctl.attachHere(id, "")
	helloHere := here.waitType("hello")
	here.typeIn("from the socket\r")
	here.waitOut("got:from the socket")
	if errs := here.errors(); len(errs) > 0 {
		t.Fatalf("local typing as alice's device was refused: %q", errs)
	}
	page.waitDoc("the keyboard on the control socket", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder"] == "local" && d["holder_conn"] == connOf(helloHere)
	})

	// Back to the page, then the page drops: within its grace, local
	// attaching again is alice coming back on another device.
	page.typeIn("from the page\r")
	page.waitOut("got:from the page")
	here.c.conn.Close()
	page.c.conn.Close()
	p.waitViewers(id, 0)
	back := ctl.attachHere(id, "")
	if hello := back.waitType("hello"); hello["holder_conn"] != connOf(hello) {
		t.Fatalf("local coming back within alice's grace did not get the keyboard: %v", hello)
	}

	// bob is somebody else, and local is not his.
	bobs := p.attachAs(id, p.signIn("bob"), "")
	bobs.waitType("hello")
	bobs.typeIn("bob\r")
	if msg := bobs.waitType("error")["message"]; msg != notHolding {
		t.Fatalf("bob typing while local holds alice's keyboard is refused with %q, want %q", msg, notHolding)
	}
}

// The environment local sends is handed to the holder as it came, for the
// session to start from in place of the daemon's.
func TestLocalsEnvReachesTheHolder(t *testing.T) {
	log := filepath.Join(t.TempDir(), "request.json")
	p := withHeld(t, []string{"FAKE_HOLDER_LOG=" + log})
	ctl := p.door(&localControl)
	ctl.start(map[string]any{"account": 1, "env": []string{"FOO=bar", "PATH=/nowhere"}})
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var seen struct {
		Request holdwire.Request `json:"request"`
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen.Request.Env, " ") != "FOO=bar PATH=/nowhere" {
		t.Fatalf("the holder was handed env %q", seen.Request.Env)
	}
}
