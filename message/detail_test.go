package message

import (
	"strings"
	"testing"
)

// A tool event says what the tool was asked to do, not only which tool: the
// file a Read opened or the command a Bash ran is the fact a client wants
// to show, and until now it was only in raw.
func TestAToolEventCarriesItsInput(t *testing.T) {
	var tools []Event
	for _, line := range claudeFixture(t) {
		for _, ev := range Normalize([]byte(line)) {
			switch ev.Type {
			case "tool":
				tools = append(tools, ev)
			case "text", "thinking", "tool_result":
				if ev.Input != nil {
					t.Fatalf("only a tool call has an input: %+v", ev)
				}
			}
		}
	}
	if len(tools) == 0 {
		t.Fatal("the fixture has tool calls")
	}
	if !strings.Contains(string(tools[0].Input), `"command":"rm -rf /tmp/does-not-exist-rota-probe`) {
		t.Fatalf("the input is the tool's own, verbatim: %s", tools[0].Input)
	}
}

// A usage event carries the numbers it was made of, in one vocabulary for
// every provider: claude's message accounting when partial messages are
// streamed, and codex's turn accounting. A limit reading that has no token
// numbers still arrives, without them.
func TestUsageEventsCarryTheirNumbers(t *testing.T) {
	var usage []Event
	for _, line := range partialFixture(t) {
		for _, ev := range Normalize([]byte(line)) {
			if ev.Type == "usage" {
				usage = append(usage, ev)
			}
		}
	}
	if len(usage) != 2 {
		t.Fatalf("the message's accounting and then the limit reading: %+v", usage)
	}
	if u := usage[0].Usage; u == nil || u.InputTokens != 10 || u.OutputTokens != 49 || u.CacheReadInputTokens != 13734 || u.CacheCreationInputTokens != 11870 {
		t.Fatalf("message_delta carries the message's usage: %+v", usage[0].Usage)
	}
	if usage[1].Usage != nil {
		t.Fatalf("a limit reading has no token numbers: %+v", usage[1].Usage)
	}

	codex := `{"type":"turn.completed","usage":{"input_tokens":5,"cached_input_tokens":2,"output_tokens":7}}`
	got := Normalize([]byte(codex))
	if len(got) != 1 || got[0].Type != "usage" || got[0].Usage == nil {
		t.Fatalf("%+v", got)
	}
	if u := got[0].Usage; u.InputTokens != 5 || u.CacheReadInputTokens != 2 || u.OutputTokens != 7 {
		t.Fatalf("codex's cached_input_tokens is the same reading as claude's cache_read_input_tokens: %+v", u)
	}
}
