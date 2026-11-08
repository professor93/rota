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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
)

// ErrNoDaemon is a store nobody serves: its daemon lock is free.
var ErrNoDaemon = errors.New("no rota daemon is running for this store")

// How long a held lock with a silent socket is tried before the daemon is
// said not to be answering, and how often within that.
const (
	dialWait = 5 * time.Second
	dialPoll = 50 * time.Millisecond
)

// requestWait bounds one request that the daemon answers from memory and
// from the store's records. holderWait bounds the two that wait on a
// holder: a create, which the daemon gives 30 seconds to answer, and a
// send, which it gives 35 to type.
const (
	requestWait = 30 * time.Second
	holderWait  = time.Minute
)

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
func Dial(terminals string) (*Client, error) { return dial(terminals, dialWait) }

// DialNow is Dial without the wait: one try, for a caller with a deadline of
// its own, such as a start waiting for the daemon it started. A held lock
// whose socket does not accept at once is the daemon not answering, at once.
func DialNow(terminals string) (*Client, error) { return dial(terminals, 0) }

// Ensure is Dial, starting a daemon first where none runs: start is called
// only when the lock is free, and the daemon it started is then dialled as
// any other is. A held lock whose socket does not answer is Dial's error,
// and start is never called: a daemon that is slow to answer is a daemon,
// and starting another beside it would make two. started says start was
// called and worked, whatever the dial after it said.
func Ensure(terminals string, start func() error) (c *Client, started bool, err error) {
	return ensure(terminals, dialWait, start)
}

// ensure is Ensure with Dial's wait for a silent socket given, so a test of
// it need not spend five seconds on every run.
func ensure(terminals string, wait time.Duration, start func() error) (c *Client, started bool, err error) {
	c, err = dial(terminals, wait)
	if !errors.Is(err, ErrNoDaemon) {
		return c, false, err
	}
	if err := start(); err != nil {
		return nil, false, err
	}
	c, err = dial(terminals, wait)
	return c, true, err
}

func dial(terminals string, wait time.Duration) (*Client, error) {
	sock := holdwire.CtlSock(terminals)
	deadline := time.Now().Add(wait)
	for {
		d, running := holdwire.RunningDaemon(terminals)
		if !running {
			return nil, ErrNoDaemon
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, err := dialOwn(ctx, sock)
		cancel()
		if err == nil {
			conn.Close()
			return newClient(sock), nil
		}
		var stranger notOwn
		if errors.As(err, &stranger) {
			// Asking again would find the same listener.
			return nil, err
		}
		if !time.Now().Before(deadline) {
			// A daemon.pid that names no process this rota can check names
			// nobody, rather than pid 0.
			who := "the rota daemon"
			if d.PID != 0 {
				who = fmt.Sprintf("the rota daemon (pid %d)", d.PID)
			}
			return nil, fmt.Errorf("%s is not answering on %s", who, sock)
		}
		time.Sleep(dialPoll)
	}
}

func newClient(sock string) *Client {
	return &Client{sock: sock, http: &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialOwn(ctx, sock)
			},
		},
	}}
}

// owner is the user whose daemon a control socket must be: the one this
// process runs as. A variable so a test can make the daemon a stranger.
var owner = os.Geteuid

// notOwn is a control socket that answered and is not this user's daemon's
// as far as the kernel can say: another user listens on it, or who does
// cannot be told.
type notOwn struct{ why string }

func (e notOwn) Error() string { return e.why }

// dialOwn connects to the control socket at sock and keeps the connection
// only when the process listening there runs as this user, as the kernel
// says, before a byte is sent: a request carries what only this user's
// daemon may have, a session's environment and its typing. The socket's
// directory is closed to everybody else, but a store in a directory another
// user can write to would let that user's socket, and a lock file this user
// cannot open, stand where the daemon's are.
func dialOwn(ctx context.Context, sock string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	uid, err := -1, errors.New("not a unix socket")
	if uc, ok := c.(*net.UnixConn); ok {
		uid, err = proc.PeerUID(uc)
	}
	switch {
	case err != nil:
		c.Close()
		return nil, notOwn{fmt.Sprintf("who listens on %s cannot be told (%v), so nothing was sent to it", sock, err)}
	case uid != owner():
		c.Close()
		return nil, notOwn{fmt.Sprintf("%s is listened on by user %d, not by this one (%d), so nothing was sent to it", sock, uid, owner())}
	}
	return c, nil
}

// Health asks the daemon whether it is serving.
func (c *Client) Health() error {
	var doc struct {
		OK bool `json:"ok"`
	}
	if err := c.do(http.MethodGet, "/v1/health", nil, &doc); err != nil {
		return err
	}
	if !doc.OK {
		return fmt.Errorf("the rota daemon on %s says it is not serving", c.sock)
	}
	return nil
}

