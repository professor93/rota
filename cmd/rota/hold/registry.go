package hold

import (
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
)

// Registry is what Claude Code says about one of its sessions: which
// conversation it is in, what that conversation is called, the folder it
// really works in, and whether it is idle or waiting for the person. It is
// the protocol's own type, so what is read is what the daemon is told.
// UpdatedAt is the file's updatedAt as written: milliseconds since the epoch.
type Registry = holdwire.Registry

// registryFile is one <home>/sessions/<pid>.json read: what it says about
// the session, and which process says it wrote it.
type registryFile struct {
	Registry
	PID int
	// ProcStart and ProcTicks are the writer's own start, as Claude Code's
	// macOS build writes it, a time, and as its Linux build does, clock
	// ticks since boot. Each is zero when the file gave none in its form.
	ProcStart time.Time
	ProcTicks int64
	// PidDomain is the pid space the writer says it ran in, and NamesDomain
	// whether the file has a pidDomain at all: one that is there but not a
	// string, or longer than maxField, is named and empty.
	PidDomain   string
	NamesDomain bool
}

// registrySlack is how far a macOS file's procStart may be from the
// child's start and still be the child's. procStart holds whole seconds and
// the child's start may not, so a second either way is the reading's own
// error; a pid reused by another claude later is minutes or hours off.
const registrySlack = time.Second

var (
	// startsInTicks says whether a file written here gives its writer's
	// start in clock ticks since boot, as Claude Code's Linux build writes
	// it, rather than as a time, as its macOS build does.
	startsInTicks = runtime.GOOS == "linux"
	// ourPidDomain is this machine's pid space as Claude Code names it.
	ourPidDomain = sync.OnceValue(proc.PidDomain)
)

// waitingUnknown is what a waitingFor nobody can name reads as. The file
// says the session waits, so it waits: Enter is never typed at a prompt rota
// failed to recognise.
const waitingUnknown = "unknown"

// maxField bounds each string a registry file gives, as the scanner bounds a
// title. Claude Code writes a few dozen bytes in each; a longer one is not
// believed, so that the State every link begins with always fits in a frame,
// even with each string at the bound and written six bytes to the byte.
const maxField = 4096

// registryPath is where Claude Code keeps the record of the session running
// as pid in an account's home.
func registryPath(home string, pid int) string {
	return filepath.Join(home, "sessions", strconv.Itoa(pid)+".json")
}

// parseRegistry reads one registry file. The file is undocumented, so each
// key is read on its own and one that is missing, or not what Claude Code
// 2.1.318 writes there, leaves only its own field empty, as does a string
// longer than maxField. Only a file that is
// not one JSON object — one caught half written — is an error, so the caller
// keeps what it last knew.
func parseRegistry(b []byte) (registryFile, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return registryFile{}, err
	}
	if m == nil {
		return registryFile{}, errors.New("a session registry file holds no JSON object")
	}
	f := registryFile{
		Registry: Registry{
			SessionID:  str(m["sessionId"]),
			Name:       str(m["name"]),
			Cwd:        str(m["cwd"]),
			Status:     str(m["status"]),
			WaitingFor: waitingFor(m["waitingFor"]),
			Kind:       str(m["kind"]),
			UpdatedAt:  number(m["updatedAt"]),
		},
		PID: int(number(m["pid"])),
	}
	// procStart is the writer's start as Claude Code's build for its system
	// writes it. On macOS (2.1.317 to 2.1.322) it is what `ps -o lstart= -p
	// <pid>` prints under LC_ALL=C TZ=UTC: the start in UTC, whatever zone
	// it runs in. On Linux (as 2.1.319 writes it) it is field 22 of
	// /proc/<pid>/stat: the start in clock ticks since boot, as digits.
	start := str(m["procStart"])
	if strings.Trim(start, "0123456789") == "" {
		if n, err := strconv.ParseInt(start, 10, 64); err == nil && n > 0 {
			f.ProcTicks = n
		}
	} else if t, err := time.Parse(time.ANSIC, start); err == nil {
		f.ProcStart = t
	}
	domain, named := m["pidDomain"]
	f.PidDomain, f.NamesDomain = str(domain), named
	return f, nil
}

// belongsTo says whether the file is the one written by the child that is
// pid and started at start, or at tick ticks: it names no other pid and no
// other pid space, and its procStart is the child's start in the form
// Claude Code writes on this system, on Linux the very tick and on macOS a
// time within registrySlack. A start in the other system's form, or none,
// is never the child's: a session that cannot tell its file for its own
// learns nothing from it, never another session's conversation. As in
// Claude Code's own rule, any pidDomain the file gives that is not this
// machine's, one that cannot be read included, is another pid space's, and
// only a file with no pidDomain at all, as from a build older than the
// field, is judged by its start alone. A known limit, Claude Code's too: on
// Linux a file left behind before a reboot whose pid and tick happen to be
// the child's matches, until the child writes its own as it starts.
func (f registryFile) belongsTo(pid int, start time.Time, ticks int64) bool {
	if f.PID != 0 && f.PID != pid {
		return false
	}
	if f.NamesDomain && (f.PidDomain == "" || f.PidDomain != ourPidDomain()) {
		return false
	}
	if startsInTicks {
		return f.ProcTicks > 0 && f.ProcTicks == ticks
	}
	if f.ProcStart.IsZero() || start.IsZero() {
		return false
	}
	d := f.ProcStart.Sub(start)
	return d >= -registrySlack && d <= registrySlack
}

// str is a JSON string's value, and empty for anything else and for a
// string longer than maxField.
func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil || len(s) > maxField {
		return ""
	}
	return s
}

// number is a non-negative JSON number's whole part, and zero for anything
// else.
func number(raw json.RawMessage) int64 {
	var n float64
	if json.Unmarshal(raw, &n) != nil || n < 0 || n >= math.MaxInt64 {
		return 0
	}
	return int64(n)
}

// waitingFor names what the session waits for: a string as written, an
// object by its type or else its kind, nothing for null, false or an empty
// string, and waitingUnknown for any other answer, a name longer than
// maxField among them.
func waitingFor(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	switch w := v.(type) {
	case nil:
		return ""
	case bool:
		if !w {
			return ""
		}
	case string:
		if len(w) <= maxField {
			return w
		}
	case map[string]any:
		for _, k := range []string{"type", "kind"} {
			if s, ok := w[k].(string); ok && s != "" {
				if len(s) > maxField {
					break
				}
				return s
			}
		}
	}
	return waitingUnknown
}
