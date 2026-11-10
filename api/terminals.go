package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
	"github.com/professor93/rota/internal/pty"
	"github.com/professor93/rota/internal/ring"
	"github.com/professor93/rota/internal/termenv"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/rotation"
	"github.com/professor93/rota/store"
)

// The doors into a terminal: six routes and one socket protocol.
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
	// Env, set, is the environment a held session starts from in place of
	// the daemon's: the caller's own, which its holder keeps whatever
	// describes another terminal out of. Only this machine's own user hands
	// it over, on the control socket; from anybody else it would be choosing
	// what runs as the daemon's user. A server that runs its terminals
	// itself has no control socket, and so never sees one.
	Env []string `json:"env,omitempty"`
	// RemoteControl starts an account's CLI with Claude Code's Remote
	// Control on, for whichever account is chosen, as `rota run
	// --remote-control` does: the account's setting turned on first, and
	// Claude Code's flag added last. It changes the account, so it too is
	// taken only on the control socket — as are a claude account's Args that
	// ask for Remote Control themselves (remoteArgs), which are readied the
	// same way, and a change to an account's remote_control setting
	// (patchAccount).
	RemoteControl remoteControl `json:"remote_control"`
}

// remoteControl is a request's remote_control: true, or the name to give
// the session in Remote Control, which is on as well. false and null are
// off.
type remoteControl struct {
	on   bool
	name string
}

func (rc *remoteControl) UnmarshalJSON(raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	switch v := v.(type) {
	case nil:
		*rc = remoteControl{}
		return nil
	case bool:
		*rc = remoteControl{on: v}
		return nil
	case string:
		if name := strings.TrimSpace(v); name != "" {
			*rc = remoteControl{on: true, name: name}
			return nil
		}
	}
	return errors.New("remote_control is true, false, or the name to give the session")
}

// The refusals this file answers with, as constants because they are part of
// what this server says and a test reads them.
const (
	shellOff     = "this server runs accounts' CLIs only; a plain shell needs terminal.shell = true in the server file"
	noTermFor    = "a terminal needs a pseudo-terminal, and this machine has none"
	termStopping = "this server is stopping; start the terminal again once it is back"
	envLocal     = "env is taken only over the control socket"
	envEmpty     = "env is empty, which would start the session with no PATH; leave it out for the daemon's own"
	remoteLocal  = "remote_control is taken only over the control socket"
	remoteShell  = "remote_control is Claude Code's, and a shell runs no account"
	// remoteArgsLocal and settingsLocal are for a claude account's
	// arguments, a terminal's or a run's: ones that ask for Remote Control
	// themselves, and a --settings rota cannot read, which might.
	remoteArgsLocal = "--remote-control and the remote-control subcommand are taken only over the control socket, as remote_control is"
	settingsLocal   = "--settings that rota cannot read is taken only over the control socket, since it could turn Remote Control on"
	// managedLocal, managedUnread and managedReenables are for Claude Code's
	// --managed-settings, the policy tier: refused from anybody but this
	// machine's own user whatever it says, refused from that user when it is
	// not one JSON object, and when it would lift the guard.
	managedLocal     = "--managed-settings is taken only over the control socket: it is the policy tier, which ranks above everything else a session is given"
	managedUnread    = "--managed-settings is not one JSON object, so the session is not started"
	managedReenables = "--managed-settings turns disableRemoteControl off above the guard that keeps Remote Control off in this session, so it is not started; " +
		"turn the account's remote control setting on for a session with Remote Control"
	// guardUnread is a session launched behind the guard against Remote
	// Control whose own last --settings cannot be folded into it.
	guardUnread = "--settings that rota cannot read leaves no way to keep Remote Control off in this session, so it is not started; " +
		"name a settings file rota can read, or give the settings inline"
)

// localPrincipal reports whether a request is this machine's own user's:
// the principal LocalHandler serves, "via":"local", which the control socket
// mounts and nothing a request carries can claim. That user is the only
// caller who may decide whose identity an account's Remote Control acts as.
// Via "local" is reserved for this machine's own user (see Principal.Via),
// never for a key or a sign-in.
func localPrincipal(r *http.Request) bool {
	p := who(r)
	return p != nil && p.Via == "local"
}

