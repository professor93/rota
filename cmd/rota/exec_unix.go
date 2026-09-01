//go:build unix

package main

import "syscall"

// execCLI replaces this process with the vendor CLI, so its exit status,
// signals and terminal are its own. argv[0] is the name the CLI sees for
// itself. It only returns on failure.
//
// This lives in the command rather than the SDK: handing the controlling
// terminal over is a terminal program's move, and this program is the only
// thing that makes it.
func execCLI(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
