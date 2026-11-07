// Package client talks to a store's rota daemon over its control socket,
// ctl.sock, as this machine's own user: the one way every command on this
// machine reaches the sessions the daemon holds. `rota serve status` and
// `rota serve stop --all` use it, and so do `rota run --detach`, `rota send`,
// the SSH server and the TUI.
//
// Whether a daemon exists is the lock's to say, never a connect's: a socket
// file can outlive the daemon that made it, and a daemon that holds the
// lock can be slow to answer. So no daemon is a free lock, and a held lock
// whose socket will not answer is a daemon that is not answering — said as
// that, with its pid, and never taken for no daemon at all, which would
// have a caller start a second one.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// ErrNoDaemon is a store nobody serves: its daemon lock is free.
var ErrNoDaemon = errors.New("no rota daemon is running for this store")

// How long a held lock with a silent socket is tried before the daemon is
// said not to be answering, and how often within that.
const (
	dialWait = 5 * time.Second
	dialPoll = 50 * time.Millisecond
)

// requestWait bounds one request. The daemon answers from memory and from
// the store's records; nothing a client asks for here waits on a CLI.
const requestWait = 30 * time.Second

// maxBody bounds what is read of one answer: a listing of a few hundred
// sessions is well under it.
const maxBody = 8 << 20

// Client is a connection to one store's daemon. Each request opens a
// connection of its own, so a client left idle holds nothing open.
type Client struct {
	sock string
	http *http.Client
}

// Dial finds the daemon serving the store whose terminals directory this
// is. A free lock is ErrNoDaemon at once. A held lock is tried until its
// socket accepts a connection, for up to five seconds, and is then the
// daemon not answering; a lock let go of meanwhile is ErrNoDaemon after all.
func Dial(terminals string) (*Client, error) {
	sock := holdwire.CtlSock(terminals)
	deadline := time.Now().Add(dialWait)
	for {
		d, running := holdwire.RunningDaemon(terminals)
		if !running {
			return nil, ErrNoDaemon
		}
		conn, err := net.DialTimeout("unix", sock, time.Second)
		if err == nil {
			conn.Close()
			return newClient(sock), nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the rota daemon (pid %d) is not answering on %s", d.PID, sock)
		}
		time.Sleep(dialPoll)
	}
}

func newClient(sock string) *Client {
	return &Client{sock: sock, http: &http.Client{
		Timeout: requestWait,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}}
}

// Health asks the daemon whether it is serving.
func (c *Client) Health() error {
	var doc struct {
		OK bool `json:"ok"`
	}
	if err := c.do(http.MethodGet, "/v1/health", &doc); err != nil {
		return err
	}
	if !doc.OK {
		return fmt.Errorf("the rota daemon on %s says it is not serving", c.sock)
	}
	return nil
}

// Session is one session as the daemon describes it: running, or stopped
// and still listed. Ended is when it stopped, and nil while it runs; Exit is
// how its CLI ended, -1 for one that was interrupted, and nil while it runs.
// Account, AccountLabel and Provider are empty for a session that is not an
// account's CLI.
type Session struct {
	ID, Name, Label, Kind, State, Status, Provider, AccountLabel, Cwd, Conversation string

	Account, PID          int
	Attachable, Resumable bool
	Started               time.Time
	Ended                 *time.Time
	Exit                  *int
}

// session is a description document as the daemon writes it: ended a bool,
// with when beside it, and the account an object.
type session struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Label        string     `json:"label"`
	Kind         string     `json:"kind"`
	State        string     `json:"state"`
	Status       string     `json:"status"`
	Cwd          string     `json:"cwd"`
	Conversation string     `json:"conversation"`
	PID          int        `json:"pid"`
	Attachable   bool       `json:"attachable"`
	Resumable    bool       `json:"resumable"`
	Started      time.Time  `json:"started"`
	Ended        bool       `json:"ended"`
	EndedAt      *time.Time `json:"ended_at"`
	Exit         *int       `json:"exit"`
	Account      *struct {
		ID       int    `json:"id"`
		Label    string `json:"label"`
		Provider string `json:"provider"`
	} `json:"account"`
}

func (w *session) session() Session {
	s := Session{
		ID: w.ID, Name: w.Name, Label: w.Label, Kind: w.Kind, State: w.State, Status: w.Status,
		Cwd: w.Cwd, Conversation: w.Conversation, PID: w.PID,
		Attachable: w.Attachable, Resumable: w.Resumable, Started: w.Started,
	}
	if a := w.Account; a != nil {
		s.Account, s.AccountLabel, s.Provider = a.ID, a.Label, a.Provider
	}
	if w.Ended {
		// A stopped session whose moment is not known is still stopped.
		var at time.Time
		if w.EndedAt != nil {
			at = *w.EndedAt
		}
		s.Ended, s.Exit = &at, w.Exit
	}
	return s
}

// List is every session the daemon holds or lists, running and stopped.
func (c *Client) List() ([]Session, error) {
	var doc struct {
		Terminals []session `json:"terminals"`
	}
	if err := c.do(http.MethodGet, "/v1/terminals", &doc); err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(doc.Terminals))
	for i := range doc.Terminals {
		out = append(out, doc.Terminals[i].session())
	}
	return out, nil
}

// Stop ends a session: its CLI is hung up, and killed if it is still there
// a few seconds later. It returns once the daemon has begun that, not once
// the session has ended.
func (c *Client) Stop(id string) error {
	return c.do(http.MethodDelete, "/v1/terminals/"+url.PathEscape(id), nil)
}

// refusal is the daemon saying no, in its own words.
type refusal struct {
	status int
	msg    string
}

func (r *refusal) Error() string { return r.msg }

// do makes one request and decodes the answer into out, which may be nil.
// An answer that is not a success is the daemon's own message.
func (c *Client) do(method, path string, out any) error {
	req, err := http.NewRequest(method, "http://rota"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the rota daemon on %s: %w", c.sock, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("the rota daemon on %s: %w", c.sock, err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) != nil || strings.TrimSpace(e.Error) == "" {
			e.Error = fmt.Sprintf("the rota daemon answered %s to %s %s", resp.Status, method, path)
		}
		return &refusal{status: resp.StatusCode, msg: e.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the rota daemon's answer to %s %s: %w", method, path, err)
	}
	return nil
}
