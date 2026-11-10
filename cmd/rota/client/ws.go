package client

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// The client's end of a WebSocket, as a terminal is attached over one.
// RFC 6455 is written here rather than imported, as the server's end is in
// api/ws.go: a handshake, masked frames out, unmasked frames in, ping, pong
// and close. Extensions, compression and subprotocols are absent rather
// than stubbed.
//
// Two of the RFC's SHOULDs are left out on purpose, and are harmless over
// a unix socket or a pipe: a server that breaks the protocol is dropped
// without a close frame naming why, and our own close lets the connection
// go once it is written rather than waiting for the server's.

// maxFrame bounds one message from the server, its fragments reassembled
// included: the server caps what it is sent at the same.
const maxFrame = 1 << 20

const (
	// wsGUID is the constant RFC 6455 mixes into the key, so the answer
	// proves the server read the handshake rather than echoed it.
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	wsOpContinue = 0x0
	wsOpText     = 0x1
	wsOpBinary   = 0x2
	wsOpClose    = 0x8
	wsOpPing     = 0x9
	wsOpPong     = 0xA

	wsNormal   = 1000 // the client is done
	wsNoStatus = 1005 // a close frame that named no code

	// wsMaxHeader bounds the handshake's answer, status line and headers:
	// past it the answer is not the daemon's.
	wsMaxHeader = 64 << 10
	// wsWriteWait bounds one write: a server that has stopped reading must
	// not hold a typist forever.
	wsWriteWait = 10 * time.Second
	// wsCloseWait is how long a goodbye waits for its turn to be written
	// and for the writing, before the connection is dropped without it.
	wsCloseWait = time.Second
	// wsQuiet is how long the server may say nothing at all before it is
	// taken for gone: it pings every 15 seconds, so four have gone missing.
	wsQuiet = time.Minute
)

// wsConn is one WebSocket, from the client's side. Reading is one
// goroutine's; writing is anybody's, a message at a time.
type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
	// quiet is how long one frame is waited for once attached: wsQuiet,
	// unless a test made it shorter before reading.
	quiet time.Duration

	// turn is the writer's turn, a channel rather than a mutex so that a
	// goodbye can give up waiting on a write that is stuck.
	turn chan struct{}
	// shut says the connection is being let go of: nothing more is written.
	shut atomic.Bool

	// held is a message being reassembled, holding whether there is one,
	// and heldOp the kind it started as. The reader's alone.
	held    []byte
	holding bool
	heldOp  byte
}

// wsProtocolError is a frame the server should never have sent.
type wsProtocolError struct{ why string }

func (e wsProtocolError) Error() string {
	return "the rota daemon broke the WebSocket protocol: " + e.why
}

// wsQuietError is a server that said nothing, not even a ping, for as long
// as one is waited for.
type wsQuietError struct{ err error }

func (e wsQuietError) Error() string { return "the rota daemon has said nothing for a minute" }
func (e wsQuietError) Unwrap() error { return e.err }

// wsHandshake asks for the upgrade on conn and reads the answer. A 101 that
// proves itself is the socket, read from what the answer left buffered;
// any other answer is the response, with its body read, for a refusal.
func wsHandshake(conn net.Conn, path string, deadline time.Time) (*wsConn, *http.Response, []byte, error) {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	key := base64.StdEncoding.EncodeToString(nonce[:])
	req, err := http.NewRequest(http.MethodGet, "http://rota"+path, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	_ = conn.SetDeadline(deadline)
	if err := req.Write(conn); err != nil {
		return nil, nil, nil, err
	}
	// The answer's head is read through a limit, lifted once it is read:
	// the body has maxBody, and frames have maxFrame.
	lr := &io.LimitedReader{R: conn, N: wsMaxHeader}
	br := bufio.NewReader(lr)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		if lr.N == 0 {
			return nil, nil, nil, wsProtocolError{fmt.Sprintf("its answer's head ran past %d KiB", wsMaxHeader>>10)}
		}
		return nil, nil, nil, err
	}
	lr.N = math.MaxInt64
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		return nil, resp, raw, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	switch {
	case !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket"):
		return nil, nil, nil, wsProtocolError{"its 101 did not upgrade to websocket"}
	case !wsHeaderHas(resp.Header, "Connection", "upgrade"):
		return nil, nil, nil, wsProtocolError{"its 101 did not say Connection: Upgrade"}
	case resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]):
		return nil, nil, nil, wsProtocolError{"its 101 answered another key"}
	case len(resp.Header.Values("Sec-WebSocket-Extensions")) > 0:
		return nil, nil, nil, wsProtocolError{"its 101 named an extension nobody asked for"}
	}
	return &wsConn{conn: conn, r: br, quiet: wsQuiet, turn: make(chan struct{}, 1)}, nil, nil, nil
}

