package api

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

/* ------------------------------------------------- a WebSocket client --- */

// wsClient is as much of RFC 6455 as a test needs: the handshake, masked
// frames out, whole frames in. It answers nothing on its own — a test about
// heartbeats needs a peer that can be silent.
type wsClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	// log is every frame read so far. An ack and the run's own events are
	// written by different goroutines and arrive in either order, so a test
	// looks for what it wants among everything that has come rather than only
	// among what comes next.
	log []map[string]any
}

// dialWS opens one socket. It returns the response when the handshake was
// refused, so a test can read the status the server answered with.
func dialWS(t *testing.T, h *harness, path string, headers ...string) (*wsClient, *http.Response) {
	t.Helper()
	u, err := url.Parse(h.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	for i := 0; i+1 < len(headers); i += 2 {
		req += headers[i] + ": " + headers[i+1] + "\r\n"
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := conn.Write([]byte(req + "\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, resp
	}
	// The answer is the key the request sent, mixed with the protocol's own
	// constant: an echo would not prove the server understood the handshake.
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("the handshake must be answered with the accept key: %q", got)
	}
	c := &wsClient{t: t, conn: conn, r: br}
	t.Cleanup(func() { conn.Close() })
	return c, resp
}

// dialRun opens a socket carrying the token as the subprotocol, which is how
// a browser does it.
func dialRun(t *testing.T, h *harness, path string) *wsClient {
	t.Helper()
	c, resp := dialWS(t, h, path, "Sec-WebSocket-Protocol", "rota, bearer."+h.token)
	if c == nil {
		t.Fatalf("the handshake was refused: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "rota" {
		t.Fatalf("only rota is echoed back, never the token: %q", got)
	}
	return c
}

// send writes one frame, masked as every client frame is.
func (c *wsClient) send(head byte, payload []byte) {
	c.t.Helper()
	mask := []byte{0x37, 0xfa, 0x21, 0x3d}
	out := []byte{head}
	switch n := len(payload); {
	case n < 126:
		out = append(out, 0x80|byte(n))
	case n < 1<<16:
		out = append(out, 0x80|126, byte(n>>8), byte(n))
	default:
		out = append(out, 0x80|127)
		out = binary.BigEndian.AppendUint64(out, uint64(n))
	}
	out = append(out, mask...)
	body := append([]byte(nil), payload...)
	for i := range body {
		body[i] ^= mask[i%4]
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write(append(out, body...)); err != nil {
		c.t.Fatalf("writing a frame: %v", err)
	}
}

func (c *wsClient) say(v any) {
	c.t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(0x80|opText, raw)
}

// sayInPieces sends one message as two frames, which a client is entitled to
// do and a server must put back together.
func (c *wsClient) sayInPieces(first, rest string) {
	c.t.Helper()
	c.send(opText, []byte(first)) // FIN clear: there is more coming
	c.send(0x80|opContinue, []byte(rest))
}

// frame reads one whole frame. The server never masks and never fragments,
// so this is all the reading a client of it has to do.
func (c *wsClient) frame() (byte, []byte) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		c.t.Fatalf("reading a frame: %v", err)
	}
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		io.ReadFull(c.r, b[:])
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		io.ReadFull(c.r, b[:])
		n = binary.BigEndian.Uint64(b[:])
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		c.t.Fatalf("reading a payload of %d: %v", n, err)
	}
	return head[0] & 0x0f, payload
}

// read takes one frame, keeping every event and ack it sees.
func (c *wsClient) read() (byte, []byte) {
	c.t.Helper()
	op, payload := c.frame()
	if op == opText {
		var doc map[string]any
		if err := json.Unmarshal(payload, &doc); err != nil {
			c.t.Fatalf("every frame out is one JSON object: %q", payload)
		}
		c.log = append(c.log, doc)
	}
	return op, payload
}

// wait finds a frame that satisfies want, among those already read and then
// among those still to come, and says everything it saw when none did.
func (c *wsClient) wait(what string, want func(map[string]any) bool) map[string]any {
	c.t.Helper()
	for _, doc := range c.log {
		if want(doc) {
			return doc
		}
	}
	for i := 0; i < 400; i++ {
		op, payload := c.read()
		switch op {
		case opText:
			if doc := c.log[len(c.log)-1]; want(doc) {
				return doc
			}
		case opClose:
			c.t.Fatalf("the socket closed with %d before %s arrived:\n%s",
				closeCode(payload), what, c.all())
		}
	}
	c.t.Fatalf("%s never arrived:\n%s", what, c.all())
	return nil
}

// all is everything read so far, for an error message that has to say what
// did arrive.
func (c *wsClient) all() string {
	var b strings.Builder
	for _, doc := range c.log {
		raw, _ := json.Marshal(doc)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

// ack is the answer to the frame a client sent under this ref.
func (c *wsClient) ack(ref string) map[string]any {
	c.t.Helper()
	return c.wait("an ack for "+ref, func(doc map[string]any) bool {
		return doc["type"] == "ack" && doc["ref"] == ref
	})
}

func (c *wsClient) waitType(kind string) map[string]any {
	c.t.Helper()
	return c.wait("a "+kind+" frame", func(doc map[string]any) bool { return doc["type"] == kind })
}

// expectClose reads until the close frame, checks the code, and says goodbye
// back so the server lets the connection go rather than waiting for it.
func (c *wsClient) expectClose(want uint16) {
	c.t.Helper()
	for i := 0; i < 400; i++ {
		op, payload := c.read()
		if op != opClose {
			continue
		}
		if got := closeCode(payload); got != want {
			c.t.Fatalf("the socket closed with %d, wanted %d (%q)", got, want, payload)
		}
		c.send(0x80|opClose, []byte{0x03, 0xe8})
		c.conn.Close()
		return
	}
	c.t.Fatalf("no close frame arrived")
}

func closeCode(payload []byte) uint16 {
	if len(payload) < 2 {
		return 0
	}
	return binary.BigEndian.Uint16(payload)
}

/* ------------------------------------------------------------- the tests --- */

// One socket is the whole conversation: the run starts on it, its events come
// out of it, and messages, interrupts and the close go in — each acknowledged
// by the ref the client gave it.
func TestAWebSocketStartsARunAndCarriesItBothWays(t *testing.T) {
	h := newHarness(t, Options{})
	echoClaude(t)
	c := dialRun(t, h, "/v1/accounts/1/ws")

	c.say(map[string]any{"type": "start", "prompt": "one"})
	init := c.waitType("init")
	id, _ := init["run_id"].(string)
	if id == "" {
		t.Fatalf("a run started over a socket is addressable, so it says its id: %v", init)
	}
	t.Cleanup(func() { closeRun(t, h, id) })

	c.say(map[string]any{"type": "message", "text": "two", "ref": "c1"})
	ack := c.ack("c1")
	two, _ := ack["id"].(string)
	if two == "" || ack["state"] != "accepted" {
		t.Fatalf("a message is acknowledged by the ref it carried, with the id its events will be under: %v", ack)
	}
	c.wait("the message being accepted", func(doc map[string]any) bool {
		return doc["type"] == "input" && doc["id"] == two && doc["state"] == "accepted"
	})
	c.wait("the answer to it", func(doc map[string]any) bool {
		text, _ := doc["text"].(string)
		return doc["type"] == "text" && strings.Contains(text, "echo: two")
	})
	c.wait("the message being answered", func(doc map[string]any) bool {
		return doc["type"] == "input" && doc["id"] == two && doc["state"] == "answered"
	})
	c.waitType("idle")

	c.say(map[string]any{"type": "interrupt", "ref": "c2"})
	ack = c.ack("c2")
	stop, _ := ack["id"].(string)
	if stop == "" {
		t.Fatalf("an interrupt is acknowledged by id too: %v", ack)
	}
	c.waitType("interrupted")

	c.say(map[string]any{"type": "close", "ref": "c3"})
	ack = c.ack("c3")
	if ack["id"] != nil {
		t.Fatalf("a close is acknowledged, and has no id: there is no message to have one: %v", ack)
	}
	c.waitType("done")
	c.expectClose(wsNormal)
}

// The socket and the endpoints are two ways to the same run: one started over
// HTTP is picked up on a socket, from where its first reader got to.
func TestAWebSocketAttachesToARunStartedOverHTTP(t *testing.T) {
	h := newHarness(t, Options{})
	echoClaude(t)
	s, id := startInputRun(t, h, "one")
	s.cancel() // the first reader drops; the run goes on through its grace

	c := dialRun(t, h, "/v1/runs/"+id+"/ws?since=1")
	c.wait("what the first reader missed", func(doc map[string]any) bool {
		if seq, ok := doc["seq"].(float64); ok && seq <= 1 {
			t.Fatalf("since asks for what came after it: %v", doc)
		}
		text, _ := doc["text"].(string)
		return strings.Contains(text, "echo: one")
	})

	c.say(map[string]any{"type": "message", "text": "two", "ref": "r1"})
	ack := c.ack("r1")
	if ack["id"] == "" {
		t.Fatalf("a reattached socket sends into the run like any other: %v", ack)
	}
	c.wait("the answer", func(doc map[string]any) bool {
		text, _ := doc["text"].(string)
		return strings.Contains(text, "echo: two")
	})

	c.say(map[string]any{"type": "close"})
	c.waitType("done")
	c.expectClose(wsNormal)
}

// The token is checked before the connection is taken over, so a stranger
// never gets a socket at all. It may travel as a header or as the subprotocol
// a browser can set — and never in the URL, which is written to every log on
// the way.
func TestWebSocketAuthIsCheckedBeforeTheUpgrade(t *testing.T) {
	h := newHarness(t, Options{})
	for _, call := range []struct {
		what    string
		path    string
		headers []string
	}{
		{"no token at all", "/v1/ws", nil},
		{"a wrong token in the subprotocol", "/v1/ws", []string{"Sec-WebSocket-Protocol", "rota, bearer.wrong"}},
		{"a token in the query string", "/v1/ws?token=" + h.token, nil},
		{"a token in the query string as well as a wrong header", "/v1/ws?token=" + h.token,
			[]string{"Authorization", "Bearer wrong"}},
	} {
		c, resp := dialWS(t, h, call.path, call.headers...)
		if c != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s must be refused with 401, got %d", call.what, resp.StatusCode)
		}
	}
	c, _ := dialWS(t, h, "/v1/ws", "Authorization", "Bearer "+h.token)
	if c == nil {
		t.Fatal("a bearer header is the other way in")
	}
	c.send(0x80|opClose, []byte{0x03, 0xe8})
	c.conn.Close()

	// And the version is the one refusal with an answer of its own.
	_, resp := dialWSVersion(t, h, "/v1/ws", "8")
	if resp.StatusCode != http.StatusUpgradeRequired || resp.Header.Get("Sec-WebSocket-Version") != "13" {
		t.Fatalf("a client on another version is told which one this is: %d %v", resp.StatusCode, resp.Header)
	}
}

// dialWSVersion is the handshake with a version of the client's choosing.
func dialWSVersion(t *testing.T, h *harness, path, version string) (*wsClient, *http.Response) {
	t.Helper()
	u, _ := url.Parse(h.srv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: %s\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Authorization: Bearer %s\r\n\r\n", path, u.Host, version, h.token)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	return nil, resp
}

// The framing is the protocol's, not a convenience: a message split across
// frames is one message, a ping is answered where it arrives, and anything
// this server will not read ends the socket with the code that says why.
func TestWebSocketFramesAreRobust(t *testing.T) {
	h := newHarness(t, Options{})
	echoClaude(t)

	c := dialRun(t, h, "/v1/accounts/1/ws")
	c.say(map[string]any{"type": "start", "prompt": "one"})
	id, _ := c.waitType("init")["run_id"].(string)
	t.Cleanup(func() { closeRun(t, h, id) })

	c.sayInPieces(`{"type":"mess`, `age","text":"two","ref":"f1"}`)
	ack := c.ack("f1")
	if ack["id"] == "" {
		t.Fatalf("two frames are one message: %v", ack)
	}

	c.send(0x80|opPing, []byte("are you there"))
	for {
		op, payload := c.read()
		if op != opPong {
			continue
		}
		if string(payload) != "are you there" {
			t.Fatalf("a pong carries the ping's own payload back: %q", payload)
		}
		break
	}

	// A frame that is not an object is not something this server can act on.
	c.say([]int{1, 2})
	c.expectClose(wsPolicy)

	// A frame that says it is two megabytes is refused on that alone: the
	// payload is never read, which is the point of the cap.
	big, _ := dialWS(t, h, "/v1/accounts/1/ws", "Sec-WebSocket-Protocol", "rota, bearer."+h.token)
	head := []byte{0x80 | opText, 0x80 | 127}
	head = binary.BigEndian.AppendUint64(head, 2<<20)
	if _, err := big.conn.Write(head); err != nil {
		t.Fatal(err)
	}
	big.expectClose(wsTooBig)

	bad, _ := dialWS(t, h, "/v1/accounts/1/ws", "Sec-WebSocket-Protocol", "rota, bearer."+h.token)
	bad.send(0x80|opText, []byte{'{', '"', 0xff, 0xfe, '"', '}'})
	bad.expectClose(wsBadData)
}

// Nothing is spent on a start this server would refuse: the frame is checked
// exactly as the request body is, and the socket is closed saying why.
func TestABadStartFrameIsRefusedBeforeAnythingRuns(t *testing.T) {
	h := newHarness(t, Options{})
	echoClaude(t)

	c := dialRun(t, h, "/v1/accounts/1/ws")
	c.say(map[string]any{"type": "start", "prompt": "one", "nosuchfield": true})
	e := c.waitType("error")
	if msg, _ := e["error"].(string); !strings.Contains(msg, "nosuchfield") {
		t.Fatalf("a misspelled option is named, not ignored: %v", e)
	}
	c.expectClose(wsPolicy)

	// Only claude has a streaming input, and a socket is one by construction.
	codex := dialRun(t, h, "/v1/accounts/2/ws")
	codex.say(map[string]any{"type": "start", "prompt": "one"})
	e = codex.waitType("error")
	if msg, _ := e["error"].(string); !strings.Contains(msg, "input") {
		t.Fatalf("a CLI with no streaming input is refused by name: %v", e)
	}
	codex.expectClose(wsPolicy)

	code, doc := post(t, h, "GET", "/v1/runs", nil)
	if code != http.StatusOK || fmt.Sprint(doc["runs"]) != "[]" {
		t.Fatalf("a refused start leaves no run behind: %d %v", code, doc)
	}
}

// A socket nobody is at the other end of is not a reader. The server pings,
// and a peer that answers nothing is let go — the run keeps going, with
// nobody reading it, exactly as a dropped HTTP reader leaves it.
func TestTheServerPingsAndDropsASilentPeer(t *testing.T) {
	was := heartbeat
	heartbeat = 50 * time.Millisecond
	t.Cleanup(func() { heartbeat = was })
	h := newHarness(t, Options{InputGrace: 30 * time.Second})
	echoClaude(t)

	c := dialRun(t, h, "/v1/accounts/1/ws")
	c.say(map[string]any{"type": "start", "prompt": "one"})
	id, _ := c.waitType("init")["run_id"].(string)
	t.Cleanup(func() { closeRun(t, h, id) })

	// Nothing is answered from here on: no pong, no frame of any kind.
	done := make(chan uint16, 1)
	go func() {
		for {
			op, payload := c.read()
			if op == opClose {
				done <- closeCode(payload)
				return
			}
		}
	}()
	select {
	case got := <-done:
		if got != wsGoing {
			t.Fatalf("a silent peer is let go with 1001, got %d", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a peer that answers no heartbeat must not hold the socket forever")
	}

	for deadline := time.Now().Add(5 * time.Second); ; {
		code, doc := post(t, h, "GET", "/v1/runs/"+id, nil)
		if code == http.StatusOK && doc["attached"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run must be left with nobody reading it: %d %v", code, doc)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// closeRun closes a run and waits until it has ended. Closing is a request,
// not an event: the CLI exits a moment later, and a test that returns before
// it has leaves a process holding the temporary directory the test is about
// to remove — which Windows refuses, failing a test that had passed.
func closeRun(t *testing.T, h *harness, id string) {
	t.Helper()
	h.do("POST", "/v1/runs/"+id+"/close", nil)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		code, doc := post(t, h, "GET", "/v1/runs/"+id, nil)
		if code != http.StatusOK || doc["state"] == "ended" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

/* ------------------------------------------------------- roles on a socket --- */

// TestAWatcherOnASocketSeesEverythingAndSaysNothing: a watcher may attach to
// a run and is sent every event, and each of the four frames that would
// change it is refused by name — with the socket still open afterwards,
// because a read-only page must not be able to hang up on itself.
func TestAWatcherOnASocketSeesEverythingAndSaysNothing(t *testing.T) {
	watchTok := "watch-token-aaaa"
	sum := sha256.Sum256([]byte(watchTok))
	h := newHarness(t, Options{Tokens: []TokenPrincipal{
		{Name: "ci-watch", Role: RoleWatch, SHA256: hex.EncodeToString(sum[:])},
	}})
	echoClaude(t)
	s, id := startInputRun(t, h, "one")
	defer s.cancel()
	t.Cleanup(func() { closeRun(t, h, id) })

	c, resp := dialWS(t, h, "/v1/runs/"+id+"/ws?since=0", "Sec-WebSocket-Protocol", "rota, bearer."+watchTok)
	if c == nil {
		t.Fatalf("a watcher must be able to attach: %d", resp.StatusCode)
	}
	// Reading works: the run's events reach it like any other reader.
	c.wait("the run's own events", func(doc map[string]any) bool {
		text, _ := doc["text"].(string)
		return strings.Contains(text, "echo: one")
	})

	for i, frame := range []map[string]any{
		{"type": "message", "text": "two", "ref": "w1"},
		{"type": "interrupt", "ref": "w2"},
		{"type": "raw", "line": map[string]any{"type": "x"}, "ref": "w3"},
		{"type": "close", "ref": "w4"},
	} {
		ref := fmt.Sprintf("w%d", i+1)
		c.say(frame)
		ack := c.ack(ref)
		if ack["error"] != watchOnly {
			t.Fatalf("%v was answered %v, want a refusal saying the role may only watch", frame, ack)
		}
		if ack["state"] == "accepted" {
			t.Fatalf("%v was accepted: %v", frame, ack)
		}
	}
	// The socket is still there, and still streaming: the run is asked for
	// another turn through the controlling side and the watcher sees it.
	if code, doc := post(t, h, "POST", "/v1/runs/"+id+"/messages", map[string]any{"text": "three"}); code != http.StatusAccepted {
		t.Fatalf("the controller's message: %d %v", code, doc)
	}
	c.wait("what the controller asked for", func(doc map[string]any) bool {
		text, _ := doc["text"].(string)
		return strings.Contains(text, "echo: three")
	})
}

// TestAWatcherMayNotStartARunOverASocket: refused before the upgrade, so
// there is no connection to take over and the answer is ordinary HTTP.
func TestAWatcherMayNotStartARunOverASocket(t *testing.T) {
	watchTok := "watch-token-aaaa"
	sum := sha256.Sum256([]byte(watchTok))
	h := newHarness(t, Options{Tokens: []TokenPrincipal{
		{Name: "ci-watch", Role: RoleWatch, SHA256: hex.EncodeToString(sum[:])},
	}})
	for _, path := range []string{"/v1/ws", "/v1/accounts/1/ws"} {
		c, resp := dialWS(t, h, path, "Sec-WebSocket-Protocol", "rota, bearer."+watchTok)
		if c != nil {
			t.Fatalf("GET %s as a watcher was upgraded", path)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s as a watcher: %d", path, resp.StatusCode)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(raw), watchOnly) {
			t.Fatalf("GET %s must say why: %s", path, raw)
		}
	}
}

// TestASocketOnACookieNeedsItsOwnOrigin: any page on the internet may open a
// WebSocket to this server and the browser will attach the cookie, so an
// upgrade authorized by one is held to the same rule as a write.
func TestASocketOnACookieNeedsItsOwnOrigin(t *testing.T) {
	h := newHarness(t, Options{
		Users:      []User{{Name: "driver", Role: RoleControl, Password: cheapHash()}},
		SessionTTL: time.Hour,
	})
	echoClaude(t)
	s, id := startInputRun(t, h, "one")
	defer s.cancel()
	t.Cleanup(func() { closeRun(t, h, id) })

	// Sign in the way the page does, and keep the cookie.
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/session",
		strings.NewReader(`{"name":"driver","password":"`+testPlain+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", h.srv.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signing in: %d", resp.StatusCode)
	}
	var cookie string
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie {
			cookie = ck.Name + "=" + ck.Value
		}
	}
	if cookie == "" {
		t.Fatal("no cookie was set")
	}

	// From somebody else's page: refused, without an upgrade.
	c, refused := dialWS(t, h, "/v1/runs/"+id+"/ws", "Cookie", cookie, "Origin", "https://evil.example")
	if c != nil {
		t.Fatal("a cross-site socket on a cookie was upgraded")
	}
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross-site socket: %d", refused.StatusCode)
	}
	// With no Origin at all, likewise: a browser always sends one.
	if c, refused := dialWS(t, h, "/v1/runs/"+id+"/ws", "Cookie", cookie); c != nil {
		t.Fatalf("a socket on a cookie with no Origin was upgraded (%d)", refused.StatusCode)
	}
	// From this server's own page, it works.
	c, resp2 := dialWS(t, h, "/v1/runs/"+id+"/ws", "Cookie", cookie, "Origin", h.srv.URL)
	if c == nil {
		t.Fatalf("the page's own socket was refused: %d", resp2.StatusCode)
	}
	c.wait("the run's own events", func(doc map[string]any) bool {
		text, _ := doc["text"].(string)
		return strings.Contains(text, "echo: one")
	})
}
