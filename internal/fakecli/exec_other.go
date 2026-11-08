//go:build !linux

package fakecli

import "errors"

// execFromThread is Linux's: only there does an exec from another thread show
// anything a test needs to see.
func execFromThread(string) error { return errors.New("an exec from a thread is a Linux fake") }
