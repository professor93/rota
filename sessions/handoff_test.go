package sessions

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
// on B. A's own copy is older than B's, and is replaced by it, at its own
// path, leaving nothing else behind. Moved back with nothing said on B,
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

// A grok conversation is a directory, measured by its newest file. A newer
// one replaces the target's whole: its files are the sibling's, and a file
// only the older copy had is gone.
func TestANewerGrokCopyReplacesTheDirectory(t *testing.T) {
	s, dir := seedStore(t, `{"accounts":[
		{"id":8,"provider":"grok","uuid":"g1","order":1},
		{"id":9,"provider":"grok","uuid":"g2","order":2}],"nextId":10,"ordered":true}`)
	id := "01a00000-0000-7000-8000-0000000009fe"
	rel := filepath.Join("sessions", "%2Ftmp%2Fx", id)
	older, newer := filepath.Join(dir, "homes", "grok-9", rel), filepath.Join(dir, "homes", "grok-8", rel)
	writeLines(t, filepath.Join(older, "summary.json"), `{"info":{"id":"`+id+`","cwd":"/tmp/x"}}`)
	writeLines(t, filepath.Join(older, "chat_history.jsonl"), `{"role":"user"}`)
	writeLines(t, filepath.Join(older, "stale.json"), `{}`)
	writeLines(t, filepath.Join(newer, "summary.json"), `{"info":{"id":"`+id+`","cwd":"/tmp/x"}}`)
	writeLines(t, filepath.Join(newer, "chat_history.jsonl"), `{"role":"user"}`, `{"role":"assistant"}`)
	now := time.Now()
	for _, f := range []string{"summary.json", "chat_history.jsonl", "stale.json"} {
		handoffAt(t, filepath.Join(older, f), now.Add(-time.Hour))
	}
	handoffAt(t, filepath.Join(newer, "summary.json"), now.Add(-time.Hour))
	handoffAt(t, filepath.Join(newer, "chat_history.jsonl"), now)

	if err := CopyForResume(s, s.Find(9), id); err != nil {
		t.Fatal(err)
	}
	if got, want := handoffRead(t, filepath.Join(older, "chat_history.jsonl")), handoffRead(t, filepath.Join(newer, "chat_history.jsonl")); got != want {
		t.Fatalf("the newer history must replace the older:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(older, "stale.json")); !os.IsNotExist(err) {
		t.Fatalf("a file only the older copy had must go: %v", err)
	}
	entries, err := os.ReadDir(older)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("the replaced directory holds %d entries, want the sibling's two: %v", len(entries), entries)
	}
}
