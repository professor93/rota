package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// configOf reads a .claude.json as a map.
func configOf(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return m
}

func isRegular(t *testing.T, path string) bool {
	t.Helper()
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// By default an account shares the person's configuration file, as it
// shares everything else of theirs: a link, and nothing written, to the
// person's file or the account's home.
func TestByDefaultTheConfigurationFileIsThePersons(t *testing.T) {
	src := personWorld(t)
	before, _ := os.ReadFile(filepath.Join(src, ".claude.json"))
	stamp := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(src, ".claude.json"), stamp, stamp)
	s, a := claudeStore(t)
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	dst := s.ownHome(a)
	linksTo(t, filepath.Join(dst, ".claude.json"), filepath.Join(src, ".claude.json"))
	after, _ := os.ReadFile(filepath.Join(src, ".claude.json"))
	fi, _ := os.Stat(filepath.Join(src, ".claude.json"))
	if string(after) != string(before) || !fi.ModTime().Equal(stamp) {
		t.Fatal("the person's file is never written")
	}
	absent(t, filepath.Join(dst, ".claude.json.own"))
}

// Turning Remote Control on, with nothing running in the home, gives the
// account its own copy: the person's settings, the account's identity and
// not the person's, and Claude Code's backups of it its own too.
func TestTurningRemoteControlOnGivesTheAccountItsOwnConfiguration(t *testing.T) {
	src := personWorld(t)
	s, a := claudeStore(t)
	a.Email, a.Org = "c1@x", "org-1"
	a.Extra = map[string]string{"organization_name": "Org One"}
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	a.RemoteControl = true
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	dst := s.ownHome(a)
	path := filepath.Join(dst, ".claude.json")
	if !isRegular(t, path) {
		t.Fatal("a file of its own, not a link")
	}
	cfg := configOf(t, path)
	id, _ := cfg["oauthAccount"].(map[string]any)
	if id["accountUuid"] != "c1" || id["emailAddress"] != "c1@x" || id["organizationUuid"] != "org-1" || id["organizationName"] != "Org One" {
		t.Fatalf("the account's identity: %v", id)
	}
	if cfg["numStartups"] != float64(7) || cfg["mcpServers"] == nil || cfg["projects"] == nil {
		t.Fatalf("started from the person's: %v", cfg)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("private: %v", fi.Mode())
	}
	if fi, err := os.Lstat(filepath.Join(dst, "backups")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("its backups are its own while its file is")
	}
	if strings.Contains(string(mustRead(t, filepath.Join(src, ".claude.json"))), "c1@x") {
		t.Fatal("the person's file is never written")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Turning it off puts the account's own copy aside, never deleting it, and
// links the person's file again; turning it on again brings the same copy
// back with everything the account had added.
func TestTurningRemoteControlOffPutsTheCopyAsideAndOnBringsItBack(t *testing.T) {
	src := personWorld(t)
	s, a := claudeStore(t)
	a.RemoteControl = true
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	dst := s.ownHome(a)
	path := filepath.Join(dst, ".claude.json")
	cfg := configOf(t, path)
	cfg["theAccountsOwn"] = "kept"
	raw, _ := json.Marshal(cfg)
	writeFile(t, path, string(raw))
	if err := os.Mkdir(filepath.Join(dst, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}

	a.RemoteControl = false
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	linksTo(t, path, filepath.Join(src, ".claude.json"))
	if got := configOf(t, filepath.Join(dst, ".claude.json.own")); got["theAccountsOwn"] != "kept" {
		t.Fatal("put aside, not deleted")
	}
	if fi, err := os.Stat(filepath.Join(dst, "backups.own")); err != nil || !fi.IsDir() {
		t.Fatal("its backups go aside with it")
	}
	linksTo(t, filepath.Join(dst, "backups"), filepath.Join(src, "backups"))

	a.RemoteControl = true
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	if !isRegular(t, path) || configOf(t, path)["theAccountsOwn"] != "kept" {
		t.Fatal("the same copy comes back")
	}
	absent(t, filepath.Join(dst, ".claude.json.own"))
	if fi, err := os.Lstat(filepath.Join(dst, "backups")); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("and its backups")
	}
}

// While Claude Code runs in the home nothing is switched, either way, and
// the person is told when the setting takes effect.
func TestRemoteControlWaitsForTheHomeToBeQuiet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps claude on a token in its environment, where Remote Control never works, so there is nothing for it to wait for")
	}
	src := personWorld(t)
	s, a := claudeStore(t)
	// A login Claude Code can keep, which Remote Control needs.
	a.Token.Refresh, a.Token.ExpiresAt = "r", time.Now().Add(time.Hour).UnixMilli()
	var said []string
	s.Warn = func(m string) { said = append(said, m) }
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	dst := s.ownHome(a)
	alive(t, dst, "sessions/1.json", os.Getpid())
	a.RemoteControl = true
	if got := s.RemoteControlNow(a); got == nil || !strings.Contains(got.Error(), "takes effect when") {
		t.Fatalf("rota set says it at once: %q", got)
	}
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	linksTo(t, filepath.Join(dst, ".claude.json"), filepath.Join(src, ".claude.json"))
	if len(said) != 1 || !strings.Contains(said[0], "takes effect when the account's running sessions end") {
		t.Fatalf("and the launch says it: %q", said)
	}
	os.Remove(filepath.Join(dst, "sessions", "1.json"))
	if s.RemoteControlNow(a) != nil {
		t.Fatal("nothing to wait for in a quiet home")
	}
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	if !isRegular(t, filepath.Join(dst, ".claude.json")) {
		t.Fatal("switched once quiet")
	}
	alive(t, dst, "sessions/1.json", os.Getpid())
	a.RemoteControl = false
	said = nil
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	if !isRegular(t, filepath.Join(dst, ".claude.json")) || len(said) != 1 {
		t.Fatalf("not switched back while alive either: %q", said)
	}
}

