package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// On a terminal, a login is one command: rota prints what it always prints
// and then waits, in the same process, for whatever finishes the login — the
// code from the page, a key, or nothing at all while a device login is
// approved. Anywhere else, and always with --json, a login is the two steps
// it has always been, so a script or a pipe sees exactly what it saw before.
//
// rota still opens no browser. It waits for what a person brings back from
// one.

// loginTerminal reports whether a login's input is a person at a terminal,
// who can be asked for the code. A variable so a test can say yes for a pipe.
var loginTerminal = isLoginTerminal

func isLoginTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && isTerminal(f)
}

// loginTTL is how long a parked login can be finished, counted from its
// CreatedAt: the store's own bound, a variable so a test need not wait it out.
var loginTTL = store.PendingTTL

// devicePoll is how long a device login waits between two questions to the
// provider when the login does not say (its state's "interval", in seconds).
// Five seconds is the interval RFC 8628 gives a client that is told none.
var devicePoll = 5 * time.Second

// awaitBegun, when set, is called once the wait has its signals and before
// anything is read: how a test does something at the moment a person would.
var awaitBegun func(context.Context)

// waits reports whether this login is waited for here rather than finished by
// a second command. A delegated login is the vendor CLI's to finish.
func (c *cli) waits(l *rota.Login) bool {
	return !c.json && l.Kind != "delegated" && loginTerminal(c.in)
}

// afterBegin waits for the login just printed, when it is waited for. The
// lock is not rota's to hold while a person reads a browser page: every other
// rota command needs it meanwhile, so each attempt takes it again.
func (c *cli) afterBegin(s *store.Store, l *rota.Login) error {
	if !c.waits(l) {
		return nil
	}
	_ = s.Release()
	return c.await(l)
}

// await waits until the login is finished, runs out, or cannot be finished.
//
// A rejected code, and an approval by the wrong account, are mistakes
// approving again puts right, so the wait says what went wrong and asks
// again, on the same parked login. Where approving again cannot help
// (store.Unfinishable), or the login is gone, it stops with that error.
//
// The end of the input, or an interrupt, ends the wait and not the login: it
// stays parked, and the command that finishes it later is said. A login that
// has run out is not offered to be finished later, since it cannot be.
func (c *cli) await(l *rota.Login) error {
	ctx, stop := interrupted()
	defer stop()
	if awaitBegun != nil {
		awaitBegun(ctx)
	}
	ends := time.UnixMilli(l.CreatedAt).Add(loginTTL)
	runOut := time.NewTimer(time.Until(ends))
	defer runOut.Stop()
	done := make(chan struct{})
	defer close(done)

	var lines <-chan typedLine
	if l.Kind == "device" {
		fmt.Fprintf(c.err, "Waiting for the approval (%s left; ^C stops waiting and leaves it open)...\n", timeLeft(ends))
	} else {
		lines = typedLines(c.in, done)
	}
	for {
		code := ""
		if l.Kind != "device" {
			r, why := c.ask(ctx, l, ends, lines, runOut.C)
			if why != nil {
				return why
			}
			if code = strings.TrimSpace(r); code == "" {
				continue
			}
		}
		s, err := c.openStore()
		if err != nil {
			return err
		}
		waiting, err := c.finishOnce(s, l.ID, code)
		s.Close()
		switch {
		case err == nil && !waiting:
			return nil
		case errors.Is(err, rota.ErrNoLogin), store.Unfinishable(err):
			return err
		case err != nil:
			say(c.err, 0, "error: "+unsecret(err.Error())+"\n")
			continue
		case l.Kind != "device":
			fmt.Fprintln(c.err, "Not approved yet; approve in the browser, then give it again.")
			continue
		}
		select {
		case <-ctx.Done():
			return c.leftOpen(l, ends, errInterrupted)
		case <-runOut.C:
			return expired(l)
		case <-time.After(pollEvery(l)):
		}
	}
}