// wsHeaderHas reports whether a comma-separated header names this token.
func wsHeaderHas(h http.Header, name, want string) bool {
	for _, v := range h.Values(name) {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), want) {
				return true
			}
		}
	}
	return false
}

// send writes one frame, masked with a key of its own, as every frame a
// client sends must be.
func (w *wsConn) send(op byte, payload []byte) error {
	w.turn <- struct{}{}
	defer func() { <-w.turn }()
	if w.shut.Load() {
		return net.ErrClosed
	}
	return w.write(op, payload, wsWriteWait)
}

// sendAll writes payload as frames of op of at most chunk bytes each, all
// in one turn, so that nothing anybody else sends lands between them. The
// last frame is never a single byte: the server reads a lone Esc or
// Ctrl-C in a frame of its own as a key pressed on its own.
func (w *wsConn) sendAll(op byte, payload []byte, chunk int) error {
	w.turn <- struct{}{}
	defer func() { <-w.turn }()
	for len(payload) > 0 {
		n := min(len(payload), chunk)
		if len(payload)-n == 1 {
			n--
		}
		if w.shut.Load() {
			return net.ErrClosed
		}
		if err := w.write(op, payload[:n], wsWriteWait); err != nil {
			return err
		}
		payload = payload[n:]
	}
	return nil
}

// say writes one document as a text frame.
func (w *wsConn) say(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.send(wsOpText, raw)
}

