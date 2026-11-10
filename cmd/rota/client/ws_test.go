package client

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// These are about the WebSocket itself, against a hand-written peer in
// place of a server, so they need no terminal and run everywhere.

/* ------------------------------------------------------------ the peer --- */

// peerT is what a peer's script may say about the test. It is collected
// and reported by the test itself once the script is done, so a script
// that outlives its test never reports into one that has ended.
type peerT struct {
	mu   sync.Mutex
	errs []string
}

func (p *peerT) Errorf(format string, a ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, fmt.Sprintf(format, a...))
}

// peerRaw is a client whose every dial reaches script, which answers the
// handshake itself. The test waits for the script, briefly, when it ends,
// and reports what it said.
func peerRaw(t *testing.T, script func(pt *peerT, conn net.Conn, br *bufio.Reader)) *Client {
	t.Helper()
	pt := &peerT{}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the peer was still going 5s after the test")
		}
		pt.mu.Lock()
		defer pt.mu.Unlock()
		for _, e := range pt.errs {
			t.Error(e)
		}
	})
	return dialledBy("a test peer", func(context.Context) (net.Conn, error) {
		server, client := net.Pipe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer server.Close()
			script(pt, server, bufio.NewReader(server))
		}()
		return client, nil
	})
}

// peer is peerRaw with the handshake answered as a server does, and hello
// sent.
func peer(t *testing.T, script func(pt *peerT, conn net.Conn, br *bufio.Reader)) *Client {
	t.Helper()
	return peerRaw(t, func(pt *peerT, conn net.Conn, br *bufio.Reader) {
		key, ok := peerHandshake(pt, br)
		if !ok {
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+peerAccept(key)+"\r\n\r\n"); err != nil {
			pt.Errorf("the peer's 101: %v", err)
			return
		}
		if err := peerWrite(conn, 0x1, []byte(`{"type":"hello","id":"p","kind":"account","cols":80,"rows":24,"offset":0,"holder":null,"you":{"name":"me","role":"control","conn":"c1"},"viewers":[]}`)); err != nil {
			pt.Errorf("the peer's hello: %v", err)
			return
		}
		script(pt, conn, br)
	})
}

// peerHandshake reads the client's request, checks it, and says its key.
func peerHandshake(pt *peerT, br *bufio.Reader) (string, bool) {
	req, err := http.ReadRequest(br)
	if err != nil {
		pt.Errorf("the peer read no request: %v", err)
		return "", false
	}
	if req.Host != "rota" || req.Header.Get("Sec-WebSocket-Version") != "13" {
		pt.Errorf("the handshake: host %q, version %q", req.Host, req.Header.Get("Sec-WebSocket-Version"))
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	if raw, _ := base64.StdEncoding.DecodeString(key); len(raw) != 16 {
		pt.Errorf("the key is %d bytes, want 16", len(raw))
	}
	return key, true
}

func peerAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// peerFrame writes one frame with this first byte, unmasked, as a server
// does.
func peerFrame(w io.Writer, first byte, payload []byte) error {
	head := []byte{first}
	switch n := len(payload); {
	case n < 126:
		head = append(head, byte(n))
	case n < 1<<16:
		head = append(head, 126, byte(n>>8), byte(n))
	default:
		head = append(head, 127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	_, err := w.Write(append(head, payload...))
	return err
}

// peerWrite writes one whole frame of op.
func peerWrite(w io.Writer, op byte, payload []byte) error { return peerFrame(w, 0x80|op, payload) }

// peerRead reads one frame as a server does, and says whether it was masked.
func peerRead(r io.Reader) (op byte, payload []byte, masked bool, err error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, false, err
	}
	op, masked = head[0]&0x0f, head[1]&0x80 != 0
	size := uint64(head[1] & 0x7f)
	switch size {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, false, err
		}
		size = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, false, err
		}
		size = binary.BigEndian.Uint64(b[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, false, err
		}
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, false, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return op, payload, masked, nil
}

// attachPeer attaches to a peer, failing the test if it cannot.
func attachPeer(t *testing.T, c *Client) *Term {
	t.Helper()
	tm, err := c.Attach("p", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tm.Close() })
	return tm
}

// wrote waits for the result of a peer's write.
func wrote(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("the peer is still writing: the connection was not closed")
		return nil
	}
}

/* ------------------------------------------------------- the handshake --- */

