package client

import (
	"net/http"
	"net/url"
	"time"
)

// changeWait bounds a change. A live move is the longest thing the daemon
// does for one request, about 46 seconds on its side, and the TUI gives a
// move 100: a change outwaits both, so it is never the client that gives up
// on a move the daemon is still making (R27).
const changeWait = 2 * time.Minute

// Change is a change to a held session, and is one of three: Resume starts
// a stopped session again on its conversation, Account moves the session's
// conversation to that account, and Forget lets a stopped session's record
// go. Confirm goes with Account alone, and says yes to what the daemon said
// the move costs. Cols and Rows are the size a stopped session starts at;
// Env, which the daemon takes only over its control socket, is the
// environment it starts from in place of the daemon's, as Create's is.
// Anything left out is not sent, and the daemon refuses any other mix.
type Change struct {
	Resume  bool     `json:"resume,omitempty"`
	Account int      `json:"account,omitempty"`
	Confirm bool     `json:"confirm,omitempty"`
	Forget  bool     `json:"forget,omitempty"`
	Cols    uint16   `json:"cols,omitempty"`
	Rows    uint16   `json:"rows,omitempty"`
	Env     []string `json:"env,omitempty"`
}

// Change resumes, moves or forgets a held session, and says what it is
// after a resume or a move; a forgotten session is no session at all. A
// refusal is a *Refusal whose Reason a program can act on: a move that
// costs something is refused with confirm and the question to ask, and is
// made by sending it again with Confirm.
func (c *Client) Change(id string, ch Change) (Session, error) {
	return c.change(id, ch, changeWait)
}

// change is Change waiting as long as wait, so a test of it need not wait
// minutes.
func (c *Client) change(id string, ch Change, wait time.Duration) (Session, error) {
	path := "/v1/terminals/" + url.PathEscape(id)
	if ch.Forget {
		// The daemon answers a forget with {"ok":true}, which describes
		// nobody.
		return Session{}, c.doAs("", wait, http.MethodPatch, path, ch, nil)
	}
	var s Session
	if err := c.doAs("", wait, http.MethodPatch, path, ch, &s); err != nil {
		return Session{}, err
	}
	return s, nil
}
