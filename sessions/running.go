package sessions

import (
	jsonv2 "encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Instance is a vendor CLI or an editor running right now.
type Instance struct {
	Kind     string    `json:"kind"` // "cli", or the editor's own name
	Provider string    `json:"provider,omitempty"`
	Account  int       `json:"account,omitempty"` // 0 when nothing can attribute it
	Label    string    `json:"label,omitempty"`
	Dir      string    `json:"dir,omitempty"`
	PID      int       `json:"pid,omitempty"`
	Session  string    `json:"session,omitempty"`
	Since    time.Time `json:"since,omitzero"`
}

// runningFile is where rota writes down what it launched, inside ROTA_HOME
// beside the accounts. runningLock is held while it changes; a separate file
// so the data can be replaced whole without the lock moving with it.
const (
	runningFile = "running.json"
	runningLock = "running.lock"
)

// guard serializes changes inside one process, as the file lock does between
// them. Both are needed: a server runs several agents at once in one process,
// and a person can run rota in another terminal at the same time. Each caller
// makes its own Registry, so the mutex cannot live on one.
var guard sync.Mutex

/* ------------------------------------------------------ editors on disk --- */

// IDEInstances reports the editors that have Claude Code open under home.
//
// Each writes <home>/ide/<port>.lock naming the workspace it has open and the
// process holding it. Nothing removes a lock but the editor that wrote it, so
// one whose process is gone is dropped rather than reported as running.
//
// Those files also hold the token that editor authenticates with. rota reads
// the three fields it needs and never the fourth: a credential belonging to
// another program has no reason to pass through here, let alone reach a
// terminal or a JSON reply.
func IDEInstances(home string) []Instance {
	entries, err := os.ReadDir(filepath.Join(home, "ide"))
	if err != nil {
		return nil
	}
	var out []Instance
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(home, "ide", e.Name()))
		if err != nil {
			continue
		}
		// Only these fields are named, so the token is never even decoded.
		var lock struct {
			WorkspaceFolders []string `json:"workspaceFolders"`
			PID              int      `json:"pid"`
			IDEName          string   `json:"ideName"`
		}
		if jsonv2.Unmarshal(blob, &lock) != nil || !alive(lock.PID) {
			continue
		}
		in := Instance{Kind: lock.IDEName, PID: lock.PID}
		if in.Kind == "" {
			in.Kind = "editor"
		}
		if len(lock.WorkspaceFolders) > 0 {
			in.Dir = lock.WorkspaceFolders[0]
		}
		out = append(out, in)
	}
	return out
}

/* ----------------------------------------------- what rota itself began --- */

// Registry is the list of runs rota started, kept in Dir.
//
// It exists because nothing else can answer "which account is this". By
// default every Claude Code account reads the same ~/.claude, so a process
// list and a set of transcripts both show the work without showing whose
// quota is paying for it. rota knows, because rota launched it.
type Registry struct{ Dir string }

// Add records a run and returns the function that takes it off the list.
//
// The pid matters more than it looks: a run that hands the terminal over
// replaces rota with the vendor CLI through execve, which keeps the same
// process id. The entry written before the handover therefore goes on
// describing the CLI that took rota's place, and is cleaned up by the pid
// check rather than by the returned function, which that process never
// reaches.
func (r *Registry) Add(in Instance) (*Run, error) {
	if in.Kind == "" {
		in.Kind = "cli"
	}
	if in.Since.IsZero() {
		in.Since = time.Now()
	}
	if in.PID == 0 {
		in.PID = os.Getpid()
	}
	err := r.change(func(list []Instance) []Instance {
		// A server runs several agents at once inside one process, so the
		// process id does not name one of them. The moment it started
		// completes the key, and is nudged if two began in the same
		// nanosecond — cheaper than a second identifier on every entry, and
		// the ordering it implies is true either way.
		for taken(list, in.PID, in.Since) {
			in.Since = in.Since.Add(time.Nanosecond)
		}
		return append(list, in)
	})
	return &Run{reg: r, pid: in.PID, since: in.Since}, err
}

