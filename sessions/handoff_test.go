package sessions

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// A conversation follows the rotation: when the account asked to resume a
// session does not have it, but a sibling account of the same provider
// does, the transcript is copied into the target's home before the CLI
// launches. Credentials never move — only the conversation.
func TestResumeCopiesTheTranscriptFromASibling(t *testing.T) {
	s, dir := seedStore(t, `{"accounts":[
		{"id":1,"provider":"codex","uuid":"c1","order":1},
		{"id":3,"provider":"codex","uuid":"c2","order":2}],"nextId":4,"ordered":true}`)

	id := "01a00000-0000-7000-8000-00000000cafe"
	rel := filepath.Join("sessions", "2026", "09", "02", "rollout-2026-09-02T10-00-00-"+id+".jsonl")
	writeLines(t, filepath.Join(dir, "homes", "codex-1", rel),
		`{"type":"session_meta","payload":{"session_id":"`+id+`","cwd":"/tmp/x"}}`)

	if err := CopyForResume(s, s.Find(3), id); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(dir, "homes", "codex-3", rel)
	raw, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("the transcript must land at the same relative path: %v", err)
	}
	if !strings.Contains(string(raw), id) {
		t.Fatalf("copied whole: %s", raw)
	}
	// The source stays: a copy, not a move.
	if _, err := os.Stat(filepath.Join(dir, "homes", "codex-1", rel)); err != nil {
		t.Fatal("the source transcript must survive")
	}
}

// grok's session is a directory, and the whole directory travels.
func TestResumeCopiesAGrokSessionDirectory(t *testing.T) {
	s, dir := seedStore(t, `{"accounts":[
		{"id":8,"provider":"grok","uuid":"g1","order":1},
		{"id":9,"provider":"grok","uuid":"g2","order":2}],"nextId":10,"ordered":true}`)

	id := "01a00000-0000-7000-8000-0000000000fe"
	rel := filepath.Join("sessions", "%2Ftmp%2Fx", id)
	writeLines(t, filepath.Join(dir, "homes", "grok-8", rel, "summary.json"),
		`{"info":{"id":"`+id+`","cwd":"/tmp/x"}}`)
	writeLines(t, filepath.Join(dir, "homes", "grok-8", rel, "chat_history.jsonl"), `{"role":"user"}`)

	if err := CopyForResume(s, s.Find(9), id); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"summary.json", "chat_history.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, "homes", "grok-9", rel, f)); err != nil {
			t.Fatalf("%s must travel with the session: %v", f, err)
		}
	}
}

// Already present, absent everywhere, or a provider with no readable
// sessions: all quiet no-ops — the CLI is the judge of what resumes.
func TestResumeHandoffIsQuietWhenThereIsNothingToDo(t *testing.T) {
	s, dir := seedStore(t, `{"accounts":[
		{"id":1,"provider":"codex","uuid":"c1","order":1},
		{"id":2,"provider":"kimi","uuid":"k1","order":2}],"nextId":3,"ordered":true}`)

	if err := CopyForResume(s, s.Find(1), "01a00000-0000-7000-8000-000000000404"); err != nil {
		t.Fatalf("absent everywhere is not an error: %v", err)
	}
	if err := CopyForResume(s, s.Find(2), "anything"); err != nil {
		t.Fatalf("kimi has no readable sessions and stays quiet: %v", err)
	}

	// Present in the target already: nothing is copied over it.
	id := "01a00000-0000-7000-8000-000000000abc"
	rel := filepath.Join("sessions", "2026", "09", "02", "rollout-2026-09-02T10-00-00-"+id+".jsonl")
	writeLines(t, filepath.Join(dir, "homes", "codex-1", rel), `{"type":"session_meta","payload":{"session_id":"`+id+`","cwd":"/a"}}`)
	before, _ := os.Stat(filepath.Join(dir, "homes", "codex-1", rel))
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(dir, "homes", "codex-1", rel))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("a session already home must be left untouched")
	}
}

// handoffPair is two claude accounts that keep their conversations to
// themselves, each in its own home: a conversation moved between them is a
// copy in each.
const handoffPair = `{"accounts":[
	{"id":1,"provider":"claude","uuid":"a1","order":1,"sessions":"own"},
	{"id":2,"provider":"claude","uuid":"a2","order":2,"sessions":"own"}],"nextId":3,"ordered":true}`

// handoffClaudes is n claude accounts like handoffPair's, ids 1 to n.
func handoffClaudes(n int) string {
	var accounts []string
	for i := 1; i <= n; i++ {
		accounts = append(accounts, fmt.Sprintf(`{"id":%d,"provider":"claude","uuid":"a%d","order":%d,"sessions":"own"}`, i, i, i))
	}
	return fmt.Sprintf(`{"accounts":[%s],"nextId":%d,"ordered":true}`, strings.Join(accounts, ","), n+1)
}

// handoffClaude is where one conversation is in a claude account's home.
func handoffClaude(dir string, account int, id string) string {
	return filepath.Join(dir, "homes", "claude-"+strconv.Itoa(account), "projects", "-tmp-x", id+".jsonl")
}

