package store

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/proc"
)

// A record Claude Code keeps in a home — `daemon.lock`, `sessions/<pid>.json`
// — names a pid, and the system hands a pid on once its process has gone:
// after a crash or a reboot, a record left behind can name a live process
// that has nothing to do with Claude Code, and the home would read as busy
// for as long as that process runs. Nothing would refresh its login and
// every env-route run would be refused over a session that does not exist.
//
// So the record also says when its writer started, in `procStart`, and
// Claude Code tells its own records from leftovers by it. Each of its builds
// writes the start its own system keeps (seen in 2.1.317 to 2.1.322):
//
//   - Linux: field 22 of /proc/<pid>/stat, the start in clock ticks since
//     boot, as digits, such as "48273615". The kernel fixes it for the
//     process's life and no setting of the clock moves it, so the process at
//     the pid is the writer exactly when its own start is the same number.
//   - macOS: `ps -o lstart=` under LC_ALL=C TZ=UTC, such as "Tue Nov 10
//     09:14:07 2026": the time the kernel wrote down at fork, which no
//     setting of the clock moves either. A process at the pid that started
//     more than procStartSlack after it cannot be the writer. This one is
//     believed only when the record's own `startedAt` — the moment the
//     writer made the record, in milliseconds since the epoch, which has no
//     zone to get wrong — agrees with it: a procStart written in local time
//     instead would otherwise make every live session west of UTC read as
//     started hours after its record, and rota would refresh under it.
//
// Either is believed only of a record that says it was written in this
// machine's pid space, `pidDomain`: a pid from another host or container
// sharing the home names nothing here, and a record that names no pid space
// at all, from a build older than the field, is judged by its pid alone, as
// before rota read procStart.
//
// Every doubt is read as alive, as everywhere else in this file: a record
// with no procStart or no pidDomain, one in the shape another system
// writes, a macOS one with no startedAt or one that disagrees with it, one
// from another pid space, and a process whose start cannot be read (on
// Windows Claude Code writes a procStartFt that rota does not read) are
// kept, as before.

// procStartSlack is how much later than its macOS record a process may have
// started and still be the record's writer. procStart is whole seconds and
// each reading rounds its own way; a pid given to another process is minutes
// or days later, after a reboot or a wrap of the pids.
const procStartSlack = 2 * time.Minute

// madeWithin is how long after its writer started a macOS record may have
// been made and procStart still be believed: Claude Code writes both within
// moments of starting.
const madeWithin = 10 * time.Minute

var (
	// startsInTicks says whether this system's records give their writer's
	// start in clock ticks since boot, as Claude Code's Linux build writes
	// them, rather than as a time.
	startsInTicks = runtime.GOOS == "linux"
	// ticksOf is when the process at a pid started, in clock ticks since
	// boot.
	ticksOf = proc.StartTicks
	// startOf is when the process at a pid started.
	startOf = proc.StartTime
	// ourPidDomain is this machine's pid space as Claude Code names it.
	ourPidDomain = sync.OnceValue(thisPidDomain)
)

// recycled reports whether the process now at a record's pid is provably
// not the one that wrote the record.
func recycled(r record) bool {
	if r.procStart == "" || r.pidDomain == "" || r.pidDomain != ourPidDomain() {
		return false
	}
	if startsInTicks {
		return ticksRecycled(r)
	}
	return lstartRecycled(r)
}

// ticksRecycled is recycled for a Linux record: its procStart is a start in
// ticks, and the process at its pid started at another tick.
func ticksRecycled(r record) bool {
	if strings.Trim(r.procStart, "0123456789") != "" {
		return false
	}
	recorded, err := strconv.ParseInt(r.procStart, 10, 64)
	if err != nil || recorded <= 0 {
		return false
	}
	ticks, err := ticksOf(r.pid)
	return err == nil && ticks > 0 && ticks != recorded
}

// lstartRecycled is recycled for a macOS record: its procStart is a time in
// UTC that its startedAt bears out, and the process at its pid started more
// than procStartSlack after it.
func lstartRecycled(r record) bool {
	recorded, err := time.Parse(time.ANSIC, strings.TrimSpace(r.procStart))
	if err != nil || r.startedAt <= 0 {
		return false
	}
	made := time.UnixMilli(r.startedAt)
	if recorded.Before(made.Add(-madeWithin)) || recorded.After(made.Add(procStartSlack)) {
		return false
	}
	started, err := startOf(r.pid)
	return err == nil && started.After(recorded.Add(procStartSlack))
}

// thisPidDomain is the pidDomain Claude Code writes here: "darwin" on
// macOS, and on Linux "linux:<machine-id>:<pid namespace>", the namespace
// as /proc/self/ns/pid links it, such as "pid:[4026531836]", each part
// empty when it cannot be read, as Claude Code leaves it. Elsewhere rota
// does not know it, and a record naming any domain is a doubt.
func thisPidDomain() string {
	switch runtime.GOOS {
	case "darwin":
		return "darwin"
	case "linux":
		id, _ := os.ReadFile("/etc/machine-id")
		ns, _ := os.Readlink("/proc/self/ns/pid")
		return "linux:" + strings.TrimSpace(string(id)) + ":" + ns
	}
	return ""
}