// Run is one recorded run, and the way to say anything more about it.
//
// It is a handle rather than a process id because a server has several going
// at once and they all share one. A nil Run does nothing, so a caller that
// could not record its run does not have to check before every use.
type Run struct {
	reg   *Registry
	pid   int
	since time.Time
}

// End takes the run off the list.
func (h *Run) End() error {
	if h == nil || h.reg == nil {
		return nil
	}
	return h.reg.remove(h.pid, h.since)
}

// taken reports whether an entry already has this key.
func taken(list []Instance, pid int, since time.Time) bool {
	return slices.ContainsFunc(list, func(in Instance) bool {
		return in.PID == pid && in.Since.Equal(since)
	})
}

// Running is what rota started and has not finished, and prunes what died.
//
// A killed run never gets to remove its own entry, so whoever reads the file
// next is what keeps one crash from leaving a ghost in the list for good.
func (r *Registry) Running() []Instance {
	var live []Instance
	_ = r.change(func(list []Instance) []Instance {
		live = make([]Instance, 0, len(list))
		for _, in := range list {
			if alive(in.PID) {
				live = append(live, in)
			}
		}
		return live
	})
	return live
}

// Learned fills in the conversation this run turned out to be in.
//
// A run has no session id when it starts: the CLI decides it and says so in
// its first events, by which time the entry describing the run is already
// written. Without this, a listing could say which account is spending and
// not which conversation it is spending on, which is half the question.
//
// The first id wins. A run is one conversation, so a later event naming a
// different one is about something else, and overwriting would make the entry
// follow whatever was mentioned last. Failing to record it is not worth
// reporting: the run matters and this is a label on it.
func (h *Run) Learned(session string) {
	if h == nil || h.reg == nil || session == "" {
		return
	}
	_ = h.reg.change(func(list []Instance) []Instance {
		for i := range list {
			if list[i].PID == h.pid && list[i].Since.Equal(h.since) && list[i].Session == "" {
				list[i].Session = session
				break
			}
		}
		return list
	})
}

func (r *Registry) remove(pid int, since time.Time) error {
	return r.change(func(list []Instance) []Instance {
		return slices.DeleteFunc(list, func(in Instance) bool {
			return in.PID == pid && in.Since.Equal(since)
		})
	})
}

// change reads the list, hands it to fn, and writes back what comes out, with
// nobody else touching the file in between.
//
// Everything that writes goes through here. Read, modify, write is a lost
// update the moment two runs start together: both read the same list, both
// write one missing the other, and a server that takes eight at once loses
// nearly all of them. Measured, before this existed: one of twenty-four
// concurrent runs survived.
func (r *Registry) change(fn func([]Instance) []Instance) error {
	guard.Lock()
	defer guard.Unlock()
	lock, err := lockFile(filepath.Join(r.Dir, runningLock))
	if err != nil {
		return err
	}
	defer lock.Close() // releases the lock
	return r.save(fn(r.load()))
}

// load reads the file, and treats anything it cannot parse as empty. This is
// written by every run and read by every list, so a shape it does not
// understand must not take the command down with it.
func (r *Registry) load() []Instance {
	blob, err := os.ReadFile(filepath.Join(r.Dir, runningFile))
	if err != nil {
		return nil
	}
	var list []Instance
	if jsonv2.Unmarshal(blob, &list) != nil {
		return nil
	}
	return list
}

// save replaces the file, or removes it when there is nothing left to say.
//
// The write goes to a temporary file and is renamed over the real one, so a
// reader never sees a half-written list. Only writers take the lock, and a
// listing is not worth blocking a run for.
func (r *Registry) save(list []Instance) error {
	path := filepath.Join(r.Dir, runningFile)
	if len(list) == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	blob, err := jsonv2.Marshal(list)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.Dir, "running-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // harmless once the rename has happened
	if _, err := tmp.Write(blob); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
