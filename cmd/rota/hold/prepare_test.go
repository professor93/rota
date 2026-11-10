//go:build linux || darwin

package hold

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// holdingProvider spends each refresh token once and holds its answer until
// the test lets it go, by which time it has rotated the token.
type holdingProvider struct {
	mu        sync.Mutex
	presented []string
	arrived   chan struct{}
	release   chan struct{}
}

func newHoldingProvider(t *testing.T) *holdingProvider {
	t.Helper()
	p := &holdingProvider{arrived: make(chan struct{}, 8), release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Refresh string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		p.mu.Lock()
		p.presented = append(p.presented, req.Refresh)
		p.mu.Unlock()
		p.arrived <- struct{}{}
		<-p.release
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "A+", "refresh_token": req.Refresh + "+", "expires_in": 28800})
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Token = old })
	return p
}

// expiredAccount leaves a store whose account 1 is a claude login kept in its
// home, its access token run out, nothing running there.
func expiredAccount(t *testing.T) (dir string, st *store.Store) {
	t.Helper()
	dir = t.TempDir()
	expired := time.Now().Add(-time.Minute).UnixMilli()
	a := &rota.Account{ID: 1, Provider: "claude", UUID: "u1", Email: "u1@x", Order: 1,
		Token: rota.Token{Access: "A0", Refresh: "R0", ExpiresAt: expired, Scopes: []string{"user:inference"}},
		Extra: map[string]string{"refresh_token_expires_at": "1999999999000", "subscription_type": "max", "route": "stored"}}
	a.StagedWritten()
	raw, _ := json.Marshal(map[string]any{"ordered": true, "accounts": []*rota.Account{a}, "nextId": 2})
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	login, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "A0", "refreshToken": "R0",
		"expiresAt": expired, "refreshTokenExpiresAt": 1999999999000, "scopes": []string{"user:inference"}}})
	acct := filepath.Join(dir, "homes", "claude-1")
	if err := os.MkdirAll(acct, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acct, ".credentials.json"), login, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return dir, st
}

// A SIGTERM or a SIGHUP while a holder prepares its account does not lose the
// refresh under way: the provider has replaced the token by then, so the answer
// is waited for, saved and written into the home, and only then does the holder
// give up, starting no CLI.
func TestASignalWhileAHolderPreparesFinishesTheRefreshAndStartsNothing(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			if signal.Ignored(sig) {
				t.Skip("left as this process was started, by design")
			}
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			p := newHoldingProvider(t)
			dir, st := expiredAccount(t)
			var (
				mu   sync.Mutex
				look context.Context
			)
			prepareBegun = func(ctx context.Context) {
				mu.Lock()
				defer mu.Unlock()
				look = ctx
			}
			t.Cleanup(func() { prepareBegun = nil })
			h := &holder{req: holdwire.Request{Kind: holdwire.KindAccount, Account: 1, Cwd: t.TempDir()}}
			planned := make(chan error, 1)
			go func() {
				_, err := h.plan(st)
				planned <- err
			}()
			select {
			case <-p.arrived:
			case <-time.After(10 * time.Second):
				t.Fatal("no refresh reached the provider")
			}
			mu.Lock()
			waiting := look
			mu.Unlock()
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-waiting.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("the signal did not reach the launch")
			}
			close(p.release)
			var err error
			select {
			case err = <-planned:
			case <-time.After(20 * time.Second):
				t.Fatal("the prepare did not end")
			}
			if !store.Left(err) {
				t.Fatalf("no CLI is started, and the error says only that: %v", err)
			}
			if got := st.Find(1).Token.Refresh; got != "R0+" {
				t.Errorf("the account holds %q, not the token the provider answered with", got)
			}
			raw, _ := os.ReadFile(filepath.Join(dir, "accounts.json"))
			if !strings.Contains(string(raw), "R0+") {
				t.Errorf("the answer is not saved: %s", raw)
			}
			raw, _ = os.ReadFile(filepath.Join(dir, "homes", "claude-1", ".credentials.json"))
			if !strings.Contains(string(raw), "R0+") {
				t.Errorf("the answer is not in the home: %s", raw)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if len(p.presented) != 1 {
				t.Errorf("presented %v", p.presented)
			}
		})
	}
}

