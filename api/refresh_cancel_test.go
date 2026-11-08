package api

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
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// heldProvider spends each refresh token once and holds every answer until
// the test lets it go. It has rotated the token by the time it holds the
// answer, as a real provider has, so an answer rota stops waiting for is a
// token lost: abandoned is told of each one.
type heldProvider struct {
	mu        sync.Mutex
	spent     map[string]bool
	presented []string
	arrived   chan string
	abandoned chan string
	release   chan struct{}
	once      sync.Once
}

func newHeldProvider(t *testing.T) *heldProvider {
	t.Helper()
	p := &heldProvider{spent: map[string]bool{}, arrived: make(chan string, 16), abandoned: make(chan string, 16),
		release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(srv.Close)
	// Registered after the server, so it runs first and Close never waits on
	// an answer still held.
	t.Cleanup(p.letGo)
	for _, e := range []struct {
		p   *string
		url string
	}{{&rota.ClaudeEndpoints.Token, srv.URL + "/token"}, {&rota.ClaudeEndpoints.Usage, srv.URL + "/usage"},
		{&rota.ClaudeEndpoints.Profile, srv.URL + "/profile"}} {
		old := *e.p
		*e.p = e.url
		t.Cleanup(func() { *e.p = old })
	}
	return p
}

func (p *heldProvider) letGo() { p.once.Do(func() { close(p.release) }) }

func (p *heldProvider) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/token":
		var req struct {
			Refresh string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		p.mu.Lock()
		reused := p.spent[req.Refresh]
		p.spent[req.Refresh] = true
		p.presented = append(p.presented, req.Refresh)
		p.mu.Unlock()
		p.arrived <- req.Refresh
		if reused {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant",
				"error_description": "Refresh token not found or invalid"})
			return
		}
		select {
		case <-p.release:
		case <-r.Context().Done():
			p.abandoned <- req.Refresh
			return
		}
		next := req.Refresh + "+"
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "A" + next, "refresh_token": next, "expires_in": 28800})
	case "/usage":
		_ = json.NewEncoder(w).Encode(map[string]any{"five_hour": map[string]any{"utilization": 10}})
	case "/profile":
		_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]string{"uuid": "u1", "email": "u1@x"}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (p *heldProvider) tokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.presented)
}

// cancelWorld is a server over claude accounts whose logins are kept in their
// homes, each expired, each home holding the account's login as rota wrote
// it there and nothing alive in it; and a provider that holds its answers.
// The one request to watch is followed: gone closes when the server notices
// its caller has gone while it is still being handled, handled when its
// handler returns.
type cancelWorld struct {
	p       *heldProvider
	api     *Server
	srv     *httptest.Server
	dir     string
	gone    chan struct{}
	handled chan struct{}
}

func newCancelWorld(t *testing.T, every time.Duration, watch string, refresh ...string) *cancelWorld {
	t.Helper()
	bin := t.TempDir()
	if err := fakecli.Link(filepath.Join(bin, fakecli.Exe("claude"))); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	w := &cancelWorld{p: newHeldProvider(t), gone: make(chan struct{}), handled: make(chan struct{})}
	// Short, as the harness's is: see newHarness.
	dir, err := os.MkdirTemp("", "rota")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	w.dir = dir
	expired := time.Now().Add(-time.Minute).UnixMilli()
	var accounts []*rota.Account
	for i, rt := range refresh {
		a := &rota.Account{ID: i + 1, Provider: "claude", UUID: "u" + strconv.Itoa(i+1), Email: "u" + strconv.Itoa(i+1) + "@x",
			Token: rota.Token{Access: "A" + rt, Refresh: rt, ExpiresAt: expired, Scopes: []string{"user:inference"}},
			Extra: map[string]string{"refresh_token_expires_at": "1999999999000", "subscription_type": "max", "route": "stored"}}
		a.StagedWritten()
		accounts = append(accounts, a)
		login, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": a.Token.Access,
			"refreshToken": rt, "expiresAt": expired, "refreshTokenExpiresAt": 1999999999000, "scopes": []string{"user:inference"}}})
		home := filepath.Join(dir, "homes", "claude-"+strconv.Itoa(a.ID))
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".credentials.json"), login, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]any{"accounts": accounts, "nextId": len(accounts) + 1})
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Dir: dir, Token: "secret", Roots: []string{t.TempDir()}, RefreshEvery: every,
		Routes: &Routes{API: true, WebSocket: true}})
	if err != nil {
		t.Fatal(err)
	}
	w.api = s
	handler := s.Handler()
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != watch {
			handler.ServeHTTP(rw, r)
			return
		}
		defer close(w.handled)
		stop := context.AfterFunc(r.Context(), func() { close(w.gone) })
		defer stop()
		handler.ServeHTTP(rw, r)
	}))
	t.Cleanup(s.Stop)
	t.Cleanup(w.srv.Close)
	// Last, so it runs first: a test that failed with an answer still held
	// lets it go before anything waits for the request holding it.
	t.Cleanup(w.p.letGo)
	return w
}

