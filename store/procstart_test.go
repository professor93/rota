package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/proc"
)

// lstart is a start as Claude Code's macOS build writes it into a record's
// procStart: `ps -o lstart=` under LC_ALL=C TZ=UTC.
func lstart(t time.Time) string { return t.UTC().Format(time.ANSIC) }

// claudeRecord is a record as Claude Code writes one: its pid, its
// procStart, when it made the record (startedAt, milliseconds since the
// epoch) and the pid space it wrote in. An empty procStart or pidDomain and
// a zero startedAt are left out.
type claudeRecord struct {
	pid                  int
	procStart, pidDomain string
	startedAt            time.Time
}

func (r claudeRecord) write(t *testing.T, home, name string) {
	t.Helper()
	body := `{"pid":` + itoa(r.pid) + `,"kind":"interactive","status":"busy"`
	if r.procStart != "" {
		body += `,"procStart":` + strconv.Quote(r.procStart)
	}
	if !r.startedAt.IsZero() {
		body += `,"startedAt":` + strconv.FormatInt(r.startedAt.UnixMilli(), 10)
	}
	if r.pidDomain != "" {
		body += `,"pidDomain":` + strconv.Quote(r.pidDomain)
	}
	writeFile(t, filepath.Join(home, name), body+"}")
}

// with is r with one change made.
func (r claudeRecord) with(change func(*claudeRecord)) claudeRecord {
	change(&r)
	return r
}

// The two pid spaces the tests play: a macOS one, and a Linux one as
// Claude Code names it there.
const (
	macDomain   = "darwin"
	linuxDomain = "linux:5f0c2a1e7b3d4e8a9c612d4f8a0b3e17:pid:[4026531836]"
)

// onMac makes the store read records as on macOS; onLinux, as on Linux,
// with the process at each pid started at the tick the map gives.
func onMac(t *testing.T) {
	t.Helper()
	seam(t, &startsInTicks, false)
	seam(t, &ourPidDomain, func() string { return macDomain })
}

func onLinux(t *testing.T, ticks map[int]int64) {
	t.Helper()
	seam(t, &startsInTicks, true)
	seam(t, &ourPidDomain, func() string { return linuxDomain })
	seam(t, &ticksOf, func(pid int) (int64, error) {
		if n, ok := ticks[pid]; ok {
			return n, nil
		}
		return 0, errors.New("no such process")
	})
}

// macWrittenBy is the record Claude Code's macOS build writes about itself
// as pid, having started at start, made a few seconds after it started.
func macWrittenBy(pid int, start time.Time) claudeRecord {
	return claudeRecord{pid: pid, procStart: lstart(start), startedAt: start.Add(3 * time.Second), pidDomain: macDomain}
}

// linuxWrittenBy is the record Claude Code's Linux build writes about itself
// as pid, having started at tick ticks.
func linuxWrittenBy(pid int, ticks int64) claudeRecord {
	return claudeRecord{pid: pid, procStart: strconv.FormatInt(ticks, 10), startedAt: time.Now(), pidDomain: linuxDomain}
}

// sleeper starts a fake that does nothing for a while and gives its pid; it
// is killed when the test ends.
func sleeper(t *testing.T) int {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	path := fakecli.Install(t, t.TempDir(), "sleeper", fakecli.Spec{Sleep: "60s", Ready: ready})
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return fakecli.WaitReady(t, ready)
}

// seam sets one of the package's seams for the rest of the test.
func seam[T any](t *testing.T, at *T, v T) {
	t.Helper()
	old := *at
	*at = v
	t.Cleanup(func() { *at = old })
}

// aliveIn reports whether a home holding only rec, as name, is alive.
func aliveIn(t *testing.T, rec claudeRecord, name string) bool {
	t.Helper()
	home := t.TempDir()
	rec.write(t, home, name)
	return len(claudeLive(home)) > 0
}