// handoffAt sets when a file or folder was last written, to the second, so
// every filesystem keeps it exactly.
func handoffAt(t *testing.T, path string, at time.Time) {
	t.Helper()
	at = at.Truncate(time.Second)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func handoffStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func handoffRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// handoffAdd appends lines to a file, as a CLI adds turns.
func handoffAdd(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// handoffUntouched fails when the copy at path is no longer the file it was,
// or has been written since.
func handoffUntouched(t *testing.T, path string, before os.FileInfo, body string) {
	t.Helper()
	after := handoffStat(t, path)
	if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("%s was replaced or written: %v then %v", path, before.ModTime(), after.ModTime())
	}
	if got := handoffRead(t, path); got != body {
		t.Fatalf("%s changed:\n%s\nwant\n%s", path, got, body)
	}
}

// handoffWarnings collects what the store is warned of.
func handoffWarnings(s *store.Store) *[]string {
	var got []string
	s.Warn = func(msg string) { got = append(got, msg) }
	return &got
}

// handoffNoWarning fails when the store was warned of anything.
func handoffNoWarning(t *testing.T, warned *[]string) {
	t.Helper()
	if len(*warned) != 0 {
		t.Fatalf("nothing forked, and the store was warned: %q", *warned)
	}
}

// handoffForkSaid fails unless the store was warned once, of a fork between
// the target and the sibling, in the conversation's short id.
func handoffForkSaid(t *testing.T, warned *[]string, id string, target, sibling *rota.Account) {
	t.Helper()
	if len(*warned) != 1 {
		t.Fatalf("a fork is said once; the store was warned %d times: %q", len(*warned), *warned)
	}
	msg := (*warned)[0]
	for _, want := range []string{id[:8], target.String(), sibling.String(), "separate ways"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the warning %q does not say %q", msg, want)
		}
	}
}

// handoffSymlink links new to old, or skips a test on a system where a test
// may not make links.
func handoffSymlink(t *testing.T, old, new string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(new), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, new); err != nil {
		t.Skipf("links cannot be made here: %v", err)
	}
}

// A chat moved from A to B and back again resumes on A with what was said
// on B. B's copy is newer than A's and extends it, A's bytes followed by what
// was said on B, so what B has more is added to A's own file, which stays
// the same file, at A's own path, with nothing else left behind. Moved back
// with nothing said on B, A's copy is left alone: B's is newer, being a copy
// made later, but holds nothing more.
func TestAChatMovedBackKeepsWhatWasSaidElsewhere(t *testing.T) {
	s, dir := seedStore(t, handoffPair)
	warned := handoffWarnings(s)
	id := "01a00000-0000-7000-8000-00000000b0b0"
	a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
	first := `{"type":"user","sessionId":"` + id + `","cwd":"/tmp/x","message":{"content":"on A"}}`
	writeLines(t, a, first)
	handoffAt(t, a, time.Now().Add(-time.Hour))

	if err := CopyForResume(s, s.Find(2), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffRead(t, b); got != first+"\n" {
		t.Fatalf("B holds:\n%s", got)
	}
	before := handoffStat(t, a)
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	handoffUntouched(t, a, before, first+"\n")

	said := `{"type":"user","sessionId":"` + id + `","message":{"content":"said on B"}}`
	handoffAdd(t, b, said)
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffRead(t, a); got != first+"\n"+said+"\n" {
		t.Fatalf("A resumes without what was said on B:\n%s", got)
	}
	if !os.SameFile(before, handoffStat(t, a)) {
		t.Fatal("A's transcript must stay the same file, extended in place")
	}
	entries, err := os.ReadDir(filepath.Dir(a))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("A's project holds %d entries, want only the conversation: %v", len(entries), entries)
	}
	handoffNoWarning(t, warned)
}

