package api

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/professor93/rota/internal/pty"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/rotation"
	"github.com/professor93/rota/store"
)

// The doors into a terminal: five routes and one socket protocol.
//
// Everything that reads is watch and everything that changes is control, as
// everywhere else here — with the one twist that attaching is reading. A
// watcher may open the socket and is sent every byte; what it sends back is
// refused, in the one place that writes to the terminal, by the identity of
// the connection rather than by anything the page chose to draw.

// termRequest is what starts a terminal.
type termRequest struct {
	// Account names which account's CLI to run. Left out, the rotation
	// chooses, exactly as a run with no account does.
	Account int `json:"account,omitempty"`
	// Kind is "shell" for the person's login shell instead of a CLI, which
	// this server refuses unless the file asked for it.
	Kind string `json:"kind,omitempty"`
	// Args are handed to the CLI verbatim, after its own name.
	Args  []string `json:"args,omitempty"`
	Cwd   string   `json:"cwd,omitempty"`
	Cols  uint16   `json:"cols,omitempty"`
	Rows  uint16   `json:"rows,omitempty"`
	Label string   `json:"label,omitempty"`
}

// The refusals this file answers with, as constants because they are part of
// what this server says and a test reads them.
const (
	shellOff  = "this server runs accounts' CLIs only; a plain shell needs terminal.shell = true in the server file"
	noTermFor = "a terminal needs a pseudo-terminal, and this machine has none"
)

/* --------------------------------------------------------- the endpoints --- */

func (s *Server) listTerminals(w http.ResponseWriter, _ *http.Request) {
	terms := s.everyTerminal()
	out := make([]map[string]any, 0, len(terms))
	for _, ts := range terms {
		out = append(out, ts.view())
	}
	writeJSON(w, http.StatusOK, map[string]any{"terminals": out})
}

// terminalOf finds the terminal the path names, answering for an id nobody
// knows.
func (s *Server) terminalOf(w http.ResponseWriter, r *http.Request) (*termSession, bool) {
	id := r.PathValue("id")
	ts := s.findTerminal(id)
	if ts == nil {
		fail(w, http.StatusNotFound, "no terminal "+id)
		return nil, false
	}
	return ts, true
}

func (s *Server) describeTerminal(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.terminalOf(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, ts.view())
}

