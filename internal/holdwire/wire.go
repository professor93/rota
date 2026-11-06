// Package holdwire is what a held session's two processes agree on: the
// holder, which owns one pty and the CLI on it, and the rota daemon, which
// links viewers to it. It is the protocol between them, the request that
// starts a holder and its answer, the record each session leaves on disk,
// and the locks that say who is alive.
//
// Holders outlive the daemon that started them, so a holder from last
// week's rota may be linked to by this week's. That is why the protocol has
// a version of its own, a major number and a list of features, apart from
// share.Version: frames are only ever added, an unknown frame is ignored,
// and the core set — output, input, resize, exit, since and screen — never
// changes meaning within a major.
//
// The framing is internal/share's Conn: a length, a kind byte and a payload.
// Only the kind bytes are this package's. Payloads are JSON where they are a
// document and raw bytes where they are a terminal's output or somebody's
// typing, each behind an 8-byte big-endian number that says where it
// belongs.
package holdwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/professor93/rota/internal/vtscan"
	rota "github.com/professor93/rota/lib"
)

// Major is the protocol this rota speaks first, and Majors every one it can
// still speak: a daemon keeps every major it has shipped, so a holder an
// upgrade left behind stays attachable.
const Major = 1

// Majors is every major this rota speaks, Major among them.
var Majors = []int{1}

// The features a holder may offer on top of its major. A daemon uses the
// frames of a feature only when the holder said it has it.
const (
	FeatScreen = "screen" // a repaint of the current screen on ScreenAsk
	FeatState  = "state"  // title, bells, modes and the CLI's own registry
	FeatSend   = "send"   // text typed when the session is ready for it
	FeatStop   = "stop"   // a hang-up asked for over the link
)

// The frames, one byte each on the wire. Holder-to-daemon and daemon-to-
// holder share one numbering so a frame read on the wrong side is plainly
// one nobody should have sent.
const (
	Hello        byte = 1  // daemon: HelloMsg, the majors and features it speaks
	Welcome      byte = 2  // holder: WelcomeMsg, or Refused
	Refused      byte = 3  // holder: RefusedMsg
	Output       byte = 4  // holder: PutOutput, what the CLI printed and where
	Input        byte = 5  // daemon: PutInput, keystrokes and their sequence number
	InputRefused byte = 6  // holder: InputRefusedMsg, keystrokes it could not take
	Resize       byte = 7  // daemon: SizeMsg, a size somebody asked for
	Resized      byte = 8  // holder: SizeMsg, the pty's size now
	Exit         byte = 9  // holder: ExitMsg, the CLI has ended
	Since        byte = 10 // daemon: SinceMsg, send output from this offset on
	ScreenAsk    byte = 11 // daemon: ScreenAskMsg
	Screen       byte = 12 // holder: PutScreen, a repaint and the offset it stands for
	State        byte = 13 // holder: StateMsg
	Stop         byte = 14 // daemon: hang up the CLI
	Send         byte = 15 // daemon: SendMsg
	Sent         byte = 16 // holder: SentMsg
	Ping         byte = 17 // either way
	Pong         byte = 18 // either way
)

// HelloMsg opens a link: every major the daemon speaks and the features it
// knows.
type HelloMsg struct {
	Majors   []int    `json:"majors"`
	Features []string `json:"features,omitempty"`
}

// WelcomeMsg is the holder taking the link: the major both speak, the
// features it has, the rota that made it, the span of output its ring still
// holds, and the pty's size.
type WelcomeMsg struct {
	Major    int      `json:"major"`
	Features []string `json:"features,omitempty"`
	Version  string   `json:"version"`
	Oldest   int64    `json:"oldest"`
	End      int64    `json:"end"`
	Cols     uint16   `json:"cols"`
	Rows     uint16   `json:"rows"`
}

// RefusedMsg is the holder not taking the link, in a sentence a person can
// read.
type RefusedMsg struct {
	Message string `json:"message"`
}

// SinceMsg asks for output from Offset on. Asked for what the ring no longer
// has, the holder sends from its oldest byte, and the first Output's offset
// says how much was missed.
type SinceMsg struct {
	Offset int64 `json:"offset"`
}

// SizeMsg is a pty size: asked for in Resize, reported in Resized.
type SizeMsg struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// ExitMsg is how the CLI ended.
type ExitMsg struct {
	Code int `json:"code"`
}

// ScreenAskMsg asks for a repaint, answered by a Screen with the same Seq.
type ScreenAskMsg struct {
	Seq uint64 `json:"seq"`
}

