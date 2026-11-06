//go:build linux || darwin

package holdwire

import (
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