// remoteArgs reads a claude account's CLI arguments for Remote Control
// (holdwire.AsksRemoteControl), for a terminal and a run alike. Another
// provider's are not read, since none has a Remote Control to start. From
// anybody but this machine's own user, arguments that ask for it are
// refused, and so is --settings rota cannot read, since rota cannot tell
// what Claude Code would make of it. Over the control socket, such
// --settings is Claude Code's to refuse, and counts as asking for nothing.
func remoteArgs(r *http.Request, a *rota.Account, args []string, cwd string) (bool, error) {
	if rota.Flavor(a.Provider) != "claude" {
		return false, nil
	}
	asks, err := holdwire.AsksRemoteControl(args, cwd)
	managed := holdwire.ReadManaged(args)
	switch {
	case localPrincipal(r) && managed.Err != nil:
		// Claude Code would ignore it, and rota would be guessing at what
		// a parent meant by it: refused rather than launched on a guess.
		return false, refuse(http.StatusBadRequest, managedUnread)
	case localPrincipal(r):
		return asks, nil
	case managed.Present:
		return false, refuse(http.StatusForbidden, managedLocal)
	case asks:
		return false, refuse(http.StatusForbidden, remoteArgsLocal)
	case err != nil:
		return false, refuse(http.StatusForbidden, settingsLocal)
	}
	return false, nil
}

/* --------------------------------------------------------- the endpoints --- */

// listTerminals is every terminal this server holds, and in a daemon every
// session its store's records describe besides: the stopped ones, and the
// running ones it is not linked to.
func (s *Server) listTerminals(w http.ResponseWriter, _ *http.Request) {
	terms, records := s.everySession()
	out := make([]map[string]any, 0, len(terms)+len(records))
	for _, ts := range terms {
		out = append(out, ts.view())
	}
	if len(records) > 0 {
		labels := s.accountLabels()
		for i := range records {
			out = append(out, records[i].Describe(labels))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"terminals": out})
}

// terminalOf finds the terminal the path names: one this server holds, or
// in a daemon a session it knows only from its record, which is rec. Both
// nil is an id nobody knows, and has been answered.
func (s *Server) terminalOf(w http.ResponseWriter, r *http.Request) (*termSession, *holdwire.Record) {
	id := r.PathValue("id")
	ts, rec := s.lookup(id)
	if ts == nil && rec == nil {
		fail(w, http.StatusNotFound, "no terminal "+id)
	}
	return ts, rec
}

func (s *Server) describeTerminal(w http.ResponseWriter, r *http.Request) {
	switch ts, rec := s.terminalOf(w, r); {
	case ts != nil:
		writeJSON(w, http.StatusOK, ts.view())
	case rec != nil:
		writeJSON(w, http.StatusOK, rec.Describe(s.accountLabels()))
	}
}

