package api

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/ring"
)

// A daemon that starts finds what the store's records say and takes it all
// up, each record on its own so that one broken directory hides nothing:
//
//   - a holder still running that speaks a protocol this rota does is linked
//     to again, and is from then on a held terminal like one this daemon
//     started — a restart looks to a viewer like a brief reconnect;
//   - one whose link fails is listed as running and not reachable, and is
//     tried again at every sweep, never taken for a holder that has gone:
//     only its lock says that;
//   - one made by a rota this one cannot speak to is listed as running and
//     not attachable, and is never signalled but by somebody asking, and
//     then only while its pid is still the process its record names;
//   - a session that has stopped — its CLI ended, or its holder vanished — is
//     listed for keep_stopped, so it can be read about and resumed, and is
//     then forgotten. Nothing restarts by itself.
//
// The sweep that does this runs when the daemon starts and with every idle
// sweep after. It deletes only the directories of sessions that are over,
// never one whose lock is held, and never anything a session's CLI made.

const (
	// defaultKeepStopped is how long a stopped held session stays listed.
	defaultKeepStopped = 168 * time.Hour
	// strayAfter is how old a session's directory with no record in it must
	// be before it is taken for a holder that failed before it wrote one,
	// rather than one about to.
	strayAfter = time.Hour
)

/* ------------------------------------------------------- whose is which --- */

// pend marks a held session as on its way to being this server's, so that
// nothing else takes the same one up meanwhile. started is a holder this
// server is starting, which has its place under max_sessions already. False
// is one already held here, or already on its way.
func (s *Server) pend(id string, started bool) bool {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	if _, on := s.pending[id]; on || s.terms[id] != nil {
		return false
	}
	s.pending[id] = started
	return true
}

func (s *Server) unpend(id string) {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	delete(s.pending, id)
}

// known reports whether a held session is this server's, or on its way.
func (s *Server) known(id string) bool {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	_, on := s.pending[id]
	return on || s.terms[id] != nil
}

// keepHeld adds a held session — unless Stop has begun, which lets go of
// every held session it finds and would never find this one. False is the
// caller's to let go of instead.
func (s *Server) keepHeld(ts *termSession) bool {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	delete(s.pending, ts.id)
	if s.stopping {
		return false
	}
	s.terms[ts.id] = ts
	return true
}

/* ------------------------------------------------------------- the sweep --- */

// sweepRecords goes through the store's records, each on its own: a live
// holder not held here is linked to, and a session nobody holds the lock of
// is tidied. Only a daemon has records to sweep.
func (s *Server) sweepRecords() {
	if s.terminals == "" {
		return
	}
	now := s.now()
	records := holdwire.Scan(s.terminals)
	s.readLabels(records)
	var live []holdwire.Record
	running := map[string]bool{}
	for _, r := range records {
		switch r.State {
		case holdwire.StateLive:
			running[r.ID] = true
			if !s.known(r.ID) {
				live = append(live, r)
			}
		case holdwire.StateUnreadable:
			// Another rota's, running: listed, and left alone.
		default:
			s.tidy(&r, now)
		}
	}
	s.unlinkedOnly(running)
	s.readopt(live)
}

// tidy deals with one directory nobody seemed to hold when it was judged:
// all of it goes once its session has been stopped for longer than
// KeepStopped — or, with no record ever written, once it is strayAfter
// old — and otherwise only a socket a holder left behind. Whatever goes,
// goes under the directory's own lock: a holder that held it would make
// that fail, so a locked session's socket is never deleted, however it was
// judged a moment before. The directory is the session's alone; nothing the
// CLI made, a worktree least of all, is in it.
func (s *Server) tidy(r *holdwire.Record, now time.Time) {
	if r.Locked {
		return
	}
	expired := s.expired(r, now)
	sock := holdwire.SockPath(r.Dir)
	if _, err := os.Lstat(sock); !expired && err != nil {
		return // nothing to do, so the lock is not even asked for
	}
	release, err := holdwire.Lock(r.Dir)
	if err != nil {
		return // held: a holder's, and not to be touched
	}
	defer release()
	if expired {
		if err := os.RemoveAll(r.Dir); err != nil {
			s.log.Warn("a stopped held session could not be forgotten", "terminal", r.ID, "err", err)
			return
		}
		s.log.Info("held session forgotten", "terminal", r.ID, "state", string(r.State))
		return
	}
	if err := os.Remove(sock); err == nil {
		s.log.Info("stale socket removed", "terminal", r.ID)
	}
}

// expired reports whether a stopped session's directory is due to go.
func (s *Server) expired(r *holdwire.Record, now time.Time) bool {
	if noRecord(r) {
		fi, err := os.Stat(r.Dir)
		return err == nil && now.Sub(fi.ModTime()) > strayAfter
	}
	keep := s.opts.Terminal.KeepStopped
	return keep > 0 && !r.Stopped.IsZero() && now.Sub(r.Stopped) > keep
}