// Session is one session as the daemon describes it: running, or stopped
// and still listed. Name is what it is called, and Title the title its CLI
// set, which changes as the CLI works. Ended is when it stopped, and nil
// while it runs; Exit is how its CLI ended, -1 for one that was interrupted,
// and nil while it runs. Account, AccountLabel and Provider are empty for a
// session that is not an account's CLI.
//
// As JSON it is the daemon's description document, the shape GET
// /v1/terminals lists a session in — the account an object, ended a bool
// with ended_at beside it — and a session read from one is written back as
// the very document it was read from, so a command's --json and the API are
// read alike.
type Session struct {
	ID           string
	Name         string
	Title        string
	Label        string
	Kind         string
	State        string
	Status       string
	Provider     string
	Account      int
	AccountLabel string
	Cwd          string
	Conversation string
	PID          int
	Attachable   bool
	Resumable    bool
	Started      time.Time
	Ended        *time.Time
	Exit         *int

	// doc is the description this was read from, which is what it is
	// written as.
	doc json.RawMessage
}

// session is a description document as the daemon writes it: ended a bool,
// with when beside it, and the account an object.
type session struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Title        string     `json:"title"`
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
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	Exit         *int       `json:"exit,omitempty"`
	Account      *account   `json:"account,omitempty"`
}

