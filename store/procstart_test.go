package store

import (
	"os"
	"os/exec"
	"path/filepath"
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

// recordWith writes a record naming pid, with procStart when it is given.
func recordWith(t *testing.T, home, name string, pid int, procStart string) {
	t.Helper()
	body := `{"pid":` + itoa(pid) + `,"kind":"interactive","status":"busy"`
	if procStart != "" {
		body += `,"procStart":` + strconv.Quote(procStart)
	}
	writeFile(t, filepath.Join(home, name), body+"}")
}

// sleeper starts a fake that does nothing for a while and gives its pid; it
// is killed when the test ends.
func sleeper(t *testing.T) int { return sleeperCalled(t, "sleeper") }

// sleeperCalled is a sleeper installed as name.
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
// start is alive; so is one with no procStart, or one rota cannot read the
// start of, as before. The process here is this test's, whatever its name.
func TestARecordWhosePidWentToAnotherProcessIsALeftover(t *testing.T) {
	started, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("no start time here: %v", err)
	}
	seam(t, &looksLikeClaude, func(int) bool { return false })
	for _, name := range []string{"sessions/41.json", claudeDaemonLock} {
		for _, c := range []struct {
			what, procStart string
			alive           bool
		}{
			{"its own start", lstart(started), true},
			{"its own start a second off", lstart(started.Add(-time.Second)), true},
			{"no procStart", "", true},
			{"a procStart in no layout Claude Code writes", "1793942047", true},
			{"a start an hour before this process's", lstart(started.Add(-time.Hour)), false},
			{"a start before the last reboot", lstart(started.Add(-72 * time.Hour)), false},
			// A writer that started after the process now at its pid is not
			// one a recycled pid makes; whatever it is, it is not proof.
			{"a start an hour after this process's", lstart(started.Add(time.Hour)), true},
		} {
			home := t.TempDir()
			recordWith(t, home, name, os.Getpid(), c.procStart)
			if got := len(claudeLive(home)) > 0; got != c.alive {
				t.Fatalf("%s, %s: alive=%v %v", name, c.what, got, claudeLive(home))
			}
		}
	}
	// A procStart that is not a string is no start, and the record is read
	// as well as it ever was: a daemon, not an unreadable lock.
	home := t.TempDir()
	writeFile(t, filepath.Join(home, claudeDaemonLock), `{"pid":`+itoa(os.Getpid())+`,"procStart":1793942047}`)
	if live := claudeLive(home); len(live) != 1 || live[0] != "daemon "+itoa(os.Getpid()) {
		t.Fatalf("a procStart that is a number: %v", live)
	}
}

// A fakecli child is a live process of its own: its record is alive with
// its own start, and a leftover with another's.
func TestAChildsRecordIsAliveOnlyWithItsOwnStart(t *testing.T) {
	child := sleeper(t)
	started, err := proc.StartTime(child)
	if err != nil {
		t.Skipf("no start time here: %v", err)
	}
	seam(t, &looksLikeClaude, func(int) bool { return false })
	home := t.TempDir()
	recordWith(t, home, "sessions/"+itoa(child)+".json", child, lstart(started))
	if live := claudeLive(home); len(live) != 1 || live[0] != "session "+itoa(child) {
		t.Fatalf("the child's own record: %v", live)
	}
	recordWith(t, home, "sessions/"+itoa(child)+".json", child, lstart(started.Add(-10*time.Minute)))
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
		recordWith(t, home, "sessions/7.json", os.Getpid(), lstart(recorded))
		if got := len(claudeLive(home)) > 0; got != c.alive {
			t.Fatalf("clock moves %v, a claude %v: alive=%v", c.moves, c.claude, got)
		}
	}
}

// What a Linux process is, for the guard above, is its command line: one
// naming claude anywhere, in any case, is a claude, and one rota cannot
// read could be. A process with none, a kernel thread's or a zombie's, is
// not.
func TestAClaudeIsKnownByItsCommandLine(t *testing.T) {
	for _, c := range []struct {
		args   []string
		err    error
		claude bool
	}{
		{args: []string{"claude", "--resume"}, claude: true},
		{args: []string{"/home/me/.local/share/claude/versions/2.1.322", "daemon"}, claude: true},
		{args: []string{"node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, claude: true},
		{args: []string{"/usr/bin/Claude"}, claude: true},
		{args: []string{"/usr/bin/python3", "-m", "http.server"}, claude: false},
		{args: nil, claude: false},
		{err: os.ErrPermission, claude: true},
	} {
		got := claudeIn(c.args, c.err)
		if got != c.claude {
			t.Fatalf("%q (%v): claude=%v", c.args, c.err, got)
		}
	}
}

// On Linux the guard reads a live process's own command line: a fake
// installed as claude is a claude, and one installed as something else,
// in a folder whose path names no claude, is not.
func TestALiveProcessIsAClaudeByItsOwnCommandLine(t *testing.T) {
	if _, err := proc.Cmdline(os.Getpid()); err != nil {
		t.Skipf("no command lines here: %v", err)
	}
	if pid := sleeperCalled(t, "claude"); !looksLikeClaude(pid) {
		t.Fatal("a process run as claude is no claude")
	}
	pid := sleeper(t)
	args, err := proc.Cmdline(pid)
	if err != nil || len(args) == 0 {
		t.Fatalf("the sleeper's command line: %q, %v", args, err)
	}
	if strings.Contains(strings.ToLower(strings.Join(args, " ")), "claude") {
		t.Skipf("the temporary folder names claude: %q", args)
	}
	if looksLikeClaude(pid) {
		t.Fatalf("the sleeper %q reads as a claude", args)
	}
}