// noRecord is a directory a holder never wrote meta.json in.
func noRecord(r *holdwire.Record) bool {
	return r.State == holdwire.StateBroken && errors.Is(r.Err, fs.ErrNotExist)
}

/* ------------------------------------------------------ taking one up --- */

// readopt links to every live holder in records at once, each on its own:
// one that does not answer within holdHandshake costs only itself, and the
// daemon is not held up by more than the slowest.
func (s *Server) readopt(records []holdwire.Record) {
	if len(records) == 0 {
		return
	}
	labels := s.accountLabels()
	var wg sync.WaitGroup
	for i := range records {
		wg.Go(func() { s.adopt(&records[i], labels) })
	}
	wg.Wait()
}

// adopt links to one live holder and holds its session from here on, as if
// this server had started it. It asks for everything the holder still has,
// and the ring takes the holder's numbering, so an offset a viewer had from
// the daemon before means the same bytes now. A link that fails is said
// once in the log; the record goes on being listed, and the next sweep
// tries again.
func (s *Server) adopt(r *holdwire.Record, labels map[int]string) {
	if s.isStopping() || !s.pend(r.ID, false) {
		return
	}
	conn, w, err := linkHolder(r.Dir, func(*holdwire.WelcomeMsg) int64 { return 0 })
	if err != nil {
		s.unpend(r.ID)
		if s.unlinkedOnce(r.ID) {
			s.log.Warn("a held session could not be linked; it is tried again at every sweep", "terminal", r.ID, "err", err)
		}
		return
	}
	m := r.Meta
	h := &heldTerm{s: s, dir: r.Dir, pid: m.HolderPID, start: m.HolderStart, ticks: m.HolderStartTicks, boot: m.BootID,
		conn: conn, features: w.Features, state: heldLive, lastOut: s.now(),
		conv: m.Conversation, convName: m.ConversationName, convCwd: m.ConversationCwd,
		reg:  holdwire.Registry{SessionID: m.Conversation, Name: m.ConversationName, Cwd: m.ConversationCwd},
		asks: map[uint64]chan *heldScreen{}, left: make(chan struct{})}
	kind, started := m.Kind, m.Started
	if kind == "" {
		kind = termKindAccount
	}
	if started.IsZero() {
		started = s.now()
	}
	cols, rows := boundSize(w.Cols, w.Rows)
	ts := &termSession{
		s: s, id: r.ID, log: s.log.Info,
		kind: kind, account: m.Account, label: labels[m.Account], provider: m.Provider,
		name: m.Name, cwd: m.Cwd, started: started, tokenUntil: m.TokenUntil,
		back: h, cols: cols, rows: rows, ring: ring.NewAt(s.opts.Terminal.Scrollback, w.Oldest),
	}
	ts.idleAt = s.now()
	h.ts = ts
	s.recordTerminalFrom(ts, true)
	if !s.keepHeld(ts) {
		h.detach()
		if ts.rec != nil {
			ts.rec.close(ts.view())
		}
		return
	}
	ts.audit("terminal adopted", "kind", kind, "account", m.Account, "pid", h.pid)
	go ts.run()
}

// unlinkedOnce reports whether this is the first failed link to a holder
// since it was last linked to, so a holder that stays unreachable is said
// once in the log rather than at every sweep.
func (s *Server) unlinkedOnce(id string) bool {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	if s.unlinked[id] {
		return false
	}
	if s.unlinked == nil {
		s.unlinked = map[string]bool{}
	}
	s.unlinked[id] = true
	return true
}

// unlinkedOnly forgets the failed links of holders that are not running
// any more, or that this server has since linked to.
func (s *Server) unlinkedOnly(running map[string]bool) {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	for id := range s.unlinked {
		if !running[id] || s.terms[id] != nil {
			delete(s.unlinked, id)
		}
	}
}

func (s *Server) isStopping() bool {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	return s.stopping
}

// readLabels reads each account's own name from the store, for the
// sessions whose records name only the account's id, and keeps them for
// describing those. Only a sweep opens the store for them, and only when a
// record names an account — never a request, which would otherwise wait on
// the store's lock to say what is on disk. A store that cannot be read
// keeps the names already known: the records say everything else.
func (s *Server) readLabels(records []holdwire.Record) {
	if !slices.ContainsFunc(records, func(r holdwire.Record) bool { return r.Meta != nil && r.Meta.Account != 0 }) {
		return
	}
	st, err := s.openStore()
	if err != nil {
		return
	}
	labels := make(map[int]string, len(st.Accounts))
	for _, a := range st.Accounts {
		labels[a.ID] = a.Label()
	}
	st.Close()
	s.termsMu.Lock()
	s.labels = labels
	s.termsMu.Unlock()
}

// accountLabels is each account's own name by id as the last sweep read
// it. The map is replaced whole and never written to, so it is read after
// the lock is let go of.
func (s *Server) accountLabels() map[int]string {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	return s.labels
}

