package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// No test starts a real watcher unless it means to: one started from here
// would be this test binary, run again with rota's arguments.
func init() { startWatcher = func(int) error { return nil } }

// watchers records the watchers a handover starts instead of starting them.
func watchers(t *testing.T, fail error) *[]int {
	t.Helper()
	orig := startWatcher
	t.Cleanup(func() { startWatcher = orig })
	var ids []int
	startWatcher = func(id int) error { ids = append(ids, id); return fail }
	return &ids
}

// An interactive run on a claude account whose login lives in its home leaves
// a watcher behind, and is handed over exactly as before. Windows keeps every
// account on a token in its environment, so there it leaves none.
func TestAnInteractiveRunOnAStoredLoginStartsAWatcher(t *testing.T) {
	seedLiving(t, "")
	started := watchers(t, nil)
	argv := handover(t)
	if _, errOut, code := call(t, "run", "1"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if len(*argv) == 0 || (*argv)[0] != "claude" {
		t.Fatalf("the CLI is still handed the terminal: %q", *argv)
	}
	want := 1
	if runtime.GOOS == "windows" {
		want = 0
	}
	if len(*started) != want || (want == 1 && (*started)[0] != 1) {
		t.Fatalf("watchers started: %v", *started)
	}
}

// A watcher that cannot be started is said, and the run goes on without it.
func TestARunWhoseWatcherCannotStartStillRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows starts no watcher")
	}
	seedLiving(t, "")
	watchers(t, errors.New("no such file"))
	argv := handover(t)
	_, errOut, code := call(t, "run", "1")
	if code != 0 || len(*argv) == 0 {
		t.Fatalf("%d %q %q", code, errOut, *argv)
	}
	if !strings.Contains(errOut, "watcher") || !strings.Contains(errOut, "no such file") {
		t.Fatalf("said: %q", errOut)
	}
}

// Nothing but an interactive run on the stored route starts one: not a
// question, and not an account on a token in a directory somebody chose.
func TestOnlyAnInteractiveRunOnTheStoredRouteStartsAWatcher(t *testing.T) {
	t.Run("a question", func(t *testing.T) {
		seedLiving(t, "")
		started := watchers(t, nil)
		call(t, "run", "1", "what is two and two")
		if len(*started) != 0 {
			t.Fatalf("started: %v", *started)
		}
	})
	t.Run("a directory of the person's choosing", func(t *testing.T) {
		dir := t.TempDir()
		seedLiving(t, `,"configDir":"`+filepath.ToSlash(dir)+`"`)
		started := watchers(t, nil)
		handover(t)
		if _, errOut, code := call(t, "run", "1"); code != 0 {
			t.Fatalf("%d %q", code, errOut)
		}
		if len(*started) != 0 {
			t.Fatalf("started: %v", *started)
		}
	})
}

// `rota serve watch` takes one account id and nothing else.
func TestServeWatchTakesOneAccountID(t *testing.T) {
	for _, args := range [][]string{{"serve", "watch"}, {"serve", "watch", "x"}, {"serve", "watch", "1", "2"}} {
		if _, _, code := call(t, args...); code != 2 {
			t.Fatalf("%q: %d", args, code)
		}
	}
}

// The watcher's log stays small: past its cap it is moved aside, once, and a
// new one begun.
func TestTheWatchersLogIsCapped(t *testing.T) {
	old := watchLogCap
	watchLogCap = 100
	t.Cleanup(func() { watchLogCap = old })
	path := filepath.Join(t.TempDir(), "watch.log")
	l := &watchLog{path: path}
	for i := 0; i < 20; i++ {
		l.say(strings.Repeat("x", 30))
	}
	l.say("the last word")
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prev, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cur), "the last word") || int64(len(cur)) > 2*watchLogCap || int64(len(prev)) > 2*watchLogCap {
		t.Fatalf("%d %d %q", len(cur), len(prev), cur)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 2 {
		t.Fatalf("one old file kept: %d", len(entries))
	}
}
