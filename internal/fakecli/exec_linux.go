package fakecli

import (
	"os"
	"runtime"
	"syscall"
)

// execFromThread replaces the fake with path from a thread other than the
// main one, and returns only if that failed. A try that finds itself on the
// main thread keeps it, so the next lands elsewhere.
func execFromThread(path string) error {
	failed := make(chan error, 1)
	var try func()
	try = func() {
		runtime.LockOSThread()
		if syscall.Gettid() == os.Getpid() {
			go try()
			select {}
		}
		failed <- syscall.Exec(path, []string{path}, os.Environ())
	}
	go try()
	return <-failed
}
