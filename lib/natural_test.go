package rota

import (
	"context"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// A run that failed without an answer reports no answer. Its reason stays
// where the CLI put it, in stderr; rota does not move it into result and
// pass it off as what the agent said.
func TestAFailedRunKeepsItsAnswerEmpty(t *testing.T) {
	bin := fakecli.Install(t, t.TempDir(), "claude", fakecli.Spec{Stderr: []string{"Not logged in"}, Exit: 2})
	Register(&fakeProvider{name: "t-run-fail", launched: &Command{Bin: bin}})
	a := &Account{ID: 1, Provider: "t-run-fail"}
	a.Token.Access = "tok"

	res, err := Run(context.Background(), a, "", nil, Spec{Prompt: "hello", flavorOverride: "claude"}, nil, nil)
	if err != nil {
		t.Fatalf("the CLI ran and failed: that is a result, not an error: %v", err)
	}
	if !res.IsError || res.ExitCode != 2 || !strings.Contains(res.Stderr, "Not logged in") {
		t.Fatalf("%+v", res)
	}
	if res.Result != "" {
		t.Fatalf("result must be the answer or nothing, not stderr: %q", res.Result)
	}
}
