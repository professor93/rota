//go:build !unix

package main

import "os"

// Windows has no exec: the CLI runs as rota's child while rota holds the
// claim, and a child is given no handle of rota's it was not passed. There
// is nothing to lend, and no holder.

func holdInBackground([]*os.File) (claimHolder, error) { return nil, errNoHolder }

func holdLent(int, int) error {
	return usageErr("serve claim is internal, and there is no claim to hold on this platform")
}
