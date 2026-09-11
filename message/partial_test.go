package message

import (
	"os"
	"strings"
	"testing"
)

// The fixture is a real run with --include-partial-messages: claude prints
// each fragment as the model writes it, then the whole piece those added up
// to, wrapped in the framing of the message they belong to.
func partialFixture(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/claude-partial.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// A fragment arrives as a delta of the piece it belongs to, and the whole
// piece still follows: a client that ignores deltas sees everything once, as
// before, and one that shows them knows what to skip. The framing around
// them — a message opening, a block starting or ending, a signature — says
// nothing a client can show, so it is no event at all rather than noise.
func TestPartialFragmentsAreDeltasAndTheWholeStillFollows(t *testing.T) {
	var got []string
	var deltas, wholes []Event
	for _, line := range partialFixture(t) {
		for _, ev := range Normalize([]byte(line)) {
			kind := ev.Type
			if ev.Delta {
				kind += "+"
				deltas = append(deltas, ev)
			} else if ev.Type == "text" {
				wholes = append(wholes, ev)
			}
			got = append(got, kind)
		}
	}
	want := []string{
		"thinking", // the whole thinking block; its only delta was empty
		"text+",    // "Hello there, friend"
		"text+",    // "."
		"text",     // the whole text block
		"usage",    // message_delta, with the message's token numbers
		"usage",    // rate_limit_event
		"other",    // the CLI's result event
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
	if deltas[0].Text != "Hello there, friend" || deltas[1].Text != "." {
		t.Fatalf("a delta carries its fragment and nothing more: %+v", deltas)
	}
	if deltas[0].Blocks != nil || deltas[0].SessionID == "" {
		t.Fatalf("a fragment is not split into blocks, but it says which conversation: %+v", deltas[0])
	}
	if wholes[0].Text != "Hello there, friend." || wholes[0].Blocks != nil {
		t.Fatalf("the whole piece is unchanged by the deltas before it: %+v", wholes[0])
	}
}

// Thinking streams the same way as text, under its own kind; a fragment with
// nothing in it is not a happening, and half a tool call is not a tool call.
func TestAThinkingFragmentIsAThinkingDelta(t *testing.T) {
	line := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}},"session_id":"s"}`
	got := Normalize([]byte(line))
	if len(got) != 1 || got[0].Type != "thinking" || !got[0].Delta || got[0].Text != "hm" || got[0].SessionID != "s" {
		t.Fatalf("%+v", got)
	}
	empty := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}},"session_id":"s"}`
	if got := Normalize([]byte(empty)); got != nil {
		t.Fatalf("an empty fragment is nothing: %+v", got)
	}
	tool := `{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd"}},"session_id":"s"}`
	if got := Normalize([]byte(tool)); got != nil {
		t.Fatalf("half a tool call is not a tool call: %+v", got)
	}
}
