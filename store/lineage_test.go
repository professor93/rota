package store

// A world for refresh tests in which a refresh token can be spent only once.
//
// A provider with real refresh-token rotation and reuse detection: each
// refresh spends the token presented and issues the next; presenting a spent
// token revokes the whole family, so every later presentation — the newest
// token included — is refused with "Refresh token not found or invalid". A
// fake Claude Code keeps the login in the home the way 2.1.x does on macOS
// (keychain item first, file only when there is no item), takes
// .oauth_refresh.lock around each refresh, writes each rotation into the
// keychain item and leaves any file beside it alone, and on a refusal blanks
// the login and writes auth_required. rota then runs its ordinary looks —
// listing, short listing / watcher, maintenance, launch — and the provider
// records, for every token rota presents, whether it was already spent, what
// the home held at that instant, and whether Claude Code's refresh lock was
// held.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

const lineageUntil = "1999999999000" // refreshTokenExpiresAt: one lineage

type lineagePresented struct {
	who, rt, outcome string
	wasSpent         bool   // the token had already been spent when presented
	homeRT           string // the refresh token the home held at that instant
	lockHeld         bool   // .oauth_refresh.lock existed at that instant
	daemon           bool   // daemon.lock existed at that instant
}

type lineageProvider struct {
	mu      sync.Mutex
	n       int
	current string
	spentBy map[string]string
	revoked string
	issued  map[string]bool // access tokens, which outlive a revoked family here
	log     []lineagePresented
	homeRT  func() string
	home    string
	onRota  func(rt string) // runs when rota presents, before the answer
	url     string
}

func newLineageProvider(t *testing.T) *lineageProvider {
	t.Helper()
	p := &lineageProvider{current: "R0", spentBy: map[string]string{}, issued: map[string]bool{"A0": true}}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	for _, e := range []struct {
		p   *string
		url string
	}{{&rota.ClaudeEndpoints.Token, srv.URL + "/token"}, {&rota.ClaudeEndpoints.Profile, srv.URL + "/profile"},
		{&rota.ClaudeEndpoints.Usage, srv.URL + "/usage"}} {
		old := *e.p
		*e.p = e.url
		t.Cleanup(func() { *e.p = old })
	}
	return p
}

func (p *lineageProvider) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/token":
		var req struct {
			Refresh string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		who := r.Header.Get("User-Agent")
		if strings.HasPrefix(who, "rota") {
			who = "rota"
			if p.onRota != nil {
				p.onRota(req.Refresh)
			}
		}
		status, body := p.present(who, req.Refresh)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	case "/profile", "/usage":
		p.mu.Lock()
		ok := p.issued[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		p.mu.Unlock()
		if !ok {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/profile" {
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]string{"uuid": "u1", "email": "u1@x"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"five_hour": map[string]any{"utilization": 10}})
	default:
		w.WriteHeader(404)
	}
}

func (p *lineageProvider) present(who, rt string) (int, any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := lineagePresented{who: who, rt: rt, wasSpent: p.spentBy[rt] != ""}
	if p.homeRT != nil {
		e.homeRT = p.homeRT()
	}
	if p.home != "" {
		_, err := os.Stat(filepath.Join(p.home, ".oauth_refresh.lock"))
		e.lockHeld = err == nil
		_, err = os.Stat(filepath.Join(p.home, "daemon.lock"))
		e.daemon = err == nil
	}
	refused := map[string]string{"error": "invalid_grant", "error_description": "Refresh token not found or invalid"}
	var status int
	var body any
	switch {
	case p.revoked != "":
		e.outcome, status, body = "refused (family revoked: "+p.revoked+")", 400, refused
	case rt == p.current:
		p.n++
		next, access := fmt.Sprintf("R%d", p.n), fmt.Sprintf("A%d", p.n)
		p.spentBy[rt], p.current, p.issued[access] = who, next, true
		e.outcome, status = "rotated to "+next, 200
		body = map[string]any{"access_token": access, "refresh_token": next, "expires_in": 28800}
	case p.spentBy[rt] != "":
		p.revoked = fmt.Sprintf("%s presented %s, spent by %s", who, rt, p.spentBy[rt])
		e.outcome, status, body = "REUSE -> family revoked", 400, refused
	default:
		e.outcome, status, body = "unknown token", 400, refused
	}
	p.log = append(p.log, e)
	return status, body
}

