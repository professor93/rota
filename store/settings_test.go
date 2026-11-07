package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// resolved is a path as the warning names it: with every link resolved, which
// for a file reached through the mirror is the person's own.
func resolved(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// noManaged points the machine's managed settings at an empty directory for
// one test, and returns it.
func noManaged(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := managedSettingsDir
	managedSettingsDir = dir
	t.Cleanup(func() { managedSettingsDir = old })
	return dir
}

// prepared launches a by the handover's road and returns what it warned.
func prepared(t *testing.T, s *Store, a *rota.Account) []string {
	t.Helper()
	var said []string
	s.Warn = func(msg string) { said = append(said, msg) }
	_, _, release, err := s.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	release()
	return said
}

// ran runs a with spec on a fake claude and returns what it warned.
func ran(t *testing.T, s *Store, a *rota.Account, spec rota.Spec) []string {
	t.Helper()
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1","result":"ok","num_turns":1}`))
	t.Setenv("PATH", bin)
	var said []string
	s.Warn = func(msg string) { said = append(said, msg) }
	spec.Prompt = "hi"
	if _, err := s.Run(context.Background(), a, spec, nil, nil); err != nil {
		t.Fatal(err)
	}
	return said
}

// A settings file can set back what a claude launch drops — a host for the
// login, another credential, a proxy, another configuration directory:
// Claude Code writes its env block over the environment it inherited, and
// apiKeyHelper is a credential of its own ranked above the account's login.
// The person's
// settings.json reaches every account through the mirror, so the launch
// names the file — the person's own, not the link — and the keys, once,
// and never a value. The file is left exactly as it was.
func TestASettingsFileThatSetsBackWhatALaunchDropsIsNamedOnce(t *testing.T) {
	noManaged(t)
	src := claudeWorld(t)
	body := `{"model":"opus","apiKeyHelper":"vault-read SECRET-HELPER",` +
		`"env":{"ANTHROPIC_BASE_URL":"https://SECRET-HOST.example","DISABLE_AUTO_COMPACT":"1",` +
		`"ANTHROPIC_AUTH_TOKEN":"SECRET-TOKEN","HTTPS_PROXY":"http://proxy.example:8080","CLAUDE_CONFIG_DIR":"/SECRET-DIR"}}`
	settings := filepath.Join(src, "settings.json")
	writeFile(t, settings, body)
	t.Chdir(t.TempDir())
	onPath(t, "claude")
	s, a := claudeStore(t)

	said := prepared(t, s, a)
	if len(said) != 1 {
		t.Fatalf("one warning for the launch: %q", said)
	}
	want := resolved(t, settings) + " sets apiKeyHelper, env.ANTHROPIC_AUTH_TOKEN, env.ANTHROPIC_BASE_URL, env.CLAUDE_CONFIG_DIR, env.HTTPS_PROXY"
	if !strings.Contains(said[0], want) || !strings.HasPrefix(said[0], a.String()+": ") {
		t.Fatalf("the account, the person's own file and the keys it sets:\n%s\nwant %s", said[0], want)
	}
	for _, never := range []string{"SECRET", "proxy.example", "DISABLE_AUTO_COMPACT", "model"} {
		if strings.Contains(said[0], never) {
			t.Fatalf("%q has no place in the warning: %s", never, said[0])
		}
	}
	if got, _ := os.ReadFile(settings); string(got) != body {
		t.Fatalf("the file is the person's and stays as it was: %s", got)
	}
}

// What a launch does not drop is nobody's business here: a timeout, a model,
// a key set to "" — which is how a settings file cancels an inherited
// variable — or null, a key outside env, a name in the wrong case, CLAUDECODE,
// a file that is not JSON. None of them is a warning.
func TestSettingsThatSetNothingOfTheAccountsAreNotAWarning(t *testing.T) {
	noManaged(t)
	claudeWorld(t) // its settings.json is not JSON at all
	cwd := t.TempDir()
	t.Chdir(cwd)
	writeFile(t, filepath.Join(cwd, ".claude", "settings.json"),
		`{"ANTHROPIC_BASE_URL":"https://outside.env.example","ApiKeyHelper":"x","apiKeyHelper":"",`+
			`"env":{"API_TIMEOUT_MS":"600000","ANTHROPIC_MODEL":"opus","ANTHROPIC_BASE_URL":"","HTTPS_PROXY":"",`+
			`"CLAUDE_CODE_USE_VERTEX":null,"anthropic_api_key":"k","CLAUDECODE":"1"}}`)
	writeFile(t, filepath.Join(cwd, ".claude", "settings.local.json"), `{"env":["ANTHROPIC_API_KEY"]}`)
	onPath(t, "claude")
	s, a := claudeStore(t)

	if said := prepared(t, s, a); len(said) != 0 {
		t.Fatalf("nothing here sends the login anywhere: %q", said)
	}
}

// The files a project keeps are read where Claude Code reads them: the
// shared one in the directory it starts in, and the local one there and at
// the root of the repository that directory is in. A shared file at the
// root is not read by a run started below it, so it is not named.
func TestAProjectsSettingsAreNamedWhereClaudeCodeReadsThem(t *testing.T) {
	noManaged(t)
	claudeWorld(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "app")
	writeFile(t, filepath.Join(sub, ".claude", "settings.json"), `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`)
	writeFile(t, filepath.Join(sub, ".claude", "settings.local.json"), `{"env":{"ANTHROPIC_API_KEY":"k"}}`)
	writeFile(t, filepath.Join(repo, ".claude", "settings.local.json"), `{"apiKeyHelper":"h"}`)
	writeFile(t, filepath.Join(repo, ".claude", "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://x"}}`)
	t.Chdir(sub)
	onPath(t, "claude")
	s, a := claudeStore(t)

	said := prepared(t, s, a)
	if len(said) != 1 {
		t.Fatalf("one warning for the launch: %q", said)
	}
	for _, want := range []string{
		resolved(t, filepath.Join(sub, ".claude", "settings.json")) + " sets env.CLAUDE_CODE_USE_BEDROCK",
		resolved(t, filepath.Join(sub, ".claude", "settings.local.json")) + " sets env.ANTHROPIC_API_KEY",
		resolved(t, filepath.Join(repo, ".claude", "settings.local.json")) + " sets apiKeyHelper",
	} {
		if !strings.Contains(said[0], want) {
			t.Fatalf("missing %q in %s", want, said[0])
		}
	}
	if strings.Contains(said[0], "ANTHROPIC_BASE_URL") {
		t.Fatalf("the root's shared file is not read from a subdirectory: %s", said[0])
	}
}

// In a worktree Claude Code keeps the local settings at the main checkout's
// root, which the worktree's .git file leads to.
func TestAWorktreesLocalSettingsAreTheMainCheckouts(t *testing.T) {
	noManaged(t)
	claudeWorld(t)
	main := t.TempDir()
	gitdir := filepath.Join(main, ".git", "worktrees", "wt")
	writeFile(t, filepath.Join(gitdir, "commondir"), "../..\n")
	wt := filepath.Join(t.TempDir(), "wt")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+gitdir+"\n")
	writeFile(t, filepath.Join(main, ".claude", "settings.local.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"t"}}`)
	t.Chdir(wt)
	onPath(t, "claude")
	s, a := claudeStore(t)

	said := prepared(t, s, a)
	want := resolved(t, filepath.Join(main, ".claude", "settings.local.json")) + " sets env.ANTHROPIC_AUTH_TOKEN"
	if len(said) != 1 || !strings.Contains(said[0], want) {
		t.Fatalf("want %q: %q", want, said)
	}
}

