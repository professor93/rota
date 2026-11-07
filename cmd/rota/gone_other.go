//go:build unix && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package main

// exitWatchable: nothing here says when another process exits, so no holder
// starts and the claim is kept across the exec, as before.
const exitWatchable = false

func exitOf(int) (func(), error) { return nil, errNoHolder }
