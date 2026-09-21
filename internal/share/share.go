// Package share is the protocol between a rota that is sitting at a terminal
// and a rota that is serving one.
//
// It exists because both ends are rota and neither is a browser. A terminal
// the server started is read over a WebSocket, because the thing reading it
// is a page; a terminal somebody is sitting at is offered to the server over
// a unix socket in the store, because the thing offering it is another
// process of the same program run by the same person. Who may connect is
// decided by the filesystem — the socket is in a directory only its owner
// can enter — rather than by a token, which is the right answer when the
// only possible peer is yourself.
//
// The framing is a length and a type byte. There is no handshake beyond the
// first two frames: the sharer says hello, and the server answers with a
// welcome or with a refusal in words the sharer prints. Everything after
// that is one type byte and a payload, either way down the same connection.
//
//	+--------+------+-----------------+
//	| length | kind |     payload     |
//	|  4 BE  |  1   | length-1 bytes  |
//	+--------+------+-----------------+
//
// The length counts the type byte, so an empty frame is five bytes on the
// wire. Payloads are JSON where they are a document and raw bytes where they
// are a terminal's output or somebody's typing: a keystroke put in JSON
// would have to be escaped, and a screenful of output would double.
package share

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	rota "github.com/professor93/rota/lib"
)

// Version is the protocol both ends must agree on. It is a whole number and
// it is compared for equality, not for order: a sharer and a server that do
// not match are a rota that was upgraded and one that was not, and guessing
// which halves of the two versions overlap is how a terminal ends up half
// working.
const Version = 1

// SockName is the socket's name inside the store's terminals directory. One
// socket serves every sharer, because a sharer arrives before it has a name
// and the id is the server's to give.
const SockName = "share.sock"

// The frames. Each is one byte on the wire and each travels in one direction
// only, except the two heartbeats.
const (
	// Hello is the sharer's first frame, and Welcome or Refused is the
	// server's answer to it.
	Hello   byte = 1
	Welcome byte = 2
	Refused byte = 3

	// Sharer to server: what the terminal printed, how big it is now, how
	// much was dropped on the way here, and how it ended.
	Output  byte = 4
	Resized byte = 5
	Gap     byte = 6
	Exit    byte = 7

	// Server to sharer: what somebody typed on the page, and the end of it.
	Input byte = 8
	Kill  byte = 9

	// Both ways, so either end notices a peer that is there but gone.
	Ping byte = 10
	Pong byte = 11
)

// MaxFrame bounds one frame. Output arrives in bursts of tens of kilobytes;
// this is far above anything either end sends and is here so that a length
// somebody made up cannot ask for a gigabyte of memory before anything has
// looked at it.
const MaxFrame = 1 << 20

// HelloMsg is what a sharer says it is offering. None of it is checked
// against anything: the socket's own permissions are the whole of the
// authorization, and what this says is the same person's account described
// by the same program.
type HelloMsg struct {
	Version int `json:"version"`
	// Account, AccountLabel and Provider are the account whose CLI is
	// running, so the page names it exactly as it names any other terminal.
	Account      int    `json:"account,omitempty"`
	AccountLabel string `json:"account_label,omitempty"`
	Provider     string `json:"provider,omitempty"`
	// Label is what the person calls this terminal: the folder's base name
	// unless --label said otherwise.
	Label string `json:"label,omitempty"`
	Cwd   string `json:"cwd,omitempty"`
	Cols  uint16 `json:"cols,omitempty"`
	Rows  uint16 `json:"rows,omitempty"`
	// PID is the sharing rota, not the CLI under it: it is the process that
	// answers for this terminal, and the one to look for when it stops.
	PID int `json:"pid,omitempty"`
	// Mode is "control" or "watch". Watch is a terminal offered to be read
	// and never typed into, whatever role the person on the page has.
	Mode string `json:"mode,omitempty"`
}

// WelcomeMsg is the server taking it, and the id it gave it.
type WelcomeMsg struct {
	Version  int    `json:"version"`
	Terminal string `json:"terminal"`
}

// RefusedMsg is the server not taking it, in a sentence the sharer prints as
// it is. Final says there is no point coming back: a version that will not
// match after a retry, rather than a server that is full this minute.
type RefusedMsg struct {
	Message string `json:"message"`
	Final   bool   `json:"final,omitempty"`
}

