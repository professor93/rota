package fakecli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/proc"
)

func TestMain(m *testing.M) {
	Maybe()
	os.Exit(m.Run())
}

// A fake asked to warm up exits at once and does nothing it was installed
// to do: Link runs it that way, and nothing may come of that run.
func TestAFakeAskedToWarmUpDoesNothing(t *testing.T) {
	dir := t.TempDir()
	touched := filepath.Join(dir, "touched")
	path := Install(t, dir, "claude", Spec{Stdout: []string{"played"}, Touch: touched, Exit: 3})
	out, err := exec.Command(path, warmArg).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("a warm-up exits 0 and says nothing: %v %q", err, out)
	}
	if _, err := os.Stat(touched); err == nil {
		t.Fatal("a warm-up played the fake")
	}
	// Run for real, it plays its part.
	out, _ = exec.Command(path).Output()
	if string(out) != "played\n" {
		t.Fatalf("the fake said %q", out)
	}
}

// WaitReady returns once the fake is up, with the pid of the fake itself.
func TestWaitReadyGivesTheFakesPid(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	path := Install(t, dir, "claude", Spec{Ready: ready, Sleep: "30s", KeepStdin: true})
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if pid := WaitReady(t, ready); pid != cmd.Process.Pid {
		t.Fatalf("WaitReady says %d, the fake is %d", pid, cmd.Process.Pid)
	}
}

