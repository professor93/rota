package api

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	rota "github.com/professor93/rota/lib"
)

// A WebSocket is the same open run as the HTTP endpoints, over one connection
// that carries both directions: every event out as a text frame, and messages,
// interrupts and the close in. Nothing here is a second kind of run — the
// endpoints and the socket reach the same liveRun, and a client may use either.
//
// RFC 6455 is implemented here rather than imported. rota depends on the
// standard library alone, and what a server needs of that protocol is a
// handshake, masked frames one way, unmasked the other, and a close. The parts
// that are not needed — extensions, compression, client-side masking — are
// absent rather than stubbed.

const (
	// wsGUID is the constant RFC 6455 mixes into the client's key. The answer
	// proves the server understood the handshake rather than echoed it.
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	// wsProto is the subprotocol this server speaks, and wsBearer the one a
	// browser smuggles the token in: a WebSocket cannot carry a header.
	wsProto  = "rota"
	wsBearer = "bearer."

	// maxWSMessage bounds one message, reassembled fragments included. A
	// message into a run is capped far lower than this by the library; what
	// this stops is a peer filling memory a frame at a time.
	maxWSMessage = 1 << 20
	// wsWriteWait bounds one write. A reader that has stopped reading must not
	// hold up the run whose events it is being sent.
	wsWriteWait = 10 * time.Second
	// wsCloseWait is how long the peer's own close frame is waited for before
	// the connection is dropped anyway.
	wsCloseWait = 500 * time.Millisecond
	// wsFirstWait is how long a socket that has not started a run yet may stay
	// silent. It is generous: the frame is a request somebody may be typing.
	wsFirstWait = 30 * time.Second
)

// The opcodes, and the close codes this server sends.
const (
	opContinue = 0x0
	opText     = 0x1
	opBinary   = 0x2
	opClose    = 0x8
	opPing     = 0x9
	opPong     = 0xA

	wsNormal   = 1000 // the run ended, or the client said goodbye
	wsGoing    = 1001 // the server is done with this socket: a silent peer, a shutdown
	wsProtocol = 1002 // the framing itself was wrong
	wsBadData  = 1007 // a text frame that is not UTF-8
	wsPolicy   = 1008 // a frame this server will not act on
	wsTooBig   = 1009 // a message past the cap
	wsInternal = 1011 // rota broke
)

/* --------------------------------------------------------- the handshake --- */

// wsAuth is auth and authorization for a socket, and it runs before the
// upgrade: a wrong credential is an ordinary 401 and a wrong role an
// ordinary 403, with no connection taken over.
//
// A browser cannot put a header on a WebSocket, so a credential may travel
// three ways here rather than one: the Authorization header a program sends,
// a subprotocol — Sec-WebSocket-Protocol: rota, bearer.<token> — and, for a
// page whose person has signed in, the session cookie the browser attaches
// on its own. The reply names only rota: the other entry is a credential,
// and a server that repeated it would write it into the client's own logs.
//
// A token may not travel in the query string: a URL is written to every
// access log on the way, and a token in one is a token given away.
//
// A cookie-authenticated upgrade is held to the same Origin rule as a
// cookie-authenticated write, because an upgrade is one: any page on the
// internet may open a WebSocket to this server, and the browser would attach
// the cookie to it.
func (s *Server) wsAuth(pattern string, next http.HandlerFunc) http.HandlerFunc {
	need := needs(pattern)
	return func(w http.ResponseWriter, r *http.Request) {
		p, guessed := s.principalWS(r)
		if p == nil {
			s.refuseToken(w, r, guessed)
			return
		}
		if p.Via != "token" && !sameOrigin(r) {
			s.log.Warn("refused a cross-site socket", "name", p.Name, "role", p.Role,
				"path", r.URL.Path, "ip", clientIP(r))
			fail(w, http.StatusForbidden, crossSiteNo)
			return
		}
		if !p.Role.allows(need) {
			s.refuseRole(w, r, p, need)
			return
		}
		next(w, withPrincipal(r, p))
	}
}

// principalWS is resolve with the subprotocol as one more place a bearer
// token may be.
func (s *Server) principalWS(r *http.Request) (*Principal, bool) {
	for _, tok := range []string{bearerToken(r.Header.Get("Authorization")), protoToken(r)} {
		if tok == "" {
			continue
		}
		if p := s.byToken(tok); p != nil {
			return p, false
		}
		return nil, true
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if p := s.sessions.get(c.Value); p != nil {
			return p, false
		}
	}
	return nil, false
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return header[len(prefix):]
}

