package hold

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const fixture = "testdata/registry-2.1.318.json"

// fixtureWith is the committed file with one key changed, or removed when v
// is the marker absent.
func fixtureWith(t *testing.T, key string, v any) []byte {
	t.Helper()
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if v == absent {
		delete(m, key)
	} else {
		m[key] = v
	}
	if b, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	return b
}

var absent = new(struct{ absent bool })

// zones are local zones the registry's times are also tested in, neither of
// them UTC, so that a time read or written as local time cannot pass for one
// in UTC, as it does on a machine kept in UTC.
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

func TestTheRegistryShapeIsRead(t *testing.T) {
	if !inZones(t) {
		return
	}
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture is a whole 2.1.318 file: the keys that version writes, no
	// more and no fewer, with made-up values.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	want := []string{"cwd", "kind", "name", "pid", "procStart", "sessionId", "startedAt", "status", "updatedAt", "version", "waitingFor"}
	if got := slices.Sorted(maps.Keys(keys)); !slices.Equal(got, want) {
		t.Fatalf("fixture keys %q, want %q", got, want)
	}

	full, err := parseRegistry(b)
	if err != nil {
		t.Fatal(err)
	}
	wantReg := Registry{
		SessionID: "5f0c2a1e-7b3d-4e8a-9c61-2d4f8a0b3e17",
		Name:      "made-up-session",
		Cwd:       "/home/someone/work/project",
		Status:    "idle",
		Kind:      "interactive",
		UpdatedAt: 1793956500981,
	}
	if full.Registry != wantReg {
		t.Fatalf("read %+v, want %+v", full.Registry, wantReg)
	}
	if full.PID != 48213 {
		t.Fatalf("pid %d", full.PID)
	}
	if at := time.Date(2026, time.November, 6, 9, 14, 7, 0, time.UTC); !full.ProcStart.Equal(at) {
		t.Fatalf("procStart %v, want %v", full.ProcStart, at)
	}

	// What it waits for: a string as it is, an object by its type or kind,
	// and something it cannot name still as waiting, so that Enter is never
	// typed at a prompt rota failed to recognise.
	for _, c := range []struct {
		v    any
		want string
	}{
		{"permission", "permission"},
		{map[string]any{"type": "question"}, "question"},
		{map[string]any{"kind": "permission", "tool": "Bash"}, "permission"},
		{map[string]any{"type": "tool", "kind": "other"}, "tool"},
		{map[string]any{"type": 3, "kind": "question"}, "question"},
		{map[string]any{}, "unknown"},
		{map[string]any{"type": ""}, "unknown"},
		{true, "unknown"},
		{7, "unknown"},
		{[]any{"permission"}, "unknown"},
		{nil, ""},
		{"", ""},
		{false, ""},
		{absent, ""},
	} {
		got, err := parseRegistry(fixtureWith(t, "waitingFor", c.v))
		if err != nil || got.WaitingFor != c.want {
			t.Fatalf("waitingFor %#v: %q, %v; want %q", c.v, got.WaitingFor, err, c.want)
		}
	}

	// A key that is missing, or is not what that version wrote, leaves its
	// own field empty and every other as it was.
	for key, wipe := range map[string]func(*registryFile){
		"sessionId": func(f *registryFile) { f.SessionID = "" },
		"name":      func(f *registryFile) { f.Name = "" },
		"cwd":       func(f *registryFile) { f.Cwd = "" },
		"status":    func(f *registryFile) { f.Status = "" },
		"kind":      func(f *registryFile) { f.Kind = "" },
		"updatedAt": func(f *registryFile) { f.UpdatedAt = 0 },
		"pid":       func(f *registryFile) { f.PID = 0 },
		"procStart": func(f *registryFile) { f.ProcStart, f.ProcTicks = time.Time{}, 0 },
		"startedAt": func(*registryFile) {},
		"version":   func(*registryFile) {},
	} {
		want := full
		wipe(&want)
		for _, v := range []any{absent, map[string]any{"x": 1}, []any{1}} {
			got, err := parseRegistry(fixtureWith(t, key, v))
			if err != nil || !sameFile(got, want) {
				t.Fatalf("%s as %#v: %+v, %v; want %+v", key, v, got, err, want)
			}
		}
	}
	// procStart is read in the form Claude Code's macOS build writes, a
	// time, and in the one its Linux build writes, a tick as digits, and in
	// no other.
	if full.ProcTicks != 0 {
		t.Fatalf("a start written as a time read as tick %d", full.ProcTicks)
	}
	for _, c := range []struct {
		procStart string
		ticks     int64
	}{
		{"48273615", 48273615},
		{"2026-11-06T09:14:07Z", 0},
		{"0", 0},
		{"+48273615", 0},
		{"-48273615", 0},
		{" 48273615", 0},
		{"48273615 ", 0},
		{"4827361x", 0},
		{"99999999999999999999", 0},
		{"", 0},
	} {
		got, err := parseRegistry(fixtureWith(t, "procStart", c.procStart))
		if err != nil || !got.ProcStart.IsZero() || got.ProcTicks != c.ticks {
			t.Fatalf("procStart %q read as %v and tick %d, %v; want tick %d", c.procStart, got.ProcStart, got.ProcTicks, err, c.ticks)
		}
	}
	if got, err := parseRegistry(fixtureWith(t, "procStart", 48273615)); err != nil || got.ProcTicks != 0 {
		t.Fatalf("a procStart that is a number, not the digits Claude Code writes: tick %d, %v", got.ProcTicks, err)
	}

	// pidDomain, which this file does not name, is read as written, and is
	// empty for anything but a string.
	for _, c := range []struct {
		v    any
		want string
	}{
		{"darwin", "darwin"},
		{"linux:5f0c:pid:[4026531836]", "linux:5f0c:pid:[4026531836]"},
		{map[string]any{"x": 1}, ""},
		{7, ""},
		{absent, ""},
	} {
		want := full
		want.PidDomain = c.want
		if got, err := parseRegistry(fixtureWith(t, "pidDomain", c.v)); err != nil || !sameFile(got, want) {
			t.Fatalf("pidDomain %#v: %+v, %v; want %q", c.v, got, err, c.want)
		}
	}
	if got, err := parseRegistry(fixtureWith(t, "updatedAt", 1793956500981.75)); err != nil || got.UpdatedAt != 1793956500981 {
		t.Fatalf("a fractional updatedAt: %d, %v", got.UpdatedAt, err)
	}

	// A file that is not one JSON object — one caught half written, say —
	// is an error, so the caller keeps what it last knew.
	for _, bad := range []string{"", `{"pid":48213,"sessi`, "null", "[]", `"x"`} {
		if _, err := parseRegistry([]byte(bad)); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}

	if got, want := registryPath("/h", 48213), filepath.Join("/h", "sessions", "48213.json"); got != want {
		t.Fatalf("path %q, want %q", got, want)
	}
}

