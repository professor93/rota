//go:build !unix

package fakecli

import "os/exec"

// detach leaves cmd where it is: no test here detaches a helper off unix,
// where a process group is not something a CLI leaves the same way.
func detach(*exec.Cmd) {}
