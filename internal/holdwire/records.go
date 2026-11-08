package holdwire

import (
	"cmp"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/professor93/rota/internal/proc"
	rota "github.com/professor93/rota/lib"
)

// RecordState is what a session's directory says about it, judged by its
// lock and its meta.json.
type RecordState string

// The states. Ended and interrupted are both stopped, and differ in what
// stopped them: the CLI ending (or being stopped) writes an exit, and a
// holder that vanished — a reboot, a kill — leaves none. Only an interrupted
// session leads with Resume.
const (
	StateLive        RecordState = "live"        // locked, and its protocol is one this rota speaks
	StateEnded       RecordState = "ended"       // unlocked, with an exit
	StateInterrupted RecordState = "interrupted" // unlocked, with no exit
	StateUnreadable  RecordState = "unreadable"  // locked, made by a rota this one cannot read
	StateBroken      RecordState = "broken"      // meta.json missing, or not a record and nobody holding the lock
)

// Record is one session's directory as Scan found it. Stopped is when it
// stopped (StoppedAt). Err says why a record is broken or unreadable.
//
// An unreadable record is a holder running that this rota cannot read: no
// major agrees with the one its record names, or the record itself is not
// one this rota can parse, a later rota's or a damaged one. Its Meta is
// never nil, but for a record that could not be parsed it holds only what
// could still be read of it (see identity).
//
// Locked is what the lock said when the record was judged, whatever the
// state. A broken record can be locked: a holder starting holds its lock
// before it writes meta.json. Nothing is deleted from a locked directory,
// and the answer is here so that a sweep need not ask the lock again.
type Record struct {
	ID, Dir string
	Meta    *Meta
	State   RecordState
	Stopped time.Time
	Locked  bool
	Err     error
}

// ErrHeld is a lock somebody else holds.
var ErrHeld = errors.New("held by another process")

// ErrPlatform is the answer where held sessions do not run. It is proc's
// error, so the sentence is written once and errors.Is finds either.
var ErrPlatform = proc.ErrPlatform

// Lock takes a session's lock for the holder's whole life, without waiting:
// a session is one holder's, and a second holder for it is a mistake to
// report, not a turn to wait for. The lock goes when release is called or
// the process ends, however it ends. It is not inherited by the child CLI.
func Lock(dir string) (release func(), err error) {
	f, err := tryLock(LockPath(dir))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", LockPath(dir), err)
	}
	return sync.OnceFunc(func() { unlock(f) }), nil
}

// Scan is every session record in a store's terminals directory, newest
// first. Each directory is judged on its own, so one broken record hides
// nothing, and the scan as a whole never fails: an unreadable directory is
// a store with no records.
func Scan(terminals string) []Record {
	entries, err := os.ReadDir(terminals)
	if err != nil {
		return nil
	}
	type found struct {
		r  Record
		at time.Time
	}
	var all []found
	for _, e := range entries {
		// Directories only, so a symlink is never followed out of the
		// store, and only names a session could have.
		if !e.IsDir() || !IsID(e.Name()) {
			continue
		}
		r := judge(terminals, e.Name())
		at := time.Time{}
		if r.Meta != nil {
			at = r.Meta.Started
		}
		if at.IsZero() {
			if fi, err := e.Info(); err == nil {
				at = fi.ModTime()
			}
		}
		all = append(all, found{r, at})
	}
	if len(all) == 0 {
		return nil
	}
	slices.SortFunc(all, func(a, b found) int {
		if c := b.at.Compare(a.at); c != 0 {
			return c
		}
		return cmp.Compare(b.r.ID, a.r.ID)
	})
	out := make([]Record, len(all))
	for i, f := range all {
		out[i] = f.r
	}
	return out
}

