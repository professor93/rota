package store

import (
	"runtime"
	"strings"
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
// So the record also says when its writer started, in `procStart`: Claude
// Code's reading of its own start, `ps -o lstart=` under LC_ALL=C TZ=UTC
// (2.1.319 to 2.1.322 alike), such as "Tue Nov 10 09:14:07 2026". Claude
// Code tells its own records from leftovers by it, and so does rota: a
// process now at the pid that started after the record's writer did cannot
// be that writer.
//
// Every doubt is read as alive, as everywhere else in this file: a record
// with no procStart, one in a layout Claude Code does not write, and a
// process whose start cannot be read (on Windows Claude Code writes a
// procStartFt that rota does not read) are kept, as before.

// procStartSlack is how much later than its record a process may have
// started and still be the record's writer. procStart is whole seconds and
// each reading rounds its own way, and on Linux both are made from a boot
// time that wanders by a second between readings; a pid given to another
// process is minutes or days later, after a reboot or a wrap of the pids.
const procStartSlack = 2 * time.Minute

var (
	// startOf is when the process at a pid started.
	startOf = proc.StartTime
	// startsMove says whether startOf moves when the clock is set. On Linux
	// a start is the boot time plus the ticks since boot, and the boot time
	// is the clock less the time since boot, so a clock stepped on — by a
	// network time sync, or by WSL catching up after the laptop slept —
	// makes a live process read as having started later than its record
	// says. On macOS a start is the time the kernel wrote down at fork, and
	// stays.
	startsMove = runtime.GOOS == "linux"
	// looksLikeClaude says whether the process at a pid is a Claude Code
	// for all rota can tell.
	looksLikeClaude = func(pid int) bool { return claudeIn(proc.Cmdline(pid)) }
)

// recycled reports whether the process now at pid is not the one that wrote
// a record with this procStart: it started more than procStartSlack after
// the record's writer did. Where a start moves with the clock, a process
// that may be a Claude Code is never called recycled, since a clock step
// alone makes it look so; one that is not cannot have written the record.
func recycled(pid int, procStart string) bool {
	recorded, err := time.Parse(time.ANSIC, strings.TrimSpace(procStart))
	if err != nil {
		return false
	}
	started, err := startOf(pid)
	if err != nil || !started.After(recorded.Add(procStartSlack)) {
		return false
	}
	return !startsMove || !looksLikeClaude(pid)
}

// claudeIn says whether a command line is a Claude Code's: any of its
// arguments names claude, in any case — the native build is run as claude
// and kept under .../claude/versions/, the npm one is cli.js in a
// claude-code package. A command line that cannot be read could be one.
func claudeIn(args []string, err error) bool {
	if err != nil {
		return true
	}
	for _, a := range args {
		if strings.Contains(strings.ToLower(a), "claude") {
			return true
		}
	}
	return false
}
