//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/professor93/rota/internal/pty"
	"github.com/professor93/rota/internal/share"
)

// The parts of sharing that are a Unix terminal: the window rota is sitting
// at, the CLI on a pseudo-terminal of its own, and the loop between them.
//
// Both are behind interfaces, and not for the sake of it: the copy loop is
// the one piece here whose bugs cost somebody their terminal, so it is
// written against something a test can be both ends of. A test drives a
// local terminal that is two pipes and a recorded raw mode, and a child that
// dies when it is told to, and asks the questions that matter — was the
// terminal put back, did the screen get everything, did a link that never
// reads slow any of it down.

/* ------------------------------------------------- the window you are at --- */

// shareLocal is the terminal rota was started at.
type shareLocal interface {
	// Read is what is being typed and Write is the screen.
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	// size is how big the window is now, asked of the device.
	size() (cols, rows uint16, err error)
	// raw puts it into the mode a full-screen program needs and hands back
	// the one way out, which the caller must take on every path.
	raw() (restore func(), err error)
	// resized fires when the window was dragged.
	resized() <-chan struct{}
}

// osLocal is that terminal for real: this process's own standard input and
// output, and the signal the kernel sends when the window changes.
type osLocal struct {
	in   *os.File
	out  *os.File
	win  chan os.Signal
	seen chan struct{}
}

func openLocal() *osLocal {
	l := &osLocal{in: os.Stdin, out: os.Stdout, win: make(chan os.Signal, 1), seen: make(chan struct{}, 1)}
	signal.Notify(l.win, syscall.SIGWINCH)
	go func() {
		for range l.win {
			select {
			case l.seen <- struct{}{}:
			default:
			}
		}
	}()
	return l
}

func (l *osLocal) Read(p []byte) (int, error)  { return l.in.Read(p) }
func (l *osLocal) Write(p []byte) (int, error) { return l.out.Write(p) }

func (l *osLocal) size() (uint16, uint16, error) { return pty.Size(l.in) }

func (l *osLocal) raw() (func(), error) { return rawMode(l.in.Fd()) }

func (l *osLocal) resized() <-chan struct{} { return l.seen }

func (l *osLocal) stop() { signal.Stop(l.win); close(l.win) }

/* --------------------------------------------------------- the CLI on a pty --- */

// shareChild is the CLI, on a pseudo-terminal of its own.
type shareChild interface {
	// Read is what it prints and Write is what it is typed.
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	resize(cols, rows uint16) error
	// hangUp is what closing a terminal window sends, and end the signal
	// nothing survives.
	hangUp()
	end()
	// wait is the exit code, once it has one.
	wait() int
	close()
}

// ptyChild is that CLI for real.
type ptyChild struct {
	master *os.File
	cmd    *exec.Cmd
}

func (p *ptyChild) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *ptyChild) Write(b []byte) (int, error) { return p.master.Write(b) }

func (p *ptyChild) resize(cols, rows uint16) error { return pty.Resize(p.master, cols, rows) }

func (p *ptyChild) hangUp() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGHUP)
	}
}

func (p *ptyChild) end() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
	}
}

func (p *ptyChild) wait() int {
	_ = p.cmd.Wait()
	if p.cmd.ProcessState != nil {
		return p.cmd.ProcessState.ExitCode()
	}
	return -1
}

func (p *ptyChild) close() { p.master.Close() }

// startChild puts the account's CLI on a pseudo-terminal of the size of the
// window rota is at. argv[0] is the CLI's own name rather than the path it
// was found at, which is what the handover does and what a CLI prints in its
// own help.
func startChild(path string, args, env []string, cwd string, cols, rows uint16) (*ptyChild, error) {
	cmd := exec.Command(path, args...)
	cmd.Args = append([]string{filepath.Base(path)}, args...)
	cmd.Env = env
	cmd.Dir = cwd
	master, err := pty.Start(cmd, cols, rows)
	if err != nil {
		return nil, err
	}
	return &ptyChild{master: master, cmd: cmd}, nil
}

/* ----------------------------------------------------------- the loop --- */

// shareKillAfter is how long the CLI has to end on a hangup before rota
// insists. It matches the server's own wait for the same thing, and it is a
// variable so a test need not take that long.
var shareKillAfter = 3 * time.Second

// shareSession is one shared terminal on this machine: the window, the CLI,
// and the link that may or may not be there.
type shareSession struct {
	lo    shareLocal
	child shareChild
	link  *shareLink
	// hangs is a signal to rota itself — a hangup or a termination. It is
	// not a keystroke: ^C and ^Z are bytes on this terminal, because it is
	// raw, and they become signals on the CLI's terminal where they belong.
	hangs <-chan os.Signal
}