// ask sends a request that ctx can take back, and closes what it returns when
// the client has its answer or has given up.
func (w *cancelWorld) ask(ctx context.Context, method, path string, body any) <-chan struct{} {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequestWithContext(ctx, method, w.srv.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	return done
}

func wait(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatalf("waited in vain for %s", what)
	}
}

// arrival is the next refresh token presented.
func (w *cancelWorld) arrival(t *testing.T) string {
	t.Helper()
	select {
	case rt := <-w.p.arrived:
		return rt
	case <-time.After(10 * time.Second):
		t.Fatal("no refresh reached the provider")
		return ""
	}
}

// notAbandoned gives a refresh cancelled with its caller the time to show
// itself, and fails if one does.
func (w *cancelWorld) notAbandoned(t *testing.T) {
	t.Helper()
	select {
	case rt := <-w.p.abandoned:
		t.Fatalf("rota gave up on the provider's answer to %s, which has rotated it: the new token is lost", rt)
	case <-time.After(300 * time.Millisecond):
	}
}

// holds checks that the store on disk and the account's home both hold rt.
func (w *cancelWorld) holds(t *testing.T, id int, rt string) {
	t.Helper()
	st, err := store.Open(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	a := st.Find(id)
	st.Close()
	if a == nil || a.Token.Refresh != rt {
		t.Errorf("account %d: the store holds %+v, not %s", id, a, rt)
	}
	if runtime.GOOS == "windows" {
		return // no login is kept in a home there
	}
	raw, _ := os.ReadFile(filepath.Join(w.dir, "homes", "claude-"+strconv.Itoa(id), ".credentials.json"))
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

// presented checks that the provider was shown exactly these tokens, each once.
func (w *cancelWorld) presented(t *testing.T, want ...string) {
	t.Helper()
	if got := w.p.tokens(); !slices.Equal(got, want) {
		t.Errorf("presented %v, want %v", got, want)
	}
}

// A refresh is not cancelled by the request that started it. A client asks
// for a forced refresh and hangs up while the provider answers. The provider
// has rotated the login by then, so a refresh cancelled with the request would
// lose the only copy of the new token and leave the account on the spent one,
// for the next look to present again. rota waits for the answer instead, saves
// it and writes it into the home.
func TestARefreshOutlivesTheRequestThatStartedIt(t *testing.T) {
	w := newCancelWorld(t, -1, "/v1/accounts", "R0")
	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	asked := w.ask(ctx, "GET", "/v1/accounts?refresh=1", nil)
	if rt := w.arrival(t); rt != "R0" {
		t.Fatalf("presented %s", rt)
	}
	hangUp()
	wait(t, asked, "the client to give up")
	wait(t, w.gone, "the server to notice the client had gone")
	w.notAbandoned(t)
	w.p.letGo()
	wait(t, w.handled, "the request's handler to return")
	w.holds(t, 1, "R0+")
	w.presented(t, "R0")
}

// The same for a launch: a run's caller hangs up while the refresh before it
// is being answered. The run does not start, and the refresh is kept.
func TestALaunchsRefreshOutlivesTheRequestThatStartedIt(t *testing.T) {
	w := newCancelWorld(t, -1, "/v1/accounts/1/run", "R0")
	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	asked := w.ask(ctx, "POST", "/v1/accounts/1/run", map[string]any{"prompt": "p"})
	if rt := w.arrival(t); rt != "R0" {
		t.Fatalf("presented %s", rt)
	}
	hangUp()
	wait(t, asked, "the client to give up")
	wait(t, w.gone, "the server to notice the client had gone")
	w.notAbandoned(t)
	w.p.letGo()
	wait(t, w.handled, "the run's handler to return")
	w.holds(t, 1, "R0+")
	w.presented(t, "R0")
}

// A server that is stopping waits for the refresh its sweep has under way,
// lets it be saved and written into the home, and starts no other: the second
// account, which the sweep had not come to, is left as it was for the next
// one.
func TestAServerStoppingWaitsForTheRefreshUnderWayAndStartsNoOther(t *testing.T) {
	w := newCancelWorld(t, 20*time.Millisecond, "", "Ra", "Rb")
	first := w.arrival(t)
	stopped := make(chan struct{})
	go func() {
		w.api.Stop()
		close(stopped)
	}()
	wait(t, w.api.ctx.Done(), "the server to stop")
	w.notAbandoned(t)
	select {
	case <-stopped:
		t.Fatal("Stop returned while a refresh of its sweep was still waiting for its answer")
	default:
	}
	w.p.letGo()
	wait(t, stopped, "Stop to return once the refresh was answered")
	firstID, other, otherID := 1, "Rb", 2
	if first == "Rb" {
		firstID, other, otherID = 2, "Ra", 1
	}
	w.holds(t, firstID, first+"+")
	w.holds(t, otherID, other)
	w.presented(t, first)
}

// The provider's answer to a refresh a WebSocket start made is held while
// rota serve stops: http.Server.Shutdown does not wait for a connection taken
// over, so Stop is what must. It does not return until the answer is in, and
// the store and the home hold the new token after it.
func TestAWebSocketStartsRefreshIsWaitedForWhenTheServerStops(t *testing.T) {
	w := newCancelWorld(t, -1, "", "R0")
	c := dialRun(t, &harness{t: t, srv: w.srv, token: "secret", dir: w.dir, api: w.api}, "/v1/ws")
	c.say(map[string]any{"type": "start", "prompt": "p"})
	if rt := w.arrival(t); rt != "R0" {
		t.Fatalf("presented %s", rt)
	}
	// rota serve's exit: the http server's shutdown, then Stop.
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.srv.Config.Shutdown(shut); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		w.api.Stop()
		close(stopped)
	}()
	w.notAbandoned(t)
	select {
	case <-stopped:
		t.Fatal("Stop returned while the refresh a WebSocket start made was still waiting for its answer")
	default:
	}
	w.p.letGo()
	wait(t, stopped, "Stop to return once the refresh was answered")
	w.holds(t, 1, "R0+")
	w.presented(t, "R0")
}

// A WebSocket start that arrives once Stop was called refreshes nothing: the
// request's own context lives as long as the socket does, so the server's
// stopping is what says no.
func TestAWebSocketStartAfterStopRefreshesNothing(t *testing.T) {
	w := newCancelWorld(t, -1, "", "R0")
	w.api.Stop()
	c := dialRun(t, &harness{t: t, srv: w.srv, token: "secret", dir: w.dir, api: w.api}, "/v1/ws")
	c.say(map[string]any{"type": "start", "prompt": "p"})
	select {
	case rt := <-w.p.arrived:
		t.Fatalf("a start after Stop presented %s", rt)
	case <-time.After(500 * time.Millisecond):
	}
	w.p.letGo()
	w.holds(t, 1, "R0")
	w.presented(t)
}

// rota serve stopping first stops the sweeps, then waits for the requests in
// flight, which can take as long as a run. No sweep starts during that wait:
// one would be a refresh for a second interrupt to cut short. The runs go on,
// since they are what the wait is for.
func TestNoSweepStartsWhileShutdownWaitsForTheRunsInFlight(t *testing.T) {
	// The first sweep is an hour off, so none can have come before the sweeps
	// are stopped; the keeper having returned is what says none ever will.
	w := newCancelWorld(t, time.Hour, "", "Ra")
	// A run in flight, which the http server's shutdown waits for.
	block, entered := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(block) })
	long := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-block
	}))
	t.Cleanup(long.Close)
	t.Cleanup(unblock)
	go func() {
		if resp, err := http.Get(long.URL); err == nil {
			resp.Body.Close()
		}
	}()
	wait(t, entered, "the run to start")
	w.api.StopRefreshing()
	wait(t, w.api.keeperDone, "the sweeps to end as the shutdown begins")
	shut := make(chan error, 1)
	go func() { shut <- long.Config.Shutdown(context.Background()) }()
	if w.api.ctx.Err() != nil {
		t.Fatal("stopping the sweeps stopped the runs too")
	}
	unblock()
	select {
	case err := <-shut:
		if err != nil {
			t.Fatalf("the shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown did not end once the run had")
	}
	if got := w.p.tokens(); len(got) != 0 {
		t.Fatalf("a sweep presented %v", got)
	}
	w.holds(t, 1, "Ra")
}

// Stop waits for a sweep's refresh as long as that refresh may take, which
// is longer than the five seconds it used to give up after: the provider
// answers six seconds into the stop, and the answer is kept.
func TestStopWaitsForARefreshLongerThanFiveSeconds(t *testing.T) {
	w := newCancelWorld(t, 20*time.Millisecond, "", "Ra")
	if rt := w.arrival(t); rt != "Ra" {
		t.Fatalf("presented %s", rt)
	}
	stopped := make(chan struct{})
	go func() {
		w.api.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the refresh was still waiting for its answer")
	case <-time.After(6 * time.Second):
	}
	w.p.letGo()
	wait(t, stopped, "Stop to return once the refresh was answered")
	w.holds(t, 1, "Ra+")
	w.presented(t, "Ra")
}
