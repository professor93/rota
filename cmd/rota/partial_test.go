package main

import (
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// partialCLI is a fake that prints what claude prints with
// --include-partial-messages: each fragment as it is written, then the whole
// piece those made, then the framing around them. A second piece says what
// the fake was run with, so a test can see which flags reached it.
func partialCLI(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(
		`{"type":"system","subtype":"init","session_id":"s-1"}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},"session_id":"s-1"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}},"session_id":"s-1"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}},"session_id":"s-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello world"}]},"session_id":"s-1"}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0},"session_id":"s-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"\nARGS={{args}}"}]},"session_id":"s-1"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s-1","result":"hello world","num_turns":1,"total_cost_usd":0.01}`,
	))
	t.Setenv("PATH", bin)
}

// --with deltas asks the CLI for each fragment as the model writes it. It is a
// way of streaming, so it streams: the CLI is put in its streaming mode
// without --stream having to be said as well.
func TestPartialAsksTheCLIForFragmentsAndStreams(t *testing.T) {
	oneAccount(t)
	partialCLI(t)

	out, _, code := call(t, "--json", "run", "1", "hi", "--with", "deltas")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	events := eventsOf(t, out)
	if k := kinds(events); k[0] != "init" || k[len(k)-1] != "done" {
		t.Fatalf("a stream, opened by rota and closed by rota: %v", k)
	}
	var args string
	for _, ev := range events {
		if text, _ := ev["text"].(string); strings.HasPrefix(text, "\nARGS=") {
			args = text
		}
	}
	if !strings.Contains(args, "--include-partial-messages") || !strings.Contains(args, "--output-format stream-json") {
		t.Fatalf("the CLI must be asked for fragments, in its streaming mode: %q", args)
	}
}

// In text mode a piece that arrived in fragments is printed as they arrive,
// and not again when the whole of it follows; a piece that arrived whole is
// printed as it always was.
func TestPartialTextIsPrintedOnce(t *testing.T) {
	oneAccount(t)
	partialCLI(t)

	out, _, code := call(t, "run", "1", "hi", "--with", "deltas")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	if strings.Count(out, "hello world") != 1 || !strings.Contains(out, "\nARGS=") {
		t.Fatalf("the answer is printed exactly once, and the whole piece after it still arrives: %q", out)
	}
}

// In JSON mode every fragment is its own event, marked as one, and the whole
// piece still follows unmarked: a reader that ignores deltas sees what it
// always saw, and one that shows them knows which event to skip.
func TestPartialJSONMarksFragmentsAndKeepsTheWhole(t *testing.T) {
	oneAccount(t)
	partialCLI(t)

	out, _, code := call(t, "--json", "run", "1", "hi", "--with", "deltas")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	var fragments, wholes []string
	for _, ev := range eventsOf(t, out) {
		if ev["type"] != "text" {
			continue
		}
		text, _ := ev["text"].(string)
		if ev["delta"] == true {
			fragments = append(fragments, text)
		} else {
			wholes = append(wholes, text)
		}
	}
	if strings.Join(fragments, "|") != "hello |world" || len(wholes) != 2 || wholes[0] != "hello world" {
		t.Fatalf("fragments %q, whole pieces %q in:\n%s", fragments, wholes, out)
	}
}
