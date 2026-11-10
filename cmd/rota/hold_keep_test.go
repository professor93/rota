//go:build linux || darwin

package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// A held session's record keeps the arguments it was asked for, exactly as
// the person gave them, and never the guard against Remote Control that
// each launch adds afresh: a --settings file stays a path, rather than being
// copied in, and nothing of disableRemoteControl is there for a later launch
// to carry onto an account whose Remote Control is on. The CLI itself is
// handed the guarded arguments.
func TestAHeldSessionsRecordKeepsTheArgumentsAsGiven(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	req := holdRequest(t)
	req.Keep = []string{"--settings", "mine.json", "--model", "opus"}
	req.Args = []string{"--model", "opus", "--settings", `{"disableRemoteControl":true,"model":"y"}`}
	h := startHolderProc(t, terminals, bin, req)
	if !h.ready.OK {
		t.Fatalf("the holder did not start: %+v", h.ready)
	}
	m := h.meta()
	if !slices.Equal(m.Args, req.Keep) || strings.Contains(strings.Join(m.Args, " "), "disableRemoteControl") {
		t.Fatalf("the record keeps %q, want %q as given", m.Args, req.Keep)
	}
}
