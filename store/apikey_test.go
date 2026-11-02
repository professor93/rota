package store

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// Made-up credentials in the shapes Claude Code tells apart. None of them is
// anybody's.
const (
	testKey      = "sk-ant-api03-TESTONLY-not-a-real-key-0000000000-WXYZ"
	testOtherKey = "sk-ant-api03-TESTONLY-another-made-up-key-1111-ABCD"
	testSetup    = "sk-ant-oat01-TESTONLY-not-a-real-setup-token-2222"
)

// keyCheck stands in for the API's model list and answers status to every
// question about a key, counting them.
func keyCheck(t *testing.T, status int) *atomic.Int64 {
	t.Helper()
	var asked atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Models
	rota.ClaudeEndpoints.Models = srv.URL + "/v1/models"
	t.Cleanup(func() { rota.ClaudeEndpoints.Models = old })
	return &asked
}

// A key makes an account of its own, outside the rotation because every run
// on it is paid for; the same key again is that same account.
func TestLoginKeyAddsAnAccountOutsideTheRotation(t *testing.T) {
	asked := keyCheck(t, 200)
	s := openTemp(t)
	first := claudeAccount(s, "u1", "one@x")
	if first.Order != 1 {
		t.Fatalf("an ordinary account joins the queue: %d", first.Order)
	}

	a, added, unverified, err := s.LoginKey(context.Background(), "claude", testKey, "")
	if err != nil || unverified != nil || !added {
		t.Fatalf("login: %v %v added=%v", err, unverified, added)
	}
	if !a.APIKey() || a.Order != 0 || a.Label() != "api key ...WXYZ" || !s.Ordered {
		t.Fatalf("a key account starts out of the rotation, on purpose: %+v ordered=%v", a, s.Ordered)
	}
	if asked.Load() != 1 {
		t.Fatalf("the key is checked once: %d", asked.Load())
	}

	again, added, _, err := s.LoginKey(context.Background(), "claude", testKey, "work credit")
	if err != nil || added || again != a || len(s.Accounts) != 2 {
		t.Fatalf("the same key is the same account: %v added=%v n=%d", err, added, len(s.Accounts))
	}
	if a.Label() != "work credit" || a.Order != 0 {
		t.Fatalf("a label names it and nothing else changes: %q %d", a.Label(), a.Order)
	}
	a.Order = 3 // a person put it in the rotation
	if _, _, _, err := s.LoginKey(context.Background(), "claude", testKey, ""); err != nil || a.Order != 3 || a.Label() != "work credit" {
		t.Fatalf("pasting it again keeps what was chosen about it: %v %d %q", err, a.Order, a.Label())
	}

	b, added, _, err := s.LoginKey(context.Background(), "claude", testOtherKey, "")
	if err != nil || !added || b == a || b.Order != 0 {
		t.Fatalf("another key is another account: %v %v", err, added)
	}

	// The key lives in the store, which is private, and nowhere else rota
	// writes: no login is parked for it.
	raw, err := os.ReadFile(filepath.Join(storeDir(s), "accounts.json"))
	if err != nil || !strings.Contains(string(raw), testKey) {
		t.Fatalf("the store holds the key: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(storeDir(s), "accounts.json")); runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the store is private: %o", fi.Mode().Perm())
	}
	if _, err := os.Stat(s.pendingPath()); !os.IsNotExist(err) {
		t.Fatalf("a key login parks nothing: %v", err)
	}
}

func TestLoginKeyRefusedOrMistakenStoresNothing(t *testing.T) {
	keyCheck(t, 401)
	s := openTemp(t)
	for _, in := range []string{testKey, testSetup, "sk-proj-TESTONLY"} {
		_, _, _, err := s.LoginKey(context.Background(), "claude", in, "")
		if !errors.Is(err, rota.ErrInvalidRequest) || strings.Contains(err.Error(), "TESTONLY") {
			t.Fatalf("%s: want a refusal that does not repeat it, got %v", in[:10], err)
		}
	}
	if len(s.Accounts) != 0 {
		t.Fatalf("nothing is stored: %d", len(s.Accounts))
	}
	if _, _, _, err := s.LoginKey(context.Background(), "claude", testKey, "a\nb"); !errors.Is(err, rota.ErrInvalidRequest) {
		t.Fatalf("a label is one line: %v", err)
	}
}

// When the API cannot be asked, the key is taken and that is said.
func TestLoginKeyUncheckedIsStoredAndSaysSo(t *testing.T) {
	s := openTemp(t)
	a, added, unverified, err := s.LoginKey(context.Background(), "claude", testKey, "")
	if err != nil || !added || unverified == nil || a == nil {
		t.Fatalf("fail open: %v %v %v", err, unverified, added)
	}
	if strings.Contains(unverified.Error(), "TESTONLY") {
		t.Fatal("the warning carries the key")
	}
}

