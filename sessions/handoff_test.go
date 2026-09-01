package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