// ScreenHead is the document at the front of a Screen frame. OK false is a
// holder with no screen to give — its emulator failed — and a daemon then
// replays the ring instead. Offset is the ring offset the repaint stands for:
// output from there on follows it. Modes are the terminal modes the CLI had
// set at that offset, which a repaint alone does not restore.
type ScreenHead struct {
	Seq    uint64       `json:"seq"`
	OK     bool         `json:"ok"`
	Offset int64        `json:"offset"`
	Cols   uint16       `json:"cols"`
	Rows   uint16       `json:"rows"`
	Modes  vtscan.Modes `json:"modes,omitzero"`
}

// StateMsg is what the holder knows about the session beyond its bytes: the
// title the CLI set, how many bells it rang, the terminal modes it has on,
// and what the CLI's own registry says, when it keeps one.
type StateMsg struct {
	Title    string       `json:"title,omitempty"`
	Bells    uint64       `json:"bells,omitempty"`
	Modes    vtscan.Modes `json:"modes,omitzero"`
	Registry *Registry    `json:"registry,omitempty"`
}

// Registry is one CLI session as the CLI itself recorded it (Claude Code's
// sessions/<pid>.json). Every field may be empty: the file is undocumented
// and is read defensively.
type Registry struct {
	SessionID  string `json:"session_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	Status     string `json:"status,omitempty"`
	WaitingFor string `json:"waiting_for,omitempty"`
	Kind       string `json:"kind,omitempty"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
}

// SendMsg asks the holder to type Text and Enter once the session is ready
// for it, waiting at most WaitMS. Force types it even while the CLI waits for
// a permission or a question, where Enter may approve something.
type SendMsg struct {
	Seq    uint64 `json:"seq"`
	Text   string `json:"text"`
	Force  bool   `json:"force,omitempty"`
	WaitMS int    `json:"wait_ms,omitempty"`
}

// SentMsg answers a SendMsg with the same Seq. Code is SentOK when the text
// went in.
type SentMsg struct {
	Seq     uint64 `json:"seq"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// The answers a Send can have.
const (
	SentOK      = ""        // typed
	SentWaiting = "waiting" // the CLI waits for a permission or a question; Force types anyway
	SentTimeout = "timeout" // the session never became ready in time
	SentEnded   = "ended"   // the CLI ended first
)

// InputRefusedMsg is keystrokes the holder could not take — its writer's
// queue was full — said back so the person who typed them is told rather
// than left to wonder.
type InputRefusedMsg struct {
	Seq     uint64 `json:"seq"`
	Message string `json:"message"`
}

// errShort is a raw payload too short for the number in front of it.
var errShort = errors.New("holdwire: a payload shorter than its 8-byte header")

// PutOutput is an Output payload: the offset of p's first byte, then p.
func PutOutput(off int64, p []byte) []byte { return putNumbered(uint64(off), p) }

// OutputOf reads an Output payload. The bytes it returns share b's memory.
func OutputOf(b []byte) (int64, []byte, error) {
	n, p, err := numbered(b)
	return int64(n), p, err
}

// PutInput is an Input payload: a sequence number, then the keystrokes.
func PutInput(seq uint64, p []byte) []byte { return putNumbered(seq, p) }

// InputOf reads an Input payload. The bytes it returns share b's memory.
func InputOf(b []byte) (uint64, []byte, error) { return numbered(b) }

func putNumbered(n uint64, p []byte) []byte {
	out := make([]byte, 8+len(p))
	binary.BigEndian.PutUint64(out, n)
	copy(out[8:], p)
	return out
}

func numbered(b []byte) (uint64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, errShort
	}
	return binary.BigEndian.Uint64(b), b[8:], nil
}

// PutScreen is a Screen payload: the header as compact JSON, a newline, and
// the repaint's raw bytes. Compact JSON never holds a raw newline, so the
// first one ends the header whatever the repaint holds.
func PutScreen(h ScreenHead, repaint []byte) ([]byte, error) {
	head, err := rota.Encode(h)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(head)+1+len(repaint))
	out = append(append(append(out, head...), '\n'), repaint...)
	return out, nil
}

// ScreenOf reads a Screen payload. The repaint shares b's memory.
func ScreenOf(b []byte) (ScreenHead, []byte, error) {
	var h ScreenHead
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return h, nil, errors.New("holdwire: a Screen with no end to its header")
	}
	if err := rota.UnmarshalLenient(b[:i], &h); err != nil {
		return h, nil, fmt.Errorf("holdwire: a Screen header: %w", err)
	}
	return h, b[i+1:], nil
}

// Agree is the highest major both ends speak, and false when they share
// none: a holder and a daemon with no major in common cannot link, and the
// daemon lists that holder as made by another rota rather than guessing.
func Agree(ours, theirs []int) (int, bool) {
	best, ok := 0, false
	for _, m := range ours {
		if slices.Contains(theirs, m) && (!ok || m > best) {
			best, ok = m, true
		}
	}
	return best, ok
}