// A signal that reaches the holder as the prepare ends is not lost on the way
// to the signals going back: the launch is said to be cut short, so no CLI
// starts, and the holder does not go on to be a session it was told to stop
// before it was one.
func TestASignalAsThePrepareEndsCutsTheLaunch(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			if signal.Ignored(sig) {
				t.Skip("left as this process was started, by design")
			}
			// Taken here too, to know when it has reached this process,
			// which on Linux may be after kill returns: a signal sent to a
			// process may be taken by another of its threads than the sender.
			kept := make(chan os.Signal, 1)
			signal.Notify(kept, sig)
			t.Cleanup(func() { signal.Stop(kept) })
			for i := range 50 {
				cut := finishingOnSignal(func(context.Context) {
					if err := syscall.Kill(os.Getpid(), sig); err != nil {
						t.Fatal(err)
					}
					select {
					case <-kept:
					case <-time.After(10 * time.Second):
						t.Fatal("the signal never reached the process")
					}
				})
				if !cut {
					t.Fatalf("try %d: the signal was taken and the launch went on", i)
				}
			}
		})
	}
}

// Once the launch is made the signals are what they were before it: SIGTERM ends
// the process as it would have, and a signal the process was started ignoring is
// ignored throughout. Each is measured in a process of its own.
func TestASignalIsAsItWasOnceThePrepareIsOver(t *testing.T) {
	switch os.Getenv("ROTA_HOLD_SIGNAL_HELPER") {
	case "default":
		finishingOnSignal(func(context.Context) {})
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(10 * time.Second)
		os.Exit(0) // the signal did not end it
	case "ignored":
		signal.Ignore(syscall.SIGHUP)
		finishingOnSignal(func(context.Context) {
			if !signal.Ignored(syscall.SIGHUP) {
				os.Exit(3)
			}
		})
		if !signal.Ignored(syscall.SIGHUP) {
			os.Exit(4)
		}
		os.Exit(0)
	}
	run := func(mode string) *exec.ExitError {
		cmd := exec.Command(os.Args[0], "-test.run=^TestASignalIsAsItWasOnceThePrepareIsOver$")
		cmd.Env = append(os.Environ(), "ROTA_HOLD_SIGNAL_HELPER="+mode)
		err := cmd.Run()
		if err == nil {
			return nil
		}
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		return ee
	}
	ee := run("default")
	if ee == nil {
		t.Fatal("SIGTERM did not end the process once the prepare was over")
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("not ended by SIGTERM, as it would have been: %v", ee)
	}
	if ee := run("ignored"); ee != nil {
		t.Fatalf("a signal started ignored did not stay ignored: %v", ee)
	}
}

// A holder goes by "holder" in the refresh journal, and by "detach" when it was
// started for `rota run --detach`; nothing else the daemon might say gets in.
func TestAHolderNamesItselfInTheJournal(t *testing.T) {
	for _, c := range []struct{ caller, path string }{
		{"", "holder.renew"},
		{holdwire.CallerDetach, "detach.renew"},
		{"something-else", "holder.renew"},
	} {
		t.Run(c.path+"/"+c.caller, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			bin := t.TempDir()
			fakecli.Install(t, bin, "claude", fakecli.Spec{})
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			p := newHoldingProvider(t)
			close(p.release)
			dir, st := expiredAccount(t)
			h := &holder{req: holdwire.Request{Kind: holdwire.KindAccount, Account: 1, Cwd: t.TempDir(), Caller: c.caller}}
			pl, err := h.plan(st)
			if err != nil {
				t.Fatal(err)
			}
			pl.release()
			raw, err := os.ReadFile(filepath.Join(dir, "refresh.log"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"path":"`+c.path+`"`) {
				t.Fatalf("no %s line in:\n%s", c.path, raw)
			}
		})
	}
}
