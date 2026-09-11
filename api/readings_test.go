package api

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// Over HTTP every reading lands beside the answer when named in "with", the
// requests among them reach the CLI as flags, and a stream's done carries
// the readings the tally made.
func TestReadingsOverHTTP(t *testing.T) {
	h := newHarness(t, Options{})
	answer := "Fixed, see [docs](https://example.dev/docs).\n```bash\nmake test\n```\nARGS={{args}}"
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(
		`{"type":"system","subtype":"init","session_id":"s-r"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/srv/api/README.md"}}]},"session_id":"s-r"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Write","input":{"file_path":"/srv/api/NEW.md","content":"x"}}]},"session_id":"s-r"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":`+strconv.Quote(answer)+`}]},"session_id":"s-r"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s-r","result":`+strconv.Quote(answer)+`,"num_turns":2}`,
	))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	code, _, raw := h.run(1, map[string]any{"prompt": "p", "with": []string{"code,files,tools,stats,timing,plain,links,account,argv"}})
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"code", "files", "tools", "stats", "timing", "plain", "links", "account_label", "argv", "env_set"} {
		if doc[want] == nil {
			t.Fatalf("missing %q in %s", want, raw)
		}
	}
	files := doc["files"].(map[string]any)
	if w, _ := files["written"].([]any); len(w) != 1 || w[0] != "/srv/api/NEW.md" {
		t.Fatalf("files: %v", files)
	}
	if !strings.Contains(raw, "--include-argv") && !strings.Contains(doc["plain"].(string), "ARGS=") {
		t.Fatalf("the answer carries the argv the fake saw: %s", raw)
	}

	// The requests become claude's own flags.
	code, _, raw = h.run(1, map[string]any{"prompt": "p", "stream": true, "with": []string{"hooks", "subagents", "suggestions", "deltas"}})
	for _, want := range []string{"--include-hook-events", "--forward-subagent-text", "--prompt-suggestions true", "--include-partial-messages"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %q in %d %s", want, code, raw)
		}
	}

	// A stream: at on each event, and the readings on done.
	_, body := h.do("POST", "/v1/accounts/1/run",
		map[string]any{"prompt": "p", "stream": true, "with": []string{"timing", "files", "tools"}}, "Accept", "application/x-ndjson")
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	for _, line := range lines[1 : len(lines)-1] {
		if !strings.Contains(line, `"at":`) {
			t.Fatalf("every event carries at: %s", line)
		}
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"type":"done"`) || !strings.Contains(last, `"files":`) || !strings.Contains(last, `"tools":`) || !strings.Contains(last, `"timing":`) {
		t.Fatalf("done carries the readings: %s", last)
	}
}
