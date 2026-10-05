package store

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/professor93/rota/internal/claudecode"
	rota "github.com/professor93/rota/lib"
)

// deadPID names no process on any machine these tests run on.
const deadPID = 2147483000

// livingClaude adds a claude account whose login Claude Code can keep: a
// refresh token, an access token good for an hour, and the refresh token's
// own expiry, which is what tells a rotation of this login from a new one.
func livingClaude(s *Store, uuid string) *rota.Account {
	a := s.add("claude")
	a.UUID, a.Email = uuid, uuid+"@x"
	a.Token = rota.Token{Access: "A-" + uuid, Refresh: "R-" + uuid, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		Scopes: []string{"user:inference"}}
	a.Extra = map[string]string{"refresh_token_expires_at": "1999999999000", "subscription_type": "max", "rate_limit_tier": "t20"}
	a.StagedSuperseded()
	return a
}

// expire makes the account's access token one that must be refreshed.
func expire(a *rota.Account) { a.Token.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli() }

// later is a moment the given time from now, in unix ms.
func later(d time.Duration) int64 { return time.Now().Add(d).UnixMilli() }

// storeLogin is a credential store holding one login, as Claude Code writes
// it, with something of Claude Code's own beside it.
func storeLogin(access, refresh string, expires int64, until string) string {
	b, _ := json.Marshal(map[string]any{
		"mcpOAuth": map[string]any{"srv": map[string]string{"accessToken": "mcp-secret"}},
		"claudeAiOauth": map[string]any{"accessToken": access, "refreshToken": refresh, "expiresAt": expires,
			"refreshTokenExpiresAt": json.RawMessage(until), "scopes": []string{"user:inference"}},
	})
	return string(b)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

// alive writes a liveness record naming pid, as Claude Code does for a
// window somebody has open.
func alive(t *testing.T, home, name string, pid int) {
	t.Helper()
	writeFile(t, filepath.Join(home, name), `{"pid":`+itoa(pid)+`,"kind":"interactive","status":"busy"}`)
}

// hosted writes the record of a background session the daemon runs.
func hosted(t *testing.T, home, name string, pid int) {
	t.Helper()
	writeFile(t, filepath.Join(home, name), `{"pid":`+itoa(pid)+`,"kind":"bg","status":"busy"}`)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// readLogin is the login in a home's credential file.
func readLogin(t *testing.T, home string) (refresh string, doc map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".credentials.json"))
	if err != nil {
		return "", nil
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	l, _ := doc["claudeAiOauth"].(map[string]any)
	r, _ := l["refreshToken"].(string)
	return r, doc
}

// keychain stands in for the macOS keychain, and remembers every question
// security was asked. findCode and deleteCode, when set, are what security
// answers instead of the truth.
type keychain struct {
	mu         sync.Mutex
	items      map[string]string
	log        []string
	onDelete   func(service string)
	findCode   int
	deleteCode int
}

func fakeKeychain(t *testing.T) *keychain {
	t.Helper()
	k := &keychain{items: map[string]string{}}
	oldKept := keychainKept
	keychainKept = true
	claudecode.Security = func(_ context.Context, args ...string) ([]byte, int, error) {
		k.mu.Lock()
		defer k.mu.Unlock()
		svc := ""
		for i, a := range args {
			if a == "-s" && i+1 < len(args) {
				svc = args[i+1]
			}
		}
		k.log = append(k.log, args[0]+" "+svc)
		switch args[0] {
		case "find-generic-password":
			if k.findCode != 0 {
				return nil, k.findCode, nil
			}
			if v, ok := k.items[svc]; ok {
				return []byte(v + "\n"), 0, nil
			}
			return nil, claudecode.Absent, nil
		case "delete-generic-password":
			if k.onDelete != nil {
				k.onDelete(svc)
			}
			if k.deleteCode != 0 {
				return nil, k.deleteCode, nil
			}
			if _, ok := k.items[svc]; ok {
				delete(k.items, svc)
				return nil, 0, nil
			}
			return nil, claudecode.Absent, nil
		}
		t.Errorf("rota must never ask security for %v", args)
		return nil, 1, nil
	}
	t.Cleanup(func() {
		keychainKept = oldKept
		claudecode.StandIn()
	})
	return k
}

func (k *keychain) asked() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.log)
}

func (k *keychain) item(svc string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.items[svc]
	return v, ok
}

func service(t *testing.T, home string) string {
	t.Helper()
	svc, ok := claudecode.Service(home)
	if !ok {
		t.Fatalf("%s has no service name", home)
	}
	return svc
}

// daemons stands in for `claude daemon stop --any`.
type daemons struct {
	mu    sync.Mutex
	homes []string
	envs  [][]string
	err   error
	then  func(home string)
}

func fakeDaemons(t *testing.T) *daemons {
	t.Helper()
	d := &daemons{}
	claudecode.StopDaemon = func(_ context.Context, home string, env []string) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.homes = append(d.homes, home)
		d.envs = append(d.envs, env)
		if d.then != nil {
			d.then(home)
		}
		return d.err
	}
	t.Cleanup(claudecode.StandIn)
	return d
}

func (d *daemons) stops() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.homes)
}