// An answer whose head runs past 64 KiB is refused once 64 KiB are read,
// and the rest never is.
func TestALongAnswerHeadIsRefused(t *testing.T) {
	sent := make(chan error, 1)
	c := peerRaw(t, func(pt *peerT, conn net.Conn, br *bufio.Reader) {
		if _, ok := peerHandshake(pt, br); !ok {
			return
		}
		_, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nX-Long: "+strings.Repeat("x", 1<<20)+"\r\n\r\n")
		sent <- err
	})
	_, err := c.Attach("p", false)
	if err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("an answer with a 1 MiB header: %v, want it refused past 64 KiB", err)
	}
	if err := wrote(t, sent); err == nil {
		t.Fatal("the whole 1 MiB header was read")
	}
}

// A 101 that is not a WebSocket's, as RFC 6455 4.1 has the client check,
// is refused.
func TestA101MustBeAWebSocketsOwn(t *testing.T) {
	for name, extra := range map[string]string{
		"no Connection":       "Upgrade: websocket\r\n",
		"another key":         "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: AAAA\r\n",
		"an extension":        "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Extensions: permessage-deflate\r\n",
		"no Upgrade: websock": "Upgrade: h2c\r\nConnection: Upgrade\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			c := peerRaw(t, func(pt *peerT, conn net.Conn, br *bufio.Reader) {
				key, ok := peerHandshake(pt, br)
				if !ok {
					return
				}
				accept := "Sec-WebSocket-Accept: " + peerAccept(key) + "\r\n"
				if strings.Contains(extra, "Sec-WebSocket-Accept") {
					accept = ""
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+extra+accept+"\r\n")
			})
			var pe wsProtocolError
			if _, err := c.Attach("p", false); !errors.As(err, &pe) {
				t.Fatalf("a 101 with %s: %v, want a protocol error", name, err)
			}
		})
	}
}

/* ------------------------------------------------------- what comes in --- */

// A frame longer than maxFrame is refused before it is read, and the
// connection with it.
func TestAnOversizedFrameIsRefused(t *testing.T) {
	sent := make(chan error, 1)
	tm := attachPeer(t, peer(t, func(_ *peerT, conn net.Conn, _ *bufio.Reader) {
		sent <- peerWrite(conn, 0x2, make([]byte, 2<<20))
	}))
	if _, err := tm.Next(); err == nil {
		t.Fatal("a 2 MiB frame was read")
	}
	if err := wrote(t, sent); err == nil {
		t.Fatal("the peer wrote all 2 MiB: the connection was not closed")
	}
	if _, err := tm.Next(); err == nil {
		t.Fatal("Next read on after refusing a frame")
	}
}

// The cap is on the message: fragments that are each under it and
// together over it are refused.
func TestFragmentsArePastTheCapTogether(t *testing.T) {
	sent := make(chan error, 1)
	tm := attachPeer(t, peer(t, func(_ *peerT, conn net.Conn, _ *bufio.Reader) {
		if err := peerFrame(conn, 0x1, make([]byte, 600<<10)); err != nil {
			sent <- err
			return
		}
		sent <- peerFrame(conn, 0x80, make([]byte, 600<<10))
	}))
	_, err := tm.Next()
	var pe wsProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("two fragments of 600 KiB: %v, want a protocol error", err)
	}
	if err := wrote(t, sent); err == nil {
		t.Fatal("the peer wrote both fragments whole")
	}
}

// Fragments are joined into one message, and a ping between them is
// answered without breaking it.
func TestFragmentsAreJoined(t *testing.T) {
	pong := make(chan []byte, 1)
	tm := attachPeer(t, peer(t, func(pt *peerT, conn net.Conn, br *bufio.Reader) {
		go func() {
			_ = peerFrame(conn, 0x1, []byte(`{"type":"resi`))
			_ = peerWrite(conn, 0x9, []byte("mid"))
			_ = peerFrame(conn, 0x80, []byte(`zed","cols":7,"rows":8}`))
		}()
		op, payload, masked, err := peerRead(br)
		if err != nil || op != 0xA || !masked {
			pt.Errorf("the answer to a ping between fragments: op %#x masked %v err %v", op, masked, err)
		}
		pong <- payload
		_, _, _, _ = peerRead(br)
	}))
	f, err := tm.Next()
	if err != nil || f.Doc == nil || f.Doc.Type != "resized" || f.Doc.Cols != 7 || f.Doc.Rows != 8 {
		t.Fatalf("Next over fragments: %+v %v, want resized 7x8", f.Doc, err)
	}
	if got := <-pong; string(got) != "mid" {
		t.Fatalf("the pong carries %q, want mid", got)
	}
}