func keyAccountIn(t *testing.T, s *Store) *rota.Account {
	t.Helper()
	keyCheck(t, 200)
	a, _, _, err := s.LoginKey(context.Background(), "claude", testKey, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A key account runs in its own home, like any claude account, on the key in
// its environment: no login is written there, nothing is refreshed, and no
// other credential reaches the CLI.
func TestAKeyAccountLaunchesOnItsKeyInItsOwnHome(t *testing.T) {
	f := &claudeFake{}
	newClaudeFake(t, f)
	onPath(t, "claude")
	s := openTemp(t)
	a := keyAccountIn(t, s)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "inherited")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "inherited")

	_, env, release, err := s.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	release()
	var creds []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if k == "ANTHROPIC_API_KEY" || k == "CLAUDE_CODE_OAUTH_TOKEN" || k == "ANTHROPIC_AUTH_TOKEN" {
			creds = append(creds, e)
		}
	}
	if !slices.Equal(creds, []string{"ANTHROPIC_API_KEY=" + testKey}) {
		t.Fatal("the key must be the only credential the CLI is given")
	}
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR="+s.Home(a)) {
		t.Fatal("it runs in its own home")
	}
	if _, err := os.Stat(filepath.Join(s.Home(a), ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("no login is written for a key: %v", err)
	}
	if f.refreshes.Load() != 0 || a.Dead {
		t.Fatalf("a key is never refreshed: %d %v", f.refreshes.Load(), a.Dead)
	}

	// A run without its home is the same key.
	cmd, release, err := s.ready(context.Background(), a, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if !slices.Contains(cmd.Env, "ANTHROPIC_API_KEY="+testKey) || slices.ContainsFunc(cmd.Env, func(e string) bool {
		return strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=")
	}) {
		t.Fatal("a hermetic run is on the key too, and on nothing else")
	}
	if a.Extra[routeKey] != "" {
		t.Fatalf("a key account takes no route of a login's: %q", a.Extra[routeKey])
	}
}

// No usage endpoint is ever asked about a key, however hard a listing asks.
func TestAKeyAccountReadsNoUsage(t *testing.T) {
	var asked atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Usage
	rota.ClaudeEndpoints.Usage = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Usage = old })
	s := openTemp(t)
	a := keyAccountIn(t, s)

	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 {
		t.Fatal(errs)
	}
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if asked.Load() != 0 || a.Quota != nil || a.QuotaAt != 0 || a.Metered() {
		t.Fatalf("usage asked %d times, quota %+v", asked.Load(), a.Quota)
	}
}

func TestAKeyAccountRefusesRemoteControl(t *testing.T) {
	s := openTemp(t)
	a := keyAccountIn(t, s)
	want := *a
	want.RemoteControl = true
	if err := s.CheckRemoteControl(&want); !errors.Is(err, rota.ErrInvalidRequest) || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("want a refusal naming the key, got %v", err)
	}
	if err := s.RemoteControlNow(&want); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("and now too: %v", err)
	}
}

// A setup token goes on the account it is put on, as its long-lived token,
// and never makes one.
func TestPasteLongPutsASetupTokenOnAnAccount(t *testing.T) {
	s := openTemp(t)
	a := claudeAccount(s, "u1", "one@x")
	k := keyAccountIn(t, s)

	got, err := s.PasteLong(a.ID, " "+testSetup+"\n")
	if err != nil || got != a {
		t.Fatalf("paste: %v", err)
	}
	if a.LongAccess() != testSetup || a.LongUntil().Before(time.Now().Add(360*24*time.Hour)) {
		t.Fatalf("a year-long token, where --long puts one: %+v", a.LongUntil())
	}
	if a.Token.Access != "stale" || a.Token.Refresh != "r" {
		t.Fatal("the ordinary login is left alone")
	}
	for name, try := range map[string]func() error{
		"a key account": func() error { _, err := s.PasteLong(k.ID, testSetup); return err },
		"no account":    func() error { _, err := s.PasteLong(99, testSetup); return err },
		"a key":         func() error { _, err := s.PasteLong(a.ID, testKey); return err },
	} {
		err := try()
		if err == nil || strings.Contains(err.Error(), "TESTONLY") {
			t.Fatalf("%s must be refused without repeating it: %v", name, err)
		}
	}
	if k.Long != nil || len(s.Accounts) != 2 {
		t.Fatal("nothing else changed")
	}
}
