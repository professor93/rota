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
	"sync"
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
	p, id, rec, ready := heldBusy(t)
	answered := p.sendLater(id, p.signIn("alice"), "", "from alice")
	waitRecorded(t, rec, `send "from alice" force=false wait=30000`)

	bob := p.attachAs(id, p.signIn("bob"), "")
	if holder := bob.waitType("hello")["holder"]; holder != "bob" {
		t.Fatalf("bob attaching to a free keyboard did not take it: %v", holder)
	}
	bob.typeIn("half a line")
	if msg := bob.waitType("error")["message"]; msg != fmt.Sprintf(sendFirst, "alice") {
		t.Fatalf("typing while another person's send waits is refused with %q", msg)
	}

	becomeReady(t, ready)
	answeredWith(t, answered, 200)
	bob.waitOut("got:from alice")
	bob.typeIn("x\r")
	bob.waitOut("got:x")
	if got := typedInto(rec); got != "x\r" {
		t.Fatalf("the holder was given %q to type besides the send, want %q", got, "x\r")
	}
}

/* ------------------------------------------------- stopping a turn meanwhile --- */

// heldBusy is a held session, between alice and bob, whose holder types a
// send only once ready exists, as a CLI busy with a turn for a while. rec is
// everything the holder was given.
func heldBusy(t *testing.T) (p *terms, id, rec, ready string) {
	t.Helper()
	dir := t.TempDir()
	rec, ready = filepath.Join(dir, "record"), filepath.Join(dir, "ready")
	p = withHeld(t, []string{"FAKE_HOLDER_RECORD=" + rec, "FAKE_HOLDER_SEND_AFTER=" + ready}, withPersons)
	id = p.start(nil)["id"].(string)
	return p, id, rec, ready
}