// stopsClean makes a stopped daemon take its lock and the sessions it hosts
// with it, as Claude Code's does.
func stopsClean(home string) {
	os.Remove(filepath.Join(home, "daemon.lock"))
	entries, _ := os.ReadDir(filepath.Join(home, "sessions"))
	for _, e := range entries {
		p := filepath.Join(home, "sessions", e.Name())
		if strings.Contains(readFile(p), `"bg"`) {
			os.Remove(p)
		}
	}
}

// anthropic stands in for the provider: its token, profile and usage
// endpoints, each counted.
type anthropic struct {
	refreshes, profiles, usages atomic.Int64
	mu                          sync.Mutex
	refresh                     func() (int, any)
	profile                     func(auth string) (int, any)
	usage                       func(auth string) (int, any)
}

func (f *anthropic) set(fn func(f *anthropic)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func fakeAnthropic(t *testing.T) *anthropic {
	t.Helper()
	f := &anthropic{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body := 500, any(nil)
		f.mu.Lock()
		refresh, profile, usage := f.refresh, f.profile, f.usage
		f.mu.Unlock()
		switch r.URL.Path {
		case "/token":
			f.refreshes.Add(1)
			if refresh != nil {
				status, body = refresh()
			}
		case "/profile":
			f.profiles.Add(1)
			if profile != nil {
				status, body = profile(r.Header.Get("Authorization"))
			}
		case "/usage":
			f.usages.Add(1)
			status, body = 200, map[string]any{"five_hour": map[string]any{"utilization": 10}}
			if usage != nil {
				status, body = usage(r.Header.Get("Authorization"))
			}
		}
		w.WriteHeader(status)
		if body != nil {
			json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(srv.Close)
	for _, e := range []struct {
		p   *string
		url string
	}{{&rota.ClaudeEndpoints.Token, srv.URL + "/token"}, {&rota.ClaudeEndpoints.Profile, srv.URL + "/profile"},
		{&rota.ClaudeEndpoints.Usage, srv.URL + "/usage"}} {
		old := *e.p
		*e.p = e.url
		t.Cleanup(func() { *e.p = old })
	}
	return f
}

// refreshesTo makes the token endpoint answer every refresh with this login.
func refreshesTo(access, refresh string) func() (int, any) {
	return func() (int, any) {
		return 200, map[string]any{"access_token": access, "refresh_token": refresh, "expires_in": 3600}
	}
}

// profileOf makes the profile endpoint name this account for the given
// access token and refuse every other.
func profileOf(access, uuid string) func(string) (int, any) {
	return func(auth string) (int, any) {
		if auth != "Bearer "+access {
			return 401, nil
		}
		return 200, map[string]any{"account": map[string]string{"uuid": uuid, "email": uuid + "@x"}}
	}
}

// launchEnv stages a launch the way a handover does and returns the child's
// environment, with the claim already let go.
func launchEnv(t *testing.T, s *Store, a *rota.Account) ([]string, error) {
	t.Helper()
	cmd, release, err := s.prepare(context.Background(), a, true)
	if err != nil {
		return nil, err
	}
	release()
	return rota.Environ(HostEnv(), cmd), nil
}

func hasVar(env []string, name string) bool {
	return slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, name+"=") })
}

func storedRouteHere(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps the environment route")
	}
}

// said collects what a store warns.
func said(s *Store) *[]string {
	var out []string
	var mu sync.Mutex
	s.Warn = func(m string) {
		mu.Lock()
		defer mu.Unlock()
		out = append(out, m)
	}
	return &out
}

// orderBackend is a file backend that notes, the first time a save carries
// want, whether the home already held it: a refreshed login must reach the
// store before it reaches the home, because from the moment the provider
// answers, the one in the home is spent.
type orderBackend struct {
	*FileBackend
	want, homeFile string
	saved          bool // a save has carried want
	homeFirst      bool // and the home held it before that save
	// then runs once, right after that save: what happens between the save
	// and the write into the home.
	then func()
}

func (o *orderBackend) Save(b []byte) error {
	first := !o.saved && o.want != "" && bytes.Contains(b, []byte(o.want))
	if first {
		o.saved = true
		o.homeFirst = strings.Contains(readFile(o.homeFile), o.want)
	}
	err := o.FileBackend.Save(b)
	if first && o.then != nil {
		o.then()
	}
	return err
}

func orderedStore(t *testing.T) (*Store, *orderBackend) {
	t.Helper()
	fb, err := NewFileBackend(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	ob := &orderBackend{FileBackend: fb}
	s, err := NewStore(ob)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ob
}

// watch makes the backend watch for this refresh token reaching the account's
// home.
func (o *orderBackend) watch(s *Store, a *rota.Account, refresh string) {
	o.want, o.homeFile = refresh, filepath.Join(s.Home(a), ".credentials.json")
}

func (o *orderBackend) savedThenWritten(t *testing.T) {
	t.Helper()
	if !o.saved || o.homeFirst || !strings.Contains(readFile(o.homeFile), o.want) {
		t.Fatalf("the refreshed login must be saved first and written into the home after: saved=%v home first=%v home=%q",
			o.saved, o.homeFirst, readFile(o.homeFile))
	}
}
