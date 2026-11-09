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
	"github.com/professor93/rota/internal/termenv"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

// How a daemon comes to hold terminals: the claim on its store, and each
// holder started, asked and either linked to or turned into the answer a
// terminal this server ran itself would have given. The far end each one
// becomes is held.go's.

/* --------------------------------------------------------------- the claim --- */

// claimDaemon makes this server the one daemon of its store, or says which
// process already is, as a holdwire.DaemonRunning.
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
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	s.terminals, s.unclaim = terminals, release
	s.claimed.Store(true)
	return nil
}

// HoldsDaemon reports whether this server holds the daemon claim on the
// store whose terminals directory this is: from New, for a server with
// HoldCommand, until Stop lets go of it. Whatever only a store's daemon may
// do — listening on its control socket, above all — asks this rather than
// daemon.pid, which says which process held the claim last, not which server
// holds it now.
func (s *Server) HoldsDaemon(terminals string) bool {
	return s.claimed.Load() && s.terminals != "" && filepath.Clean(terminals) == filepath.Clean(s.terminals)
}

/* ------------------------------------------------------------ starting one --- */

// startHeld is startTerminal for a daemon: what may run is chosen here, by
// chooseTerminal as for any terminal, and everything after that — Prepare,
// the pseudo-terminal, the CLI — is the holder's. list is the place
// startTerminal took for it.
func (s *Server) startHeld(r *http.Request, kind string, req *termRequest, cwd string, list func(*termSession) bool) (*termSession, error) {
	hreq := holdwire.Request{ID: runID(), Kind: kind, Args: req.Args, Cwd: cwd, Env: req.Env,
		Name: strings.TrimSpace(req.Label), Scrollback: s.opts.Terminal.Scrollback}
	if r.Header.Get(holdwire.CallerHeader) == holdwire.CallerDetach {
		hreq.Caller = holdwire.CallerDetach
	}
	hreq.Cols, hreq.Rows = boundSize(req.Cols, req.Rows)
	st, a, err := s.chooseTerminal(r, kind, req.Account)
	if err != nil {
		return nil, err
	}
	var label, provider string
	if a != nil {
		hreq.Args, err = s.argsFor(r, st, a, req)
		// The store is let go of before a holder opens it.
		st.Close()
		if err != nil {
			return nil, err
		}
		hreq.Account, label, provider = a.ID, a.Label(), a.Provider
	}
	// On its way from here: a sweep finding its record before this has
	// linked to it must not link to it as well.
	s.pend(hreq.ID, true)
	defer s.unpend(hreq.ID)
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
		h.start, h.ticks, h.boot = m.HolderStart, m.HolderStartTicks, m.BootID
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
	if !list(ts) {
		// Stop has begun and let go of every held session it found. This
		// one is let go of too, still running, as Stop would have: the next
		// daemon takes it up.
		h.detach()
		if ts.rec != nil {
			ts.rec.close(ts.view())
		}
		return nil, refuse(http.StatusServiceUnavailable,
			"this server is stopping; session "+ts.id+" was started and goes on running, for the next rota daemon to take up")
	}
	ts.audit("terminal created", "kind", kind, "account", hreq.Account, "by", who(r).Name, "pid", h.pid)
	go ts.run()
	return ts, nil
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
	cmd.Env = termenv.Set(env, "ROTA_HOME", filepath.Dir(s.terminals))
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
	case <-holdDeadline():
		s.giveUpOnHolder(cmd)
		return nil, nil, fmt.Errorf("the holder of %s did not answer within %s", req.ID, holdReady)
	}
}

// holdDeadline is what waiting for a holder's answer ends with. A variable so
// that a test can give up on a holder at the moment it chooses.
var holdDeadline = func() <-chan time.Time { return time.After(holdReady) }

// holdKillAfter is how long a holder that has not answered has, once it is told
// to stop, before it is killed: all a refresh under way may take, and a margin
// for its save. A variable so that a test need not wait for it.
var holdKillAfter = func() time.Duration { return store.RefreshTimeout() + stopMargin }

// giveUpOnHolder tells a holder that has not answered in time to stop, and kills
// it only once it has had all the time a refresh under way can take. It may be
// preparing its account, with the provider's answer still to come: a SIGTERM
// lets that finish, the save and the write into the home included, and starts
// nothing else, where a SIGKILL would lose the new token with the one it
// replaced already spent. The holder is this server's child until it is reaped
// here, so its pid can be nobody else's while it is signalled.
func (s *Server) giveUpOnHolder(cmd *exec.Cmd) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	go func() {
		select {
		case <-exited:
		case <-time.After(s.killHolderAfter):
			_ = cmd.Process.Kill()
		}
	}()
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
		msg += holdwire.HeldBy(s.terminals, account)
	}
	return refuse(code, msg)
}
