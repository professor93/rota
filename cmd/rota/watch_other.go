//go:build !unix

package main

import "errors"

// Windows keeps every claude account on a token in its environment, so no
// account is ever Watchable there and no watcher is started. `rota serve
// watch` still answers, in the foreground, and finds nothing to keep.

func watchInBackground(int) error { return errors.New("no watcher on this platform") }

func ownSession() bool { return true }

func detachWatcher(int) error { return nil }
