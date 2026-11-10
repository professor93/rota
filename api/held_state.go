package api

import (
	"github.com/professor93/rota/internal/vtscan"
	rota "github.com/professor93/rota/lib"
)

// A held session says how it is in a frame of its own, the state frame:
//
//	{"type":"state","status":"running|needs_you|quiet","waiting_for":"…","title":"…","name":"…"}
//
// Each viewer is sent it after its hello, and everyone attached is sent it
// again whenever any of it changes, so a page knows the moment the CLI
// starts waiting on its person rather than at its next poll. It says the
// same as the description's status, waiting_for, title and name. A session
// that has stopped says so with its exit frame instead, and a terminal that
// is not held has no state frame at all.
//
// tellState is the one place a held session's state is announced.

// stateDoc is the state frame, or nil for a terminal that is not held or a
// held one that is no longer live. Called with ts.mu held: the lock order is
// ts.mu and then h.mu, as describe has it.
func (ts *termSession) stateDoc() map[string]any {
	h, ok := ts.back.(*heldTerm)
	if !ok {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != heldLive {
		return nil
	}
	return map[string]any{
		"type": "state", "status": h.status(), "waiting_for": h.reg.WaitingFor,
		"title": vtscan.CleanTitle(h.title), "name": h.name(),
	}
}

// tellState tells everyone attached the session's state, when it differs
// from what they were last told. Called without ts.mu or h.mu held.
func (ts *termSession) tellState() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.sayState(nil)
}

// sayState tells everyone attached the state if it changed since it was
// last told, and otherwise tells it to one connection alone, if one is
// given: a viewer that has just attached. Called with ts.mu held.
func (ts *termSession) sayState(to *termConn) {
	doc := ts.stateDoc()
	if doc == nil {
		return
	}
	raw, err := rota.Encode(doc)
	if err != nil {
		return
	}
	h := ts.back.(*heldTerm)
	if string(raw) != h.said {
		h.said = string(raw)
		for _, tc := range ts.conns {
			tc.push(opText, raw)
		}
		return
	}
	if to != nil {
		to.push(opText, raw)
	}
}
