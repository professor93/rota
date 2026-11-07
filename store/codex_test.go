package store

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// codexRefreshes stands in for OpenAI's token endpoint and counts the refresh
// grants it is sent.
func codexRefreshes(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"a-rota","refresh_token":"r-rota","id_token":"i","expires_in":864000}`)
	}))
	t.Cleanup(srv.Close)
	old := rota.CodexEndpoints.Token
	rota.CodexEndpoints.Token = srv.URL
	t.Cleanup(func() { rota.CodexEndpoints.Token = old })
	return &n
}

// dueCodex is a codex account whose access token is due for a refresh, staged
// into its home the way a run leaves it.
func dueCodex(t *testing.T) (*Store, *rota.Account) {
	t.Helper()
	s := openTemp(t)
	a := s.add("codex")
	a.Token = rota.Token{Access: "a-old", Refresh: "r-old", ExpiresAt: 1}
	a.Extra = map[string]string{"id_token": "i", "account_id": "acct"}
	if _, err := rota.Stage(a, s.Home(a)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	return s, a
}

// codexDaemonAt writes the record Codex keeps for the daemon it runs in a
// home.
func codexDaemonAt(t *testing.T, home, name string, pid int) {
	t.Helper()
	writeFile(t, filepath.Join(home, "app-server-daemon", name), `{"pid":`+itoa(pid)+`,"processStartTime":"x"}`)
}

// Alive is what Codex's own record says: a daemon record naming a live pid,
// or a record nobody can read. The updater Codex may run beside the daemon
// holds no login.
func TestACodexDaemonIsAliveByTheRecordCodexKeeps(t *testing.T) {
	for _, c := range []struct {
		what, name, body string
		alive            bool
	}{
		{"a daemon", "daemon.pid", `{"pid":` + itoa(os.Getpid()) + `,"processStartTime":"x"}`, true},
		{"a daemon from a standalone install", "app-server.pid", `{"pid":` + itoa(os.Getpid()) + `}`, true},
		{"an unreadable record", "daemon.pid", `{"pid":`, true},
		{"a record with no pid", "daemon.pid", `{}`, true},
		{"a dead daemon", "daemon.pid", `{"pid":` + itoa(deadPID) + `}`, false},
		{"the updater alone", "daemon-updater.pid", `{"pid":` + itoa(os.Getpid()) + `}`, false},
	} {
		home := t.TempDir()
		writeFile(t, filepath.Join(home, "app-server-daemon", c.name), c.body)
		if _, got := codexDaemon(home); got != c.alive {
			t.Fatalf("%s: alive=%v", c.what, got)
		}
	}
	if _, alive := codexDaemon(t.TempDir()); alive {
		t.Fatal("a home with no record is quiet")
	}
}

// Codex leaves its daemon running after an interactive session, and the
// daemon refreshes the login itself when it falls due — at the same five
// minutes before expiry rota uses. A refresh by rota would leave the daemon
// holding a spent refresh token, which it presents at its next turn; codex
// refuses a reused one for good. So maintenance reads back what the daemon
// wrote, keeps it, and refreshes nothing.
func TestMaintenanceLeavesACodexLoginToItsDaemon(t *testing.T) {
	n := codexRefreshes(t)
	s, a := dueCodex(t)
	codexDaemonAt(t, s.Home(a), "daemon.pid", os.Getpid())

	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	if n.Load() != 0 || a.Token.Refresh != "r-old" {
		t.Fatalf("a login a daemon holds is the daemon's to refresh: %d refreshes, token %q", n.Load(), a.Token.Refresh)
	}

	// The daemon refreshes; the next sweep adopts what it wrote and keeps it.
	writeFile(t, filepath.Join(s.Home(a), "auth.json"),
		`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"i","access_token":"a-daemon","refresh_token":"r-daemon","account_id":"acct"}}`)
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	if n.Load() != 0 || a.Token.Refresh != "r-daemon" {
		t.Fatalf("what the daemon rotated is read back: %d refreshes, token %q", n.Load(), a.Token.Refresh)
	}
	s.Close()
	again, err := Open(storeDir(s))
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := again.Find(a.ID).Token.Refresh; got != "r-daemon" {
		t.Fatalf("an adopted rotation is saved, not left in memory: %q", got)
	}
}

// Once the daemon is gone the login is rota's again, as it always was.
func TestMaintenanceRefreshesACodexLoginOnceItsDaemonIsGone(t *testing.T) {
	n := codexRefreshes(t)
	s, a := dueCodex(t)
	codexDaemonAt(t, s.Home(a), "daemon.pid", deadPID)

	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	if n.Load() != 1 || a.Token.Refresh != "r-rota" {
		t.Fatalf("a due login with no daemon is refreshed: %d refreshes, token %q", n.Load(), a.Token.Refresh)
	}
}

// A run is the same: with the daemon alive it adopts and stages, and the CLI
// — or the daemon it attaches to — refreshes the token when it needs to.
func TestARunLeavesACodexLoginToItsDaemon(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no codex: Prepare stops at finding it, after the part watched here
	for _, c := range []struct {
		what      string
		daemon    bool
		refreshes int32
		token     string
	}{
		{"with the daemon alive", true, 0, "r-old"},
		{"with no daemon", false, 1, "r-rota"},
	} {
		n := codexRefreshes(t)
		s, a := dueCodex(t)
		if c.daemon {
			codexDaemonAt(t, s.Home(a), "app-server.pid", os.Getpid())
		}
		_, _, release, _ := s.Prepare(context.Background(), a)
		if release != nil {
			release()
		}
		if n.Load() != c.refreshes || a.Token.Refresh != c.token {
			t.Fatalf("%s: %d refreshes, token %q", c.what, n.Load(), a.Token.Refresh)
		}
	}
}
