package store

import (
	"path/filepath"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// remoteControlAtStartup connects every interactive session to Remote
// Control as it starts, as whoever the home's .claude.json names. With the
// account's own remote control setting off, that is the shared file, so a
// launch says so, once, naming the file that turns it on — and changes
// nothing. Claude Code 2.1.322 decides it in this order: false in a
// project's or a local settings file turns it off; otherwise the managed
// settings, then the person's own, say; and only when none of them does,
// the key in .claude.json. A project's true is ignored, as Claude Code
// ignores it.
func TestRemoteControlAtStartupIsSaidWhenTheAccountsSettingIsOff(t *testing.T) {
	managed := noManaged(t)
	src := t.TempDir()
	env := []string{"CLAUDE_CONFIG_DIR=" + src}
	cwd := t.TempDir()
	settings := filepath.Join(src, "settings.json")
	config := filepath.Join(src, ".claude.json")
	project := filepath.Join(cwd, ".claude", "settings.json")
	storedRouteHere(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	said := func() []string {
		t.Helper()
		var out []string
		s.Warn = func(msg string) { out = append(out, msg) }
		s.sayRiskySettings(a, env, cwd, nil, false, true)
		return out
	}
	const advice = "`rota set 1 --remote-control on` gives it a .claude.json of its own"
	check := func(what, from string) {
		t.Helper()
		got := said()
		if from == "" {
			if len(got) != 0 {
				t.Fatalf("%s: %q, want nothing said", what, got)
			}
			return
		}
		if len(got) != 1 || !strings.Contains(got[0], "remoteControlAtStartup in "+resolved(t, from)) || !strings.Contains(got[0], advice) {
			t.Fatalf("%s: %q, want one warning naming %s", what, got, from)
		}
	}

	check("nothing sets it", "")
	writeFile(t, config, `{"remoteControlAtStartup":true}`)
	check(".claude.json alone", config)
	writeFile(t, settings, `{"remoteControlAtStartup":false}`)
	check("the person's settings say false", "")
	writeFile(t, settings, `{"remoteControlAtStartup":true}`)
	check("the person's settings say true", settings)
	writeFile(t, project, `{"remoteControlAtStartup":false}`)
	check("a project's false", "")
	writeFile(t, project, `{"remoteControlAtStartup":true}`)
	writeFile(t, settings, `{}`)
	writeFile(t, config, `{}`)
	check("a project's true alone", "")
	writeFile(t, filepath.Join(managed, "managed-settings.json"), `{"remoteControlAtStartup":true}`)
	writeFile(t, settings, `{"remoteControlAtStartup":false}`)
	check("managed settings, above the person's", filepath.Join(managed, "managed-settings.json"))

	// Only an account that could have the setting is told to turn it on:
	// one on an API key, or one in a directory the person chose, is refused
	// it, and Claude Code refuses it Remote Control anyway.
	keyed := *a
	keyed.Credential = "apikey"
	chosen := *a
	chosen.ConfigDir = t.TempDir()
	for what, b := range map[string]*rota.Account{"an API key": &keyed, "a chosen directory": &chosen} {
		var out []string
		s.Warn = func(msg string) { out = append(out, msg) }
		s.sayRiskySettings(b, env, cwd, nil, false, true)
		for _, msg := range out {
			if strings.Contains(msg, "remoteControlAtStartup") {
				t.Fatalf("%s is told to turn on what it is refused: %s", what, msg)
			}
		}
	}

	a.RemoteControl = true
	check("the account's own setting on", "")
}

// Only an interactive launch connects at startup: a run is claude -p, which
// never does, so a run of the same account is not warned. The handover's
// launch is, through the home's mirror of the person's .claude.json.
func TestRemoteControlAtStartupIsNotSaidForARun(t *testing.T) {
	noManaged(t)
	src := claudeWorld(t)
	writeFile(t, filepath.Join(src, ".claude.json"), `{"remoteControlAtStartup":true}`)
	t.Chdir(t.TempDir())
	storedRouteHere(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	for _, msg := range ran(t, s, a, rota.Spec{}) {
		if strings.Contains(msg, "remoteControlAtStartup") {
			t.Fatalf("a run is warned about Remote Control at startup: %s", msg)
		}
	}
	onPath(t, "claude")
	// A run lets go of its store, so the launch opens one of its own.
	s = openTemp(t)
	a = livingClaude(s, "u1")
	said := prepared(t, s, a)
	if len(said) != 1 || !strings.Contains(said[0], "remoteControlAtStartup") {
		t.Fatalf("the handover's launch: %q, want the warning", said)
	}
}

// With an account's remote control setting on, Remote Control registers as
// whoever the .claude.json in its home names. When that is not the account
// — the person's shared file still in place while the switch waits, or a
// sign-in somebody made since — the launch says so in one line, naming who
// it will be. A file that names the account, or nobody, says nothing; and
// with the setting off nothing registers, so nothing is said either.
func TestRemoteControlUnderAnotherIdentityIsSaid(t *testing.T) {
	noManaged(t)
	home := t.TempDir()
	env := []string{"CLAUDE_CONFIG_DIR=" + home}
	config := filepath.Join(home, ".claude.json")
	s, a := claudeStore(t)
	a.Email, a.RemoteControl = "mine@x", true
	said := func() []string {
		t.Helper()
		var out []string
		s.Warn = func(msg string) { out = append(out, msg) }
		s.sayRiskySettings(a, env, "", nil, false, true)
		return out
	}
	writeFile(t, config, `{"oauthAccount":{"emailAddress":"other@x","accountUuid":"u2"}}`)
	want := "Remote Control will register under the claude.ai identity other@x, which " + resolved(t, config) +
		" names, not this account's mine@x"
	if got := said(); len(got) != 1 || !strings.Contains(got[0], want) {
		t.Fatalf("another identity: %q, want %q", got, want)
	}
	for _, body := range []string{`{"oauthAccount":{"emailAddress":"MINE@x"}}`, `{}`, `not json`} {
		writeFile(t, config, body)
		if got := said(); len(got) != 0 {
			t.Fatalf("%s: %q, want nothing said", body, got)
		}
	}
	writeFile(t, config, `{"oauthAccount":{"emailAddress":"other@x"}}`)
	a.RemoteControl = false
	if got := said(); len(got) != 0 {
		t.Fatalf("remote control off: %q, want nothing said", got)
	}
}
