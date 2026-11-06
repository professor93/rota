package holdwire

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/professor93/rota/internal/proc"
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
	StateUnreadable  RecordState = "unreadable"  // locked, made by a rota no major agrees with
	StateBroken      RecordState = "broken"      // meta.json missing or not a record
)

// Record is one session's directory as Scan found it. Stopped is when it
// stopped: the ending meta.json records, or for an interrupted session the
// last time its holder touched meta.json. Err says why a record is broken.
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
		if !e.IsDir() || !isID(e.Name()) {
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
		r.State, r.Stopped = StateEnded, m.Ended
		if r.Stopped.IsZero() {
			r.Stopped = mtime()
		}
	default:
		r.State, r.Stopped = StateInterrupted, mtime()
	}
	return r
}

// Holding is the newest running session on an account, or nil. A holder
// this rota cannot speak to still holds its account, so it counts.
func Holding(terminals string, account int) *Record {
	for _, r := range Scan(terminals) {
		if (r.State == StateLive || r.State == StateUnreadable) && r.Meta.Kind == KindAccount && r.Meta.Account == account {
			return &r
		}
	}
	return nil
}

// isID is a session id: sixteen lower-case hex characters, as runID makes.
func isID(s string) bool {
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