// A CLI still running the conversation on the target, holding its transcript
// open, goes on writing to it: the transcript is extended where it is, never
// swapped for a new file under the CLI. Its mode is the same as before.
func TestACLIStillWritingToTheTargetLosesNothing(t *testing.T) {
	s, dir := seedStore(t, handoffPair)
	id := "01a00000-0000-7000-8000-00000000c11e"
	a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
	first, onB, later := `{"type":"user","message":{"content":"on A"}}`, `{"type":"user","message":{"content":"on B"}}`, `{"type":"user","message":{"content":"on A, still running"}}`
	writeLines(t, a, first)
	writeLines(t, b, first, onB)
	if err := os.Chmod(a, 0o644); err != nil {
		t.Fatal(err)
	}
	handoffAt(t, a, time.Now().Add(-time.Hour))
	live, err := os.OpenFile(a, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	if _, err := live.WriteString(later + "\n"); err != nil {
		t.Fatal(err)
	}
	if got, want := handoffRead(t, a), first+"\n"+onB+"\n"+later+"\n"; got != want {
		t.Fatalf("A's transcript lost the running CLI's turn:\n%s\nwant\n%s", got, want)
	}
	if runtime.GOOS != "windows" {
		if mode := handoffStat(t, a).Mode().Perm(); mode != 0o644 {
			t.Fatalf("A's transcript is now %v, want the 0644 it had", mode)
		}
	}
}

// A sibling's copy written before the target's leaves the target's alone,
// even one that extends it: what the target dropped since is not brought
// back.
func TestAnOlderSiblingCopyLeavesTheTargetsAlone(t *testing.T) {
	s, dir := seedStore(t, handoffPair)
	warned := handoffWarnings(s)
	id := "01a00000-0000-7000-8000-00000000a1d0"
	a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
	writeLines(t, a, `{"type":"user","message":{"content":"on A"}}`)
	writeLines(t, b, `{"type":"user","message":{"content":"on A"}}`, `{"type":"assistant"}`, `{"type":"user"}`)
	now := time.Now()
	handoffAt(t, a, now)
	handoffAt(t, b, now.Add(-time.Hour))

	before, body := handoffStat(t, a), handoffRead(t, a)
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	handoffUntouched(t, a, before, body)
	handoffNoWarning(t, warned)
}

// A sibling's copy written after the target's but holding nothing more leaves
// the target's alone, and is no fork: one that holds less, one that holds the
// same, and one that holds only half a record more.
func TestANewerCopyWithNothingMoreLeavesTheTargetsAlone(t *testing.T) {
	first, second := `{"type":"user","message":{"content":"one"}}`, `{"type":"assistant","message":{"content":"two"}}`
	for name, theirs := range map[string]string{
		"less":          first + "\n",
		"the same":      first + "\n" + second + "\n",
		"half a record": first + "\n" + second + "\n" + `{"type":"user","mess`,
	} {
		t.Run(name, func(t *testing.T) {
			s, dir := seedStore(t, handoffPair)
			warned := handoffWarnings(s)
			id := "01a00000-0000-7000-8000-00000000babe"
			a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
			writeLines(t, a, first, second)
			writeLines(t, b)
			if err := os.WriteFile(b, []byte(theirs), 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			handoffAt(t, a, now.Add(-time.Hour))
			handoffAt(t, b, now)

			before, body := handoffStat(t, a), handoffRead(t, a)
			if err := CopyForResume(s, s.Find(1), id); err != nil {
				t.Fatal(err)
			}
			handoffUntouched(t, a, before, body)
			handoffNoWarning(t, warned)
		})
	}
}

// Two copies written at the same moment: the one that holds more is the
// newer. A smaller one leaves the target's alone; a larger one that extends
// it gives it what it has more.
func TestSizeBreaksATie(t *testing.T) {
	s, dir := seedStore(t, handoffPair)
	id := "01a00000-0000-7000-8000-00000000713e"
	a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
	at := time.Now().Add(-time.Minute)
	writeLines(t, a, `{"type":"user"}`, `{"type":"assistant"}`)
	writeLines(t, b, `{"type":"user"}`)
	handoffAt(t, a, at)
	handoffAt(t, b, at)

	before, body := handoffStat(t, a), handoffRead(t, a)
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	handoffUntouched(t, a, before, body)

	writeLines(t, b, `{"type":"user"}`, `{"type":"assistant"}`, `{"type":"user","message":{"content":"more"}}`)
	handoffAt(t, b, at)
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffRead(t, a); got != handoffRead(t, b) {
		t.Fatalf("the larger copy must win a tie, A holds:\n%s", got)
	}
}

// When the target holds no copy, the newest of its siblings' is the one
// brought in, wherever its account stands in the order.
func TestTheNewestSiblingCopyIsTheOneBroughtIn(t *testing.T) {
	s, dir := seedStore(t, handoffClaudes(4))
	id := "01a00000-0000-7000-8000-00000000c3c3"
	now := time.Now()
	for account, age := range map[int]time.Duration{1: 3 * time.Hour, 2: 0, 3: time.Hour} {
		path := handoffClaude(dir, account, id)
		writeLines(t, path, `{"type":"user","message":{"content":"on `+strconv.Itoa(account)+`"}}`)
		handoffAt(t, path, now.Add(-age))
	}

	if err := CopyForResume(s, s.Find(4), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffRead(t, handoffClaude(dir, 4, id)); got != handoffRead(t, handoffClaude(dir, 2, id)) {
		t.Fatalf("4 must resume 2's newest copy, holds:\n%s", got)
	}
}

// Of copies equally new, the first account's in the order is brought in.
func TestOfCopiesEquallyNewTheFirstAccountsIsTaken(t *testing.T) {
	s, dir := seedStore(t, handoffClaudes(3))
	id := "01a00000-0000-7000-8000-00000000e9e9"
	at := time.Now().Add(-time.Hour)
	for _, account := range []int{1, 2} {
		path := handoffClaude(dir, account, id)
		writeLines(t, path, `{"type":"user","message":{"content":"on `+strconv.Itoa(account)+`"}}`)
		handoffAt(t, path, at)
	}

	if err := CopyForResume(s, s.Find(3), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffRead(t, handoffClaude(dir, 3, id)); got != handoffRead(t, handoffClaude(dir, 1, id)) {
		t.Fatalf("3 must resume 1's copy, the first of two equally new, holds:\n%s", got)
	}
}

// A conversation taken on separately on two accounts has forked: each copy
// holds turns the other lacks. The target's is kept exactly as it was, so
// nothing said on it is lost, however new the other copy is, and the store
// is told where the rest was said.
func TestAForkedConversationIsLeftAsItWas(t *testing.T) {
	s, dir := seedStore(t, handoffPair)
	warned := handoffWarnings(s)
	id := "01a00000-0000-7000-8000-00000000f0f0"
	a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
	first := `{"type":"user","message":{"content":"on A"}}`
	writeLines(t, a, first)
	if err := CopyForResume(s, s.Find(2), id); err != nil {
		t.Fatal(err)
	}
	handoffAdd(t, a, `{"type":"user","message":{"content":"then on A"}}`)
	handoffAdd(t, b, `{"type":"user","message":{"content":"then, differently and at more length, on B"}}`)
	now := time.Now()
	handoffAt(t, a, now.Add(-time.Hour))
	handoffAt(t, b, now)

	before, body := handoffStat(t, a), handoffRead(t, a)
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatalf("a fork is not a failed copy: %v", err)
	}
	handoffUntouched(t, a, before, body)
	handoffForkSaid(t, warned, id, s.Find(1), s.Find(2))
}

// Of the copies newer than the target's, one that extends it is taken over
// a newer one that has forked from it: the target gains turns and loses none.
// The fork is still said.
func TestACopyThatExtendsTheTargetsWinsOverANewerFork(t *testing.T) {
	s, dir := seedStore(t, handoffClaudes(3))
	warned := handoffWarnings(s)
	id := "01a00000-0000-7000-8000-00000000e7e7"
	a, b, c := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id), handoffClaude(dir, 3, id)
	base := `{"type":"user","message":{"content":"base"}}`
	writeLines(t, a, base, `{"type":"user","message":{"content":"on A"}}`)
	writeLines(t, b, base, `{"type":"user","message":{"content":"on A"}}`, `{"type":"user","message":{"content":"then on B"}}`)
	writeLines(t, c, base, `{"type":"user","message":{"content":"on C instead"}}`)
	now := time.Now()
	handoffAt(t, a, now.Add(-2*time.Hour))
	handoffAt(t, b, now.Add(-time.Hour))
	handoffAt(t, c, now)

	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffRead(t, a); got != handoffRead(t, b) {
		t.Fatalf("A must take B's copy, which extends its own; holds:\n%s", got)
	}
	handoffForkSaid(t, warned, id, s.Find(1), s.Find(3))
}

// The target's copy is extended where it is, even when it is filed under
// another project folder than the sibling's: nothing is written at the
// sibling's path in the target's home.
func TestTheTargetsOwnPathIsTheOneExtended(t *testing.T) {
	s, dir := seedStore(t, handoffPair)
	id := "01a00000-0000-7000-8000-00000000d1d1"
	a := filepath.Join(dir, "homes", "claude-1", "projects", "-tmp-y", id+".jsonl")
	b := handoffClaude(dir, 2, id)
	writeLines(t, a, `{"type":"user"}`)
	writeLines(t, b, `{"type":"user"}`, `{"type":"assistant"}`)
	handoffAt(t, a, time.Now().Add(-time.Hour))

	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffRead(t, a); got != handoffRead(t, b) {
		t.Fatalf("A's own copy must be extended, holds:\n%s", got)
	}
	if _, err := os.Lstat(handoffClaude(dir, 1, id)); !os.IsNotExist(err) {
		t.Fatalf("nothing may be written at the sibling's path in A's home: %v", err)
	}
}

// A log is copied to its last complete line, never half a record: a sibling's
// CLI part-way through writing one gives only what it had finished, and the
// rest comes on the next resume once the record is whole. A record longer
// than a read is found from its end all the same.
func TestALogIsCopiedToItsLastCompleteLine(t *testing.T) {
	first, rest := `{"type":"user","message":{"content":"one"}}`, `{"type":"assistant","message":{"content":"two"}}`
	t.Run("then finished", func(t *testing.T) {
		s, dir := seedStore(t, handoffPair)
		id := "01a00000-0000-7000-8000-00000000aaaa"
		a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
		writeLines(t, b, first)
		half := rest[:len(rest)/2]
		f, err := os.OpenFile(b, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(half); err != nil {
			t.Fatal(err)
		}
		if err := CopyForResume(s, s.Find(1), id); err != nil {
			t.Fatal(err)
		}
		if got := handoffRead(t, a); got != first+"\n" {
			t.Fatalf("A must hold the finished records only:\n%s", got)
		}
		if _, err := f.WriteString(rest[len(half):] + "\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		handoffAt(t, a, now.Add(-time.Hour))
		handoffAt(t, b, now)
		if err := CopyForResume(s, s.Find(1), id); err != nil {
			t.Fatal(err)
		}
		if got := handoffRead(t, a); got != first+"\n"+rest+"\n" {
			t.Fatalf("A must hold the record once it is whole:\n%s", got)
		}
	})
	t.Run("a long last record", func(t *testing.T) {
		s, dir := seedStore(t, handoffPair)
		id := "01a00000-0000-7000-8000-00000000bbbb"
		a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
		writeLines(t, b, first)
		f, err := os.OpenFile(b, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(`{"type":"user","message":{"content":"` + strings.Repeat("x", 200<<10)); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := CopyForResume(s, s.Find(1), id); err != nil {
			t.Fatal(err)
		}
		if got := handoffRead(t, a); got != first+"\n" {
			t.Fatalf("A must hold the finished records only, %d bytes", len(got))
		}
	})
}

// A copy cut short leaves nothing half done. A new copy is whole or absent,
// with no temporary file left behind. An extension cut short leaves the
// target a longer start of the copy it was extended from, newer now than that
// copy but ending part-way through a record, and the next resume finishes it.
func TestACopyCutShortIsFinishedByTheNextResume(t *testing.T) {
	s, dir := seedStore(t, handoffPair)
	id := "01a00000-0000-7000-8000-00000000cccc"
	a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
	first, more := `{"type":"user","message":{"content":"on A"}}`, `{"type":"user","message":{"content":"on B, at some length"}}`
	writeLines(t, b, first)
	cut := errors.New("cut short")
	handoffCopy = func(w io.Writer, r io.Reader) (int64, error) {
		n, _ := io.CopyN(w, r, 5)
		return n, cut
	}
	t.Cleanup(func() { handoffCopy = io.Copy })

	if err := CopyForResume(s, s.Find(1), id); !errors.Is(err, cut) {
		t.Fatalf("a copy cut short is an error, got %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(a))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a new copy cut short leaves nothing, found %v", entries)
	}

	handoffCopy = io.Copy
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	handoffAdd(t, b, more)
	now := time.Now()
	handoffAt(t, a, now.Add(-time.Hour))
	handoffAt(t, b, now.Add(-30*time.Minute))
	handoffCopy = func(w io.Writer, r io.Reader) (int64, error) {
		n, _ := io.CopyN(w, r, 5)
		return n, cut
	}
	if err := CopyForResume(s, s.Find(1), id); !errors.Is(err, cut) {
		t.Fatalf("an extension cut short is an error, got %v", err)
	}
	got := handoffRead(t, a)
	if !strings.HasPrefix(handoffRead(t, b), got) || len(got) != len(first)+1+5 {
		t.Fatalf("A must be a longer start of B's copy:\n%s", got)
	}

	handoffCopy = io.Copy
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffRead(t, a); got != handoffRead(t, b) {
		t.Fatalf("the next resume must finish the copy, A holds:\n%s", got)
	}
}

// Whatever is already at the target's path is left as it is, though the
// target's own listing did not see it there: a transcript in a folder the
// person linked into the home, or the sibling's own, reached through a link.
func TestNothingAlreadyAtTheTargetPathIsReplaced(t *testing.T) {
	first, second := `{"type":"user","message":{"content":"one"}}`, `{"type":"assistant","message":{"content":"two"}}`
	t.Run("a linked folder", func(t *testing.T) {
		s, dir := seedStore(t, handoffPair)
		id := "01a00000-0000-7000-8000-00000000dddd"
		elsewhere := filepath.Join(t.TempDir(), "-repo", id+".jsonl")
		writeLines(t, elsewhere, first, second)
		handoffSymlink(t, filepath.Dir(elsewhere), filepath.Dir(handoffClaude(dir, 1, id)))
		writeLines(t, handoffClaude(dir, 2, id), first)

		before, body := handoffStat(t, elsewhere), handoffRead(t, elsewhere)
		if err := CopyForResume(s, s.Find(1), id); err != nil {
			t.Fatal(err)
		}
		handoffUntouched(t, elsewhere, before, body)
	})
	t.Run("the sibling's own", func(t *testing.T) {
		s, dir := seedStore(t, handoffPair)
		id := "01a00000-0000-7000-8000-00000000eeee"
		b := handoffClaude(dir, 2, id)
		writeLines(t, b, first)
		handoffSymlink(t, filepath.Dir(b), filepath.Dir(handoffClaude(dir, 1, id)))

		before, body := handoffStat(t, b), handoffRead(t, b)
		if err := CopyForResume(s, s.Find(1), id); err != nil {
			t.Fatal(err)
		}
		handoffUntouched(t, b, before, body)
	})
	t.Run("a linked grok folder", func(t *testing.T) {
		id := "01a00000-0000-7000-8000-0000000009f9"
		s, target, sibling := handoffGrokPair(t, id)
		elsewhere := filepath.Join(t.TempDir(), id)
		handoffGrokWrite(t, elsewhere, time.Now(), map[string][]string{
			"summary.json":       handoffSummary(id, "2026-08-28T14:30:48.935066Z"),
			"chat_history.jsonl": {`{"role":"user"}`, `{"role":"assistant"}`},
		})
		handoffSymlink(t, elsewhere, target)
		handoffGrokWrite(t, sibling, time.Now(), map[string][]string{
			"summary.json":       handoffSummary(id, "2026-08-29T09:12:03.120500Z"),
			"chat_history.jsonl": {`{"role":"user"}`},
			"plan.md":            {"# plan"},
		})

		if err := CopyForResume(s, s.Find(9), id); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(elsewhere)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("the linked folder now holds %v, want its own two files", entries)
		}
	})
	t.Run("one that appears while the copy is written", func(t *testing.T) {
		s, dir := seedStore(t, handoffPair)
		id := "01a00000-0000-7000-8000-00000000abab"
		a := handoffClaude(dir, 1, id)
		writeLines(t, handoffClaude(dir, 2, id), first)
		handoffCopy = func(w io.Writer, r io.Reader) (int64, error) {
			writeLines(t, a, first, second)
			return io.Copy(w, r)
		}
		t.Cleanup(func() { handoffCopy = io.Copy })

		if err := CopyForResume(s, s.Find(1), id); err != nil {
			t.Fatal(err)
		}
		if got := handoffRead(t, a); got != first+"\n"+second+"\n" {
			t.Fatalf("a transcript that appeared meanwhile must be kept, holds:\n%s", got)
		}
	})
}

// handoffGrokPair is two grok accounts, and the folder one conversation is
// in, in the home of each: the target's (account 9) and the sibling's (8).
func handoffGrokPair(t *testing.T, id string) (s *store.Store, target, sibling string) {
	t.Helper()
	s, dir := seedStore(t, `{"accounts":[
		{"id":8,"provider":"grok","uuid":"g1","order":1},
		{"id":9,"provider":"grok","uuid":"g2","order":2}],"nextId":10,"ordered":true}`)
	rel := filepath.Join("sessions", "%2Ftmp%2Fx", id)
	return s, filepath.Join(dir, "homes", "grok-9", rel), filepath.Join(dir, "homes", "grok-8", rel)
}

// handoffGrokWrite writes a grok conversation's files, every one last
// written at the given time.
func handoffGrokWrite(t *testing.T, folder string, at time.Time, files map[string][]string) {
	t.Helper()
	for name, lines := range files {
		writeLines(t, filepath.Join(folder, name), lines...)
		handoffAt(t, filepath.Join(folder, name), at)
	}
}

// handoffSummary is grok's summary.json as it is on disk: pretty-printed,
// and written again whole on every turn, with the time of that turn.
func handoffSummary(id, updated string) []string {
	return []string{
		`{`,
		`  "info": {`,
		`    "id": "` + id + `",`,
		`    "cwd": "/tmp/x"`,
		`  },`,
		`  "session_summary": "hi",`,
		`  "created_at": "2026-08-28T14:30:32.762108Z",`,
		`  "updated_at": "` + updated + `"`,
		`}`,
	}
}

// A grok conversation is a folder, measured by its newest file. A newer one
// whose every log extends the target's gives each log what it has more, in
// place, and its other files come along: the summary, written again whole on
// every turn and so never an extension of the old one, and a file the target
// had not.
func TestANewerGrokCopyExtendsTheFolder(t *testing.T) {
	id := "01a00000-0000-7000-8000-0000000009fe"
	s, target, sibling := handoffGrokPair(t, id)
	warned := handoffWarnings(s)
	now := time.Now()
	handoffGrokWrite(t, target, now.Add(-time.Hour), map[string][]string{
		"summary.json":       handoffSummary(id, "2026-08-28T14:30:48.935066Z"),
		"chat_history.jsonl": {`{"role":"user"}`},
	})
	handoffGrokWrite(t, sibling, now, map[string][]string{
		"summary.json":       handoffSummary(id, "2026-08-29T09:12:03.120500Z"),
		"chat_history.jsonl": {`{"role":"user"}`, `{"role":"assistant"}`},
		"plan.md":            {"# plan"},
	})
	history := handoffStat(t, filepath.Join(target, "chat_history.jsonl"))

	if err := CopyForResume(s, s.Find(9), id); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"summary.json", "chat_history.jsonl", "plan.md"} {
		if got, want := handoffRead(t, filepath.Join(target, f)), handoffRead(t, filepath.Join(sibling, f)); got != want {
			t.Fatalf("%s must be the newer copy's:\n%s", f, got)
		}
	}
	if !os.SameFile(history, handoffStat(t, filepath.Join(target, "chat_history.jsonl"))) {
		t.Fatal("the history must be extended in place")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("the folder holds %d entries, want the newer copy's three: %v", len(entries), entries)
	}
	handoffNoWarning(t, warned)
}

// A grok folder is measured by its files, not by the folder's own time, which
// changes only when an entry comes or goes.
func TestAGrokFolderIsMeasuredByItsFiles(t *testing.T) {
	id := "01a00000-0000-7000-8000-0000000009fd"
	s, target, sibling := handoffGrokPair(t, id)
	now := time.Now()
	handoffGrokWrite(t, target, now.Add(-time.Hour), map[string][]string{
		"summary.json":       handoffSummary(id, "2026-08-28T14:30:48.935066Z"),
		"chat_history.jsonl": {`{"role":"user"}`},
	})
	handoffGrokWrite(t, sibling, now, map[string][]string{
		"summary.json":       handoffSummary(id, "2026-08-29T09:12:03.120500Z"),
		"chat_history.jsonl": {`{"role":"user"}`, `{"role":"assistant"}`},
	})
	handoffAt(t, target, now)
	handoffAt(t, sibling, now.Add(-2*time.Hour))

	if err := CopyForResume(s, s.Find(9), id); err != nil {
		t.Fatal(err)
	}
	if got, want := handoffRead(t, filepath.Join(target, "chat_history.jsonl")), handoffRead(t, filepath.Join(sibling, "chat_history.jsonl")); got != want {
		t.Fatalf("the newer files must extend the target's, whatever the folders' own times:\n%s", got)
	}
}

// A grok folder has forked when a log the target has is missing from the
// newer copy, or is not the start of the newer copy's: the target's folder
// is then kept exactly as it was, and the fork is said.
func TestAForkedGrokCopyIsLeftAsItWas(t *testing.T) {
	for name, c := range map[string]struct {
		target, sibling map[string][]string
	}{
		"a log only the target has": {
			target:  map[string][]string{"chat_history.jsonl": {`{"role":"user"}`}, "events.jsonl": {`{"event":"mine"}`}},
			sibling: map[string][]string{"chat_history.jsonl": {`{"role":"user"}`, `{"role":"assistant","content":"a long answer"}`}},
		},
		"a history that went its own way": {
			target:  map[string][]string{"chat_history.jsonl": {`{"role":"user"}`, `{"role":"user","content":"here"}`}},
			sibling: map[string][]string{"chat_history.jsonl": {`{"role":"user"}`, `{"role":"user","content":"there, and at more length"}`}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			id := "01a00000-0000-7000-8000-0000000009ff"
			s, target, sibling := handoffGrokPair(t, id)
			warned := handoffWarnings(s)
			c.target["summary.json"] = handoffSummary(id, "2026-08-28T14:30:48.935066Z")
			c.sibling["summary.json"] = handoffSummary(id, "2026-08-29T09:12:03.120500Z")
			now := time.Now()
			handoffGrokWrite(t, target, now.Add(-time.Hour), c.target)
			handoffGrokWrite(t, sibling, now, c.sibling)
			before := map[string]os.FileInfo{}
			bodies := map[string]string{}
			for f := range c.target {
				before[f], bodies[f] = handoffStat(t, filepath.Join(target, f)), handoffRead(t, filepath.Join(target, f))
			}

			if err := CopyForResume(s, s.Find(9), id); err != nil {
				t.Fatalf("a fork is not a failed copy: %v", err)
			}
			for f := range c.target {
				handoffUntouched(t, filepath.Join(target, f), before[f], bodies[f])
			}
			entries, err := os.ReadDir(target)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(c.target) {
				t.Fatalf("the kept folder holds %d entries, want its own %d: %v", len(entries), len(c.target), entries)
			}
			handoffForkSaid(t, warned, id, s.Find(9), s.Find(8))
		})
	}
}

// A grok log written to by the target's CLI after it was read is left as the
// CLI left it, while the rest of the folder is extended.
func TestATargetWrittenSinceItWasReadIsLeftAlone(t *testing.T) {
	id := "01a00000-0000-7000-8000-0000000009fc"
	s, target, sibling := handoffGrokPair(t, id)
	now := time.Now()
	handoffGrokWrite(t, target, now.Add(-time.Hour), map[string][]string{
		"summary.json":       handoffSummary(id, "2026-08-28T14:30:48.935066Z"),
		"chat_history.jsonl": {`{"role":"user"}`},
		"events.jsonl":       {`{"event":"one"}`},
	})
	handoffGrokWrite(t, sibling, now, map[string][]string{
		"summary.json":       handoffSummary(id, "2026-08-29T09:12:03.120500Z"),
		"chat_history.jsonl": {`{"role":"user"}`, `{"role":"assistant"}`},
		"events.jsonl":       {`{"event":"one"}`, `{"event":"two"}`},
	})
	events := filepath.Join(target, "events.jsonl")
	once := false
	handoffCopy = func(w io.Writer, r io.Reader) (int64, error) {
		if !once {
			once = true
			handoffAdd(t, events, `{"event":"the CLI's"}`)
		}
		return io.Copy(w, r)
	}
	t.Cleanup(func() { handoffCopy = io.Copy })

	if err := CopyForResume(s, s.Find(9), id); err != nil {
		t.Fatal(err)
	}
	if got, want := handoffRead(t, filepath.Join(target, "chat_history.jsonl")), handoffRead(t, filepath.Join(sibling, "chat_history.jsonl")); got != want {
		t.Fatalf("the history must be extended:\n%s", got)
	}
	if got, want := handoffRead(t, events), `{"event":"one"}`+"\n"+`{"event":"the CLI's"}`+"\n"; got != want {
		t.Fatalf("the log the CLI wrote to must be left as it left it:\n%s", got)
	}
}

// Only a conversation's own files travel: neither a temporary file a copy
// left behind nor a link. A leftover an hour old is removed where it is
// found; a younger one may be a copy still under way, and stays.
func TestOnlyAConversationsOwnFilesAreCopied(t *testing.T) {
	id := "01a00000-0000-7000-8000-0000000009fb"
	s, target, sibling := handoffGrokPair(t, id)
	now := time.Now()
	handoffGrokWrite(t, sibling, now, map[string][]string{
		"summary.json":       handoffSummary(id, "2026-08-29T09:12:03.120500Z"),
		"chat_history.jsonl": {`{"role":"user"}`},
		".rota-1.tmp":        {"young"},
		".rota-2.tmp":        {"old"},
	})
	handoffAt(t, filepath.Join(sibling, ".rota-2.tmp"), now.Add(-2*time.Hour))
	if err := os.Symlink(filepath.Join(sibling, "summary.json"), filepath.Join(sibling, "link.json")); err != nil && runtime.GOOS != "windows" {
		t.Fatal(err)
	}

	if err := CopyForResume(s, s.Find(9), id); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "chat_history.jsonl summary.json" {
		t.Fatalf("the copy holds %v, want the conversation's two files", names)
	}
	if _, err := os.Stat(filepath.Join(sibling, ".rota-2.tmp")); !os.IsNotExist(err) {
		t.Fatalf("a leftover an hour old must be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sibling, ".rota-1.tmp")); err != nil {
		t.Fatalf("a young temporary file must stay: %v", err)
	}
}

// A temporary file in the target's folder does not count: not as the
// newest file there, which would make the target look newer than the copy
// that extends it, and not as a file the newer copy lacks. One left in a
// folder a copy is written into is removed there once it is an hour old.
func TestLeftoverTempFilesAreNotCounted(t *testing.T) {
	t.Run("in a grok folder", func(t *testing.T) {
		id := "01a00000-0000-7000-8000-0000000009fa"
		s, target, sibling := handoffGrokPair(t, id)
		now := time.Now()
		handoffGrokWrite(t, target, now.Add(-time.Hour), map[string][]string{
			"summary.json":       handoffSummary(id, "2026-08-28T14:30:48.935066Z"),
			"chat_history.jsonl": {`{"role":"user"}`},
		})
		handoffGrokWrite(t, target, now, map[string][]string{".rota-3.tmp": {"a copy under way"}})
		handoffGrokWrite(t, sibling, now.Add(-30*time.Minute), map[string][]string{
			"summary.json":       handoffSummary(id, "2026-08-29T09:12:03.120500Z"),
			"chat_history.jsonl": {`{"role":"user"}`, `{"role":"assistant"}`},
		})

		if err := CopyForResume(s, s.Find(9), id); err != nil {
			t.Fatal(err)
		}
		if got, want := handoffRead(t, filepath.Join(target, "chat_history.jsonl")), handoffRead(t, filepath.Join(sibling, "chat_history.jsonl")); got != want {
			t.Fatalf("a temporary file must not make the target look newer:\n%s", got)
		}
	})
	t.Run("in a claude project", func(t *testing.T) {
		s, dir := seedStore(t, handoffPair)
		id := "01a00000-0000-7000-8000-00000000ffff"
		a := handoffClaude(dir, 1, id)
		old := filepath.Join(filepath.Dir(a), ".rota-4.tmp")
		writeLines(t, old, "left behind")
		handoffAt(t, old, time.Now().Add(-2*time.Hour))
		writeLines(t, handoffClaude(dir, 2, id), `{"type":"user"}`)

		if err := CopyForResume(s, s.Find(1), id); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatalf("a leftover an hour old must be removed: %v", err)
		}
	})
}

// Transcripts run to megabytes, and are compared in pieces: a difference far
// past the first piece still counts, and so does an older copy that runs on
// past the newer one's end. What the newer copy holds more is given to its
// last complete line.
func TestALongTranscriptIsComparedToItsEnd(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat(`{"type":"assistant","message":{"content":"a long answer"}}`+"\n", 5000)
	older, newer := filepath.Join(dir, "older.jsonl"), filepath.Join(dir, "newer.jsonl")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	more := `{"type":"user"}` + "\n"

	write(older, long)
	write(newer, long+more+`{"type":"ass`)
	if tail, _, how := handoffTail(newer, older); how != handoffAhead || string(tail) != more {
		t.Fatalf("a copy that goes on from the older one's last byte is ahead by %q, got %q (%d)", more, tail, how)
	}
	write(older, long[:len(long)-10]+"0123456789")
	if _, _, how := handoffTail(newer, older); how != handoffApart {
		t.Fatalf("a difference in the last bytes of a long copy is a fork, got %d", how)
	}
	write(older, long+more+`{"type":"assistant"}`+"\n")
	write(newer, long+more)
	if _, _, how := handoffTail(newer, older); how != handoffBehind {
		t.Fatalf("an older copy that runs on past the newer one's end is ahead of it, got %d", how)
	}
}