// createTerminal starts one. The answer is the description, so the client has
// the id it will attach by before anything has been printed.
func (s *Server) createTerminal(w http.ResponseWriter, r *http.Request) {
	var req termRequest
	if err := strictJSON(r.Body, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ts, err := s.startTerminal(r, &req)
	if err != nil {
		s.report(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ts.view())
}

// killTerminal ends one: a hangup, which is what closing a terminal window
// sends, and the signal nothing survives three seconds later.
func (s *Server) killTerminal(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.terminalOf(w, r)
	if !ok {
		return
	}
	if ts.over() {
		termGone(w, ts)
		return
	}
	ts.kill(who(r).Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ------------------------------------------------------------ starting it --- */

// startTerminal does everything between the request and a process on a
// pseudo-terminal: the limits, the directory, the account's own environment,
// and the recording.
func (s *Server) startTerminal(r *http.Request, req *termRequest) (*termSession, error) {
	if !pty.Supported {
		return nil, refuse(http.StatusNotImplemented, noTermFor)
	}
	kind := termKindAccount
	switch req.Kind {
	case "", termKindAccount:
	case termKindShell:
		kind = termKindShell
	default:
		return nil, refuse(http.StatusBadRequest, `kind is "account" or "shell"`)
	}
	if kind == termKindShell && req.Account != 0 {
		return nil, refuse(http.StatusBadRequest, "a shell runs no account: name one or the other, not both")
	}
	if n := s.liveTerminals(); n >= s.opts.Terminal.MaxSessions {
		return nil, refuse(http.StatusConflict, fmt.Sprintf(
			"this server is holding %d terminals, which is terminal.max_sessions; end one before starting another", n))
	}
	cwd, err := s.terminalCwd(req.Cwd)
	if err != nil {
		return nil, err
	}
	plan, err := s.planTerminal(r, kind, req)
	if err != nil {
		return nil, err
	}
	cols, rows := boundSize(req.Cols, req.Rows)
	cmd := exec.Command(plan.path, plan.args...)
	// argv[0] is the CLI's own name rather than the path it was found at,
	// which is what the handover does and what a CLI prints in its own help.
	cmd.Args = append([]string{filepath.Base(plan.path)}, plan.args...)
	cmd.Env = plan.env
	cmd.Dir = cwd
	master, err := pty.Start(cmd, cols, rows)
	if err != nil {
		// Nothing started, so nothing is holding the account.
		plan.release()
		return nil, err
	}
	ts := &termSession{
		s: s, id: runID(), log: s.log.Info,
		kind: kind, account: plan.account, label: plan.label, provider: plan.provider,
		name: strings.TrimSpace(req.Label), cwd: cwd, started: s.now(),
		tokenUntil: plan.tokenUntil,
		master:     master, proc: cmd.Process, release: plan.release,
		cols: cols, rows: rows, ring: newOutRing(s.opts.Terminal.Scrollback),
	}
	ts.idleAt = ts.started
	if s.opts.Terminal.Record {
		if rec, err := s.recorderFor(ts.id); err != nil {
			s.log.Warn("this terminal is not being recorded", "terminal", ts.id, "err", err)
		} else {
			ts.rec = rec
		}
	}
	s.addTerminal(ts)
	ts.audit("terminal created", "kind", kind, "account", plan.account, "by", who(r).Name)
	go ts.run(cmd)
	return ts, nil
}

// termPlan is what to run and as whom: the executable, its arguments, the
// environment it is given, and the claim on the account that must be let go
// of when the process ends.
type termPlan struct {
	path    string
	args    []string
	env     []string
	release func()

	account    int
	label      string
	provider   string
	tokenUntil time.Time
}

// planTerminal builds that, which for an account means the whole of the CLI
// handover: the token refreshed or the long-lived one used, the credential
// staged, the account's own Claude world mirrored. It is st.Prepare because
// there is exactly one right order for those steps and one copy of it.
func (s *Server) planTerminal(r *http.Request, kind string, req *termRequest) (*termPlan, error) {
	if kind == termKindShell {
		return s.planShell(req)
	}
	st, err := s.openStore()
	if err != nil {
		s.log.Error("opening the store", "err", err)
		return nil, refuse(http.StatusInternalServerError, "the account store could not be opened")
	}
	defer st.Close()
	rotation.Backfill(st)
	a, err := rotation.Choose(r.Context(), st, req.Account)
	if err != nil {
		return nil, err
	}
	if a.Dead && !a.LongValid() {
		return nil, refuse(http.StatusConflict, "account "+strconv.Itoa(a.ID)+" needs re-auth")
	}
	path, env, release, err := st.Prepare(r.Context(), a)
	if err != nil {
		return nil, err
	}
	// Everything the store had to say is on disk; let go of its lock before
	// a process that may run for hours starts, exactly as the handover does.
	_ = st.Close()
	return &termPlan{
		path: path, args: append([]string{}, req.Args...), env: termEnv(env), release: release,
		account: a.ID, label: a.Label(), provider: a.Provider, tokenUntil: loginUntil(a),
	}, nil
}

// planShell is the opt-in: the person's own login shell, with the server's
// environment minus the secrets no child may inherit. It is refused unless
// the file asked for it, because a terminal that runs an account's CLI is
// what this feature is and a general remote shell is a different offer.
func (s *Server) planShell(req *termRequest) (*termPlan, error) {
	if !s.opts.Terminal.Shell {
		return nil, refuse(http.StatusForbidden, shellOff)
	}
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	path, err := exec.LookPath(sh)
	if err != nil {
		return nil, refuse(http.StatusInternalServerError, "no shell could be found to run")
	}
	// A login shell, so the person's own profile is read and the terminal
	// behaves like one they opened themselves.
	args := append([]string{"-l"}, req.Args...)
	return &termPlan{path: path, args: args, env: termEnv(store.HostEnv()), release: func() {}}, nil
}

// termEnv adds what a program at a terminal expects to be told. Both are set
// rather than appended: which of two entries with the same name a program
// reads is not something to leave to whichever C library it was linked with.
func termEnv(env []string) []string {
	env = setEnv(env, "TERM", "xterm-256color")
	return setEnv(env, "COLORTERM", "truecolor")
}

func setEnv(env []string, name, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); k != name {
			out = append(out, e)
		}
	}
	return append(out, name+"="+value)
}

// loginUntil is when the credential inside this terminal stops working: the
// long-lived token's date where the account has one, because that is what a
// launch uses, and the access token's expiry otherwise.
func loginUntil(a *rota.Account) time.Time {
	if a.LongValid() {
		return a.LongUntil()
	}
	if a.Token.ExpiresAt != 0 {
		return time.UnixMilli(a.Token.ExpiresAt)
	}
	return time.Time{}
}

// terminalCwd resolves and confines the directory a terminal starts in, by
// the same function and with the same refusals as a run's cwd. A request
// that names none starts where a run would.
func (s *Server) terminalCwd(in string) (string, error) {
	if in == "" {
		if len(s.opts.Roots) > 0 {
			return s.opts.Roots[0], nil
		}
		return "", nil
	}
	return rota.CheckCwd(in, &rota.Limits{Roots: s.opts.Roots})
}

// recorderFor opens the files a recorded terminal writes, under the store,
// where nothing a CLI can reach may read them.
func (s *Server) recorderFor(id string) (*recorder, error) {
	st, err := s.openStore()
	if err != nil {
		return nil, err
	}
	dir, err := st.TerminalDir()
	st.Close()
	if err != nil {
		return nil, err
	}
	return newRecorder(dir, id, s.opts.Terminal.RecordMax)
}

/* ------------------------------------------------------------- the socket --- */

// terminalWS attaches one connection to a terminal.
//
// The mode is what a control principal asks for when it wants to look
// without taking the keyboard from whoever has it — over somebody's
// shoulder, so to speak. A watcher never has one: watch is what it is
// whatever the query string says.
func (s *Server) terminalWS(w http.ResponseWriter, r *http.Request) {
	ts, ok := s.terminalOf(w, r)
	if !ok {
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode != "" && mode != "watch" && mode != "control" {
		fail(w, http.StatusBadRequest, `mode is "watch" or "control"`)
		return
	}
	since, all, ok := termSince(w, r)
	if !ok {
		return
	}
	p := who(r)
	watch := mode == "watch" || !p.Role.allows(RoleControl)
	c, err := upgradeWS(w, r)
	if err != nil {
		return
	}
	s.serveTerminal(c, ts, p, watch, since, all)
}

// termSince is where a client says it got to, as an absolute byte offset.
// Saying nothing means everything still kept, which is what a page opening
// a terminal for the first time wants.
func termSince(w http.ResponseWriter, r *http.Request) (since int64, all bool, ok bool) {
	v := r.URL.Query().Get("since")
	if v == "" {
		return 0, true, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		fail(w, http.StatusBadRequest, "since must be a byte offset")
		return 0, false, false
	}
	return n, false, true
}

// serveTerminal carries one connection until it, the terminal or the server
// stops.
func (s *Server) serveTerminal(c *wsConn, ts *termSession, p *Principal, watch bool, since int64, all bool) {
	tc := newTermConn(c, p, watch, s.now())
	go tc.write()
	ts.attach(tc, since, all)
	reads := make(chan inFrame, 8)
	go c.readFrames(reads)
	t := time.NewTicker(c.hb)
	defer t.Stop()
	for {
		select {
		case f, open := <-reads:
			if !open {
				// The client went away, or said something the protocol does
				// not allow.
				ts.detach(tc)
				tc.settle()
				code := uint16(wsNormal)
				if c.late.Load() {
					code = wsGoing
				}
				farewell(c, code, "", reads)
				return
			}
			if err := s.onTerm(ts, tc, f); err != nil {
				ts.detach(tc)
				tc.settle()
				farewell(c, wsPolicy, err.Error(), reads)
				return
			}
		case <-tc.gone:
			// The terminal ended, or this connection fell too far behind to
			// be worth keeping. Either way whatever was queued goes out
			// before the goodbye does: the last frame of a terminal that has
			// ended is the one that says how.
			ts.detach(tc)
			tc.settle()
			if tc.slow.Load() {
				farewell(c, wsTooSlow, "this connection could not keep up; reattach with since", reads)
				return
			}
			farewell(c, wsNormal, "", reads)
			return
		case <-s.ctx.Done():
			ts.detach(tc)
			tc.settle()
			farewell(c, wsGoing, "the server is stopping", reads)
			return
		case <-t.C:
			// A ping the peer answers, which is also how a dead connection is
			// noticed: three unanswered and the read deadline passes.
			tc.push(opPing, nil)
		}
	}
}

// termFrameIn is one thing a client says to a terminal it is attached to.
// The bytes of input are not here — those are a binary frame, and a frame
// that carried them as a field would have to escape every key.
type termFrameIn struct {
	Type  string `json:"type"`
	Cols  uint16 `json:"cols"`
	Rows  uint16 `json:"rows"`
	Force bool   `json:"force"`
	To    string `json:"to"`
}

// onTerm acts on one frame. Returning an error ends the socket: that is for
// a frame this server cannot read at all. Everything it can read and will
// not do is an error frame with a reason, because the connection is fine.
func (s *Server) onTerm(ts *termSession, tc *termConn, f inFrame) error {
	if f.op == opBinary {
		if !tc.role.allows(termNeeds("input")) {
			tc.fail(watchOnly)
			return nil
		}
		if err := ts.typeInto(tc, f.data); err != nil {
			tc.fail(err.Error())
		}
		return nil
	}
	if !isObject(f.data) {
		return wsFail{wsPolicy, "a text frame here is one JSON object; input is a binary frame"}
	}
	var in termFrameIn
	if err := rota.UnmarshalLenient(f.data, &in); err != nil {
		return wsFail{wsPolicy, "a frame this server cannot read"}
	}
	if !tc.role.allows(termNeeds(in.Type)) {
		tc.fail(watchOnly)
		return nil
	}
	switch in.Type {
	case "resize":
		if err := ts.resizeTo(tc, in.Cols, in.Rows); err != nil {
			tc.fail(err.Error())
		}
	case "claim":
		ts.claim(tc, in.Force)
	case "release":
		ts.giveUp(tc)
	case "grant":
		ts.grant(tc, in.To)
	case "deny":
		ts.deny(tc, in.To)
	case "ping":
		tc.say(map[string]any{"type": "pong"})
	default:
		tc.fail("unknown frame type " + strconv.Quote(in.Type) +
			"; a terminal takes resize, claim, release, grant, deny and ping, and input as bytes")
	}
	return nil
}