// protoToken reads the token out of the offered subprotocols.
func protoToken(r *http.Request) string {
	for _, p := range protocols(r) {
		if strings.HasPrefix(p, wsBearer) {
			return p[len(wsBearer):]
		}
	}
	return ""
}

func protocols(r *http.Request) []string {
	var out []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for part := range strings.SplitSeq(header, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// upgradeWS takes the connection over and answers the handshake, or says in
// HTTP why it would not. The version is the one refusal with an answer of its
// own: a client speaking another one can read 426 and know what to do.
func upgradeWS(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !headerHas(r, "Connection", "upgrade") || !headerHas(r, "Upgrade", "websocket") {
		fail(w, http.StatusBadRequest, "this endpoint takes a WebSocket: Connection: Upgrade and Upgrade: websocket")
		return nil, errors.New("not an upgrade")
	}
	if v := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")); v != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		fail(w, http.StatusUpgradeRequired, "this server speaks WebSocket version 13")
		return nil, errors.New("wrong version")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		fail(w, http.StatusBadRequest, "Sec-WebSocket-Key is missing")
		return nil, errors.New("no key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		fail(w, http.StatusBadRequest, "this connection cannot become a WebSocket")
		return nil, errors.New("not hijackable")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		fail(w, http.StatusInternalServerError, "this connection cannot become a WebSocket")
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	b.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n")
	// Only rota is echoed. The other entry is a credential, and a server that
	// repeated it would write it into the client's own logs.
	for _, p := range protocols(r) {
		if p == wsProto {
			b.WriteString("Sec-WebSocket-Protocol: " + wsProto + "\r\n")
			break
		}
	}
	b.WriteString("\r\n")
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if _, err := brw.WriteString(b.String()); err != nil {
		conn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	// The heartbeat is read once: it is a variable a test moves, and every
	// deadline on this connection should be measured against the same one.
	return &wsConn{conn: conn, r: brw.Reader, hb: heartbeat, done: make(chan struct{})}, nil
}

// headerHas reports whether a comma-separated header names this token.
func headerHas(r *http.Request, name, want string) bool {
	for _, header := range r.Header.Values(name) {
		for part := range strings.SplitSeq(header, ",") {
			if strings.EqualFold(strings.TrimSpace(part), want) {
				return true
			}
		}
	}
	return false
}

/* ------------------------------------------------------ the connection --- */

// wsConn is one WebSocket: the connection, the reader the handshake left
// behind, and the mutex every frame this server writes goes through — a run
// writes its events from one goroutine while acks are written from another,
// and two frames interleaved are not frames at all.
type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
	hb   time.Duration

	// held is a message being reassembled, and holding whether there is one.
	// Both belong to the reading goroutine alone.
	held    []byte
	holding bool
	// late says the peer stopped answering, so the close can say so.
	late atomic.Bool

	// done closes with the connection, so the reading goroutine is never left
	// holding a frame nobody will take.
	done chan struct{}

	mu   sync.Mutex
	out  []byte // the write buffer, reused: one frame is one write
	sent bool   // a close frame has gone out; the first one is the one that counts
	shut bool
}

// wsFail is a frame this server will not read, and the close code that says
// why. It is an error because both travel the same way out.
type wsFail struct {
	code uint16
	why  string
}

func (e wsFail) Error() string { return e.why }

// errPeerClose is the peer saying goodbye, which is not a failure.
var errPeerClose = errors.New("the peer closed the socket")

// send writes one frame, unmasked, as a server's frames are.
func (c *wsConn) send(op byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shut {
		return net.ErrClosed
	}
	c.out = c.out[:0]
	c.out = append(c.out, 0x80|op)
	switch n := len(payload); {
	case n < 126:
		c.out = append(c.out, byte(n))
	case n < 1<<16:
		c.out = append(c.out, 126, byte(n>>8), byte(n))
	default:
		c.out = append(c.out, 127)
		c.out = binary.BigEndian.AppendUint64(c.out, uint64(n))
	}
	c.out = append(c.out, payload...)
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	_, err := c.conn.Write(c.out)
	return err
}

// say writes one document as a text frame: one frame is one JSON object, with
// no newline after it, because a frame already has an end.
func (c *wsConn) say(v any) error {
	raw, err := rota.Encode(v)
	if err != nil {
		return err
	}
	return c.send(opText, raw)
}

// sendClose says goodbye once. A second close frame would be noise: the first
// one already named the reason.
func (c *wsConn) sendClose(code uint16, why string) {
	c.mu.Lock()
	already := c.sent
	c.sent = true
	c.mu.Unlock()
	if already {
		return
	}
	body := make([]byte, 2, 2+len(why))
	binary.BigEndian.PutUint16(body, code)
	// A reason is for a person reading a log, so it is bounded rather than
	// trusted: a close frame carries 125 bytes at most, code included.
	if len(why) > 123 {
		why = why[:123]
	}
	_ = c.send(opClose, append(body, why...))
}

// bye ends the connection: the close frame, then the peer's own close or a
// short wait, then the socket. Dropping the connection before the peer has
// read the close would turn a clean goodbye into an error at the other end.
//
// reads is the channel the reading goroutine is filling, when there is one: it
// is drained until it closes, so that goroutine is never left holding a frame
// nobody will take.
func (c *wsConn) bye(code uint16, why string, reads <-chan []byte) {
	c.sendClose(code, why)
	if reads == nil {
		// Nobody else is reading this socket, so the peer's close is read
		// here, and only for as long as the wait allows: a peer that goes on
		// talking instead must not keep this goroutine.
		until := time.Now().Add(wsCloseWait)
		for time.Now().Before(until) {
			if _, _, _, err := c.frame(wsCloseWait); err != nil {
				break
			}
		}
		c.close()
		return
	}
	deadline := time.After(wsCloseWait)
	for {
		select {
		case _, open := <-reads:
			if !open {
				c.close()
				return
			}
		case <-deadline:
			c.close()
			return
		}
	}
}

func (c *wsConn) close() {
	c.mu.Lock()
	already := c.shut
	c.shut = true
	c.mu.Unlock()
	if !already {
		close(c.done)
	}
	c.conn.Close()
}

// frame reads one frame and unmasks it. Every frame a client sends is masked;
// one that is not is not a client's.
func (c *wsConn) frame(wait time.Duration) (op byte, payload []byte, fin bool, err error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(wait))
	var head [2]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		return 0, nil, false, err
	}
	fin = head[0]&0x80 != 0
	if head[0]&0x70 != 0 {
		return 0, nil, false, wsFail{wsProtocol, "this server negotiated no extension, so the reserved bits must be zero"}
	}
	op = head[0] & 0x0f
	masked := head[1]&0x80 != 0
	size := uint64(head[1] & 0x7f)
	switch size {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return 0, nil, false, err
		}
		size = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return 0, nil, false, err
		}
		size = binary.BigEndian.Uint64(b[:])
	}
	if op >= opClose && (!fin || size > 125) {
		return 0, nil, false, wsFail{wsProtocol, "a control frame is short and never fragmented"}
	}
	if !masked {
		return 0, nil, false, wsFail{wsProtocol, "every frame a client sends is masked"}
	}
	if size > maxWSMessage {
		return 0, nil, false, wsFail{wsTooBig, "one message is capped at 1 MB"}
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.r, mask[:]); err != nil {
		return 0, nil, false, err
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return 0, nil, false, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return op, payload, fin, nil
}

