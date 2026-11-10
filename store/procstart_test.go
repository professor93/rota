package store

import (
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

// lstart is a start as Claude Code writes it into a record's procStart:
// `ps -o lstart=` under LC_ALL=C TZ=UTC.
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

// writtenBy is the record a Claude Code that started at start writes about
// itself as pid, made a few seconds after it started.
func writtenBy(pid int, start time.Time) claudeRecord {
	return claudeRecord{pid: pid, procStart: lstart(start), startedAt: start.Add(3 * time.Second)}
}

// sleeper starts a fake that does nothing for a while and gives its pid; it
// is killed when the test ends.
func sleeper(t *testing.T) int { return sleeperCalled(t, "sleeper") }

// sleeperCalled is a sleeper installed as name, in a folder named after the
// test.
func sleeperCalled(t *testing.T, name string) int {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	path := fakecli.Install(t, t.TempDir(), name, fakecli.Spec{Sleep: "60s", Ready: ready})
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

// A record whose pid is alive but now names a process that started after
// the one that wrote it is a leftover: after a crash or a reboot the number
// went to something else. A record whose procStart is this process's own
// start is alive; so is every record that leaves a doubt: one with no
// procStart, one in a layout Claude Code does not write, one whose procStart
// its own startedAt does not bear out, one with no startedAt, and one from
// another pid space. The process here is this test's, whatever its name.
func TestARecordWhosePidWentToAnotherProcessIsALeftover(t *testing.T) {
	started, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("no start time here: %v", err)
	}
	seam(t, &looksLikeClaude, func(int) bool { return false })
	me := os.Getpid()
	before := writtenBy(me, started.Add(-time.Hour))
	noStartedAt, disagrees, foreign, ours := before, before, before, before
	noStartedAt.startedAt = time.Time{}
	disagrees.startedAt = started.Add(-5 * time.Hour)
	foreign.pidDomain = "darwin:another-machine:pid:[4026531836]"
	ours.pidDomain = ourPidDomain()
	noProcStart := writtenBy(me, started)
	noProcStart.procStart = ""
	badLayout := writtenBy(me, started)
	badLayout.procStart = "1793942047"
	for _, name := range []string{"sessions/41.json", claudeDaemonLock} {
		for _, c := range []struct {
			what  string
			rec   claudeRecord
			alive bool
		}{
			{"its own start", writtenBy(me, started), true},
			{"its own start a second off", writtenBy(me, started.Add(-time.Second)), true},
			{"no procStart", noProcStart, true},
			{"a procStart in no layout Claude Code writes", badLayout, true},
			{"a start an hour before this process's", before, false},
			{"a start before the last reboot", writtenBy(me, started.Add(-72*time.Hour)), false},
			{"a start before, in this machine's pid space", ours, false},
			// A writer that started after the process now at its pid is not
			// one a recycled pid makes; whatever it is, it is not proof.
			{"a start an hour after this process's", writtenBy(me, started.Add(time.Hour)), true},
			{"a start before, with no startedAt", noStartedAt, true},
			{"a start before that its startedAt does not bear out", disagrees, true},
			{"a start before, in another pid space", foreign, true},
		} {
			home := t.TempDir()
			c.rec.write(t, home, name)
			if got := len(claudeLive(home)) > 0; got != c.alive {
				t.Fatalf("%s, %s: alive=%v %v", name, c.what, got, claudeLive(home))
			}
		}
	}
	// A procStart or a startedAt that is not what Claude Code writes there
	// is missing, and the record is read as well as it ever was: a daemon,
	// not an unreadable lock.
	for _, body := range []string{`"procStart":1793942047`, `"startedAt":"soon"`, `"pidDomain":7`} {
		home := t.TempDir()
		writeFile(t, filepath.Join(home, claudeDaemonLock), `{"pid":`+itoa(me)+`,`+body+`}`)
		if live := claudeLive(home); len(live) != 1 || live[0] != "daemon "+itoa(me) {
			t.Fatalf("%s: %v", body, live)
		}
	}
}

// A Claude Code that wrote procStart in its own local time, not UTC, is
// still alive, west of UTC — where its procStart reads hours before its
// real start — and east of it: the record's startedAt, which has no zone,
// does not bear the procStart out, so it proves nothing.
func TestAProcStartInLocalTimeProvesNothing(t *testing.T) {
	started, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("no start time here: %v", err)
	}
	seam(t, &looksLikeClaude, func(int) bool { return false })
	for _, zone := range []*time.Location{time.FixedZone("EST", -5*3600), time.FixedZone("UZT", 5*3600),
		time.FixedZone("HST", -10*3600), time.FixedZone("LINT", 14*3600)} {
		rec := writtenBy(os.Getpid(), started)
		rec.procStart = started.In(zone).Format(time.ANSIC)
		home := t.TempDir()
		rec.write(t, home, "sessions/9.json")
		if len(claudeLive(home)) == 0 {
			t.Fatalf("a procStart in %s reads as a leftover", zone)
		}
	}
}

// A fakecli child is a live process of its own: its record is alive with
// its own start, and a leftover with another's — by what rota itself makes
// of the child, on macOS and on Linux alike, since the child is no claude.
func TestAChildsRecordIsAliveOnlyWithItsOwnStart(t *testing.T) {
	child := sleeper(t)
	started, err := proc.StartTime(child)
	if err != nil {
		t.Skipf("no start time here: %v", err)
	}
	home := t.TempDir()
	name := "sessions/" + itoa(child) + ".json"
	writtenBy(child, started).write(t, home, name)
	if live := claudeLive(home); len(live) != 1 || live[0] != "session "+itoa(child) {
		t.Fatalf("the child's own record: %v", live)
	}
	if strings.Contains(strings.ToLower(os.TempDir()), "claude") && startsMove {
		t.Skipf("the temporary folder %s names claude, so the child reads as one", os.TempDir())
	}
	writtenBy(child, started.Add(-10*time.Minute)).write(t, home, name)
	if live := claudeLive(home); len(live) != 0 {
		t.Fatalf("another's record at the child's pid: %v", live)
	}
}

