package sessions

import (
	"errors"
	"os"
	"testing"
)

// The process list is full of a CLI's own helpers. Claude Code runs several
// children that rename themselves "claude bg-pty-host" and the like, and
// counting those as open sessions would report four instances for one. Only a
// process whose command is the executable itself counts.
func TestOnlyTheCLIItselfCountsNotItsHelpers(t *testing.T) {
	defer stubProcs(t, []proc{
		{pid: 36287, command: "/Users/me/.local/bin/claude"},
		{pid: 889, command: "claude"},
		{pid: 7192, command: "claude"},
		{pid: 40001, command: "/opt/homebrew/bin/codex"},
		{pid: 50002, command: "/usr/bin/vim"},
	}, map[int]string{36287: "/Users/me/src/api", 40001: "/Users/me/src/api"})()

	got, note := ProcessInstances(nil)
	if note != "" {
		t.Fatalf("nothing should be missing here: %q", note)
	}
	if len(got) != 2 {
		t.Fatalf("one claude and one codex, got %d: %+v", len(got), got)
	}
	seen := map[string]Instance{}
	for _, in := range got {
		seen[in.Provider] = in
	}
	if seen["claude"].PID != 36287 || seen["claude"].Dir != "/Users/me/src/api" {
		t.Fatalf("%+v", seen["claude"])
	}
	if seen["codex"].PID != 40001 {
		t.Fatalf("%+v", seen["codex"])
	}
	if seen["claude"].Kind != "cli" {
		t.Fatalf("a bare CLI is not an editor: %+v", seen["claude"])
	}
}

// A process rota launched itself is already known, with the account attached.
// Reporting it twice — once with an account and once without — would read as
// two instances where there is one.
func TestAProcessRotaAlreadyKnowsIsNotReportedTwice(t *testing.T) {
	defer stubProcs(t, []proc{
		{pid: 36287, command: "/usr/bin/claude"},
		{pid: 99, command: "/usr/bin/claude"},
	}, map[int]string{36287: "/a", 99: "/b"})()

	got, _ := ProcessInstances([]int{36287})
	if len(got) != 1 || got[0].PID != 99 {
		t.Fatalf("the one rota started is left to the registry: %+v", got)
	}
}

// Where a process is working is not always knowable: on macOS it takes lsof,
// which may not be installed. Saying so is better than dropping the row with
// no explanation, and better than inventing a directory.
func TestAnUnknowableDirectoryIsSaidRatherThanGuessed(t *testing.T) {
	defer stubProcs(t, []proc{{pid: 5, command: "/usr/bin/claude"}}, nil)()
	cwdOf = func(int) (string, error) { return "", errors.New("lsof: not found") }

	got, note := ProcessInstances(nil)
	if len(got) != 1 || got[0].Dir != "" {
		t.Fatalf("the instance is still real, its directory is not known: %+v", got)
	}
	if note == "" {
		t.Fatal("a gap this size must be explained, not left silent")
	}
}

// stubProcs replaces the two things that touch the machine, and returns the
// undo. Tests must not depend on what happens to be running.
func stubProcs(t *testing.T, list []proc, dirs map[int]string) func() {
	t.Helper()
	origList, origCwd := listProcs, cwdOf
	listProcs = func() ([]proc, error) { return list, nil }
	cwdOf = func(pid int) (string, error) {
		if d, ok := dirs[pid]; ok {
			return d, nil
		}
		return "", os.ErrNotExist
	}
	return func() { listProcs, cwdOf = origList, origCwd }
}

// The parser is tested against what ps actually printed on the machine this
// was written on, because that is where the mistake was.
//
// Claude Code's helpers rename themselves to "claude bg-pty-host …". Cutting
// the line at its first space and matching the name reports every one of them
// as an open project — six rows for one daemon. Only argv[0] with a path in it
// is the executable.
func TestRealPSOutputCountsOnlyTheExecutables(t *testing.T) {
	const out = `  889 claude bg-pty-host --bg-pty-host /tmp/cc-daemon-501/e25d20cc/spare/363b5986.pty.sock 200 50 -- /Users/me
 2305 /Applications/Claude.app/Contents/Helpers/chrome-native-host chrome-extension://fcoeoabgfenejglbffodgkkb
 7178 claude bg-pty-host --bg-pty-host /tmp/cc-daemon-501/e25d20cc/spare/5b909c78.pty.sock 200 50 -- /Users/me
 7192 claude bg-spare --bg-spare /tmp/cc-daemon-501/e25d20cc/spare/5b909c78.claim.sock
36287 /Users/me/.local/bin/claude daemon run --origin transient --spawned-by {"label":"claude","cwd":
43679 claude bg-pty-host --bg-pty-host /tmp/cc-daemon-501/e25d20cc/spare/5c5a4e23.pty.sock 200 50 -- /Users/me
50070 claude bg-pty-host --bg-pty-host /tmp/cc-daemon-501/e25d20cc/spare/3a461c2e.pty.sock 200 50 -- /Users/me`

	list := parsePS(out)
	if len(list) != 7 {
		t.Fatalf("every line is a process, got %d", len(list))
	}
	var cli []proc
	for _, p := range list {
		if cliFor(p.command) != "" {
			cli = append(cli, p)
		}
	}
	if len(cli) != 1 {
		t.Fatalf("one claude executable among six helpers, got %d: %+v", len(cli), cli)
	}
	if cli[0].pid != 36287 {
		t.Fatalf("the executable is the one with a path: %+v", cli[0])
	}
	// The Claude desktop app's helper is not a vendor CLI either.
	if cliFor("/Applications/Claude.app/Contents/Helpers/chrome-native-host") != "" {
		t.Fatal("only the CLI binaries count")
	}
}
