package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// An active workload identity federation profile in the person's Anthropic
// configuration directory signs Claude Code in ahead of the account's login.
// A launch on that login says so on stderr, once, as a warning naming the
// account and the file and nothing in it, by a handover and by a run alike;
// the answer on stdout is untouched. A launch on a token in its environment
// signs in with the token, and says nothing of the profile.
func TestALaunchOnTheAccountsLoginWarnsAboutAnActiveFederationProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps the environment route, whose token outranks any profile")
	}
	seedLiving(t, "")
	answering(t, "claude")
	handover(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("ANTHROPIC_CONFIG_DIR", "")
	file := filepath.Join(home, ".config", "anthropic", "configs", "default.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"authentication":{"type":"oidc_federation","federation_rule_id":"fdrl_SECRET"},`+
		`"organization_id":"SECRET-ORG"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	want := "warning: claude/a@b.c: " + file + " is the active Anthropic profile and signs Claude Code in by " +
		"workload identity federation, in place of this account's login and every other account's; activate another " +
		"profile with `ant profile activate <name>`, or move the file away, to turn it off\n"

	_, errOut, code := call(t, "run", "1")
	if code != 0 || strings.Count(errOut, want) != 1 || strings.Contains(errOut, "SECRET") {
		t.Fatalf("the handover warns once, naming the file only: %d %q", code, errOut)
	}
	out, errOut, code := call(t, "run", "1", "hello")
	if code != 0 || out != "ANSWERED\n" || strings.Count(errOut, want) != 1 || strings.Contains(errOut, "SECRET") {
		t.Fatalf("a run warns once, naming the file only: %d %q %q", code, out, errOut)
	}

	rotaHome := t.TempDir()
	t.Setenv("ROTA_HOME", rotaHome)
	writeStore(t, rotaHome, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@b.c","order":1,"token":{"accessToken":"t"}}]}`)
	if _, errOut, code := call(t, "run", "1", "hello"); code != 0 || strings.Contains(errOut, "profile") {
		t.Fatalf("a token in the environment outranks the profile: %d %q", code, errOut)
	}
}
