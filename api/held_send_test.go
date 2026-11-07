//go:build linux || darwin

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These are about POST /v1/terminals/{id}/send: a message typed into a held
// session the way a person would, once the session is ready for one, and
// under the same keyboard rules as a person typing.

// send asks for one send with a bearer token, and reads the answer.
func (p *terms) send(token, id string, body map[string]any) (int, map[string]any) {
	p.tt.Helper()
	resp, raw := p.as(token, "POST", "/v1/terminals/"+id+"/send", body)
	return resp.StatusCode, answerOf(p.tt, raw)
}

// sendAs is the same send from the page, by a person's session cookie.
func (p *terms) sendAs(cookie, id string, body map[string]any) (int, map[string]any) {
	p.tt.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", p.srv.URL+"/v1/terminals/"+id+"/send", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", p.srv.URL)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.tt.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, answerOf(p.tt, out)
}

func answerOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the answer is not JSON: %q", raw)
	}
	return doc
}

// A send types when the keyboard is free, or held by the sender's own person,
// and is refused with who holds it otherwise. A session waiting on its person
// refuses it unless it is forced. Only a held session takes one.
func TestSendTypesUnderTheKeyboardRules(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "record")
	p := withHeld(t, []string{"FAKE_HOLDER_RECORD=" + rec}, withPersons)
	id := p.start(nil)["id"].(string)

	if code, doc := p.send(p.alice, id, map[string]any{"text": "hello"}); code != 200 || doc["ok"] != true {
		t.Fatalf("a send with the keyboard free answered %d %v", code, doc)
	}
	waitRecorded(t, rec, `send "hello" force=false wait=30000`)
	watcher := p.attach(id, p.watch, "since=0")
	watcher.waitOut("got:hello")

	bob := p.attach(id, p.bob, "")
	if holder := bob.waitType("hello")["holder"]; holder != "bob" {
		t.Fatalf("bob attaching did not take the keyboard: %v", holder)
	}
	code, doc := p.send(p.alice, id, map[string]any{"text": "over bob"})
	if code != 409 || doc["reason"] != "keyboard" || doc["holder"] != "bob" ||
		doc["error"] != "bob holds the keyboard of this session" {
		t.Fatalf("a send while somebody else holds the keyboard answered %d %v", code, doc)
	}
	// A bearer token is never a person, so not even the holder's own token
	// sends past the keyboard.
	if code, doc := p.send(p.bob, id, map[string]any{"text": "mine"}); code != 409 || doc["reason"] != "keyboard" {
		t.Fatalf("a token's send while its own connection holds the keyboard answered %d %v", code, doc)
	}

	// A person holding the keyboard on one device sends from another.
	other := p.start(nil)["id"].(string)
	cookie := p.signIn("alice")
	alice := p.attachAs(other, p.signIn("alice"), "")
	if holder := alice.waitType("hello")["holder"]; holder != "alice" {
		t.Fatalf("alice attaching did not take the keyboard: %v", holder)
	}
	if code, doc := p.sendAs(cookie, other, map[string]any{"text": "same person"}); code != 200 {
		t.Fatalf("a send by the person holding the keyboard answered %d %v", code, doc)
	}
	alice.waitOut("got:same person")
	if code, doc := p.sendAs(p.signIn("bob"), other, map[string]any{"text": "not bob"}); code != 409 ||
		doc["reason"] != "keyboard" || doc["holder"] != "alice" {
		t.Fatalf("another person's send answered %d %v", code, doc)
	}

	// Waiting on its person, a session refuses a send unless it is forced.
	q := withHeld(t, []string{"FAKE_HOLDER_SEND=waiting", "FAKE_HOLDER_RECORD=" + rec + "2"})
	waiting := q.start(nil)["id"].(string)
	if code, doc := q.send(q.alice, waiting, map[string]any{"text": "approve?"}); code != 409 || doc["reason"] != "waiting" {
		t.Fatalf("a send to a session waiting on its person answered %d %v", code, doc)
	}
	if code, doc := q.send(q.alice, waiting, map[string]any{"text": "anyway", "force": true}); code != 200 {
		t.Fatalf("a forced send answered %d %v", code, doc)
	}
	waitRecorded(t, rec+"2", `send "anyway" force=true wait=30000`)

	// A terminal this server runs itself is not a held session.
	l := withTerminals(t)
	local := l.start(nil)["id"].(string)
	if code, doc := l.send(l.alice, local, map[string]any{"text": "x"}); code != 409 || doc["reason"] != "not_held" {
		t.Fatalf("a send to a terminal that is not held answered %d %v", code, doc)
	}
	if resp, _ := l.as(l.alice, "POST", "/v1/terminals/nope/send", map[string]any{"text": "x"}); resp.StatusCode != 404 {
		t.Fatalf("a send to nobody answered %d", resp.StatusCode)
	}
	if resp, _ := p.as(p.alice, "POST", "/v1/terminals/"+id+"/send", map[string]any{"txt": "x"}); resp.StatusCode != 400 {
		t.Fatalf("a send with a misspelled field answered %d", resp.StatusCode)
	}
	// Enter alone may approve something, so a send with nothing to type is
	// no send; and a send is a message, not a file.
	if code, doc := p.send(p.alice, id, map[string]any{"text": ""}); code != 400 {
		t.Fatalf("a send with no text answered %d %v", code, doc)
	}
	if code, doc := p.send(p.alice, id, map[string]any{"text": strings.Repeat("a", heldSendMax)}); code != 413 {
		t.Fatalf("a send over %d bytes answered %d %v", heldSendMax, code, doc)
	}
	for _, line := range recorded(rec) {
		if strings.HasPrefix(line, `send "" `) || strings.HasPrefix(line, `send "aaa`) {
			t.Fatalf("a refused send reached the holder: %.40q", line)
		}
	}
	if resp, _ := p.as(p.watch, "POST", "/v1/terminals/"+id+"/send", map[string]any{"text": "x"}); resp.StatusCode != 403 {
		t.Fatalf("a watcher's send answered %d", resp.StatusCode)
	}
}

