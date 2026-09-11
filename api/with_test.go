package api

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// A reply is the answer and nothing read out of it, unless the request
// names the readings it wants in "with". An unknown name is refused before
// anything is spent.
func TestReadingsComeOnlyWithWith(t *testing.T) {
	h := newHarness(t, Options{})
	answer := "Run:\n```sh\nls\n```\nWhich one?\n- keep\n- drop"
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(
		`{"type":"system","subtype":"init","session_id":"s-w"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":`+strconv.Quote(answer)+`}]},"session_id":"s-w"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s-w","result":`+strconv.Quote(answer)+`,"num_turns":1}`,
	))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	code, _, raw := h.run(1, map[string]any{"prompt": "p"})
	if code != 200 || strings.Contains(raw, `"blocks"`) || strings.Contains(raw, `"ask"`) {
		t.Fatalf("unasked, the reply is the answer alone: %d %s", code, raw)
	}
	code, _, raw = h.run(1, map[string]any{"prompt": "p", "with": []string{"blocks", "ask"}})
	if code != 200 || !strings.Contains(raw, `"lang":"sh"`) || !strings.Contains(raw, `"kind":"choice"`) {
		t.Fatalf("both readings: %d %s", code, raw)
	}
	code, _, raw = h.run(1, map[string]any{"prompt": "p", "with": []string{"blocks,ask"}})
	if code != 200 || !strings.Contains(raw, `"lang":"sh"`) || !strings.Contains(raw, `"kind":"choice"`) {
		t.Fatalf("a comma list works here too: %d %s", code, raw)
	}
	code, _, raw = h.run(1, map[string]any{"prompt": "p", "with": []string{"foo"}})
	if code != 400 || !strings.Contains(raw, "foo") || !strings.Contains(raw, "blocks, ask") {
		t.Fatalf("an unknown reading is refused by name: %d %s", code, raw)
	}

	// Streams: blocks on text events only when asked.
	_, body := h.do("POST", "/v1/accounts/1/run", map[string]any{"prompt": "p", "stream": true}, "Accept", "application/x-ndjson")
	if strings.Contains(string(body), `"blocks"`) {
		t.Fatalf("unasked stream: %s", body)
	}
	_, body = h.do("POST", "/v1/accounts/1/run", map[string]any{"prompt": "p", "stream": true, "with": []string{"blocks"}}, "Accept", "application/x-ndjson")
	if strings.Count(string(body), `"blocks"`) != 1 || !strings.Contains(string(body), `"lang":"sh"`) {
		t.Fatalf("asked stream, once per whole text event: %s", body)
	}
}
