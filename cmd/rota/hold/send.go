//go:build linux || darwin

package hold

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// Sending is typing a message into the CLI the way a person would, once it
// is ready for one: the text as a paste where the CLI has bracketed paste
// on, then Enter on its own a moment later, because a CLI that reads a paste
// and its Enter in one read may take the Enter as part of the paste.
//
// Ready means what the CLI's own registry says, when there is one: idle and
// not waiting on the person. While it waits on the person, for a permission
// or a question, Enter may approve something, so a send is refused unless it
// is forced; forced, that wait counts as ready. With no registry, ready is
// the first quiet after output. rota never reads the CLI's screen to decide.

// The sentences a send is answered with.
const (
	sentEnded   = "the session ended before the text was typed"
	sentTimeout = "the session did not become ready for the text in time"
	sentWaiting = "the session is waiting for the person (%s), where Enter may approve something; forcing the send types it anyway"
)

// sendPoll is how often a waiting send looks again.
const sendPoll = 20 * time.Millisecond

// sending is one send, from its frame to its one answer.
type sending struct {
	l    *link
	seq  uint64
	once sync.Once
}

// answer says how the send ended, once: whichever of the send and the
// CLI's exit gets there first is the answer.
func (x *sending) answer(code, msg string) {
	x.once.Do(func() {
		_ = x.l.sendJSON(holdwire.Sent, holdwire.SentMsg{Seq: x.seq, Code: code, Message: msg})
	})
}

// enlist counts a send as waiting, so that the CLI's exit answers it. Once
// the CLI has exited it is answered at once, and nil says so.
func (s *Session) enlist(l *link, seq uint64) *sending {
	x := &sending{l: l, seq: seq}
	s.mu.Lock()
	ending := s.ending
	if !ending {
		s.sending[x] = struct{}{}
	}
	s.mu.Unlock()
	if ending {
		x.answer(holdwire.SentEnded, sentEnded)
		return nil
	}
	return x
}

// send waits until the session is ready, types m's text and Enter, and
// answers. A daemon that goes away while the send waits takes it with it;
// one that goes while it types does not, since text typed without its Enter
// would be left sitting in the CLI's prompt.
func (s *Session) send(x *sending, m holdwire.SendMsg) {
	defer s.recovered("a send")
	defer func() {
		s.mu.Lock()
		delete(s.sending, x)
		s.mu.Unlock()
	}()
	wait := sendWait
	if m.WaitMS > 0 {
		wait = time.Duration(m.WaitMS) * time.Millisecond
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	// One at a time: two texts typed together would interleave.
	select {
	case s.turn <- struct{}{}:
		defer func() { <-s.turn }()
	case <-deadline.C:
		x.answer(holdwire.SentTimeout, sentTimeout)
		return
	case <-s.exited:
		return // the exit answers it
	case <-x.l.gone:
		return
	}
	tick := time.NewTicker(sendPoll)
	defer tick.Stop()
	for {
		ready, waitingFor := s.ready(m.Force)
		if waitingFor != "" {
			x.answer(holdwire.SentWaiting, fmt.Sprintf(sentWaiting, waitingFor))
			return
		}
		if ready {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			x.answer(holdwire.SentTimeout, sentTimeout)
			return
		case <-s.exited:
			return
		case <-x.l.gone:
			return
		}
	}
	s.mu.Lock()
	bracketed := s.modes.BracketedPaste
	s.mu.Unlock()
	text := unbracket(m.Text)
	if bracketed {
		text = "\x1b[200~" + text + "\x1b[201~"
	}
	if !s.typed([]byte(text)) {
		return
	}
	select {
	case <-time.After(enterAfter):
	case <-s.exited:
		return
	}
	if !s.typed([]byte("\r")) {
		return
	}
	x.answer(holdwire.SentOK, "")
}

// ready says whether a send may type now, or what the CLI waits on the
// person for, when that refuses it.
func (s *Session) ready(force bool) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.reg; r != nil {
		if r.WaitingFor != "" {
			if force {
				return true, ""
			}
			return false, r.WaitingFor
		}
		return r.Status == "idle", ""
	}
	return time.Since(s.lastOut) >= quietFor, ""
}

// typed writes p through the writer and waits until it is written, or the
// CLI is gone.
func (s *Session) typed(p []byte) bool {
	select {
	case <-s.w.paste(p):
		return true
	case <-s.exited:
		return false
	}
}

// unbracket takes the paste markers out of text, so that it cannot end the
// paste it is put in and go on as keystrokes. Taking one out can join the
// halves of another, so it repeats until none is left.
func unbracket(text string) string {
	for {
		t := strings.ReplaceAll(strings.ReplaceAll(text, "\x1b[200~", ""), "\x1b[201~", "")
		if t == text {
			return t
		}
		text = t
	}
}
