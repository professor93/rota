package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	src := personWorld(t)
	s, a := claudeStore(t)
	var said []string
	s.Warn = func(m string) { said = append(said, m) }
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	dst := s.ownHome(a)
	alive(t, dst, "sessions/1.json", os.Getpid())
	a.RemoteControl = true
	if got := s.RemoteControlWaits(a); !strings.Contains(got, "takes effect when") {
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
	if s.RemoteControlWaits(a) != "" {
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

// An account with a directory of its own is given an identity there only
// when the file has none, and only with Remote Control on.
func TestAChosenDirectoryIsOnlyGivenAnIdentityItLacks(t *testing.T) {
	personWorld(t)
	s, a := claudeStore(t)
	a.ConfigDir = t.TempDir()
	path := filepath.Join(a.ConfigDir, ".claude.json")
	writeFile(t, path, `{"theirs": 1}`)
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := configOf(t, path)["oauthAccount"]; ok {
		t.Fatal("left alone with Remote Control off")
	}
	a.RemoteControl = true
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	cfg := configOf(t, path)
	if cfg["theirs"] != float64(1) || cfg["oauthAccount"].(map[string]any)["accountUuid"] != "c1" {
		t.Fatalf("given an identity: %v", cfg)
	}
	writeFile(t, path, `{"oauthAccount": {"accountUuid": "set-by-claude"}}`)
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	if configOf(t, path)["oauthAccount"].(map[string]any)["accountUuid"] != "set-by-claude" {
		t.Fatal("an identity already there is not touched")
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
