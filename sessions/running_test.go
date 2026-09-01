package sessions

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// An editor with Claude Code open writes a lock file naming the workspace it
// has open and the process holding it. The same file carries the token that
// editor authenticates with, which rota has no business reading: it takes the
// three fields it needs and never touches the fourth.
func TestIDEInstancesComeFromTheLockFilesWithoutTheirToken(t *testing.T) {
	home := t.TempDir()
	lock := filepath.Join(home, "ide", "62232.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	const secret = "mo_testtoken0testtoken0testtoken0testtoken0testtoken0testtoken0000000"
	body := `{"workspaceFolders":["/Users/me/src/api"],"pid":` + itoa(os.Getpid()) +
		`,"ideName":"GoLand","transport":"ws","runningInWindows":false,"authToken":"` + secret + `"}`
	if err := os.WriteFile(lock, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got := IDEInstances(home)
	if len(got) != 1 {
		t.Fatalf("one editor, got %d: %+v", len(got), got)
	}
	if got[0].Kind != "GoLand" || got[0].Dir != "/Users/me/src/api" || got[0].PID != os.Getpid() {
		t.Fatalf("%+v", got[0])
	}
	// The token must not survive anywhere on the value rota passes around.
	blob, err := jsonv2.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), secret) || strings.Contains(string(blob), "authToken") {
		t.Fatalf("an editor's credential must never leave the lock file: %s", blob)
	}
}