type account struct {
	ID       int    `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
}

func (w *session) session() Session {
	s := Session{
		ID: w.ID, Name: w.Name, Title: w.Title, Label: w.Label, Kind: w.Kind, State: w.State, Status: w.Status,
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

// UnmarshalJSON reads a session from a description document, and keeps the
// document.
func (s *Session) UnmarshalJSON(raw []byte) error {
	var w session
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	*s = w.session()
	s.doc = bytes.Clone(raw)
	return nil
}

// MarshalJSON writes the document the session was read from, and for one
// made otherwise a document in the same shape.
func (s Session) MarshalJSON() ([]byte, error) {
	if s.doc != nil {
		return s.doc, nil
	}
	w := session{
		ID: s.ID, Name: s.Name, Title: s.Title, Label: s.Label, Kind: s.Kind, State: s.State, Status: s.Status,
		Cwd: s.Cwd, Conversation: s.Conversation, PID: s.PID,
		Attachable: s.Attachable, Resumable: s.Resumable, Started: s.Started,
		Ended: s.Ended != nil, EndedAt: s.Ended, Exit: s.Exit,
	}
	if s.Account != 0 {
		w.Account = &account{ID: s.Account, Label: s.AccountLabel, Provider: s.Provider}
	}
	return json.Marshal(w)
}

// List is every session the daemon holds or lists, running and stopped.
func (c *Client) List() ([]Session, error) {
	var doc struct {
		Terminals []Session `json:"terminals"`
	}
	if err := c.do(http.MethodGet, "/v1/terminals", nil, &doc); err != nil {
		return nil, err
	}
	if doc.Terminals == nil {
		doc.Terminals = []Session{}
	}
	return doc.Terminals, nil
}

// Get is one session, running or stopped, by its whole id.
func (c *Client) Get(id string) (Session, error) {
	var s Session
	if err := c.do(http.MethodGet, "/v1/terminals/"+url.PathEscape(id), nil, &s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// Resolve finds the one session a person means by what they typed, among
// every session the daemon holds or lists, by Match's rule.
func (c *Client) Resolve(nameOrID string) (Session, error) {
	all, err := c.List()
	if err != nil {
		return Session{}, err
	}
	return Match(all, nameOrID)
}

// Match finds the one session among all that a person means by what they
// typed: its whole id first, running or stopped; then an id that what they
// typed begins, at least four characters of it; then a live session whose
// label, name or folder it is. More than one session at either of the last
// two is an error that names their ids, so the person can choose. So a label
// that could begin an id — beef, or cafe1 — is read as an id first: a
// session whose id it begins is the one found, and two such are an error
// even when a live session has that very label, which is then named by its
// id. None at all is an error that is ErrNoSession.
//
// The folder is what a session nobody labelled is called until its CLI
// names its conversation, and what rota run --detach told the person to
// type; it goes on finding the session after the name changes. The title
// the CLI sets finds nothing: it changes as the CLI works.
func Match(all []Session, nameOrID string) (Session, error) {
	for _, s := range all {
		if s.ID == nameOrID {
			return s, nil
		}
	}
	var begun, named []Session
	for _, s := range all {
		if len(nameOrID) >= 4 && strings.HasPrefix(s.ID, nameOrID) {
			begun = append(begun, s)
		}
		if s.State == "live" && nameOrID != "" && (s.Label == nameOrID || s.Name == nameOrID || folder(s.Cwd) == nameOrID) {
			named = append(named, s)
		}
	}
	switch {
	case len(begun) == 1:
		return begun[0], nil
	case len(begun) > 1:
		return Session{}, fmt.Errorf("%q begins the ids of %d sessions, %s; give more of the id", nameOrID, len(begun), ids(begun))
	case len(named) == 1:
		return named[0], nil
	case len(named) > 1:
		return Session{}, fmt.Errorf("%d live sessions are called %q, %s; name one by its id", len(named), nameOrID, ids(named))
	}
	return Session{}, noSession(nameOrID)
}

// folder is the last element of a session's folder, as its record names a
// session by, and "" for a session that has none.
func folder(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

// ErrNoSession is Resolve finding no session at all by what it was given,
// as against finding more than one.
var ErrNoSession = errors.New("no such session")

// noSession is ErrNoSession for one thing typed, said as that.
type noSession string

func (n noSession) Error() string {
	return fmt.Sprintf("no session is %q: it is not an id, the start of one, or the name of a live session", string(n))
}

func (noSession) Is(target error) bool { return target == ErrNoSession }

// ids is the sessions' ids, in order, for a sentence.
func ids(sessions []Session) string {
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.ID)
	}
	slices.Sort(out)
	return strings.Join(out, " and ")
}

// Create is a session to start. Account 0 is the rotation's choice, made by
// the daemon. Args go to the CLI after its own name. Cwd, Cols and Rows left
// out are the daemon's defaults, and Label names the session.
//
// Env, when set, is the environment the session starts from in place of the
// daemon's: the caller's own, from which the holder keeps whatever describes
// another terminal. RemoteControl starts it with Claude Code's Remote
// Control on, named RemoteControlName when that is set, for whichever
// account the daemon chooses: the daemon turns the account's setting on, as
// `rota run --remote-control` does, and adds Claude Code's flag. The daemon
// takes both only over its control socket.
type Create struct {
	Account    int
	Args       []string
	Cwd        string
	Cols, Rows uint16
	Label      string
	Env        []string

	RemoteControl     bool
	RemoteControlName string
}

// createDoc is a Create as the daemon reads it: remote_control is true, or
// the name to give the session.
type createDoc struct {
	Account       int      `json:"account,omitempty"`
	Args          []string `json:"args,omitempty"`
	Cwd           string   `json:"cwd,omitempty"`
	Cols          uint16   `json:"cols,omitempty"`
	Rows          uint16   `json:"rows,omitempty"`
	Label         string   `json:"label,omitempty"`
	Env           []string `json:"env,omitempty"`
	RemoteControl any      `json:"remote_control,omitempty"`
}

// Create starts a session and says what it is, once its holder is running
// the CLI.
func (c *Client) Create(r Create) (Session, error) {
	in := createDoc{Account: r.Account, Args: r.Args, Cwd: r.Cwd, Cols: r.Cols, Rows: r.Rows, Label: r.Label, Env: r.Env}
	switch {
	case r.RemoteControlName != "":
		in.RemoteControl = r.RemoteControlName
	case r.RemoteControl:
		in.RemoteControl = true
	}
	var s Session
	if err := c.do(http.MethodPost, "/v1/terminals", in, &s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// SendReq is a message to type into a held session: the text, then Enter.
// Force types it even while the CLI waits on its person, where Enter may
// approve something.
type SendReq struct {
	Text  string `json:"text"`
	Force bool   `json:"force"`
}

// Send types a message into a held session once it is ready for one. A
// refusal is a *Refusal whose Reason says why: waiting, keyboard, ended,
// not_held or timeout.
func (c *Client) Send(id string, r SendReq) error {
	return c.do(http.MethodPost, "/v1/terminals/"+url.PathEscape(id)+"/messages", r, nil)
}

// Stop ends a session: its CLI is hung up, and killed if it is still there
// a few seconds later. It returns once the daemon has begun that, not once
// the session has ended.
func (c *Client) Stop(id string) error {
	return c.do(http.MethodDelete, "/v1/terminals/"+url.PathEscape(id), nil, nil)
}

// Refusal is the daemon saying no, in its own words: the status it answered
// with, the reason a program can act on where it gave one, and the sentence
// for a person, which is its Error. Holder is who holds the keyboard, for a
// send refused because somebody does.
type Refusal struct {
	Status          int
	Reason, Message string
	Holder          string
}

func (r *Refusal) Error() string { return r.Message }

// do makes one request, with in as its JSON body when it is not nil, and
// decodes the answer into out, which may be nil. An answer that is not a
// success is a *Refusal. A request with a body is a create or a send, which
// wait on a holder, so it is given holderWait rather than requestWait.
func (c *Client) do(method, path string, in, out any) error {
	var body io.Reader
	wait := requestWait
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, wait = bytes.NewReader(raw), holderWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://rota"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
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
			Error  string `json:"error"`
			Reason string `json:"reason"`
			Holder string `json:"holder"`
		}
		if json.Unmarshal(raw, &e) != nil || strings.TrimSpace(e.Error) == "" {
			e.Error = fmt.Sprintf("the rota daemon answered %s to %s %s", resp.Status, method, path)
		}
		return &Refusal{Status: resp.StatusCode, Reason: e.Reason, Message: e.Error, Holder: e.Holder}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the rota daemon's answer to %s %s: %w", method, path, err)
	}
	return nil
}