// createTerminal starts one. The answer is the description, so the client has
// the id it will attach by before anything has been printed.
func (s *Server) createTerminal(w http.ResponseWriter, r *http.Request) {
	var req termRequest
	if err := strictJSON(r.Body, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// What only this machine's own user hands a daemon, refused from
	// anybody else before anything is chosen or started.
	if !localPrincipal(r) {
		switch {
		case req.Env != nil:
			fail(w, http.StatusForbidden, envLocal)
			return
		case req.RemoteControl.on:
			fail(w, http.StatusForbidden, remoteLocal)
			return
		}
	}
	if req.Env != nil && len(req.Env) == 0 {
		fail(w, http.StatusBadRequest, envEmpty)
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
	ts, rec := s.terminalOf(w, r)
	if rec != nil {
		s.stopRecord(w, r, rec)
		return
	}
	if ts == nil {
		return
	}
	if ts.over() {
		termGone(w, ts)
		return
	}
	ts.kill(who(r).Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// heldSendMax bounds the body of a send: a message, not a file.
const heldSendMax = 256 << 10

// sendTerminal types a message into a held session the way a person would:
// its holder waits until the CLI is ready for one, types it, and presses
// Enter. It is typing, so it goes under the keyboard's rule: the keyboard
// free, or held by the sender's own person, and from the moment it is let in
// until it is answered nobody else types into the session but one Esc or
// Ctrl-C (heldTerm.sendingFor). A session waiting on its person for a
// permission or a question refuses it unless it is forced, because Enter
// there may approve something.
//
// The answer is 200 once the text is in, or the refusal with a reason a
// program can act on: 409 waiting, keyboard (naming the holder), ended or
// not_held, and 504 timeout for a session that never became ready.
func (s *Server) sendTerminal(w http.ResponseWriter, r *http.Request) {
	ts, rec := s.terminalOf(w, r)
	if ts == nil && rec == nil {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, heldSendMax+1))
	if err != nil || len(raw) > heldSendMax {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a send is at most %d KiB", heldSendMax>>10))
		return
	}
	var req struct {
		Text  string `json:"text"`
		Force bool   `json:"force"`
	}
	if err := strictJSON(bytes.NewReader(raw), &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Text == "" {
		fail(w, http.StatusBadRequest, "text is empty; a send types a message and then Enter")
		return
	}
	if rec != nil {
		reason := "ended"
		if rec.Running() {
			reason = "not_held"
		}
		sendRefused(w, http.StatusConflict, reason, recordRefusal(rec), "")
		return
	}
	h, held := ts.back.(*heldTerm)
	switch {
	case !held:
		sendRefused(w, http.StatusConflict, "not_held",
			"terminal "+ts.id+" is not a held session; a send types only into one", "")
		return
	case ts.over():
		sendRefused(w, http.StatusConflict, "ended", "terminal "+ts.id+" has ended", "")
		return
	}
	p := who(r)
	person := s.personOf(p)
	x, answer := ts.letSendIn(h, person, p.Name)
	if answer.Code == sentKeyboard {
		holder := answer.Message
		sendRefused(w, http.StatusConflict, "keyboard", holder+" holds the keyboard of this session", holder)
		return
	}
	if x != nil {
		heldSendLetIn()
		answer = h.sendText(r.Context(), x, req.Text, req.Force)
	}
	word := answer.Code
	if word == holdwire.SentOK {
		word = "typed"
	}
	ts.audit("terminal sent", "by", p.Name, "force", req.Force, "answer", word)
	say := func(fallback string) string {
		if answer.Message != "" {
			return answer.Message
		}
		return fallback
	}
	switch answer.Code {
	case holdwire.SentOK:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case holdwire.SentWaiting:
		sendRefused(w, http.StatusConflict, "waiting",
			say("the session is waiting for its person, and Enter there may approve something; force sends anyway"), "")
	case holdwire.SentTimeout:
		sendRefused(w, http.StatusGatewayTimeout, "timeout",
			say("the session did not become ready for the text in time; nothing was typed"), "")
	case holdwire.SentEnded:
		sendRefused(w, http.StatusConflict, "ended", "terminal "+ts.id+" has ended", "")
	case sentUnlinked:
		sendRefused(w, http.StatusConflict, "not_held",
			say("the link to this session's holder went before it answered; the text may not have been typed"), "")
	case sentNoSend:
		sendRefused(w, http.StatusConflict, "not_held", "this session's holder takes no sends", "")
	case sentGone:
		// Nobody is left to read an answer.
	default:
		sendRefused(w, http.StatusBadGateway, answer.Code, say("the holder answered "+answer.Code), "")
	}
}

// sendRefused is a send's refusal: the sentence, the reason a program reads,
// and for the keyboard, who holds it.
func sendRefused(w http.ResponseWriter, code int, reason, msg, holder string) {
	doc := map[string]any{"error": msg, "reason": reason}
	if holder != "" {
		doc["holder"] = holder
	}
	writeJSON(w, code, doc)
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
	if kind == termKindShell && req.RemoteControl.on {
		return nil, refuse(http.StatusBadRequest, remoteShell)
	}
	list, err := s.reserveTerminal()
	if err != nil {
		return nil, err
	}
	// A terminal that never gets as far as being listed gives its place
	// back; one that was listed has made this a no-op.
	defer list(nil)
	cwd, err := s.terminalCwd(req.Cwd)
	if err != nil {
		return nil, err
	}
	if s.opts.HoldCommand != nil {
		return s.startHeld(r, kind, req, cwd, list)
	}
	plan, err := s.planTerminal(r, kind, req, cwd)
	if err != nil {
		return nil, err
	}
	// The claim on the account is this function's until a running terminal
	// takes it over. Whatever stops that from happening — an error, or a
	// panic, which goes on as it was and which the server recovers from and
	// goes on serving — ends what was started and lets go of the account,
	// which would otherwise look busy until the server restarted.
	var (
		cmd     *exec.Cmd
		master  *os.File
		ts      *termSession
		running bool
	)
	defer func() {
		switch {
		case running:
		case ts != nil:
			// Listed, and perhaps recorded: its own ending ends all of it,
			// the claim included, once the CLI it started has gone.
			ts.back.end()
			go ts.run()
		default:
			if master != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				_ = master.Close()
			}
			plan.release()
		}
	}()
	cols, rows := boundSize(req.Cols, req.Rows)
	cmd = exec.Command(plan.path, plan.args...)
	// argv[0] is the CLI's own name rather than the path it was found at,
	// which is what the handover does and what a CLI prints in its own help.
	cmd.Args = append([]string{filepath.Base(plan.path)}, plan.args...)
	cmd.Env = plan.env
	cmd.Dir = cwd
	master, err = pty.Start(cmd, cols, rows)
	if err != nil {
		return nil, err // nothing started, so nothing is holding the account
	}
	ts = &termSession{
		s: s, id: runID(), log: s.log.Info,
		kind: kind, account: plan.account, label: plan.label, provider: plan.provider,
		name: strings.TrimSpace(req.Label), cwd: cwd, started: s.now(),
		tokenUntil: plan.tokenUntil,
		back:       &localTerm{master: master, cmd: cmd, proc: cmd.Process},
		release:    plan.release,
		cols:       cols, rows: rows, ring: ring.New(s.opts.Terminal.Scrollback),
	}
	ts.idleAt = ts.started
	s.recordTerminal(ts)
	if !list(ts) {
		return nil, refuse(http.StatusServiceUnavailable, termStopping)
	}
	ts.audit("terminal created", "kind", kind, "account", plan.account, "by", who(r).Name)
	running = true
	go ts.run()
	return ts, nil
}

// reserveTerminal takes one of terminal.max_sessions' places for a terminal
// that is starting, so the cap counts it from the request and not from the
// moment it is listed: a holder has holdReady to answer, and every request
// in that time would otherwise count the same terminals and find room.
//
// list lists the terminal and hands its place over in one step, under the
// registry's own lock, so nothing ever counts it twice or not at all. It
// refuses once Stop has begun, which has ended or let go of every terminal
// it found and would never find this one: false leaves the terminal to the
// caller to end, or for a held one to let go of. list(nil) gives the place
// back. Only its first call does anything.
func (s *Server) reserveTerminal() (list func(*termSession) bool, err error) {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if n := s.liveTerminals(); n >= s.opts.Terminal.MaxSessions {
		return nil, refuse(http.StatusConflict, fmt.Sprintf(
			"this server is holding %d terminals, which is terminal.max_sessions; end one before starting another", n))
	}
	s.termsMu.Lock()
	s.starting++
	s.termsMu.Unlock()
	var once sync.Once
	return func(ts *termSession) bool {
		listed := false
		once.Do(func() {
			s.termsMu.Lock()
			defer s.termsMu.Unlock()
			s.starting--
			if ts != nil && !s.stopping {
				s.terms[ts.id] = ts
				listed = true
			}
		})
		return listed
	}, nil
}

// recordTerminal opens the recording, where the file asked for one. A
// terminal that could not be recorded still runs: the warning is all that
// happens, because the alternative is refusing somebody a terminal over a
// directory they cannot see.
func (s *Server) recordTerminal(ts *termSession) { s.recordTerminalFrom(ts, false) }

// recordTerminalFrom is recordTerminal, or with resume the recording a
// daemon before this one kept of a held session, taken up and added to.
func (s *Server) recordTerminalFrom(ts *termSession, resume bool) {
	if !s.opts.Terminal.Record {
		return
	}
	rec, err := s.recorderFor(ts.id, resume)
	if err != nil {
		s.log.Warn("this terminal is not being recorded", "terminal", ts.id, "err", err)
		return
	}
	ts.rec = rec
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
// staged, the account's own Claude world mirrored. It is st.PrepareIn, for
// cwd, the directory the terminal starts in, because there is exactly one
// right order for those steps and one copy of it.
func (s *Server) planTerminal(r *http.Request, kind string, req *termRequest, cwd string) (*termPlan, error) {
	// A server that has stopped starts no refresh (see Stop).
	ctx, cancel := joinContexts(r.Context(), s.ctx)
	defer cancel()
	st, a, err := s.chooseTerminal(r, kind, req.Account)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return s.planShell(req)
	}
	defer st.Close()
	// A private place for the guard against Remote Control, should the
	// account get one; it goes with the terminal's CLI. An account whose
	// setting is on already gets none, so has no place made for it.
	guardPath, unguard := "", func() {}
	if rota.Flavor(a.Provider) == "claude" && !a.RemoteControl {
		if guardPath, unguard, err = guardPlace(st); err != nil {
			return nil, err
		}
	}
	placed := false
	defer func() {
		if !placed {
			unguard()
		}
	}()
	args, _, guard, err := s.argsFor(r, st, a, req, cwd, guardPath)
	if err != nil {
		return nil, err
	}
	if guard == nil {
		// Readied, so Remote Control is the account's own: no guard.
		unguard()
		unguard = func() {}
	} else if err := holdwire.WriteGuard(guardPath, guard); err != nil {
		return nil, err
	}
	// Prepared for the directory the terminal starts in, whose project
	// settings its Claude Code reads, rather than for the server's own.
	path, env, claimed, err := st.PrepareIn(ctx, a, cwd)
	if err != nil {
		return nil, err
	}
	release := func() {
		claimed()
		unguard()
	}
	placed = true
	// The claim goes with the plan, or — should building it panic — goes.
	handed := false
	defer func() {
		if !handed {
			release()
		}
	}()
	// Everything the store had to say is on disk; let go of its lock before
	// a process that may run for hours starts, exactly as the handover does.
	_ = st.Close()
	plan := &termPlan{
		path: path, args: args, env: termenv.Env(env), release: release,
		account: a.ID, label: a.Label(), provider: a.Provider, tokenUntil: holdwire.LoginUntil(a, env),
	}
	handed = true
	return plan, nil
}

// chooseTerminal is what a terminal may run, decided in this one place for a
// terminal this server runs and for one a holder runs. A shell is refused
// unless the file asked for it, because a terminal that runs an account's
// CLI is what this feature is and a general remote shell is a different
// offer. An account is the rotation's choice, and a dead lineage is refused
// before anything starts, by the store's own rule, for the reasons run.go
// gives.
//
// For an account the store comes back open, for whatever the caller does
// with it next, and is the caller's to close. A shell has neither.
func (s *Server) chooseTerminal(r *http.Request, kind string, id int) (*store.Store, *rota.Account, error) {
	if kind == termKindShell {
		if !s.opts.Terminal.Shell {
			return nil, nil, refuse(http.StatusForbidden, shellOff)
		}
		return nil, nil, nil
	}
	st, err := s.openStore()
	if err != nil {
		s.log.Error("opening the store", "err", err)
		return nil, nil, refuse(http.StatusInternalServerError, "the account store could not be opened")
	}
	// Closed on every way out but success, a panic included: the store's
	// file lock is otherwise kept until the collector finds the store.
	handed := false
	defer func() {
		if !handed {
			st.Close()
		}
	}()
	rotation.Backfill(st)
	// A server that has stopped starts no refresh (see Stop).
	ctx, cancel := joinContexts(r.Context(), s.ctx)
	defer cancel()
	a, err := rotation.Choose(ctx, st, id)
	if err == nil {
		err = st.Launchable(ctx, a)
	}
	if err != nil {
		return nil, nil, err
	}
	handed = true
	return st, a, nil
}

// argsFor is what the account's CLI is handed: the request's arguments, and
// where it asked for Remote Control, Claude Code's flag last, so nothing
// before it is read as its name. A claude account's arguments that ask for
// Remote Control themselves (remoteArgs) are refused from anybody but this
// machine's own user, and from that user handed over as they are, once the
// account is readied as remote_control readies it (readyRemoteControl). cwd
// is the directory the CLI starts in, which a relative --settings is read
// from.
//
// Every terminal this server starts is one somebody other than the person at
// this machine's own terminal can type into: a page's user, an SSH user, or
// whoever attaches to a held session. So a claude account whose remote
// control setting is off is launched behind Claude Code's own guard against
// Remote Control (holdwire.GuardRemoteControl). It is --settings at both
// ends of their arguments, with a --settings of theirs folded in, so Remote
// Control cannot be started in that session under the identity a shared
// .claude.json names: not from /remote-control, nor auto-started by a
// remoteControlAtStartup that /config wrote into the settings every account
// shares. An account whose setting is on has its own identity and is left
// as it is.
//
// The guard is named by guardPath, never given inline: guard is the document
// the caller writes there, privately, before the CLI starts, and nil when
// there is no guard. keep is what a held session's record keeps
// (holdwire.Request.Keep): the arguments before the guard, rota's own
// --remote-control included, as every launch decides the guard afresh.
func (s *Server) argsFor(r *http.Request, st *store.Store, a *rota.Account, req *termRequest, cwd, guardPath string) (args, keep []string, guard []byte, err error) {
	args = slices.Clone(req.Args)
	rc := req.RemoteControl
	asks, err := remoteArgs(r, a, args, cwd)
	if err != nil {
		return nil, nil, nil, err
	}
	if rc.on || asks {
		if err := s.readyRemoteControl(r, st, a); err != nil {
			return nil, nil, nil, err
		}
	}
	if rc.on {
		args = append(args, "--remote-control")
		if rc.name != "" {
			args = append(args, rc.name)
		}
	}
	if rota.Flavor(a.Provider) != "claude" || a.RemoteControl {
		return args, args, nil, nil
	}
	// The policy tier ranks above --settings, so one that turns
	// disableRemoteControl off would lift the guard from above it.
	if holdwire.ReadManaged(args).Reenables {
		return nil, nil, nil, refuse(http.StatusBadRequest, managedReenables)
	}
	guarded, guard, err := holdwire.GuardRemoteControl(args, cwd, guardPath)
	if err != nil {
		return nil, nil, nil, refuse(http.StatusBadRequest, guardUnread)
	}
	if args == nil {
		args = []string{} // said, so the record keeps nothing rather than the guard
	}
	return guarded, args, guard, nil
}

// guardPlace makes a private directory for one process's guard against
// Remote Control: under the store's terminals directory, in guards/, readable
// by this machine's user alone, and named for this process so a later sweep
// can tell a dead process's from a live one's (sweepGuards). It answers the
// file to write the guard to, and how to take the directory away again, once
// the process it is for has ended.
func guardPlace(st *store.Store) (path string, remove func(), err error) {
	terminals, err := st.TerminalDir()
	if err != nil {
		return "", nil, err
	}
	root := filepath.Join(terminals, "guards")
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", nil, err
	}
	if fi, err := os.Lstat(root); err != nil || !fi.IsDir() {
		return "", nil, fmt.Errorf("%s is not a directory", root)
	}
	if _, done := guardsSwept.LoadOrStore(root, true); !done {
		sweepGuards(root)
	}
	dir, err := os.MkdirTemp(root, strconv.Itoa(os.Getpid())+"-")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(dir, "guard.json"), func() { _ = os.RemoveAll(dir) }, nil
}

// guardsSwept is the guard directories swept already by this process.
var guardsSwept sync.Map

// sweepGuards takes away the guard directories processes that have ended
// left behind: one named for a pid that is not alive, or not this one.
func sweepGuards(root string) {
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		pid, _, ok := strings.Cut(e.Name(), "-")
		n, err := strconv.Atoi(pid)
		if !ok || err != nil || n == os.Getpid() || proc.Alive(n) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// readyRemoteControl readies an account for a session or a run with Remote
// Control on, by the store's one rule for it, the one `rota run
// --remote-control` follows (store.ReadyRemoteControl): its setting turned
// on and saved, and whatever could not work refused, saying why.
func (s *Server) readyRemoteControl(r *http.Request, st *store.Store, a *rota.Account) error {
	turnedOn, err := st.ReadyRemoteControl(a)
	if turnedOn {
		s.log.Info("remote control turned on", "account", a.ID, "by", who(r).Name)
	}
	if errors.Is(err, rota.ErrUnsupported) {
		// About the account, not this server: said as it is, where a 502 of
		// its own would be hidden as an internal error.
		return refuse(http.StatusBadGateway, err.Error())
	}
	return err
}

// planShell is the opt-in, once chooseTerminal has allowed it: the person's
// own login shell, with the server's environment minus the secrets no child
// may inherit.
func (s *Server) planShell(req *termRequest) (*termPlan, error) {
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
	return &termPlan{path: path, args: args, env: termenv.Env(store.HostEnv()), release: func() {}}, nil
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
func (s *Server) recorderFor(id string, resume bool) (*recorder, error) {
	st, err := s.openStore()
	if err != nil {
		return nil, err
	}
	dir, err := st.TerminalDir()
	st.Close()
	if err != nil {
		return nil, err
	}
	return newRecorder(dir, id, s.opts.Terminal.RecordMax, resume)
}

/* ------------------------------------------------------------- the socket --- */

// terminalWS attaches one connection to a terminal.
//
// The mode is what a control principal asks for when it wants to look
// without taking the keyboard from whoever has it — over somebody's
// shoulder, so to speak. A watcher never has one: watch is what it is
// whatever the query string says.
func (s *Server) terminalWS(w http.ResponseWriter, r *http.Request) {
	ts, rec := s.terminalOf(w, r)
	if rec != nil {
		// Nothing to attach to: a session this server is not linked to.
		fail(w, http.StatusConflict, recordRefusal(rec))
		return
	}
	if ts == nil {
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
	if ts.held() {
		tc.repainting(s.personOf(p), ts.catchUp)
	}
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
	Fit   bool   `json:"fit"`
	Force bool   `json:"force"`
	To    string `json:"to"`
}

// onTerm acts on one frame. Returning an error ends the socket: that is for
// a frame this server cannot read at all. Everything it can read and will
// not do is an error frame with a reason, because the connection is fine.
//
// On a held session what a connection types first has its own terminal's
// answers taken out — the holder answered those queries already, when the
// CLI asked — and a frame that held nothing else is nothing at all: no
// refusal, no keystroke, no keyboard moved.
func (s *Server) onTerm(ts *termSession, tc *termConn, f inFrame) error {
	if f.op == opBinary {
		data := f.data
		if tc.replies != nil {
			if data = tc.replies.Strip(data); len(data) == 0 {
				return nil
			}
		}
		if !tc.role.allows(termNeeds("input")) {
			tc.fail(tc.refusal)
			return nil
		}
		if err := ts.typeInto(tc, data); err != nil && !errors.Is(err, errSendTold) {
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
		tc.fail(tc.refusal)
		return nil
	}
	switch in.Type {
	case "resize":
		if ts.held() && !in.Fit {
			// A window resizing itself is not somebody asking: said once,
			// and otherwise ignored.
			if !tc.noFit {
				tc.noFit = true
				tc.fail(heldNoFit)
			}
			return nil
		}
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
