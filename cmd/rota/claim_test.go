package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/professor93/rota/store"
)

// seedAccounts writes a store by hand, which is quicker than driving a login
// for accounts whose only interesting property is that they exist.
func seedAccounts(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// busy claims an account the way a run in another terminal would, and gives
// the store lock straight back so the command under test can open it.
func busy(t *testing.T, dir string, id int) {
	t.Helper()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release, ok := s.Hold(s.Find(id))
	if !ok {
		t.Fatalf("account %d was already claimed", id)
	}
	t.Cleanup(release)
	// The claim is not the store lock, so this hands back only the latter.
	_ = s.Release()
}

// Removing several accounts must not half-finish.
//
// Remove deletes the private home before it touches the list, and the store is
// written once at the end — so a refusal partway through used to leave the
// earlier accounts' credentials deleted from disk while the store still listed
// them, having already printed "Removed" for each. For a delegated account
// that directory is the credential: there is no other copy.
//
// So the whole set is checked before anything is deleted.
func TestRemovingSeveralAccountsRefusesBeforeDeletingAny(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROTA_HOME", dir)
	seedAccounts(t, dir, `{"accounts":[
		{"id":1,"provider":"grok","delegated":true,"uuid":"g1","order":1},
		{"id":2,"provider":"grok","delegated":true,"uuid":"g2","order":2},
		{"id":3,"provider":"grok","delegated":true,"uuid":"g3","order":3}],"nextId":4,"ordered":true}`)
	// Something in each home, so "deleted" is a fact rather than an absence.
	for _, id := range []string{"1", "2", "3"} {
		home := filepath.Join(dir, "homes", "grok-"+id)
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	busy(t, dir, 3)

	out, errOut, code := call(t, "remove", "1", "2", "3")
	if code == 0 || !strings.Contains(errOut, "running") {
		t.Fatalf("removing a running account must be refused: %d %q", code, errOut)
	}
	// Nothing may have been reported as removed, because nothing was.
	if strings.Contains(out, "Removed") {
		t.Fatalf("nothing was removed, so nothing may say it was: %q", out)
	}
	for _, id := range []string{"1", "2", "3"} {
		if _, err := os.Stat(filepath.Join(dir, "homes", "grok-"+id, "auth.json")); err != nil {
			t.Fatalf("account %s's credentials were deleted by a command that failed: %v", id, err)
		}
	}
	list, _, _ := call(t, "list", "--short")
	for _, id := range []string{"g1", "g2", "g3"} {
		if !strings.Contains(list, id) {
			t.Fatalf("every account must still be listed after a refusal:\n%s", list)
		}
	}
}

// And when a removal fails for a reason nothing could have foreseen, what did
// happen still reaches disk: an account whose home is gone must not still be
// listed as usable.
func TestAFailedRemovalStillRecordsWhatItRemoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory cannot be made undeletable by its mode on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can delete from a directory it cannot write")
	}
	dir := t.TempDir()
	t.Setenv("ROTA_HOME", dir)
	// The second account's home sits inside a directory nothing may write to,
	// so deleting it fails after the first has already gone.
	locked := filepath.Join(t.TempDir(), "locked")
	stuck := filepath.Join(locked, "two")
	if err := os.MkdirAll(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	seedAccounts(t, dir, `{"accounts":[
		{"id":1,"provider":"grok","delegated":true,"uuid":"g1","order":1},
		{"id":2,"provider":"grok","delegated":true,"uuid":"g2","order":2,"config_dir":"`+stuck+`"}],
		"nextId":3,"ordered":true}`)

	if _, _, code := call(t, "remove", "1", "2"); code == 0 {
		t.Fatal("a home that cannot be deleted must be reported")
	}
	list, _, _ := call(t, "list", "--short")
	if strings.Contains(list, "g1") {
		t.Fatalf("account 1's home is gone, so it must not still be listed:\n%s", list)
	}
	if !strings.Contains(list, "g2") {
		t.Fatalf("account 2 was not removed, so it must remain:\n%s", list)
	}
}

// --all says how many conversations to list, which only means anything when
// the listing was asked for. Taking it silently would look like it did
// something.
func TestAllOnItsOwnIsAMistakeWorthSaying(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	if _, errOut, code := call(t, "list", "--all"); code != 2 || !strings.Contains(errOut, "--sessions") {
		t.Fatalf("--all without --sessions must say so: %d %q", code, errOut)
	}
	if _, _, code := call(t, "list", "--sessions", "--all"); code != 0 {
		t.Fatalf("together they are the whole listing: %d", code)
	}
}

// Signing an account in writes a fresh credential into the very home a running
// CLI is reading from, so it takes the same claim every other writer takes.
//
// This is the fourth path into that directory. A run, the handover and remove
// all hold the account; the delegated login ran the vendor's own `login` with
// no claim at all, which is the one case where the file is certain to be
// rewritten.
func TestSigningInIsRefusedWhileTheAccountIsRunning(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROTA_HOME", dir)
	// Nothing on the path, so a refusal that does not happen fails on the
	// missing binary instead of running somebody's real login.
	t.Setenv("PATH", t.TempDir())
	seedAccounts(t, dir, `{"accounts":[
		{"id":1,"provider":"grok","delegated":true,"uuid":"g1","order":1}],"nextId":2,"ordered":true}`)
	busy(t, dir, 1)

	_, errOut, code := call(t, "login", "1")
	if code == 0 {
		t.Fatal("signing in while the CLI is running must be refused")
	}
	if !strings.Contains(errOut, "running") {
		t.Fatalf("the refusal must say what it is protecting, got %q", errOut)
	}
}