// message reads frames until it has a whole message. A ping is answered where
// it arrives, carrying the same payload back, because a peer checking that the
// connection works should not have to wait for a message to be finished first.
func (c *wsConn) message(wait time.Duration) ([]byte, error) {
	for {
		op, payload, fin, err := c.frame(wait)
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.send(opPong, payload); err != nil {
				return nil, err
			}
		case opPong: // the peer is alive, which is all a pong says
		case opClose:
			return nil, errPeerClose
		case opText, opBinary:
			// Binary is treated as text: what this server reads is JSON, and a
			// client that framed it as bytes meant the same thing.
			if c.holding {
				return nil, wsFail{wsProtocol, "a message started inside another"}
			}
			if fin {
				return whole(payload)
			}
			c.held, c.holding = payload, true
		case opContinue:
			if !c.holding {
				return nil, wsFail{wsProtocol, "a continuation with nothing to continue"}
			}
			if len(c.held)+len(payload) > maxWSMessage {
				return nil, wsFail{wsTooBig, "one message is capped at 1 MB"}
			}
			c.held = append(c.held, payload...)
			if fin {
				msg := c.held
				c.held, c.holding = nil, false
				return whole(msg)
			}
		default:
			return nil, wsFail{wsProtocol, "unknown opcode"}
		}
	}
}

// whole is a finished message, which must be text: everything this server
// reads is JSON, and JSON that is not UTF-8 is not JSON.
func whole(payload []byte) ([]byte, error) {
	if !utf8.Valid(payload) {
		return nil, wsFail{wsBadData, "a text frame must be valid UTF-8"}
	}
	return payload, nil
}

