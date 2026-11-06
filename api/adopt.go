package api

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
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
	var live []holdwire.Record
	running := map[string]bool{}
	for _, r := range holdwire.Scan(s.terminals) {
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
	h := &heldTerm{s: s, dir: r.Dir, pid: m.HolderPID, start: m.HolderStart, boot: m.BootID,
		conn: conn, features: w.Features, state: heldLive, lastOut: s.now(),
		conv: m.Conversation, convCwd: m.ConversationCwd,
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

// accountLabels is each account's own name by id, for the sessions whose
// records name only the id. A store that cannot be read is no names rather
// than a failure: the records say everything else.
func (s *Server) accountLabels() map[int]string {
	st, err := s.openStore()
	if err != nil {
		return nil
	}
	defer st.Close()
	out := make(map[int]string, len(st.Accounts))
	for _, a := range st.Accounts {
		out[a.ID] = a.Label()
	}
	return out
}

/* ------------------------------------------------- what a record says --- */

// stored is every session a daemon describes from its record rather than
// from a session it holds: the stopped ones, and the running ones it is not
// linked to. A directory with no record yet, or a locked one whose record
// cannot be read, is a holder on its way and is not one of them.
func (s *Server) stored() []holdwire.Record {
	if s.terminals == "" {
		return nil
	}
	var out []holdwire.Record
	for _, r := range holdwire.Scan(s.terminals) {
		if s.known(r.ID) || noRecord(&r) {
			continue
		}
		if r.State == holdwire.StateBroken && r.Locked {
			continue
		}
		out = append(out, r)
	}
	return out
}

// storedRecord is the one session of stored with this id, or nil.
func (s *Server) storedRecord(id string) *holdwire.Record {
	for _, r := range s.stored() {
		if r.ID == id {
			return &r
		}
	}
	return nil
}

// describeRecord is a session's description from its record alone, in the
// shape of a held terminal's. It says what Resume and Move need: whether it
// stopped and how, whether it can be resumed, and on which conversation in
// which folder.
func describeRecord(r *holdwire.Record, labels map[int]string) map[string]any {
	doc := map[string]any{
		"id": r.ID, "held": true, "name": recordName(r), "title": "",
		"holder": nil, "viewers": []any{}, "recording": false,
		"conversation": "", "conversation_cwd": "", "resumable": false,
	}
	m := r.Meta
	if m != nil {
		doc["kind"], doc["label"], doc["cwd"] = m.Kind, m.Name, m.Cwd
		doc["conversation"], doc["conversation_cwd"] = m.Conversation, m.ConversationCwd
		if !m.Started.IsZero() {
			doc["started"] = m.Started.Format(time.RFC3339)
		}
		if m.Kind != termKindShell && m.Account != 0 {
			doc["account"] = map[string]any{"id": m.Account, "label": labels[m.Account], "provider": m.Provider}
		}
	}
	switch r.State {
	case holdwire.StateLive, holdwire.StateUnreadable:
		doc["state"], doc["status"], doc["ended"] = heldLive, "running", false
		doc["reachable"], doc["pid"] = false, m.HolderPID
		doc["attachable"] = r.State == holdwire.StateLive
		if r.State == holdwire.StateUnreadable {
			doc["made_by"] = madeBy(m)
		}
		if !m.TokenUntil.IsZero() {
			doc["token_until"] = m.TokenUntil.Format(time.RFC3339)
		}
	default:
		doc["state"], doc["status"], doc["ended"] = string(r.State), "stopped", true
		doc["attachable"], doc["reachable"] = false, false
		exit := -1
		if m != nil && m.Exit != nil {
			exit = *m.Exit
		}
		doc["exit"] = exit
		if !r.Stopped.IsZero() {
			doc["ended_at"] = r.Stopped.Format(time.RFC3339)
		}
		doc["resumable"] = r.State == holdwire.StateInterrupted && m.Conversation != ""
	}
	return doc
}

// madeBy is the rota a holder this one cannot speak to says made it.
func madeBy(m *holdwire.Meta) string {
	if m.Version == "" {
		return fmt.Sprintf("unknown (protocol %d)", m.Protocol)
	}
	return m.Version
}

/* ---------------------------------------------- answering for a record --- */

// recordRefusal is why a session known only from its record cannot be
// attached to.
func recordRefusal(r *holdwire.Record) string {
	switch r.State {
	case holdwire.StateUnreadable:
		return fmt.Sprintf("session %s was made by rota %s, which this rota cannot attach to; stop it, or attach with that version", r.ID, madeBy(r.Meta))
	case holdwire.StateLive:
		return "session " + r.ID + " is running, but its holder is not answering; it is tried again within " + short(termSweep)
	}
	return "terminal " + r.ID + " has ended"
}

// stopRecord ends a running session this daemon is not linked to, by
// signal: a hangup, and a kill a little after the time the holder has to
// stop its own CLI if its lock is still held then. Each goes only while the
// pid is still the holder the record names. A stopped one has ended
// already.
func (s *Server) stopRecord(w http.ResponseWriter, r *http.Request, rec *holdwire.Record) {
	if rec.State != holdwire.StateLive && rec.State != holdwire.StateUnreadable {
		fail(w, http.StatusConflict, "terminal "+rec.ID+" has ended")
		return
	}
	kill, err := holdwire.StopHolder(rec, termKillAfter+holdKillLater)
	switch {
	case errors.Is(err, holdwire.ErrNotSameProcess):
		fail(w, http.StatusConflict, "session "+rec.ID+" is no longer the process that started it; nothing was signalled")
		return
	case err != nil:
		s.log.Error("a held session could not be signalled", "terminal", rec.ID, "err", err)
		fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.log.Info("terminal killed", "terminal", rec.ID, "by", who(r).Name, "pid", rec.Meta.HolderPID)
	go kill()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