// A frame no server sends is refused: masked, with a reserved bit, or with
// an opcode that means nothing, whose payload is never read.
func TestAFrameNoServerSendsIsRefused(t *testing.T) {
	for name, first := range map[string]byte{"masked": 0, "reserved": 0x80 | 0x40 | 0x2, "opcode 3": 0x80 | 0x3} {
		t.Run(name, func(t *testing.T) {
			sent := make(chan error, 1)
			tm := attachPeer(t, peer(t, func(_ *peerT, conn net.Conn, _ *bufio.Reader) {
				if name == "masked" {
					sent <- func() error { _, err := conn.Write([]byte{0x82, 0x81, 1, 2, 3, 4, 'x' ^ 1}); return err }()
					return
				}
				sent <- peerFrame(conn, first, make([]byte, 1<<20))
			}))
			_, err := tm.Next()
			var pe wsProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("a %s frame: %v, want a protocol error", name, err)
			}
			if err := wrote(t, sent); err == nil && name != "masked" {
				t.Fatalf("the payload of a %s frame was read", name)
			}
		})
	}
}

// A ping is answered with a masked pong carrying its payload, and is not a
// frame Next returns.
func TestPingsAreAnswered(t *testing.T) {
	pong := make(chan []byte, 1)
	tm := attachPeer(t, peer(t, func(pt *peerT, conn net.Conn, br *bufio.Reader) {
		go func() {
			_ = peerWrite(conn, 0x9, []byte("p1"))
			_ = peerWrite(conn, 0x2, []byte("x"))
		}()
		op, payload, masked, err := peerRead(br)
		if err != nil || op != 0xA || !masked {
			pt.Errorf("the answer to a ping: op %#x masked %v err %v, want a masked pong", op, masked, err)
		}
		pong <- payload
		_, _, _, _ = peerRead(br)
	}))
	if h := tm.Hello(); h.ID != "p" || h.Holder != "" || h.You.Conn != "c1" {
		t.Fatalf("hello %+v", h)
	}
	f, err := tm.Next()
	if err != nil || f.Doc != nil || string(f.Output) != "x" {
		t.Fatalf("Next after a ping: %+v %v, want the output x", f, err)
	}
	select {
	case got := <-pong:
		if string(got) != "p1" {
			t.Fatalf("the pong carries %q, want p1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no pong")
	}
}

// A field the server sends as another type than the client knows is left
// empty, the rest of the document is kept, a field it does not know is
// passed over, and the socket stays up.
func TestAMistypedFieldKeepsTheSocket(t *testing.T) {
	tm := attachPeer(t, peer(t, func(_ *peerT, conn net.Conn, br *bufio.Reader) {
		_ = peerWrite(conn, 0x1, []byte(`{"type":"resized","cols":"wide","rows":15,"later":{"x":[1,2]}}`))
		_ = peerWrite(conn, 0x2, []byte("x"))
		_, _, _, _ = peerRead(br)
	}))
	f, err := tm.Next()
	if err != nil || f.Doc == nil || f.Doc.Type != "resized" || f.Doc.Rows != 15 || f.Doc.Cols != 0 {
		t.Fatalf("a document with a mistyped field: %+v %v, want resized with rows 15", f.Doc, err)
	}
	if f, err := tm.Next(); err != nil || string(f.Output) != "x" {
		t.Fatalf("after a mistyped field: %+v %v, want the output x", f, err)
	}
}

// A server that says nothing for as long as one is waited for is said to
// have gone quiet, by name.
func TestASilentServerIsSaidSo(t *testing.T) {
	tm := attachPeer(t, peer(t, func(_ *peerT, _ net.Conn, br *bufio.Reader) {
		_, _, _, _ = peerRead(br)
	}))
	tm.ws.quiet = 100 * time.Millisecond
	_, err := tm.Next()
	if err == nil || !strings.Contains(err.Error(), "the rota daemon on a test peer") || !strings.Contains(err.Error(), "said nothing") {
		t.Fatalf("a silent server: %v", err)
	}
}

/* ---------------------------------------------------------- the close --- */

// The server's close is what Next returns, code and reason, and it is
// answered with the same code, masked.
func TestTheServersCloseIsReturned(t *testing.T) {
	echo := make(chan []byte, 1)
	tm := attachPeer(t, peer(t, func(pt *peerT, conn net.Conn, br *bufio.Reader) {
		go func() { _ = peerWrite(conn, 0x8, append([]byte{0x03, 0xE9}, "going away"...)) }()
		op, payload, masked, err := peerRead(br)
		if err != nil || op != 0x8 || !masked {
			pt.Errorf("the answer to a close: op %#x masked %v err %v, want a masked close", op, masked, err)
		}
		echo <- payload
	}))
	_, err := tm.Next()
	var closed *Closed
	if !errors.As(err, &closed) || closed.Code != 1001 || closed.Reason != "going away" {
		t.Fatalf("Next after the peer's close: %v, want 1001 going away", err)
	}
	if got := <-echo; len(got) != 2 || got[0] != 0x03 || got[1] != 0xE9 {
		t.Fatalf("the close was answered with %x, want 03e9", got)
	}
}

// A close code no frame may carry is not repeated: the answer is 1000. A
// close body of one byte is no close at all.
func TestACloseIsAnsweredOnlyWithACodeThatMayBeSent(t *testing.T) {
	echo := make(chan []byte, 1)
	tm := attachPeer(t, peer(t, func(_ *peerT, conn net.Conn, br *bufio.Reader) {
		go func() { _ = peerWrite(conn, 0x8, []byte{0x03, 0xED}) }()
		_, payload, _, _ := peerRead(br)
		echo <- payload
	}))
	_, err := tm.Next()
	var closed *Closed
	if !errors.As(err, &closed) || closed.Code != 1005 {
		t.Fatalf("a close naming 1005: %v", err)
	}
	if got := <-echo; len(got) != 2 || got[0] != 0x03 || got[1] != 0xE8 {
		t.Fatalf("a close naming 1005 was answered with %x, want 03e8", got)
	}

	tm = attachPeer(t, peer(t, func(_ *peerT, conn net.Conn, _ *bufio.Reader) {
		_ = peerWrite(conn, 0x8, []byte{0x03})
	}))
	_, err = tm.Next()
	var pe wsProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("a close of one byte: %v, want a protocol error", err)
	}
}