// ResizedMsg is the local window having changed.
type ResizedMsg struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// GapMsg is output that never left the sharer: the local screen is never
// held up for the server, so a server that stopped reading is told how much
// it missed rather than being waited for.
type GapMsg struct {
	Bytes int64 `json:"bytes"`
}

// ExitMsg is the CLI having ended.
type ExitMsg struct {
	Code int `json:"code"`
}

// The two modes, spelled once.
const (
	ModeControl = "control"
	ModeWatch   = "watch"
)

// ErrClosed is what a send on a connection somebody has already finished
// with answers, so a caller can tell it from a write that failed.
var ErrClosed = errors.New("share: the link is closed")

// Conn is one link, with the one lock that keeps two goroutines from
// interleaving halves of two frames. Reading is single-threaded by
// construction — there is exactly one reader on each end — so only writes
// are guarded.
type Conn struct {
	c  net.Conn
	mu sync.Mutex
	// closed is under the same lock, so a frame is never written to a
	// descriptor that has just been handed back to the kernel.
	closed bool
	// beat is how long a link may go without a frame before it is treated
	// as gone. Both ends ping well inside it, and any frame at all refreshes
	// it: an active terminal never reaches it.
	beat time.Duration
}

// NewConn wraps a dialed or accepted socket. beat of zero leaves the
// deadlines off, which is what a test that drives both ends by hand wants.
func NewConn(c net.Conn, beat time.Duration) *Conn {
	k := &Conn{c: c, beat: beat}
	k.touch()
	return k
}

// touch pushes the read deadline out. A link is judged by whether anything
// at all arrives on it, not by whether the answer to a particular ping did:
// the second would need a table of what is outstanding for no more truth.
func (c *Conn) touch() {
	if c.beat > 0 {
		_ = c.c.SetReadDeadline(time.Now().Add(3 * c.beat))
	}
}

// Send writes one frame.
func (c *Conn) Send(kind byte, payload []byte) error {
	if len(payload)+1 > MaxFrame {
		return fmt.Errorf("share: a frame of %d bytes is past the limit", len(payload)+1)
	}
	var head [5]byte
	binary.BigEndian.PutUint32(head[:4], uint32(len(payload)+1))
	head[4] = kind
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if c.beat > 0 {
		_ = c.c.SetWriteDeadline(time.Now().Add(3 * c.beat))
	}
	if _, err := c.c.Write(head[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.c.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// SendJSON writes one frame whose payload is a document.
func (c *Conn) SendJSON(kind byte, v any) error {
	raw, err := rota.Encode(v)
	if err != nil {
		return err
	}
	return c.Send(kind, raw)
}

// Read takes the next frame. An oversized length ends the link rather than
// being trimmed: a peer that sent one is not speaking this protocol.
func (c *Conn) Read() (byte, []byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(c.c, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n == 0 || n > MaxFrame {
		return 0, nil, fmt.Errorf("share: a frame of %d bytes is not one of ours", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.c, body); err != nil {
		return 0, nil, err
	}
	c.touch()
	return body[0], body[1:], nil
}

// ReadJSON is Read for a frame that carries a document, for a caller that
// already knows which frame it is waiting for.
func ReadJSON(c *Conn, v any) (byte, error) {
	kind, payload, err := c.Read()
	if err != nil {
		return 0, err
	}
	if len(payload) > 0 {
		if err := rota.UnmarshalLenient(payload, v); err != nil {
			return kind, err
		}
	}
	return kind, nil
}

// Deadline bounds the next few frames, for the handshake: a socket that
// accepts a connection and then says nothing must not hold either end.
func (c *Conn) Deadline(t time.Time) {
	_ = c.c.SetDeadline(t)
}

// Clear takes the handshake's deadline off again and puts the heartbeat's
// back, which is what a link that is now carrying a terminal wants.
func (c *Conn) Clear() {
	_ = c.c.SetWriteDeadline(time.Time{})
	_ = c.c.SetReadDeadline(time.Time{})
	c.touch()
}

// Close ends the link. It is safe to call twice, because both the reader and
// whatever noticed the ending call it.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.c.Close()
}

// Unmarshal reads a document out of a payload, so neither end has to name
// the JSON library.
func Unmarshal(payload []byte, v any) error { return rota.UnmarshalLenient(payload, v) }
