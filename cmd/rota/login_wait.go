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

	"github.com/professor93/rota/internal/logins"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// At a terminal, a login is one command: rota prints what it always prints
// and then waits, in the same process, for the code from the page. A
// provider whose login is a pasted key, or a device approval, is waited for
// the same way: the key is read unseen, and a device login is asked about
// until it is approved. Anywhere else, and always with --json, a login is the
// two steps it has always been, so a script or a pipe sees exactly what it
// saw before.
//
// rota still opens no browser. It waits for what a person brings back from
// one.

// loginTerminal reports whether one end of a login, its input, the output its
// URL is printed to or the one its prompt is, is a terminal. A variable so a
// test can say yes for a pipe.
var loginTerminal = isLoginTerminal

func isLoginTerminal(end any) bool {
	f, ok := end.(*os.File)
	return ok && isTerminal(f)
}

// loginTTL is how long a parked login can be finished, counted from its
// CreatedAt: the store's own bound, a variable so a test need not wait it out.
var loginTTL = logins.TTL

// devicePoll is how long a device login waits between two questions to the
// provider: the interval RFC 8628 gives a client that is told none.
var devicePoll = 5 * time.Second

// pendingCheck is how often a wait looks whether its login is still parked,
// so that one finished by another rota meanwhile is said to be that, rather
// than waited on until it seems to have run out.
var pendingCheck = 10 * time.Second

// awaitBegun, when set, is called once the wait has its signals and before
// anything is read: how a test does something at the moment a person would.
var awaitBegun func(context.Context)

// waits reports whether this login is waited for here rather than finished by
// a second command: only with a person at a terminal on every end, the one
// typing, the one the URL is printed to and the one the prompt is. A delegated
// login is the vendor CLI's to finish, and so is one that can also be
// delegated, grok's: its keyless route is a second command, which a wait for
// the key would take away.
func (c *cli) waits(l *rota.Login) bool {
	return !c.json && l.Kind != "delegated" && !l.Delegated &&
		loginTerminal(c.in) && loginTerminal(c.out) && loginTerminal(c.err)
}

// afterBegin waits for the login just printed, when it is waited for, begun
// for account (zero for none). What the accounts it can sign in look like now
// is noted first, while the store is still held, so that a login that vanishes
// later can be told finished from merely gone. The lock is not rota's to hold
// while a person reads a browser page: every other rota command needs it
// meanwhile, so each attempt takes it again.
func (c *cli) afterBegin(s *store.Store, l *rota.Login, account int) error {
	if !c.waits(l) {
		return nil
	}
	before := grantsOf(s, l.Provider, account)
	dir := "" // where the store is, from the store itself; none: nothing can be told
	if fb, ok := s.Backend().(*store.FileBackend); ok {
		dir = fb.Dir
	}
	_ = s.Release()
	return c.await(l, account, before, dir)
}

// waitFor is one login being waited for, and the clocks that end the wait.
type waitFor struct {
	l       *rota.Login
	account int               // the one it was begun for, zero for none
	before  map[int]grant     // grantsOf as the wait began
	dir     string            // the store's directory, never made by a look
	ends    time.Time         // when the login runs out
	runOut  *time.Timer       // fires then
	check   *time.Ticker      // when to look whether it is still parked
	looked  chan lookedResult // what a look found, one look at a time
	looking bool              // a look is under way
	ctx     context.Context
}

