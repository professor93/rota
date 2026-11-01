package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// A rescue must not depend on what a person asked to see. --short asks no
// provider anything of its own accord, but a home Claude Code blanked is not a
// reading to skip: it is mended here too, and what goes wrong is said here too.
func TestAShortListingStillRescuesAHomeAndSaysWhenItCannot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps no login in a home")
	}
	var refreshes atomic.Int32
	var down atomic.Bool
	down.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "A-new", "refresh_token": "R-new", "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Token = old })

	dir := t.TempDir()
	t.Setenv("ROTA_HOME", dir)
	a := &rota.Account{ID: 1, Provider: "claude", Email: "a@x"}
	a.Token = rota.Token{Access: "A", Refresh: "R", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	a.StagedWritten()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	writeStore(t, dir, fmt.Sprintf(`{"ordered":true,"nextId":2,"accounts":[%s]}`, raw))
	home := filepath.Join(dir, "homes", "claude-1")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(home, ".credentials.json")
	blanked := `{"mcpOAuth":{"srv":{"accessToken":"mcp-secret"}},"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`
	if err := os.WriteFile(creds, []byte(blanked), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := call(t, "list", "--short")
	if code != 0 || refreshes.Load() != 1 || !strings.Contains(stderr, "cleared") {
		t.Fatalf("tried, and said so: %d %d %q", code, refreshes.Load(), stderr)
	}
	down.Store(false)
	if _, stderr, code = call(t, "list", "--short"); code != 0 || !strings.Contains(stderr, "refreshed it") {
		t.Fatalf("%d %q", code, stderr)
	}
	body, err := os.ReadFile(creds)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "R-new") || !strings.Contains(string(body), "mcp-secret") {
		t.Fatalf("the login is back: %s", body)
	}
	if refreshes.Load() != 2 {
		t.Fatalf("once a look: %d", refreshes.Load())
	}
}
