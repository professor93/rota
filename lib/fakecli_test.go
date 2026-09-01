package rota

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeCLI writes a shell script that behaves like a vendor CLI: it echoes
// what it was given as a result event, so a test can assert on argv, stdin
// and environment without a network or a real CLI.
func fakeCLI(t *testing.T, name string, extraLine, exitCode string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if exitCode == "" {
		exitCode = "0"
	}
	script := `#!/bin/sh
stdin=$(cat)
echo "` + extraLine + `"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"s-fake","result":"STDIN=%s ARGS=%s","num_turns":1,"total_cost_usd":0.5}\n' "$stdin" "$*"
echo "fake-stderr" >&2
exit ` + exitCode + `
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return name
}
