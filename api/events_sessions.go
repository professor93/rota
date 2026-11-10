package api

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/professor93/rota/internal/events"
	rota "github.com/professor93/rota/lib"
)

// What a session says has happened to it: that it started, that it ended,
// why and after how long, that a person attached to it or left it, and that
// the token it runs on ended. Each is said once, from the one place it
// happens: the paths that list a new running session (sessionStarted),
// finish (sessionEnded), attach and detach (viewerCame, viewerWent), and the
// terminals' sweep (tokensEnded).
//
// A session is named by eventName: what the person who started it called
// it, else its folder, else its id, and never what its CLI called its
// conversation. The words follow that name in a sentence (events.Sentence).
// A session this server lets go of, still running, says nothing: the next
// daemon takes it up, and says nothing of it either until it ends.

// Why a session started or ended: session.started's reason, which is absent
// for a session started the ordinary way, and session.ended's.
const (
	whyExited      = "exited"
	whyStopped     = "stopped"
	whyInterrupted = "interrupted"
	whyResumed     = "resumed"
	whyMoved       = "moved"
)

// byIdle is who stops a terminal that nobody has been attached to for the
// idle timeout (sweepTerminals).
const byIdle = "idle"

// viewerSettle is how long a person who left a session has to come back
// before they are said to have left it, so a reconnect — a laptop lid, a
// refreshed tab, a phone between networks — says nothing.
var viewerSettle = 10 * time.Second

// presence is one person attached to one session: how many connections
// they have open, and the timer that says they left once the last of them
// has been gone for viewerSettle. It belongs to the session's mu.
type presence struct {
	n     int
	timer *time.Timer
}

/* ------------------------------------------------------------- started --- */

// sessionStarted says ts has started, as by started it, for why, in text,
// or in its own words when text is "". Every path that lists a new running
// session calls it once, after listing it and before it runs; why is "", or
// whyResumed or whyMoved. A session taken up from a daemon before is no new
// session, and never comes here.
func (s *Server) sessionStarted(ts *termSession, by, why, text string) {
	if text == "" {
		text = ts.startedWords(by, why)
	}
	s.emit(events.Event{Type: events.SessionStarted, Account: ts.account, Session: ts.id,
		Name: ts.eventName(), Reason: why, Text: text})
}

// startedWords is what session.started says when it is given no words:
// "started by alice on work@x.com (claude) in app".
func (ts *termSession) startedWords(by, why string) string {
	on, in := ts.accountWords(), ""
	if f := ts.folder(); f != "" {
		in = " in " + f
	}
	switch {
	case why == whyResumed:
		if on == "" {
			return "was resumed" + in
		}
		return "was resumed on " + on + in
	case why == whyMoved:
		if on == "" {
			return "was moved" + in
		}
		return "moved to " + on
	case ts.kind == termKindShared:
		return "is shared from a terminal on this machine"
	case ts.kind == termKindShell:
		if namedBy(by) {
			return "started by " + by + " as a shell" + in
		}
		return "started a shell" + in
	}
	words := "started"
	if namedBy(by) {
		words += " by " + by
	}
	if on != "" {
		words += " on " + on
	}
	return words + in
}

/* --------------------------------------------------------------- ended --- */

// endingAs says how ts is about to end, before it is hung up for it: a move
// says moved rather than stopped. text "" is the reason's own words.
func (ts *termSession) endingAs(why, text string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.endWhy, ts.endText = why, text
}

// sessionEnded says ts has ended with code, why and after how long. finish
// calls it once, after the session reads ended. Nobody is said to leave a
// session that has ended, so the viewers' timers go with it.
//
// The reason is the one endingAs gave; else interrupted, for a holder that
// went without its CLI's exit; else stopped, for one somebody stopped; else
// exited.
func (ts *termSession) sessionEnded(code int) {
	ts.mu.Lock()
	why, text, by := ts.endWhy, ts.endText, ts.stoppedBy
	for _, p := range ts.present {
		if p.timer != nil {
			p.timer.Stop()
		}
	}
	ts.present = nil
	ts.mu.Unlock()
	if why == "" {
		switch {
		case ts.interrupted():
			why = whyInterrupted
		case by != "":
			why = whyStopped
		default:
			why = whyExited
		}
	}
	if why == whyInterrupted {
		code = -1
	}
	if text == "" {
		text = ts.endedWords(why, by, code, ts.s.now().Sub(ts.started))
	}
	ts.s.emit(events.Event{Type: events.SessionEnded, Account: ts.account, Session: ts.id,
		Name: ts.eventName(), Reason: why, Exit: &code, Text: text})
}