/* ------------------------------------------------------- what goes out --- */

// Two pastes typed at once go out one after the other, never shuffled.
func TestTwoPastesNeverInterleave(t *testing.T) {
	const each = 300 << 10
	order := make(chan string, 1)
	tm := attachPeer(t, peer(t, func(pt *peerT, _ net.Conn, br *bufio.Reader) {
		var seq strings.Builder
		for got := 0; got < 2*each; {
			op, payload, _, err := peerRead(br)
			if err != nil || op != 0x2 || len(payload) == 0 {
				pt.Errorf("frame %d of typing: op %#x %d bytes, err %v", seq.Len(), op, len(payload), err)
				break
			}
			got += len(payload)
			seq.WriteByte(payload[0])
		}
		order <- seq.String()
		_, _, _, _ = peerRead(br)
	}))
	var wg sync.WaitGroup
	for _, b := range []byte("ab") {
		wg.Go(func() {
			if err := tm.Type([]byte(strings.Repeat(string(b), each))); err != nil {
				t.Errorf("typing %c: %v", b, err)
			}
		})
	}
	wg.Wait()
	seq := <-order
	if changes := strings.Count(seq, "ab") + strings.Count(seq, "ba"); changes != 1 {
		t.Fatalf("two pastes went out as %s, shuffled", seq)
	}
}

// A paste one byte past a frame's worth does not end in a frame of one
// byte, which the server would take for a key pressed on its own.
func TestAPasteNeverEndsInALoneByte(t *testing.T) {
	sizes := make(chan []int, 1)
	tm := attachPeer(t, peer(t, func(pt *peerT, _ net.Conn, br *bufio.Reader) {
		var got []int
		for total := 0; total < termTypeChunk+1; {
			_, payload, _, err := peerRead(br)
			if err != nil {
				pt.Errorf("reading typing: %v", err)
				break
			}
			got = append(got, len(payload))
			total += len(payload)
		}
		sizes <- got
		_, _, _, _ = peerRead(br)
	}))
	if err := tm.Type(append([]byte(strings.Repeat("x", termTypeChunk)), 0x1b)); err != nil {
		t.Fatal(err)
	}
	got := <-sizes
	if len(got) != 2 || got[1] < 2 {
		t.Fatalf("a paste of %d bytes went out as frames of %v", termTypeChunk+1, got)
	}
}
