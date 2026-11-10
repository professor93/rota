package sessions

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// handoffClaude is where one conversation is in a claude account's home.
func handoffClaude(dir string, account int, id string) string {
	return filepath.Join(dir, "homes", "claude-"+strconv.Itoa(account), "projects", "-tmp-x", id+".jsonl")
}

// handoffAt sets when a copy was last written, to the second, so every
// filesystem keeps it exactly.
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

// A chat moved from A to B and back again resumes on A with what was said
// on B. B's copy is newer than A's and extends it, A's bytes followed by
// what was said on B, so it replaces A's at A's own path, leaving nothing
// else behind. Moved back with nothing said on B,
// though, A's copy is left alone: a copy keeps its source's time, so a copy
// made later is never taken for newer work.
func TestAChatMovedBackKeepsWhatWasSaidElsewhere(t *testing.T) {
	s, dir := seedStore(t, handoffPair)
	id := "01a00000-0000-7000-8000-00000000b0b0"
	a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
	first := `{"type":"user","sessionId":"` + id + `","cwd":"/tmp/x","message":{"content":"on A"}}`
	writeLines(t, a, first)
	handoffAt(t, a, time.Now().Add(-time.Hour))

	if err := CopyForResume(s, s.Find(2), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffStat(t, b).ModTime(); !got.Equal(handoffStat(t, a).ModTime()) {
		t.Fatalf("B's copy was written at %v; a copy keeps its source's time, %v", got, handoffStat(t, a).ModTime())
	}
	before := handoffStat(t, a)
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	handoffUntouched(t, a, before, first+"\n")

	said := `{"type":"user","sessionId":"` + id + `","message":{"content":"said on B"}}`
	f, err := os.OpenFile(b, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(said + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffRead(t, a); got != first+"\n"+said+"\n" {
		t.Fatalf("A resumes without what was said on B:\n%s", got)
	}
	entries, err := os.ReadDir(filepath.Dir(a))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("A's project holds %d entries, want only the conversation: %v", len(entries), entries)
	}
}

// A sibling's copy written before the target's leaves the target's alone,
// however much more it holds: size only breaks a tie.
func TestAnOlderSiblingCopyLeavesTheTargetsAlone(t *testing.T) {
	s, dir := seedStore(t, handoffPair)
	id := "01a00000-0000-7000-8000-00000000a1d0"
	a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
	writeLines(t, a, `{"type":"user","message":{"content":"on A, later"}}`)
	writeLines(t, b, `{"type":"user","message":{"content":"on B"}}`, `{"type":"assistant"}`, `{"type":"user"}`)
	now := time.Now()
	handoffAt(t, a, now)
	handoffAt(t, b, now.Add(-time.Hour))

	before, body := handoffStat(t, a), handoffRead(t, a)
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatal(err)
	}
	handoffUntouched(t, a, before, body)
}

// Two copies written at the same moment: the one that holds more is the
// newer. A smaller or equal one leaves the target's alone.
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
// brought in, not the first account's.
func TestTheNewestSiblingCopyIsTheOneBroughtIn(t *testing.T) {
	s, dir := seedStore(t, `{"accounts":[
		{"id":1,"provider":"claude","uuid":"a1","order":1,"sessions":"own"},
		{"id":2,"provider":"claude","uuid":"a2","order":2,"sessions":"own"},
		{"id":3,"provider":"claude","uuid":"a3","order":3,"sessions":"own"}],"nextId":4,"ordered":true}`)
	id := "01a00000-0000-7000-8000-00000000c3c3"
	a, b, c := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id), handoffClaude(dir, 3, id)
	writeLines(t, a, `{"type":"user","message":{"content":"on A"}}`)
	writeLines(t, b, `{"type":"user","message":{"content":"on A"}}`, `{"type":"user","message":{"content":"then on B"}}`)
	now := time.Now()
	handoffAt(t, a, now.Add(-time.Hour))
	handoffAt(t, b, now)

	if err := CopyForResume(s, s.Find(3), id); err != nil {
		t.Fatal(err)
	}
	if got := handoffRead(t, c); got != handoffRead(t, b) {
		t.Fatalf("C must resume B's newer copy, holds:\n%s", got)
	}
}

// A conversation taken on separately on two accounts has forked: each copy
// holds turns the other lacks. The target's is kept exactly as it was, so
// nothing said on it is lost, however new the other copy is.
func TestAForkedConversationIsLeftAsItWas(t *testing.T) {
	s, dir := seedStore(t, handoffPair)
	id := "01a00000-0000-7000-8000-00000000f0f0"
	a, b := handoffClaude(dir, 1, id), handoffClaude(dir, 2, id)
	first := `{"type":"user","message":{"content":"on A"}}`
	writeLines(t, a, first)
	now := time.Now()
	handoffAt(t, a, now.Add(-2*time.Hour))
	if err := CopyForResume(s, s.Find(2), id); err != nil {
		t.Fatal(err)
	}
	writeLines(t, a, first, `{"type":"user","message":{"content":"then on A"}}`)
	writeLines(t, b, first, `{"type":"user","message":{"content":"then, differently and at more length, on B"}}`)
	handoffAt(t, a, now.Add(-time.Hour))
	handoffAt(t, b, now)

	before, body := handoffStat(t, a), handoffRead(t, a)
	if err := CopyForResume(s, s.Find(1), id); err != nil {
		t.Fatalf("a fork is not a failed copy: %v", err)
	}
	handoffUntouched(t, a, before, body)
}

// Of the copies newer than the target's, one that extends it is taken over
// a newer one that has forked from it: the target gains turns and loses none.
func TestACopyThatExtendsTheTargetsWinsOverANewerFork(t *testing.T) {
	s, dir := seedStore(t, `{"accounts":[
		{"id":1,"provider":"claude","uuid":"a1","order":1,"sessions":"own"},
		{"id":2,"provider":"claude","uuid":"a2","order":2,"sessions":"own"},
		{"id":3,"provider":"claude","uuid":"a3","order":3,"sessions":"own"}],"nextId":4,"ordered":true}`)
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

// A grok conversation is a folder, measured by its newest file. A newer one
// that extends the target's, every file the target has being the start of
// the same file there, replaces it: the target ends holding the newer one's
// files, a file only the newer one has included.
func TestANewerGrokCopyReplacesTheDirectory(t *testing.T) {
	id := "01a00000-0000-7000-8000-0000000009fe"
	s, target, sibling := handoffGrokPair(t, id)
	summary := `{"info":{"id":"` + id + `","cwd":"/tmp/x"}}`
	now := time.Now()
	handoffGrokWrite(t, target, now.Add(-time.Hour), map[string][]string{
		"summary.json":       {summary},
		"chat_history.jsonl": {`{"role":"user"}`},
	})
	handoffGrokWrite(t, sibling, now, map[string][]string{
		"summary.json":       {summary},
		"chat_history.jsonl": {`{"role":"user"}`, `{"role":"assistant"}`},
		"plan.md":            {"# plan"},
	})

	if err := CopyForResume(s, s.Find(9), id); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"summary.json", "chat_history.jsonl", "plan.md"} {
		if got, want := handoffRead(t, filepath.Join(target, f)), handoffRead(t, filepath.Join(sibling, f)); got != want {
			t.Fatalf("%s must be the newer copy's:\n%s", f, got)
		}
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("the replaced folder holds %d entries, want the newer copy's three: %v", len(entries), entries)
	}
}

// A grok folder has forked when a file the target has is missing from the
// newer copy, or is not the start of the newer copy's: the target's folder
// is then kept exactly as it was.
func TestAForkedGrokCopyIsLeftAsItWas(t *testing.T) {
	summary := func(id string) string { return `{"info":{"id":"` + id + `","cwd":"/tmp/x"}}` }
	for name, c := range map[string]struct {
		target, sibling map[string][]string
	}{
		"a file only the target has": {
			target:  map[string][]string{"chat_history.jsonl": {`{"role":"user"}`}, "notes.md": {"mine"}},
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
			c.target["summary.json"], c.sibling["summary.json"] = []string{summary(id)}, []string{summary(id)}
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
		})
	}
}

// Transcripts run to megabytes, and the comparison reads them in pieces: a
// difference far past the first piece still counts, and so does an older
// copy that runs on past the newer one's end.
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

	write(older, long)
	write(newer, long+`{"type":"user"}`+"\n")
	if !handoffExtends(newer, older) {
		t.Fatal("a copy that goes on from the older one's last byte extends it")
	}
	write(older, long[:len(long)-10]+"0123456789")
	if handoffExtends(newer, older) {
		t.Fatal("a difference in the last bytes of a long copy is a fork")
	}
	write(older, long+`{"type":"user"}`+"\n"+`{"type":"assistant"}`+"\n")
	if handoffExtends(newer, older) {
		t.Fatal("an older copy that runs on past the newer one's end is not extended by it")
	}
}
