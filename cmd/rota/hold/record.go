//go:build linux || darwin

package hold

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/owner"
)

// What a holder leaves on disk while it runs: the session's record, kept
// current with the conversation the CLI is in, and its own log.

// The timings, variables so the reasons for them can be read in one place.
var (
	// registryEvery is how often the CLI's own registry is read.
	registryEvery = 2 * time.Second
	// touchEvery is how often meta.json is touched, so that a session whose
	// holder went without recording an ending — killed outright, a reboot,
	// the power — and with no daemon there to see it go is dated from the
	// last time it was known alive: at most this long before it went.
	touchEvery = time.Minute
)

// logCap is how large holder.log grows before it is moved to holder.log.1,
// replacing the one there.
const logCap = 1 << 20

// maxRegistry bounds what is read of a registry file, which is a few hundred
// bytes when it is what it should be.
const maxRegistry = 1 << 20

// metaFile is meta.json, written whole under one lock: the ending and the
// conversation are written from different goroutines, and neither may undo
// the other.
type metaFile struct {
	mu  sync.Mutex
	dir string
	m   holdwire.Meta
}

// update changes the record and writes it.
func (r *metaFile) update(change func(*holdwire.Meta)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	change(&r.m)
	return holdwire.WriteMeta(r.dir, &r.m)
}

// touch moves meta.json's time to now without changing a byte of it, while
// no ending is recorded: once one is, a touch never moves it, and the
// ending is what says when the session stopped.
func (r *metaFile) touch() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m.Exit != nil {
		return nil
	}
	now := time.Now()
	return os.Chtimes(holdwire.MetaPath(r.dir), now, now)
}

// keep is what the holder does while the CLI runs: the registry read every
// registryEvery, for a claude, and the record touched every touchEvery.
func (h *holder) keep() {
	defer h.sess.recovered("keeping the record")
	touch := time.NewTicker(touchEvery)
	defer touch.Stop()
	var poll <-chan time.Time
	if h.registry != "" {
		t := time.NewTicker(registryEvery)
		defer t.Stop()
		poll = t.C
	}
	var last *Registry
	for {
		select {
		case <-h.sess.Done():
			return
		case <-touch.C:
			if err := h.rec.touch(); err != nil {
				h.log("touching the record", "err", err)
			}
		case <-poll:
			h.sess.safely("reading the registry", func() { last = h.readRegistry(last) })
		}
	}
}

// readRegistry reads the CLI's registry file once and passes on what
// changed. A file that is missing, half written or another process's keeps
// what was last known: the conversation is never guessed, and never
// forgotten because a file went away.
func (h *holder) readRegistry(last *Registry) *Registry {
	raw, err := readSmall(h.registry, maxRegistry)
	if err != nil {
		return last
	}
	f, err := parseRegistry(raw, h.zone)
	if err != nil || !f.belongsTo(h.child.Pid(), h.childStart) {
		return last
	}
	reg := f.Registry
	if last != nil && *last == reg {
		return last
	}
	h.sess.SetRegistry(&reg)
	if last == nil || reg.SessionID != last.SessionID || reg.Name != last.Name || reg.Cwd != last.Cwd {
		if err := h.rec.update(func(m *holdwire.Meta) {
			if reg.SessionID != "" {
				m.Conversation = reg.SessionID
			}
			if reg.Name != "" {
				m.ConversationName = reg.Name
			}
			if reg.Cwd != "" {
				m.ConversationCwd = reg.Cwd
			}
		}); err != nil {
			h.log("recording the conversation", "err", err)
		}
	}
	return &reg
}

// registryOf is where Claude Code started with env keeps the record of the
// session that is pid, and the zone it writes that record's times in.
func registryOf(env []string, pid int) (string, *time.Location) {
	home := lookup(env, "CLAUDE_CONFIG_DIR")
	if home == "" {
		if h := lookup(env, "HOME"); h != "" {
			home = filepath.Join(h, ".claude")
		}
	}
	if home == "" {
		return "", nil
	}
	return registryPath(home, pid), zoneOf(env)
}

// zoneOf is the zone a program started with env reads its local time in:
// TZ's, as the C library reads it, or this process's own when TZ names none
// that can be loaded.
func zoneOf(env []string) *time.Location {
	tz, set := lookupOK(env, "TZ")
	if !set {
		return time.Local
	}
	tz = strings.TrimPrefix(tz, ":")
	if tz == "" || tz == "UTC" {
		return time.UTC
	}
	if filepath.IsAbs(tz) {
		if raw, err := readSmall(tz, maxRegistry); err == nil {
			if loc, err := time.LoadLocationFromTZData(tz, raw); err == nil {
				return loc
			}
		}
		return time.Local
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.Local
}

func lookup(env []string, name string) string {
	v, _ := lookupOK(env, name)
	return v
}

// lookupOK is the value env gives name, the last one where it gives several,
// as the C library's getenv would see the environment rota builds.
func lookupOK(env []string, name string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// readSmall reads a file of at most max bytes; a larger one is an error,
// not the start of one.
func readSmall(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, errors.New(path + " is larger than any record of its kind")
	}
	return raw, nil
}

// logFile is holder.log: written by every goroutine of the holder at once,
// moved aside to holder.log.1 at logCap. A log that cannot be written costs
// the log, never the session.
type logFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	// crash makes the file the one the Go runtime writes a fatal error or an
	// unrecovered panic to, besides standard error, which a holder points at
	// /dev/null: the file open now, and after a move the new one.
	crash bool
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil && l.size > 0 && l.size+int64(len(p)) > logCap {
		_ = l.f.Close()
		l.f = nil
		_ = os.Rename(l.path, l.path+".1")
	}
	if l.f == nil {
		f, err := openLog(l.path)
		if err != nil {
			return len(p), nil
		}
		l.f, l.size = f, 0
		if fi, err := f.Stat(); err == nil {
			l.size = fi.Size()
		}
		if l.crash {
			_ = debug.SetCrashOutput(f, debug.CrashOptions{})
		}
	}
	n, _ := l.f.Write(p)
	l.size += int64(n)
	return len(p), nil
}

// openLog opens a holder's log for appending as the daemon opens its own:
// never through a link, never waiting on a FIFO, and only a plain file
// nobody but its owner may read or write, so that whoever can write the
// session's directory cannot point the log at a file of the owner's.
func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|owner.OpenFlags, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a plain file", path)
	}
	if err == nil {
		err = owner.Only(path, fi.Mode())
	}
	if err == nil {
		err = owner.Blocking(f)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (l *logFile) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

// logger is the holder's log function, safe to call from any goroutine.
func (l *logFile) logger() func(msg string, args ...any) {
	lg := slog.New(slog.NewTextHandler(l, nil))
	return func(msg string, args ...any) { lg.Info(msg, args...) }
}