// TestTheTtyFakePrintsAnyBytesAndShowsControlBytesItReads drives the tty
// fake the way a capture does: a print line writes exactly its bytes, with no
// newline, and a line with control bytes in it comes back with each one
// spelled out, so a test can see what arrived without the terminal acting on
// it.
func TestTheTtyFakePrintsAnyBytesAndShowsControlBytesItReads(t *testing.T) {
	in := strings.Join([]string{
		"print 1b5b3f3130343968",
		"print e4b896",
		"print zz",
		"a\x1bb\x7fc\x01",
		"plain words",
		"bye",
	}, "\n") + "\n"
	var out bytes.Buffer
	if code := atty(Spec{Tty: true}, strings.NewReader(in), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	_, got, ok := strings.Cut(out.String(), "\r\n")
	if !ok {
		t.Fatalf("no ready line: %q", out.String())
	}
	want := "\x1b[?1049h" + "世" +
		"bad print: zz\r\n" +
		`got:a\x1bb\x7fc\x01` + "\r\n" +
		"got:plain words\r\n" +
		"bye\r\n"
	if got != want {
		t.Fatalf("the fake wrote\n%q\nwant\n%q", got, want)
	}
}

// TestTheTtyFakeSaysWhatItsEnvironmentHolds: env names one variable, envhas
// looks for some text anywhere in the environment, and registry writes the
// file Claude Code keeps for a running session where Claude Code keeps it.
func TestTheTtyFakeSaysWhatItsEnvironmentHolds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("FAKECLI_SET", "a value")
	in := strings.Join([]string{
		"env FAKECLI_SET",
		"env FAKECLI_NEVER_SET",
		"envhas a valu",
		"envhas FAKECLI_SE",
		"envhas nothing-has-this-0f3b",
		"registry idle",
		"registry waiting permission",
		"bye",
	}, "\n") + "\n"
	var out bytes.Buffer
	if code := atty(Spec{Tty: true}, strings.NewReader(in), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	_, got, _ := strings.Cut(out.String(), "\r\n")
	want := "env FAKECLI_SET=a value\r\n" +
		"env FAKECLI_NEVER_SET unset\r\n" +
		"envhas yes\r\n" + "envhas yes\r\n" + "envhas no\r\n" +
		"registry idle\r\n" + "registry waiting\r\n" +
		"bye\r\n"
	if got != want {
		t.Fatalf("the fake wrote\n%q\nwant\n%q", got, want)
	}

	raw, err := os.ReadFile(filepath.Join(home, "sessions", strconv.Itoa(os.Getpid())+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var reg map[string]any
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(reg))
	for k := range reg {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	wantKeys := []string{"cwd", "kind", "name", "pid", "procStart", "sessionId", "startedAt", "status", "updatedAt", "version", "waitingFor"}
	if proc.PidDomain() != "" {
		wantKeys = append(wantKeys, "pidDomain")
		slices.Sort(wantKeys)
	}
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("a registry file has the keys %v, want %v", keys, wantKeys)
	}
	if reg["sessionId"] != "fake-"+strconv.Itoa(os.Getpid()) || reg["status"] != "waiting" ||
		reg["waitingFor"] != "permission" || reg["kind"] != "interactive" || reg["version"] != "2.1.318" ||
		reg["name"] != "fake" || reg["pid"] != float64(os.Getpid()) {
		t.Fatalf("the last line wrote %s", raw)
	}
	if start, _ := reg["procStart"].(string); start == "" {
		t.Fatalf("no procStart: %s", raw)
	}
}

// The fake writes procStart and pidDomain as Claude Code's build for this
// system does, whatever zone it runs in: on Linux the tick since boot it
// started at, field 22 of /proc/<pid>/stat, and "linux:<machine-id>:<pid
// namespace>"; on macOS the output of `ps -o lstart= -p <pid>` under
// LC_ALL=C TZ=UTC, its own start in UTC laid out as ANSIC, and "darwin".
// startedAt is when it first made the file, and stays so as it is
// rewritten.
func TestTheFakeWritesItsStartAsClaudeCodeDoesHere(t *testing.T) {
	if !inZones(t) {
		return
	}
	started, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("no start time here: %v", err)
	}
	var want, domain string
	switch runtime.GOOS {
	case "linux":
		ticks, err := proc.StartTicks(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		want, domain = strconv.FormatInt(ticks, 10), proc.PidDomain()
		if !strings.HasPrefix(domain, "linux:") {
			t.Fatalf("this machine's pid space is %q", domain)
		}
	case "darwin":
		want, domain = started.UTC().Format(time.ANSIC), "darwin"
	default:
		t.Skipf("Claude Code's start is not written on %s", runtime.GOOS)
	}
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	type file struct {
		ProcStart, PidDomain string
		StartedAt            int64
	}
	write := func(line string) file {
		t.Helper()
		var out bytes.Buffer
		atty(Spec{Tty: true}, strings.NewReader(line+"\nbye\n"), &out)
		raw, err := os.ReadFile(filepath.Join(home, "sessions", strconv.Itoa(os.Getpid())+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var f file
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	first := write("registry idle")
	if first.ProcStart != want || first.PidDomain != domain {
		t.Fatalf("procStart %q and pidDomain %q, want %q and %q, as Claude Code writes them here", first.ProcStart, first.PidDomain, want, domain)
	}
	if made := time.UnixMilli(first.StartedAt); made.Before(started.Add(-2*time.Second)) || made.After(time.Now()) {
		t.Fatalf("startedAt %v is not a moment since this process started at %v", made, started)
	}
	time.Sleep(5 * time.Millisecond)
	if again := write("registry waiting permission"); again != first {
		t.Fatalf("rewritten, the file says %+v; it said %+v", again, first)
	}
}

// zones are local zones the fake is also tested in, neither of them UTC, so
// that a time written as local time cannot pass for one in UTC.
var zones = []string{"Asia/Tashkent", "America/New_York"}

// zoneRun names the zone a test binary started by inZones runs in.
const zoneRun = "ROTA_TEST_ZONE"

// inZones runs the calling test again in a test binary of its own under each
// of zones, as TZ names them, and says whether this run is one of those, the
// only kind that goes on to test. A zone the machine has no data for is
// skipped.
func inZones(t *testing.T) bool {
	t.Helper()
	if z := os.Getenv(zoneRun); z != "" {
		if _, off := time.Date(2026, time.November, 10, 12, 0, 0, 0, time.UTC).In(time.Local).Zone(); off == 0 {
			t.Fatalf("TZ=%s gave a local zone at UTC's offset", z)
		}
		return true
	}
	name := t.Name()
	for _, z := range zones {
		t.Run(z, func(t *testing.T) {
			if _, err := time.LoadLocation(z); err != nil {
				t.Skipf("no zone data for %s: %v", z, err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), "TZ="+z, zoneRun+"="+z)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("under TZ=%s: %v\n%s", z, err, out)
			}
			if !strings.Contains(string(out), "--- PASS: "+name) {
				t.Fatalf("under TZ=%s the test did not run:\n%s", z, out)
			}
		})
	}
	return false
}

// TestTheTtyFakeWritesANullWaitingForWhenNotWaiting: a registry line with no
// second word is a session waiting for nothing, which Claude Code writes as
// null.
func TestTheTtyFakeWritesANullWaitingForWhenNotWaiting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	var out bytes.Buffer
	atty(Spec{Tty: true}, strings.NewReader("registry idle\nbye\n"), &out)
	raw, err := os.ReadFile(filepath.Join(home, "sessions", strconv.Itoa(os.Getpid())+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"waitingFor":null`) {
		t.Fatalf("waitingFor is null when nothing is waited for: %s", raw)
	}
}