// The account's own copy keeps up with the person's, one way and only by
// adding: a new top-level key, a new project, a new MCP server, a folder
// trusted since. Nothing the account has is removed or overwritten, and the
// person's identity never comes over.
func TestTheAccountsOwnConfigurationKeepsUpByAddingOnly(t *testing.T) {
	src := personWorld(t)
	s, a := claudeStore(t)
	a.RemoteControl = true
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	dst := s.ownHome(a)
	path := filepath.Join(dst, ".claude.json")
	cfg := configOf(t, path)
	cfg["numStartups"] = 99
	cfg["projects"] = map[string]any{
		"/work/a": map[string]any{"hasTrustDialogAccepted": false, "allowedTools": []string{"Edit"}},
		"/work/z": map[string]any{"hasTrustDialogAccepted": true},
	}
	raw, _ := json.Marshal(cfg)
	writeFile(t, path, string(raw))
	writeFile(t, filepath.Join(src, ".claude.json"), `{
  "numStartups": 8, "newKey": "n",
  "oauthAccount": {"accountUuid": "someone-else"},
  "mcpServers": {"shared-srv": {"command": "changed"}, "new-srv": {"command": "n"}},
  "projects": {"/work/a": {"hasTrustDialogAccepted": true}, "/work/b": {"hasTrustDialogAccepted": false}}
}`)
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	got := configOf(t, path)
	if got["numStartups"] != float64(99) || got["newKey"] != "n" {
		t.Fatalf("the account's values stay, new keys come: %v", got)
	}
	mcp := got["mcpServers"].(map[string]any)
	if mcp["new-srv"] == nil || mcp["shared-srv"].(map[string]any)["command"] != "srv" {
		t.Fatalf("servers are added, not changed: %v", mcp)
	}
	projects := got["projects"].(map[string]any)
	pa := projects["/work/a"].(map[string]any)
	if pa["hasTrustDialogAccepted"] != true || pa["allowedTools"].([]any)[0] != "Edit" || projects["/work/b"] == nil ||
		projects["/work/z"].(map[string]any)["hasTrustDialogAccepted"] != true {
		t.Fatalf("trust spreads, projects are added, nothing else changes: %v", projects)
	}
	if got["oauthAccount"].(map[string]any)["accountUuid"] != "c1" {
		t.Fatal("the identity is the account's, always")
	}
	stamp := time.Now().Add(-time.Hour)
	os.Chtimes(path, stamp, stamp)
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(stamp) {
		t.Fatal("with nothing new to fold the file is not written")
	}
}

// A directory the person chose is never given an identity, nor is its
// .claude.json changed in any other way — not even for an account an older
// store holds with Remote Control on, which it cannot have there and is
// refused, in the one sentence that says why.
func TestAChosenDirectorysConfigurationIsNeverChanged(t *testing.T) {
	personWorld(t)
	s, a := claudeStore(t)
	a.ConfigDir = t.TempDir()
	path := filepath.Join(a.ConfigDir, ".claude.json")
	writeFile(t, path, `{"theirs": 1}`)
	for _, on := range []bool{false, true} {
		a.RemoteControl = on
		if _, err := s.command(a, true); err != nil {
			t.Fatal(err)
		}
		if got := readFile(path); got != `{"theirs": 1}` {
			t.Fatalf("untouched with remote control %v: %s", on, got)
		}
	}
	err := s.CheckRemoteControl(a)
	if !errors.Is(err, rota.ErrInvalidRequest) || !strings.Contains(err.Error(),
		"it runs on a token because its configuration directory is one you chose, and Remote Control needs the home rota keeps for an account (clear `--config`)") {
		t.Fatalf("refused: %v", err)
	}
	a.ConfigDir = ""
	if err := s.CheckRemoteControl(a); err != nil {
		t.Fatalf("its own home may have it: %v", err)
	}
}