// On Linux a record's procStart is its writer's start in clock ticks since
// boot, which the kernel fixes for a process's life: the process at the pid
// is the writer exactly when it started at the same tick, and a leftover
// whose pid went to another process — after a crash, or any process after a
// reboot — started at another. Every record that leaves a doubt is alive:
// one from another pid space or naming none, one with a start in the shape macOS writes,
// none, or a number that is no tick, and one whose process's start cannot
// be read.
func TestOnLinuxARecordIsItsWritersByTheTickItStarted(t *testing.T) {
	pid := os.Getpid()
	onLinux(t, map[int]int64{pid: 48273615})
	same := linuxWrittenBy(pid, 48273615)
	for _, name := range []string{"sessions/41.json", claudeDaemonLock} {
		for _, c := range []struct {
			what  string
			rec   claudeRecord
			alive bool
		}{
			{"the writer itself", same, true},
			{"a writer that started a tick earlier", linuxWrittenBy(pid, 48273614), false},
			{"a writer from before a reboot", linuxWrittenBy(pid, 912), false},
			{"a writer that started later", linuxWrittenBy(pid, 48273715), false},
			// A record from a build older than pidDomain is judged by its
			// pid alone, as before rota read procStart: alive while the pid
			// is, gone once it is not.
			{"no pidDomain, another tick", same.with(func(r *claudeRecord) { r.pidDomain, r.procStart = "", "912" }), true},
			{"no pidDomain, a pid that is gone", same.with(func(r *claudeRecord) { r.pid, r.pidDomain = deadPID, "" }), false},
			{"another machine", same.with(func(r *claudeRecord) { r.pidDomain, r.procStart = "linux:another:pid:[4026531836]", "912" }), true},
			{"another pid namespace", same.with(func(r *claudeRecord) {
				r.pidDomain, r.procStart = "linux:5f0c2a1e7b3d4e8a9c612d4f8a0b3e17:pid:[4026532001]", "912"
			}), true},
			{"a macOS pid space", same.with(func(r *claudeRecord) { r.pidDomain, r.procStart = macDomain, "912" }), true},
			{"a start in the shape macOS writes", same.with(func(r *claudeRecord) { r.procStart = lstart(time.Now().Add(-time.Hour)) }), true},
			{"no procStart", same.with(func(r *claudeRecord) { r.procStart = "" }), true},
			{"a signed number", same.with(func(r *claudeRecord) { r.procStart = "+912" }), true},
			{"zero", same.with(func(r *claudeRecord) { r.procStart = "0" }), true},
			{"a process whose start cannot be read", same.with(func(r *claudeRecord) { r.pid, r.procStart = os.Getppid(), "912" }), true},
		} {
			if got := aliveIn(t, c.rec, name); got != c.alive {
				t.Fatalf("%s, %s: alive=%v", name, c.what, got)
			}
		}
	}
}

// On macOS a record's procStart is its writer's start as a UTC time, and a
// process at the pid that started more than two minutes after it is not
// the writer. It is believed only when the record's own startedAt bears it
// out, and only of a record that names this machine's pid space; every
// doubt is alive. The
// process here is this test's, whatever its name.
func TestOnMacARecordIsItsWritersByTheTimeItStarted(t *testing.T) {
	started, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("no start time here: %v", err)
	}
	onMac(t)
	me := os.Getpid()
	before := macWrittenBy(me, started.Add(-time.Hour))
	for _, name := range []string{"sessions/41.json", claudeDaemonLock} {
		for _, c := range []struct {
			what  string
			rec   claudeRecord
			alive bool
		}{
			{"its own start", macWrittenBy(me, started), true},
			{"its own start a second off", macWrittenBy(me, started.Add(-time.Second)), true},
			{"a start an hour before this process's", before, false},
			{"a start before the last reboot", macWrittenBy(me, started.Add(-72*time.Hour)), false},
			{"a start before, with no pidDomain", before.with(func(r *claudeRecord) { r.pidDomain = "" }), true},
			{"no pidDomain, a pid that is gone", before.with(func(r *claudeRecord) { r.pid, r.pidDomain = deadPID, "" }), false},
			// A writer that started after the process now at its pid is not
			// one a recycled pid makes; whatever it is, it is not proof.
			{"a start an hour after this process's", macWrittenBy(me, started.Add(time.Hour)), true},
			{"no procStart", macWrittenBy(me, started).with(func(r *claudeRecord) { r.procStart = "" }), true},
			{"a start in the shape Linux writes", before.with(func(r *claudeRecord) { r.procStart = "1793942047" }), true},
			{"a start before, with no startedAt", before.with(func(r *claudeRecord) { r.startedAt = time.Time{} }), true},
			{"a start before that its startedAt does not bear out", before.with(func(r *claudeRecord) { r.startedAt = started.Add(-5 * time.Hour) }), true},
			{"a start before, in a Linux pid space", before.with(func(r *claudeRecord) { r.pidDomain = linuxDomain }), true},
		} {
			if got := aliveIn(t, c.rec, name); got != c.alive {
				t.Fatalf("%s, %s: alive=%v", name, c.what, got)
			}
		}
	}
}

