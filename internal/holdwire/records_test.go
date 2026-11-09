//go:build linux || darwin

package holdwire

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// record makes one session's directory with meta.json as given; raw, when
// set, is written as meta.json in place of m.
func record(t *testing.T, terminals, id string, m *Meta, raw string) string {
	t.Helper()
	dir := Dir(terminals, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if raw != "" {
		if err := os.WriteFile(MetaPath(dir), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	m.ID = id
	if err := WriteMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	return dir
}

func lock(t *testing.T, dir string) {
	t.Helper()
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}

// Each directory is judged on its own, by its lock and its record, and every
// way a record can be comes back as a different state.
func TestScanTellsEveryStateApart(t *testing.T) {
	terminals := t.TempDir()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	code := 0

	live := record(t, terminals, "000000000000000a", &Meta{Kind: KindAccount, Account: 1, Protocol: Major, Started: at(5)}, "")
	lock(t, live)
	ended := record(t, terminals, "000000000000000b", &Meta{Kind: KindAccount, Account: 2, Protocol: Major, Started: at(4),
		Ended: at(30), Exit: &code}, "")
	interrupted := record(t, terminals, "000000000000000c", &Meta{Kind: KindShell, Protocol: Major, Started: at(3)}, "")
	stopped := at(40)
	if err := os.Chtimes(MetaPath(interrupted), stopped, stopped); err != nil {
		t.Fatal(err)
	}
	// An ended record's file was touched later than it ended; the ending
	// is what it says, not when the file last changed.
	if err := os.Chtimes(MetaPath(ended), at(50), at(50)); err != nil {
		t.Fatal(err)
	}
	unreadable := record(t, terminals, "000000000000000d", &Meta{Kind: KindAccount, Account: 4, Protocol: 9, Version: "9.0.0", Started: at(2)}, "")
	lock(t, unreadable)
	record(t, terminals, "000000000000000e", nil, "{not json")
	// A holder starting: it holds its lock and has yet to write meta.json.
	// Broken, as the other, but locked, which a sweep must see without
	// asking again.
	starting := Dir(terminals, "000000000000000f")
	if err := os.MkdirAll(starting, 0o700); err != nil {
		t.Fatal(err)
	}
	lock(t, starting)
	if err := os.Chtimes(starting, at(1), at(1)); err != nil {
		t.Fatal(err)
	}

	// Things that live beside the records and are none of them.
	for _, f := range []string{"ctl.sock", "daemon.lock", "0123456789abcdef.log"} {
		if err := os.WriteFile(filepath.Join(terminals, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"nothex", "0123456789ABCDEF", "0123456789abcde"} {
		if err := os.MkdirAll(filepath.Join(terminals, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	got := Scan(terminals)
	want := []struct {
		id     string
		state  RecordState
		locked bool
	}{
		{"000000000000000a", StateLive, true},
		{"000000000000000b", StateEnded, false},
		{"000000000000000c", StateInterrupted, false},
		{"000000000000000d", StateUnreadable, true},
		{"000000000000000e", StateBroken, false},
		{"000000000000000f", StateBroken, true},
	}
	if len(got) != len(want) {
		t.Fatalf("%d records: %+v", len(got), got)
	}
	// Newest first. The broken ones have no start of their own and sort by
	// their directories: the one with a bad meta.json is the newest thing
	// here, and the starting one was dated before everything else.
	byID := map[string]Record{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if got[0].ID != "000000000000000e" || got[1].ID != "000000000000000a" || got[4].ID != "000000000000000d" ||
		got[5].ID != "000000000000000f" {
		t.Errorf("not newest first: %s %s ... %s %s", got[0].ID, got[1].ID, got[4].ID, got[5].ID)
	}
	for _, w := range want {
		r, ok := byID[w.id]
		if !ok {
			t.Errorf("%s is missing", w.id)
			continue
		}
		if r.State != w.state {
			t.Errorf("%s is %s, want %s (%v)", w.id, r.State, w.state, r.Err)
		}
		if r.Locked != w.locked {
			t.Errorf("%s is locked: %v, want %v", w.id, r.Locked, w.locked)
		}
		if r.Dir != Dir(terminals, w.id) {
			t.Errorf("%s is at %s", w.id, r.Dir)
		}
	}
	for _, id := range []string{"000000000000000e", "000000000000000f"} {
		if r := byID[id]; r.Err == nil || r.Meta != nil {
			t.Errorf("a broken record says nothing about why: %+v", r)
		}
	}
	if r := byID["000000000000000f"]; !r.Stopped.IsZero() {
		t.Errorf("a session with no meta.json yet stopped at %v", r.Stopped)
	}
	if r := byID["000000000000000c"]; !r.Stopped.Equal(stopped) {
		t.Errorf("interrupted at %v, want meta.json's mtime %v", r.Stopped, stopped)
	}
	if r := byID["000000000000000b"]; !r.Stopped.Equal(at(30)) {
		t.Errorf("ended at %v, want %v", r.Stopped, at(30))
	}
	if r := byID["000000000000000a"]; !r.Stopped.IsZero() || r.Meta == nil || r.Meta.Account != 1 {
		t.Errorf("the live one: %+v", r)
	}

	if h := Holding(terminals, 1); h == nil || h.ID != "000000000000000a" {
		t.Errorf("account 1 is held by %+v", h)
	}
	if h := Holding(terminals, 2); h != nil {
		t.Errorf("an ended session holds account 2: %+v", h)
	}
	if h := Holding(terminals, 4); h == nil || h.ID != "000000000000000d" {
		t.Errorf("a holder this rota cannot read still holds its account: %+v", h)
	}
	if Scan(filepath.Join(terminals, "missing")) != nil {
		t.Error("a store with no terminals has records")
	}
}

// A locked directory whose record cannot be read is a holder running all
// the same: unreadable, not broken. What can still be read of which process
// it is comes with it, but only all of it, and only from a record that
// names this session. Nobody holding the lock, the same file is broken.
func TestALockedRecordThatCannotBeReadIsUnreadable(t *testing.T) {
	terminals := t.TempDir()
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	later := `{"id":"%s","account":{"id":1},"holder_pid":42,"holder_start":"` + start.Format(time.RFC3339) +
		`","holder_start_ticks":12345,"boot_id":"b-1","protocol":3,"version":"10.0.0"}`
	cases := []struct {
		id, raw string
		locked  bool
		state   RecordState
		pid     int
		version string
	}{
		{"0000000000000001", "{not json", true, StateUnreadable, 0, ""},
		{"0000000000000002", fmt.Sprintf(later, "0000000000000002"), true, StateUnreadable, 42, "10.0.0"},
		{"0000000000000003", fmt.Sprintf(later, "00000000000000ff"), true, StateUnreadable, 0, ""},
		{"0000000000000004", `{"id":"0000000000000004","holder_pid":"42","holder_start":"x","boot_id":"b-1","account":[]}`, true, StateUnreadable, 0, ""},
		{"0000000000000005", fmt.Sprintf(later, "0000000000000005"), false, StateBroken, 0, ""},
	}
	for _, c := range cases {
		dir := record(t, terminals, c.id, nil, c.raw)
		if c.locked {
			lock(t, dir)
		}
	}
	byID := map[string]Record{}
	for _, r := range Scan(terminals) {
		byID[r.ID] = r
	}
	for _, c := range cases {
		r := byID[c.id]
		if r.State != c.state || r.Locked != c.locked || r.Err == nil {
			t.Errorf("%s is %s, locked %v (%v); want %s", c.id, r.State, r.Locked, r.Err, c.state)
			continue
		}
		if c.state == StateBroken {
			if r.Meta != nil {
				t.Errorf("%s: a broken record has %+v", c.id, r.Meta)
			}
			continue
		}
		if m := r.Meta; m == nil || m.ID != c.id || m.HolderPID != c.pid || m.Version != c.version || !r.Stopped.IsZero() {
			t.Errorf("%s: what could be read is %+v, stopped %v", c.id, m, r.Stopped)
		} else if c.pid != 0 && (!m.HolderStart.Equal(start) || m.HolderStartTicks != 12345 || m.BootID != "b-1" || m.Protocol != 3) {
			t.Errorf("%s: the holder is %+v", c.id, m)
		}
	}
}

// Judge is Scan's judgement of one directory, and no directory at all for
// anything that is not a session's: a name that is not an id, nothing
// there, a file, or a symlink, which is never followed out of the store.
func TestJudgeIsScanForOneSession(t *testing.T) {
	terminals := t.TempDir()
	code := 3
	live := record(t, terminals, "000000000000000a", &Meta{Kind: KindAccount, Account: 1, Protocol: Major}, "")
	lock(t, live)
	record(t, terminals, "000000000000000b", &Meta{Protocol: Major, Ended: time.Now().Truncate(time.Second), Exit: &code}, "")
	record(t, terminals, "000000000000000c", &Meta{Protocol: Major}, "")
	record(t, terminals, "000000000000000e", nil, "{not json")
	for _, r := range Scan(terminals) {
		got := Judge(terminals, r.ID)
		if got.State != r.State || got.Locked != r.Locked || !got.Stopped.Equal(r.Stopped) || got.Dir != r.Dir {
			t.Errorf("Judge(%s) is %+v, Scan said %+v", r.ID, got, r)
		}
	}

	outside := record(t, t.TempDir(), "000000000000000d", &Meta{Protocol: Major}, "")
	lock(t, outside)
	if err := os.Symlink(outside, Dir(terminals, "000000000000000d")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Dir(terminals, "000000000000000f"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"000000000000000d", "000000000000000f", "0000000000000009", "../00000000000a", "000000000000000A"} {
		if r := Judge(terminals, id); r.State != StateBroken || r.Meta != nil || !errors.Is(r.Err, fs.ErrNotExist) || r.Locked {
			t.Errorf("Judge(%q) is %+v, want no record", id, r)
		}
	}
}

// Asking whether something is locked never takes the lock, and a lock is
// one holder's at a time.
func TestLockedNeverTakesTheLock(t *testing.T) {
	dir := t.TempDir()
	if Locked(LockPath(dir)) {
		t.Fatal("a lock file that is not there is locked")
	}
	if _, err := os.Stat(LockPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("asking made the lock file: %v", err)
	}
	if err := os.WriteFile(LockPath(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if Locked(LockPath(dir)) {
		t.Fatal("a free lock is locked")
	}
	release, err := Lock(dir)
	if err != nil {
		t.Fatalf("the lock was taken by asking: %v", err)
	}
	if !Locked(LockPath(dir)) {
		t.Fatal("a held lock is free")
	}
	if _, err := Lock(dir); err == nil {
		t.Fatal("a second holder took the same lock")
	}
	release()
	release()
	if Locked(LockPath(dir)) {
		t.Fatal("a released lock is held")
	}
}

// A record names its session by the first of what the person called it, the
// conversation's own name, its folder and its id; says whether it runs, how
// it ended and whether Resume can take it up; and HeldBy names the newest
// running session on an account in the words a busy refusal ends with.
func TestARecordSaysWhatItIsCalledAndHowItStands(t *testing.T) {
	for _, c := range []struct {
		m    *Meta
		want string
	}{
		{&Meta{Name: "api", ConversationName: "conv", Cwd: "/src/x"}, "api"},
		{&Meta{ConversationName: "conv", Cwd: "/src/x"}, "conv"},
		{&Meta{Cwd: "/src/x"}, "x"},
		{&Meta{}, "0123456789abcdef"},
		{nil, "0123456789abcdef"},
	} {
		r := Record{ID: "0123456789abcdef", Meta: c.m}
		if got := r.Name(); got != c.want {
			t.Errorf("%+v is called %q; want %q", c.m, got, c.want)
		}
	}

	code := 3
	for _, c := range []struct {
		r                  Record
		running, resumable bool
		exit               int
	}{
		{Record{State: StateLive, Meta: &Meta{}}, true, false, -1},
		{Record{State: StateUnreadable, Meta: &Meta{}}, true, false, -1},
		{Record{State: StateEnded, Meta: &Meta{Exit: &code, Conversation: "c"}}, false, false, 3},
		{Record{State: StateInterrupted, Meta: &Meta{Conversation: "c"}}, false, true, -1},
		{Record{State: StateInterrupted, Meta: &Meta{}}, false, false, -1},
		{Record{State: StateBroken}, false, false, -1},
	} {
		if c.r.Running() != c.running || c.r.Resumable() != c.resumable || c.r.ExitCode() != c.exit {
			t.Errorf("%s: running %v, resumable %v, exit %d; want %v, %v, %d", c.r.State,
				c.r.Running(), c.r.Resumable(), c.r.ExitCode(), c.running, c.resumable, c.exit)
		}
	}

	terminals := t.TempDir()
	now := time.Now().Truncate(time.Second)
	older := record(t, terminals, "000000000000000a", &Meta{Kind: KindAccount, Account: 1, Protocol: Major, Name: "older",
		Started: now.Add(-time.Hour)}, "")
	lock(t, older)
	newer := record(t, terminals, "000000000000000b", &Meta{Kind: KindAccount, Account: 1, Protocol: Major,
		Cwd: "/src/api", Started: now}, "")
	lock(t, newer)
	record(t, terminals, "000000000000000c", &Meta{Kind: KindAccount, Account: 2, Protocol: Major, Name: "gone",
		Started: now, Ended: now, Exit: &code}, "")
	if got := HeldBy(terminals, 1); got != " — held by session api (000000000000000b)" {
		t.Errorf("HeldBy(1) = %q", got)
	}
	if got := HeldBy(terminals, 2); got != "" {
		t.Errorf("HeldBy(2), whose only session has ended, = %q", got)
	}
}
