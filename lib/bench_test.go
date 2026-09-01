package rota

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// The paths every run touches. They are benchmarked so a change that makes
// rota slower or greedier shows up as a number rather than as a feeling.

func BenchmarkArgvClaude(b *testing.B) {
	spec := Spec{
		Prompt: "summarise this repository", Model: "sonnet", Effort: "high",
		PermissionMode: "plan", AllowedTools: []string{"Bash(git *)", "Read"},
		SettingSources: []string{}, AddDirs: []string{"/tmp"},
		JSONSchema: json.RawMessage(`{"type":"object"}`), Stream: true,
	}
	b.ReportAllocs()
	for b.Loop() {
		argv, err := spec.argv("claude", nil)
		if err != nil {
			b.Fatal(err)
		}
		_ = argv
	}
}

func BenchmarkEnviron(b *testing.B) {
	inherited := make([]string, 0, 64)
	for _, k := range []string{"PATH", "HOME", "SHELL", "TERM", "LANG", "PWD", "USER", "TMPDIR"} {
		inherited = append(inherited, k+"=/some/value")
	}
	for i := range 56 {
		inherited = append(inherited, "VAR"+string(rune('A'+i%26))+"=x")
	}
	cmd := &Command{
		Env:  []string{"CLAUDE_CODE_OAUTH_TOKEN=tok"},
		Drop: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"},
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = Environ(inherited, cmd)
	}
}

// BenchmarkScanEvents is the streaming hot path: one line per event, and a
// long run emits thousands.
func BenchmarkScanEvents(b *testing.B) {
	var sb strings.Builder
	for range 200 {
		sb.WriteString(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"text":"some words here"}}}` + "\n")
	}
	sb.WriteString(`{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"done","num_turns":3,"total_cost_usd":0.01}` + "\n")
	payload := sb.String()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		res := &Result{}
		if err := scanEvents(strings.NewReader(payload), io.Discard, false, (*Limits)(nil).caps(), res); err != nil {
			b.Fatal(err)
		}
		if res.Result != "done" {
			b.Fatalf("%q", res.Result)
		}
	}
}