// interrupted reports whether ts is a held session whose holder went away
// without its CLI's exit.
func (ts *termSession) interrupted() bool {
	h, ok := ts.back.(*heldTerm)
	if !ok {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state == heldInterrupted
}

// endedWords is what session.ended says when it is given no words: "ended
// with exit 0 after 1h 3m". A stop names who stopped it, unless that is
// this machine's own user; one for being left alone says for how long.
func (ts *termSession) endedWords(why, by string, code int, age time.Duration) string {
	after := sessionFor(age)
	switch why {
	case whyInterrupted:
		return "was interrupted after " + after + ": its holder went away"
	case whyMoved:
		return "was moved after " + after
	case whyStopped:
		switch {
		case by == byIdle:
			alone := ts.s.opts.Terminal.IdleTimeout
			if alone <= 0 {
				alone = age
			}
			return "was stopped after " + sessionFor(alone) + " with nobody attached"
		case namedBy(by):
			return fmt.Sprintf("was stopped by %s after %s (exit %d)", by, after, code)
		}
		return fmt.Sprintf("was stopped after %s (exit %d)", after, code)
	}
	return fmt.Sprintf("ended with exit %d after %s", code, after)
}

/* ------------------------------------------------------------- viewers --- */

// viewerCame is tc having attached to ts. A person is said to join when
// their first connection comes, and not again for another, nor for one that
// comes back within viewerSettle of their last going. This machine's own
// user, on the control socket, is never said, and nobody joins a session
// that has ended: it stays listed for a while, and replays. Called with
// ts.mu held.
func (ts *termSession) viewerCame(tc *termConn) {
	if tc.via == "local" || ts.ended {
		return
	}
	key := tc.via + "\x00" + tc.name
	p := ts.present[key]
	if p == nil {
		if ts.present == nil {
			ts.present = map[string]*presence{}
		}
		p = &presence{}
		ts.present[key] = p
		to := "control"
		if tc.watch || ts.lookOnly || !tc.role.allows(RoleControl) {
			to = "watch"
		}
		ts.s.emit(events.Event{Type: events.ViewerJoined, Account: ts.account, Session: ts.id,
			Name: ts.eventName(), Reason: tc.via, Viewer: tc.name,
			Text: "has " + tc.name + " attached, to " + to})
	}
	p.n++
	if p.timer != nil {
		p.timer.Stop() // back within the settle
		p.timer = nil
	}
}

// viewerWent is tc having left ts. Once a person's last connection has gone,
// they are said to have left viewerSettle later, unless they came back
// meanwhile or the session ended. Called with ts.mu held.
func (ts *termSession) viewerWent(tc *termConn) {
	if tc.via == "local" {
		return
	}
	key := tc.via + "\x00" + tc.name
	p := ts.present[key]
	if p == nil || p.n == 0 {
		return
	}
	if p.n--; p.n > 0 {
		return
	}
	if ts.ended {
		delete(ts.present, key)
		return
	}
	via, name := tc.via, tc.name
	var t *time.Timer
	t = time.AfterFunc(viewerSettle, func() {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		// Still this timer's, and still gone: one that was stopped too late
		// finds the person back, or another timer in its place.
		if ts.present[key] != p || p.timer != t || p.n != 0 {
			return
		}
		delete(ts.present, key)
		if !ts.ended {
			ts.s.emit(events.Event{Type: events.ViewerLeft, Account: ts.account, Session: ts.id,
				Name: ts.eventName(), Reason: via, Viewer: name,
				Text: "no longer has " + name + " attached"})
		}
	})
	p.timer = t
}

/* --------------------------------------------------------------- tokens --- */

// tokensEnded says, once for each live session, that the token it was
// started with has ended: account.dead with reason token, named by the
// account and saying which session can no longer reach it. A claude session
// on its account's stored login says its login ended instead. The terminals'
// sweep calls it, whether or not it ends idle terminals.
func (s *Server) tokensEnded(now time.Time) {
	for _, ts := range s.everyTerminal() {
		ts.mu.Lock()
		due := !ts.tokenSaid && !ts.ended && !ts.left && !ts.tokenUntil.IsZero() && !now.Before(ts.tokenUntil)
		if due {
			ts.tokenSaid = true
		}
		ts.mu.Unlock()
		if !due {
			continue
		}
		at := ts.tokenUntil.UTC().Format("15:04") + " UTC"
		what := "the token it was started with ended at " + at
		if rota.Flavor(ts.provider) == "claude" && !ts.onToken {
			what = "its login ended at " + at
		}
		s.emit(events.Event{Type: events.AccountDead, Account: ts.account, Session: ts.id,
			Name: ts.accountName(), Reason: "token",
			Text: "can no longer be reached from " + ts.eventName() + ": " + what})
	}
}

// onTokenFor reports whether a claude session on account a runs on a token
// rather than on a's stored login, from until, the time its credential
// lasts: a session on the stored login lasts as long as the login does
// (holdwire.LoginUntil), and one on a token as long as the token. It is the
// one rule for both kinds of session, since a holder says only until. A
// session of any other flavor is on its token whatever this says.
func onTokenFor(a *rota.Account, until time.Time) bool {
	if a == nil || until.IsZero() || rota.Flavor(a.Provider) != "claude" {
		return false
	}
	return !until.Equal(a.LoginUntil())
}

/* ------------------------------------------------------------- words --- */

// accountWords is the account a session runs on, as its start says it:
// "work@x.com (claude)", and "" for none.
func (ts *termSession) accountWords() string {
	if ts.account == 0 && ts.label == "" {
		return ""
	}
	on := ts.accountName()
	if ts.provider != "" {
		on += " (" + ts.provider + ")"
	}
	return on
}

// accountName is the account a session runs on by its label, or by its id
// when its label is not known.
func (ts *termSession) accountName() string {
	if ts.label != "" {
		return ts.label
	}
	return fmt.Sprintf("account %d", ts.account)
}

// folder is the base name of the session's folder, never the path, and ""
// for none.
func (ts *termSession) folder() string {
	if ts.cwd == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(ts.cwd))
	if base == "." || base == string(filepath.Separator) || base == "/" {
		return ""
	}
	return base
}

// namedBy reports whether by is somebody a sentence names: not nobody, and not
// this machine's own user, who is not a name anybody reads.
func namedBy(by string) bool { return by != "" && by != localName }

// sessionFor is a duration as a session's words say it, cut to its two
// largest units: 45s, 12m, 1h 3m, 2h, 2d 4h, 2d.
func sessionFor(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	}
	days, h := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, h)
}