// run is the whole life of a shared terminal, and the one place the local
// terminal's modes are put back.
//
// They are put back by a deferred call, which is the only arrangement that
// survives all three ways out of here: the CLI exiting, a signal to rota,
// and a panic anywhere below. Nothing in this function calls os.Exit, for
// exactly that reason — a terminal left raw is one somebody has to blindly
// type `stty sane` into, and it is the kind of damage a program does once
// and is never forgiven for.
func (sh *shareSession) run() int {
	restore := func() {}
	if back, err := sh.lo.raw(); err == nil {
		restore = back
	}
	defer restore()
	done := make(chan struct{})
	defer close(done)

	// What is typed here goes into the CLI, and nowhere else, by the
	// shortest path there is.
	go shareTyped(sh.lo, sh.child)
	go sh.follow(done)
	go sh.listen(done)

	// What the CLI prints goes to the screen and then, at no cost to it, to
	// the queue the link drains.
	sharePrinted(sh.child, sh.lo, sh.link.q)
	code := sh.child.wait()
	// The terminal is the person's again here rather than on the way out.
	// What follows gives the exit frame a couple of seconds to reach the
	// server, and nobody should be sitting in front of a raw terminal while
	// it does. Calling it twice is safe, which is what the deferred one
	// above is for — it is the path where none of this was reached.
	restore()
	sh.link.finished(code)
	sh.child.close()
	return code
}

// follow keeps the CLI's terminal the size of the window rota is at, and
// tells the server so.
func (sh *shareSession) follow(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-sh.lo.resized():
			cols, rows, err := sh.lo.size()
			if err != nil {
				continue
			}
			_ = sh.child.resize(cols, rows)
			sh.link.resized(cols, rows)
		}
	}
}

// listen is a signal to rota itself. A hangup or a termination is passed on
// to the CLI, which is what closing a terminal window means, and insisted on
// three seconds later — so that this process always reaches the deferred
// restore above rather than sitting behind a CLI that ignored it.
func (sh *shareSession) listen(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case _, ok := <-sh.hangs:
			if !ok {
				return
			}
			sh.endChild()
		}
	}
}

// endChild is the hangup and then the signal nothing survives, which is also
// what a kill frame from the server means here.
func (sh *shareSession) endChild() {
	sh.child.hangUp()
	time.AfterFunc(shareKillAfter, sh.child.end)
}

/* ------------------------------------------------- putting it together --- */

// startShare is --share itself: everything between an account that is ready
// and this process exiting with the CLI's own code.
//
// The order matters and it is this. The window is measured and the terminal
// is offered to the server first, because the one line rota prints has to be
// true and has to be printed before the CLI takes the screen. Then the modes
// change, then the CLI starts. Nothing about the link can fail in a way that
// stops any of that: a server that is not there is a line saying so.
func (c *cli) startShare(p sharePlan) (int, error) {
	lo := openLocal()
	defer lo.stop()
	cols, rows, err := lo.size()
	if err != nil {
		return 0, err
	}

	q := newShareQueue(shareQueueMax)
	link := newShareLink(p.sock, q, c.err, func() share.HelloMsg {
		// Asked again on every attempt: a link that comes back after the
		// window was dragged must not tell the server the old size.
		cols, rows := cols, rows
		if c, r, err := lo.size(); err == nil {
			cols, rows = c, r
		}
		return share.HelloMsg{
			Version: share.Version,
			Account: p.account, AccountLabel: p.label, Provider: p.provider,
			Label: p.ask.label, Cwd: p.cwd, Cols: cols, Rows: rows,
			PID: os.Getpid(), Mode: p.ask.mode,
		}
	})
	id, why := link.start()
	if why != "" {
		// A server that will not have this terminal says so in a sentence,
		// and that sentence is printed instead of the opening line: the two
		// are about the same thing and it is the more useful of them.
		fmt.Fprintf(c.err, "rota: %s\n", why)
		fmt.Fprintf(c.err, "rota: %s via %s — running here, unshared\n", p.who, p.bin)
	} else {
		fmt.Fprint(c.err, shareOpening(p.who, p.bin, id, p.sock, p.ask.mode))
	}

	child, err := startChild(p.path, p.args, p.env, p.cwd, cols, rows)
	if err != nil {
		link.close()
		return 0, err
	}
	// What the server may do to this terminal, and the second place watch
	// mode is enforced: a terminal offered to be watched has no way in from
	// the link at all, whatever the server sends down it.
	if p.ask.mode != share.ModeWatch {
		link.into = func(b []byte) { _, _ = child.Write(b) }
	}
	hangs := make(chan os.Signal, 1)
	signal.Notify(hangs, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(hangs)
	sh := &shareSession{lo: lo, child: child, link: link, hangs: hangs}
	link.kill = sh.endChild
	return sh.run(), nil
}
