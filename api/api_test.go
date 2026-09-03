package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/professor93/rota/internal/fakecli"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/wire"
)

// TestMain doubles as the fake vendor CLIs: the test binary is symlinked as
// `claude` and `codex` into a private PATH, and behaves by its argv[0].
func TestMain(m *testing.M) {
	// A fake installed with a spec plays that; the harness's own claude and
	// codex links carry none and are played below.
	fakecli.Maybe()
	switch strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") {
	case "claude":
		fakeClaude()
	case "codex":
		fakeCodex()
	}
	os.Exit(m.Run())
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func fakeSleepAndExit() {
	if s := os.Getenv("FAKE_SLEEP"); s != "" {
		d, _ := time.ParseDuration(s)
		time.Sleep(d)
	}
	fmt.Fprintln(os.Stderr, "stderr-line")
	if c := os.Getenv("FAKE_EXIT"); c != "" {
		os.Exit(3)
	}
	os.Exit(0)
}

func fakeClaude() {
	args := os.Args[1:]
	prompt, _ := io.ReadAll(os.Stdin)
	var files []string
	for i, a := range args {
		if a == "--add-dir" {
			for _, d := range args[i+1:] {
				if strings.HasPrefix(d, "-") {
					break
				}
				filepath.WalkDir(d, func(p string, e os.DirEntry, _ error) error {
					if e != nil && !e.IsDir() {
						files = append(files, filepath.Base(p))
					}
					return nil
				})
			}
		}
	}
	summary := fmt.Sprintf("PROMPT=%s ARGS=%s TOKEN=%s FILES=%s CWD=%s", prompt, strings.Join(args, " "),
		os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), strings.Join(files, ","), mustCwd())
	result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "session_id": "s-1",
		"result": summary, "structured_output": map[string]bool{"ok": true}, "num_turns": 1, "total_cost_usd": 0.01,
		"usage": map[string]int{"input_tokens": 1}}
	enc := json.NewEncoder(os.Stdout)
	if flagValue(args, "--output-format") == "stream-json" {
		enc.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s-1"})
		if s := os.Getenv("FAKE_SLEEP"); s != "" {
			d, _ := time.ParseDuration(s)
			time.Sleep(d)
		}
		enc.Encode(map[string]any{"type": "assistant", "session_id": "s-1", "message": map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": summary}}}})
		enc.Encode(result)
		fmt.Fprintln(os.Stderr, "stderr-line")
		os.Exit(0)
	}
	enc.Encode([]any{map[string]any{"type": "system", "subtype": "init"}, result})
	fakeSleepAndExit()
}

func fakeCodex() {
	args := os.Args[1:]
	prompt, _ := io.ReadAll(os.Stdin)
	last := fmt.Sprintf("PROMPT=%s ARGS=%s HOME=%s", prompt, strings.Join(args, " "), os.Getenv("CODEX_HOME"))
	if out := flagValue(args, "-o"); out != "" {
		os.WriteFile(out, []byte(last), 0o600)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.Encode(map[string]any{"type": "thread.started", "thread_id": "t-1"})
	enc.Encode(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": last}})
	enc.Encode(map[string]any{"type": "turn.completed", "usage": map[string]int{"input_tokens": 5, "output_tokens": 2}})
	fakeSleepAndExit()
}

func mustCwd() string { d, _ := os.Getwd(); return d }

// harness wires a private PATH with the fake CLIs, a store with four
// accounts, and a server.
type harness struct {
	t       testing.TB
	handler http.Handler
	root    string
	srv     *httptest.Server
	token   string
	dir     string
}

func newHarness(t testing.TB, opts Options) *harness {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"claude", "codex"} {
		if err := fakecli.Link(filepath.Join(bin, fakecli.Exe(name))); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_EXIT", "")
	t.Setenv("FAKE_SLEEP", "")

	home := t.TempDir()
	store := `{"accounts":[
	 {"id":1,"provider":"claude","email":"a@x","token":{"accessToken":"tok-1"}},
	 {"id":2,"provider":"codex","email":"c@x","token":{"accessToken":"tok-2","refreshToken":"r2"},"extra":{"id_token":"idt","account_id":"acct"},"staged":"-"},
	 {"id":3,"provider":"claude","email":"b@x","token":{"accessToken":"tok-3"}},
	 {"id":4,"provider":"claude","email":"dead@x","dead":true,"token":{"accessToken":"x"}}]}`
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	opts.Dir = home
	if opts.Token == "" {
		opts.Token = "secret"
	}
	if opts.Roots == nil {
		opts.Roots = []string{root}
	}
	// Off unless a test asks for it: these accounts carry made-up tokens,
	// and a timer refreshing them would put a real provider on the wire.
	if opts.RefreshEvery == 0 {
		opts.RefreshEvery = -1
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	h := &harness{t: t, handler: handler, root: root, dir: home, srv: httptest.NewServer(handler), token: opts.Token}
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) do(method, path string, body any, hdr ...string) (*http.Response, []byte) {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		r = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func (h *harness) run(id int, body map[string]any) (int, rota.Result, string) {
	h.t.Helper()
	resp, raw := h.do("POST", fmt.Sprintf("/v1/accounts/%d/run", id), body)
	var out rota.Result
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func TestNewRequiresToken(t *testing.T) {
	if _, err := New(Options{Dir: t.TempDir()}); err == nil {
		t.Fatal("a server without a token must refuse to start")
	}
}

func TestBearerAuthGuardsEverythingButHealth(t *testing.T) {
	h := newHarness(t, Options{})
	resp, _ := http.Get(h.srv.URL + "/")
	if resp.StatusCode != 200 {
		t.Fatalf("the root: %d", resp.StatusCode)
	}
	for _, auth := range []string{"", "Bearer wrong", "Basic secret", "Bearer secre"} {
		req, _ := http.NewRequest("GET", h.srv.URL+"/v1/accounts", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != 401 {
			t.Fatalf("%q: %d", auth, resp.StatusCode)
		}
	}
	resp2, raw := h.do("GET", "/v1/accounts", nil)
	var doc struct {
		Accounts []struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
		} `json:"accounts"`
	}
	json.Unmarshal(raw, &doc)
	if resp2.StatusCode != 200 || len(doc.Accounts) != 4 || doc.Accounts[3].Status != "reauth" || strings.Contains(string(raw), "tok-1") {
		t.Fatalf("%d %s", resp2.StatusCode, raw)
	}
}

// A caller who asked for "opus" and left effort alone cannot see from the
// outside which model that became, or what the provider's default effort is.
// The result says, so a log of finished runs is worth keeping.
func TestResultReportsTheModelAndEffortThatActuallyRan(t *testing.T) {
	h := newHarness(t, Options{})

	code, out, raw := h.run(1, map[string]any{"prompt": "hi", "model": "opus", "effort": "high"})
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if out.Model != "claude-opus-5" {
		t.Errorf("model: got %q, want the resolved id claude-opus-5", out.Model)
	}
	if out.Effort != "high" {
		t.Errorf("effort: got %q, want high", out.Effort)
	}

	// Left out, both must still be reported: the default is the answer.
	wantModel, wantEffort := rota.Defaults("claude")
	code, out, raw = h.run(1, map[string]any{"prompt": "hi"})
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if out.Model != wantModel || out.Effort != wantEffort {
		t.Errorf("defaults: got model %q effort %q, want %q and %q", out.Model, out.Effort, wantModel, wantEffort)
	}
}

// An answer is markdown with code in the middle of it. The reply carries
// the original text and rota's split of it, so a client showing code
// differently from prose does not have to parse markdown itself.
func TestResultCarriesTheAnswerSplitIntoBlocks(t *testing.T) {
	h := newHarness(t, Options{})

	code, _, raw := h.run(1, map[string]any{"prompt": "here:\n```go\nx := 1\n```\ndone"})
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	var out struct {
		rota.Result
		Blocks []message.Block `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Result.Result, "x := 1") {
		t.Fatalf("the original text must still be there: %q", out.Result.Result)
	}
	var codeBlocks []message.Block
	for _, b := range out.Blocks {
		if b.Kind == "code" {
			codeBlocks = append(codeBlocks, b)
		}
	}
	if len(codeBlocks) != 1 {
		t.Fatalf("want one code block, got %d: %+v", len(codeBlocks), out.Blocks)
	}
	if codeBlocks[0].Lang != "go" || codeBlocks[0].Text != "x := 1" {
		t.Fatalf("code block: %+v", codeBlocks[0])
	}
	if len(out.Blocks) < 2 {
		t.Fatalf("the prose around it is blocks too: %+v", out.Blocks)
	}
}

// An account can be tied to one project over HTTP, the same as it can from
// the command line, and refuses the same two mistakes.
func TestAnAccountCanBeTiedToAProjectOverHTTP(t *testing.T) {
	h := newHarness(t, Options{})
	project := t.TempDir()
	config := t.TempDir()

	resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"cwd": project, "config_dir": config})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var got wire.Account
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cwd != project || got.ConfigDir != config {
		t.Fatalf("%+v", got)
	}

	// A relative directory means a different place depending on where the
	// server happened to be started.
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"cwd": "relative"}); resp.StatusCode != 400 ||
		!strings.Contains(string(raw), "absolute") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	// Credentials are staged in the config directory. A repository is not
	// the place for them.
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"config_dir": project}); resp.StatusCode != 400 ||
		!strings.Contains(string(raw), "credential") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	// The refusal must not have half-applied. The path is looked for as
	// JSON carries it, backslashes escaped.
	quoted, _ := json.Marshal(config)
	if resp, raw := h.do("GET", "/v1/accounts", nil); resp.StatusCode != 200 ||
		!strings.Contains(string(raw), strings.Trim(string(quoted), `"`)) {
		t.Fatalf("the rejected change was kept: %d %s", resp.StatusCode, raw)
	}
}

// A run that names no directory of its own starts in the account's.
func TestARunStartsInTheAccountsOwnProject(t *testing.T) {
	h := newHarness(t, Options{})
	// Inside the server's own roots: an account cannot be given a directory
	// the server was told to stay out of, which is the whole point of them.
	project := filepath.Join(h.root, "sub")
	real, _ := filepath.EvalSymlinks(project)
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"cwd": project}); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	code, out, raw := h.run(1, map[string]any{"prompt": "p"})
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if !strings.Contains(out.Result, "CWD="+real) {
		t.Fatalf("ran somewhere else: %q", out.Result)
	}
}

func TestRunClaudeMapsEveryFieldAndFeedsPromptOnStdin(t *testing.T) {
	// Every field, which means the two a confined server refuses as well:
	// what they map to still has to be right for the server that allows them.
	// That they are refused otherwise is checked in the table below.
	h := newHarness(t, Options{AllowRawFlags: true})
	sub := filepath.Join(h.root, "sub")
	realSub, _ := filepath.EvalSymlinks(sub)
	code, out, raw := h.run(1, map[string]any{
		"prompt": "hello there", "model": "opus", "effort": "high", "fallback_model": "sonnet",
		"json_schema": map[string]any{"type": "object"}, "max_budget_usd": 1.5,
		"session_id": "11111111-1111-1111-1111-111111111111", "fork_session": true, "ephemeral": true, "name": "n1", "debug": "true",
		"system_prompt": "sys", "append_system_prompt": "app", "settings": map[string]any{"a": 1}, "agents": map[string]any{"r": map[string]string{"description": "d", "prompt": "p"}},
		"agent": "r", "strict_mcp_config": true,
		"plugin_urls": []string{"https://x/p.zip"}, "autocompact": "200000",
		"exclude_dynamic_system_prompt_sections": true, "worktree": "wt1",
		"setting_sources": []string{"user", "project"}, "permission_mode": "plan", "allowed_tools": []string{"Bash(git *)", "Edit"}, "disallowed_tools": []string{"WebFetch"}, "tools": []string{"Bash", "Edit"},
		"restricted": true, "safe_mode": true, "disable_slash_commands": true,
		"include_partial_messages": true, "include_hook_events": true, "forward_subagent_text": true, "prompt_suggestions": true, "verbose": true,
		"add_dirs": []string{sub}, "cwd": sub,
	})
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	for _, want := range []string{"PROMPT=hello there", "TOKEN=tok-1", "CWD=" + realSub,
		"-p --output-format json", "--model claude-opus-5", "--effort high", "--fallback-model sonnet", `--json-schema {"type":"object"}`,
		"--max-budget-usd 1.5", "--session-id 11111111-1111-1111-1111-111111111111", "--fork-session", "--no-session-persistence", "--name n1",
		"--system-prompt sys", "--append-system-prompt app", `--settings {"a":1}`, `--agents {"r":{"description":"d","prompt":"p"}}`, "--agent r",
		"--strict-mcp-config", "--plugin-url https://x/p.zip", "--autocompact 200000", "--exclude-dynamic-system-prompt-sections",
		"--setting-sources user,project", "--worktree wt1", "--permission-mode plan", "--allowedTools Bash(git *),Edit", "--disallowedTools WebFetch", "--tools Bash,Edit",
		"--restricted", "--safe-mode", "--disable-slash-commands", "--include-partial-messages", "--include-hook-events", "--forward-subagent-text",
		"--prompt-suggestions true", "--verbose", "--debug",
		"--add-dir " + sub} {
		if !strings.Contains(out.Result, want) {
			t.Fatalf("missing %q in %q", want, out.Result)
		}
	}
	if out.SessionID != "s-1" || string(out.Structured) != `{"ok":true}` || out.ExitCode != 0 || !strings.Contains(out.Stderr, "stderr-line") ||
		out.Account != 1 || out.Provider != "claude" || out.CostUSD != 0.01 || out.NumTurns != 1 || out.IsError {
		t.Fatalf("result: %+v", out)
	}
	if strings.Contains(out.Result, "--continue") || strings.Contains(out.Result, "--resume") || strings.Contains(out.Result, "--dangerously") {
		t.Fatalf("flags that were not asked for: %q", out.Result)
	}
}

func TestRunDefaultsAreCleanAndMinimal(t *testing.T) {
	h := newHarness(t, Options{})
	_, out, _ := h.run(3, map[string]any{"prompt": "p"})
	if !strings.Contains(out.Result, "ARGS=-p --output-format json --setting-sources ") {
		t.Fatalf("defaults: %q", out.Result)
	}
	_, out, _ = h.run(1, map[string]any{"prompt": "p"})
	dm, de := rota.Defaults("claude")
	if !strings.Contains(out.Result, "--model "+dm) || !strings.Contains(out.Result, "--effort "+de) {
		t.Fatalf("a run must name the provider default model and effort: %q", out.Result)
	}
	_, out, _ = h.run(1, map[string]any{"prompt": "p", "resume": "abc", "continue": true, "include_events": true})
	if !strings.Contains(out.Result, "--resume abc") || !strings.Contains(out.Result, " -c ") || len(out.Events) != 2 {
		t.Fatalf("%q events=%d", out.Result, len(out.Events))
	}
	// Naming settings sources points the run at workspace files, which is
	// the operator's call: a confined server refuses it.
	if code, _, raw := h.run(1, map[string]any{"prompt": "p", "setting_sources": []string{"user"}}); code != 400 {
		t.Fatalf("named setting_sources must be refused here: %d %s", code, raw)
	}
}

func TestRunStreamsServerSentEvents(t *testing.T) {
	h := newHarness(t, Options{})
	resp, raw := h.do("POST", "/v1/accounts/1/run", map[string]any{"prompt": "p", "stream": true, "include_partial_messages": true})
	body := string(raw)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	// The stream speaks rota's vocabulary, not the CLI's: it opens with who
	// is answering and on what, and every later event is named by what
	// happened rather than by which vendor said it.
	for _, want := range []string{
		"event: init\ndata: {", `"provider":"claude"`, `"model":"claude-opus-5"`, `"seq":1`,
		"event: text\n", "event: done\n", `"exit_code":0`,
		"--output-format stream-json --verbose", "--include-partial-messages"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	// The provider's own event is not sent unless it was asked for.
	if strings.Contains(body, `"raw":`) {
		t.Fatalf("raw events were not asked for:\n%s", body)
	}
	resp, raw = h.do("POST", "/v1/accounts/1/run", map[string]any{"prompt": "p", "stream": true}, "Accept", "application/x-ndjson")
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") || len(lines) != 5 ||
		!strings.Contains(lines[0], `"type":"init"`) || !strings.Contains(lines[4], `"type":"done"`) {
		t.Fatalf("%d %v", resp.StatusCode, lines)
	}
}

// A caller that wants the vendor's own event can still have it, by the same
// field that puts events in a buffered reply.
func TestTheProvidersOwnEventRidesAlongWhenAsked(t *testing.T) {
	h := newHarness(t, Options{})
	_, raw := h.do("POST", "/v1/accounts/1/run",
		map[string]any{"prompt": "p", "stream": true, "include_events": true}, "Accept", "application/x-ndjson")
	body := string(raw)
	if !strings.Contains(body, `"raw":`) {
		t.Fatalf("no raw event:\n%s", body)
	}
	// Whatever rides along, the event is still rota's.
	if !strings.Contains(body, `"type":"text"`) {
		t.Fatalf("the normalized event must still be there:\n%s", body)
	}
}

func TestRunDangerousModesNeedServerConsent(t *testing.T) {
	h := newHarness(t, Options{})
	for _, body := range []map[string]any{
		{"prompt": "p", "permission_mode": "bypassPermissions"},
		{"prompt": "p", "dangerously_skip_permissions": true},
		{"prompt": "p", "allow_dangerously_skip_permissions": true},
	} {
		if code, _, raw := h.run(1, body); code != 403 {
			t.Fatalf("%v: %d %s", body, code, raw)
		}
	}
	for _, body := range []map[string]any{
		{"prompt": "p", "sandbox": "danger-full-access"},
		{"prompt": "p", "dangerously_bypass_approvals_and_sandbox": true},
		{"prompt": "p", "dangerously_bypass_hook_trust": true},
	} {
		if code, _, raw := h.run(2, body); code != 403 {
			t.Fatalf("%v: %d %s", body, code, raw)
		}
	}
	h2 := newHarness(t, Options{AllowDangerous: true})
	if code, out, raw := h2.run(1, map[string]any{"prompt": "p", "permission_mode": "bypassPermissions", "dangerously_skip_permissions": true}); code != 200 ||
		!strings.Contains(out.Result, "--permission-mode bypassPermissions --dangerously-skip-permissions") {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestRunConfinesPathsToRoots(t *testing.T) {
	h := newHarness(t, Options{})
	outside := t.TempDir()
	for _, body := range []map[string]any{
		{"prompt": "p", "cwd": outside},
		{"prompt": "p", "add_dirs": []string{outside}},
		{"prompt": "p", "cwd": h.root + "/../"},
		{"prompt": "p", "files": []map[string]string{{"path": "../escape.txt", "content": "aGk="}}},
		{"prompt": "p", "files": []map[string]string{{"path": "/abs.txt", "content": "aGk="}}},
		{"prompt": "p", "cwd": filepath.Join(h.root, "missing")},
	} {
		if code, _, raw := h.run(1, body); code != 400 {
			t.Fatalf("%v: %d %s", body, code, raw)
		}
	}
	realRoot, _ := filepath.EvalSymlinks(h.root)
	if code, out, raw := h.run(1, map[string]any{"prompt": "p"}); code != 200 || !strings.Contains(out.Result, "CWD="+realRoot) {
		t.Fatalf("default cwd must be the first root: %d %s", code, raw)
	}
	open := newHarness(t, Options{Roots: []string{}})
	realOutside, _ := filepath.EvalSymlinks(outside)
	if code, out, raw := open.run(1, map[string]any{"prompt": "p", "cwd": outside}); code != 200 || !strings.Contains(out.Result, "CWD="+realOutside) {
		t.Fatalf("without roots any cwd goes: %d %s", code, raw)
	}
}

func TestRunUploadsLandInPrivateDirAddedToTheSession(t *testing.T) {
	h := newHarness(t, Options{})
	code, out, raw := h.run(1, map[string]any{"prompt": "p", "files": []map[string]string{
		{"path": "notes/a.txt", "content": base64.StdEncoding.EncodeToString([]byte("hi"))},
		{"path": "b.png", "content": base64.StdEncoding.EncodeToString([]byte{1, 2})},
	}})
	if code != 200 || !strings.Contains(out.Result, "FILES=a.txt,b.png") && !strings.Contains(out.Result, "FILES=b.png,a.txt") {
		t.Fatalf("%d %s", code, raw)
	}
	dir := strings.TrimSuffix(strings.Split(strings.SplitAfter(out.Result, "--add-dir ")[1], " ")[0], "\n")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("upload dir %q must be removed after the run", dir)
	}

	// multipart: a JSON "request" part plus file parts
	var buf bytes.Buffer
	mw := newMultipart(&buf)
	mw.field("request", `{"prompt":"mp"}`)
	mw.file("files", "c.md", "# c")
	ct := mw.close()
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/accounts/1/run", &buf)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", ct)
	resp, _ := http.DefaultClient.Do(req)
	rawb, _ := io.ReadAll(resp.Body)
	json.Unmarshal(rawb, &out)
	if resp.StatusCode != 200 || !strings.Contains(out.Result, "PROMPT=mp") || !strings.Contains(out.Result, "FILES=c.md") {
		t.Fatalf("multipart: %d %s", resp.StatusCode, rawb)
	}
}

func TestRunRejectsBadRequests(t *testing.T) {
	h := newHarness(t, Options{})
	cases := []struct {
		id   int
		body map[string]any
		want int
	}{
		{1, map[string]any{"prompt": "p", "bogus": 1}, 400},
		{1, map[string]any{}, 400},
		{1, map[string]any{"prompt": "p", "effort": "ultra"}, 400},
		{1, map[string]any{"prompt": "p", "permission_mode": "yolo"}, 400},
		{2, map[string]any{"prompt": "p", "sandbox": "none"}, 400},
		{1, map[string]any{"prompt": "p", "args": []string{"--verbose"}}, 400},
		{1, map[string]any{"prompt": "p", "args": []string{"--dangerously-skip-permissions"}}, 400},
		{1, map[string]any{"prompt": "p", "settings": map[string]any{"env": map[string]string{"X": "1"}}}, 400},
		// A plugin is code from wherever the URL points, and a codex config
		// override names the endpoint the run is sent to. Both are the same
		// trust as a raw flag, and this server was not given it.
		{1, map[string]any{"prompt": "p", "plugin_urls": []string{"https://x/p.zip"}}, 400},
		{2, map[string]any{"prompt": "p", "config": map[string]string{"model_providers.evil.base_url": "http://127.0.0.1:1"}}, 400},
		{99, map[string]any{"prompt": "p"}, 404},
		{4, map[string]any{"prompt": "p"}, 409},
		{1, map[string]any{"prompt": "p", "timeout_seconds": -1}, 400},
	}
	for _, c := range cases {
		if code, _, raw := h.run(c.id, c.body); code != c.want || !strings.Contains(string(raw), `"error"`) {
			t.Fatalf("%d %v: got %d want %d: %s", c.id, c.body, code, c.want, raw)
		}
	}
	t.Setenv("FAKE_EXIT", "1")
	if code, out, raw := h.run(1, map[string]any{"prompt": "p"}); code != 502 || out.ExitCode != 3 || !strings.Contains(out.Stderr, "stderr-line") || !strings.Contains(out.Result, "PROMPT=p") {
		t.Fatalf("child failure: %d %s", code, raw)
	}
}

func TestRunCodexMapsFieldsAndReadsLastMessage(t *testing.T) {
	// config is among the fields checked here, so this server is one that
	// allows it; the refusal is in the table above.
	h := newHarness(t, Options{AllowRawFlags: true})
	code, out, raw := h.run(2, map[string]any{
		"prompt": "fix it", "sandbox": "read-only", "approve_for_me": true,
		"profile": "work", "config": map[string]string{"a": "1"}, "enable": []string{"f1"}, "disable": []string{"f2"}, "strict_config": true,
		"json_schema": map[string]any{"type": "object"}, "ephemeral": true, "skip_git_repo_check": true, "ignore_user_config": true, "ignore_rules": true,
		"add_dirs": []string{filepath.Join(h.root, "sub")},
		"files":    []map[string]string{{"path": "shot.png", "content": "AQI="}}, "images": []string{"shot.png"},
	})
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	for _, want := range []string{"PROMPT=fix it", "ARGS=exec - --json --color never", "-s read-only", "--approve-for-me",
		"-p work", "-c a=1", "--enable f1", "--disable f2", "--strict-config", "--output-schema ", "--ephemeral", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules",
		"--add-dir " + filepath.Join(h.root, "sub"), "-i ", string(os.PathSeparator) + "shot.png"} {
		if !strings.Contains(out.Result, want) {
			t.Fatalf("missing %q in %q", want, out.Result)
		}
	}
	if !strings.Contains(out.Result, "HOME=") || strings.Contains(out.Result, "HOME= ") {
		t.Fatalf("CODEX_HOME must be staged: %q", out.Result)
	}
	if out.SessionID != "t-1" || out.Provider != "codex" || !strings.Contains(string(out.Usage), `"output_tokens":2`) {
		t.Fatalf("%+v", out)
	}
	_, out, _ = h.run(2, map[string]any{"prompt": "again", "resume": "t-1"})
	if !strings.Contains(out.Result, "ARGS=exec resume t-1 -") {
		t.Fatalf("resume: %q", out.Result)
	}
	_, out, _ = h.run(2, map[string]any{"prompt": "again", "resume": "last", "fork_session": true})
	if !strings.Contains(out.Result, "ARGS=exec fork --last -") {
		t.Fatalf("fork: %q", out.Result)
	}
	resp, rawb := h.do("POST", "/v1/accounts/2/run", map[string]any{"prompt": "p", "stream": true})
	if resp.StatusCode != 200 || !strings.Contains(string(rawb), "event: init") || !strings.Contains(string(rawb), "event: text") ||
		!strings.Contains(string(rawb), "event: done") {
		t.Fatalf("codex stream: %d %s", resp.StatusCode, rawb)
	}
}

func TestRunTimesOutAndKillsTheChild(t *testing.T) {
	h := newHarness(t, Options{Timeout: time.Second})
	t.Setenv("FAKE_SLEEP", "3s")
	start := time.Now()
	code, _, raw := h.run(1, map[string]any{"prompt": "p"})
	if code != 504 || time.Since(start) > 2*time.Second {
		t.Fatalf("%d after %v: %s", code, time.Since(start), raw)
	}
	h2 := newHarness(t, Options{})
	t.Setenv("FAKE_SLEEP", "3s")
	start = time.Now()
	if code, _, raw := h2.run(1, map[string]any{"prompt": "p", "timeout_seconds": 1}); code != 504 || time.Since(start) > 2*time.Second {
		t.Fatalf("per-request timeout: %d after %v: %s", code, time.Since(start), raw)
	}
}

func TestStreamStopsWhenClientLeaves(t *testing.T) {
	h := newHarness(t, Options{})
	t.Setenv("FAKE_SLEEP", "3s")
	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{"prompt": "p", "stream": true})
	req, _ := http.NewRequestWithContext(ctx, "POST", h.srv.URL+"/v1/accounts/1/run", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(resp.Body)
	if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, "event: init") {
		t.Fatalf("first event: %q", line)
	}
	cancel()
	os.Setenv("FAKE_SLEEP", "") // the next child must not sleep
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if code, _, _ := h.run(3, map[string]any{"prompt": "p"}); code != 200 || time.Since(start) > 2*time.Second {
		t.Fatalf("server stuck after the client left: %d in %v", code, time.Since(start))
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	h := newHarness(t, Options{MaxConcurrent: 1})
	t.Setenv("FAKE_SLEEP", "600ms")
	start := time.Now()
	done := make(chan int, 2)
	for range 2 {
		go func() { code, _, _ := h.run(3, map[string]any{"prompt": "p"}); done <- code }()
	}
	if a, b := <-done, <-done; a != 200 || b != 200 || time.Since(start) < 1100*time.Millisecond {
		t.Fatalf("codes %d %d after %v: runs were not serialized", a, b, time.Since(start))
	}
}

// A run waiting for a slot holds nothing: the store stays open to every
// other request while it queues, so a full server still lists, patches
// and logs in.
func TestAQueuedRunHoldsNoLock(t *testing.T) {
	h := newHarness(t, Options{MaxConcurrent: 1})
	t.Setenv("FAKE_SLEEP", "1500ms")
	for range 2 {
		go h.run(3, map[string]any{"prompt": "p"})
	}
	time.Sleep(200 * time.Millisecond) // both runs are in: one running, one queued
	answered := make(chan int, 1)
	go func() { resp, _ := h.do("GET", "/v1/accounts", nil); answered <- resp.StatusCode }()
	select {
	case code := <-answered:
		if code != 200 {
			t.Fatalf("listing while a run is queued: %d", code)
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("a queued run held the store lock and the listing waited on it")
	}
}

// apiKeyProvider stands in for a provider whose login is a pasted key.
type apiKeyProvider struct{}

func (apiKeyProvider) Name() string { return "t-api-fake" }
func (apiKeyProvider) Begin(_ context.Context) (string, map[string]string, error) {
	return "https://example.test/keys", map[string]string{"kind": "apikey"}, nil
}
func (apiKeyProvider) Complete(_ context.Context, key string, _ map[string]string) (*rota.Token, error) {
	return &rota.Token{Access: key, Identity: &rota.Identity{UUID: "key-" + key}}, nil
}
func (apiKeyProvider) Launch(a *rota.Account, _ string) (*rota.Command, error) {
	return &rota.Command{Bin: "claude", Env: []string{"FAKE=" + a.Token.Access}}, nil
}

func TestLoginAndRemoveEndpoints(t *testing.T) {
	rota.Register(apiKeyProvider{})
	h := newHarness(t, Options{})
	resp, raw := h.do("POST", "/v1/login", map[string]string{"provider": "t-api-fake"})
	var login struct{ ID, Provider, URL, Kind string }
	json.Unmarshal(raw, &login)
	if resp.StatusCode != 200 || len(login.ID) != 6 || login.Kind != "apikey" || login.Provider != "t-api-fake" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if resp, raw := h.do("POST", "/v1/login", map[string]string{"provider": "nope"}); resp.StatusCode != 400 || !strings.Contains(string(raw), "unknown provider") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if resp, raw := h.do("POST", "/v1/login/zzzzzz", map[string]string{"code": "k"}); resp.StatusCode != 404 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	resp, raw = h.do("POST", "/v1/login/"+login.ID, map[string]string{"code": "sk-new"})
	var fin struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	}
	json.Unmarshal(raw, &fin)
	if resp.StatusCode != 200 || fin.ID != 5 || fin.Status != "added" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if resp, raw := h.do("DELETE", "/v1/accounts/5", nil); resp.StatusCode != 200 || !strings.Contains(string(raw), `"removed"`) {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if resp, _ := h.do("DELETE", "/v1/accounts/5", nil); resp.StatusCode != 404 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if resp, _ := h.do("DELETE", "/v1/accounts/abc", nil); resp.StatusCode != 400 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestTheRootSchemaAndPlayground(t *testing.T) {
	h := newHarness(t, Options{})
	resp, err := http.Get(h.srv.URL + "/playground")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(page), "/v1/accounts") {
		t.Fatalf("playground: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// The root answers for itself, unauthenticated, and says where the page
	// is rather than serving it.
	root, err := http.Get(h.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	var hello map[string]any
	rawRoot, _ := io.ReadAll(root.Body)
	json.Unmarshal(rawRoot, &hello)
	if root.StatusCode != 200 || !strings.HasPrefix(root.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("/ must answer with JSON: %d %s", root.StatusCode, root.Header.Get("Content-Type"))
	}
	// Alive, and which version: nothing else. The name is in the URL that
	// was just asked, and the page is one level of help away.
	if hello["success"] != true || hello["version"] != wire.Version || len(hello) != 2 {
		t.Fatalf("/ must say only that it is alive and which version: %s", rawRoot)
	}
	if len(rawRoot) > 140 {
		t.Fatalf("/ must say only that: %s", rawRoot)
	}
	if resp, _ := http.Get(h.srv.URL + "/v1/accounts"); resp.StatusCode != 401 {
		t.Fatalf("accounts without a token: %d", resp.StatusCode)
	}
	// The two endpoints that used to say the same thing three ways are gone.
	for _, gone := range []string{"/healthz", "/v1/ping"} {
		if resp, _ := http.Get(h.srv.URL + gone); resp.StatusCode != 404 {
			t.Fatalf("%s must be gone, the root answers for it: %d", gone, resp.StatusCode)
		}
	}

	resp2, raw := h.do("GET", "/v1/schema", nil)
	var doc struct {
		Version   string `json:"version"`
		Providers map[string]struct {
			Flavor   string       `json:"flavor"`
			Models   []rota.Model `json:"models"`
			Efforts  []string     `json:"efforts"`
			Metered  bool         `json:"metered"`
			Defaults struct{ Model, Effort string }
			Fields   []wire.Field `json:"fields"`
		} `json:"providers"`
		Fields         []wire.Field `json:"fields"`
		AllowDangerous bool         `json:"allow_dangerous"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || resp2.StatusCode != 200 {
		t.Fatalf("%d %v %s", resp2.StatusCode, err, raw)
	}
	cl, ok := doc.Providers["claude"]
	if !ok || cl.Flavor != "claude" || len(cl.Models) == 0 || cl.Defaults.Model == "" || !cl.Metered {
		t.Fatalf("claude: %+v", cl)
	}
	if cx := doc.Providers["codex"]; cx.Flavor != "codex" || cx.Metered {
		t.Fatalf("codex drives its own CLI and publishes no usage endpoint: %+v", cx)
	}
	names := map[string]bool{}
	for _, f := range cl.Fields {
		names[f.Name] = true
	}
	for _, want := range []string{"prompt", "stream", "model", "effort", "permission_mode", "json_schema", "files", "args"} {
		if want == "files" {
			continue // files is an HTTP-only field, described below
		}
		if !names[want] {
			t.Fatalf("claude fields lack %q", want)
		}
	}
	if names["sandbox"] {
		t.Fatal("claude must not be offered codex-only fields")
	}
	if cx := doc.Providers["codex"]; len(cx.Fields) == 0 {
		t.Fatal("codex fields")
	}
	if len(doc.Fields) == 0 || doc.AllowDangerous || doc.Version == "" {
		t.Fatalf("%+v", doc)
	}
}

func TestTenBadTokensBlockTheAddressForAnHour(t *testing.T) {
	h := newHarness(t, Options{})
	bad := func() int {
		req, _ := http.NewRequest("GET", h.srv.URL+"/v1/accounts", nil)
		req.Header.Set("Authorization", "Bearer nope")
		resp, _ := http.DefaultClient.Do(req)
		return resp.StatusCode
	}
	for i := range 10 {
		if code := bad(); code != 401 {
			t.Fatalf("attempt %d: %d", i+1, code)
		}
	}
	if code := bad(); code != 429 {
		t.Fatalf("11th bad token must be blocked: %d", code)
	}
	// The block is for guesses. The right token is not a guess, and an
	// address shared with an attacker — a proxy, or the loopback a web page
	// can reach — must not lock the operator out of their own server.
	if resp, _ := h.do("GET", "/v1/accounts", nil); resp.StatusCode != 200 {
		t.Fatalf("the right token is admitted while the address is blocked: %d", resp.StatusCode)
	}
	if code := bad(); code != 429 {
		t.Fatalf("and wrong ones stay blocked: %d", code)
	}
	if resp, _ := http.Get(h.srv.URL + "/"); resp.StatusCode != 200 {
		t.Fatal("the root is never blocked: it is what a watchdog reads")
	}
	l := newLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	for range 10 {
		l.fail("1.2.3.4")
	}
	if !l.blocked("1.2.3.4") || l.blocked("5.6.7.8") {
		t.Fatal("limiter state")
	}
	now = now.Add(61 * time.Minute)
	if l.blocked("1.2.3.4") {
		t.Fatal("block must lift after an hour")
	}
	for range 9 {
		l.fail("9.9.9.9")
	}
	now = now.Add(61 * time.Minute)
	l.fail("9.9.9.9")
	if l.blocked("9.9.9.9") {
		t.Fatal("old failures must not count in a new window")
	}
}

type mp struct{ w *multipart.Writer }

func newMultipart(w io.Writer) *mp     { return &mp{multipart.NewWriter(w)} }
func (m *mp) field(name, value string) { m.w.WriteField(name, value) }
func (m *mp) file(field, name, content string) {
	f, _ := m.w.CreateFormFile(field, name)
	f.Write([]byte(content))
}
func (m *mp) close() string { m.w.Close(); return m.w.FormDataContentType() }

// The account list can carry what the CLIs are doing, on request. It is not
// sent by default: answering means reading directories other programs own,
// and a dashboard polling this endpoint should not pay for that every tick.
func TestAccountsCanCarryTheSessionsOnRequest(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	full := "abcdef12-3456-7890-abcd-ef1234567890"
	dir := filepath.Join(claude, "projects", "-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, full+".jsonl"), []byte(`{"type":"user","cwd":"/x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{})

	// Not asked for, not sent.
	_, raw := h.do("GET", "/v1/accounts", nil)
	if strings.Contains(string(raw), `"sessions"`) {
		t.Fatalf("sessions must be asked for: %s", raw)
	}

	resp, raw := h.do("GET", "/v1/accounts?sessions=1", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	for _, want := range []string{`"sessions"`, `"instances"`, full} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
	// The same shapes the CLI prints, so a caller can move between the two
	// without changing what reads them.
	if !strings.Contains(string(raw), `"shared"`) {
		t.Fatalf("a conversation nobody owns must say so: %s", raw)
	}
}

// A running server is where "which account is running what" matters most: it
// takes several agents at once, and unless each names a project of its own
// they all read the same ~/.claude, so nothing on disk says whose quota is
// paying.
//
// The server records its runs the way the command line does. It did not at
// first: the registry was wired into the CLI only, so the one place with
// concurrent runs contributed nothing to the listing.
func TestTheServerRecordsTheRunsItStarts(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	h := newHarness(t, Options{})

	// A CLI that announces its session and then takes its time, as the real
	// ones do: the run has to still be going when the listing is read.
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(
		`{"type":"system","subtype":"init","session_id":"s-server"}`,
		"{{sleep:1s}}",
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s-server","result":"ok","total_cost_usd":0.01}`,
	))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Streaming, because that is when the conversation can be known while the
	// run is still going: a buffered run's CLI prints one document at the end,
	// so there is nothing to read until there is nothing left to say.
	go h.do("POST", "/v1/accounts/1/run", map[string]any{"prompt": "hi", "stream": true})

	// Both facts are looked for while the run is going, and neither has to
	// arrive in the same reading: the account is known when the run starts,
	// the conversation only once the CLI has said what it is.
	var sawAccount, sawSession bool
	var last string
	for range 60 {
		time.Sleep(50 * time.Millisecond)
		_, raw := h.do("GET", "/v1/accounts?sessions=1", nil)
		last = string(raw)
		// The HTTP body is compact, so the assertions are too.
		sawAccount = sawAccount || strings.Contains(last, `"kind":"cli","provider":"claude","account":1`)
		sawSession = sawSession || strings.Contains(last, `"session":"s-server"`)
		if sawAccount && sawSession {
			return
		}
	}
	t.Fatalf("account seen: %v, conversation seen: %v; last listing:\n%s", sawAccount, sawSession, last)
}

// The CLI calls it login and the API called it auth, for one act. It is
// /v1/login now, and /v1/auth goes on working: a published path that starts
// refusing requests breaks whoever was calling it, and one name is worth
// having without that.
func TestLoginIsOneEndpointUnderTwoNames(t *testing.T) {
	rota.Register(apiKeyProvider{})
	h := newHarness(t, Options{})
	for _, base := range []string{"/v1/login", "/v1/auth"} {
		resp, raw := h.do("POST", base, map[string]string{"provider": "t-api-fake"})
		var login struct{ ID, Kind, Provider string }
		json.Unmarshal(raw, &login)
		if resp.StatusCode != 200 || len(login.ID) != 6 || login.Provider != "t-api-fake" {
			t.Fatalf("%s begins a login: %d %s", base, resp.StatusCode, raw)
		}
		resp, raw = h.do("POST", base+"/"+login.ID, map[string]string{"code": "sk-" + login.ID})
		var fin struct{ Status string }
		json.Unmarshal(raw, &fin)
		if resp.StatusCode != 200 || fin.Status != "added" {
			t.Fatalf("%s finishes it: %d %s", base, resp.StatusCode, raw)
		}
	}
}