// write is one frame, in one write. The caller has the turn.
func (w *wsConn) write(op byte, payload []byte, wait time.Duration) error {
	n := len(payload)
	frame := make([]byte, 0, 14+n)
	frame = append(frame, 0x80|op)
	switch {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n < 1<<16:
		frame = append(frame, 0x80|126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(wait))
	_, err := w.conn.Write(frame)
	return err
}

// goodbye says close with body, once, and lets the connection go. A write
// that holds the turn is waited for only so long: the connection is closed
// under it, which ends it.
func (w *wsConn) goodbye(body []byte) {
	if w.shut.Swap(true) {
		return
	}
	select {
	case w.turn <- struct{}{}:
		_ = w.write(wsOpClose, body, wsCloseWait)
		<-w.turn
	case <-time.After(wsCloseWait):
	}
	w.conn.Close()
}

// drop lets the connection go without a word, for a server that broke the
// protocol: nothing it says next can be trusted to be read as meant.
func (w *wsConn) drop() {
	w.shut.Store(true)
	w.conn.Close()
}

// wsCloseBody is a close frame's body for code.
func wsCloseBody(code uint16) []byte { return binary.BigEndian.AppendUint16(nil, code) }

// wsSendable reports whether a close code may be sent: RFC 6455 7.4 keeps
// 1004, 1005, 1006 and 1015 for saying what happened, never for a frame,
// and leaves everything under 1000 unused.
func wsSendable(code int) bool {
	switch {
	case code >= 1000 && code <= 1003, code >= 1007 && code <= 1014, code >= 3000 && code <= 4999:
		return true
	}
	return false
}

// frame reads one frame, waiting at most wait for it. The server's frames
// are never masked, and a frame is judged by its head before its payload
// is allocated.
func (w *wsConn) frame(wait time.Duration) (op byte, payload []byte, fin bool, err error) {
	_ = w.conn.SetReadDeadline(time.Now().Add(wait))
	var head [2]byte
	if _, err := io.ReadFull(w.r, head[:]); err != nil {
		return 0, nil, false, err
	}
	fin, op = head[0]&0x80 != 0, head[0]&0x0f
	switch {
	case head[0]&0x70 != 0:
		return 0, nil, false, wsProtocolError{"a reserved bit was set, with no extension agreed"}
	case head[1]&0x80 != 0:
		return 0, nil, false, wsProtocolError{"a frame from the server was masked"}
	case op > wsOpBinary && op < wsOpClose, op > wsOpPong:
		return 0, nil, false, wsProtocolError{fmt.Sprintf("an opcode %#x that means nothing", op)}
	}
	size := uint64(head[1] & 0x7f)
	switch size {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(w.r, b[:]); err != nil {
			return 0, nil, false, err
		}
		size = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(w.r, b[:]); err != nil {
			return 0, nil, false, err
		}
		size = binary.BigEndian.Uint64(b[:])
	}
	if op >= wsOpClose && (!fin || size > 125) {
		return 0, nil, false, wsProtocolError{"a control frame was long or fragmented"}
	}
	if size > maxFrame {
		return 0, nil, false, wsProtocolError{fmt.Sprintf("a frame of %d bytes, past the %d a message may be", size, maxFrame)}
	}
	// A continuation counts with what it continues, before it is read.
	if op == wsOpContinue && uint64(len(w.held))+size > maxFrame {
		return 0, nil, false, wsProtocolError{fmt.Sprintf("a message past the %d bytes one may be", maxFrame)}
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(w.r, payload); err != nil {
		return 0, nil, false, err
	}
	return op, payload, fin, nil
}

// message reads one whole message, text or binary, its fragments joined,
// waiting at most wait for each frame. A ping is answered on the way and a
// pong passed over. The server's close is answered and is a *Closed; an
// end with no close is io.ErrUnexpectedEOF, after our own close it is
// net.ErrClosed, and a server silent for wait is a wsQuietError. Any error
// lets the connection go.
func (w *wsConn) message(wait time.Duration) (op byte, payload []byte, err error) {
	op, payload, err = w.next(wait)
	if err != nil {
		var closed *Closed
		switch {
		case errors.As(err, &closed):
		case w.shut.Load():
			err = net.ErrClosed
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			err = io.ErrUnexpectedEOF
		case errors.Is(err, os.ErrDeadlineExceeded):
			err = wsQuietError{err}
		}
		w.drop()
	}
	return op, payload, err
}

func (w *wsConn) next(wait time.Duration) (byte, []byte, error) {
	for {
		op, payload, fin, err := w.frame(wait)
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case wsOpPing:
			if err := w.send(wsOpPong, payload); err != nil {
				return 0, nil, err
			}
		case wsOpPong:
		case wsOpClose:
			return 0, nil, w.closed(payload)
		case wsOpText, wsOpBinary:
			if w.holding {
				return 0, nil, wsProtocolError{"a message began inside another"}
			}
			if fin {
				return op, payload, nil
			}
			w.held, w.holding, w.heldOp = payload, true, op
		case wsOpContinue:
			if !w.holding {
				return 0, nil, wsProtocolError{"a continuation with no message to continue"}
			}
			w.held = append(w.held, payload...)
			if fin {
				whole := w.held
				w.held, w.holding = nil, false
				return w.heldOp, whole, nil
			}
		}
	}
}

// closed answers the server's close and says what it said. A body of one
// byte is no close at all. The answer repeats the server's code when it is
// one that may be sent, and is 1000 otherwise.
func (w *wsConn) closed(body []byte) error {
	switch len(body) {
	case 0:
		w.goodbye(nil)
		return &Closed{Code: wsNoStatus}
	case 1:
		return wsProtocolError{"a close frame of one byte"}
	}
	c := &Closed{Code: int(binary.BigEndian.Uint16(body)), Reason: string(body[2:])}
	if wsSendable(c.Code) {
		w.goodbye(body[:2])
	} else {
		w.goodbye(wsCloseBody(wsNormal))
	}
	return c
}