// The CLI ending while a send waits answers the send at once, as ended,
// rather than leaving it to its timeout; and a session that has ended takes
// no more.
func TestSendAnswersEndedWhenTheCLIExits(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_SEND=exit"})
	id := p.start(nil)["id"].(string)
	began := time.Now()
	code, doc := p.send(p.alice, id, map[string]any{"text": "last words"})
	took := time.Since(began)
	if code != 409 || doc["reason"] != "ended" {
		t.Fatalf("a send the CLI ended under answered %d %v", code, doc)
	}
	// The fake exits 200 ms after it takes the send.
	if took > time.Second {
		t.Fatalf("the send was answered %v after it was made, not as the CLI ended", took)
	}
	p.waitEnded(id)
	if code, doc := p.send(p.alice, id, map[string]any{"text": "after"}); code != 409 || doc["reason"] != "ended" {
		t.Fatalf("a send to an ended session answered %d %v", code, doc)
	}
}

// A send is let in under the keyboard's rule and may wait in the holder
// before it is typed. Until it is in, it holds the session for its person:
// somebody who takes the free keyboard meanwhile is refused typing, rather
// than having half a line typed in front of the send and submitted by its
// Enter, and types as ever once the send is in.
func TestASendIsNotTypedOverWhileItWaits(t *testing.T) {
	dir := t.TempDir()
	rec, busy := filepath.Join(dir, "record"), filepath.Join(dir, "ready")
	p := withHeld(t, []string{"FAKE_HOLDER_RECORD=" + rec, "FAKE_HOLDER_SEND_AFTER=" + busy}, withPersons)
	id := p.start(nil)["id"].(string)

	body, _ := json.Marshal(map[string]any{"text": "from alice"})
	req, _ := http.NewRequest("POST", p.srv.URL+"/v1/terminals/"+id+"/send", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", p.srv.URL)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: p.signIn("alice")})
	answered := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- 0
			return
		}
		resp.Body.Close()
		answered <- resp.StatusCode
	}()
	waitRecorded(t, rec, `send "from alice" force=false wait=30000`)

	bob := p.attachAs(id, p.signIn("bob"), "")
	if holder := bob.waitType("hello")["holder"]; holder != "bob" {
		t.Fatalf("bob attaching to a free keyboard did not take it: %v", holder)
	}
	bob.typeIn("half a line")
	if msg := bob.waitType("error")["message"]; msg != fmt.Sprintf(sendFirst, "alice") {
		t.Fatalf("typing while another person's send waits is refused with %q", msg)
	}

	if err := os.WriteFile(busy, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := <-answered; code != 200 {
		t.Fatalf("the send answered %d", code)
	}
	bob.waitOut("got:from alice")
	bob.typeIn("x\r")
	bob.waitOut("got:x")
	if got := typedInto(rec); got != "x\r" {
		t.Fatalf("the holder was given %q to type besides the send, want %q", got, "x\r")
	}
}
