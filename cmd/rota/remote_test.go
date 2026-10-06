package main

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/professor93/rota/internal/claudecode"
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
// that it waits. Windows keeps Claude Code on a token in its environment,
// which Remote Control refuses: the setting is kept there too, and rota says
// at once, every time it is turned on, that it cannot work on that platform.
func TestSetRemoteControl(t *testing.T) {
	const onToken = "warning: not supported by this provider: Remote Control needs claude/a@b.c's own stored login, " +
		"and it does not run on one: this platform keeps Claude Code on a token in its environment\n"
	quiet, waits := "", "takes effect when the account's running sessions end"
	if runtime.GOOS == "windows" {
		quiet, waits = onToken, onToken
	}
	rotaHome, home := seedLiving(t, "")
	out, errOut, code := call(t, "set", "1", "--remote-control", "on")
	if code != 0 || !strings.Contains(out, "remote      on") || errOut != quiet {
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
	if _, errOut, _ := call(t, "set", "1", "--remote-control", "on"); !strings.Contains(errOut, waits) {
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

// A claude account's home cannot change under a running Claude Code, and
// when it does change its login goes with it: the old home keeps no copy of
// the refresh token for anybody to present.
func TestSettingAClaudeAccountsConfigMovesItsLogin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps the environment route, and no login in a home")
	}
	rotaHome, home := seedLiving(t, "")
	handover(t)
	if _, errOut, code := call(t, "run", "1"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, ".credentials.json")); err != nil {
		t.Fatal("the home holds the login")
	}
	next := t.TempDir()
	liveIn(t, home)
	if _, errOut, code := call(t, "set", "1", "--config", next); code == 0 || !strings.Contains(errOut, "running") {
		t.Fatalf("refused while it runs: %d %q", code, errOut)
	}
	if storedAccount(t, rotaHome)["config_dir"] != nil {
		t.Fatal("nothing saved")
	}
	os.Remove(filepath.Join(home, "sessions", "1.json"))
	if _, errOut, code := call(t, "set", "1", "--config", next); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("the old home's login is taken out")
	}
	if got := storedAccount(t, rotaHome); got["staged"] != "-" {
		t.Fatalf("and the account forgets it: %v", got["staged"])
	}
}

// An account told a directory of the person's choosing runs on a token, so
// Remote Control is refused for it, saying why in one sentence: by `rota run
// --remote-control`, by `rota set --remote-control on`, and by naming such a
// directory for an account that has it on. Nothing is saved.
func TestRemoteControlIsRefusedForAChosenDirectory(t *testing.T) {
	dir := t.TempDir()
	rotaHome, _ := seedLiving(t, fmt.Sprintf(`,"config_dir":%q`, dir))
	handedTo := handover(t)
	const why = "it runs on a token because its configuration directory is one you chose, and Remote Control needs the home rota keeps for an account (clear `--config`)"
	if _, errOut, code := call(t, "run", "1", "--remote-control"); code == 0 || !strings.Contains(errOut, why) || len(*handedTo) != 0 {
		t.Fatalf("run refused: %d %q", code, errOut)
	}
	if _, errOut, code := call(t, "set", "1", "--remote-control", "on"); code == 0 || !strings.Contains(errOut, why) {
		t.Fatalf("set refused: %d %q", code, errOut)
	}
	if _, on := storedAccount(t, rotaHome)["remoteControl"]; on {
		t.Fatal("nothing saved")
	}
	if _, errOut, code := call(t, "set", "1", "--clear", "--remote-control", "on"); code != 0 {
		t.Fatalf("its own home may have it: %d %q", code, errOut)
	}
	if _, errOut, code := call(t, "set", "1", "--config", dir); code == 0 || !strings.Contains(errOut, "--remote-control off") {
		t.Fatalf("nor may it name such a directory while it has it: %d %q", code, errOut)
	}
	if got := storedAccount(t, rotaHome); got["config_dir"] != nil || got["remoteControl"] != true {
		t.Fatalf("nothing saved: %v", got)
	}
}

