package api

import (
	"os"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// A request that asks for partial messages gets each fragment as its own
// event, marked as a delta, and the whole piece afterwards unmarked — over
// HTTP exactly as on the command line, because both read the CLI through
// the same stream.
func TestPartialFragmentsStreamAsDeltas(t *testing.T) {
	h := newHarness(t, Options{})
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(
		`{"type":"system","subtype":"init","session_id":"s-p"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}},"session_id":"s-p"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}},"session_id":"s-p"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello world"}]},"session_id":"s-p"}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0},"session_id":"s-p"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s-p","result":"hello world","num_turns":1,"total_cost_usd":0.01}`,
	))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, raw := h.do("POST", "/v1/accounts/1/run",
		map[string]any{"prompt": "p", "stream": true, "include_partial_messages": true}, "Accept", "application/x-ndjson")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var fragments, wholes, framing int
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		switch {
		case strings.Contains(line, `"delta":true`):
			fragments++
		case strings.Contains(line, `"type":"text"`):
			wholes++
		case strings.Contains(line, `"type":"other"`):
			framing++
		}
	}
	// Two fragments, one whole; the block ending is framing and no event.
	// The CLI's own init and result lines are the two "other" a stream
	// always carries.
	if fragments != 2 || wholes != 1 || framing != 2 {
		t.Fatalf("fragments %d, wholes %d, other %d in:\n%s", fragments, wholes, framing, raw)
	}
	// Without the stream they belong to, fragments are refused before
	// anything is spent, naming what to add.
	resp, raw = h.do("POST", "/v1/accounts/1/run", map[string]any{"prompt": "p", "include_partial_messages": true})
	if resp.StatusCode != 400 || !strings.Contains(string(raw), "stream") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
}