// On Linux a start time is the boot time plus the ticks since boot, and the
// boot time moves when the clock is set: a laptop's WSL resuming from a
// night's sleep steps the clock by hours, and a live Claude Code then reads
// as having started after its own record. So there a later start is a
// leftover only when the process at the pid is no claude; on macOS, whose
// start times are written once at fork, it is a leftover whatever runs.
func TestAClaudeWhoseStartMovedWithTheClockIsStillAlive(t *testing.T) {
	recorded := time.Date(2026, 11, 9, 22, 0, 0, 0, time.UTC)
	seam(t, &startOf, func(int) (time.Time, error) { return recorded.Add(9 * time.Hour), nil })
	for _, c := range []struct {
		moves, claude, alive bool
	}{
		{moves: true, claude: true, alive: true},
		{moves: true, claude: false, alive: false},
		{moves: false, claude: true, alive: false},
		{moves: false, claude: false, alive: false},
	} {
		seam(t, &startsMove, c.moves)
		seam(t, &looksLikeClaude, func(int) bool { return c.claude })
		home := t.TempDir()
		writtenBy(os.Getpid(), recorded).write(t, home, "sessions/7.json")
		if got := len(claudeLive(home)) > 0; got != c.alive {
			t.Fatalf("clock moves %v, a claude %v: alive=%v", c.moves, c.claude, got)
		}
	}
}

// Start times move with the clock on Linux alone.
func TestStartTimesMoveOnLinuxAlone(t *testing.T) {
	if startsMove != (runtime.GOOS == "linux") {
		t.Fatalf("on %s start times move: %v", runtime.GOOS, startsMove)
	}
}

// What a Linux process is, for the guard above, is the names it goes by: one
// naming claude anywhere, in any case, is a claude, and one none of whose
// names rota can read could be. A process whose names name no claude is
// not.
func TestAClaudeIsKnownByTheNamesItGoesBy(t *testing.T) {
	for _, c := range []struct {
		names  []string
		err    error
		claude bool
	}{
		{names: []string{"claude", "--resume"}, claude: true},
		{names: []string{"/home/me/.local/share/claude/versions/2.1.322", "daemon"}, claude: true},
		{names: []string{"node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, claude: true},
		{names: []string{"/opt/ai/bin/cc", "cc", "/home/me/.local/share/claude/versions/2.1.322"}, claude: true},
		{names: []string{"/usr/bin/Claude"}, claude: true},
		{names: []string{"/usr/bin/python3", "-m", "http.server", "python3", "/usr/bin/python3.12"}, claude: false},
		{names: nil, claude: false},
		{err: os.ErrPermission, claude: true},
	} {
		if got := claudeIn(c.names, c.err); got != c.claude {
			t.Fatalf("%q (%v): claude=%v", c.names, c.err, got)
		}
	}
}

// A look at a process's names that does not end — a process stuck in the
// kernel holds its command line — is given up on, and the process could be
// a claude.
func TestALookAtANameThatDoesNotEndCountsAsAClaude(t *testing.T) {
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	seam(t, &namesOf, func(int) ([]string, error) { <-stuck; return []string{"sshd"}, nil })
	begun := time.Now()
	if !looksLikeClaude(os.Getpid()) {
		t.Fatal("a look that never ended is no claude")
	}
	if waited := time.Since(begun); waited > namesWait+5*time.Second {
		t.Fatalf("the look took %v", waited)
	}
	seam(t, &namesOf, func(int) ([]string, error) { return []string{"sshd"}, nil })
	if looksLikeClaude(os.Getpid()) {
		t.Fatal("sshd is a claude")
	}
}

// On Linux the guard reads a live process's own names: a fake installed as
// claude is a claude, and one installed as something else, in a folder
// whose path names no claude, is not.
func TestALiveProcessIsJudgedByItsOwnNames(t *testing.T) {
	if _, err := proc.Names(os.Getpid()); err != nil {
		t.Skipf("no process names here: %v", err)
	}
	if strings.Contains(strings.ToLower(os.TempDir()), "claude") {
		t.Skipf("the temporary folder %s names claude", os.TempDir())
	}
	if pid := sleeperCalled(t, "claude"); !looksLikeClaude(pid) {
		t.Fatal("a process run as claude is no claude")
	}
	pid := sleeper(t)
	if looksLikeClaude(pid) {
		names, err := proc.Names(pid)
		t.Fatalf("the sleeper reads as a claude: %q, %v", names, err)
	}
}

// This machine's pid space is named as Claude Code names it.
func TestThisMachinesPidSpaceIsNamedAsClaudeCodeNamesIt(t *testing.T) {
	switch got := thisPidDomain(); runtime.GOOS {
	case "darwin":
		if got != "darwin" {
			t.Fatalf("macOS: %q", got)
		}
	case "linux":
		if !strings.HasPrefix(got, "darwin:") || strings.Count(got, ":") < 2 {
			t.Fatalf("linux: %q", got)
		}
	default:
		if got != "" {
			t.Fatalf("%s: %q", runtime.GOOS, got)
		}
	}
}
