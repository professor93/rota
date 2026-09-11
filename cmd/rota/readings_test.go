package main

import (
	jsonv2 "encoding/json/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// workingCLI is a fake that reads a file, is refused a command, edits the
// file, and answers with a fence and a link: something for every reading
// to find. Its answer also carries the arguments it was run with.
func workingCLI(t *testing.T) {
	t.Helper()
	answer := "Fixed it, see [the docs](https://example.dev/docs).\n```bash\nmake test\n```\nARGS={{args}}"
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(
		`{"type":"system","subtype":"init","session_id":"s-r"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/srv/api/README.md"}}]},"session_id":"s-r"}`,
		`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"t2","message":"no","session_id":"s-r"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Edit","input":{"file_path":"/srv/api/README.md","old_string":"a","new_string":"b"}}]},"session_id":"s-r"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":`+strconv.Quote(answer)+`}]},"session_id":"s-r"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s-r","result":`+strconv.Quote(answer)+`,"num_turns":2,"total_cost_usd":0.02}`,
	))
	t.Setenv("PATH", bin)
}

func document(t *testing.T, out string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := jsonv2.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out)
	}
	return doc
}

// Every reading of the answer and the stream lands beside the result when
// named, and nowhere unless.
func TestEveryReadingLandsBesideTheAnswer(t *testing.T) {
	oneAccount(t)
	workingCLI(t)

	out, _, code := call(t, "--json", "run", "1", "hi")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	for _, absent := range []string{"code", "files", "timing", "tools", "blocked", "stats", "argv", "env_set", "plain", "links", "account_label", "quota"} {
		if _, there := document(t, out)[absent]; there {
			t.Fatalf("unasked, %q must not be there:\n%s", absent, out)
		}
	}

	out, _, code = call(t, "run", "1", "hi", "--with", "code,files,timing,tools,stats,argv,plain,links,account,quota")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	doc := document(t, out)
	if c, _ := doc["code"].([]any); len(c) != 1 || c[0].(map[string]any)["text"] != "make test" {
		t.Fatalf("code: %v", doc["code"])
	}
	files, _ := doc["files"].(map[string]any)
	if r, _ := files["read"].([]any); len(r) != 1 || r[0] != "/srv/api/README.md" {
		t.Fatalf("files: %v", doc["files"])
	}
	if w, _ := files["written"].([]any); len(w) != 1 {
		t.Fatalf("files: %v", doc["files"])
	}
	tools, _ := doc["tools"].(map[string]any)
	blocked, _ := doc["blocked"].(map[string]any)
	if tools["Read"] != 1.0 || tools["Edit"] != 1.0 || blocked["Bash"] != 1.0 {
		t.Fatalf("tools %v blocked %v", tools, blocked)
	}
	stats, _ := doc["stats"].(map[string]any)
	if stats["events"].(float64) < 5 || stats["bytes"].(float64) == 0 {
		t.Fatalf("stats: %v", stats)
	}
	if _, ok := doc["timing"].(map[string]any); !ok {
		t.Fatalf("timing: %v", doc["timing"])
	}
	if argv, _ := doc["argv"].([]any); len(argv) == 0 || !strings.HasSuffix(argv[0].(string), "claude") {
		t.Fatalf("argv: %v", doc["argv"])
	}
	if env, _ := doc["env_set"].([]any); len(env) == 0 || env[0] != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("env names: %v", doc["env_set"])
	}
	if plain, _ := doc["plain"].(string); !strings.HasPrefix(plain, "Fixed it, see the docs (https://example.dev/docs).\nmake test") {
		t.Fatalf("plain: %q", plain)
	}
	if links, _ := doc["links"].([]any); len(links) != 1 || links[0] != "https://example.dev/docs" {
		t.Fatalf("links: %v", doc["links"])
	}
	if doc["account_label"] != "claude/a@x" || doc["order"] != 1.0 || doc["threshold"] == nil {
		t.Fatalf("account: %v %v %v", doc["account_label"], doc["order"], doc["threshold"])
	}
	// The fake provider has no usage endpoint: a quota reading that could
	// not be taken is left out, and the run is not spoiled by it.
	if _, there := doc["quota"]; there {
		t.Fatalf("no usage to read: %v", doc["quota"])
	}
}

// Requests reach the CLI as its own flags: raw and deltas as before, and
// hooks, subagents and suggestions, which had no command-line spelling.
func TestRequestsReachTheCLIAsFlags(t *testing.T) {
	oneAccount(t)
	workingCLI(t)
	out, _, code := call(t, "--json", "run", "1", "hi", "--with", "hooks,subagents,suggestions,deltas")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	var args string
	for _, ev := range eventsOf(t, out) {
		if text, _ := ev["text"].(string); strings.Contains(text, "ARGS=") {
			args = text
		}
	}
	for _, want := range []string{"--include-hook-events", "--forward-subagent-text", "--prompt-suggestions true", "--include-partial-messages", "--output-format stream-json"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %q", want, args)
		}
	}
}

// Timing stamps every streamed event, and the tally rides on done.
func TestAStreamCarriesTimingAndDoneCarriesTheReadings(t *testing.T) {
	oneAccount(t)
	workingCLI(t)
	out, _, code := call(t, "--json", "run", "1", "hi", "--stream", "--with", "timing,files,tools")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	events := eventsOf(t, out)
	for _, ev := range events[1 : len(events)-1] {
		if _, ok := ev["at"]; !ok {
			t.Fatalf("every event after init carries at: %v", ev)
		}
	}
	done := events[len(events)-1]
	if done["type"] != "done" || done["files"] == nil || done["tools"] == nil || done["timing"] == nil {
		t.Fatalf("done carries the readings: %v", done)
	}
}

// stderr is the one reading that touches result, and only on a failed run
// that had no answer.
func TestStderrReadingCopiesTheReasonOnRequest(t *testing.T) {
	oneAccount(t)
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Spec{Stderr: []string{"Not logged in"}, Exit: 1})
	t.Setenv("PATH", bin)
	out, _, _ := call(t, "--json", "run", "1", "hi")
	if doc := document(t, out); doc["result"] != "" || doc["stderr"] != "Not logged in" {
		t.Fatalf("unasked, result is empty and the reason is in stderr: %s", out)
	}
	out, _, _ = call(t, "run", "1", "hi", "--with", "stderr")
	if doc := document(t, out); doc["result"] != "Not logged in" {
		t.Fatalf("asked, result carries the reason: %s", out)
	}
}
