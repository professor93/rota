package sessions

import (
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
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
	cwdsOf = func(pids []int) []cwd {
		out := make([]cwd, len(pids))
		for i := range out {
			out[i].err = errors.New("lsof: not found")
		}
		return out
	}

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
	origList, origCwd := listProcs, cwdsOf
	listProcs = func() ([]proc, error) { return list, nil }
	cwdsOf = func(pids []int) []cwd {
		out := make([]cwd, len(pids))
		for i, pid := range pids {
			if d, ok := dirs[pid]; ok {
				out[i].dir = d
			} else {
				out[i].err = os.ErrNotExist
			}
		}
		return out
	}
	return func() { listProcs, cwdsOf = origList, origCwd }
}

// On macOS each directory question is an lsof, which costs tens of
// milliseconds on a busy machine, so a listing asks once for every CLI it
// found, in the order ps gave them — and not at all when it found none.
func TestEveryDirectoryIsAskedForInOneGo(t *testing.T) {
	defer stubProcs(t, []proc{
		{pid: 30, command: "/usr/bin/claude"},
		{pid: 10, command: "/opt/homebrew/bin/codex"},
		{pid: 11, command: "/usr/bin/vim"},
		{pid: 20, command: "/usr/bin/claude"},
		{pid: 40, command: "/usr/bin/claude"},
	}, map[int]string{30: "/a", 10: "/b", 40: "/d"})()
	stub := cwdsOf
	var asked [][]int
	cwdsOf = func(pids []int) []cwd {
		asked = append(asked, slices.Clone(pids))
		return stub(pids)
	}

	got, note := ProcessInstances([]int{20})
	if len(asked) != 1 || !slices.Equal(asked[0], []int{30, 10, 40}) {
		t.Fatalf("one question for every CLI rota did not start, got %v", asked)
	}
	if len(got) != 3 || got[0].Dir != "/a" || got[1].Dir != "/b" || got[2].Dir != "/d" || note != "" {
		t.Fatalf("each answer goes to its own process: %+v %q", got, note)
	}

	asked = nil
	listProcs = func() ([]proc, error) { return []proc{{pid: 11, command: "/usr/bin/vim"}}, nil }
	if got, note := ProcessInstances(nil); len(got) != 0 || note != "" || len(asked) != 0 {
		t.Fatalf("no CLI, no question: %+v %q %v", got, note, asked)
	}
}

// What one lsof printed for six pids on a Mac, the paths shortened: one that
// had exited, launchd (pid 1, root's and not readable), and directories with
// a space, a newline, a tab and accents in their names. lsof left the first
// two out and exited 1, and escaped the newline and the tab, so a line of its
// output is never part of a path. The record for 4242 names no path; lsof was
// not seen to print one, and it is read as a single lsof would have been.
func TestLsofIsReadByProcessRecord(t *testing.T) {
	const printed = "p36732\nfcwd\nn/Users/me/src/with space\n" +
		"p36733\nfcwd\nn/Users/me/src/new\\nline\n" +
		"p36734\nfcwd\nn/Users/me/src/tab\\tx\n" +
		"p36735\nfcwd\nn/Users/me/src/\u00fcn\u00ef\n" +
		"p4242\nfcwd\nn(unknown)\n"
	exit1 := errors.New("exit status 1")
	pids := []int{36735, 999999, 36732, 1, 36733, 4242, 36734}
	var lists []string
	got := make([]cwd, len(pids))
	lsofCwds(pids, got, func(list string) ([]byte, error) {
		lists = append(lists, list)
		return []byte(printed), exit1
	})
	if len(lists) != 1 || lists[0] != "36735,999999,36732,1,36733,4242,36734" {
		t.Fatalf("one lsof for every pid: %q", lists)
	}
	want := []cwd{
		{dir: "/Users/me/src/\u00fcn\u00ef"},
		{err: exit1},
		{dir: "/Users/me/src/with space"},
		{err: exit1},
		{dir: `/Users/me/src/new\nline`},
		{err: os.ErrNotExist},
		{dir: `/Users/me/src/tab\tx`},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("each pid is read from its own record:\n got %+v\nwant %+v", got, want)
	}

	// Should lsof exit 0 and still leave a pid out, which it was not seen to
	// do, that pid is not found rather than given anything lsof did not say.
	lsofCwds([]int{36732, 7}, got[:2], func(string) ([]byte, error) { return []byte(printed), nil })
	if got[0].dir != "/Users/me/src/with space" || got[1].err != os.ErrNotExist {
		t.Fatalf("%+v", got[:2])
	}

	// Without lsof at all, nothing is known and every pid says why.
	gone := errors.New(`exec: "lsof": executable file not found in $PATH`)
	lsofCwds([]int{36732, 36733}, got[:2], func(string) ([]byte, error) { return nil, gone })
	if got[0].err != gone || got[1].err != gone {
		t.Fatalf("%+v", got[:2])
	}
}

// However many CLIs are open, one lsof's argument stays short: the pids go in
// chunks, and every pid is answered from the chunk it went in.
func TestLsofIsAskedInChunks(t *testing.T) {
	pids := make([]int, 2*lsofChunk+3)
	for i := range pids {
		pids[i] = 90000 + i
	}
	var calls int
	got := make([]cwd, len(pids))
	lsofCwds(pids, got, func(list string) ([]byte, error) {
		calls++
		ids := strings.Split(list, ",")
		if len(ids) > lsofChunk {
			t.Fatalf("%d pids in one lsof", len(ids))
		}
		// Only the even pids are still there.
		var b strings.Builder
		for _, id := range ids {
			if n, _ := strconv.Atoi(id); n%2 == 0 {
				b.WriteString("p" + id + "\nfcwd\nn/w/" + id + "\n")
			}
		}
		return []byte(b.String()), errors.New("exit status 1")
	})
	if calls != 3 {
		t.Fatalf("%d pids take three lsofs, took %d", len(pids), calls)
	}
	for i, pid := range pids {
		if pid%2 == 0 && got[i].dir != "/w/"+strconv.Itoa(pid) {
			t.Fatalf("pid %d: %+v", pid, got[i])
		}
		if pid%2 == 1 && (got[i].dir != "" || got[i].err == nil) {
			t.Fatalf("pid %d is gone: %+v", pid, got[i])
		}
	}
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
