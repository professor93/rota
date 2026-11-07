package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// federation is a workload identity federation profile as the `ant` CLI
// writes one. Every value in it is marked, so a warning that carried one
// would be seen to.
const federation = `{"version":"1.0","authentication":{"type":"oidc_federation",` +
	`"federation_rule_id":"fdrl_SECRET","service_account_id":"svac_SECRET",` +
	`"identity_token":{"source":"file","path":"/var/run/SECRET/token"}},` +
	`"organization_id":"SECRET-ORG","workspace_id":"wrkspc_SECRET","base_url":"https://SECRET.example"}`

// personHome gives the test a home of its own and none of the variables that
// would name another Anthropic configuration directory, and returns the one
// Claude Code would read in that home. "" is what Claude Code takes for unset.
func personHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ANTHROPIC_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	return filepath.Join(home, ".config", "anthropic")
}

// profileIn writes the profile name, kept as body, into the Anthropic
// configuration directory dir, with active as the active_config file when
// it is not "", and returns the profile's file.
func profileIn(t *testing.T, dir, active, name, body string) string {
	t.Helper()
	if active != "" {
		writeFile(t, filepath.Join(dir, "active_config"), active)
	}
	file := filepath.Join(dir, "configs", name+".json")
	writeFile(t, file, body)
	return file
}

// overridden is the warning a launch of a gives when file is the active
// federation profile.
func overridden(a *rota.Account, file string) string {
	return a.String() + ": " + file + " is the active Anthropic profile and signs Claude Code in by workload " +
		"identity federation, in place of this account's login and every other account's; " +
		"activate another profile with `ant profile activate <name>`, or move the file away, to turn it off"
}

// The profile of the Anthropic configuration directory that is active by
// default is the person's, and every account's Claude Code reads it: a
// workload identity federation one signs Claude Code in ahead of the login
// rota keeps in the account's home. Each launch on such a login says so once,
// naming the file and nothing in it, and leaves the files as they were.
func TestAnActiveFederationProfileIsNamedOnceByALaunchOnTheAccountsLogin(t *testing.T) {
	storedRouteHere(t)
	noManaged(t)
	claudeWorld(t)
	dir := personHome(t)
	file := profileIn(t, dir, "work\n", "work", federation)
	t.Chdir(t.TempDir())
	onPath(t, "claude")
	s := openTemp(t)
	a := livingClaude(s, "u1")

	for road, said := range map[string][]string{"handover": prepared(t, s, a), "run": ran(t, s, a, rota.Spec{})} {
		if len(said) != 1 || said[0] != overridden(a, file) {
			t.Fatalf("a %s says it once, naming the file:\n%q\nwant %q", road, said, overridden(a, file))
		}
		for _, never := range []string{"SECRET", "fdrl_", "svac_", "wrkspc_"} {
			if strings.Contains(said[0], never) {
				t.Fatalf("%q has no place in the warning: %s", never, said[0])
			}
		}
	}
	if got := readFile(file); got != federation {
		t.Fatalf("the profile is the person's and stays as it was: %s", got)
	}
	if got := readFile(filepath.Join(dir, "active_config")); got != "work\n" {
		t.Fatalf("so does the choice of it: %q", got)
	}
}