// With Remote Control on, a real configuration file in the home is what the
// account is meant to have, and is not reported as one in the way.
func TestWithRemoteControlOnTheAccountsOwnFileIsNotReported(t *testing.T) {
	personWorld(t)
	s, a := claudeStore(t)
	dst := s.ownHome(a)
	writeFile(t, filepath.Join(dst, ".claude.json"), `{}`)
	alive(t, dst, "sessions/1.json", os.Getpid())
	a.RemoteControl = true
	var said []string
	s.Warn = func(m string) { said = append(said, m) }
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	for _, m := range said {
		if strings.Contains(m, "holds its own") {
			t.Fatalf("not reported: %q", said)
		}
	}
}

// A setting that could not be saved is not on: ReadyRemoteControl turns it
// back off in memory, so a store that lives on, a daemon's, never claims
// Remote Control the file does not record.
func TestReadyRemoteControlThatCannotBeSavedLeavesItOff(t *testing.T) {
	if os.Getuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs an unwritable directory")
	}
	personWorld(t)
	dir := t.TempDir()
	until := time.Now().Add(time.Hour).UnixMilli()
	writeAccounts(t, dir, `{"accounts":[
		{"id":1,"provider":"claude","uuid":"c1","token":{"accessToken":"tok","refreshToken":"r","expiresAt":`+itoa(int(until))+`}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	before, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	a := s.Find(1)
	on, err := s.ReadyRemoteControl(a)
	if on || err == nil || a.RemoteControl {
		t.Fatalf("a setting that could not be saved: turned on %v, err %v, in memory %v; want it off and the error", on, err, a.RemoteControl)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "accounts.json")); string(after) != string(before) {
		t.Fatalf("accounts.json changed:\n%s", after)
	}
}

// ReadyRemoteControl is the one place a session with Remote Control is
// readied, for the command line and the daemon alike. Another provider's
// account is refused and left as it was. A claude account has the setting
// turned on and saved, said once; a home still running Claude Code from
// before is refused with the wrong-account guard, the setting kept for the
// next quiet launch; and an account on a token, which Remote Control
// refuses, says why. A chosen configuration directory is refused before
// anything is saved.
func TestReadyRemoteControl(t *testing.T) {
	personWorld(t)
	dir := t.TempDir()
	until := time.Now().Add(time.Hour).UnixMilli()
	login := `"token":{"accessToken":"tok","refreshToken":"r","expiresAt":` + itoa(int(until)) + `}`
	writeAccounts(t, dir, `{"accounts":[
		{"id":1,"provider":"codex","email":"c@x","token":{"accessToken":"x"}},
		{"id":2,"provider":"claude","uuid":"c2",`+login+`},
		{"id":3,"provider":"claude","uuid":"c3","token":{"accessToken":"tok"}},
		{"id":4,"provider":"claude","uuid":"c4","configDir":"`+filepath.ToSlash(t.TempDir())+`",`+login+`}],"nextId":5}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	saved := func(id int) bool {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Accounts []struct {
				ID            int  `json:"id"`
				RemoteControl bool `json:"remoteControl"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, a := range doc.Accounts {
			if a.ID == id {
				return a.RemoteControl
			}
		}
		return false
	}

	on, err := s.ReadyRemoteControl(s.Find(1))
	if on || !errors.Is(err, rota.ErrInvalidRequest) || err.Error() != "Remote Control is Claude Code's, and codex/c@x is a codex account" || saved(1) {
		t.Fatalf("another provider's account: %v %v, saved %v", on, err, saved(1))
	}
	on, err = s.ReadyRemoteControl(s.Find(4))
	if on || !errors.Is(err, rota.ErrInvalidRequest) || saved(4) || s.Find(4).RemoteControl {
		t.Fatalf("an account with a chosen directory: %v %v, saved %v", on, err, saved(4))
	}
	if runtime.GOOS == "windows" {
		return // Windows keeps claude on a token, where Remote Control never works
	}

	a := s.Find(2)
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	alive(t, s.ownHome(a), "sessions/1.json", os.Getpid())
	on, err = s.ReadyRemoteControl(a)
	if !on || !errors.Is(err, rota.ErrBusy) || !strings.Contains(err.Error(), "until then Remote Control would act as the wrong account") || !saved(2) {
		t.Fatalf("a home still running Claude Code: %v %v, saved %v", on, err, saved(2))
	}
	os.Remove(filepath.Join(s.ownHome(a), "sessions", "1.json"))
	if on, err := s.ReadyRemoteControl(a); on || err != nil {
		t.Fatalf("once it is quiet, already on: %v %v", on, err)
	}

	on, err = s.ReadyRemoteControl(s.Find(3))
	if !on || !errors.Is(err, rota.ErrUnsupported) || !strings.Contains(err.Error(), "own stored login") || !saved(3) {
		t.Fatalf("an account on a token: %v %v, saved %v", on, err, saved(3))
	}
}