// becomeReady lets a busy holder type the sends waiting in it.
func becomeReady(t *testing.T, ready string) {
	t.Helper()
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sendLater makes a send without waiting for its answer, whose status comes
// on the channel: from the page by a person's session cookie when cookie is
// set, else by the bearer token.
func (p *terms) sendLater(id, cookie, token, text string) <-chan int {
	body, _ := json.Marshal(map[string]any{"text": text})
	req, _ := http.NewRequest("POST", p.srv.URL+"/v1/terminals/"+id+"/send", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Origin", p.srv.URL)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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
	return answered
}

// waitTyped waits for the holder to have been given exactly want to type.
func waitTyped(t *testing.T, rec, want string) {
	t.Helper()
	for range 500 {
		if typedInto(rec) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the holder was given %q, want %q", typedInto(rec), want)
}

// answeredWith waits for a send made with sendLater to be answered with want.
func answeredWith(t *testing.T, answered <-chan int, want int) {
	t.Helper()
	select {
	case code := <-answered:
		if code != want {
			t.Fatalf("the send answered %d, want %d", code, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the send was never answered")
	}
}

// frozenClock stops the clock the terminals are measured against, and
// returns what moves it on: how often a refusal is said is a matter of time,
// and a test about it must not be one about how fast it runs.
func (p *terms) frozenClock() (forward func(time.Duration)) {
	var mu sync.Mutex
	at := time.Now()
	p.api.clock.set(func() time.Time { mu.Lock(); defer mu.Unlock(); return at })
	return func(d time.Duration) { mu.Lock(); at = at.Add(d); mu.Unlock() }
}

// While a send waits, whoever holds the keyboard may still stop the turn:
// one frame of a single Esc or a single Ctrl-C goes through, once a hold. A
// second would do more than stop it — two Ctrl-C quit Claude Code, two Esc
// open its rewind picker — so it is refused, together or apart, and so is
// everything else: an arrow key or history recall begins with Esc but
// carries more, and Ctrl-D may end the CLI. Going through is typing under
// the keyboard's rules all the same: a connection that does not hold the
// keyboard, one attached to watch, and a watcher type nothing, and do not
// use the hold's one interrupt up. Once the send is in, keys are keys again.
func TestOneEscOrCtrlCGoesThroughAWaitingSend(t *testing.T) {
	p, id, rec, ready := heldBusy(t)
	answered := p.sendLater(id, p.signIn("alice"), "", "from alice")
	waitRecorded(t, rec, `send "from alice" force=false wait=30000`)
	bob := p.attachAs(id, p.signIn("bob"), "")
	if holder := bob.waitType("hello")["holder"]; holder != "bob" {
		t.Fatalf("bob attaching to a free keyboard did not take it: %v", holder)
	}

	for _, o := range []struct {
		who  string
		conn *term
		want string
	}{
		{"a watcher", p.attach(id, p.watch, ""), watchOnly},
		{"bob attached to watch", p.attachAs(id, p.signIn("bob"), "mode=watch"), notHolding},
		{"a connection without the keyboard", p.attach(id, p.alice, ""), notHolding},
	} {
		o.conn.waitType("hello")
		o.conn.typeIn("\x03")
		if msg := o.conn.waitType("error")["message"]; msg != o.want {
			t.Fatalf("Ctrl-C from %s while a send waits is answered %q, want %q", o.who, msg, o.want)
		}
	}

	bob.typeIn("\x1b[A")
	if msg := bob.waitType("error")["message"]; msg != fmt.Sprintf(sendFirst, "alice") {
		t.Fatalf("an arrow key while another person's send waits is refused with %q", msg)
	}
	for _, key := range []string{"\x04", "\x1b\x1b", "\x03\x03", "\x1b\x03"} {
		bob.typeIn(key)
	}
	bob.typeIn("\x1b")
	waitRecorded(t, rec, `input "\x1b"`)
	bob.typeIn("\x03")
	bob.typeIn("\x1b")
	bob.settled()
	if got := typedInto(rec); got != "\x1b" {
		t.Fatalf("the holder was given %q while the send waited, want one Esc alone", got)
	}

	becomeReady(t, ready)
	answeredWith(t, answered, 200)
	bob.waitOut("from alice")
	bob.typeIn("\x03\x03")
	bob.typeIn("\x1b\x1b")
	waitRecorded(t, rec, `input "\x1b\x1b"`)
	if got, want := typedInto(rec), "\x1b\x03\x03\x1b\x1b"; got != want {
		t.Fatalf("once the send was in the holder was given %q, want %q", got, want)
	}
}

// An interrupt stops the turn, not the send: the send goes on waiting, still
// holds the session, and is typed once the holder finds the CLI ready. The
// next send's hold lets one interrupt through again.
func TestAnInterruptLeavesAWaitingSendToBeTyped(t *testing.T) {
	p, id, rec, ready := heldBusy(t)
	alice := p.signIn("alice")
	answered := p.sendLater(id, alice, "", "from alice")
	waitRecorded(t, rec, `send "from alice" force=false wait=30000`)
	bob := p.attachAs(id, p.signIn("bob"), "")
	if holder := bob.waitType("hello")["holder"]; holder != "bob" {
		t.Fatalf("bob attaching to a free keyboard did not take it: %v", holder)
	}

	bob.typeIn("\x03")
	bob.waitOut("^C")
	bob.typeIn("after")
	if msg := bob.waitType("error")["message"]; msg != fmt.Sprintf(sendFirst, "alice") {
		t.Fatalf("after an interrupt, typing while the send waits is refused with %q", msg)
	}
	select {
	case code := <-answered:
		t.Fatalf("the interrupt answered the send, %d", code)
	default:
	}

	becomeReady(t, ready)
	answeredWith(t, answered, 200)
	bob.waitOut("got:from alice")
	bob.typeIn("x\r")
	bob.waitOut("got:x")

	// bob lets the keyboard go, so that alice's next send is let in, and
	// takes it again while that one waits.
	if err := os.Remove(ready); err != nil {
		t.Fatal(err)
	}
	bob.say(map[string]any{"type": "release"})
	bob.settled()
	again := p.sendLater(id, alice, "", "again")
	waitRecorded(t, rec, `send "again" force=false wait=30000`)
	bob.say(map[string]any{"type": "claim"})
	bob.waitDoc("the keyboard back", func(doc map[string]any) bool {
		return doc["type"] == "keyboard" && doc["holder"] == "bob"
	})
	bob.typeIn("\x03")
	waitTyped(t, rec, "\x03x\r\x03")
	bob.typeIn("\x03")
	bob.settled()
	becomeReady(t, ready)
	answeredWith(t, again, 200)
	bob.waitOut("got:again")
	if got := typedInto(rec); got != "\x03x\r\x03" {
		t.Fatalf("the holder was given %q besides the sends, want one Ctrl-C a send and %q", got, "x\r")
	}
}

// A send is let in and holds the session in one step. Somebody who takes
// the free keyboard the moment after it is let in, before its frame has even
// gone to the holder, finds the session held already, and the half-line
// they type is refused rather than submitted by the send's Enter. A token's
// send is let in the same way.
func TestASendHoldsTheSessionFromTheMomentItIsLetIn(t *testing.T) {
	for _, by := range []string{"person", "token"} {
		t.Run(by, func(t *testing.T) {
			letIn, goOn := make(chan struct{}, 1), make(chan struct{})
			was := heldSendLetIn
			heldSendLetIn = func() {
				letIn <- struct{}{}
				<-goOn
			}
			t.Cleanup(func() { heldSendLetIn = was })
			p, id, rec, ready := heldBusy(t)
			release := sync.OnceFunc(func() { close(goOn) })
			t.Cleanup(release)

			cookie := ""
			if by == "person" {
				cookie = p.signIn("alice")
			}
			answered := p.sendLater(id, cookie, p.alice, "from alice")
			select {
			case <-letIn:
			case code := <-answered:
				t.Fatalf("the send answered %d without being let in", code)
			}
			bob := p.attachAs(id, p.signIn("bob"), "")
			if holder := bob.waitType("hello")["holder"]; holder != "bob" {
				t.Fatalf("bob attaching to a free keyboard did not take it: %v", holder)
			}
			bob.typeIn("half a line")
			bob.settled()
			release()

			waitRecorded(t, rec, `send "from alice" force=false wait=30000`)
			if errs := bob.errors(); len(errs) != 1 || errs[0] != fmt.Sprintf(sendFirst, "alice") {
				t.Fatalf("typing the moment a send was let in was answered %q, want the send named", errs)
			}
			becomeReady(t, ready)
			answeredWith(t, answered, 200)
			if got := typedInto(rec); got != "" {
				t.Fatalf("the holder was given %q to type in front of the send", got)
			}
		})
	}
}

// A connection typing while a send waits is told so at most once every
// sendFirstEvery while its keys go on being refused: a typist on a slow link
// who keeps typing is not sent a refusal for every key until it is too far
// behind to be kept, and a key never vanishes for long without a reason.
// Another connection is told on its own.
func TestAWaitingSendIsNamedAtMostEveryTwoSeconds(t *testing.T) {
	p, id, rec, ready := heldBusy(t)
	forward := p.frozenClock()
	answered := p.sendLater(id, p.signIn("alice"), "", "from alice")
	waitRecorded(t, rec, `send "from alice" force=false wait=30000`)
	bob := p.attachAs(id, p.signIn("bob"), "")
	if holder := bob.waitType("hello")["holder"]; holder != "bob" {
		t.Fatalf("bob attaching to a free keyboard did not take it: %v", holder)
	}
	named := fmt.Sprintf(sendFirst, "alice")
	saidOnly := func(conn *term, n int, when string) {
		t.Helper()
		errs := conn.errors()
		if len(errs) != n {
			t.Fatalf("%s the refusals were said %d times, want %d", when, len(errs), n)
		}
		for _, e := range errs {
			if e != named {
				t.Fatalf("%s a refusal was said as %q, want %q", when, e, named)
			}
		}
	}

	for range 100 {
		bob.typeIn("x")
	}
	bob.settled()
	saidOnly(bob, 1, "with no time passing, after 100 refused frames")
	desk := p.attachAs(id, p.signIn("bob"), "")
	desk.waitType("hello")
	desk.typeIn("y")
	desk.typeIn("y")
	desk.settled()
	saidOnly(desk, 1, "on another connection of bob's,")

	forward(sendFirstEvery - time.Millisecond)
	bob.typeIn("x")
	bob.settled()
	saidOnly(bob, 1, "just short of sendFirstEvery later,")
	forward(time.Millisecond)
	bob.typeIn("x")
	bob.settled()
	saidOnly(bob, 2, "sendFirstEvery later,")

	becomeReady(t, ready)
	answeredWith(t, answered, 200)
	if got := typedInto(rec); got != "" {
		t.Fatalf("the holder was given %q while the send waited", got)
	}
}

// A token is never a person, so its send holds the session against everyone,
// its own connections too, under the same rules: one Esc or one Ctrl-C from
// whoever holds the keyboard goes through, and a refusal is said at most so
// often.
func TestATokensSendHoldsTheSessionAgainstEveryoneButAnInterrupt(t *testing.T) {
	p, id, rec, ready := heldBusy(t)
	p.frozenClock()
	answered := p.sendLater(id, "", p.alice, "from a token")
	waitRecorded(t, rec, `send "from a token" force=false wait=30000`)
	own := p.attach(id, p.alice, "")
	if holder := own.waitType("hello")["holder"]; holder != "alice" {
		t.Fatalf("the token's own connection did not take the free keyboard: %v", holder)
	}

	own.typeIn("mine")
	own.typeIn("more")
	own.typeIn("\x03")
	waitRecorded(t, rec, `input "\x03"`)
	own.typeIn("\x1b")
	own.settled()
	if errs := own.errors(); len(errs) != 1 || errs[0] != fmt.Sprintf(sendFirst, "alice") {
		t.Fatalf("the token's own connection typing under its send was answered %q", errs)
	}
	if got := typedInto(rec); got != "\x03" {
		t.Fatalf("the holder was given %q while the token's send waited, want one Ctrl-C alone", got)
	}

	becomeReady(t, ready)
	answeredWith(t, answered, 200)
	own.waitOut("got:from a token")
}
