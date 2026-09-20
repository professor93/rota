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

// A mirror built by an older rota may hold a link for a name that is no
// longer shared — sessions/ was shared once. The next launch removes that
// link, because a link in the mirror is always rota's own work. A real entry
// of the same name is the account's and stays.
func TestAStaleLinkForANameNoLongerSharedIsRemoved(t *testing.T) {
	src := claudeWorld(t)
	s, a := claudeStore(t)
	dst := s.ownHome(a)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "sessions"), filepath.Join(dst, "sessions")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dst, "daemon"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	absent(t, filepath.Join(dst, "sessions"))
	if fi, err := os.Lstat(filepath.Join(dst, "daemon")); err != nil || !fi.IsDir() {
		t.Fatalf("the account's own daemon directory must stay: %v %v", fi, err)
	}
	// And the link that is meant to be there is not mistaken for a stale one.
	linksTo(t, filepath.Join(dst, ".claude.json"), filepath.Join(src, ".claude.json"))
}

// Where an account's conversations live is its own setting, and `own` is one
// half of it: the entries a conversation is keyed by are left out of the
// mirror, so Claude Code makes them in the account's home and nobody else
// ever reads them. Everything else — settings, memory, skills — is still
// the person's own world through the links.
func TestAnAccountKeepingItsConversationsToItselfIsLinkedToNoneOfThem(t *testing.T) {
	src := claudeWorld(t)
	s, a := claudeStore(t)
	a.Sessions = rota.SessionsOwn

	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	dst := s.ownHome(a)
	for _, name := range conversationState {
		absent(t, filepath.Join(dst, name))
	}
	for _, name := range []string{"skills", "plugins", "settings.json", "CLAUDE.md"} {
		linksTo(t, filepath.Join(dst, name), filepath.Join(src, name))
	}
	linksTo(t, filepath.Join(dst, ".claude.json"), filepath.Join(src, ".claude.json"))
	absent(t, filepath.Join(dst, "sessions"))
}

// An account told which folder its conversations belong in is linked into
// that folder, whether or not the person's own directory has anything of the
// kind. rota makes the folder first: a link to nothing is pruned on the next
// refresh, and Claude Code cannot create a directory through one.
func TestAnAccountToldWhereItsConversationsGoIsLinkedIntoThatFolder(t *testing.T) {
	src := claudeWorld(t)
	s, a := claudeStore(t)
	folder := filepath.Join(t.TempDir(), "threads") // not there yet
	a.Sessions = folder

	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	dst := s.ownHome(a)
	for _, name := range conversationState {
		linksTo(t, filepath.Join(dst, name), filepath.Join(folder, name))
		fi, err := os.Stat(filepath.Join(folder, name))
		if err != nil {
			t.Fatalf("%s must exist to be linked to: %v", name, err)
		}
		if want := name != conversationHistory; fi.IsDir() != want {
			t.Fatalf("%s is %s", name, fi.Mode())
		}
	}
	// The rest of the world is shared exactly as before, and the registry of
	// live sessions is the account's own in every mode.
	linksTo(t, filepath.Join(dst, "settings.json"), filepath.Join(src, "settings.json"))
	linksTo(t, filepath.Join(dst, "skills"), filepath.Join(src, "skills"))
	absent(t, filepath.Join(dst, "sessions"))
}

