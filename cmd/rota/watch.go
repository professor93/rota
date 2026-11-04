package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// An interactive run hands the terminal to the CLI by replacing rota with it,
// so no rota is left to notice that Claude Code has stranded or cleared the
// login in the account's home. Before the handover, a run on a claude account
// whose login lives there starts one watcher, `rota serve watch <id>`, which
// makes the look `rota list` starts with every few minutes for as long as
// something runs in the home (store.Watch). The CLI's process, terminal,
// signals, environment and arguments are what they were: the watcher is not
// its child, nor its parent, and holds none of its descriptors.
//
// `rota run --share` keeps rota as the CLI's parent, and starts the same
// watcher process rather than a goroutine of its own: one way, and nothing
// more to write.

// startWatcher starts the watcher for an account and returns once it is on
// its own; a variable so a test can see it called instead of starting this
// test binary again.
var startWatcher = watchInBackground

// watchLogin starts the watcher for an account a handover is about to open, if
// it has anything to keep. It must run before the claim is kept across the
// exec, so the watcher cannot inherit it.
func (c *cli) watchLogin(s *store.Store, a *rota.Account) {
	if !s.Watchable(a) {
		return
	}
	if err := startWatcher(a.ID); err != nil {
		fmt.Fprintf(c.err, "warning: could not start the watcher that keeps %s's login alive while it runs: %v\n", a, err)
	}
}

// serveWatch is `rota serve watch <id>`: started by rota run, it keeps the
// account's login alive while its CLI runs. A watcher runs in a session of its
// own; started from anywhere else it starts itself again in one and returns,
// so that the process that started it — a rota about to become the CLI — has
// no child left behind.
func (c *cli) serveWatch(args []string) error {
	if len(args) != 1 {
		return usageErr("serve watch takes one account id")
	}
	id, err := parseID(args[0])
	if err != nil {
		return err
	}
	if !ownSession() {
		return detachWatcher(id)
	}
	dir, err := store.DefaultDir()
	if err != nil {
		return err
	}
	log := &watchLog{path: filepath.Join(dir, "watch.log")}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	store.Watch(ctx, c.openStore, id, log.say)
	return nil
}

// watchLogCap is how large the watcher's log grows before it is moved aside to
// watch.log.1, replacing the one there.
var watchLogCap int64 = 1 << 20

// watchLog is where a watcher's remarks go: it has no terminal to say them on.
type watchLog struct {
	mu   sync.Mutex
	path string
}

func (l *watchLog) say(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if fi, err := os.Stat(l.path); err == nil && fi.Size() >= watchLogCap {
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s [%d] %s\n", time.Now().Format(time.RFC3339), os.Getpid(), msg)
}
