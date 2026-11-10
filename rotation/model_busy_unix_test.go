//go:build unix

package rotation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/askmodel"
	"github.com/professor93/rota/store"
)

// Accounts whose catalog lists no model asked for, all of them running, are
// still said to be running: a request for a model nobody lists is chosen for
// as one that names none, and "wait" is the true answer, not "spent".
func TestBusyAccountsThatListNoSuchModelAreStillRunning(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"codex","order":1,"token":{"accessToken":"a"}},
		{"id":2,"provider":"codex","order":2,"token":{"accessToken":"b"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	busy(t, st, st.Find(1))
	busy(t, st, st.Find(2))
	_, err = Choose(askmodel.With(context.Background(), "fable"), st, 0)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("the reason has to be the true one: %v", err)
	}
}
