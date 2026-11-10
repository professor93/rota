package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Term is one attach to a terminal, over its WebSocket: hello first, then
// the CLI's output and the server's documents as Next reads them, and the
// person's typing, a fit and a claim the other way. Next is one
// goroutine's; Type, Fit, Claim and Close may be called from any, and
// what each sends goes out whole, never between another's frames.
type Term struct {
	ws    *wsConn
	sock  string
	hello Hello

	closeOnce sync.Once
	// ended is the error Next ended with: once ended it says so again.
	ended error
}

// Hello is the first frame of an attach: which terminal, at what size, how
// far into its output, who holds the keyboard — "" for nobody — and who
// this connection is. Repaint says the first output is the screen as it
// stands, for Offset.
type Hello struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Cols       uint16 `json:"cols"`
	Rows       uint16 `json:"rows"`
	Offset     int64  `json:"offset"`
	Holder     string `json:"holder"`
	HolderConn string `json:"holder_conn"`
	You        Viewer `json:"you"`
	Repaint    bool   `json:"repaint"`
}

// Viewer is one connection to a terminal: whose, with what role, and its
// id among the connections.
type Viewer struct {
	Name string `json:"name"`
	Role string `json:"role"`
	Conn string `json:"conn"`
}

// Frame is one thing Next read: the CLI's output, or one of the server's
// documents. Exactly one is set.
type Frame struct {
	Output []byte
	Doc    *Doc
}

// Doc is one document the terminal socket sends after hello: keyboard,
// repaint, claim_request, resized, viewers, gap, exit, error or pong, named
// by Type, with whichever of the rest that kind carries. A field the
// server sent as another type than this one is left empty, and the rest
// of the document is kept: a newer daemon may say more than this knows.
type Doc struct {
	Type       string `json:"type"`
	Holder     string `json:"holder"`
	HolderConn string `json:"holder_conn"`
	Message    string `json:"message"`
	By         string `json:"by"`
	ID         string `json:"id"`
	Cols       uint16 `json:"cols"`
	Rows       uint16 `json:"rows"`
	Offset     int64  `json:"offset"`
	From       int64  `json:"from"`
	To         int64  `json:"to"`
	Bytes      int64  `json:"bytes"`
	Code       int    `json:"code"`
}

// Closed is the server's goodbye: the close code it sent, and its reason,
// raw, as the server wrote it. Whoever shows it to a person cleans it
// first.
type Closed struct {
	Code   int
	Reason string
}

func (c *Closed) Error() string {
	if c.Reason == "" {
		return fmt.Sprintf("the rota daemon closed the terminal's socket (%d)", c.Code)
	}
	return fmt.Sprintf("the rota daemon closed the terminal's socket (%d): %s", c.Code, c.Reason)
}

// termTypeChunk bounds one frame of typing. It is needed, not a nicety: a
// held session's holder link carries at most 1 MiB a frame, its own nine
// bytes of kind and sequence included, so a message near 1 MiB fails there,
// and one over it has the server close the socket with 1009.
const termTypeChunk = 64 << 10

// Attach opens the terminal id's socket, watching only when watch is set,
// from the start of what it keeps, and reads its hello. An answer other
// than the upgrade is a *Refusal, as every other request's is.
func (c *Client) Attach(id string, watch bool) (*Term, error) {
	path := "/v1/terminals/" + url.PathEscape(id) + "/ws"
	if watch {
		path += "?mode=watch"
	}
	deadline := time.Now().Add(requestWait)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("the rota daemon on %s: %w", c.sock, err)
	}
	ws, resp, raw, err := wsHandshake(conn, path, deadline)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("the rota daemon on %s: %w", c.sock, err)
	}
	if resp != nil {
		conn.Close()
		return nil, termRefusal(resp, raw, path)
	}
	t := &Term{ws: ws, sock: c.sock}
	op, payload, err := ws.message(time.Until(deadline))
	if err != nil {
		return nil, fmt.Errorf("the rota daemon on %s: %w", c.sock, err)
	}
	var kind struct {
		Type string `json:"type"`
	}
	if op != wsOpText || !termDecode(payload, &kind) || kind.Type != "hello" || !termDecode(payload, &t.hello) {
		ws.drop()
		return nil, fmt.Errorf("the rota daemon on %s: %w", c.sock, wsProtocolError{"its first frame was not hello"})
	}
	_ = conn.SetDeadline(time.Time{})
	return t, nil
}

// termDecode reads a document into v leniently: a field of another type
// than v's is passed over and the rest kept. It is false only for what is
// not a JSON object at all.
func termDecode(raw []byte, v any) bool {
	err := json.Unmarshal(raw, v)
	var mistyped *json.UnmarshalTypeError
	return err == nil || errors.As(err, &mistyped) && mistyped.Field != ""
}

// termRefusal is an answer that was not the upgrade, said as do says one.
func termRefusal(resp *http.Response, raw []byte, path string) *Refusal {
	var e struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
		Holder string `json:"holder"`
	}
	if json.Unmarshal(raw, &e) != nil || strings.TrimSpace(e.Error) == "" {
		e.Error = fmt.Sprintf("the rota daemon answered %s to %s %s", resp.Status, http.MethodGet, path)
	}
	return &Refusal{Status: resp.StatusCode, Reason: e.Reason, Message: e.Error, Holder: e.Holder}
}

// Hello is the first frame, which Attach read.
func (t *Term) Hello() Hello { return t.hello }

// Next is the next output or document. Pings are answered and never
// returned. The server's close is a *Closed, an end without one is
// io.ErrUnexpectedEOF, and after Close it is net.ErrClosed, each as itself;
// anything else names the daemon. Once Next has returned an error it
// returns it again.
func (t *Term) Next() (Frame, error) {
	if t.ended != nil {
		return Frame{}, t.ended
	}
	op, payload, err := t.ws.message(t.ws.quiet)
	if err == nil && op == wsOpText {
		var d Doc
		if termDecode(payload, &d) {
			return Frame{Doc: &d}, nil
		}
		t.ws.drop()
		err = wsProtocolError{"a document that is not a JSON object"}
	}
	if err != nil {
		var closed *Closed
		if !errors.As(err, &closed) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, net.ErrClosed) {
			err = fmt.Errorf("the rota daemon on %s: %w", t.sock, err)
		}
		t.ended = err
		return Frame{}, err
	}
	return Frame{Output: payload}, nil
}

// Type sends p as typing, in binary frames of at most termTypeChunk, all of
// them before anything else is sent. The server judges each frame on its
// own: should the keyboard move, or a send begin to wait, partway through
// a long paste, the rest of it is refused.
func (t *Term) Type(p []byte) error { return t.ws.sendAll(wsOpBinary, p, termTypeChunk) }

// Fit asks for the session to be this size: the one resize a held session
// takes.
func (t *Term) Fit(cols, rows uint16) error {
	return t.ws.say(struct {
		Type string `json:"type"`
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
		Fit  bool   `json:"fit"`
	}{"resize", cols, rows, true})
}

// Claim asks for the keyboard; force takes it from a holder who has not
// answered.
func (t *Term) Claim(force bool) error {
	return t.ws.say(struct {
		Type  string `json:"type"`
		Force bool   `json:"force"`
	}{"claim", force})
}

// Close says goodbye with 1000 and lets the connection go. A Next blocked
// meanwhile returns. A second Close does nothing.
func (t *Term) Close() error {
	t.closeOnce.Do(func() { t.ws.goodbye(wsCloseBody(wsNormal)) })
	return nil
}
