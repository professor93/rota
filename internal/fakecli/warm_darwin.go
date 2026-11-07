//go:build darwin

package fakecli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// warmLimit is how long macOS may take to let a new fake run at all. A test
// of the limit itself shortens it.
var warmLimit = 30 * time.Second

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
func warm(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), warmLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, warmArg)
	// Built with -race, a program sleeps a second on its way out unless told
	// not to, and this one has nothing to wait for. The options already set
	// are kept: the last of them wins.
	cmd.Env = append(os.Environ(), "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("fake cli %s: a first run that does nothing had not ended after %v", path, warmLimit)
		}
		return fmt.Errorf("fake cli %s: a first run that does nothing failed: %w", path, err)
	}
	return nil
}
