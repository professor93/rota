package rota

import (
	"os"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// fakeCLI installs a stand-in vendor CLI on PATH: it prints extraLine (one
// or more lines) and then a result event echoing stdin and argv, so a test
// can assert on what reached it without a network or a real CLI.
func fakeCLI(t *testing.T, name string, extraLine, exitCode string) string {
	t.Helper()
	dir := t.TempDir()
	spec := fakecli.Result(0)
	if exitCode != "" && exitCode != "0" {
		spec.Exit = 1
		if exitCode == "3" {
			spec.Exit = 3
		}
	}
	spec.Stdout = append(strings.Split(extraLine, "\n"), spec.Stdout...)
	fakecli.Install(t, dir, name, spec)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return name
}
