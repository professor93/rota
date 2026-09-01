package rota

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The SDK reads no environment. Which variables exist, which are secret, and
// what a child may inherit are all the application's facts; a library that
// consults os.Getenv or os.Environ has smuggled a decision in. This scans the
// package's own production sources so the rule cannot erode quietly.
func TestTheSDKReadsNoEnvironment(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"os.Getenv", "os.LookupEnv", "os.Environ("} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s uses %s: the environment belongs to the application, which passes what the SDK needs", name, banned)
			}
		}
	}
}

// The child's environment is exactly what the caller supplies as
// Command.BaseEnv plus the credential entries — nothing inherited from the
// process behind the caller's back. A parent-only variable must not leak in,
// and a BaseEnv entry must arrive verbatim.
func TestTheChildEnvironmentComesOnlyFromTheCaller(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "envcli")
	script := `#!/bin/sh
exec </dev/null
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"s-env","result":"M=%s P=%s","num_turns":1}\n' "$MARKER" "$PARENT_ONLY"
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PARENT_ONLY", "leaked")

	a := &Account{ID: 1, Provider: "claude"}
	a.Token.Access = "tok"
	given := &Command{Bin: bin, Env: []string{"FAKE=1"}, BaseEnv: []string{"PATH=/usr/bin:/bin", "MARKER=yes"}}
	var out bytes.Buffer
	res, err := Run(context.Background(), a, "", given, Spec{Prompt: "p", flavorOverride: "claude"}, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Result, "M=yes") {
		t.Fatalf("a BaseEnv entry must reach the child verbatim: %q", res.Result)
	}
	if strings.Contains(res.Result, "leaked") {
		t.Fatalf("the process environment must not leak into the child: %q", res.Result)
	}
}
