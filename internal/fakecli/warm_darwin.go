//go:build darwin

package fakecli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// warmLimit is how long macOS may take to let a new fake run at all. A test
// of the limit itself shortens it.
var warmLimit = 30 * time.Second

// warmTries is how many times a first run is started when macOS kills it on
// its way in.
const warmTries = 3

// warm runs the fake at path once, doing nothing, and waits for it.
//
// macOS looks at an executable the first time it is started from a new
// path, and the exec waits in the kernel until syspolicyd has answered. A
// signal that arrives meanwhile ends the process before it has run a line,
// a handler of its own for that signal included; an answer that never
// comes leaves it there for good, holding the pipes of whoever started it.
// Run once here, the look is over before any test starts the fake, signals
// it or reads it, and a start macOS never allows fails now, in a bounded
// time, rather than hanging a test later.
//
// Now and then the answer is a refusal: syspolicyd logs "Terminating process
// due to Malware rejection" and SIGKILLs the process inside exec. It is not a
// verdict on the file, which starts the next time, so a first run killed that
// way is started again, within the same limit. A fake that dies the same way
// every time still fails, by name.
func warm(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), warmLimit)
	defer cancel()
	var err error
	for range warmTries {
		cmd := exec.CommandContext(ctx, path, warmArg)
		// Built with -race, a program sleeps a second on its way out unless
		// told not to, and this one has nothing to wait for. The options
		// already set are kept: the last of them wins.
		cmd.Env = append(os.Environ(), "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
		if err = cmd.Run(); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("fake cli %s: a first run that does nothing had not ended after %v", path, warmLimit)
		}
		if !killed(err) {
			break
		}
	}
	return fmt.Errorf("fake cli %s: a first run that does nothing failed: %w", path, err)
}

// killed reports whether a run ended on SIGKILL, which nothing in a run
// that does nothing sends itself.
func killed(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// warmKillEnv lets a test play syspolicyd refusing first runs: "N:file" makes
// each first run add a byte to file and die of SIGKILL while the file holds
// no more than N.
const warmKillEnv = "FAKECLI_WARM_KILL"

// refuseWarm is that stand-in, and returns only for a run it lets through.
func refuseWarm() { playKill(os.Getenv(warmKillEnv)) }