// A procStart, a startedAt or a pidDomain that is not what Claude Code
// writes there is missing, and the record is read as well as it ever was:
// a daemon, not an unreadable lock.
func TestAFieldOfTheWrongKindIsMissing(t *testing.T) {
	me := os.Getpid()
	for _, body := range []string{`"procStart":1793942047`, `"startedAt":"soon"`, `"pidDomain":7`} {
		home := t.TempDir()
		writeFile(t, filepath.Join(home, claudeDaemonLock), `{"pid":`+itoa(me)+`,`+body+`}`)
		if live := claudeLive(home); len(live) != 1 || live[0] != "daemon "+itoa(me) {
			t.Fatalf("%s: %v", body, live)
		}
	}
}

// A macOS record whose procStart was written in local time, not UTC, is
// still alive, west of UTC — where its procStart reads hours before its
// real start — and east of it: the record's startedAt, which has no zone,
// does not bear the procStart out, so it proves nothing.
func TestAProcStartInLocalTimeProvesNothing(t *testing.T) {
	started, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("no start time here: %v", err)
	}
	onMac(t)
	for _, zone := range []*time.Location{time.FixedZone("EST", -5*3600), time.FixedZone("UZT", 5*3600),
		time.FixedZone("HST", -10*3600), time.FixedZone("LINT", 14*3600)} {
		rec := macWrittenBy(os.Getpid(), started).with(func(r *claudeRecord) { r.procStart = started.In(zone).Format(time.ANSIC) })
		if !aliveIn(t, rec, "sessions/9.json") {
			t.Fatalf("a procStart in %s reads as a leftover", zone)
		}
	}
}

// On the system the tests run on, by nothing but what the system says: a
// record in the shape Claude Code writes here — ticks on Linux, a UTC time
// on macOS, with this machine's pid space — is alive for a live fakecli
// child that wrote its own start, and a leftover with an earlier writer's.
func TestAChildsRecordIsAliveOnlyWithItsOwnStart(t *testing.T) {
	child := sleeper(t)
	var own, earlier claudeRecord
	switch runtime.GOOS {
	case "linux":
		ticks, err := proc.StartTicks(child)
		if err != nil {
			t.Fatal(err)
		}
		own = claudeRecord{pid: child, procStart: strconv.FormatInt(ticks, 10), startedAt: time.Now(), pidDomain: thisPidDomain()}
		earlier = own.with(func(r *claudeRecord) { r.procStart = strconv.FormatInt(ticks-6000, 10) })
	case "darwin":
		started, err := proc.StartTime(child)
		if err != nil {
			t.Fatal(err)
		}
		own = macWrittenBy(child, started)
		earlier = macWrittenBy(child, started.Add(-10*time.Minute))
	default:
		t.Skipf("Claude Code's records are not checked on %s", runtime.GOOS)
	}
	home := t.TempDir()
	name := "sessions/" + itoa(child) + ".json"
	own.write(t, home, name)
	if live := claudeLive(home); len(live) != 1 || live[0] != "session "+itoa(child) {
		t.Fatalf("the child's own record: %v", live)
	}
	earlier.write(t, home, name)
	if live := claudeLive(home); len(live) != 0 {
		t.Fatalf("another's record at the child's pid: %v", live)
	}
}

// Records give their writer's start in ticks on Linux alone.
func TestStartsAreTicksOnLinuxAlone(t *testing.T) {
	if startsInTicks != (runtime.GOOS == "linux") {
		t.Fatalf("on %s starts are ticks: %v", runtime.GOOS, startsInTicks)
	}
}

// This machine's pid space is named as Claude Code's build for this system
// names it.
func TestThisMachinesPidSpaceIsNamedAsClaudeCodeNamesIt(t *testing.T) {
	switch got := thisPidDomain(); runtime.GOOS {
	case "darwin":
		if got != "darwin" {
			t.Fatalf("macOS: %q", got)
		}
	case "linux":
		ns, _ := os.Readlink("/proc/self/ns/pid")
		if !strings.HasPrefix(got, "linux:") || !strings.HasSuffix(got, ":"+ns) {
			t.Fatalf("linux: %q", got)
		}
	default:
		if got != "" {
			t.Fatalf("%s: %q", runtime.GOOS, got)
		}
	}
}
