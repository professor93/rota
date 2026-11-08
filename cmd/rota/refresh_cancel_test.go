package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// heldProvider spends each refresh token once and holds every answer until
// the test lets it go; it has rotated the token by then, as a real provider
// has. arrived is told of each token presented.
type heldProvider struct {
	mu        sync.Mutex
	presented []string
	arrived   chan string
	release   chan struct{}
	once      sync.Once
}

func newHeldProvider(t *testing.T) *heldProvider {
	t.Helper()
	p := &heldProvider{arrived: make(chan string, 16), release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			var req struct {
				Refresh string `json:"refresh_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			p.mu.Lock()
			p.presented = append(p.presented, req.Refresh)
			p.mu.Unlock()
			p.arrived <- req.Refresh
			select {
			case <-p.release:
			case <-r.Context().Done():
				return
			}
			next := req.Refresh + "+"
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "A" + next, "refresh_token": next, "expires_in": 28800})
		case "/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{"five_hour": map[string]any{"utilization": 10}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(p.letGo) // before Close, which would wait on a held answer
	for _, e := range []struct {
		p   *string
		url string
	}{{&rota.ClaudeEndpoints.Token, srv.URL + "/token"}, {&rota.ClaudeEndpoints.Usage, srv.URL + "/usage"}} {
		old := *e.p
		*e.p = e.url
		t.Cleanup(func() { *e.p = old })
	}
	return p
}

func (p *heldProvider) letGo() { p.once.Do(func() { close(p.release) }) }

func (p *heldProvider) tokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.presented)
}

// arrival is the next token presented.
func (p *heldProvider) arrival(t *testing.T) string {
	t.Helper()
	select {
	case rt := <-p.arrived:
		return rt
	case <-time.After(10 * time.Second):
		t.Fatal("no refresh reached the provider")
		return ""
	}
}

// expiredClaude is a claude account whose login is kept in its home, expired,
// with the home holding it as rota wrote it there and nothing alive in it.
func expiredClaude(t *testing.T, home string, id int, rt string) *rota.Account {
	t.Helper()
	return claudeExpiring(t, home, id, rt, time.Now().Add(-time.Minute).UnixMilli())
}

// claudeExpiring is expiredClaude with an access token that expires at expired.
func claudeExpiring(t *testing.T, home string, id int, rt string, expired int64) *rota.Account {
	t.Helper()
	a := &rota.Account{ID: id, Provider: "claude", UUID: "u" + strconv.Itoa(id), Email: "u" + strconv.Itoa(id) + "@x",
		Order: id, Token: rota.Token{Access: "A" + rt, Refresh: rt, ExpiresAt: expired, Scopes: []string{"user:inference"}},
		Extra: map[string]string{"refresh_token_expires_at": "1999999999000", "subscription_type": "max", "route": "stored"}}
	a.StagedWritten()
	login, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": a.Token.Access,
		"refreshToken": rt, "expiresAt": expired, "refreshTokenExpiresAt": 1999999999000, "scopes": []string{"user:inference"}}})
	dir := filepath.Join(home, "homes", "claude-"+strconv.Itoa(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), login, 0o600); err != nil {
		t.Fatal(err)
	}
	return a
}

func writeAccounts(t *testing.T, home string, accounts ...*rota.Account) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"ordered": true, "accounts": accounts, "nextId": len(accounts) + 1})
	writeStore(t, home, string(raw))
}

// holdsLogin checks that the store on disk and, where a login is kept in a
// home, the account's home hold rt.
func holdsLogin(t *testing.T, home string, id int, rt string) {
	t.Helper()
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	a := st.Find(id)
	st.Close()
	if a == nil || a.Token.Refresh != rt {
		t.Errorf("account %d: the store holds %+v, not %s", id, a, rt)
	}
	if !storedLoginHere() {
		return
	}
	raw, _ := os.ReadFile(filepath.Join(home, "homes", "claude-"+strconv.Itoa(id), ".credentials.json"))
	var doc struct {
		Login struct {
			Refresh string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	_ = json.Unmarshal(raw, &doc)
	if doc.Login.Refresh != rt {
		t.Errorf("account %d: its home holds %q, not %s", id, doc.Login.Refresh, rt)
	}
}

// storedLoginHere is whether a claude login is kept in its home on this
// system: everywhere but Windows.
func storedLoginHere() bool { return runtime.GOOS != "windows" }

// lockedBuffer is a buffer several goroutines may write at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// rota serve, asked to stop, stops its sweeps at once, while it still waits
// for the runs in flight: a sweep started in that wait would be one more
// refresh for a second interrupt to cut short. Here the sweeps run every 100 ms
// over an account with nothing to refresh, a run is held in flight, and the
// server is asked to stop. Once its listener has closed, which comes after the
// sweeps are told to stop, the account's token is made to have run out on disk:
// a sweep still running would present it at once, and none does for as long as
// the server waits for the run.
func TestServeStartsNoSweepOnceAskedToStop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	t.Setenv("ROTA_TOKEN", "the-token")
	p := newHeldProvider(t)
	runnable := &rota.Account{ID: 2, Provider: "claude", Email: "run@x", Order: 2, Token: rota.Token{Access: "t"}}
	writeAccounts(t, home, claudeExpiring(t, home, 1, "Ra", time.Now().Add(time.Hour).UnixMilli()), runnable)
	bin, up := t.TempDir(), filepath.Join(t.TempDir(), "up")
	fakecli.Install(t, bin, "claude", fakecli.Spec{Stdout: []string{`[{"type":"result","result":"ANSWERED"}]`}, Sleep: "3s",
		Ready: up})
	t.Setenv("PATH", bin)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	was := serveUntil
	serveUntil = func() (context.Context, context.CancelFunc) { return ctx, stop }
	defer func() { serveUntil = was }()
	// Written by the server's goroutines at once, as stderr may be.
	var out, errb lockedBuffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"serve", addr, "--refresh-every=100ms"}, &out, &errb) }()
	base := "http://" + addr
	ask := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer the-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if code, _ := ask("GET", "/v1/health", ""); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never answered: %q", errb.String())
		}
	}
	// A run in flight, which the shutdown waits for.
	go ask("POST", "/v1/accounts/2/run", `{"prompt":"p"}`)
	fakecli.WaitReady(t, up)
	stop() // what SIGTERM does
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if code, _ := ask("GET", "/v1/health", ""); code == 0 {
			break // the listener is closed: Shutdown has begun, after StopRefreshing
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve kept answering after it was asked to stop: %q", errb.String())
		}
	}
	writeAccounts(t, home, expiredClaude(t, home, 1, "Ra"), runnable)
	select {
	case rt := <-p.arrived:
		t.Fatalf("a sweep presented %s while the server waited for a run to stop", rt)
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
	if got := p.tokens(); len(got) != 0 {
		t.Fatalf("a sweep presented %v while the server waited for a run to stop", got)
	}
	holdsLogin(t, home, 1, "Ra")
}