/* ------------------------------------------------- what a record says --- */

// everySession is every terminal this server holds, and in a daemon every
// session it describes from its record instead: the stopped ones, and the
// running ones it is not linked to. The records are read first and the
// terminals after, in one look with what rules a record out, so a session
// moving from its record to this server in between is in exactly one of
// the two.
func (s *Server) everySession() ([]*termSession, []holdwire.Record) {
	var scanned []holdwire.Record
	if s.terminals != "" {
		scanned = holdwire.Scan(s.terminals)
	}
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	terms := make([]*termSession, 0, len(s.terms))
	for _, ts := range s.terms {
		terms = append(terms, ts)
	}
	var records []holdwire.Record
	for _, r := range scanned {
		if s.describedFrom(&r) {
			records = append(records, r)
		}
	}
	return terms, records
}

// lookup is the session with this id: a terminal this server holds, or in
// a daemon a session it describes from its record, which is that one
// directory judged alone. Both nil is an id nobody knows.
func (s *Server) lookup(id string) (*termSession, *holdwire.Record) {
	if ts := s.findTerminal(id); ts != nil || s.terminals == "" {
		return ts, nil
	}
	r := holdwire.Judge(s.terminals, id)
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	if ts := s.terms[id]; ts != nil {
		return ts, nil // taken up while its record was being read
	}
	if !s.describedFrom(&r) {
		return nil, nil
	}
	return nil, &r
}

// describedFrom reports whether a session is described from its record:
// it is one — a directory with no record in it is a holder on its way, or
// one that never got there — and it is neither held here nor a holder this
// server is starting. One a sweep is linking to again is still described
// from its record until it is held, so a holder slow to answer the link
// stays listed and can be stopped meanwhile. Called with termsMu held.
func (s *Server) describedFrom(r *holdwire.Record) bool {
	return !noRecord(r) && s.terms[r.ID] == nil && !s.pending[r.ID]
}

// checkable reports whether a record names its holder well enough to be
// signalled: the pid, with the start time and boot id it is checked
// against. One whose record could not be read may not.
func checkable(m *holdwire.Meta) bool {
	return m != nil && m.HolderPID > 0 && !m.HolderStart.IsZero() && m.BootID != ""
}

/* ---------------------------------------------- answering for a record --- */

// recordRefusal is why a session known only from its record cannot be
// attached to.
func recordRefusal(r *holdwire.Record) string {
	switch r.State {
	case holdwire.StateUnreadable:
		if by := holdwire.MadeBy(r.Meta); by != "unknown" {
			return fmt.Sprintf("session %s was made by rota %s, which this rota cannot attach to; stop it, or attach with that version", r.ID, by)
		}
		return "session " + r.ID + " has a record this rota cannot read, so it cannot be attached to; attach with the rota that made it"
	case holdwire.StateLive:
		return "session " + r.ID + " is running, but its holder is not answering; it is tried again within " + short(termSweep)
	}
	return "terminal " + r.ID + " has ended"
}

// stopRecord ends a running session this daemon is not linked to, by
// signal: a hangup, and a kill a little after the time the holder has to
// stop its own CLI if its lock is still held then. Each goes only while the
// pid is still the holder the record names. A stopped one has ended
// already, and one a DELETE is stopping already is sent nothing more.
func (s *Server) stopRecord(w http.ResponseWriter, r *http.Request, rec *holdwire.Record) {
	if !rec.Running() {
		fail(w, http.StatusConflict, "terminal "+rec.ID+" has ended")
		return
	}
	if !checkable(rec.Meta) {
		fail(w, http.StatusConflict, "session "+rec.ID+"'s record names no process this rota can check; nothing was signalled")
		return
	}
	if !s.signal(rec.ID) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	kill, err := holdwire.StopHolder(rec, termKillAfter+holdKillLater)
	switch {
	case errors.Is(err, holdwire.ErrNotSameProcess):
		s.unsignal(rec.ID)
		fail(w, http.StatusConflict, "session "+rec.ID+" is no longer the process that started it; nothing was signalled")
		return
	case err != nil:
		s.unsignal(rec.ID)
		s.log.Error("a held session could not be signalled", "terminal", rec.ID, "err", err)
		fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.log.Info("terminal killed", "terminal", rec.ID, "by", who(r).Name, "pid", rec.Meta.HolderPID)
	go func() {
		defer s.unsignal(rec.ID)
		kill()
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// signal marks a session as being stopped by signal, and reports whether
// it was not already: one hangup and one kill to come per session, however
// many times it is asked. unsignal is the stop over — the holder gone, or
// killed — after which a session somehow still running may be asked again.
func (s *Server) signal(id string) bool {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	if s.signalled[id] {
		return false
	}
	if s.signalled == nil {
		s.signalled = map[string]bool{}
	}
	s.signalled[id] = true
	return true
}

func (s *Server) unsignal(id string) {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	delete(s.signalled, id)
}
