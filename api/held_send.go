package api

import (
	"context"
	"slices"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/share"
)

// Sending is typing a message into a held session the way a person would:
// the holder waits until the CLI is ready for one, types it, and presses
// Enter. This is the daemon's half — the frame out, and the one answer back
// to whoever asked — and POST /v1/terminals/{id}/send is its door.
//
// A send is let in under the keyboard's rule, and the holder may wait up to
// heldSendWaitMS before it types. From the moment it is let in until it is
// answered it holds the session for its person: nobody else types into it
// meanwhile (sendingFor), so the rule it was let in under is the rule it is
// typed under. Esc and Ctrl-C alone are the exception, since a person must
// always be able to stop a turn; they leave the send waiting.

var (
	// heldSendWait is how long a send waits for its holder's answer. The
	// holder is told heldSendWaitMS, and this is a little longer, so that a
	// session that never became ready is answered by the holder's own
	// timeout rather than by this one.
	heldSendWait   = 35 * time.Second
	heldSendWaitMS = 30000
)

// heldSendLetIn is called once a send has been let in, and holds its
// session, before its frame goes to the holder. It does nothing: it is where
// a test stands to act in the moment after the keyboard's rule let a send
// in, when the session must be held already.
var heldSendLetIn = func() {}

// The answers a send has that are this server's rather than its holder's.
const (
	sentKeyboard = "keyboard" // another person holds the keyboard; the Message names them
	sentUnlinked = "unlinked" // the link to the holder was down, or went while the send waited
	sentNoSend   = "nosend"   // a holder that takes no sends
	sentGone     = "gone"     // whoever asked went away
)

// sending is one send waiting for its answer, the link it went down, and
// who sent it: person, as personOf has it, and the name to say. until is
// when it stops holding the session whether or not it was answered.
type sending struct {
	seq    uint64
	answer chan holdwire.SentMsg
	conn   *share.Conn
	person string
	name   string
	until  time.Time
}

// hold is a send by person, made ready to go and holding the session for
// them from now on, before its frame goes to the holder. A session that
// cannot take a send holds nothing, and its answer is said at once.
//
// Called with the session's mu held (letSendIn), so that the keyboard rule a
// send is let in under and its hold are one step: nobody can take a free
// keyboard between the two and type the half-line its Enter would submit.
func (h *heldTerm) hold(person, name string) (*sending, holdwire.SentMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.state != heldLive:
		return nil, holdwire.SentMsg{Code: holdwire.SentEnded}
	case h.conn == nil:
		return nil, holdwire.SentMsg{Code: sentUnlinked, Message: errHeldAway.Error()}
	case !slices.Contains(h.features, holdwire.FeatSend):
		return nil, holdwire.SentMsg{Code: sentNoSend}
	}
	h.sendSeq++
	x := &sending{seq: h.sendSeq, answer: make(chan holdwire.SentMsg, 1), conn: h.conn,
		person: person, name: name, until: time.Now().Add(heldSendWait)}
	if h.sends == nil {
		h.sends = map[uint64]*sending{}
	}
	h.sends[x.seq] = x
	return x, holdwire.SentMsg{}
}

// sendText asks the holder to type text and Enter for the send x, once the
// session is ready, and waits for its answer. The CLI ending answers it at
// once, as ended, and so does the link going.
//
// One whose asker went away is still waiting in the holder, which types it
// when the session is ready: it goes on holding the session until it is
// answered, or for heldSendWait.
func (h *heldTerm) sendText(ctx context.Context, x *sending, text string, force bool) holdwire.SentMsg {
	forget := func() {
		h.mu.Lock()
		delete(h.sends, x.seq)
		h.mu.Unlock()
	}
	msg := holdwire.SendMsg{Seq: x.seq, Text: text, Force: force, WaitMS: heldSendWaitMS}
	if x.conn.SendJSON(holdwire.Send, msg) != nil {
		forget()
		return holdwire.SentMsg{Code: sentUnlinked, Message: errHeldAway.Error()}
	}
	t := time.NewTimer(heldSendWait)
	defer t.Stop()
	select {
	case m := <-x.answer:
		return m // whoever answered it has let it go
	case <-t.C:
		forget()
		return holdwire.SentMsg{Code: holdwire.SentTimeout}
	case <-ctx.Done():
		return holdwire.SentMsg{Code: sentGone}
	}
}

// sendingFor is a send waiting to be typed that is not person's, by who sent
// it and its seq: "" is nobody's, so a send by a token holds the session
// against everyone. told is the send a connection was last told of, and is
// the one given while it still waits, so that a connection is told of a
// send once rather than once a keystroke. ok is false when no other send is
// waiting. A send past its until is let go of on the way.
func (h *heldTerm) sendingFor(person string, told uint64) (name string, seq uint64, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for s, x := range h.sends {
		switch {
		case now.After(x.until):
			delete(h.sends, s)
		case x.person == "" || x.person != person:
			if !ok || s == told {
				name, seq, ok = x.name, s, true
			}
		}
	}
	return name, seq, ok
}

// sent hands the holder's answer to the send waiting for it, if one still
// is.
func (h *heldTerm) sent(msg *holdwire.SentMsg) {
	h.mu.Lock()
	x := h.sends[msg.Seq]
	delete(h.sends, msg.Seq)
	h.mu.Unlock()
	if x != nil {
		select {
		case x.answer <- *msg:
		default:
		}
	}
}

// answerSends answers with code every send still waiting that went down
// conn, or every one with conn nil. Called with h.mu held.
func (h *heldTerm) answerSends(conn *share.Conn, code string) {
	for seq, x := range h.sends {
		if conn != nil && x.conn != conn {
			continue
		}
		select {
		case x.answer <- holdwire.SentMsg{Seq: seq, Code: code}:
		default:
		}
		delete(h.sends, seq)
	}
}
