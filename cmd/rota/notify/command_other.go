//go:build !unix

package notify

import "os/exec"

// ownGroup does nothing here: a process group is a unix thing, and only the
// program itself is killed.
func ownGroup(*exec.Cmd) {}

// killGroup kills cmd's program.
func killGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }
