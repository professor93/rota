package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// codexEndpoint stands in for OpenAI's token endpoint as it behaves: every
// refresh token is good once, and presenting one again is refused for good.
// It records every refresh token presented to it, in order.
type codexEndpoint struct {
	mu        sync.Mutex
	presented []string
}

func (e *codexEndpoint) grants() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.presented)
}

func codexRefreshes(t *testing.T) *codexEndpoint {
	t.Helper()
	e := &codexEndpoint{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rt := r.PostForm.Get("refresh_token")
		e.mu.Lock()
		defer e.mu.Unlock()
		spent := slices.Contains(e.presented, rt)
		e.presented = append(e.presented, rt)
		w.Header().Set("Content-Type", "application/json")
		if spent {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh_token_reused"}`))
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"r-rota-%d","id_token":"i","expires_in":864000}`,
			codexJWT(time.Now().Add(240*time.Hour)), len(e.presented))
	}))
	t.Cleanup(srv.Close)
	old := rota.CodexEndpoints.Token
	rota.CodexEndpoints.Token = srv.URL
	t.Cleanup(func() { rota.CodexEndpoints.Token = old })
	return e
}

// codexJWT is an access token that says when it expires, as codex's do.
func codexJWT(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".x"
}

// daemonWrote writes the auth.json Codex's daemon leaves after it refreshed:
// a new refresh token, and an access token expiring at exp.
func daemonWrote(t *testing.T, home, refresh string, exp time.Time) {
	t.Helper()
	writeFile(t, filepath.Join(home, "auth.json"),
		`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"i","access_token":"`+
			codexJWT(exp)+`","refresh_token":"`+refresh+`","account_id":"acct"}}`)
}

