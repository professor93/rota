package client

import (
	"context"
	"encoding/json"
	"fmt"
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
// their frames go out one at a time.
type Term struct {
	ws    *wsConn
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
// by Type, with whichever of the rest that kind carries.
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

// Closed is the server's goodbye: the close code it sent, and its reason.
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

// termTypeChunk bounds one frame of typing, well under what the server
// takes in one message: a paste longer than it goes as several frames, in
// order, rather than as one the server would close the socket over.
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
	t := &Term{ws: ws}
	op, payload, err := ws.message()
	if err != nil {
		return nil, fmt.Errorf("the rota daemon on %s: %w", c.sock, err)
	}
	var kind struct {
		Type string `json:"type"`
	}
	if op != wsOpText || json.Unmarshal(payload, &kind) != nil || kind.Type != "hello" || json.Unmarshal(payload, &t.hello) != nil {
		ws.drop()
		return nil, fmt.Errorf("the rota daemon on %s: %w", c.sock, wsProtocolError{"its first frame was not hello"})
	}
	_ = conn.SetDeadline(time.Time{})
	return t, nil
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
// io.ErrUnexpectedEOF, and after Close it is net.ErrClosed; once Next has
// returned an error it returns it again.
func (t *Term) Next() (Frame, error) {
	if t.ended != nil {
		return Frame{}, t.ended
	}
	op, payload, err := t.ws.message()
	if err == nil && op == wsOpText {
		var d Doc
		if jerr := json.Unmarshal(payload, &d); jerr != nil {
			t.ws.drop()
			err = wsProtocolError{"a document that is not JSON: " + jerr.Error()}
		} else {
			return Frame{Doc: &d}, nil
		}
	}
	if err != nil {
		t.ended = err
		return Frame{}, err
	}
	return Frame{Output: payload}, nil
}

// Type sends p as typing, as binary frames.
func (t *Term) Type(p []byte) error {
	for len(p) > 0 {
		n := min(len(p), termTypeChunk)
		if err := t.ws.send(wsOpBinary, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

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