// `rota remove 1 2` asks about both before it touches either: with a window
// open in account 2's home, account 1's daemon is not stopped, and neither
// account goes.
func TestRemovingTwoAccountsStopsNothingWhenOneCannotGo(t *testing.T) {
	rotaHome := t.TempDir()
	t.Setenv("ROTA_HOME", rotaHome)
	until := time.Now().Add(time.Hour).UnixMilli()
	doc := fmt.Sprintf(`{"accounts":[`+
		`{"id":1,"provider":"claude","email":"a@b.c","order":1,"token":{"accessToken":"A1","refreshToken":"r1","expiresAt":%d}},`+
		`{"id":2,"provider":"claude","email":"d@e.f","order":2,"token":{"accessToken":"A2","refreshToken":"r2","expiresAt":%d}}`+
		`],"nextId":3,"ordered":true}`, until, until)
	if err := os.WriteFile(filepath.Join(rotaHome, "accounts.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	one, two := filepath.Join(rotaHome, "homes", "claude-1"), filepath.Join(rotaHome, "homes", "claude-2")
	if err := os.MkdirAll(one, 0o700); err != nil {
		t.Fatal(err)
	}
	daemon := fmt.Sprintf(`{"pid":%d,"kind":"daemon"}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(one, "daemon.lock"), []byte(daemon), 0o600); err != nil {
		t.Fatal(err)
	}
	liveIn(t, two)
	var stops atomic.Int64
	claudecode.StopDaemon = func(context.Context, string, []string) error { stops.Add(1); return nil }
	t.Cleanup(claudecode.StandIn)
	_, errOut, code := call(t, "remove", "1", "2")
	if code == 0 || !strings.Contains(errOut, "close it before removing") {
		t.Fatalf("refused: %d %q", code, errOut)
	}
	if stops.Load() != 0 {
		t.Fatal("and nothing was stopped")
	}
	raw, _ := os.ReadFile(filepath.Join(rotaHome, "accounts.json"))
	var stored struct {
		Accounts []struct {
			ID int `json:"id"`
		} `json:"accounts"`
	}
	if json.Unmarshal(raw, &stored) != nil || len(stored.Accounts) != 2 {
		t.Fatalf("both still there: %s", raw)
	}
	if _, err := os.Stat(one); err != nil {
		t.Fatal("and account 1's home with them")
	}
}

// rota set moves a claude account's home before it changes anything else:
// moving it saves the store on its way, and a move that then fails must have
// saved nothing else of the command — here a place in the queue.
func TestSetMovesTheHomeBeforeAnythingElse(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs a directory its owner cannot delete from, and the environment route keeps no login to move")
	}
	rotaHome, home := seedLiving(t, "")
	handover(t)
	if _, errOut, code := call(t, "run", "1"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	// The login cannot be taken out of the old home, after the move has
	// already read and saved it.
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(home, 0o700) })
	if _, errOut, code := call(t, "set", "1", "--config", t.TempDir(), "--order", "out"); code == 0 {
		t.Fatalf("the move fails: %q", errOut)
	}
	if got := storedAccount(t, rotaHome); got["order"] != float64(1) || got["config_dir"] != nil {
		t.Fatalf("and nothing else of the command was saved: %v %v", got["order"], got["config_dir"])
	}
	// A move in the queue that is refused is refused before the home moves.
	os.Chmod(home, 0o700)
	if _, errOut, code := call(t, "set", "1", "--order", "out"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, errOut, code := call(t, "set", "1", "--config", t.TempDir(), "--order", "up"); code == 0 || !strings.Contains(errOut, "out of the rotation") {
		t.Fatalf("refused: %d %q", code, errOut)
	}
	if storedAccount(t, rotaHome)["config_dir"] != nil {
		t.Fatal("the home did not move")
	}
	if _, err := os.Stat(filepath.Join(home, ".credentials.json")); err != nil {
		t.Fatal("and its login is where it was")
	}
}

// A stateless run with --with quota reads usage after the run, on a store
// whose lock the run released: nothing may rotate there. With a long token
// and an expired access token the provider is asked for no refresh, and the
// refresh token on disk stays the one it was.
func TestAStatelessRunWithQuotaRotatesNothing(t *testing.T) {
	seedLong(t, time.Now().Add(200*24*time.Hour), `,"token":{"accessToken":"SHORT","refreshToken":"r","expiresAt":1}`)
	hits := claudeTokenEndpoint(t)
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(`[{"type":"result","result":"OK","session_id":"s"}]`))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, errOut, code := call(t, "run", "1", "--stateless", "--with", "quota", "two plus two")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if *hits != 0 {
		t.Fatalf("the token endpoint was asked %d times", *hits)
	}
	raw, _ := os.ReadFile(filepath.Join(os.Getenv("ROTA_HOME"), "accounts.json"))
	if !strings.Contains(string(raw), `"refreshToken": "r"`) {
		t.Fatalf("the refresh token is the one it was: %s", raw)
	}
}
