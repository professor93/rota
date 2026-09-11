package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// askingCLI answers with a fenced command and then a question with two
// choices: something for both readings to find.
func askingCLI(t *testing.T) {
	t.Helper()
	answer := "Run:\n```sh\nls\n```\nWhich one?\n- keep\n- drop"
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(
		`{"type":"system","subtype":"init","session_id":"s-w"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":`+strconv.Quote(answer)+`}]},"session_id":"s-w"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s-w","result":`+strconv.Quote(answer)+`,"num_turns":1,"total_cost_usd":0.01}`,
	))
	t.Setenv("PATH", bin)
}

// The reply is the answer and nothing read out of it, unless a reading was
// asked for by name. --with takes a comma list, or one name at a time, or
// both, and implies --json since a reading has nowhere else to go.
func TestReadingsComeOnlyWithWith(t *testing.T) {
	oneAccount(t)
	askingCLI(t)

	out, _, code := call(t, "--json", "run", "1", "hi")
	if code != 0 || strings.Contains(out, `"blocks"`) || strings.Contains(out, `"ask"`) {
		t.Fatalf("unasked, the reply is the answer alone: %d %s", code, out)
	}
	for _, args := range [][]string{
		{"run", "1", "hi", "--with", "blocks,ask"},
		{"run", "1", "hi", "--with", "ask", "--with", "blocks"},
		{"run", "1", "--with", "blocks", "hi", "--with=ask"},
	} {
		out, _, code := call(t, args...)
		if code != 0 || !strings.Contains(out, `"blocks"`) || !strings.Contains(out, `"kind": "choice"`) || !strings.HasPrefix(out, "{") {
			t.Fatalf("%v: both readings, as JSON: %d %s", args, code, out)
		}
	}
	out, _, code = call(t, "run", "1", "hi", "--with", "ask")
	if code != 0 || strings.Contains(out, `"blocks"`) || !strings.Contains(out, `"question": "Which one?"`) {
		t.Fatalf("one reading, not the other: %d %s", code, out)
	}
	_, errOut, code := call(t, "run", "1", "hi", "--with", "blocks,foo")
	if code == 0 || !strings.Contains(errOut, `"foo"`) || !strings.Contains(errOut, "blocks") || !strings.Contains(errOut, "ask") {
		t.Fatalf("an unknown reading is refused by name, before anything is spent: %d %q", code, errOut)
	}
}

// On a stream, blocks ride on text events only when asked for.
func TestStreamedTextCarriesBlocksOnlyWithWith(t *testing.T) {
	oneAccount(t)
	askingCLI(t)

	out, _, code := call(t, "--json", "run", "1", "hi", "--stream")
	if code != 0 || strings.Contains(out, `"blocks"`) {
		t.Fatalf("unasked: %d %s", code, out)
	}
	out, _, code = call(t, "run", "1", "hi", "--stream", "--with", "blocks")
	if code != 0 || strings.Count(out, `"blocks"`) != 1 || !strings.Contains(out, `"lang":"sh"`) {
		t.Fatalf("asked, once per whole text event: %d %s", code, out)
	}
}
