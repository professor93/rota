//go:build unix

package rota

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// A CLI that daemonises a helper — detached into a process group of its own,
// still holding the CLI's stdout and stderr — leaves the pipes open after the
// CLI itself is gone. Reading to EOF then waits for the helper, which is not
// what the caller's timeout meant; the run must end a bounded time after
// the deadline.
func TestARunEndsWhenItsDeadlineDoesEvenIfAHelperKeepsThePipes(t *testing.T) {
	dir := t.TempDir()
	// The helper leaves the CLI's process group, so killing the group does
	// not reach it, and keeps the inherited stdout and stderr for 30s.
	helperReady := filepath.Join(dir, "helper.ready")
	helper := fakecli.Install(t, dir, "helper", fakecli.Spec{Sleep: "30s", KeepStdin: true, Ready: helperReady})
	bin := fakecli.Install(t, dir, "daemonising-cli", fakecli.Spec{Detach: helper, KeepStdin: true,
		Stdout: []string{`{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"ok","num_turns":1}`}})
	t.Cleanup(func() {
		if raw, err := os.ReadFile(helperReady); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	a := &Account{ID: 1, Provider: "claude"}
	a.Token.Access = "tok"
	given := &Command{Bin: bin, BaseEnv: []string{"PATH=/usr/bin:/bin"}}
	var out bytes.Buffer
	start := time.Now()
	_, err := Run(context.Background(), a, "", given, Spec{Prompt: "p", TimeoutSeconds: 1, flavorOverride: "claude"}, nil, &out)
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("the run outlived its deadline by %v: a helper holding the pipes must not pin it", took-time.Second)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a run that hit its deadline says so: %v", err)
	}
	// And the helper was there to hold them, out of the group's reach: a CLI
	// that died before it started one would end at the deadline too, and
	// prove nothing.
	if pid := fakecli.WaitReady(t, helperReady); syscall.Kill(pid, 0) != nil {
		t.Fatal("the helper is gone: killing the CLI's group reached it")
	}
}
