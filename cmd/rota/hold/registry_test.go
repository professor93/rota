package hold

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
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
		"procStart": func(f *registryFile) { f.ProcStart = time.Time{} },
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
	if got, err := parseRegistry(fixtureWith(t, "procStart", "2026-11-06T09:14:07Z")); err != nil || !got.ProcStart.IsZero() {
		t.Fatalf("a procStart in another layout is unknown: %v, %v", got.ProcStart, err)
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
	return a.Registry == b.Registry && a.PID == b.PID && a.ProcStart.Equal(b.ProcStart)
}

// A registry file is the child's only when it says it was written by a
// process that started within a second of the child, and names no other pid.
func TestARegistryCountsOnlyForItsOwnChild(t *testing.T) {
	if !inZones(t) {
		return
	}
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
		if got := f.belongsTo(pid, at.Add(c.start)); got != c.ok {
			t.Fatalf("child started %v off: %v, want %v", c.start, got, c.ok)
		}
	}
	if f.belongsTo(pid+1, at) {
		t.Fatal("a file naming another pid counted")
	}
	if !(registryFile{ProcStart: at}).belongsTo(pid, at) {
		t.Fatal("a file naming no pid is judged by its start alone")
	}
	if (registryFile{PID: pid}).belongsTo(pid, at) {
		t.Fatal("a file with no start counted")
	}
	if f.belongsTo(pid, time.Time{}) {
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
	if err != nil || !got.belongsTo(pid, start) {
		t.Fatalf("its own start did not count: %+v, %v", got, err)
	}
	if got.belongsTo(pid, start.Add(-time.Hour)) {
		t.Fatal("an hour off counted")
	}
	// The same clock reading taken as local time is another moment here.
	if b, err = json.Marshal(map[string]any{"pid": pid, "procStart": start.Local().Format(time.ANSIC)}); err != nil {
		t.Fatal(err)
	}
	if got, err = parseRegistry(b); err != nil || got.belongsTo(pid, start) {
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
		if !f.belongsTo(pid, c.file.Add(400*time.Millisecond).Local()) {
			t.Fatalf("a child started at 01:30 %s did not match its own file", name)
		}
		if f.belongsTo(pid, c.other.Local()) || f.belongsTo(pid, c.file.Add(2*time.Second)) || f.belongsTo(pid, c.file.Add(-2*time.Second)) {
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