// judge is one directory's record. The lock is looked at before meta.json,
// because a holder writes its ending and then lets go of the lock: read the
// other way round, a session ending at that moment would be read with no
// exit and then no lock — interrupted, which it was not.
func judge(terminals, id string) (r Record) {
	r = Record{ID: id, Dir: Dir(terminals, id)}
	defer func() {
		if p := recover(); p != nil {
			r.Meta, r.State, r.Err = nil, StateBroken, fmt.Errorf("holdwire: judging %s: %v", id, p)
		}
	}()
	locked := Locked(LockPath(r.Dir))
	r.Locked = locked
	mtime := func() time.Time {
		if fi, err := os.Stat(MetaPath(r.Dir)); err == nil {
			return fi.ModTime()
		}
		return time.Time{}
	}
	m, err := ReadMeta(r.Dir)
	if err == nil && m.ID != id {
		err = fmt.Errorf("holdwire: %s names session %q", MetaPath(r.Dir), m.ID)
	}
	if err != nil && locked && !errors.Is(err, fs.ErrNotExist) {
		// A holder is behind it whose record this rota cannot read. It is
		// running all the same, and what can still be read of which
		// process it is is kept, for a Stop that checks it before it
		// signals anything.
		r.State, r.Err, r.Meta = StateUnreadable, err, identity(r.Dir, id)
		return r
	}
	if err != nil {
		r.State, r.Err, r.Stopped = StateBroken, err, mtime()
		return r
	}
	r.Meta = m
	switch {
	case locked:
		if _, ok := Agree(Majors, []int{m.Protocol}); ok {
			r.State = StateLive
		} else {
			r.State = StateUnreadable
			r.Err = fmt.Errorf("session %s speaks protocol %d, made by rota %s", id, m.Protocol, m.Version)
		}
	case m.Exit != nil:
		r.State, r.Stopped = StateEnded, StoppedAt(r.Dir, m)
	default:
		r.State, r.Stopped = StateInterrupted, StoppedAt(r.Dir, m)
	}
	return r
}

