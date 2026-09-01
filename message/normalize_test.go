package message

import (
	"os"
	"strings"
	"testing"
)

// The fixture is a real Claude Code run, recorded from the CLI itself: a
// prompt that asked for a shell command, was refused twice, and answered in
// prose. Inventing these shapes is exactly how a normalizer ends up correct
// about a stream nobody emits.

func claudeFixture(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/claude.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestARealClaudeRunNormalizesInOrder(t *testing.T) {
	var got []string
	for _, line := range claudeFixture(t) {
		for _, ev := range Normalize([]byte(line)) {
			got = append(got, ev.Type)
		}
	}
	want := []string{
		"other",       // the CLI's own init; rota sends its own
		"other",       // thinking_tokens
		"thinking",    //
		"tool",        // Bash
		"blocked",     // permission_denied
		"tool_result", //
		"usage",       // rate_limit_event
		"tool",        // Bash again
		"blocked",     //
		"tool_result", //
		"thinking",    //
		"text",        // the answer
		"other",       // the CLI's result event, which repeats the answer
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestABlockedToolSaysWhichToolAndWhy(t *testing.T) {
	var blocked []Event
	for _, line := range claudeFixture(t) {
		for _, ev := range Normalize([]byte(line)) {
			if ev.Type == "blocked" {
				blocked = append(blocked, ev)
			}
		}
	}
	if len(blocked) != 2 {
		t.Fatalf("want two refusals, got %d", len(blocked))
	}
	if blocked[1].Tool != "Bash" {
		t.Errorf("tool: %q", blocked[1].Tool)
	}
	if !strings.HasPrefix(blocked[1].ToolID, "toolu_") {
		t.Errorf("tool id: %q", blocked[1].ToolID)
	}
	if !strings.Contains(blocked[1].Reason, "was blocked") {
		t.Errorf("reason: %q", blocked[1].Reason)
	}
}

func TestATextEventCarriesItsBlocks(t *testing.T) {
	var text *Event
	for _, line := range claudeFixture(t) {
		for _, ev := range Normalize([]byte(line)) {
			if ev.Type == "text" {
				e := ev
				text = &e
			}
		}
	}
	if text == nil {
		t.Fatal("the run said nothing")
	}
	if !strings.Contains(text.Text, "Blocked") {
		t.Fatalf("text: %q", text.Text)
	}
	var code int
	for _, b := range text.Blocks {
		if b.Kind == "code" {
			code++
		}
	}
	if len(text.Blocks) == 0 {
		t.Fatal("a text event carries the same split the result does")
	}
}

func TestAToolEventNamesTheTool(t *testing.T) {
	for _, line := range claudeFixture(t) {
		for _, ev := range Normalize([]byte(line)) {
			if ev.Type == "tool" {
				if ev.Tool != "Bash" || ev.ToolID == "" {
					t.Fatalf("%+v", ev)
				}
				return
			}
		}
	}
	t.Fatal("no tool event")
}

// codex and grok speak differently; one shape reaches the client.
func TestCodexAndGrokReachTheSameVocabulary(t *testing.T) {
	cases := []struct{ line, want, text string }{
		{`{"type":"thread.started","thread_id":"t-1"}`, "other", ""},
		{`{"type":"item.completed","item":{"type":"agent_message","text":"CODEX"}}`, "text", "CODEX"},
		{`{"type":"turn.completed","usage":{"input_tokens":5}}`, "usage", ""},
		{`{"type":"turn.failed"}`, "error", ""},
		{`{"text":"GROK","stopReason":"end_turn"}`, "text", "GROK"},
	}
	for _, c := range cases {
		got := Normalize([]byte(c.line))
		if len(got) != 1 {
			t.Fatalf("%s: got %d events", c.line, len(got))
		}
		if got[0].Type != c.want {
			t.Errorf("%s: type %q, want %q", c.line, got[0].Type, c.want)
		}
		if got[0].Text != c.text {
			t.Errorf("%s: text %q, want %q", c.line, got[0].Text, c.text)
		}
	}
}

func TestNonsenseIsNotAnEvent(t *testing.T) {
	if got := Normalize([]byte("not json")); got != nil {
		t.Fatalf("%+v", got)
	}
}
