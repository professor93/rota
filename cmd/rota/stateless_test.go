package main

import (
	"github.com/professor93/rota/internal/fakecli"
	"strings"
	"testing"
)

// --stateless is one word for "answer from nothing": per provider it maps to
// the CLI's own spelling, and a provider whose CLI cannot spell it is refused
// rather than half-promised.
func TestStatelessMapsPerProviderOrRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROTA_HOME", dir)
	seedAccounts(t, dir, `{"accounts":[
		{"id":1,"provider":"claude","uuid":"c1","order":1,"token":{"accessToken":"tok"}},
		{"id":8,"provider":"grok","uuid":"g1","order":2,"token":{"accessToken":"xai-key"}}],"nextId":9,"ordered":true}`)

	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(`{"type":"result","subtype":"success","is_error":false,"session_id":"s1","result":"ARGS={{args}} CONFIGDIR={{env:CLAUDE_CONFIG_DIR|NONE}}","num_turns":1}`))
	t.Setenv("PATH", bin)

	out, errb, code := call(t, "run", "1", "--stateless", "hello")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	for _, want := range []string{"--no-session-persistence", "--setting-sources "} {
		if !strings.Contains(out, want) {
			t.Fatalf("stateless must spell %q for claude, got: %s", want, out)
		}
	}
	// ...and a throwaway config dir, so the shared home's identity and
	// memory cannot reach the model's context.
	if !strings.Contains(out, "CONFIGDIR=") || strings.Contains(out, "CONFIGDIR=NONE") {
		t.Fatalf("stateless must isolate CLAUDE_CONFIG_DIR: %s", out)
	}

	// grok's CLI has no flags to skip its own state: refuse, don't pretend.
	_, errb, code = call(t, "run", "8", "--stateless", "hello")
	if code != 2 || !strings.Contains(errb, "--stateless is not available for grok") {
		t.Fatalf("grok must be refused honestly: exit %d, %s", code, errb)
	}
}