func (p *lineageProvider) spent(rt string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spentBy[rt] != ""
}

func (p *lineageProvider) revoke(why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoked = why
}

func (p *lineageProvider) entries() []lineagePresented {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]lineagePresented(nil), p.log...)
}

func (p *lineageProvider) dump(t *testing.T) {
	t.Helper()
	for i, e := range p.entries() {
		t.Logf("  #%d %-28s presented %-3s spent=%-5v home=%-3s lock=%-5v daemon=%-5v -> %s",
			i, e.who, e.rt, e.wasSpent, e.homeRT, e.lockHeld, e.daemon, e.outcome)
	}
}

// rotaSpent is every token rota presented that had already been spent.
func (p *lineageProvider) rotaSpent() []lineagePresented {
	var out []lineagePresented
	for _, e := range p.entries() {
		if e.who == "rota" && e.wasSpent {
			out = append(out, e)
		}
	}
	return out
}

// fakeClaudeCode is Claude Code in one home: its daemon's liveness record and
// its refresh, as it keeps the login on macOS.
type fakeClaudeCode struct {
	t    *testing.T
	home string
	svc  string
	k    *keychain // nil: the file is the store (Linux, or no item)
	url  string
}

func (c *fakeClaudeCode) start() { alive(c.t, c.home, "daemon.lock", os.Getpid()) }
func (c *fakeClaudeCode) stop()  { _ = os.Remove(filepath.Join(c.home, "daemon.lock")) }

// store is the credential store as Claude Code reads it: the item, else the file.
func (c *fakeClaudeCode) store() string {
	if c.k != nil {
		if v, ok := c.k.item(c.svc); ok {
			return v
		}
	}
	return readFile(filepath.Join(c.home, ".credentials.json"))
}

func (c *fakeClaudeCode) write(doc string) {
	if c.k != nil {
		c.k.mu.Lock()
		c.k.items[c.svc] = doc
		c.k.mu.Unlock()
		return
	}
	writeFile(c.t, filepath.Join(c.home, ".credentials.json"), doc)
}

func refreshOf(doc string) string {
	l, _ := storedLogin([]byte(doc))
	return l.Refresh
}

// refresh is one refresh of Claude Code's own, as `who`.
func (c *fakeClaudeCode) refresh(who string) string {
	lock := filepath.Join(c.home, ".oauth_refresh.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		return "refresh lock held by somebody else: waited, read the store again"
	}
	defer os.Remove(lock)
	rt := refreshOf(c.store())
	if rt == "" {
		return "no token found"
	}
	buf, _ := json.Marshal(map[string]string{"grant_type": "refresh_token", "refresh_token": rt, "client_id": "x"})
	req, _ := http.NewRequest("POST", c.url+"/token", bytes.NewReader(buf))
	req.Header.Set("User-Agent", who)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var r struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if resp.StatusCode == 200 {
		c.write(storeLogin(r.Access, r.Refresh, rota.Now().Add(8*time.Hour).UnixMilli(), lineageUntil))
		return "rotated to " + r.Refresh
	}
	c.write(clearedLogin)
	writeFile(c.t, filepath.Join(c.home, "daemon-auth-status.json"),
		fmt.Sprintf(`{"status":"auth_required","since":%d}`, rota.NowMS()))
	return "refused: login blanked, auth_required"
}

// lineageClock moves rota's clock by hand.
type lineageClock struct{ at time.Time }

func useLineageClock(t *testing.T) *lineageClock {
	t.Helper()
	c := &lineageClock{at: time.Now()}
	old := rota.Now
	rota.Now = func() time.Time { return c.at }
	t.Cleanup(func() { rota.Now = old })
	return c
}

func (c *lineageClock) pass(d time.Duration) { c.at = c.at.Add(d) }

// lineageWorld is one claude account in sync with its home on R0, with a fake
// Claude Code in that home, on the keychain (macOS) or on the file.
type lineageWorld struct {
	s     *Store
	a     *rota.Account
	p     *lineageProvider
	cc    *fakeClaudeCode
	clock *lineageClock
	k     *keychain
}

func newLineageWorld(t *testing.T, keychainMode bool) *lineageWorld {
	t.Helper()
	storedRouteHere(t)
	claudeWorld(t)
	w := &lineageWorld{clock: useLineageClock(t), p: newLineageProvider(t)}
	if keychainMode {
		w.k = fakeKeychain(t)
	}
	w.s = openTemp(t)
	w.a = livingClaude(w.s, "u1")
	w.a.Token = rota.Token{Access: "A0", Refresh: "R0", ExpiresAt: rota.Now().Add(8 * time.Hour).UnixMilli(),
		Scopes: []string{"user:inference"}}
	w.a.Extra["refresh_token_expires_at"] = lineageUntil
	home := w.s.Home(w.a)
	w.cc = &fakeClaudeCode{t: t, home: home, url: w.p.url}
	if keychainMode {
		w.cc.k, w.cc.svc = w.k, service(t, home)
	}
	// rota wrote the login into the home as a file, as its seed does: on macOS
	// the item is gone and the file is the login until Claude Code's first
	// refresh puts the next one in the keychain.
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A0", "R0", w.a.Token.ExpiresAt, lineageUntil))
	w.a.StagedWritten()
	w.a.Extra[routeKey] = routeStored
	w.p.home = home
	w.p.homeRT = func() string { return refreshOf(w.cc.store()) }
	if err := w.s.Save(); err != nil {
		t.Fatal(err)
	}
	return w
}

type lineageLook struct {
	what string
	do   func(t *testing.T, w *lineageWorld)
}

func lineageLooks() []lineageLook {
	return []lineageLook{
		{"rota list", func(t *testing.T, w *lineageWorld) {
			for _, e := range w.s.Refresh(context.Background(), false, w.a) {
				t.Logf("    list: %v", e)
			}
		}},
		{"rota list --short / watcher", func(t *testing.T, w *lineageWorld) {
			for _, e := range w.s.Rescue(context.Background(), w.a) {
				t.Logf("    rescue: %v", e)
			}
		}},
		{"rota serve maintenance", func(t *testing.T, w *lineageWorld) {
			for _, e := range w.s.Maintain(context.Background()) {
				t.Logf("    maintain: %v", e)
			}
		}},
		{"rota run (launch)", func(t *testing.T, w *lineageWorld) {
			if _, err := launchEnv(t, w.s, w.a); err != nil {
				t.Logf("    launch: %v", err)
			}
		}},
	}
}

func lineageModes() []struct {
	name     string
	keychain bool
} {
	return []struct {
		name     string
		keychain bool
	}{{"keychain", true}, {"file", false}}
}

// S0, the control: Claude Code's daemon alive in the home and rotating every
// eight hours, rota looking every way in between. rota must present nothing
// while the daemon runs and follow every rotation; once the home is quiet and
// rota's token has run out, its own refresh presents the newest token.
func TestRotaPresentsNothingWhileClaudeCodeRotatesInTheHome(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			w.cc.start()
			for i := 0; i < 3; i++ {
				w.clock.pass(7*time.Hour + 56*time.Minute)
				t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
				for _, look := range lineageLooks() {
					look.do(t, w)
				}
				if w.a.Token.Refresh != w.p.current {
					t.Errorf("rota did not follow the rotation: holds %s, home %s", w.a.Token.Refresh, w.p.current)
				}
			}
			w.cc.stop()
			w.clock.pass(9 * time.Hour)
			lineageLooks()[0].do(t, w)
			w.p.dump(t)
			if bad := w.p.rotaSpent(); len(bad) != 0 {
				t.Errorf("rota presented a spent token: %+v", bad)
			}
			if w.a.Dead {
				t.Errorf("dead: %s", w.a.WhyDead())
			}
		})
	}
}