// readInto is the reading goroutine: whole messages down the channel, and a
// closed channel when there are no more. A peer that stops answering is a peer
// that has gone, so the read deadline is three heartbeats — by then three
// pings have gone unanswered.
func (c *wsConn) readInto(out chan<- []byte) {
	defer close(out)
	for {
		msg, err := c.message(3 * c.hb)
		if err == nil {
			select {
			case out <- msg:
				continue
			case <-c.done:
				return
			}
		}
		var bad wsFail
		switch {
		case errors.As(err, &bad):
			c.sendClose(bad.code, bad.why)
		case errors.Is(err, os.ErrDeadlineExceeded):
			c.late.Store(true)
		}
		return
	}
}

/* --------------------------------------------------------- the endpoints --- */

// attachWS is a socket onto a run that is already going: the same events the
// stream carries, replayed from since and then live, one frame each.
func (s *Server) attachWS(w http.ResponseWriter, r *http.Request) {
	lr, ok := s.runOf(w, r)
	if !ok {
		return
	}
	since, ok := sinceOf(w, r)
	if !ok {
		return
	}
	c, err := upgradeWS(w, r)
	if err != nil {
		return
	}
	s.serveWS(c, lr, since, who(r).Role)
}

// startWS starts a run over a socket. The first frame is the request — the
// same body POST /v1/run takes — and everything after it is the conversation.
func (s *Server) startWS(w http.ResponseWriter, r *http.Request) {
	c, err := upgradeWS(w, r)
	if err != nil {
		return
	}
	first, err := c.message(wsFirstWait)
	if err != nil {
		var bad wsFail
		if errors.As(err, &bad) {
			c.bye(bad.code, bad.why, nil)
			return
		}
		c.bye(wsGoing, "nothing was asked for", nil)
		return
	}
	req, err := startFrame(first)
	if err != nil {
		s.refuseWS(c, r, err)
		return
	}
	// A socket is a conversation by construction: the run stays open and it
	// streams, whatever the frame asked for.
	req.Input, req.Stream = true, true
	var hold held
	defer hold.release()
	p, err := s.prepare(r, req, &hold)
	if err != nil {
		if errors.Is(err, errGone) {
			c.bye(wsGoing, "the server is stopping", nil)
			return
		}
		s.refuseWS(c, r, err)
		return
	}
	defer p.st.Close()
	lr, err := s.beginInput(p, &hold)
	if err != nil {
		s.refuseWS(c, r, err)
		return
	}
	// Only control reaches this route at all, so the run is served as what
	// started it.
	s.serveWS(c, lr, 0, RoleControl)
}

// startFrame reads the opening frame as a run request. Unknown fields are
// refused exactly as they are over HTTP: a misspelled option must not be
// quietly ignored on a request that costs money.
func startFrame(raw []byte) (*request, error) {
	if !isObject(raw) {
		return nil, wsFail{wsPolicy, "every frame is one JSON object"}
	}
	var frame struct {
		request
		Type string `json:"type"`
	}
	if err := jsonv2.Unmarshal(raw, &frame, rota.LenientOptions(), jsonv2.RejectUnknownMembers(true)); err != nil {
		return nil, rota.Invalid("bad start frame: %v", err)
	}
	if frame.Type != "start" {
		return nil, rota.Invalid(`the first frame on a socket must be {"type":"start", …}, with the fields POST /v1/run takes`)
	}
	req := frame.request
	return &req, nil
}

// refuseWS says why nothing will run, and ends the socket. It is the frame
// shape of what report writes over HTTP, down to hiding an internal failure
// that would otherwise describe this server's insides to whoever asked.
func (s *Server) refuseWS(c *wsConn, r *http.Request, err error) {
	msg, code := err.Error(), uint16(wsPolicy)
	var no *refusal
	if !errors.As(err, &no) && statusFor(err) >= http.StatusInternalServerError {
		// Not the client's fault, and not the client's business either.
		s.log.Error("socket run refused", "path", r.URL.Path, "err", err)
		msg, code = "internal error", wsInternal
	}
	_ = c.say(map[string]any{"type": "error", "error": msg})
	c.bye(code, "", nil)
}