// The machine's managed settings are read whatever a run asks for:
// managed-settings.json and the drop-ins beside it, in their order, leaving
// out a hidden file and one that is not JSON by name.
func TestManagedSettingsAreNamedToo(t *testing.T) {
	dir := noManaged(t)
	claudeWorld(t)
	t.Chdir(t.TempDir())
	writeFile(t, filepath.Join(dir, "managed-settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://gw"}}`)
	writeFile(t, filepath.Join(dir, "managed-settings.d", "20-auth.json"), `{"apiKeyHelper":"h"}`)
	writeFile(t, filepath.Join(dir, "managed-settings.d", "10-net.json"), `{"env":{"CLAUDE_CODE_USE_FOUNDRY":"1"}}`)
	writeFile(t, filepath.Join(dir, "managed-settings.d", ".hidden.json"), `{"apiKeyHelper":"h"}`)
	writeFile(t, filepath.Join(dir, "managed-settings.d", "notes.txt"), `{"apiKeyHelper":"h"}`)
	onPath(t, "claude")
	s, a := claudeStore(t)

	said := prepared(t, s, a)
	if len(said) != 1 {
		t.Fatalf("one warning: %q", said)
	}
	want := resolved(t, filepath.Join(dir, "managed-settings.json")) + " sets env.ANTHROPIC_BASE_URL; " +
		resolved(t, filepath.Join(dir, "managed-settings.d", "10-net.json")) + " sets env.CLAUDE_CODE_USE_FOUNDRY; " +
		resolved(t, filepath.Join(dir, "managed-settings.d", "20-auth.json")) + " sets apiKeyHelper"
	if !strings.HasSuffix(said[0], want) || strings.Contains(said[0], "hidden") || strings.Contains(said[0], "notes") {
		t.Fatalf("got %s\nwant it to end %s", said[0], want)
	}
}

// One file reached two ways is one file: started in the home directory, the
// project's .claude/settings.json is the person's own settings.json.
func TestOneFileReachedTwiceIsNamedOnce(t *testing.T) {
	noManaged(t)
	home := t.TempDir()
	src := filepath.Join(home, ".claude")
	writeFile(t, filepath.Join(src, "settings.json"), `{"env":{"ANTHROPIC_API_KEY":"k"}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", src)
	t.Chdir(home)
	onPath(t, "claude")
	s, a := claudeStore(t)

	said := prepared(t, s, a)
	if len(said) != 1 || strings.Count(said[0], "settings.json") != 1 {
		t.Fatalf("one file, named once: %q", said)
	}
}

// A run reads what the run will read: the directory it names, the sources
// it names — managed settings whatever they are — and no user settings when
// it is hermetic, whose directory is a fresh one with nothing in it.
func TestARunNamesWhatThatRunReads(t *testing.T) {
	managed := noManaged(t)
	src := claudeWorld(t)
	writeFile(t, filepath.Join(src, "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://u"}}`)
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, ".claude", "settings.json"), `{"env":{"CLAUDE_CODE_USE_VERTEX":"1"}}`)
	t.Chdir(elsewhere)
	proj := t.TempDir()
	writeFile(t, filepath.Join(proj, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"t"}}`)
	writeFile(t, filepath.Join(proj, ".claude", "settings.local.json"), `{"apiKeyHelper":"h"}`)
	writeFile(t, filepath.Join(managed, "managed-settings.json"), `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`)

	cases := []struct {
		name string
		spec rota.Spec
		want []string
	}{
		{"every source", rota.Spec{Cwd: proj}, []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "apiKeyHelper", "BEDROCK"}},
		{"no sources", rota.Spec{Cwd: proj, SettingSources: []string{}}, []string{"BEDROCK"}},
		{"project only", rota.Spec{Cwd: proj, SettingSources: []string{"project"}}, []string{"ANTHROPIC_AUTH_TOKEN", "BEDROCK"}},
		{"hermetic", rota.Spec{Cwd: proj, Hermetic: true}, []string{"ANTHROPIC_AUTH_TOKEN", "apiKeyHelper", "BEDROCK"}},
		{"its own directory", rota.Spec{SettingSources: []string{"project"}}, []string{"VERTEX", "BEDROCK"}},
	}
	every := []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "apiKeyHelper", "BEDROCK", "VERTEX"}
	for _, c := range cases {
		s, a := claudeStore(t) // a run lets go of its store's lock
		said := ran(t, s, a, c.spec)
		if len(said) != 1 {
			t.Fatalf("%s: one warning: %q", c.name, said)
		}
		for _, k := range every {
			if want := strings.Contains(strings.Join(c.want, " "), k); strings.Contains(said[0], k) != want {
				t.Fatalf("%s: %s named %v, want %v: %s", c.name, k, !want, want, said[0])
			}
		}
	}
}

