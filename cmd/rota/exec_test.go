package main

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// The handover really replaces the process: the child sees exactly the
// environment given, and its exit status becomes this process's own. Proved
// by re-execing the test binary and letting it exec a shell in its place.
func TestExecHandsTheProcessOver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec is emulated on windows")
	}
	if os.Getenv("ROTA_TEST_EXEC") != "" {
		code := os.Getenv("ROTA_TEST_EXEC")
		err := execCLI("/bin/sh", []string{"sh", "-c", "echo hello $ROTA_MARK; exit " + code}, []string{"ROTA_MARK=env-ok", "PATH=/bin:/usr/bin"})
		t.Fatalf("execCLI returned: %v", err)
	}
	for _, code := range []string{"0", "3"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestExecHandsTheProcessOver$")
		cmd.Env = append(os.Environ(), "ROTA_TEST_EXEC="+code)
		out, err := cmd.Output()
		var ee *exec.ExitError
		exit := 0
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		}
		if strings.TrimSpace(string(out)) != "hello env-ok" || exit != map[string]int{"0": 0, "3": 3}[code] {
			t.Fatalf("code %s: out=%q exit=%d err=%v", code, out, exit, err)
		}
	}
}
