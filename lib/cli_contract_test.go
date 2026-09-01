package rota

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestKimiAcceptsWhatRotaBuilds is the same idea as the grok test below: a
// vendor's flags are the one thing a unit test cannot check, so the real CLI
// is asked to parse a real command line. It cannot spend anything — the home
// is empty, so the furthest it gets is "no model configured".
func TestKimiAcceptsWhatRotaBuilds(t *testing.T) {
	if os.Getenv("ROTA_CONTRACT") == "" {
		t.Skip("runs the real vendor binary; set ROTA_CONTRACT=1 to opt in")
	}
	bin, err := exec.LookPath("kimi")
	if err != nil {
		// The CLI installs outside the default PATH; look where it lands.
		home, _ := os.UserHomeDir()
		bin = filepath.Join(home, ".kimi-code", "bin", "kimi")
		if _, err := os.Stat(bin); err != nil {
			t.Skip("kimi is not installed")
		}
	}
	for _, spec := range []Spec{
		{Prompt: "hi"},
		{Prompt: "hi", Stream: true},
		{Prompt: "hi", Model: "k2", PermissionMode: "plan", Agent: "reviewer", Continue: true},
		{Prompt: "hi", PermissionMode: "auto", Resume: "s-1"},
	} {
		argv, err := spec.argv("kimi", nil)
		if err != nil {
			t.Fatalf("%+v: %v", spec, err)
		}
		cmd := exec.Command(bin, argv...)
		cmd.Env = append(os.Environ(), "KIMI_CODE_HOME="+t.TempDir(), "KIMI_API_KEY=")
		out, _ := cmd.CombinedOutput()
		text := string(out)
		for _, complaint := range []string{"unknown option", "error: unknown", "Usage:", "invalid"} {
			if strings.Contains(text, complaint) {
				t.Fatalf("kimi rejected rota's command line (%s):\n%s\nargv: %v", complaint, text, argv)
			}
		}
	}
}

// TestGrokAcceptsWhatRotaBuilds feeds a real command line to the real CLI.
//
// A vendor's flags are the one thing a unit test cannot check: rota can
// assert it wrote "--allow bash" and still be wrong, because only grok knows
// whether that flag exists. So this hands grok the arguments and reads what
// it complains about — an unknown flag fails at parsing, with a usage error,
// well before the run reaches authentication.
//
// It is deliberately impossible for this to spend anything: XAI_API_KEY is
// emptied and GROK_HOME points at a directory with no session in it, so the
// furthest the CLI can get is "not signed in". The same trick is not applied
// to claude or codex, whose own logins on this machine could make a test run
// cost real money.
func TestGrokAcceptsWhatRotaBuilds(t *testing.T) {
	if os.Getenv("ROTA_CONTRACT") == "" {
		t.Skip("runs the real vendor binary; set ROTA_CONTRACT=1 to opt in")
	}
	if _, err := exec.LookPath("grok"); err != nil {
		t.Skip("grok is not installed")
	}
	cases := []struct {
		name string
		spec Spec
	}{
		{"everything at once", Spec{
			Prompt: "hi", Model: "grok-4.6", Effort: "high", PermissionMode: "plan",
			JSONSchema: json.RawMessage(`{"type":"object"}`), MaxTurns: 1, Rules: "be terse",
			SystemPrompt: "sys", AllowedTools: []string{"read"}, DisallowedTools: []string{"bash"},
			Tools: []string{"read", "edit"}, DisableWebSearch: true, NoPlan: true,
			NoSubagents: true, Verbatim: true, Sandbox: "read-only", SessionID: "sid",
		}},
		{"streaming", Spec{Prompt: "hi", Stream: true, IncludePartialMessages: true}},
		{"nothing but a prompt", Spec{Prompt: "hi"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := c.spec
			argv, err := spec.argv("grok", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer spec.cleanup()
			cmd := exec.Command("grok", argv...)
			cmd.Env = append(os.Environ(), "XAI_API_KEY=", "GROK_HOME="+t.TempDir())
			out, _ := cmd.CombinedOutput()
			text := string(out)
			for _, complaint := range []string{"unexpected argument", "invalid value", "unrecognized", "Usage:"} {
				if strings.Contains(text, complaint) {
					t.Fatalf("grok rejected rota's command line (%s):\n%s\nargv: %v", complaint, text, argv)
				}
			}
			if !strings.Contains(text, `"type"`) {
				t.Fatalf("grok answered in an unexpected shape:\n%s", text)
			}
		})
	}
}
