//go:build unix && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package main

import "errors"

// sessionOf cannot ask here, so a watcher runs where it was started.
func sessionOf(int) (int, error) { return 0, errors.New("no getsid on this platform") }