func sameFile(a, b registryFile) bool {
	return a.Registry == b.Registry && a.PID == b.PID && a.ProcStart.Equal(b.ProcStart) &&
		a.ProcTicks == b.ProcTicks && a.PidDomain == b.PidDomain
}

// The two pid spaces the tests play: a macOS one, and a Linux one as
// Claude Code names it there.
const (
	macDomain   = "darwin"
	linuxDomain = "linux:5f0c2a1e7b3d4e8a9c612d4f8a0b3e17:pid:[4026531836]"
)

// seam sets one of the package's seams for the rest of the test.
func seam[T any](t *testing.T, at *T, v T) {
	t.Helper()
	old := *at
	*at = v
	t.Cleanup(func() { *at = old })
}

// onMac makes the holder read registry files as on macOS, and onLinux as
// on Linux, whatever system the test runs on.
func onMac(t *testing.T) {
	t.Helper()
	seam(t, &startsInTicks, false)
	seam(t, &ourPidDomain, func() string { return macDomain })
}

func onLinux(t *testing.T) {
	t.Helper()
	seam(t, &startsInTicks, true)
	seam(t, &ourPidDomain, func() string { return linuxDomain })
}

// written is a registry file as parsed: the child's pid, and procStart and
// pidDomain as given, each left out when it is absent.
func written(t *testing.T, pid int, procStart, pidDomain any) registryFile {
	t.Helper()
	m := map[string]any{"pid": pid}
	if procStart != absent {
		m["procStart"] = procStart
	}
	if pidDomain != absent {
		m["pidDomain"] = pidDomain
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseRegistry(b)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The holder reads files the way the system it runs on writes them:
// ticks on Linux alone.
func TestRegistryStartsAreTicksOnLinuxAlone(t *testing.T) {
	if startsInTicks != (runtime.GOOS == "linux") {
		t.Fatalf("on %s starts are ticks: %v", runtime.GOOS, startsInTicks)
	}
}

// On Linux, Claude Code writes the start of the process that wrote the file
// as field 22 of /proc/<pid>/stat, a tick since boot that no setting of the
// clock moves: the file is the child's when it gives the very tick the child
// started at, names no other pid and no other pid space. A start in the form
// macOS writes, the child's own included, is never the child's, nor is the
// child's tick in another pid space.
func TestOnLinuxARegistryIsTheChildsByTheTickItStarted(t *testing.T) {
	onLinux(t)
	const pid, ticks = 48213, 48273615
	start := time.Date(2026, time.November, 6, 9, 14, 7, 0, time.UTC)
	for _, c := range []struct {
		name              string
		procStart, domain any
		ok                bool
	}{
		{"its own tick, here", "48273615", linuxDomain, true},
		{"its own tick, from a build that names no pid space", "48273615", absent, true},
		{"a tick before", "48273614", linuxDomain, false},
		{"a tick after", "48273616", linuxDomain, false},
		{"a minute before", "48267615", linuxDomain, false},
		{"its own tick on another machine", "48273615", "linux:0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a:pid:[4026531836]", false},
		{"its own tick in another pid namespace", "48273615", "linux:5f0c2a1e7b3d4e8a9c612d4f8a0b3e17:pid:[4026532001]", false},
		{"its own tick, from a Mac", "48273615", macDomain, false},
		{"its own tick, with an empty pid space", "48273615", "", true},
		{"its own start as macOS writes it", start.Format(time.ANSIC), linuxDomain, false},
		{"its own start as macOS writes it, no pid space", start.Format(time.ANSIC), absent, false},
		{"tick zero", "0", linuxDomain, false},
		{"its own tick, signed", "+48273615", linuxDomain, false},
		{"its own tick as a number", ticks, linuxDomain, false},
		{"no start", absent, linuxDomain, false},
	} {
		if got := written(t, pid, c.procStart, c.domain).belongsTo(pid, start, ticks); got != c.ok {
			t.Errorf("%s: %v, want %v", c.name, got, c.ok)
		}
	}
	own := written(t, pid, "48273615", linuxDomain)
	if own.belongsTo(pid+1, start, ticks) {
		t.Error("a file naming another pid counted")
	}
	if own.belongsTo(pid, start, 0) {
		t.Error("a child whose start in ticks is unknown matched")
	}
	if !written(t, 0, "48273615", linuxDomain).belongsTo(pid, start, ticks) {
		t.Error("a file naming no pid is judged by its start alone")
	}
}

// On macOS, Claude Code writes the start as `ps -o lstart=` prints it under
// LC_ALL=C TZ=UTC: the file is the child's when it gives a time within a
// second of the child's start and names no other pid space. A start in the
// form Linux writes, the child's own tick included, is never the child's,
// nor is a Linux pid space.
func TestOnMacARegistryIsTheChildsByTheTimeItStarted(t *testing.T) {
	onMac(t)
	const pid, ticks = 48213, 48273615
	start := time.Date(2026, time.November, 6, 9, 14, 7, 0, time.UTC)
	for _, c := range []struct {
		name              string
		procStart, domain any
		ok                bool
	}{
		{"its own start, here", start.Format(time.ANSIC), macDomain, true},
		{"its own start, from a build that names no pid space", start.Format(time.ANSIC), absent, true},
		{"its own start, with a Linux pid space", start.Format(time.ANSIC), linuxDomain, false},
		{"its own start, with a pid space nobody writes", start.Format(time.ANSIC), "windows", false},
		{"its own tick, as Linux writes it", "48273615", macDomain, false},
		{"its own tick, as Linux writes it, no pid space", "48273615", absent, false},
		{"an hour before", start.Add(-time.Hour).Format(time.ANSIC), macDomain, false},
		{"no start", absent, macDomain, false},
	} {
		if got := written(t, pid, c.procStart, c.domain).belongsTo(pid, start, ticks); got != c.ok {
			t.Errorf("%s: %v, want %v", c.name, got, c.ok)
		}
	}
}

// On macOS a registry file is the child's only when it says it was written
// by a process that started within a second of the child, and names no
// other pid.
func TestARegistryCountsOnlyForItsOwnChild(t *testing.T) {
	if !inZones(t) {
		return
	}
	onMac(t)
	const pid = 48213
	at := time.Date(2026, time.November, 6, 9, 14, 7, 0, time.UTC)
	f := registryFile{PID: pid, ProcStart: at}
	for _, c := range []struct {
		start time.Duration
		ok    bool
	}{
		{0, true},
		{400 * time.Millisecond, true},
		{999 * time.Millisecond, true},
		{time.Second, true},
		{-time.Second, true},
		{1001 * time.Millisecond, false},
		{-1500 * time.Millisecond, false},
		{time.Hour, false},
		{-time.Hour, false},
	} {
		if got := f.belongsTo(pid, at.Add(c.start), 0); got != c.ok {
			t.Fatalf("child started %v off: %v, want %v", c.start, got, c.ok)
		}
	}
	if f.belongsTo(pid+1, at, 0) {
		t.Fatal("a file naming another pid counted")
	}
	if !(registryFile{ProcStart: at}).belongsTo(pid, at, 0) {
		t.Fatal("a file naming no pid is judged by its start alone")
	}
	if (registryFile{PID: pid}).belongsTo(pid, at, 0) {
		t.Fatal("a file with no start counted")
	}
	if f.belongsTo(pid, time.Time{}, 0) {
		t.Fatal("a child with no known start matched")
	}

	// The way Claude Code writes it, as ps -o lstart= prints it under
	// LC_ALL=C TZ=UTC: the writer's own start in UTC, to the second, read
	// back against a start known to the nanosecond in this zone.
	start := time.Now()
	b, err := json.Marshal(map[string]any{"pid": pid, "procStart": start.UTC().Format(time.ANSIC)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseRegistry(b)
	if err != nil || !got.belongsTo(pid, start, 0) {
		t.Fatalf("its own start did not count: %+v, %v", got, err)
	}
	if got.belongsTo(pid, start.Add(-time.Hour), 0) {
		t.Fatal("an hour off counted")
	}
	// The same clock reading taken as local time is another moment here.
	if b, err = json.Marshal(map[string]any{"pid": pid, "procStart": start.Local().Format(time.ANSIC)}); err != nil {
		t.Fatal(err)
	}
	if got, err = parseRegistry(b); err != nil || got.belongsTo(pid, start, 0) {
		t.Fatalf("a start written in local time counted: %+v, %v", got, err)
	}
}

// In the hour a clock goes back the same local time happens twice, but the
// file is written in UTC, where nothing repeats: a child started at 01:30 in
// either of them matches its own file and not the other's.
func TestARegistryMatchesInTheRepeatedHour(t *testing.T) {
	if !inZones(t) {
		return
	}
	onMac(t)
	const pid = 48213
	edt := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC) // 01:30 EDT
	est := edt.Add(time.Hour)                                       // 01:30 EST
	for name, c := range map[string]struct{ file, other time.Time }{"EDT": {edt, est}, "EST": {est, edt}} {
		b, err := json.Marshal(map[string]any{"pid": pid, "procStart": c.file.Format(time.ANSIC)})
		if err != nil {
			t.Fatal(err)
		}
		f, err := parseRegistry(b)
		if err != nil {
			t.Fatal(err)
		}
		if !f.belongsTo(pid, c.file.Add(400*time.Millisecond).Local(), 0) {
			t.Fatalf("a child started at 01:30 %s did not match its own file", name)
		}
		if f.belongsTo(pid, c.other.Local(), 0) || f.belongsTo(pid, c.file.Add(2*time.Second), 0) || f.belongsTo(pid, c.file.Add(-2*time.Second), 0) {
			t.Fatalf("the file of a child started at 01:30 %s matched another start", name)
		}
	}
}

// A registry string longer than any Claude Code writes is not believed, so
// the State every link begins with always fits in a frame (see
// TestTheStateFitsWhateverTheRegistrySays). A waitingFor too long to
// believe still says the session waits.
func TestARegistryStringPastItsBoundIsNotBelieved(t *testing.T) {
	huge := strings.Repeat("\x01", 200<<10)
	for _, c := range []struct {
		key string
		v   any
	}{
		{"name", huge}, {"cwd", huge}, {"sessionId", huge}, {"status", huge}, {"kind", huge},
		{"waitingFor", huge}, {"waitingFor", map[string]any{"type": huge}},
	} {
		f, err := parseRegistry(fixtureWith(t, c.key, c.v))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := parseRegistry(fixtureWith(t, c.key, absent))
		if c.key == "waitingFor" {
			want.WaitingFor = waitingUnknown
		}
		if !sameFile(f, want) {
			g := f.Registry
			t.Fatalf("%s of %d bytes read as fields of %d, %d, %d, %d, %d and %d bytes",
				c.key, len(huge), len(g.SessionID), len(g.Name), len(g.Cwd), len(g.Status), len(g.WaitingFor), len(g.Kind))
		}
	}
}