// ask prompts for what finishes the login and waits for one line of it. A key
// is read with the terminal's echo off, the way every pasted key is (noEcho);
// a code is read as typed, since it is good once and for nothing without the
// verifier that stays on this machine. why is what ends the wait instead.
func (c *cli) ask(ctx context.Context, l *rota.Login, ends time.Time, lines <-chan typedLine, runOut <-chan time.Time) (line string, why error) {
	read := func() (string, error) {
		select {
		case <-ctx.Done():
			fmt.Fprintln(c.err)
			return "", c.leftOpen(l, ends, errInterrupted)
		case <-runOut:
			fmt.Fprintln(c.err)
			return "", expired(l)
		case r := <-lines:
			if r.err != nil {
				fmt.Fprintln(c.err)
				return "", c.leftOpen(l, ends, exitCode(1))
			}
			return r.line, nil
		}
	}
	if l.Kind != "apikey" {
		fmt.Fprintf(c.err, "Code from the page (%s left): ", timeLeft(ends))
		return read()
	}
	fmt.Fprintf(c.err, "Paste the key (it is not shown; %s left): ", timeLeft(ends))
	tty, _ := c.in.(*os.File)
	if tty == nil {
		return read()
	}
	line, why = noEcho(tty, read)
	if why == nil {
		fmt.Fprintln(c.err) // the Enter that ended it was not shown either
	}
	return line, why
}

// leftOpen ends the wait with code and says how the login is finished later.
func (c *cli) leftOpen(l *rota.Login, ends time.Time, code exitCode) error {
	fmt.Fprintf(c.err, "The login stays open for %s: %s\n", timeLeft(ends), finishLater(l))
	return code
}

// finishLater is the command that finishes this login from another process.
func finishLater(l *rota.Login) string {
	switch l.Kind {
	case "device":
		return "rota login " + l.ID
	case "apikey":
		return "rota login " + l.ID + " <key>"
	}
	return "rota login " + l.ID + " <code>"
}

// expired is the end of a login nothing finished in time, with the command
// that begins it again.
func expired(l *rota.Login) error {
	again := "rota login"
	switch {
	case l.Account != 0:
		again += " " + strconv.Itoa(l.Account)
	case l.Long && l.Provider != rota.DefaultProvider:
		again += " " + l.Provider + " --long"
	case l.Long:
		again += " --long"
	case l.Provider != rota.DefaultProvider:
		again += " " + l.Provider
	}
	return fmt.Errorf("the login %s has expired: a login can be finished for %d minutes after it begins; `%s` begins another",
		l.ID, int(store.PendingTTL/time.Minute), again)
}

// pollEvery is how long a device login waits before asking again.
func pollEvery(l *rota.Login) time.Duration {
	if n, err := strconv.Atoi(l.State["interval"]); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return devicePoll
}

// timeLeft is how long until the login runs out, in the minutes a person
// reads, and in seconds for the last of them.
func timeLeft(ends time.Time) string {
	d := time.Until(ends)
	switch {
	case d <= 0:
		return "no time"
	case d < time.Minute:
		return strconv.Itoa(int((d+time.Second-1)/time.Second)) + "s"
	}
	return strconv.Itoa(int((d+time.Minute-1)/time.Minute)) + "m"
}

// typedLine is one line a person typed, or the end of what they can type.
type typedLine struct {
	line string
	err  error
}

// typedLines reads in a line at a time, for as long as the wait wants them.
// A last line without its newline still counts. The goroutine stops once done
// is closed, or is left waiting on a read the process ends under.
func typedLines(in io.Reader, done <-chan struct{}) <-chan typedLine {
	ch := make(chan typedLine)
	go func() {
		r := bufio.NewReader(in)
		for {
			line, err := r.ReadString('\n')
			if err != nil && line != "" {
				err = nil
			}
			select {
			case ch <- typedLine{line, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}