// The setting can be changed, so the refresh converges on whatever it says
// now rather than only filling in what is missing: a link the mirror no
// longer wants goes, and one pointing at the wrong place is re-pointed.
func TestChangingWhereConversationsLiveMovesTheLinks(t *testing.T) {
	src := claudeWorld(t)
	s, a := claudeStore(t)
	dst := s.ownHome(a)
	folder := filepath.Join(t.TempDir(), "threads")

	// shared, as it starts.
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	linksTo(t, filepath.Join(dst, "projects"), filepath.Join(src, "projects"))

	// shared -> its own: the links to the shared conversations go, the rest
	// of the mirror stays.
	a.Sessions = rota.SessionsOwn
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	absent(t, filepath.Join(dst, "projects"))
	absent(t, filepath.Join(dst, "history.jsonl"))
	linksTo(t, filepath.Join(dst, "settings.json"), filepath.Join(src, "settings.json"))

	// its own -> a folder.
	a.Sessions = folder
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	linksTo(t, filepath.Join(dst, "projects"), filepath.Join(folder, "projects"))

	// and back to shared: the folder is left where it is, and the links point
	// at the person's own directory again.
	a.Sessions = ""
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	linksTo(t, filepath.Join(dst, "projects"), filepath.Join(src, "projects"))
	linksTo(t, filepath.Join(dst, "history.jsonl"), filepath.Join(src, "history.jsonl"))
	if _, err := os.Stat(filepath.Join(folder, "projects")); err != nil {
		t.Fatalf("the folder somebody named is theirs, not the mirror's: %v", err)
	}
}

// Conversations the account already has of its own are never replaced by a
// link to somebody else's. They are somebody's work, and a setting is not a
// reason to lose it: they stay, the account goes on reading them, and the
// person is told which they are so they can move them aside and mean it.
func TestConversationsAnAccountAlreadyHasOfItsOwnAreKeptAndSaidSo(t *testing.T) {
	src := claudeWorld(t)
	s, a := claudeStore(t)
	dst := s.ownHome(a)
	a.Sessions = rota.SessionsOwn
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	// Claude Code files a conversation in the home while it is the account's
	// own to file in.
	own := filepath.Join(dst, "projects", "-tmp-x")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}

	var said []string
	s.Warn = func(msg string) { said = append(said, msg) }
	a.Sessions = ""
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "projects")); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the account's own conversations must survive the change: %v %v", fi, err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Fatalf("and their contents: %v", err)
	}
	if len(said) != 1 || !strings.Contains(said[0], dst) || !strings.Contains(said[0], "projects") {
		t.Fatalf("one warning naming the directory and the entries: %v", said)
	}
	// What is not in the way is linked around them.
	linksTo(t, filepath.Join(dst, "history.jsonl"), filepath.Join(src, "history.jsonl"))
}

// An account with a directory of its own and a folder for its conversations
// gets the conversations arranged inside that directory and nothing else:
// the rest of it is the account's own world, not a mirror of anybody's. And
// there is still exactly one answer to where Claude Code's configuration is.
func TestAnOwnDirectoryGetsTheConversationFolderAndNothingElse(t *testing.T) {
	claudeWorld(t)
	s, a := claudeStore(t)
	a.ConfigDir = t.TempDir()
	folder := filepath.Join(t.TempDir(), "threads")
	a.Sessions = folder

	cmd, err := s.command(a, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := configDirs(rota.Environ(HostEnv(), cmd)); len(got) != 1 || got[0] != a.ConfigDir {
		t.Fatalf("the chosen directory is the only one: %v", got)
	}
	for _, name := range conversationState {
		linksTo(t, filepath.Join(a.ConfigDir, name), filepath.Join(folder, name))
	}
	for _, name := range []string{"settings.json", "CLAUDE.md", "skills", ".claude.json", "sessions"} {
		absent(t, filepath.Join(a.ConfigDir, name))
	}
}

// With a directory of its own and nothing said about conversations, Claude
// Code's own behaviour is the answer: they live in that directory, and rota
// touches none of it.
func TestAnOwnDirectoryIsLeftAloneWhenNoFolderIsNamed(t *testing.T) {
	claudeWorld(t)
	s, a := claudeStore(t)
	a.ConfigDir = t.TempDir()
	for _, mode := range []string{"", rota.SessionsOwn} {
		a.Sessions = mode
		if _, err := s.command(a, true); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(a.ConfigDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("sessions=%q arranged a directory rota was not asked to: %v", mode, entries)
		}
	}
}
