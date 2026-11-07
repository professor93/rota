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
// heldSendWaitMS before it types. Until it is answered it holds the session
// for its person: nobody else types into it meanwhile (sendingFor), so the
// rule it was let in under is the rule it is typed under.

var (
	// heldSendWait is how long a send waits for its holder's answer. The
	// holder is told heldSendWaitMS, and this is a little longer, so that a
	// session that never became ready is answered by the holder's own
	// timeout rather than by this one.
	heldSendWait   = 35 * time.Second
	heldSendWaitMS = 30000
)

// The answers a send has that are this server's rather than its holder's.
const (
	sentUnlinked = "unlinked" // the link to the holder was down, or went while the send waited
	sentNoSend   = "nosend"   // a holder that takes no sends
	sentGone     = "gone"     // whoever asked went away
)

// sending is one send waiting for its answer, the link it went down, and
// who sent it: person, as personOf has it, and the name to say. until is
// when it stops holding the session whether or not it was answered.
type sending struct {
	answer chan holdwire.SentMsg
	conn   *share.Conn
	person string
	name   string
	until  time.Time
}

// sendText asks the holder to type text and Enter once the session is
// ready, and waits for its answer. The CLI ending answers it at once, as
// ended, and so does the link going.
//
// One whose asker went away is still waiting in the holder, which types it
// when the session is ready: it goes on holding the session until it is
// answered, or for heldSendWait.
func (h *heldTerm) sendText(ctx context.Context, person, name, text string, force bool) holdwire.SentMsg {
	h.mu.Lock()
	conn := h.conn
	switch {
	case h.state != heldLive:
		h.mu.Unlock()
		return holdwire.SentMsg{Code: holdwire.SentEnded}
	case conn == nil:
		h.mu.Unlock()
		return holdwire.SentMsg{Code: sentUnlinked, Message: errHeldAway.Error()}
	case !slices.Contains(h.features, holdwire.FeatSend):
		h.mu.Unlock()
		return holdwire.SentMsg{Code: sentNoSend}
	}
	h.sendSeq++
	seq := h.sendSeq
	ch := make(chan holdwire.SentMsg, 1)
	if h.sends == nil {
		h.sends = map[uint64]*sending{}
	}
	h.sends[seq] = &sending{answer: ch, conn: conn, person: person, name: name, until: time.Now().Add(heldSendWait)}
	h.mu.Unlock()
	forget := func() {
		h.mu.Lock()
		delete(h.sends, seq)
		h.mu.Unlock()
	}
	msg := holdwire.SendMsg{Seq: seq, Text: text, Force: force, WaitMS: heldSendWaitMS}
	if conn.SendJSON(holdwire.Send, msg) != nil {
		forget()
		return holdwire.SentMsg{Code: sentUnlinked, Message: errHeldAway.Error()}
	}
	t := time.NewTimer(heldSendWait)
	defer t.Stop()
	select {
	case m := <-ch:
		return m // whoever answered it has let it go
	case <-t.C:
		forget()
		return holdwire.SentMsg{Code: holdwire.SentTimeout}
	case <-ctx.Done():
		return holdwire.SentMsg{Code: sentGone}
	}
}

// sendingFor is who sent a send that is waiting to be typed, when it is not
// person's: "" is nobody's, so a send by a token holds the session against
// everyone. ok is false when no other send is waiting. A send past its
// until is let go of on the way.
func (h *heldTerm) sendingFor(person string) (name string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for seq, x := range h.sends {
		switch {
		case now.After(x.until):
			delete(h.sends, seq)
		case x.person == "" || x.person != person:
			return x.name, true
		}
	}
	return "", false
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
