//go:build unix

package rotation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// The rotation steps past an account that is already running.
//
// Some providers hand their CLI a private home and let it rewrite the
// credential file there, so a second run on one is refused. Handing such an
// account to every waiting request would mean all but the first are refused
// while another account sits idle right behind it.
//
// Unix-only because the stand-in for "another process is running this
// account" is flock, which is how the claim is really kept there; Windows
// keeps the claim through LockFileEx and is exercised by the lock tests.
func TestChooseStepsPastAnAccountThatIsAlreadyRunning(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"t-rot-owns","order":1,"token":{"accessToken":"a"}},
		{"id":2,"provider":"t-rot-owns","order":2,"token":{"accessToken":"b"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Nothing running: the queue's own order decides.
	got, err := Choose(context.Background(), st, 0)
	if err != nil || got.ID != 1 {
		t.Fatalf("the first in the queue: %v %v", got, err)
	}

	// Account 1 is running, so the next one answers instead of a refusal.
	busy(t, st, st.Find(1))
	got, err = Choose(context.Background(), st, 0)
	if err != nil || got.ID != 2 {
		t.Fatalf("the rotation must move on rather than hand out a busy account: %v %v", got, err)
	}

	// Both running: saying why is better than saying "spent", which they are
	// not, and which would send someone to raise a threshold for nothing.
	busy(t, st, st.Find(2))
	if _, err = Choose(context.Background(), st, 0); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("the reason has to be the true one: %v", err)
	}

	// Naming an account by id still reaches it; the caller asked for that one
	// and Run is where the refusal belongs.
	if got, err := Choose(context.Background(), st, 1); err != nil || got.ID != 1 {
		t.Fatalf("an account named by id is not stepped past: %v %v", got, err)
	}
}

// busy holds an account's run lock for the rest of the test, which is what a
// run in another process looks like from here.
func busy(t *testing.T, st *store.Store, a *rota.Account) {
	t.Helper()
	home := st.Home(a)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(home, ".rota-run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}