// homeHolds is the refresh token the home's auth.json carries.
func homeHolds(t *testing.T, home string) string {
	t.Helper()
	var doc struct {
		Tokens struct {
			Refresh string `json:"refresh_token"`
		} `json:"tokens"`
	}
	raw, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || json.Unmarshal(raw, &doc) != nil {
		t.Fatalf("auth.json: %v %s", err, raw)
	}
	return doc.Tokens.Refresh
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
	e := codexRefreshes(t)
	s, a := dueCodex(t)
	codexDaemonAt(t, s.Home(a), "daemon.pid", os.Getpid())

	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	if len(e.grants()) != 0 || a.Token.Refresh != "r-old" {
		t.Fatalf("a login a daemon holds is the daemon's to refresh: presented %v, token %q", e.grants(), a.Token.Refresh)
	}

	// The daemon refreshes; the next sweep adopts what it wrote and keeps it.
	// What it wrote is due again, so only the daemon's being alive keeps
	// rota from refreshing it.
	daemonWrote(t, s.Home(a), "r-daemon", time.Now().Add(time.Minute))
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	if len(e.grants()) != 0 || a.Token.Refresh != "r-daemon" {
		t.Fatalf("what the daemon rotated is read back: presented %v, token %q", e.grants(), a.Token.Refresh)
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

// Once the daemon is gone the login is rota's again, as it always was, and
// what rota refreshes goes into the home at once, so whatever reads the home
// next — a daemon started again, a CLI — finds the token that is still good.
func TestMaintenanceRefreshesACodexLoginOnceItsDaemonIsGone(t *testing.T) {
	e := codexRefreshes(t)
	s, a := dueCodex(t)
	codexDaemonAt(t, s.Home(a), "daemon.pid", deadPID)

	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	if !slices.Equal(e.grants(), []string{"r-old"}) || a.Token.Refresh != "r-rota-1" {
		t.Fatalf("a due login with no daemon is refreshed: presented %v, token %q", e.grants(), a.Token.Refresh)
	}
	if got := homeHolds(t, s.Home(a)); got != "r-rota-1" {
		t.Fatalf("the refreshed login is written into the home: %q", got)
	}
}

// The whole way a daemon's login comes back to rota: the daemon rotates, rota
// reads that back, the daemon goes away — a reboot, a stop — and rota's
// sweeps carry on. No refresh token is ever presented twice. Before, rota
// remembered only what it had written itself, so once it refreshed past the
// daemon's token the next sweep took the daemon's file for a newer rotation,
// read the spent token back over the good one, and presented it again.
func TestALoginBackFromADeadDaemonPresentsNoRefreshTokenTwice(t *testing.T) {
	e := codexRefreshes(t)
	s, a := dueCodex(t)
	home := s.Home(a)
	codexDaemonAt(t, home, "daemon.pid", os.Getpid())
	daemonWrote(t, home, "r-daemon", time.Now().Add(time.Minute))
	if errs := s.Maintain(context.Background()); len(errs) != 0 || a.Token.Refresh != "r-daemon" {
		t.Fatalf("the daemon's rotation is read back: %v %q", errs, a.Token.Refresh)
	}

	codexDaemonAt(t, home, "daemon.pid", deadPID)
	for sweep := range 3 {
		if errs := s.Maintain(context.Background()); len(errs) != 0 {
			t.Fatalf("sweep %d after the daemon died: %v (presented %v)", sweep, errs, e.grants())
		}
	}
	got := e.grants()
	if !slices.Equal(got, []string{"r-daemon"}) {
		t.Fatalf("each refresh token is presented once, the daemon's last one by rota: presented %v", got)
	}
	if a.Dead || a.Token.Refresh != "r-rota-1" {
		t.Fatalf("the account keeps the token rota got: dead=%v %q, token %q", a.Dead, a.DeadReason, a.Token.Refresh)
	}
}

// A run is the same: with the daemon alive it adopts and stages, and the CLI
// — or the daemon it attaches to — refreshes the token when it needs to.
func TestARunLeavesACodexLoginToItsDaemon(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no codex: Prepare stops at finding it, after the part watched here
	for _, c := range []struct {
		what      string
		daemon    bool
		refreshes int
		token     string
	}{
		{"with the daemon alive", true, 0, "r-old"},
		{"with no daemon", false, 1, "r-rota-1"},
	} {
		e := codexRefreshes(t)
		s, a := dueCodex(t)
		if c.daemon {
			codexDaemonAt(t, s.Home(a), "app-server.pid", os.Getpid())
		}
		_, _, release, _ := s.Prepare(context.Background(), a)
		if release != nil {
			release()
		}
		if len(e.grants()) != c.refreshes || a.Token.Refresh != c.token {
			t.Fatalf("%s: presented %v, token %q", c.what, e.grants(), a.Token.Refresh)
		}
	}
}

// Maintenance writes what it refreshed into the home through a launch, which
// reads the home back first. The home still holds the token the refresh has
// just spent, and that file must be known to be the store's own, or it would
// be taken for a newer rotation and read back over the good token, to be
// presented again at the next sweep. It is known when rota wrote it last
// ("-" is a login newer than the file, which is never read back either). It
// must also be known when rota's record of its own write says nothing — a
// store from before rota kept one — or names an older write, as after a run
// whose last save failed: reading back a file that holds exactly the store's
// login now records it.
func TestAHomeWriteAfterARefreshReadsNothingBack(t *testing.T) {
	for _, staged := range []string{"", "0123abcd", "-"} {
		e := codexRefreshes(t)
		s := openTemp(t)
		a := s.add("codex")
		due := time.Now().Add(time.Minute)
		a.Token = rota.Token{Access: codexJWT(due), Refresh: "r-old", ExpiresAt: due.UnixMilli()}
		a.Extra = map[string]string{"id_token": "i", "account_id": "acct"}
		if _, err := rota.Stage(a, s.Home(a)); err != nil {
			t.Fatal(err)
		}
		a.Staged = staged
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		for sweep := range 3 {
			if errs := s.Maintain(context.Background()); len(errs) != 0 {
				t.Fatalf("staged %q, sweep %d: %v (presented %v)", staged, sweep, errs, e.grants())
			}
		}
		if got := e.grants(); !slices.Equal(got, []string{"r-old"}) || a.Dead || a.Token.Refresh != "r-rota-1" {
			t.Fatalf("staged %q: presented %v, dead=%v, token %q", staged, got, a.Dead, a.Token.Refresh)
		}
		if got := homeHolds(t, s.Home(a)); got != "r-rota-1" {
			t.Fatalf("staged %q: the home holds %q", staged, got)
		}
	}
}

// The refreshed token is on disk before the home is written, as a launch's
// is: the token it replaced is spent, so nothing after the refresh — a write,
// another account's network call, a process that ends — may stand between
// the new one and the store.
func TestMaintenanceSavesARefreshedCodexTokenBeforeWritingTheHome(t *testing.T) {
	codexRefreshes(t)
	var (
		s          *Store
		a          *rota.Account
		homeAtSave []string
	)
	b := watchedSaves{memBackend: &memBackend{home: t.TempDir()}}
	b.saved = func(blob []byte) {
		if a != nil && strings.Contains(string(blob), "r-rota-1") {
			homeAtSave = append(homeAtSave, homeHolds(t, s.Home(a)))
		}
	}
	s, err := NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a = s.add("codex")
	a.Token = rota.Token{Access: "a-old", Refresh: "r-old", ExpiresAt: 1}
	a.Extra = map[string]string{"id_token": "i", "account_id": "acct"}
	if _, err := rota.Stage(a, s.Home(a)); err != nil {
		t.Fatal(err)
	}
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	if len(homeAtSave) == 0 || homeAtSave[0] != "r-old" {
		t.Fatalf("the refreshed token is saved while the home still holds the spent one: the home held %v at those saves", homeAtSave)
	}
	if got := homeHolds(t, s.Home(a)); got != "r-rota-1" {
		t.Fatalf("and then it goes into the home: %q", got)
	}
}

// watchedSaves is a memBackend that shows each save to a test as it happens.
type watchedSaves struct {
	*memBackend
	saved func([]byte)
}

func (w watchedSaves) Save(b []byte) error {
	w.saved(b)
	return w.memBackend.Save(b)
}

// A sweep that cannot save the store spends no refresh token. Before it
// refreshes a codex login that is due, maintenance writes that login into the
// home and saves the store; when the save fails it refreshes nothing until a
// later sweep. Otherwise the new token could end up only in the home, and a
// home the store records as holding an older login's file — an account
// signed in and never run since — is never read back: the new token would
// be lost, and the next sweep would present the spent one again.
func TestASweepThatCannotSaveSpendsNoToken(t *testing.T) {
	for _, c := range []struct {
		what   string
		file   string // the refresh token the home holds: "" none, "store" the store's own
		staged string // what the store records of its write into the home; "keep" what staging recorded
	}{
		{"a home rota staged", "store", "keep"},
		{"a home rota staged, record unknown", "store", ""},
		{"signed in, never run, empty home", "", "-"},
		{"signed in, never run, the previous login's file", "r-prev", "-"},
	} {
		e := codexRefreshes(t)
		m := &memBackend{home: t.TempDir()}
		s, err := NewStore(m)
		if err != nil {
			t.Fatal(err)
		}
		a := s.add("codex")
		due := time.Now().Add(time.Minute)
		a.Extra = map[string]string{"id_token": "i", "account_id": "acct"}
		a.Token = rota.Token{Access: codexJWT(due), Refresh: "r-old", ExpiresAt: due.UnixMilli()}
		if c.file != "" {
			if c.file != "store" {
				a.Token.Refresh = c.file
			}
			if _, err := rota.Stage(a, s.Home(a)); err != nil {
				t.Fatal(err)
			}
			a.Token.Refresh = "r-old"
		}
		if c.staged != "keep" {
			a.Staged = c.staged
		}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}

		m.failOn = "save"
		if errs := s.Maintain(context.Background()); len(errs) == 0 {
			t.Fatalf("%s: a store that cannot be saved is reported", c.what)
		}
		if got := e.grants(); len(got) != 0 {
			t.Fatalf("%s: a sweep that cannot save spends nothing: presented %v", c.what, got)
		}
		s.Close() // the server drops each sweep's store, as a process that ends does
		m.failOn = ""

		again, err := NewStore(m)
		if err != nil {
			t.Fatal(err)
		}
		b := again.Find(a.ID)
		for sweep := range 2 {
			if errs := again.Maintain(context.Background()); len(errs) != 0 {
				t.Fatalf("%s, sweep %d after: %v (presented %v)", c.what, sweep, errs, e.grants())
			}
		}
		again.Close()
		if got := e.grants(); !slices.Equal(got, []string{"r-old"}) || b.Dead || b.Token.Refresh != "r-rota-1" {
			t.Fatalf("%s: presented %v, dead=%v %q, token %q", c.what, got, b.Dead, b.DeadReason, b.Token.Refresh)
		}
	}
}
