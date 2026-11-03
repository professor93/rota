//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// watchInBackground runs `rota serve watch <id>` and waits for it, which takes
// only as long as that process needs to start the watcher in a session of its
// own and leave. Its stdin, stdout and stderr are /dev/null, and it is given no
// other descriptor: every one of rota's is close-on-exec until the handover
// clears the claim's, which is after this.
func watchInBackground(id int) error {
	if _, err := sessionOf(0); err != nil {
		// A unix with no getsid: the watcher could not tell it had left this
		// session and would stay in the child this waits for, so none starts.
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, exe, "serve", "watch", strconv.Itoa(id)).Run()
}

// ownSession reports whether this process leads a session of its own. A
// session that cannot be asked about counts as its own: the watcher then runs
// where it is, rather than starting itself again for ever.
func ownSession() bool {
	sid, err := sessionOf(0)
	return err != nil || sid == os.Getpid()
}

// detachWatcher starts the watcher in a session of its own, at /, with nothing
// open but /dev/null, and leaves it: once this process exits it belongs to
// init, not to whatever started this one.
func detachWatcher(id int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "serve", "watch", strconv.Itoa(id))
	cmd.Dir = "/"
	if dir := os.Getenv("ROTA_HOME"); dir != "" {
		// Found from / as it was from where rota was started.
		if abs, err := filepath.Abs(dir); err == nil {
			cmd.Env = append(os.Environ(), "ROTA_HOME="+abs)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