// serveWS carries the run both ways until one end of it stops.
//
// role is what the socket's own principal may do. A watcher gets every event
// and none of the acts: what it sends back is answered with a refusal and
// the connection carries on, because a page that is read-only by design must
// not be able to hang up on itself by mistake.
func (s *Server) serveWS(c *wsConn, lr *liveRun, since int, role Role) {
	rd := newWSReader(c)
	lr.attach(rd, since)
	reads := make(chan []byte, 8)
	go c.readInto(reads)
	t := time.NewTicker(c.hb)
	defer t.Stop()
	for {
		select {
		case raw, open := <-reads:
			if !open {
				// The client went away, or said something the protocol does
				// not allow. Either way the run keeps its grace, as it does
				// for any reader that drops.
				lr.detach(rd)
				code := uint16(wsNormal)
				if c.late.Load() {
					code = wsGoing
				}
				c.bye(code, "", reads)
				return
			}
			if err := s.onWS(c, lr, raw, role); err != nil {
				lr.detach(rd)
				c.bye(wsPolicy, err.Error(), reads)
				return
			}
		case <-rd.gone:
			// The run ended — its last event has already gone out — or another
			// reader took this one's place.
			c.bye(wsNormal, "", reads)
			return
		case <-s.ctx.Done():
			// The server is stopping, and every run with it. Saying so is
			// better than a connection that simply stops answering.
			lr.detach(rd)
			c.bye(wsGoing, "the server is stopping", reads)
			return
		case <-t.C:
			lr.beat(rd)
		}
	}
}

/* ------------------------------------------------------- what a client says --- */

// wsFrame is one thing a client says to a run it is already attached to.
type wsFrame struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Steer bool           `json:"steer"`
	Line  jsontext.Value `json:"line"`
	// Ref is the client's own name for this frame, echoed in the ack so a
	// client that numbers what it sends can match the answers up. It is
	// optional: the run's own input, interrupted and idle events say what
	// became of everything anyway.
	Ref string `json:"ref"`
}

// wsAck is what became of one frame. An accepted message carries the id its
// events will be under; a refusal carries why instead.
type wsAck struct {
	Type  string `json:"type"`
	Ref   string `json:"ref,omitempty"`
	ID    string `json:"id,omitempty"`
	State string `json:"state,omitempty"`
	Error string `json:"error,omitempty"`
}

// onWS acts on one frame and answers it. Returning an error ends the socket:
// that is for a frame this server cannot read at all. Everything it can read
// and will not do is an ack with a reason, because the connection is fine.
func (s *Server) onWS(c *wsConn, lr *liveRun, raw []byte, role Role) error {
	if !isObject(raw) {
		return wsFail{wsPolicy, "every frame is one JSON object"}
	}
	var f wsFrame
	if err := rota.UnmarshalLenient(raw, &f); err != nil {
		return wsFail{wsPolicy, "a frame this server cannot read"}
	}
	// A write that fails is a peer that has gone, which the reading side will
	// find on its own: this one has nothing left to say about it.
	id, err := s.actWS(lr, f, role)
	if err != nil {
		_ = c.say(wsAck{Type: "ack", Ref: f.Ref, Error: s.wsErr(lr, err)})
		return nil
	}
	_ = c.say(wsAck{Type: "ack", Ref: f.Ref, ID: id, State: "accepted"})
	return nil
}

// actWS does what one frame asked for and returns the id its answer will
// carry, when the act has one. A close and a raw line have none: the CLI is
// told something, and what it makes of it is in the stream like everything
// else.
func (s *Server) actWS(lr *liveRun, f wsFrame, role Role) (string, error) {
	// Everything a socket carries inward changes the run, so a watcher is
	// told so once, whatever the frame was going to ask for.
	if !role.allows(RoleControl) {
		return "", refuse(http.StatusForbidden, watchOnly)
	}
	if lr.over() {
		return "", refuse(http.StatusConflict, "run "+lr.id+" has ended")
	}
	switch f.Type {
	case "message":
		send := lr.sess.Send
		if f.Steer {
			// Interrupting first, so the message starts the next turn instead
			// of joining the one being abandoned.
			send = lr.sess.Steer
		}
		return send(f.Text)
	case "interrupt":
		return lr.sess.Interrupt()
	case "close":
		if err := lr.sess.Close(); err != nil && !errors.Is(err, rota.ErrClosed) {
			return "", err
		}
		return "", nil
	case "raw":
		return "", lr.sess.SendRaw(f.Line)
	case "start":
		return "", refuse(http.StatusConflict, "this socket is already running "+lr.id)
	}
	return "", refuse(http.StatusBadRequest, "unknown frame type "+strconv.Quote(f.Type)+
		"; a socket takes message, interrupt, close and raw")
}

// wsErr is a refusal in words a client may be shown. A session that has closed
// under it is the same fact as a run that has ended, said by the library.
func (s *Server) wsErr(lr *liveRun, err error) string {
	if errors.Is(err, rota.ErrClosed) {
		return "run " + lr.id + " has ended"
	}
	if statusFor(err) >= http.StatusInternalServerError {
		s.log.Error("a socket frame failed", "run", lr.id, "err", err)
		return "internal error"
	}
	return err.Error()
}

func isObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}
