package store

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// personWorld is the person's own Claude Code directory with one of
// everything: what is shared, what is each account's own, and a real
// .claude.json. The environment points at it.
func personWorld(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	for _, dir := range []string{"projects", "skills", "plugins", "daemon", "sessions", "jobs", "teams",
		"backups", "telemetry", "statsig", "bridge-spawn", "todos", "file-history"} {
		if err := os.Mkdir(filepath.Join(src, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"settings.json", "CLAUDE.md", "history.jsonl", "daemon.lock", "daemon.log",
		".credentials.json", "policy-limits.json", "mcp-needs-auth-cache.json", "ide.lock"} {
		writeFile(t, filepath.Join(src, name), name)
	}
	writeFile(t, filepath.Join(src, ".claude.json"), `{
  "numStartups": 7,
  "oauthAccount": {"accountUuid": "person", "emailAddress": "me@home"},
  "mcpServers": {"shared-srv": {"command": "srv"}},
  "projects": {"/work/a": {"hasTrustDialogAccepted": true, "allowedTools": ["Bash"]}}
}
`)
	t.Setenv("CLAUDE_CONFIG_DIR", src)
	return src
}

// neverShared are the entries of a Claude Code directory that belong to one
// login or one running Claude Code, and never resolve to anybody else's.
var neverShared = []string{"sessions", "telemetry", "daemon", "daemon.lock", "daemon.log",
	".credentials.json", "policy-limits.json", "statsig", "mcp-needs-auth-cache.json", "bridge-spawn", "ide.lock"}

// sameThing reports whether two paths lead to the same file or directory.
func sameThing(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// claudeMakesItsOwn plays Claude Code starting in a home: it makes what it
// keeps for itself wherever rota left no link.
func claudeMakesItsOwn(t *testing.T, home string, names []string) {
	t.Helper()
	for _, name := range names {
		p := filepath.Join(home, name)
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		if strings.Contains(name, ".") {
			writeFile(t, p, "own "+home)
		} else if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// Two accounts are isolated where it matters — each has its own login and
// its own daemon: nothing either one's Claude Code authenticates or runs
// with is the other's or the person's, in the default shared mode and with
// both keeping their conversations in the same folder. Their background
// sessions and teams are conversations and follow that setting, linked to
// the person's or to the folder. With Remote Control on, their configuration
// file and its backups are their own too; with it off those are the
// person's, as everything else shared is.
func TestTwoAccountsShareNothingTheirRunningClaudeCodeKeeps(t *testing.T) {
	storedRouteHere(t)
	for _, folder := range []bool{false, true} {
		for _, remote := range []bool{false, true} {
			src := personWorld(t)
			s := openTemp(t)
			a, b := livingClaude(s, "uA"), livingClaude(s, "uB")
			if folder {
				shared := filepath.Join(t.TempDir(), "threads")
				a.Sessions, b.Sessions = shared, shared
			}
			a.RemoteControl, b.RemoteControl = remote, remote
			envA, err := launchEnv(t, s, a)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := launchEnv(t, s, b); err != nil {
				t.Fatal(err)
			}
			homeA, homeB := s.Home(a), s.Home(b)
			for _, name := range []string{"jobs", "teams"} {
				want := filepath.Join(src, name)
				if folder {
					want = filepath.Join(a.SessionsDir(), name)
				}
				linksTo(t, filepath.Join(homeA, name), want)
				linksTo(t, filepath.Join(homeB, name), want)
			}
			private := slices.Clone(neverShared)
			if remote {
				private = append(private, ".claude.json", "backups")
			} else {
				linksTo(t, filepath.Join(homeA, ".claude.json"), filepath.Join(src, ".claude.json"))
				linksTo(t, filepath.Join(homeA, "backups"), filepath.Join(src, "backups"))
			}
			for _, name := range private {
				if fi, err := os.Lstat(filepath.Join(homeA, name)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
					t.Fatalf("folder=%v remote=%v: %s is linked in A's home", folder, remote, name)
				}
			}
			claudeMakesItsOwn(t, homeA, private)
			claudeMakesItsOwn(t, homeB, private)
			for _, name := range private {
				pa := filepath.Join(homeA, name)
				if sameThing(pa, filepath.Join(homeB, name)) || sameThing(pa, filepath.Join(src, name)) {
					t.Fatalf("folder=%v remote=%v: %s is shared", folder, remote, name)
				}
			}
			for _, e := range envA {
				if strings.Contains(e, homeB) || e == "ROTA_ACCOUNT_ID="+strconv.Itoa(b.ID) || strings.Contains(e, b.Email) {
					t.Fatalf("A's launch names B: %s", e)
				}
			}
			s.Close()
		}
	}
}

// What is alive in one account's home says nothing about another's.
func TestOneAccountsLiveSessionsDoNotMakeAnotherAlive(t *testing.T) {
	personWorld(t)
	s := openTemp(t)
	a, b := livingClaude(s, "uA"), livingClaude(s, "uB")
	alive(t, s.Home(b), "sessions/1.json", os.Getpid())
	alive(t, s.Home(b), "daemon.lock", os.Getpid())
	if !s.claudeHome(a, s.claimed(a)).quiet() || s.claudeHome(b, s.claimed(b)).quiet() {
		t.Fatal("B's records are B's")
	}
	// And the reverse: a run of A, and A's records, leave B as quiet as B is.
	os.RemoveAll(filepath.Join(s.Home(b), "sessions"))
	os.Remove(filepath.Join(s.Home(b), "daemon.lock"))
	alive(t, s.Home(a), "daemon.lock", os.Getpid())
	release, ok := s.holdRun(a)
	if !ok {
		t.Fatal("claim")
	}
	defer release()
	if !s.claimed(a) || s.claimed(b) {
		t.Fatal("a claim is the account's own")
	}
	if s.claudeHome(a, s.claimed(a)).quiet() || !s.claudeHome(b, s.claimed(b)).quiet() {
		t.Fatal("A's run and A's daemon are A's")
	}
}

// Launching and removing one account touches nothing of another's: not its
// home, not its keychain item, not its login, and not its record in the
// store. Each launch reads its own store and no other, and each removal
// asks the keychain for its own item alone.
func TestOneAccountsLoginNeverTouchesAnothers(t *testing.T) {
	storedRouteHere(t)
	personWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	a, b := livingClaude(s, "uA"), livingClaude(s, "uB")
	homeA, homeB := s.Home(a), s.Home(b)
	svcA, svcB := service(t, homeA), service(t, homeB)
	writeFile(t, filepath.Join(homeA, "CLAUDE.md"), "A's")
	writeFile(t, filepath.Join(homeB, "CLAUDE.md"), "B's")

	// Each account's home holds a newer login of its own, as a rotation by
	// Claude Code would leave it, in the keychain.
	for _, x := range []*rota.Account{a, b} {
		if _, err := launchEnv(t, s, x); err != nil {
			t.Fatal(err)
		}
	}
	k.items[svcA] = storeLogin("A-rotA", "R-rotA", time.Now().Add(9*time.Hour).UnixMilli(), "1999999999000")
	k.items[svcB] = storeLogin("A-rotB", "R-rotB", time.Now().Add(9*time.Hour).UnixMilli(), "1999999999000")
	snapshotB := func() (string, string, string) {
		r, _ := readLogin(t, homeB)
		item, _ := k.item(svcB)
		return r + "|" + readFile(filepath.Join(homeB, "CLAUDE.md")), item, b.Token.Refresh + "|" + b.Staged
	}
	files, item, record := snapshotB()

	from := len(k.asked())
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if a.Token.Refresh != "R-rotA" {
		t.Fatalf("A took its own rotation and nothing else: %q", a.Token.Refresh)
	}
	for _, q := range k.asked()[from:] {
		if !strings.HasSuffix(q, svcA) {
			t.Fatalf("A's launch asked for another item: %v", k.asked()[from:])
		}
	}
	if f, i, r := snapshotB(); f != files || i != item || r != record {
		t.Fatal("A's launch left B exactly as it was")
	}

	from = len(k.asked())
	if err := s.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	for _, q := range k.asked()[from:] {
		if !strings.HasSuffix(q, svcA) {
			t.Fatalf("A's removal asked for another item: %v", k.asked()[from:])
		}
	}
	if f, i, r := snapshotB(); f != files || i != item || r != record || s.Find(b.ID) == nil {
		t.Fatal("A's removal left B exactly as it was")
	}
	if _, err := os.Stat(homeA); !os.IsNotExist(err) {
		t.Fatal("A's home is gone")
	}

	// And the other way round.
	from = len(k.asked())
	if _, err := launchEnv(t, s, b); err != nil {
		t.Fatal(err)
	}
	if b.Token.Refresh != "R-rotB" {
		t.Fatalf("B took its own rotation: %q", b.Token.Refresh)
	}
	if err := s.Remove(b.ID); err != nil {
		t.Fatal(err)
	}
	for _, q := range k.asked()[from:] {
		if !strings.HasSuffix(q, svcB) {
			t.Fatalf("B asked for another item: %v", k.asked()[from:])
		}
	}
	if _, ok := k.item(svcB); ok {
		t.Fatal("B's item went with B")
	}
}

// The names that belong to one login or one running Claude Code are never
// linked from the person's directory, whatever the account says about its
// conversations; jobs/ and teams/ follow that setting, as conversations do —
// shared, the account's own, or in the folder it names.
func TestTheMirrorLinksNoneOfARunningClaudeCodesOwnState(t *testing.T) {
	src := personWorld(t)
	s, a := claudeStore(t)
	for _, mode := range []string{"", rota.SessionsOwn, filepath.Join(t.TempDir(), "threads")} {
		a.Sessions = mode
		if _, err := s.command(a, true); err != nil {
			t.Fatal(err)
		}
		dst := s.ownHome(a)
		for _, name := range neverShared {
			if fi, err := os.Lstat(filepath.Join(dst, name)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("sessions=%q: %s is linked", mode, name)
			}
		}
		linksTo(t, filepath.Join(dst, "skills"), filepath.Join(src, "skills"))
		for _, name := range []string{"jobs", "teams"} {
			switch dir := a.SessionsDir(); {
			case dir != "":
				linksTo(t, filepath.Join(dst, name), filepath.Join(dir, name))
			case mode == rota.SessionsOwn:
				absent(t, filepath.Join(dst, name))
			default:
				linksTo(t, filepath.Join(dst, name), filepath.Join(src, name))
			}
		}
	}
}

// A link an older rota made for a name that is no longer shared is taken
// away only once nothing runs in the home: a running daemon has that
// directory open through it. A conversation link still follows the
// sessions setting as it always did.
func TestALinkForANameNoLongerSharedWaitsForAQuietHome(t *testing.T) {
	src := personWorld(t)
	s, a := claudeStore(t)
	dst := s.ownHome(a)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"telemetry", "statsig"} {
		if err := os.Symlink(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			t.Fatal(err)
		}
	}
	alive(t, dst, "daemon.lock", os.Getpid())
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	linksTo(t, filepath.Join(dst, "telemetry"), filepath.Join(src, "telemetry"))
	linksTo(t, filepath.Join(dst, "statsig"), filepath.Join(src, "statsig"))
	linksTo(t, filepath.Join(dst, "projects"), filepath.Join(src, "projects"))

	a.Sessions = rota.SessionsOwn
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	absent(t, filepath.Join(dst, "projects"))

	os.Remove(filepath.Join(dst, "daemon.lock"))
	if _, err := s.command(a, true); err != nil {
		t.Fatal(err)
	}
	absent(t, filepath.Join(dst, "telemetry"))
	absent(t, filepath.Join(dst, "statsig"))
}
