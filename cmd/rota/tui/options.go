package tui

import (
	"context"
	"io"
	"os/exec"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

// Options is what the TUI is run with. Locally rota fills it from this
// process; over SSH from the session, with Remote set.
type Options struct {
	// Backend is how the TUI reaches accounts, logins and runs. It is
	// required.
	Backend Backend

	// In, Out and Env are the terminal and its environment. Nil means
	// os.Stdin, os.Stdout and os.Environ().
	In  io.Reader
	Out io.Writer
	Env []string

	// Cols and Rows are the terminal's size, and Resize its later sizes, as
	// {cols, rows}; an SSH session sets them, where no terminal can be
	// asked. Run is the one reader of Resize: it hands every size to the
	// model, and to an attach that holds the terminal meanwhile (Attach).
	// Once Run has returned nobody reads it, so its sender must not block
	// on it for ever.
	Cols, Rows int
	Resize     <-chan [2]int

	// Remote says the TUI runs over SSH: on Host, for someone whose home on
	// it is Cwd. The colour depth then comes from TERM, and no signal of
	// this process is the TUI's.
	Remote    bool
	Host, Cwd string

	// Daemon says whether a daemon runs and which sessions it holds. The
	// header asks every 5 s. Nil: nobody asks, and none runs. A call must
	// end once its context does: one that does not is taken for a daemon
	// that does not run, and is not asked again until it has returned.
	Daemon func(ctx context.Context) Daemon

	// Exec is this rota with args, run on the terminal the TUI hands over.
	// It is nil over SSH, where this process's terminal is not the person's.
	Exec func(args ...string) *exec.Cmd

	// OpenURL opens a page in a browser on this machine. It is nil where
	// none is, over SSH among them (R19).
	OpenURL func(url string) error

	// Attach opens a held session on account and attaches it to the
	// terminal: the command to hand the terminal to. Nil: an account's CLI
	// runs directly through Exec.
	//
	// size and resized come from the shell (env.window) and follow
	// Options.Resize: over SSH, size answers the latest size and resized has
	// a value each time it changes, even while the attach holds the
	// terminal. Locally there is no Resize, and Bubble Tea delivers nothing
	// while it has handed the terminal over, so they hold the size at the
	// hand-off and never signal: a local attach watches its own terminal
	// (SIGWINCH) instead.
	Attach func(ctx context.Context, account int, size func() (cols, rows int), resized <-chan struct{}) (Attached, error)

	// Now and Loc are the clock and the zone times are shown in. Nil means
	// time.Now and time.Local.
	Now func() time.Time
	Loc *time.Location

	// Tick is tea.Tick, which tests replace with a clock of their own.
	Tick func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd

	// Profile is the colour depth to draw at. Nil: Profile(Out, Env,
	// Remote).
	Profile *colorprofile.Profile

	// Ended, when set, is called once the program has ended and the
	// terminal is given back, before Run leaves the tabs and overlays,
	// which may take up to ten seconds: the caller can then count, and
	// say, what quitting waits for from there.
	Ended func()
}

// Daemon is what Options.Daemon says: whether the daemon runs, its pid, the
// sessions it holds, and why it could not be asked, if it could not.
type Daemon struct {
	Running bool
	PID     int
	Live    []Live
	Err     string
}

// Live is one session the daemon holds: its id, its name and the account it
// runs on.
type Live struct {
	ID, Name string
	Account  int
}

// Attached is a held session's attach, as Options.Attach makes it: a
// command Bubble Tea hands the terminal to, which says how it ended once it
// has.
//
// Run must leave no read of the reader SetStdin gave it pending when it
// returns: the TUI reads that input again at once, and a read left behind
// would take the next key typed to the TUI into a session it has left. An
// attach hands its input through a channel it stops reading, or cancels its
// reader, before Run returns.
//
// SetStderr is always given os.Stderr, which over SSH is the daemon's
// stderr, not the person's channel: an attach over SSH writes what the
// person should read to the SetStdout writer.
type Attached interface {
	tea.ExecCommand
	// Ended is how the attach ended. It is read after Run returns.
	Ended() AttachEnd
}

// AttachEnd is how an attach ended: the person left the session, which
// keeps running (Detached), or the session ended with Code. Session is the
// session last shown, by name, and Err why the attach failed, if it did.
type AttachEnd struct {
	Detached bool
	Session  string
	Code     int
	Err      error
}

// Result is what is left when the TUI ends: a login put aside and never
// finished, which stays parked on the server until it runs out, so the
// person can still finish it with `rota login <id> <code>`.
type Result struct {
	Pending *backendLogin
}
