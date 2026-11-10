package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/cmd/rota/loginpage"
	"github.com/professor93/rota/internal/logins"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// `rota login --web [addr]` takes what finishes a login from a one-time page
// instead of the terminal: the code, an API key or a setup token, pasted on
// another device or in a browser where a paste is easier than at a prompt.
// The login is begun and printed as always; the page is one more way to bring
// the code back, and it ends with the login it finishes (see loginpage).
//
// Nothing pasted on the page is printed here, and nothing about it reaches
// argv: the value travels only in the page's form body.

// webAsk is --web on the command line: Asked that it was given, and Addr the
// address after it, empty when none was.
type webAsk struct {
	Addr  string
	Asked bool
}

// webUsage is why --web is refused beside --json or a code.
const webUsage = "--web serves a page for a person to open, so it takes no --json and no code"

// pageChoices lists where the page may listen. A variable so a test can offer
// addresses this machine does not have.
var pageChoices = loginpage.Choices

// pagePort is the port an address with none is given. At loginpage's default,
// the address is handed on as it is, so a busy default port falls back to the
// kernel's; tests set it to 0, so that none of them takes the default port.
var pagePort = loginpage.DefaultPort

// finishTimeout bounds one value's check from the page: the store's lock and
// the provider's answer. The page waits for a check under way before it ends,
// so ^C is never held up longer than this.
const finishTimeout = 60 * time.Second

// takeWeb lifts --web and its address out of login's arguments. The word
// after --web is its address only when it reads as one: it holds a colon,
// parses as an IP address, or is localhost. A bare number is an account id,
// so `rota login --web 12` is account 12, and a port alone is written :8788.
// --web=ADDR always names one.
func takeWeb(args []string) (rest []string, w *webAsk, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := strings.Cut(a, "=")
		if name != "--web" && name != "-web" {
			rest = append(rest, a)
			continue
		}
		if w != nil {
			return nil, nil, usageErr("--web is given once")
		}
		w = &webAsk{Asked: true}
		switch {
		case hasValue && value == "":
			return nil, nil, usageErr("--web= needs an address after it, such as 127.0.0.1 or :8788")
		case hasValue:
			w.Addr = value
		case i+1 < len(args) && looksLikeAddr(args[i+1]):
			i++
			w.Addr = args[i]
		}
		if w.Addr != "" {
			if err := checkAddr(w.Addr); err != nil {
				return nil, nil, err
			}
		}
	}
	return rest, w, nil
}

// looksLikeAddr reports whether the word after --web is its address.
func looksLikeAddr(s string) bool {
	if strings.Contains(s, ":") || strings.EqualFold(s, "localhost") {
		return true
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}

// checkAddr refuses an address the page could not listen on before any login
// is begun: a host that is not an IP address or localhost, or a port that is
// not one. The page itself decides the rest.
func checkAddr(addr string) error {
	host, port := addr, ""
	if h, p, err := net.SplitHostPort(addr); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			return usageErr("--web %s: the port is not a port", oneLine(addr))
		}
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		return nil
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return usageErr("--web %s: give the page an IP address, not a name", oneLine(addr))
	}
	return nil
}

// pageAddr is addr with pagePort added when it names no port and pagePort is
// not the page's own default.
func pageAddr(addr string) string {
	if pagePort == loginpage.DefaultPort {
		return addr
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]"), strconv.Itoa(pagePort))
}

// pickAddr is where the page listens: the address given, or, with a person at
// the terminal, the one they choose from this machine's, loopback first and
// every interface last. Off a terminal, where nobody can be asked, it is
// loopback, and that is said.
func (c *cli) pickAddr(w *webAsk) (string, error) {
	const loopback = "127.0.0.1"
	if w != nil && w.Addr != "" {
		return w.Addr, nil
	}
	if !loginTerminal(c.in) || !loginTerminal(c.err) {
		fmt.Fprintln(c.err, "the one-time page listens on "+loopback+"; give --web an address for another")
		return loopback, nil
	}
	choices, err := pageChoices()
	if err != nil || len(choices) == 0 {
		warn(c.err, "this machine's addresses cannot be listed (%v), so the one-time page listens on %s", err, loopback)
		return loopback, nil
	}
	ctx, stop := interrupted()
	defer stop()
	done := make(chan struct{})
	defer close(done)
	lines := typedLines(c.in, done)
	fmt.Fprintln(c.err, "Where should the one-time page listen?")
	for i, ch := range choices {
		line := fmt.Sprintf("  %d) %s  %s", i+1, oneLine(ch.Addr), oneLine(ch.Who))
		if ch.Warn {
			line += " — warning"
		}
		fmt.Fprintln(c.err, line)
	}
	for {
		fmt.Fprint(c.err, "Choice [1]: ")
		select {
		case <-ctx.Done():
			fmt.Fprintln(c.err)
			return "", errInterrupted
		case r := <-lines:
			if r.err != nil {
				fmt.Fprintln(c.err)
				return "", errors.New("no address was chosen for the one-time page")
			}
			typed := strings.TrimSpace(r.line)
			if typed == "" {
				return choices[0].Addr, nil
			}
			if n, err := strconv.Atoi(typed); err == nil && n >= 1 && n <= len(choices) {
				return choices[n-1].Addr, nil
			}
		}
	}
}

