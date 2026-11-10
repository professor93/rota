//go:build linux || darwin

package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
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

// The guard's document reaches the holder on its input, never a command
// line, and the holder writes it where its CLI's arguments name it: in the
// session's own directory, which only this user may enter, readable by this
// user alone, for as long as the CLI runs. A token a person's settings file
// held, folded into it, is in that file and nowhere the machine can read.
func TestAHeldSessionsGuardIsAPrivateFileForTheCLIsLife(t *testing.T) {
	_, terminals := holdStore(t)
	bin := holdFake(t, "claude", fakecli.Spec{})
	req := holdRequest(t)
	guard := holdwire.GuardFile(holdwire.Dir(terminals, req.ID))
	req.Guard = `{"disableRemoteControl":true,"env":{"SOME_TOKEN":"FAKE-VALUE-123"}}`
	req.Keep = []string{"--settings", "sec.json", "--model", "opus"}
	req.Args = []string{"--settings", guard, "--model", "opus", "--settings", guard}
	h := startHolderProc(t, terminals, bin, req)
	if !h.ready.OK {
		t.Fatalf("the holder did not start: %+v", h.ready)
	}
	fi, err := os.Stat(guard)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the guard's file: %v %v, want 0600", fi, err)
	}
	if di, err := os.Stat(h.dir); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("its directory: %v %v, want 0700", di, err)
	}
	if raw, _ := os.ReadFile(guard); string(raw) != req.Guard {
		t.Fatalf("the guard's file holds %s", raw)
	}
	if strings.Contains(strings.Join(h.meta().Args, " "), "FAKE-VALUE-123") {
		t.Fatal("the record holds the token")
	}
	l := h.link()
	l.wait("ready")
	l.typed("bye\r")
	if code := h.code(); code != 0 {
		t.Fatalf("the holder exits %d", code)
	}
	if _, err := os.Stat(guard); !os.IsNotExist(err) {
		t.Fatalf("the guard's file outlives the CLI: %v", err)
	}
}