// await waits until the login is finished, runs out, or cannot be finished.
//
// A rejected code, and an approval by the wrong account, are mistakes
// approving again puts right, so the wait says what went wrong and asks
// again, on the same parked login. Where approving again cannot help
// (logins.Unfinishable), it stops with that error; so does a device login the
// provider refuses, whose code is spent. A login another rota finished
// meanwhile is said to be finished there; one that is gone with nothing
// signed in by it is said to be that.
//
// The end of the input, or an interrupt, ends the wait and not the login: it
// stays parked, and the command that finishes it later is said. A login that
// has run out is not offered to be finished later, since it cannot be.
func (c *cli) await(l *rota.Login, account int, before map[int]grant, dir string) error {
	ctx, stop := interrupted()
	defer stop()
	if awaitBegun != nil {
		awaitBegun(ctx)
	}
	w := &waitFor{l: l, account: account, before: before, dir: dir, ctx: ctx,
		ends: time.UnixMilli(l.CreatedAt).Add(loginTTL), looked: make(chan lookedResult, 1)}
	w.runOut = time.NewTimer(time.Until(w.ends))
	defer w.runOut.Stop()
	w.check = time.NewTicker(pendingCheck)
	defer w.check.Stop()
	done := make(chan struct{})
	defer close(done)

	var lines <-chan typedLine
	if l.Kind == "device" {
		fmt.Fprintf(c.err, "Waiting for the approval (%s left; ^C stops waiting and leaves it open)...\n", timeLeft(w.ends))
	} else {
		lines = typedLines(c.in, done)
	}
	said := "" // the last failure said, so a device wait says each once
	for {
		code := ""
		if l.Kind != "device" {
			r, why := c.ask(w, lines)
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
		var refused *rota.OAuthError
		switch {
		case err == nil && !waiting:
			return nil
		case errors.Is(err, rota.ErrNoLogin):
			return c.gone(w, w.look())
		case logins.Unfinishable(err):
			return err
		case l.Kind == "device" && errors.As(err, &refused):
			return err
		case err != nil && l.Kind != "device":
			c.sayFailure(w, err)
			continue
		case err != nil:
			if msg := err.Error(); msg != said {
				c.sayFailure(w, err)
				said = msg
			}
		case l.Kind != "device":
			fmt.Fprintln(c.err, "Not approved yet; approve in the browser, then give it again.")
			continue
		}
		if why := c.pause(w); why != nil {
			return why
		}
	}
}

// pause is a device wait's time between two questions, ended early by
// whatever ends the wait.
func (c *cli) pause(w *waitFor) error {
	next := time.After(devicePoll)
	for {
		select {
		case <-w.ctx.Done():
			return c.leftOpen(w, errInterrupted)
		case <-w.runOut.C:
			return expired(w.l, w.account)
		case <-w.check.C:
			w.lookSoon()
		case r := <-w.looked:
			w.looking = false
			if !r.parked {
				return c.gone(w, r)
			}
		case <-next:
			return nil
		}
	}
}

// ask prompts for what finishes the login and waits for one line of it. A key
// is read with the terminal's echo off, the way every pasted key is (noEcho),
// and its prompt is printed only once the echo is off, so nothing typed ahead
// of it is shown; a code is read as typed, since it is good once and for
// nothing without the verifier that stays on this machine. why is what ends
// the wait instead.
func (c *cli) ask(w *waitFor, lines <-chan typedLine) (line string, why error) {
	read := func() (string, error) {
		for {
			select {
			case <-w.ctx.Done():
				fmt.Fprintln(c.err)
				return "", c.leftOpen(w, errInterrupted)
			case <-w.runOut.C:
				fmt.Fprintln(c.err)
				return "", expired(w.l, w.account)
			case <-w.check.C:
				w.lookSoon()
			case r := <-w.looked:
				w.looking = false
				if !r.parked {
					fmt.Fprintln(c.err)
					return "", c.gone(w, r)
				}
			case r := <-lines:
				if r.err != nil {
					fmt.Fprintln(c.err)
					return "", c.leftOpen(w, exitCode(1))
				}
				return r.line, nil
			}
		}
	}
	if w.l.Kind != "apikey" {
		fmt.Fprintf(c.err, "Code from the page (%s left): ", timeLeft(w.ends))
		return read()
	}
	prompt := func() { fmt.Fprintf(c.err, "Paste the key (it is not shown; %s left): ", timeLeft(w.ends)) }
	tty, _ := c.in.(*os.File)
	if tty == nil {
		prompt()
		return read()
	}
	line, why = noEcho(tty, func() (string, error) {
		prompt()
		return read()
	})
	if why == nil {
		fmt.Fprintln(c.err) // the Enter that ended it was not shown either
	}
	return line, why
}

// sayFailure says why an attempt failed, before the wait goes on. A rejected
// code is said in the wait's own words: lib tells whoever has no wait to go
// back to to start a new login, and this one asks again on the same login.
func (c *cli) sayFailure(w *waitFor, err error) {
	var refused *rota.OAuthError
	if errors.As(err, &refused) && refused.Code == "invalid_grant" {
		fmt.Fprintf(c.err, "error: that code was not accepted (mistyped, already used, or expired); give it again, "+
			"or approve again at the same link for a new one:\n  %s\n", w.l.URL)
		return
	}
	say(c.err, 0, "error: "+unsecret(err.Error())+"\n")
}

// lookedResult is what one look at the store found.
type lookedResult struct {
	parked   bool // the login is still waiting to be finished, or nothing could be told
	signedIn bool // an account it could sign in has a new login since the wait began
}

// lookSoon starts a look unless one is under way. It takes the store's lock,
// which another rota may hold for half a minute, so it runs beside the wait
// rather than in it: an interrupt, a typed line or the deadline is never held
// up behind it.
func (w *waitFor) lookSoon() {
	if w.looking {
		return
	}
	w.looking = true
	go func() { w.looked <- w.look() }()
}

// look reports whether the login is still parked, and if it is not, whether
// an account it could sign in got a new grant meanwhile. A store directory
// that is gone is not made again by looking; one that cannot be opened, or a
// store with no directory to look in, says nothing either way, and the wait
// goes on.
func (w *waitFor) look() lookedResult {
	if w.dir == "" {
		return lookedResult{parked: true}
	}
	if _, err := os.Stat(w.dir); os.IsNotExist(err) {
		return lookedResult{}
	} else if err != nil {
		return lookedResult{parked: true}
	}
	s, err := store.Open(w.dir)
	if err != nil {
		return lookedResult{parked: true}
	}
	defer s.Close()
	if l, err := s.PendingLogin(w.l.ID); err != nil || l != nil {
		return lookedResult{parked: true}
	}
	for id, now := range grantsOf(s, w.l.Provider, w.account) {
		before, ok := w.before[id]
		if !ok || now.newer(before) {
			return lookedResult{signedIn: true}
		}
	}
	return lookedResult{}
}

// grantEnd is where a claude account keeps its grant's own end, which lib
// writes at sign-in and no refresh ever changes (lib/claude.go,
// claudeRefreshUntil): a different one is a different grant.
const grantEnd = "refresh_token_expires_at"

// grant is what tells one sign-in of an account from another, as against a
// refresh of the same one: the grant's own end, the long-lived token's
// expiry, which only a long login sets, and whether the account is dead,
// which a refresh never undoes and a new grant always does.
type grant struct {
	until string
	long  int64
	dead  bool
}

// newer reports whether g is a sign-in that was not there when before was
// taken. A refresh changes none of the three.
func (g grant) newer(before grant) bool {
	return (g.until != "" && g.until != before.until) ||
		(g.long != 0 && g.long != before.long) ||
		(before.dead && !g.dead)
}

// grantsOf is, for each account a login can sign in, what its present grant
// is: the one account it was begun for, or every account of its provider. A
// login finished anywhere gives one of them a new grant, or adds one. Nothing
// secret is in it.
func grantsOf(s *store.Store, provider string, account int) map[int]grant {
	m := map[int]grant{}
	for _, a := range s.Accounts {
		if a.Provider != provider || (account != 0 && a.ID != account) {
			continue
		}
		g := grant{until: a.Extra[grantEnd], dead: a.Dead}
		if a.Long != nil {
			g.long = a.Long.ExpiresAt
		}
		m[a.ID] = g
	}
	return m
}

// gone ends a wait whose login is no longer parked. Before its time, with an
// account it could sign in signed in meanwhile, another rota finished it,
// which ends this command as a success too; with none, it is gone and nothing
// came of it. After its time it ran out. It is never nil: whatever it says
// ends the wait.
func (c *cli) gone(w *waitFor, r lookedResult) error {
	switch {
	case !time.Now().Before(w.ends):
		return expired(w.l, w.account)
	case r.signedIn:
		fmt.Fprintf(c.err, "The login %s is no longer open: another rota finished it meanwhile. `rota list` shows how the account stands.\n", w.l.ID)
		return finishedElsewhere
	}
	return fmt.Errorf("the login %s is no longer open, and no account was signed in by it; `%s` begins another",
		w.l.ID, beginAgain(w.l, w.account))
}

// finishedElsewhere ends a wait whose login another rota finished, with the
// status of a login finished here.
const finishedElsewhere = exitCode(0)

// leftOpen ends the wait with code and says how the login is finished later.
func (c *cli) leftOpen(w *waitFor, code exitCode) error {
	fmt.Fprintf(c.err, "The login stays open for %s: %s\n", timeLeft(w.ends), finishLater(w.l))
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
func expired(l *rota.Login, account int) error {
	return fmt.Errorf("the login %s has expired: a login can be finished for %d minutes after it begins; `%s` begins another",
		l.ID, int(logins.TTL/time.Minute), beginAgain(l, account))
}

// beginAgain is the command that begins this login again, begun for account
// (zero for none).
func beginAgain(l *rota.Login, account int) string {
	again := "rota login"
	switch {
	case account != 0:
		again += " " + strconv.Itoa(account)
	case l.Long && l.Provider != rota.DefaultProvider:
		again += " " + l.Provider + " --long"
	case l.Long:
		again += " --long"
	case l.Provider != rota.DefaultProvider:
		again += " " + l.Provider
	}
	return again
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
