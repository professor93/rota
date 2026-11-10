// Package attach hands a person's terminal to a session the rota daemon
// holds: what the session draws reaches their screen, what they type reaches
// the session, and colours, mouse and keyboard behave as the CLI's own. It is
// how `rota run --attach`, an SSH `attach` or `watch`, and the TUI's attach
// all look at a session, so each of them reads the same chord and ends with
// the same one line.
//
// The chord is Ctrl-\ then a second key: d leaves and the session goes on,
// n and p move to the next or the previous session, k takes the keyboard, z
// fits the session to this window, ? shows the keys, and the chord key again
// sends it through. Attaching never resizes the session; only z does. A
// window smaller than the session is shown a crop of the session's own
// screen, with a note, and never the raw bytes wrapped.
//
// An attach ends with its Result: why it ended, the session it last showed,
// and the one line said to the person, with how to come back.
package attach

import (
	"io"

	"github.com/professor93/rota/cmd/rota/client"
)

// DefaultKey is the chord key, Ctrl-\, the byte 0x1C; 1.4.0 has no setting
// for another. Ctrl-] was not taken because Claude Code binds it itself.
const DefaultKey byte = 0x1c

// Terminal is the person's side of an attach. Input is what they type, Write
// is their screen, Size is their window in cells, and Resized fires when that
// window changes, after which Size reads the new one. It is the person's own
// terminal for `rota run --attach`, an SSH channel with a pty, or the TUI's
// screen handed over for the attach.
//
// Whoever made the Terminal owns its reads: one goroutine of theirs reads it
// and sends each chunk on Input, a chunk the receiver keeps, and closes Input
// when the input ends. An attach receives from Input only while it runs, and
// never once it has returned, so a key typed after it is read by whatever
// reads the terminal next. A reader that is never waited on, one blocked in
// Read when the attach ends, would instead take that key and drop it.
type Terminal interface {
	Input() <-chan []byte
	io.Writer
	Size() (cols, rows int)
	Resized() <-chan struct{}
}

// Why is what ended an attach.
type Why int

const (
	// Detached is the person leaving with the chord's d. The session goes
	// on, and they can come back to it.
	Detached Why = iota + 1
	// Ended is the session itself ending while it was shown: its CLI
	// exited, was stopped by a signal (a hang-up from the daemon, a kill),
	// or its holder went without saying how.
	Ended
	// Lost is the attach ending while the session may go on: the link to the
	// daemon gone and not back within the redial, the context ended (a
	// signal, or the SSH connection closing), or the person's terminal
	// failing a write.
	Lost
	// Refused is the attach never starting: no such session, one that is not
	// attachable, or a daemon that said no.
	Refused
)

// Result is how an attach ended.
type Result struct {
	// Why is what ended it.
	Why Why
	// Session is the session it showed last, which after n or p is not the
	// one it began with. For Refused it is whatever was known of the session
	// asked for, which may be only its id.
	Session client.Session
	// Code is, for Ended, the CLI's exit code, or -1 when there is none: a
	// signal ended the CLI, or its holder went without one. It is 0
	// otherwise.
	Code int
	// Err is, for Lost and Refused, what went wrong. It is nil otherwise.
	Err error
}

// ExitCode is the code a command that attached exits with, `rota run
// --attach` and an SSH channel alike: 0 for a detach; for an end, the CLI's
// own code clamped to 0–255, with no code (-1) 1; and 1 for a lost or
// refused attach, or a Result with no Why. A usage error, 2, is the caller's
// to say before any attach.
func (r Result) ExitCode() int {
	switch r.Why {
	case Detached:
		return 0
	case Ended:
		switch {
		case r.Code < 0:
			return 1
		case r.Code > 255:
			return 255
		}
		return r.Code
	}
	return 1
}

// Options are what a caller sets on an attach. The zero value is a local
// attach that types where the keyboard rules allow it, with Ctrl-\, no
// first-attach card and no redial.
type Options struct {
	// Key is the chord key, a control byte, named to the person as KeyName
	// names it. 0 is DefaultKey, which is what DaemonKey gives in 1.4.0;
	// another is for tests, and for a later daemon that lets the key be set.
	Key byte

	// Watch shows the session and never types into it. What the person
	// types is dropped here rather than sent and refused. The chord is still
	// read, so d, n, p and ? work, and z and k are answered with a note,
	// since a watcher neither fits the session nor holds its keyboard.
	Watch bool

	// Settle drops the replies the person's terminal still owes to whatever
	// used it before the attach, up to the answer to one DA1 query it sends,
	// and waits at most a second for that answer. The TUI sets it, since its
	// own queries may be answered after it hands the terminal over.
	Settle bool

	// Teach is asked, once before an attach shows a session, whether this
	// person is still to be shown the chord. True shows the card naming the
	// keys, for five seconds or until a key, and records them as shown, so
	// the next call says false. Nil never shows it. TaughtFile gives the one
	// the daemon's terminals folder keeps.
	Teach func(person string) bool

	// Again is what to type to come back to a session, given Word of it, as
	// the last line says it. Nil is "rota run --attach " and the word; SSH
	// says its own.
	Again func(word string) string

	// Redial dials the daemon again when the link to it is lost, every half
	// second for a minute, and the attach goes on with what it returns,
	// repainted and with nothing typed meanwhile sent. Nil ends the attach as
	// Lost at once.
	Redial func() (*client.Client, error)
}
