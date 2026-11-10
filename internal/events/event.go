// Package events is the one shape of what a rota server says has happened:
// an account crossing its threshold, a session waiting for its person, a
// viewer attaching, the daemon starting or stopping. The server that makes
// them (api), the program that reads them (cmd/rota's client and notify) and
// a page in a browser all agree on this package and on nothing else, so it
// is internal to the module rather than public API: a change to the shape is
// made here, in both modules, in one commit.
//
// An event is published once into a Ring, which gives it its id, and read
// from there by every stream that asks (Frame, GapFrame, Reader). Patterns
// (Match, CheckPatterns) choose which types a sink or a stream wants.
// Sentence is how a person reads one.
package events

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// The types of event. Each is "<subject>.<what happened>", except Test.
const (
	// AccountThreshold is an account's usage crossing its threshold, up
	// (Reason "up") or back down ("down"), with Window, Percent and Limit.
	AccountThreshold = "account.threshold"
	// AccountReset is an account's window coming back after it had been
	// used, with Window, Percent (what it had reached) and Limit.
	AccountReset = "account.reset"
	// AccountDead is an account that cannot be used: its login needs doing
	// again (Reason "login"), or a session's start token ended ("token").
	AccountDead = "account.dead"
	// AccountRotated is the account first in the rotation changing; Reason
	// says why ("spent", "dead", "credit", "left", "removed", "back",
	// "order", or "none" when no account is left).
	AccountRotated = "account.rotated"
	// SessionStarted is a session listed as running. Reason is "resumed" or
	// "moved", else absent.
	SessionStarted = "session.started"
	// SessionEnded is a session's CLI ending, with Exit. Reason is "exited",
	// "stopped", "interrupted" or "moved".
	SessionEnded = "session.ended"
	// SessionAttention is a session wanting its person; Reason is
	// "permission", "question", "done", "bell", "title" or "quiet".
	SessionAttention = "session.attention"
	// SessionQuiet is a session that has printed nothing for a while.
	SessionQuiet = "session.quiet"
	// ViewerJoined is a person attaching to a session, with Viewer and the
	// way they came in as Reason.
	ViewerJoined = "viewer.joined"
	// ViewerLeft is that person no longer attached.
	ViewerLeft = "viewer.left"
	// DaemonStarted is a rota daemon starting.
	DaemonStarted = "daemon.started"
	// DaemonStopped is a rota daemon stopping: the last event it publishes.
	DaemonStopped = "daemon.stopped"
	// Test is the test button's own event. Sink names the one sink it is
	// for, and is absent when it is for every sink.
	Test = "test"

	// Gap is not a type anything publishes. A stream says it, once and
	// first, when it was asked for events older than any the ring still
	// keeps (GapFrame), and a Reader gives it as Event{Type: Gap}. No
	// pattern matches it.
	Gap = "gap"
)

// Types is every type an event can have, in the order the manual lists
// them: every constant above but Gap. It is what CheckPatterns names when it
// refuses a pattern. Nothing may change it.
var Types = []string{
	AccountThreshold, AccountReset, AccountDead, AccountRotated,
	SessionStarted, SessionEnded, SessionAttention, SessionQuiet,
	ViewerJoined, ViewerLeft,
	DaemonStarted, DaemonStopped,
	Test,
}

// Event is one thing that happened, as it travels: a JSON object whose
// first seven keys are always present and whose others are present only on
// the types that carry them.
type Event struct {
	// ID is the event's place in the stream: microseconds since the Unix
	// epoch, made strictly increasing by the Ring that published it, so it
	// also keeps rising across a daemon restart. It is what Last-Event-ID
	// and since name.
	ID int64 `json:"id"`
	// Type is one of Types, or Gap on a stream.
	Type string `json:"type"`
	// Time is when it happened. Frame writes it as RFC 3339 in UTC.
	Time time.Time `json:"time"`
	// Account is the account's id, 0 for none.
	Account int `json:"account"`
	// Session is the session's id, "" for none.
	Session string `json:"session"`
	// Name is what the subject is called: the session's name when Session
	// is set, else the account's label, else "".
	Name string `json:"name"`
	// Text is the words that follow Name in a sentence, or the whole
	// sentence when Name is "" (Sentence).
	Text string `json:"text"`

	// Reason is the kind of the thing, by type: what each type's doc
	// comment lists.
	Reason string `json:"reason,omitempty"`
	// Exit is a session.ended CLI's exit code, -1 when it was interrupted.
	Exit *int `json:"exit,omitempty"`
	// Window, Percent and Limit are an account.threshold's or an
	// account.reset's window name, its reading and the account's threshold.
	Window  string   `json:"window,omitempty"`
	Percent *float64 `json:"percent,omitempty"`
	// Limit is omitzero, since a JSON number is never "empty" to v2.
	Limit int `json:"limit,omitzero"`
	// Viewer is a viewer.* event's person.
	Viewer string `json:"viewer,omitempty"`
	// Sink is the sink a test event is for, absent when it is for all.
	Sink string `json:"sink,omitempty"`
}

// Match says whether an event of type typ is one the patterns ask for. No
// patterns asks for every event. A pattern is a type, "<prefix>.*" for every
// type under that prefix, or "*". Gap is never matched: it is a stream's
// note, not an event.
func Match(patterns []string, typ string) bool {
	if typ == Gap {
		return false
	}
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p == "*" || p == typ {
			return true
		}
		if prefix, ok := strings.CutSuffix(p, ".*"); ok && strings.HasPrefix(typ, prefix+".") {
			return true
		}
	}
	return false
}

// CheckPatterns refuses a pattern that could never match: one that is not
// "*", a type, or "<prefix>.*" for a prefix some type has. The error names
// the first such pattern and lists the types. No patterns is fine, and means
// every event.
func CheckPatterns(patterns []string) error {
	for _, p := range patterns {
		if !known(p) {
			return fmt.Errorf("on: %q names no event; the events are %s", p, strings.Join(Types, ", "))
		}
	}
	return nil
}

// known reports whether a single pattern can match some type.
func known(p string) bool {
	if p == "*" {
		return true
	}
	prefix, wild := strings.CutSuffix(p, ".*")
	for _, t := range Types {
		if t == p || (wild && prefix != "" && strings.HasPrefix(t, prefix+".")) {
			return true
		}
	}
	return false
}

// Sentence is the event as a person reads it: Name, a space, then Text. An
// event with no Name is its Text alone, and one with no Text its Name.
func Sentence(e Event) string {
	switch {
	case e.Name == "":
		return e.Text
	case e.Text == "":
		return e.Name
	}
	return e.Name + " " + e.Text
}

// Clean makes s safe to show on a terminal, a phone or a page: every C0
// control byte, DEL and C1 control becomes a space, and a byte that is not
// UTF-8 becomes U+FFFD. Then, when max is above 0 and s is longer than max
// bytes, it is cut on a rune boundary and ends with "…", all within max
// bytes. Nothing else changes: spaces are kept, even at the ends.
func Clean(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteRune(utf8.RuneError)
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			b.WriteByte(' ')
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	out := b.String()
	if max <= 0 || len(out) <= max {
		return out
	}
	const more = "…"
	if max < len(more) {
		return out[:runeStart(out, max)] // no room for the mark
	}
	return out[:runeStart(out, max-len(more))] + more
}

// runeStart is the largest index at or below i that starts a rune of s; i
// is below len(s).
func runeStart(s string, i int) int {
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}
