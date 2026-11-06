package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/ring"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/rotation"
	"github.com/professor93/rota/wire"
)

// How a daemon comes to hold terminals: the claim on its store, and each
// holder started, asked and either linked to or turned into the answer a
// terminal this server ran itself would have given. The far end each one
// becomes is held.go's.

/* --------------------------------------------------------------- the claim --- */

// claimDaemon makes this server the one daemon of its store, or says which
// process already is.
func (s *Server) claimDaemon() error {
	st, err := s.openStore()
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	terminals, err := st.TerminalDir()
	st.Close()
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	release, err := holdwire.ClaimDaemon(terminals, wire.Version)
	var running holdwire.DaemonRunning
	if errors.As(err, &running) {
		return fmt.Errorf("api: another rota daemon serves %s (pid %d)", filepath.Dir(terminals), running.PID)
	}
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	s.terminals, s.unclaim = terminals, release
	return nil
}

/* ------------------------------------------------------------ starting one --- */

// startHeld is startTerminal for a daemon: the account is chosen and judged
// launchable here, as it is today, and everything after that — Prepare, the
// pseudo-terminal, the CLI — is the holder's.
func (s *Server) startHeld(r *http.Request, kind string, req *termRequest, cwd string) (*termSession, error) {
	hreq := holdwire.Request{ID: runID(), Kind: kind, Args: req.Args, Cwd: cwd,
		Name: strings.TrimSpace(req.Label), Scrollback: s.opts.Terminal.Scrollback}
	hreq.Cols, hreq.Rows = boundSize(req.Cols, req.Rows)
	var label, provider string
	if kind == termKindShell {
		if !s.opts.Terminal.Shell {
			return nil, refuse(http.StatusForbidden, shellOff)
		}
	} else {
		a, err := s.chooseHeld(r, req.Account)
		if err != nil {
			return nil, err
		}
		hreq.Account, label, provider = a.ID, a.Label(), a.Provider
	}
	cmd, ready, err := s.spawnHolder(&hreq)
	if err != nil {
		return nil, err
	}
	if !ready.OK {
		return nil, s.heldRefusal(ready, hreq.Account)
	}
	h := &heldTerm{s: s, dir: holdwire.Dir(s.terminals, hreq.ID), pid: ready.PID,
		state: heldLive, lastOut: s.now(), asks: map[uint64]chan *heldScreen{}, left: make(chan struct{})}
	if cmd.Process.Pid == ready.PID {
		h.proc = cmd.Process
	}
	if m, err := holdwire.ReadMeta(h.dir); err == nil && m.HolderPID == ready.PID {
		h.start, h.boot = m.HolderStart, m.BootID
	}
	conn, w, err := linkHolder(h.dir, func(w *holdwire.WelcomeMsg) int64 { return w.Oldest })
	if err != nil {
		// Nothing can be served from it, so it is told to stop rather than
		// left holding the account for nobody.
		h.signal(syscall.SIGTERM)
		return nil, fmt.Errorf("linking to the holder of %s: %w", hreq.ID, err)
	}
	h.conn, h.features = conn, w.Features
	cols, rows := boundSize(w.Cols, w.Rows)
	ts := &termSession{
		s: s, id: hreq.ID, log: s.log.Info,
		kind: kind, account: hreq.Account, label: label, provider: provider,
		name: hreq.Name, cwd: cwd, started: s.now(), tokenUntil: ready.TokenUntil,
		back: h, cols: cols, rows: rows, ring: ring.NewAt(s.opts.Terminal.Scrollback, w.Oldest),
	}
	ts.idleAt = ts.started
	h.ts = ts
	s.recordTerminal(ts)
	s.addTerminal(ts)
	ts.audit("terminal created", "kind", kind, "account", hreq.Account, "by", who(r).Name, "pid", h.pid)
	go ts.run()
	return ts, nil
}

// chooseHeld is the part of planTerminal a daemon keeps: which account, and
// whether it may launch at all. The store is closed again before a holder
// opens it.
func (s *Server) chooseHeld(r *http.Request, id int) (*rota.Account, error) {
	st, err := s.openStore()
	if err != nil {
		s.log.Error("opening the store", "err", err)
		return nil, refuse(http.StatusInternalServerError, "the account store could not be opened")
	}
	defer st.Close()
	rotation.Backfill(st)
	a, err := rotation.Choose(r.Context(), st, id)
	if err != nil {
		return nil, err
	}
	if err := st.Launchable(r.Context(), a); err != nil {
		return nil, err
	}
	return a, nil
}

// spawnHolder starts one holder, in a session of its own and pointed at this
// server's store, hands it the request on its standard input and reads its
// one line of answer. The holder is reaped whenever it ends.
func (s *Server) spawnHolder(req *holdwire.Request) (*exec.Cmd, *holdwire.Ready, error) {
	cmd := s.opts.HoldCommand()
	if cmd == nil {
		return nil, nil, errors.New("HoldCommand gave no command")
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = setEnv(env, "ROTA_HOME", filepath.Dir(s.terminals))
	detachHolder(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	raw, err := rota.Encode(req)
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	go func() {
		_, _ = stdin.Write(append(raw, '\n'))
		_ = stdin.Close()
	}()
	type answer struct {
		r   holdwire.Ready
		err error
	}
	got := make(chan answer, 1)
	go func() {
		var a answer
		a.err = json.NewDecoder(io.LimitReader(stdout, 1<<16)).Decode(&a.r)
		got <- a
	}()
	select {
	case a := <-got:
		go func() { _ = cmd.Wait() }()
		if a.err != nil {
			return nil, nil, fmt.Errorf("the holder of %s gave no answer: %w", req.ID, a.err)
		}
		return cmd, &a.r, nil
	case <-time.After(holdReady):
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait() }()
		return nil, nil, fmt.Errorf("the holder of %s did not answer within %s", req.ID, holdReady)
	}
}

// heldRefusal is a holder's failure as the answer a terminal this server ran
// itself would have given: the same status for the same kind of failure,
// and for a busy account the session that holds it.
func (s *Server) heldRefusal(r *holdwire.Ready, account int) error {
	code := http.StatusInternalServerError
	switch r.Kind {
	case holdwire.KindBusy, holdwire.KindReauth:
		code = http.StatusConflict
	case holdwire.KindNoAccount:
		code = http.StatusNotFound
	case holdwire.KindUnsupported:
		code = http.StatusBadGateway
	case holdwire.KindOutside:
		code = http.StatusBadRequest
	default:
		s.log.Error("a holder could not start", "err", r.Error)
		return refuse(code, "internal error")
	}
	msg := r.Error
	if r.Kind == holdwire.KindBusy && account != 0 {
		if rec := holdwire.Holding(s.terminals, account); rec != nil {
			msg += " — held by session " + recordName(rec) + " (" + rec.ID + ")"
		}
	}
	return refuse(code, msg)
}

// recordName is what a session on disk is called, by the same rule as a
// live one as far as its record goes.
func recordName(r *holdwire.Record) string {
	switch m := r.Meta; {
	case m == nil:
	case m.Name != "":
		return m.Name
	case m.ConversationName != "":
		return m.ConversationName
	case m.Cwd != "":
		return filepath.Base(m.Cwd)
	}
	return r.ID
}