// Only what is a settings file is read for this: a directory where a file
// belongs, or a file larger than any settings document, is passed over, and
// the launch goes ahead as it would have.
func TestWhatIsNotASettingsFileIsPassedOver(t *testing.T) {
	noManaged(t)
	claudeWorld(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.MkdirAll(filepath.Join(cwd, ".claude", "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	huge := `{"env":{"ANTHROPIC_API_KEY":"k"},"pad":"` + strings.Repeat("x", maxSettingsFile) + `"}`
	writeFile(t, filepath.Join(cwd, ".claude", "settings.local.json"), huge)
	onPath(t, "claude")
	s, a := claudeStore(t)

	if said := prepared(t, s, a); len(said) != 0 {
		t.Fatalf("nothing read, nothing said: %q", said)
	}
}

// A session that stays open reads its settings like any run, and is warned
// about the same way before it starts.
func TestASessionNamesWhatItReads(t *testing.T) {
	noManaged(t)
	claudeWorld(t)
	t.Chdir(t.TempDir())
	proj := t.TempDir()
	writeFile(t, filepath.Join(proj, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"t"}}`)
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Spec{Echo: true})
	t.Setenv("PATH", bin)
	s, a := claudeStore(t)
	var said []string
	s.Warn = func(msg string) { said = append(said, msg) }

	sess, err := s.Start(context.Background(), a, rota.Spec{Prompt: "hi", Input: true, Stream: true, Cwd: proj}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Wait(); err != nil {
		t.Fatal(err)
	}
	want := resolved(t, filepath.Join(proj, ".claude", "settings.json")) + " sets env.ANTHROPIC_AUTH_TOKEN"
	if len(said) != 1 || !strings.Contains(said[0], want) {
		t.Fatalf("want %q: %q", want, said)
	}
}

// A CLI started somewhere other than this process's directory is prepared
// with PrepareIn, and the project settings read for it are that directory's.
func TestPrepareInReadsTheProjectWhereTheCLIStarts(t *testing.T) {
	noManaged(t)
	claudeWorld(t)
	here := t.TempDir()
	writeFile(t, filepath.Join(here, ".claude", "settings.json"), `{"env":{"CLAUDE_CODE_USE_VERTEX":"1"}}`)
	t.Chdir(here)
	there := t.TempDir()
	writeFile(t, filepath.Join(there, ".claude", "settings.json"), `{"env":{"ANTHROPIC_API_KEY":"k"}}`)
	onPath(t, "claude")
	s, a := claudeStore(t)
	var said []string
	s.Warn = func(msg string) { said = append(said, msg) }

	_, _, release, err := s.PrepareIn(context.Background(), a, there)
	if err != nil {
		t.Fatal(err)
	}
	release()
	want := resolved(t, filepath.Join(there, ".claude", "settings.json")) + " sets env.ANTHROPIC_API_KEY"
	if len(said) != 1 || !strings.Contains(said[0], want) || strings.Contains(said[0], "VERTEX") {
		t.Fatalf("want only %q: %q", want, said)
	}
}
