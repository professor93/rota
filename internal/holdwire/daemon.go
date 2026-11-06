package holdwire

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/professor93/rota/internal/proc"
	rota "github.com/professor93/rota/lib"
)

// Daemon is who serves a store, as daemon.pid says. ProcStart and BootID
// are there so that its pid is signalled only while it is still the same
// process: a pid file left from before a reboot names some other program.
type Daemon struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	Started   time.Time `json:"started"`
	ProcStart time.Time `json:"proc_start"`
	BootID    string    `json:"boot_id"`
}

// DaemonRunning is a claim refused because another daemon holds the store.
// PID is that daemon's, or 0 when its pid file could not be read.
type DaemonRunning struct{ PID int }

func (d DaemonRunning) Error() string {
	return fmt.Sprintf("another rota daemon serves this store (pid %d)", d.PID)
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
		return nil, DaemonRunning{PID: claimant(terminals)}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", DaemonLock(terminals), err)
	}
	d := Daemon{PID: os.Getpid(), Version: version, Started: time.Now()}
	// Without these the daemon still runs; it is only never signalled by
	// pid, since SameProcess refuses a record with either missing.
	d.ProcStart, _ = proc.StartTime(d.PID)
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
	if err != nil || !SameProcess(d.PID, d.ProcStart, d.BootID) {
		return nil
	}
	return d
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

// SameProcess says whether pid is still the process that was recorded with
// that start time in that boot. It is asked before any signal is sent to a
// recorded pid: a pid is a number the kernel hands out again, and the
// process that has it now may be anybody's. Start times are compared within
// a second, since what records them is no finer than that on every system.
// Anything missing or unreadable is not the same process.
func SameProcess(pid int, start time.Time, boot string) bool {
	if pid <= 0 || start.IsZero() || boot == "" {
		return false
	}
	if now, err := proc.BootID(); err != nil || now != boot {
		return false
	}
	now, err := proc.StartTime(pid)
	if err != nil {
		return false
	}
	d := now.Sub(start)
	return d >= -time.Second && d <= time.Second
}