// A lock left behind by an editor that has gone is not an instance. Nothing
// cleans these up but the editor itself, so a stale one would otherwise be
// reported as running forever.
func TestALockWhoseProcessIsGoneIsNotRunning(t *testing.T) {
	home := t.TempDir()
	lock := filepath.Join(home, "ide", "1.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	// A pid no process can have: the kernel refuses it, so it is never live.
	if err := os.WriteFile(lock, []byte(`{"workspaceFolders":["/x"],"pid":-5,"ideName":"GoLand"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := IDEInstances(home); len(got) != 0 {
		t.Fatalf("a stale lock is not a running editor: %+v", got)
	}
	// Neither is a lock rota cannot read.
	if err := os.WriteFile(lock, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := IDEInstances(home); len(got) != 0 {
		t.Fatalf("an unreadable lock is skipped, not guessed at: %+v", got)
	}
	// And a home with no ide directory at all is simply quiet.
	if got := IDEInstances(t.TempDir()); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// rota writes down what it launched so an instance can be traced back to the
// account paying for it, which nothing on disk would otherwise say: by
// default every Claude Code account reads the same ~/.claude.
func TestTheRegistryRemembersWhatRotaLaunched(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Dir: dir}

	run, err := r.Add(Instance{Account: 2, Label: "a@b.c", Provider: "claude",
		Dir: "/Users/me/src/api", PID: os.Getpid(), Session: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	got := r.Running()
	if len(got) != 1 {
		t.Fatalf("one run, got %d: %+v", len(got), got)
	}
	if got[0].Account != 2 || got[0].Dir != "/Users/me/src/api" || got[0].Session != "sess-1" {
		t.Fatalf("%+v", got[0])
	}
	if got[0].Since.IsZero() {
		t.Fatal("a run must say when it started")
	}

	// Ending it takes it off the list.
	if err := run.End(); err != nil {
		t.Fatal(err)
	}
	if got := r.Running(); len(got) != 0 {
		t.Fatalf("a finished run is not running: %+v", got)
	}
}

// A run that was killed never gets to clean up after itself, so the entry it
// left has to be dropped by whoever reads the file next. Otherwise one crash
// leaves a ghost in the list for good.
func TestARegistryEntryWhoseProcessDiedIsDropped(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Dir: dir}
	if _, err := r.Add(Instance{Account: 1, Provider: "claude", PID: -5, Dir: "/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(Instance{Account: 2, Provider: "claude", PID: os.Getpid(), Dir: "/y"}); err != nil {
		t.Fatal(err)
	}
	got := r.Running()
	if len(got) != 1 || got[0].Account != 2 {
		t.Fatalf("only the live one survives: %+v", got)
	}
	// And the dead entry is gone from the file, not just from this answer.
	blob, err := os.ReadFile(filepath.Join(dir, runningFile))
	if err != nil {
		t.Fatal(err)
	}
	var kept []Instance
	if err := jsonv2.Unmarshal(blob, &kept); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Fatalf("reading the list is what prunes it: %s", blob)
	}
}

// The file is written by every run and read by every list, so a shape it
// cannot parse must not take the whole command down with it.
func TestAnUnreadableRegistryIsEmptyRatherThanFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, runningFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Registry{Dir: dir}
	if got := r.Running(); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// itoa keeps the fixture readable without dragging strconv into the test.
func itoa(n int) string {
	var v jsontext.Value
	v, _ = jsonv2.Marshal(n)
	return string(v)
}

// A run does not know its conversation id until the CLI says so, which is
// after the entry describing the run has been written. Without a way to fill
// it in afterwards, "which session is running under which account" answers
// only half the question.
func TestARunLearnsItsSessionIDPartWayThrough(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Dir: dir}
	run, err := r.Add(Instance{Account: 2, Provider: "claude", Dir: "/x", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Running(); len(got) != 1 || got[0].Session != "" {
		t.Fatalf("nothing is known yet: %+v", got)
	}

	run.Learned("sess-42")
	got := r.Running()
	if len(got) != 1 || got[0].Session != "sess-42" {
		t.Fatalf("the conversation must reach the entry: %+v", got)
	}
	// And nothing else about the run was disturbed by learning it.
	if got[0].Account != 2 || got[0].Dir != "/x" || got[0].Since.IsZero() {
		t.Fatalf("%+v", got[0])
	}

	// The first id wins: a run has one conversation, and a later event
	// naming another would be about something else.
	run.Learned("sess-99")
	if got := r.Running(); got[0].Session != "sess-42" {
		t.Fatalf("%+v", got)
	}
	if err := run.End(); err != nil {
		t.Fatal(err)
	}
}

// Learning about a run that is not there changes nothing and says nothing.
// It happens: an entry may have been pruned while the run was in flight.
func TestLearningAboutARunThatIsGoneIsHarmless(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Dir: dir}
	(&Run{reg: r, pid: os.Getpid()}).Learned("sess-1")
	if got := r.Running(); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// Runs start at the same time. A server takes eight at once, and every one of
// them writes to the same small file.
//
// Reading it, appending, and writing it back is a lost update waiting to
// happen: two runs that read the same list both write a list missing the
// other. The file has to be held while it is changed.
func TestConcurrentRunsAllSurviveInTheRegistry(t *testing.T) {
	dir := t.TempDir()
	const runs = 24

	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each run has its own Registry value, the way separate requests
			// and separate processes do: nothing is shared but the file.
			r := &Registry{Dir: dir}
			if _, err := r.Add(Instance{
				Account: i + 1, Provider: "claude", Dir: "/x", PID: os.Getpid(),
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	got := (&Registry{Dir: dir}).Running()
	if len(got) != runs {
		t.Fatalf("every run must be recorded: %d of %d survived", len(got), runs)
	}
	seen := map[int]bool{}
	for _, in := range got {
		seen[in.Account] = true
	}
	for i := range runs {
		if !seen[i+1] {
			t.Fatalf("account %d was written and then lost: %+v", i+1, got)
		}
	}
}

// The same for taking entries off, which a finishing run does while others
// are still starting.
func TestConcurrentEndsLeaveExactlyWhatIsStillRunning(t *testing.T) {
	dir := t.TempDir()
	const runs = 16

	dones := make([]*Run, runs)
	for i := range runs {
		r := &Registry{Dir: dir}
		run, err := r.Add(Instance{Account: i + 1, Provider: "claude", PID: os.Getpid()})
		if err != nil {
			t.Fatal(err)
		}
		dones[i] = run
	}

	// Half of them finish at once.
	var wg sync.WaitGroup
	for i := 0; i < runs; i += 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := dones[i].End(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	got := (&Registry{Dir: dir}).Running()
	if len(got) != runs/2 {
		t.Fatalf("half finished, so half remain: %d of %d", len(got), runs/2)
	}
	for _, in := range got {
		if in.Account%2 == 1 {
			t.Fatalf("a finished run is still listed: %+v", got)
		}
	}
}

// A server runs several agents at once inside one process, so a process id
// does not name one of them. Two runs going together, each learning its own
// conversation, must not have them land on whichever entry was found first.
func TestTwoRunsInOneProcessKeepTheirOwnSessions(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Dir: dir}

	first, err := r.Add(Instance{Account: 1, Provider: "claude", Dir: "/a", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Add(Instance{Account: 2, Provider: "claude", Dir: "/b", PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	second.Learned("sess-second")
	first.Learned("sess-first")

	got := map[string]string{}
	for _, in := range r.Running() {
		got[in.Dir] = in.Session
	}
	if got["/a"] != "sess-first" || got["/b"] != "sess-second" {
		t.Fatalf("each run keeps its own conversation: %+v", got)
	}

	// And ending one leaves the other alone.
	if err := first.End(); err != nil {
		t.Fatal(err)
	}
	rest := r.Running()
	if len(rest) != 1 || rest[0].Dir != "/b" || rest[0].Session != "sess-second" {
		t.Fatalf("%+v", rest)
	}
}
