package store

import (
	"os"
	"runtime"
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
// So the record also says when its writer started, in `procStart`: Claude
// Code's reading of its own start, `ps -o lstart=` under LC_ALL=C TZ=UTC
// (2.1.317 to 2.1.322 alike), such as "Tue Nov 10 09:14:07 2026". Claude
// Code tells its own records from leftovers by it, and so does rota: a
// process now at the pid that started after the record's writer did cannot
// be that writer.
//
// procStart is believed only when the record's own `startedAt` — the moment
// the writer made the record, in milliseconds since the epoch, which has no
// zone to get wrong — agrees with it. A Claude Code that wrote procStart in
// its local time instead would otherwise make every live session west of
// UTC read as started hours after its record, and rota would refresh under
// it. And it is believed only of a record written in this machine's pid
// space (`pidDomain`): a pid from another host or container sharing the
// home names nothing here.
//
// Every doubt is read as alive, as everywhere else in this file: a record
// with no procStart or no startedAt, one whose two disagree, one from
// another pid space, one in a layout Claude Code does not write, and a
// process whose start cannot be read (on Windows Claude Code writes a
// procStartFt that rota does not read) are kept, as before.

// procStartSlack is how much later than its record a process may have
// started and still be the record's writer. procStart is whole seconds and
// each reading rounds its own way, and on Linux both are made from a boot
// time that wanders by a second between readings; a pid given to another
// process is minutes or days later, after a reboot or a wrap of the pids.
const procStartSlack = 2 * time.Minute

// madeWithin is how long after its writer started a record may have been
// made and procStart still be believed: Claude Code writes both within
// moments of starting.
const madeWithin = 10 * time.Minute

// namesWait bounds the look at what a process is called: on Linux a read of
// its command line waits on the process's memory, and a process stuck in
// the kernel holds that for as long as it is stuck.
const namesWait = 2 * time.Second

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
	// for all rota can tell. A look that does not end in namesWait could be
	// one; what it was waiting on is left to finish on its own.
	looksLikeClaude = func(pid int) bool {
		got := make(chan bool, 1)
		go func() { got <- claudeIn(namesOf(pid)) }()
		select {
		case c := <-got:
			return c
		case <-time.After(namesWait):
			return true
		}
	}
	// namesOf is every name the process at a pid goes by.
	namesOf = proc.Names
	// ourPidDomain is this machine's pid space as Claude Code names it.
	ourPidDomain = sync.OnceValue(thisPidDomain)
)

// recycled reports whether the process now at a record's pid is not the one
// that wrote the record: it started more than procStartSlack after the
// record's writer did. Where a start moves with the clock, a process that
// may be a Claude Code is never called recycled, since a clock step alone
// makes it look so; one that is not cannot have written the record.
func recycled(r record) bool {
	recorded, err := time.Parse(time.ANSIC, strings.TrimSpace(r.procStart))
	if err != nil || r.startedAt <= 0 {
		return false
	}
	made := time.UnixMilli(r.startedAt)
	if recorded.Before(made.Add(-madeWithin)) || recorded.After(made.Add(procStartSlack)) {
		return false
	}
	if r.pidDomain != "" && r.pidDomain != ourPidDomain() {
		return false
	}
	started, err := startOf(r.pid)
	if err != nil || !started.After(recorded.Add(procStartSlack)) {
		return false
	}
	return !startsMove || !looksLikeClaude(r.pid)
}

// claudeIn says whether a process is a Claude Code by the names it goes by:
// any of them names claude, in any case — the native build is run as claude,
// titles itself claude, and is kept under .../claude/versions/; the npm one
// is cli.js in a claude-code package. A process none of whose names can be
// read could be one.
func claudeIn(names []string, err error) bool {
	if err != nil {
		return true
	}
	for _, a := range names {
		if strings.Contains(strings.ToLower(a), "claude") {
			return true
		}
	}
	return false
}

// thisPidDomain is the pidDomain Claude Code would write here: "darwin" on
// macOS, and on Linux the machine id and the pid namespace,
// "darwin:<machine-id>:pid:[<inode>]", each empty when it cannot be read,
// as Claude Code leaves it. Elsewhere rota does not know it, and a record
// naming any domain is a doubt.
func thisPidDomain() string {
	switch runtime.GOOS {
	case "darwin":
		return "darwin"
	case "linux":
		id, _ := os.ReadFile("/etc/machine-id")
		ns, _ := os.Readlink("/proc/self/ns/pid")
		return "darwin:" + strings.TrimSpace(string(id)) + ":" + ns
	}
	return ""
}
