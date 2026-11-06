package hold

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

func TestTheRegistryShapeIsRead(t *testing.T) {
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

	full, err := parseRegistry(b, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	wantReg := Registry{
		SessionID: "5f0c2a1e-7b3d-4e8a-9c61-2d4f8a0b3e17",
		Name:      "made-up-session",
		Cwd:       "/home/someone/work/project",
		Status:    "idle",
		Kind:      "interactive",
		UpdatedAt: 1793942100981,
	}
	if full.Registry != wantReg {
		t.Fatalf("read %+v, want %+v", full.Registry, wantReg)
	}
	if full.PID != 48213 {
		t.Fatalf("pid %d", full.PID)
	}
	if at := time.Date(2026, time.November, 6, 9, 14, 7, 0, time.Local); !full.ProcStart.Equal(at) {
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
		got, err := parseRegistry(fixtureWith(t, "waitingFor", c.v), time.Local)
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
			got, err := parseRegistry(fixtureWith(t, key, v), time.Local)
			if err != nil || !sameFile(got, want) {
				t.Fatalf("%s as %#v: %+v, %v; want %+v", key, v, got, err, want)
			}
		}
	}
	if got, err := parseRegistry(fixtureWith(t, "procStart", "2026-11-06T09:14:07Z"), time.Local); err != nil || !got.ProcStart.IsZero() {
		t.Fatalf("a procStart in another layout is unknown: %v, %v", got.ProcStart, err)
	}
	if got, err := parseRegistry(fixtureWith(t, "updatedAt", 1793942100981.75), time.Local); err != nil || got.UpdatedAt != 1793942100981 {
		t.Fatalf("a fractional updatedAt: %d, %v", got.UpdatedAt, err)
	}

	// A file that is not one JSON object — one caught half written, say —
	// is an error, so the caller keeps what it last knew.
	for _, bad := range []string{"", `{"pid":48213,"sessi`, "null", "[]", `"x"`} {
		if _, err := parseRegistry([]byte(bad), time.Local); err == nil {
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
	const pid = 48213
	at := time.Date(2026, time.November, 6, 9, 14, 7, 0, time.Local)
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

	// The way it is written: the writer's own start in local time, to the
	// second, read back against a start known to the nanosecond.
	start := time.Now()
	b, err := json.Marshal(map[string]any{"pid": pid, "procStart": start.Local().Format(time.ANSIC)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseRegistry(b, time.Local)
	if err != nil || !got.belongsTo(pid, start) {
		t.Fatalf("its own start did not count: %+v, %v", got, err)
	}
	if got.belongsTo(pid, start.Add(-time.Hour)) {
		t.Fatal("an hour off counted")
	}
}

// In the hour a clock goes back the same wall time happens twice. A child
// started in either one matches its file, which writes only the wall time.
func TestARegistryMatchesInTheRepeatedHour(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no zone database:", err)
	}
	const pid = 48213
	b, err := json.Marshal(map[string]any{"pid": pid, "procStart": "Sun Nov  1 01:30:00 2026"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseRegistry(b, ny)
	if err != nil {
		t.Fatal(err)
	}
	edt := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC) // 01:30 EDT
	est := edt.Add(time.Hour)                                       // 01:30 EST
	for name, start := range map[string]time.Time{"EDT": edt, "EST": est} {
		if !f.belongsTo(pid, start.Add(400*time.Millisecond)) {
			t.Fatalf("a child started at 01:30 %s did not match", name)
		}
	}
	if f.belongsTo(pid, edt.Add(2*time.Second)) || f.belongsTo(pid, est.Add(-2*time.Second)) {
		t.Fatal("a start two seconds off matched")
	}
}
