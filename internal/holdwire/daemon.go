package holdwire

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/professor93/rota/internal/proc"
	rota "github.com/professor93/rota/lib"
)

// Daemon is who serves a store, as daemon.pid says. ProcStart,
// ProcStartTicks and BootID are there so that its pid is signalled only
// while it is still the same process (SameProcess): a pid file left from
// before a reboot names some other program. ProcStartTicks is Linux's alone.
// HTTP is the TCP address it serves its API on besides its control socket,
// once it has said so with RecordHTTP; empty for a daemon on ctl.sock alone.
type Daemon struct {
	PID            int       `json:"pid"`
	Version        string    `json:"version"`
	Started        time.Time `json:"started"`
	ProcStart      time.Time `json:"proc_start"`
	ProcStartTicks int64     `json:"proc_start_ticks,omitempty"`
	BootID         string    `json:"boot_id"`
	HTTP           string    `json:"http,omitempty"`
}

// DaemonRunning is a claim refused because another daemon holds the store
// whose directory is Store. PID is that daemon's, or 0 when its pid file
// could not be read.
type DaemonRunning struct {
	PID   int
	Store string
}

func (d DaemonRunning) Error() string {
	return fmt.Sprintf("another rota daemon serves %s (pid %d)", d.Store, d.PID)
}

// ClaimDaemon makes this process the one daemon of the store whose
// terminals directory this is, or says which process already is. The claim
// is daemon.lock's flock — only the lock proves a daemon is alive — and
// daemon.pid says who holds it. Release removes daemon.pid and lets go of
// the lock, in that order, so a pid file is never left naming a daemon that
// has gone while another has come.
func ClaimDaemon(terminals, version string) (release func(), err error) {
	if err := os.MkdirAll(terminals, 0o700); err != nil {
		return nil, err
	}
	f, err := tryLock(DaemonLock(terminals))
	if errors.Is(err, ErrHeld) {
		return nil, DaemonRunning{PID: claimant(terminals), Store: filepath.Dir(terminals)}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", DaemonLock(terminals), err)
	}
	d := Daemon{PID: os.Getpid(), Version: version, Started: time.Now()}
	// Without these the daemon still runs; it is only never signalled by
	// pid, since SameProcess refuses a record with either missing.
	d.ProcStart, _ = proc.StartTime(d.PID)
	d.ProcStartTicks, _ = proc.StartTicks(d.PID)
	d.BootID, _ = proc.BootID()
	raw, err := rota.EncodeIndent(&d)
	if err == nil {
		err = writeAtomic(DaemonPIDFile(terminals), append(raw, '\n'))
	}
	if err != nil {
		unlock(f)
		return nil, err
	}
	return sync.OnceFunc(func() {
		_ = os.Remove(DaemonPIDFile(terminals))
		unlock(f)
	}), nil
}

// RecordHTTP adds to daemon.pid the TCP address the daemon serves its API on
// besides ctl.sock, so that whoever asks which daemon runs is also told
// where else it can be reached. Only the daemon itself may: a daemon.pid
// that does not name this process, or whose lock nobody holds, is not this
// process's claim, and nothing is written.
func RecordHTTP(terminals, addr string) error {
	d, err := readDaemon(terminals)
	if err != nil {
		return err
	}
	if d.PID != os.Getpid() || !Locked(DaemonLock(terminals)) {
		return fmt.Errorf("holdwire: %s is not this process's claim", DaemonPIDFile(terminals))
	}
	d.HTTP = addr
	raw, err := rota.EncodeIndent(d)
	if err != nil {
		return err
	}
	return writeAtomic(DaemonPIDFile(terminals), append(raw, '\n'))
}

// claimant is the pid of the daemon holding the lock. A daemon writes its
// pid file just after taking the lock, so a claim that lost that race waits
// a moment for it rather than reporting nobody. Until then the file may be
// one a crashed daemon left, so only a record of a process still running is
// believed; after the wait, nobody is named rather than a pid that may now
// be anybody's.
func claimant(terminals string) int {
	for try := 0; try < 20; try++ {
		if d := currentDaemon(terminals); d != nil {
			return d.PID
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}

// RunningDaemon is the daemon serving a store, if one is: true only while
// somebody holds daemon.lock. The Daemon it returns is never nil when true,
// but is empty until daemon.pid names the process that is running, which
// it does not while the daemon has yet to write who it is, nor ever for a
// daemon that could not read its own start time or boot.
func RunningDaemon(terminals string) (*Daemon, bool) {
	if !Locked(DaemonLock(terminals)) {
		return nil, false
	}
	if d := currentDaemon(terminals); d != nil {
		return d, true
	}
	return &Daemon{}, true
}

// currentDaemon is what daemon.pid says when it is about a process running
// now, or nil. A daemon that crashed leaves its file behind, and the next
// one overwrites it only after it has taken the lock: in between, the file
// names a process that has gone, or a pid since given to another.
func currentDaemon(terminals string) *Daemon {
	d, err := readDaemon(terminals)
	if err != nil || !SameProcess(d.PID, d.ProcStart, d.ProcStartTicks, d.BootID) {
		return nil
	}
	return d
}

// IsDaemon says whether pid is the daemon serving the store: somebody holds
// daemon.lock, and daemon.pid names pid. It is for a caller the kernel has
// told pid, as a socket's peer: that process is running now, so nothing is
// asked of its start, which a clock set since would make look like another
// process's. The only record that could name pid wrongly is one a crashed
// daemon left, for the moment between the next daemon taking the lock and
// writing its own, and only should the crashed daemon's pid have been given
// to a process of this user's in that moment.
func IsDaemon(terminals string, pid int) bool {
	if pid <= 0 || !Locked(DaemonLock(terminals)) {
		return false
	}
	d, err := readDaemon(terminals)
	return err == nil && d.PID == pid
}

func readDaemon(terminals string) (*Daemon, error) {
	raw, err := os.ReadFile(DaemonPIDFile(terminals))
	if err != nil {
		return nil, err
	}
	var d Daemon
	if err := jsonv2.Unmarshal(raw, &d, rota.LenientOptions()); err != nil {
		return nil, err
	}
	return &d, nil
}

// What SameProcess asks the system, as variables so that a test can be a
// system whose clock it sets.
var (
	startTime  = proc.StartTime
	startTicks = proc.StartTicks
	bootID     = proc.BootID
)

// SameProcess says whether pid is still the process that was recorded with
// that start in that boot. It is asked before any signal is sent to a
// recorded pid: a pid is a number the kernel hands out again, and the
// process that has it now may be anybody's.
//
// A record with ticks, which Linux keeps, is compared by those alone and
// exactly: Linux's start time is made from a boot time that every setting of
// the clock moves, so after one a process would no longer be itself. One
// without is compared by start time, within a second, since what records it
// is no finer than that; macOS writes its start time down once and never
// moves it. Anything missing or unreadable is not the same process.
func SameProcess(pid int, start time.Time, ticks int64, boot string) bool {
	if pid <= 0 || (start.IsZero() && ticks == 0) || boot == "" {
		return false
	}
	if now, err := bootID(); err != nil || now != boot {
		return false
	}
	if ticks != 0 {
		now, err := startTicks(pid)
		return err == nil && now == ticks
	}
	now, err := startTime(pid)
	if err != nil {
		return false
	}
	d := now.Sub(start)
	return d >= -time.Second && d <= time.Second
}
