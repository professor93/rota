package rota

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// Asked to record the command, a run reports what it ran: the binary, the
// arguments as built, and the names of the variables set for it and kept
// from it. Names only — the value of the one that matters is a credential.
func TestARunRecordsItsCommandWhenAsked(t *testing.T) {
	bin := fakecli.Install(t, t.TempDir(), "claude", fakecli.Lines(
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"ok","num_turns":1}`,
	))
	Register(&fakeProvider{name: "t-run-argv", launched: &Command{Bin: bin, Env: []string{"CLAUDE_CODE_OAUTH_TOKEN=secret-token"}, Drop: []string{"ANTHROPIC_API_KEY"}}})
	a := &Account{ID: 1, Provider: "t-run-argv"}
	a.Token.Access = "tok"

	res, err := Run(context.Background(), a, "", nil, Spec{Prompt: "hello", Model: "opus", flavorOverride: "claude"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Argv != nil || res.EnvSet != nil || res.EnvDropped != nil {
		t.Fatalf("unasked, nothing is recorded: %+v", res)
	}
	res, err = Run(context.Background(), a, "", nil, Spec{Prompt: "hello", Model: "opus", IncludeArgv: true, flavorOverride: "claude"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Argv) == 0 || res.Argv[0] != bin || !slices.Contains(res.Argv, "--model") {
		t.Fatalf("argv is the binary and the arguments as built: %v", res.Argv)
	}
	if strings.Join(res.EnvSet, ",") != "CLAUDE_CODE_OAUTH_TOKEN,ROTA_PROVIDER,ROTA_ACCOUNT_ID,ROTA_ACCOUNT" || !slices.Contains(res.EnvDropped, "ANTHROPIC_API_KEY") {
		t.Fatalf("env is names: set %v dropped %v", res.EnvSet, res.EnvDropped)
	}
	if raw, _ := Encode(res); strings.Contains(string(raw), "secret-token") {
		t.Fatalf("a value must never be recorded: %s", raw)
	}
}
