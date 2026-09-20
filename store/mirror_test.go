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

// claudeWorld is a stand-in for the person's own Claude Code directory: some
// of everything that has been seen in one, including the files that must not
// be shared. The environment points at it, so the store mirrors this rather
// than whatever the machine running the test has.
func claudeWorld(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	for _, name := range []string{"projects", "skills", "plugins", "daemon", "sessions"} {
		if err := os.Mkdir(filepath.Join(src, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		"settings.json", "CLAUDE.md", "history.jsonl", ".claude.json",
		"daemon.lock", "daemon.log", "daemon.status.json", "daemon-auth-cooldown",
		".credentials.json", ".DS_Store",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", src)
	return src
}

// claudeStore is a store holding one claude account, ready to run.
func claudeStore(t *testing.T) (*Store, *rota.Account) {
	t.Helper()
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"claude","uuid":"c1","token":{"accessToken":"tok"}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, s.Find(1)
}

// linksTo fails unless path is a symlink to want.
func linksTo(t *testing.T, path, want string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s should be a link to %s: %v", path, want, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s should be a link, not %s", path, fi.Mode())
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s points at %s, want %s", path, got, want)
	}
}

// absent fails unless nothing at all is at path — not even a broken link.
func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s must not be in the mirror: %v", path, err)
	}
}

// configDirs is every CLAUDE_CONFIG_DIR in an environment. There must never
// be two: runtimes disagree on which duplicate wins, and the loser here is
// the account's own world.
func configDirs(env []string) []string {
	var out []string
	for _, e := range env {
		if k, v, _ := strings.Cut(e, "="); k == "CLAUDE_CONFIG_DIR" {
			out = append(out, v)
		}
	}
	return out
}

// A claude account runs in a directory of its own, so Claude Code starts a
// daemon of its own for it — and that directory is the person's own world
// seen through links, so nothing about the account's session is different
// except who pays for it. The daemon's own files are what stays behind.
func TestAClaudeAccountRunsInAMirrorOfTheSharedWorld(t *testing.T) {
	src := claudeWorld(t)
	s, a := claudeStore(t)

	cmd, err := s.command(a, true)
	if err != nil {
		t.Fatal(err)
	}
	dst := s.ownHome(a)
	if got := configDirs(rota.Environ(HostEnv(), cmd)); len(got) != 1 || got[0] != dst {
		t.Fatalf("the child must see exactly one config dir, its own: %v", got)
	}
	for _, name := range []string{"projects", "skills", "plugins", "settings.json", "CLAUDE.md", "history.jsonl"} {
		linksTo(t, filepath.Join(dst, name), filepath.Join(src, name))
	}
	// Settings, memory and conversations are shared; the daemon's state, and
	// a credential store rota has no business in, are not.
	linksTo(t, filepath.Join(dst, ".claude.json"), filepath.Join(src, ".claude.json"))
	for _, name := range []string{"daemon", "daemon.lock", "daemon.log", "daemon.status.json", "daemon-auth-cooldown", ".credentials.json", ".DS_Store", "sessions"} {
		absent(t, filepath.Join(dst, name))
	}
}

// The mirror is rebuilt on every launch, because the world it mirrors keeps
// changing: a skill added since the last run has to appear, and one deleted
// has to stop being a link to nothing.
func TestTheMirrorFollowsTheWorldItMirrors(t *testing.T) {
	src := claudeWorld(t)
	s, a := claudeStore(t)
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	dst := s.ownHome(a)

	if err := os.Mkdir(filepath.Join(src, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	linksTo(t, filepath.Join(dst, "agents"), filepath.Join(src, "agents"))
	absent(t, filepath.Join(dst, "settings.json"))
}

// What the account already has is its own. Claude Code writes its caches,
// its daemon files and whatever else it likes inside the mirror, and the
// next launch must not replace any of it with a link to the person's.
func TestTheMirrorNeverReplacesWhatTheAccountHasOfItsOwn(t *testing.T) {
	src := claudeWorld(t)
	s, a := claudeStore(t)
	dst := s.ownHome(a)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dst, "CLAUDE.md")
	if err := os.WriteFile(own, []byte("the account's own"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(own)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("a real file of the account's own must survive the mirror: %v %v", fi, err)
	}
	body, err := os.ReadFile(own)
	if err != nil || string(body) != "the account's own" {
		t.Fatalf("its contents too: %q %v", body, err)
	}
	// The rest is still mirrored around it.
	linksTo(t, filepath.Join(dst, "settings.json"), filepath.Join(src, "settings.json"))
}

// An account given a directory of its own was given a separate world
// deliberately. It keeps it: no mirror, and still exactly one answer to
// where Claude Code's configuration is.
func TestAnAccountWithItsOwnDirectoryIsNotMirrored(t *testing.T) {
	claudeWorld(t)
	s, a := claudeStore(t)
	a.ConfigDir = t.TempDir()

	cmd, err := s.command(a, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := configDirs(rota.Environ(HostEnv(), cmd)); len(got) != 1 || got[0] != a.ConfigDir {
		t.Fatalf("the chosen directory is the only one: %v", got)
	}
	if _, err := os.Lstat(filepath.Join(s.ownHome(a), "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("nothing should have been mirrored into the unused home: %v", err)
	}
}

// A mirror that cannot be built does not stop a run: it is billed to the
// right account either way, and the only loss is the daemon. The person is
// told once, on stderr, and Claude Code runs where it always did.
func TestAMirrorThatCannotBeBuiltIsAWarningAndNotAFailure(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", notADir)
	s, a := claudeStore(t)
	var said []string
	s.Warn = func(msg string) { said = append(said, msg) }

	cmd, err := s.command(a, true)
	if err != nil {
		t.Fatalf("the run must go ahead: %v", err)
	}
	if len(said) != 1 || !strings.Contains(said[0], "could not mirror") || !strings.Contains(said[0], s.ownHome(a)) {
		t.Fatalf("one warning naming the directory: %v", said)
	}
	if got := configDirs(cmd.Env); len(got) != 0 {
		t.Fatalf("without a mirror the child keeps the person's own directory: %v", got)
	}
}

// A hermetic run answers from nothing, and lib gives it a throwaway
// directory of its own. Mirroring the person's world into the account's home
// first would be work nobody reads — and the child must still see exactly
// one configuration directory, the throwaway one.
func TestAHermeticRunSkipsTheMirrorAndKeepsItsThrowawayDirectory(t *testing.T) {
	src := claudeWorld(t)
	s, a := claudeStore(t)
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s1","result":"CFG={{env:CLAUDE_CONFIG_DIR|NONE}}","num_turns":1}`))
	t.Setenv("PATH", bin)

	scratch := t.TempDir()
	res, err := s.Run(context.Background(), a, rota.Spec{Prompt: "hi", Hermetic: true, ScratchDir: scratch}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, got, ok := strings.Cut(res.Result, "CFG=")
	if !ok || got == src || got == s.ownHome(a) {
		t.Fatalf("a hermetic run gets the throwaway directory: %q", res.Result)
	}
	if _, err := os.Lstat(filepath.Join(s.ownHome(a), ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("the mirror should not have been built for a hermetic run: %v", err)
	}
}