// The directory is found as Claude Code finds it, in the environment the
// launch hands it: ANTHROPIC_CONFIG_DIR, then anthropic in XDG_CONFIG_HOME,
// then .config/anthropic in the home, each value trimmed and a blank one
// passed over, and the first of two values taken, as Node takes it. A
// relative one is the CLI's, so it is read from the directory the CLI starts
// in, and from nowhere when that is not known. The profile is the one
// active_config names, trimmed as JavaScript trims, a byte-order mark
// included, and the one called default when nothing is named. The
// environment here is written out, so what a launch drops from the person's
// does not decide the answer.
func TestTheActiveProfileIsFoundWhereClaudeCodeFindsIt(t *testing.T) {
	home, xdg, named, cwd := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	inHome := profileIn(t, filepath.Join(home, ".config", "anthropic"), "", "default", federation)
	inXDG := profileIn(t, filepath.Join(xdg, "anthropic"), "", "default", federation)
	inNamed := profileIn(t, named, "ci", "ci", federation)
	trimmed := filepath.Join(t.TempDir(), "anthropic")
	profileIn(t, trimmed, "", "default", `{"authentication":{"type":"user_oauth"}}`)
	inTrimmed := profileIn(t, trimmed, "\uFEFF\u00a0 prod \r\n", "prod", federation)
	inCwd := profileIn(t, filepath.Join(cwd, "cfg", "anthropic"), "", "default", federation)

	for _, c := range []struct {
		name string
		env  []string
		cwd  string
		want string
	}{
		{"ANTHROPIC_CONFIG_DIR first", []string{"HOME=" + home, "XDG_CONFIG_HOME=" + xdg, "ANTHROPIC_CONFIG_DIR= " + named + "\n"}, "", inNamed},
		{"then XDG_CONFIG_HOME", []string{"HOME=" + home, "XDG_CONFIG_HOME=" + xdg, "ANTHROPIC_CONFIG_DIR=  "}, "", inXDG},
		{"then the home, default when nothing is named", []string{"HOME=" + home, "XDG_CONFIG_HOME="}, "", inHome},
		{"the first of two values", []string{"XDG_CONFIG_HOME=" + xdg, "HOME=" + home, "XDG_CONFIG_HOME=" + named}, "", inXDG},
		{"a name trimmed as JavaScript trims", []string{"XDG_CONFIG_HOME=" + filepath.Dir(trimmed)}, "", inTrimmed},
		{"a relative directory, from where the CLI starts", []string{"HOME=" + home, "XDG_CONFIG_HOME=cfg"}, cwd, inCwd},
		{"a relative directory, and no start known", []string{"HOME=" + home, "XDG_CONFIG_HOME=cfg"}, "", ""},
		{"a credential in the environment", []string{"HOME=" + home, "CLAUDE_CODE_OAUTH_TOKEN=t"}, "", ""},
		{"no directory at all", []string{"HOME= ", "XDG_CONFIG_HOME=\t"}, cwd, ""},
	} {
		if got := federationProfile(c.env, c.cwd); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A run is warned about the profile of the directory its CLI reads, which
// for a relative XDG_CONFIG_HOME is under the directory the run starts in,
// not under rota's own.
func TestARunIsWarnedAboutTheProfileWhereItsCLIStarts(t *testing.T) {
	storedRouteHere(t)
	noManaged(t)
	claudeWorld(t)
	personHome(t)
	t.Setenv("XDG_CONFIG_HOME", "cfg")
	here, there := t.TempDir(), t.TempDir()
	profileIn(t, filepath.Join(here, "cfg", "anthropic"), "", "default", `{"authentication":{"type":"user_oauth"}}`)
	file := profileIn(t, filepath.Join(there, "cfg", "anthropic"), "", "default", federation)
	t.Chdir(here)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if said := ran(t, s, a, rota.Spec{Cwd: there}); len(said) != 1 || said[0] != overridden(a, file) {
		t.Fatalf("the profile under the run's own directory: %q", said)
	}
}

// Only a profile Claude Code would sign in with is a warning: its JSON has
// "authentication": {"type": "oidc_federation"}, by those exact names, and it
// is the active one. A user_oauth profile does not outrank the account's own
// login, a profile that is not active is not read, a name active_config gives
// that has no file is not the default one instead, and a file JavaScript's
// JSON.parse refuses — a byte-order mark before the object — is none at all.
// Nor is there a profile when no directory can be named: never one in the
// directory the launch starts in.
func TestAProfileClaudeCodeWouldNotSignInWithIsNotAWarning(t *testing.T) {
	storedRouteHere(t)
	noManaged(t)
	claudeWorld(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	onPath(t, "claude")
	s := openTemp(t)
	a := livingClaude(s, "u1")

	for name, write := range map[string]func(dir string){
		"user_oauth": func(dir string) {
			profileIn(t, dir, "", "default", `{"authentication":{"type":"user_oauth"}}`)
		},
		"the type in another case": func(dir string) {
			profileIn(t, dir, "", "default", `{"authentication":{"type":"OIDC_FEDERATION"}}`)
		},
		"the type not a string": func(dir string) {
			profileIn(t, dir, "", "default", `{"authentication":{"type":["oidc_federation"]}}`)
		},
		"authentication in another case": func(dir string) {
			profileIn(t, dir, "", "default", `{"Authentication":{"type":"oidc_federation"}}`)
		},
		"a byte-order mark": func(dir string) {
			profileIn(t, dir, "", "default", "\uFEFF"+federation)
		},
		"not JSON": func(dir string) {
			profileIn(t, dir, "", "default", `{"authentication":{"type":"oidc_federation"}`)
		},
		"a profile that is not the active one": func(dir string) {
			profileIn(t, dir, "plain", "plain", `{"authentication":{"type":"user_oauth"}}`)
			profileIn(t, dir, "", "other", federation)
		},
		"a name with no file": func(dir string) {
			profileIn(t, dir, "gone", "default", federation)
		},
		"the last of two types": func(dir string) {
			profileIn(t, dir, "", "default", `{"authentication":{"type":"oidc_federation","type":"user_oauth"}}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			write(personHome(t))
			if said := prepared(t, s, a); len(said) != 0 {
				t.Fatalf("Claude Code signs in with the account's login here: %q", said)
			}
		})
	}
	t.Run("no directory", func(t *testing.T) {
		profileIn(t, personHome(t), "", "default", federation)
		profileIn(t, filepath.Join(cwd, ".config", "anthropic"), "", "default", federation)
		profileIn(t, filepath.Join(cwd, "anthropic"), "", "default", federation)
		t.Setenv("HOME", " ")
		if said := prepared(t, s, a); len(said) != 0 {
			t.Fatalf("no home, no profile: %q", said)
		}
	})
}

// A launch that carries a credential of its own in its environment signs in
// with that and not with the profile: a token rota hands Claude Code there,
// on the environment route, and an account's API key, are both ranked above
// it. Neither launch is warned.
func TestALaunchOnACredentialInItsEnvironmentIsNotWarnedAboutTheProfile(t *testing.T) {
	noManaged(t)
	claudeWorld(t)
	profileIn(t, personHome(t), "", "default", federation)
	t.Chdir(t.TempDir())
	onPath(t, "claude")

	s, a := claudeStore(t) // a bare token, so the environment route
	if env, err := launchEnv(t, s, a); err != nil || !hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("the token goes in the environment: %v", err)
	}
	if said := prepared(t, s, a); len(said) != 0 {
		t.Fatalf("the token in the environment outranks the profile: %q", said)
	}
	key := keyAccountIn(t, openTemp(t))
	if said := prepared(t, s, key); len(said) != 0 {
		t.Fatalf("the key outranks the profile: %q", said)
	}
}

// The files are read without following them anywhere a launch could block:
// an active_config that is not a regular file is no name at all.
func TestAnActiveConfigThatIsNotAFileIsNoName(t *testing.T) {
	storedRouteHere(t)
	noManaged(t)
	claudeWorld(t)
	dir := personHome(t)
	profileIn(t, dir, "", "default", `{"authentication":{"type":"user_oauth"}}`)
	profileIn(t, dir, "", "work", federation)
	if err := os.MkdirAll(filepath.Join(dir, "active_config"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	onPath(t, "claude")
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if said := prepared(t, s, a); len(said) != 0 {
		t.Fatalf("a directory names no profile, so the default one is active: %q", said)
	}
}
