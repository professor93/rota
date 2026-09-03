//go:build unix

package rota

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A CLI that daemonises a helper — detached into its own session, still
// holding the CLI's stdout and stderr — leaves the pipes open after the CLI
// itself is gone. Reading to EOF then waits for the helper, which is not
// what the caller's timeout meant; the run must end a bounded time after
// the deadline.
func TestARunEndsWhenItsDeadlineDoesEvenIfAHelperKeepsThePipes(t *testing.T) {
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("needs perl to detach a helper into its own process group")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "helper.pid")
	// The helper leaves the CLI's process group, so killing the group does
	// not reach it, and keeps the inherited stdout and stderr for 30s.
	script := "#!/bin/sh\n" +
		perl + " -e 'setpgrp(0,0); sleep 30' &\n" +
		"echo $! > " + pidFile + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"ok","num_turns":1}'` + "\n"
	bin := filepath.Join(dir, "daemonising-cli")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if raw, err := os.ReadFile(pidFile); err == nil {
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
	_, err = Run(context.Background(), a, "", given, Spec{Prompt: "p", TimeoutSeconds: 1, flavorOverride: "claude"}, nil, &out)
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("the run outlived its deadline by %v: a helper holding the pipes must not pin it", took-time.Second)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a run that hit its deadline says so: %v", err)
	}
}
