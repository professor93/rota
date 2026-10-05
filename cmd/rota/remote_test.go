package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// seedLiving writes a store with one claude account whose login Claude Code
// can keep, and puts a stand-in claude on PATH. It returns rota's home and
// the account's own.
func seedLiving(t *testing.T, extra string) (rotaHome, accountHome string) {
	t.Helper()
	rotaHome = t.TempDir()
	t.Setenv("ROTA_HOME", rotaHome)
	until := time.Now().Add(time.Hour).UnixMilli()
	doc := fmt.Sprintf(`{"accounts":[{"id":1,"provider":"claude","email":"a@b.c","uuid":"u1","order":1,`+
		`"token":{"accessToken":"SHORT","refreshToken":"r","expiresAt":%d}%s}],"nextId":2,"ordered":true}`, until, extra)
	if err := os.WriteFile(filepath.Join(rotaHome, "accounts.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Spec{})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return rotaHome, filepath.Join(rotaHome, "homes", "claude-1")
}

func liveIn(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := fmt.Sprintf(`{"pid":%d,"kind":"interactive"}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(home, "sessions", "1.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
}

func storedAccount(t *testing.T, rotaHome string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(rotaHome, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Accounts[0]
}

// `rota run --remote-control` turns the setting on and keeps it, and hands
// Claude Code its own flag — with the name when one is given, and last, so
// nothing before it is read as one.
func TestRunWithRemoteControlSavesTheSettingAndPassesClaudeCodesFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps the environment route, which Remote Control refuses")
	}
	rotaHome, home := seedLiving(t, "")
	handedTo := handover(t)
	_, errOut, code := call(t, "run", "1", "--remote-control")
	if code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if got := *handedTo; len(got) < 2 || got[len(got)-1] != "--remote-control" {
		t.Fatalf("Claude Code's own flag, last: %v", got)
	}
	if storedAccount(t, rotaHome)["remoteControl"] != true || !strings.Contains(errOut, "remote control is on") {
		t.Fatalf("saved, and said: %q", errOut)
	}
	if fi, err := os.Lstat(filepath.Join(home, ".claude.json")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the account's own configuration is in place")
	}
	_, errOut, code = call(t, "run", "1", "--remote-control=laptop", "--", "--resume")
	if code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if got := *handedTo; !slices.Equal(got[len(got)-3:], []string{"--resume", "--remote-control", "laptop"}) {
		t.Fatalf("the name follows the flag, after the CLI's own arguments: %v", got)
	}
}

// A session whose Remote Control could not work is not started: a home
// still running Claude Code from before the setting, a question rather than
// a session, an account of another provider.
func TestRunWithRemoteControlRefusesWhatCouldNotWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps the environment route, which Remote Control refuses")
	}
	rotaHome, home := seedLiving(t, "")
	handedTo := handover(t)
	liveIn(t, home)
	_, errOut, code := call(t, "run", "1", "--remote-control")
	if code == 0 || !strings.Contains(errOut, "takes effect when") || len(*handedTo) != 0 {
		t.Fatalf("refused, saying why: %d %q %v", code, errOut, *handedTo)
	}
	if storedAccount(t, rotaHome)["remoteControl"] != true {
		t.Fatal("the setting is kept for the next quiet launch")
	}
	if _, errOut, code := call(t, "run", "1", "hello there", "--remote-control"); code == 0 || !strings.Contains(errOut, "takes no prompt") {
		t.Fatalf("a question has nothing to control: %d %q", code, errOut)
	}
	if _, errOut, code := call(t, "run", "1", "--remote-control="); code == 0 || !strings.Contains(errOut, "needs a name") {
		t.Fatalf("%d %q", code, errOut)
	}
}

// A long-lived token, or a dead login, is a token in the environment, and
// Claude Code refuses Remote Control for one.
func TestRunWithRemoteControlRefusesAnAccountNotOnItsOwnLogin(t *testing.T) {
	seedLong(t, time.Now().Add(200*24*time.Hour), `,"dead":true`)
	handedTo := handover(t)
	_, errOut, code := call(t, "run", "1", "--remote-control")
	if code == 0 || !strings.Contains(errOut, "stored login") || len(*handedTo) != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
}

// `rota set --remote-control on|off` is the setting itself; --clear resets
// it with the rest; and while Claude Code runs in the home rota says at once
// that it waits.
func TestSetRemoteControl(t *testing.T) {
	rotaHome, home := seedLiving(t, "")
	out, errOut, code := call(t, "set", "1", "--remote-control", "on")
	if code != 0 || !strings.Contains(out, "remote      on") || errOut != "" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if storedAccount(t, rotaHome)["remoteControl"] != true {
		t.Fatal("saved")
	}
	out, _, _ = call(t, "--json", "list", "--short")
	if !strings.Contains(out, `"remote_control": true`) {
		t.Fatalf("listed: %s", out)
	}
	if _, errOut, code := call(t, "set", "1", "--clear"); code != 0 || errOut != "" {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, ok := storedAccount(t, rotaHome)["remoteControl"]; ok {
		t.Fatal("--clear resets it")
	}
	liveIn(t, home)
	if _, errOut, _ := call(t, "set", "1", "--remote-control", "on"); !strings.Contains(errOut, "takes effect when the account's running sessions end") {
		t.Fatalf("said at once: %q", errOut)
	}
	if _, errOut, code := call(t, "set", "1", "--remote-control", "maybe"); code == 0 || !strings.Contains(errOut, "on or off") {
		t.Fatalf("%d %q", code, errOut)
	}
}

// Logging an account in again while Claude Code runs in its home adds one
// line: those sessions keep the login they have, and /login in one switches.
func TestLoggingInAgainWhileItRunsSaysWhatTheSessionsKeep(t *testing.T) {
	_, home := seedLiving(t, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/profile") {
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "NEW", "refresh_token": "R2", "expires_in": 3600,
			"account": map[string]string{"uuid": "u1", "email_address": "a@b.c"}})
	}))
	defer srv.Close()
	for _, e := range []*string{&rota.ClaudeEndpoints.Token, &rota.ClaudeEndpoints.Profile} {
		old := *e
		t.Cleanup(func() { *e = old })
	}
	rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile = srv.URL+"/token", srv.URL+"/profile"
	liveIn(t, home)
	out, _, code := call(t, "login")
	if code != 0 {
		t.Fatal(out)
	}
	id := strings.Fields(strings.Split(out, "Then: rota login ")[1])[0]
	out, errOut, code := call(t, "login", id, "CODE")
	if code != 0 || !strings.Contains(out, "Refreshed claude account 1") ||
		!strings.Contains(out, "keep their present login until they end; /login inside one switches it at once") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}
