//go:build windows

package main

import (
	"os"
	"os/exec"
)

// execCLI runs the vendor CLI to completion on the inherited terminal.
// Windows cannot replace a process, so the CLI's non-zero exit comes back as
// an *exec.ExitError for the caller to map onto its own exit status. argv is
// honored as given — argv[0] included — matching the unix contract.
func execCLI(path string, argv, env []string) error {
	cmd := exec.Command(path)
	cmd.Args = argv
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