// webWait serves the page for a login just begun, or parked and named by its
// id, begun for account (zero for none), in place of the terminal's wait. As
// there, what the accounts it can sign in look like is noted first, and the
// store's lock is let go while the page waits.
func (c *cli) webWait(s *store.Store, l *rota.Login, account int) error {
	if l.Kind == "device" || l.Kind == "delegated" {
		return fmt.Errorf("the login %s takes nothing pasted, so --web has no page to serve for it; `rota login %s` finishes it", l.ID, l.ID)
	}
	o := loginpage.Options{Want: loginpage.Code, Provider: l.Provider, LoginURL: l.URL}
	if l.Kind == "apikey" {
		o.Want, o.LoginURL = loginpage.Key, ""
	}
	if a := s.Find(account); a != nil {
		if o.Approve = oneLine(a.Email); o.Approve == "" {
			o.Approve = labelOf(a)
		}
	}
	w := &waitFor{l: l, account: account, before: signedIn(s, l.Provider, account),
		ends: time.UnixMilli(l.CreatedAt).Add(loginTTL), looked: make(chan lookedResult, 1)}
	w.dir, _ = store.DefaultDir()
	_ = s.Release()
	o.Ends = w.ends
	o.Submit = func(ctx context.Context, code string) (string, error) {
		s, err := c.openStoreCtx(ctx)
		if err != nil {
			return "", err
		}
		defer s.Close()
		waiting, err := c.finishOnce(ctx, s, l.ID, code)
		var refused *rota.OAuthError
		switch {
		case err == nil && waiting:
			return "", errors.New("not approved yet; approve at the sign-in page, then give it again")
		case err == nil:
			return "", nil
		case errors.Is(err, rota.ErrNoLogin), logins.Unfinishable(err):
			return "", loginpage.Final(err)
		case errors.As(err, &refused) && refused.Code == "invalid_grant":
			return "", errors.New("that code was not accepted (mistyped, already used, or expired); give it again, " +
				"or approve again at the sign-in page for a new one")
		}
		return "", err
	}
	return c.awaitWeb(w, o)
}

// awaitWeb serves the page, says where it is, and waits for it to end. w is
// the login it finishes, nil for a key or a token that makes or changes an
// account at once, where nothing is parked and nothing is left open.
//
// While it waits, it looks every so often whether the login is still parked,
// as the terminal's wait does, so that one another rota finished meanwhile
// ends the page in the same words. A value being checked on the page is let
// finish first: what it did is what is said.
func (c *cli) awaitWeb(w *waitFor, o loginpage.Options) error {
	addr, err := c.pickAddr(c.web)
	if err != nil {
		return c.stillOpen(w, err)
	}
	ctx, stop := interrupted()
	defer stop()

	// One value is checked at a time, and none once the page is being closed
	// for a login gone from the store.
	var (
		mu       sync.Mutex
		accepted bool
		closing  bool
		gone     bool // the store said the login was not there
	)
	submit := o.Submit
	o.Submit = func(ctx context.Context, v string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if closing {
			return "", loginpage.Final(errors.New("this login is no longer open here"))
		}
		ctx, cancel := context.WithTimeout(ctx, finishTimeout)
		defer cancel()
		done, err := submit(ctx, v)
		if err == nil {
			accepted = true
		} else if errors.Is(err, rota.ErrNoLogin) {
			gone = true
		}
		return done, err
	}
	o.Addr = pageAddr(addr)
	p, err := loginpage.Serve(ctx, o)
	if err != nil {
		return c.stillOpen(w, err)
	}
	defer p.Close()
	if w != nil {
		fmt.Fprintln(c.out) // after what the login's beginning printed
	}
	c.sayPage(p)
	if awaitBegun != nil {
		awaitBegun(ctx)
	}

	var looked <-chan lookedResult
	var check <-chan time.Time
	if w != nil {
		w.check = time.NewTicker(pendingCheck)
		defer w.check.Stop()
		check, looked = w.check.C, w.looked
	}
	for {
		select {
		case e := <-p.Done:
			mu.Lock()
			wasGone := gone
			mu.Unlock()
			if e.Why == loginpage.Refused && wasGone && w != nil {
				return c.gone(w, w.look())
			}
			return c.webEnded(w, e)
		case <-check:
			w.lookSoon()
		case r := <-looked:
			w.looking = false
			if r.parked {
				continue
			}
			mu.Lock()
			finished := accepted
			closing = !finished
			mu.Unlock()
			if finished {
				continue // its end is on p.Done
			}
			p.Close()
			return c.gone(w, r)
		}
	}
}

