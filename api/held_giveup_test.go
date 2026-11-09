//go:build linux || darwin

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// heldPID waits for a fake holder to write its pid, which it does once its
// signals are as it was told to set them.
func heldPID(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the holder never started")
		}
	}
}

// giveUpOn has a server give up on its holders when the channel returned closes.
func giveUpOn(t *testing.T) chan time.Time {
	t.Helper()
	gaveUp, old := make(chan time.Time), holdDeadline
	holdDeadline = func() <-chan time.Time { return gaveUp }
	t.Cleanup(func() { holdDeadline = old })
	return gaveUp
}

// starting asks for a held session on a holder that never answers, and gives up
// on it once it has started and set its signals. It returns the holder's pid.
func starting(t *testing.T, env ...string) int {
	t.Helper()
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	gaveUp := giveUpOn(t)
	p := withHeld(t, append(env, "FAKE_HOLDER_SLOW=1h", "FAKE_HOLDER_PIDFILE="+pidfile))
	pids := make(chan int, 1)
	go func() {
		pid := heldPID(t, pidfile)
		pids <- pid
		close(gaveUp)
	}()
	if resp, raw := p.do("POST", "/v1/terminals", map[string]any{"account": 1, "cols": 80, "rows": 24}); resp.StatusCode == 201 {
		t.Fatalf("a holder that never answered was a session: %s", raw)
	}
	pid := <-pids
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// A daemon that gives up on a holder tells it to stop and does not kill it: it
// may be waiting for a refresh's answer, and a SIGKILL would lose the new token.
func TestAHolderGivenUpOnIsToldToStopAndNotKilled(t *testing.T) {
	old := holdKillAfter
	holdKillAfter = func() time.Duration { return time.Hour }
	t.Cleanup(func() { holdKillAfter = old })
	noted := filepath.Join(t.TempDir(), "term")
	pid := starting(t, "FAKE_HOLDER_NOTE_TERM="+noted)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(noted); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the holder was never told to stop")
		}
	}
	if !alive(pid) {
		t.Fatal("the holder was killed")
	}
}

// One that goes on regardless is killed, once it has had all a refresh may take.
func TestAHolderThatWillNotStopIsKilledInTheEnd(t *testing.T) {
	old := holdKillAfter
	holdKillAfter = func() time.Duration { return 20 * time.Millisecond }
	t.Cleanup(func() { holdKillAfter = old })
	pid := starting(t, "FAKE_HOLDER_IGNORE_TERM=1")
	for deadline := time.Now().Add(10 * time.Second); !gone(pid); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the holder was left running")
		}
	}
}

// A daemon that gives up on a holder must not lose a refresh it is waiting on.
// The holder presents R1, the provider rotates it and keeps its answer, and the
// daemon gives up and answers the request with its error. Then the provider
// answers: the holder takes it, saves it and writes it into the home, so the
// next look holds R1+ and R1 was presented once.
func TestAHolderGivenUpOnMidRefreshKeepsTheAnswer(t *testing.T) {
	gaveUp := giveUpOn(t)
	release := make(chan struct{})
	var (
		mu        sync.Mutex
		presented []string
		once      sync.Once
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Refresh string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		spent := slices.Contains(presented, req.Refresh)
		presented = append(presented, req.Refresh)
		mu.Unlock()
		if spent {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "reused"})
			return
		}
		once.Do(func() { close(gaveUp) }) // the deadline comes while the answer is held
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "A+", "refresh_token": req.Refresh + "+", "expires_in": 28800})
	}))
	t.Cleanup(srv.Close)
	letGo := sync.OnceFunc(func() { close(release) })
	t.Cleanup(letGo) // before Close, which would wait on a held answer
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Token = old })

	p := withHeld(t, []string{"FAKE_HOLDER_PREPARE=" + srv.URL})
	heldLogin(t, p, time.Now().Add(-time.Minute))
	if resp, raw := p.do("POST", "/v1/terminals", map[string]any{"account": 1, "cols": 80, "rows": 24}); resp.StatusCode == 201 {
		t.Fatalf("the session started, so nothing was given up on: %s", raw)
	}
	letGo()
	home := filepath.Join(p.dir, "homes", "claude-1")
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		saved, _ := os.ReadFile(filepath.Join(p.dir, "accounts.json"))
		written, _ := os.ReadFile(filepath.Join(home, ".credentials.json"))
		_, lockErr := os.Stat(filepath.Join(home, ".oauth_refresh.lock"))
		if strings.Contains(string(saved), `"R1+"`) && strings.Contains(string(written), `"R1+"`) && lockErr != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the answer was lost: store %s\nhome %s\nlock there=%v", saved, written, lockErr == nil)
		}
	}
	st, err := store.Open(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := st.Find(1)
	st.Refresh(context.Background(), false, a)
	mu.Lock()
	defer mu.Unlock()
	if a.Dead || a.Token.Refresh != "R1+" || !slices.Equal(presented, []string{"R1"}) {
		t.Fatalf("holds %q dead=%v %q, presented %v", a.Token.Refresh, a.Dead, a.WhyDead(), presented)
	}
}