// StoppedAt is when a session whose holder has gone stopped, by its record:
// the ending it records, or with none the last time meta.json was touched —
// by its holder, which touches it while it runs, or by the daemon that saw
// it go. A daemon describing a session it held reads it here too, so the
// moment is the same before and after the daemon stops keeping it.
func StoppedAt(dir string, m *Meta) time.Time {
	if m != nil && m.Exit != nil && !m.Ended.IsZero() {
		return m.Ended
	}
	if fi, err := os.Stat(MetaPath(dir)); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// Judge is the record of the one session id names, judged as Scan judges
// each, for a caller after that session rather than all of them. An id that
// is not one, or that names no directory — or a symlink, which Scan never
// follows out of the store either — is a directory with no record.
func Judge(terminals, id string) Record {
	dir := Dir(terminals, id)
	if fi, err := os.Lstat(dir); !IsID(id) || err != nil || !fi.IsDir() {
		return Record{ID: id, Dir: dir, State: StateBroken,
			Err: &fs.PathError{Op: "judge", Path: dir, Err: fs.ErrNotExist}}
	}
	return judge(terminals, id)
}

// identity is what can still be read of a record that cannot be read whole:
// which process holds the session and which rota made it, each field on its
// own, so one that cannot be read costs only itself. The pid comes only with
// the start time and boot id it must be checked against, and only from a
// record that names this session — another session's would be its holder,
// not this one's. Never nil.
func identity(dir, id string) *Meta {
	m := &Meta{ID: id}
	raw, err := readMeta(dir)
	if err != nil {
		return m
	}
	var fields map[string]jsontext.Value
	if jsonv2.Unmarshal(raw, &fields, rota.LenientOptions()) != nil {
		return m
	}
	field := func(name string, v any) bool {
		raw, ok := fields[name]
		return ok && jsonv2.Unmarshal(raw, v, rota.LenientOptions()) == nil
	}
	var named string
	if !field("id", &named) || named != id {
		return m
	}
	var (
		pid   int
		start time.Time
		boot  string
	)
	if field("holder_pid", &pid) && field("holder_start", &start) && field("boot_id", &boot) {
		m.HolderPID, m.HolderStart, m.BootID = pid, start, boot
	}
	var protocol int
	if field("protocol", &protocol) {
		m.Protocol = protocol
	}
	var version string
	if field("version", &version) {
		m.Version = version
	}
	return m
}

// Holding is the newest running session on an account, or nil. A holder
// this rota cannot speak to still holds its account, so it counts.
func Holding(terminals string, account int) *Record {
	for _, r := range Scan(terminals) {
		if r.Running() && r.Meta.Kind == KindAccount && r.Meta.Account == account {
			return &r
		}
	}
	return nil
}

// HeldBy is what a busy account's refusal ends with when a held session is
// what has the account: " — held by session <name> (<id>)", naming the
// newest running one, and "" when none runs it. The daemon and the command
// line both say it, in these words.
func HeldBy(terminals string, account int) string {
	r := Holding(terminals, account)
	if r == nil {
		return ""
	}
	return " — held by session " + r.Name() + " (" + r.ID + ")"
}

// Name is what a session is called by its record: what the person called
// it, the name its CLI gave the conversation, its folder, and its id — the
// rule the daemon names a session it holds by, as far as a record goes.
func (r *Record) Name() string {
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

// Describe is the session's description from its record alone, in the
// shape the daemon describes a held session it holds in: what GET
// /v1/terminals lists for a session the daemon is not linked to, and what
// the command line prints with --json, so one reader reads all three. It
// says what Resume and Move need: whether it stopped and how, whether it
// can be resumed, and on which conversation in which folder. labels are the
// accounts' names by id.
func (r *Record) Describe(labels map[int]string) map[string]any {
	doc := map[string]any{
		"id": r.ID, "held": true, "name": r.Name(), "title": "",
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
		if m.Kind != KindShell && m.Account != 0 {
			doc["account"] = map[string]any{"id": m.Account, "label": labels[m.Account], "provider": m.Provider}
		}
	}
	switch {
	case r.Running():
		doc["state"], doc["status"], doc["ended"] = string(StateLive), "running", false
		doc["reachable"], doc["pid"] = false, m.HolderPID
		doc["attachable"] = r.State == StateLive
		if r.State == StateUnreadable {
			doc["made_by"] = MadeBy(m)
		}
		if !m.TokenUntil.IsZero() {
			doc["token_until"] = m.TokenUntil.Format(time.RFC3339)
		}
	default:
		doc["state"], doc["status"], doc["ended"] = string(r.State), "stopped", true
		doc["attachable"], doc["reachable"] = false, false
		doc["exit"] = r.ExitCode()
		if !r.Stopped.IsZero() {
			doc["ended_at"] = r.Stopped.Format(time.RFC3339)
		}
		doc["resumable"] = r.Resumable()
	}
	return doc
}

// MadeBy is the rota a holder this one cannot speak to says made it, as far
// as its record can be read.
func MadeBy(m *Meta) string {
	switch {
	case m.Version != "":
		return m.Version
	case m.Protocol != 0:
		return fmt.Sprintf("unknown (protocol %d)", m.Protocol)
	}
	return "unknown"
}

// Running reports whether a holder runs the session: a live one, or one
// held by a rota this one cannot read, which runs all the same. Every other
// state is stopped, or never started.
func (r *Record) Running() bool { return r.State == StateLive || r.State == StateUnreadable }

// ExitCode is how a stopped session's CLI ended: its exit, or -1 for one
// that recorded none — interrupted, its holder gone first.
func (r *Record) ExitCode() int {
	if r.Meta != nil && r.Meta.Exit != nil {
		return *r.Meta.Exit
	}
	return -1
}

// Resumable reports whether Resume can start the session again: it was
// interrupted, on a conversation its record knows.
func (r *Record) Resumable() bool {
	return r.State == StateInterrupted && r.Meta != nil && r.Meta.Conversation != ""
}

// IsID says whether s is a session id: sixteen lower-case hex characters,
// as runID makes, and so a single path element that cannot leave the
// terminals directory. A directory under it whose name is not one is not a
// session, so the holder refuses to make one and Scan passes it by.
func IsID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