// webEnded is how a page's end ends the command. A login the page did not
// finish stays parked, and the line that finishes it later is said, unless
// the login itself ran out.
func (c *cli) webEnded(w *waitFor, e loginpage.End) error {
	switch e.Why {
	case loginpage.Accepted:
		return nil
	case loginpage.Stopped:
		if w == nil {
			return errInterrupted
		}
		return c.leftOpen(w, errInterrupted)
	case loginpage.Refused:
		return errors.New(e.Message)
	case loginpage.Tries:
		return errors.New("five wrong tries closed the page; " + laterOf(w))
	}
	if w != nil && time.Until(w.ends) < time.Second {
		return expired(w.l, w.account)
	}
	return errors.New("the page closed after 10 minutes; " + laterOf(w))
}

// laterOf is what is left once a page ends without a login: the line that
// finishes it later, or, with none parked, that nothing changed.
func laterOf(w *waitFor) string {
	if w == nil {
		return "nothing was stored"
	}
	return "the login stays open for " + timeLeft(w.ends) + ": " + finishLater(w.l)
}

// stillOpen is err, with the line that finishes the login later said first
// when one is parked.
func (c *cli) stillOpen(w *waitFor, err error) error {
	if w != nil {
		fmt.Fprintf(c.err, "The login stays open for %s: %s\n", timeLeft(w.ends), finishLater(w.l))
	}
	return err
}

// sayPage prints where the page is, how long it lasts, and how to reach or
// trust it from another machine.
func (c *cli) sayPage(p *loginpage.Page) {
	fmt.Fprintf(c.out, "Open the one-time page: %s\n", p.URL)
	fmt.Fprintln(c.out, "It ends after one login, five wrong tries, 10 minutes or ^C.")
	if p.Fingerprint != "" {
		fmt.Fprintf(c.out, "It is HTTPS with a certificate made for this run; its SHA-256 fingerprint is %s — check that the browser shows the same.\n",
			p.Fingerprint)
		return
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	fmt.Fprintf(c.out, "From another machine: ssh -L %s:%s:%s %s@%s, then open the link there.\n",
		u.Port(), host, u.Port(), whoHere(), hostHere())
}

// whoHere and hostHere are this account's name and this machine's, for the
// ssh line; a placeholder when either cannot be told.
func whoHere() string {
	if u, err := user.Current(); err == nil && u.Username != "" && !strings.ContainsAny(u.Username, " \\") {
		return oneLine(u.Username)
	}
	return "<user>"
}

func hostHere() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return oneLine(h)
	}
	return "<host>"
}

// openStoreCtx is openStore given up on when ctx ends first: another rota may
// hold the store's lock for half a minute, and ^C must not wait for it. A
// store that opens after all is closed again.
func (c *cli) openStoreCtx(ctx context.Context) (*store.Store, error) {
	type opened struct {
		s   *store.Store
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		s, err := c.openStore()
		ch <- opened{s, err}
	}()
	select {
	case o := <-ch:
		return o.s, o.err
	case <-ctx.Done():
		go func() {
			if o := <-ch; o.s != nil {
				o.s.Close()
			}
		}()
		return nil, ctx.Err()
	}
}
