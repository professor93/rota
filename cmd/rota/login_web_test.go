package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/cmd/rota/loginpage"
	rota "github.com/professor93/rota/lib"
)

// No test binds the page's default port: many test binaries share this
// machine, and one that took 8788 would move another's page elsewhere. An
// address with no port gets the kernel's.
func init() { pagePort = 0 }

// webRun is a `rota login --web` running in this process, with what it
// prints readable while it runs.
type webRun struct {
	out, err lockedBuffer
	done     chan webEnd
}

// webEnd is what a webRun printed, and its status.
type webEnd struct {
	out, err string
	code     int
}

func webCall(t *testing.T, args ...string) *webRun {
	t.Helper()
	r := &webRun{done: make(chan webEnd, 1)}
	go func() {
		code := run(args, &r.out, &r.err)
		r.done <- webEnd{r.out.String(), r.err.String(), code}
	}()
	return r
}

var pageLine = regexp.MustCompile(`Open the one-time page: (\S+)`)

// url is the page's link, once it is printed.
func (r *webRun) url(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m := pageLine.FindStringSubmatch(r.out.String()); m != nil {
			return m[1]
		}
		select {
		case got := <-r.done:
			r.done <- got
			t.Fatalf("it ended before it served a page: %d\n%s\n%s", got.code, got.out, got.err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("no page in:\n%s\n%s", r.out.String(), r.err.String())
	return ""
}

func (r *webRun) wait(t *testing.T) webEnd {
	t.Helper()
	select {
	case got := <-r.done:
		return got
	case <-time.After(30 * time.Second):
		t.Fatalf("the command did not return:\n%s\n%s", r.out.String(), r.err.String())
		return webEnd{}
	}
}

// browser is a client that keeps no connection between requests and goes
// through no proxy.
var browser = &http.Client{
	Timeout:   20 * time.Second,
	Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

// post submits one value on the page, as its form does.
func post(t *testing.T, u, value string) (int, string) {
	t.Helper()
	resp, err := browser.PostForm(u, url.Values{"value": {value}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func get(t *testing.T, u string) string {
	t.Helper()
	resp, err := browser.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// shortLogins makes a parked login run out in d.
func shortLogins(t *testing.T, d time.Duration) {
	t.Helper()
	was := loginTTL
	loginTTL = d
	t.Cleanup(func() { loginTTL = was })
}

func TestLoginWebFinishesFromThePage(t *testing.T) {
	home := seedTwoClaude(t)
	approvalsByCode(t, known)
	r := webCall(t, "login", "12", "--web", "127.0.0.1:0")
	u := r.url(t)
	if !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Fatalf("on loopback the page is plain HTTP there: %s", u)
	}
	if page := get(t, u); !strings.Contains(page, "a@b.c") {
		t.Fatalf("the page says who to approve as: %s", page)
	}
	code, body := post(t, u, "AS13")
	if code != http.StatusBadRequest || !strings.Contains(body, "which is account 13") {
		t.Fatalf("the wrong account's approval: %d %s", code, body)
	}
	code, body = post(t, u, "AS12")
	if code != http.StatusOK {
		t.Fatalf("the right one: %d %s", code, body)
	}
	got := r.wait(t)
	if got.code != 0 || !strings.Contains(got.out, "Refreshed claude account 12") {
		t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
	}
	port := u[len("http://127.0.0.1:"):strings.LastIndex(u, "/")]
	for _, want := range []string{
		"It ends after one login, five wrong tries, 10 minutes or ^C.",
		"From another machine: ssh -L " + port + ":127.0.0.1:" + port + " ",
		", then open the link there.",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("%q is not said: %s", want, got.out)
		}
	}
	if strings.Contains(got.out+got.err, "AS1") {
		t.Fatalf("what was submitted was printed:\n%s\n%s", got.out, got.err)
	}
	if p := parked(t, home); p != "" {
		t.Fatalf("finished, and still parked: %s", p)
	}
	if resp, err := browser.Get(u); err == nil {
		resp.Body.Close()
		t.Fatal("the page still answers after the login it finished")
	}
}

// A refusal no approval can put right ends the page with it.
func TestLoginWebStopsWhereApprovingAgainCannotHelp(t *testing.T) {
	home := seedTwoClaude(t)
	approvalsByCode(t, known)
	r := webCall(t, "login", "12", "--web", "127.0.0.1:0")
	u := r.url(t)
	code, body := post(t, u, "RECREATED")
	if code != http.StatusForbidden || !strings.Contains(body, "account id differs") {
		t.Fatalf("%d %s", code, body)
	}
	got := r.wait(t)
	if got.code != 1 || !strings.Contains(got.err, "account id differs") {
		t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
	}
	if !readAccounts(t, home).Accounts[0].Dead {
		t.Fatal("account 12 changed")
	}
}

// Five wrong codes close the page, and the login is still there to finish.
func TestLoginWebClosesAfterFiveWrongTries(t *testing.T) {
	home := seedTwoClaude(t)
	approvalsByCode(t, known)
	r := webCall(t, "login", "12", "--web", "127.0.0.1:0")
	u := r.url(t)
	for i := range loginpage.MaxTries {
		if code, body := post(t, u, fmt.Sprintf("WRONG-%d", i)); code != http.StatusBadRequest && code != http.StatusForbidden {
			t.Fatalf("try %d: %d %s", i, code, body)
		}
	}
	got := r.wait(t)
	id := loginIDFrom(t, got.out)
	if got.code != 1 || !strings.Contains(got.err, "five wrong tries closed the page; ") ||
		!strings.Contains(got.err, "rota login "+id+" <code>") || !strings.Contains(parked(t, home), id) {
		t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
	}
}

func TestLoginWebSaysWhenTheLoginRanOut(t *testing.T) {
	seedTwoClaude(t)
	shortLogins(t, 500*time.Millisecond)
	got := webCall(t, "login", "12", "--web", "127.0.0.1:0").wait(t)
	if got.code != 1 || !strings.Contains(got.err, "expired") || !strings.Contains(got.err, "`rota login 12`") {
		t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
	}
}

// How each end of a page ends the command, the page's own ten minutes among
// them, which no test waits out.
func TestWebEndedSaysHowThePageEnded(t *testing.T) {
	l := &rota.Login{ID: "abc123", Provider: "claude", Kind: "code"}
	w := &waitFor{l: l, ends: time.Now().Add(5 * time.Minute)}
	for _, c := range []struct {
		w    *waitFor
		end  loginpage.End
		want string
	}{
		{w, loginpage.End{Why: loginpage.TimedOut}, "the page closed after 10 minutes; the login stays open for 5m: rota login abc123 <code>"},
		{w, loginpage.End{Why: loginpage.Tries, Message: "nope"}, "five wrong tries closed the page; the login stays open for 5m: rota login abc123 <code>"},
		{w, loginpage.End{Why: loginpage.Refused, Message: "no longer here"}, "no longer here"},
		{nil, loginpage.End{Why: loginpage.TimedOut}, "the page closed after 10 minutes; nothing was stored"},
		{nil, loginpage.End{Why: loginpage.Tries}, "five wrong tries closed the page; nothing was stored"},
		{&waitFor{l: l, ends: time.Now().Add(-time.Second)}, loginpage.End{Why: loginpage.TimedOut}, "the login abc123 has expired"},
	} {
		c0 := &cli{out: io.Discard, err: io.Discard}
		err := c0.webEnded(c.w, c.end)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.end.Why, err, c.want)
		}
	}
}

func TestLoginWebAsksWhereOnATerminal(t *testing.T) {
	was := pageChoices
	pageChoices = func() ([]loginpage.Choice, error) {
		return []loginpage.Choice{
			{Addr: "127.0.0.1", Iface: "lo", Who: "only this machine"},
			{Addr: "192.168.1.9", Iface: "en0", Who: "machines on the local network of en0"},
			{Addr: "0.0.0.0", Who: "every interface", Warn: true},
		}, nil
	}
	t.Cleanup(func() { pageChoices = was })
	for _, c := range []struct {
		typed, want string
		asked       int
	}{
		{"2\n", "192.168.1.9", 1},
		{"\n", "127.0.0.1", 1},
		{"9\nfirst\n 3 \n", "0.0.0.0", 3},
	} {
		typing(t, c.typed)
		var errb bytes.Buffer
		cl := &cli{in: stdin, out: io.Discard, err: &errb}
		addr, err := cl.pickAddr(&webAsk{Asked: true})
		if err != nil || addr != c.want {
			t.Fatalf("%q: %q %v\n%s", c.typed, addr, err, errb.String())
		}
		menu := errb.String()
		want := "Where should the one-time page listen?\n" +
			"  1) 127.0.0.1  only this machine\n" +
			"  2) 192.168.1.9  machines on the local network of en0\n" +
			"  3) 0.0.0.0  every interface — warning\n" +
			"Choice [1]: "
		if !strings.HasPrefix(menu, want) || strings.Count(menu, "Choice [1]: ") != c.asked {
			t.Fatalf("%q: the menu:\n%s", c.typed, menu)
		}
	}
}

func TestLoginWebOffATerminalTakesLoopback(t *testing.T) {
	home := seedTwoClaude(t)
	approvalsByCode(t, known)
	r := webCall(t, "login", "12", "--web")
	u := r.url(t)
	if !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Fatalf("%s", u)
	}
	if code, body := post(t, u, "AS12"); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	got := r.wait(t)
	if got.code != 0 || !strings.Contains(got.err, "the one-time page listens on 127.0.0.1; give --web an address for another") {
		t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
	}
	if strings.Contains(got.err, "Where should") || parked(t, home) != "" {
		t.Fatalf("asked off a terminal, or not finished:\n%s\n%s", got.err, parked(t, home))
	}
}

func TestLoginWebRefusesJSONAndACode(t *testing.T) {
	home := seedTwoClaude(t)
	const why = "--web serves a page for a person to open, so it takes no --json and no code"
	for _, args := range [][]string{
		{"--json", "login", "--web", "127.0.0.1:0"},
		{"login", "12", "--web", "--json"},
		{"login", "abcdef", "SOMECODE", "--web"},
		{"login", "--web", "127.0.0.1:0", "abcdef", "SOMECODE"},
	} {
		out, errOut, code := call(t, args...)
		if code != 2 || !strings.Contains(out+errOut, why) {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	if p := parked(t, home); p != "" {
		t.Fatalf("a refused command began a login: %s", p)
	}
}

func TestAnAddressIsNeverAnAccountID(t *testing.T) {
	for _, c := range []struct {
		args       []string
		rest       []string
		addr       string
		bad, given bool
	}{
		{args: []string{"--web", "12"}, rest: []string{"12"}, given: true},
		{args: []string{"--web", ":0", "12"}, rest: []string{"12"}, addr: ":0", given: true},
		{args: []string{"12", "--web=127.0.0.1:0"}, rest: []string{"12"}, addr: "127.0.0.1:0", given: true},
		{args: []string{"--web", "localhost"}, addr: "localhost", given: true},
		{args: []string{"--web", "::1"}, addr: "::1", given: true},
		{args: []string{"--web", "[::1]:0"}, addr: "[::1]:0", given: true},
		{args: []string{"--web", "10.1.2.3", "--long"}, rest: []string{"--long"}, addr: "10.1.2.3", given: true},
		{args: []string{"--web", "codex"}, rest: []string{"codex"}, given: true},
		{args: []string{"--web", "8788"}, rest: []string{"8788"}, given: true},
		{args: []string{"--web", "--key"}, rest: []string{"--key"}, given: true},
		{args: []string{"12"}, rest: []string{"12"}},
		{args: []string{"--web="}, bad: true},
		{args: []string{"--web", "--web"}, bad: true},
		{args: []string{"--web", "example.com:80"}, bad: true},
		{args: []string{"--web", "127.0.0.1:http"}, bad: true},
		{args: []string{"--web=127.0.0.1:70000"}, bad: true},
	} {
		rest, w, err := takeWeb(c.args)
		switch {
		case c.bad:
			if _, ok := err.(usageError); !ok {
				t.Errorf("%v: %v, want a usage error", c.args, err)
			}
		case err != nil || !slices.Equal(rest, c.rest) || (w != nil) != c.given || (w != nil && w.Addr != c.addr):
			t.Errorf("%v: %q %+v %v", c.args, rest, w, err)
		}
	}

	// On the command line: account 12 on the default address, and on one
	// named before it, and a new login with the address after an =.
	seedTwoClaude(t)
	shortLogins(t, 500*time.Millisecond)
	for _, c := range []struct {
		args  []string
		again string
	}{
		{[]string{"login", "--web", "12"}, "`rota login 12`"},
		{[]string{"login", "--web", ":0", "12"}, "`rota login 12`"},
		{[]string{"login", "--web=127.0.0.1:0"}, "`rota login`"},
	} {
		got := webCall(t, c.args...).wait(t)
		if got.code != 1 || !strings.Contains(got.err, c.again) || !pageLine.MatchString(got.out) {
			t.Errorf("%v: %d\n%s\n%s", c.args, got.code, got.out, got.err)
		}
		if strings.Contains(c.again, "12") && !strings.Contains(got.out, "account 12") {
			t.Errorf("%v: not account 12: %s", c.args, got.out)
		}
	}
}

// Without the seam, an address with no port is handed on as it is, for the
// page to try its default port; with it, every one gets the kernel's.
func TestPageAddrKeepsThePortItIsGiven(t *testing.T) {
	was := pagePort
	t.Cleanup(func() { pagePort = was })
	pagePort = loginpage.DefaultPort
	for _, a := range []string{"127.0.0.1", "::1", ":5", "10.0.0.1:9"} {
		if got := pageAddr(a); got != a {
			t.Errorf("%q: %q", a, got)
		}
	}
	pagePort = 0
	for a, want := range map[string]string{"127.0.0.1": "127.0.0.1:0", "::1": "[::1]:0", "[::1]": "[::1]:0",
		"localhost": "localhost:0", ":5": ":5", "10.0.0.1:9": "10.0.0.1:9"} {
		if got := pageAddr(a); got != want {
			t.Errorf("%q: %q, want %q", a, got, want)
		}
	}
}

func TestLoginWebTakesAKeyUnseen(t *testing.T) {
	t.Run("an API key", func(t *testing.T) {
		keyAPI(t, 200)
		home := t.TempDir()
		t.Setenv("ROTA_HOME", home)
		r := webCall(t, "login", "--key", "--web", "127.0.0.1:0")
		u := r.url(t)
		code, body := post(t, u, testKey)
		if code != http.StatusOK {
			t.Fatalf("%d %s", code, body)
		}
		got := r.wait(t)
		if got.code != 0 || !strings.Contains(got.out, "Added claude account 1") {
			t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
		}
		secretless(t, "login --key --web", got.out, got.err, body)
	})
	t.Run("a setup token", func(t *testing.T) {
		seedTwoClaude(t)
		r := webCall(t, "login", "12", "--token", "--web", "127.0.0.1:0")
		u := r.url(t)
		code, body := post(t, u, testSetup)
		if code != http.StatusOK {
			t.Fatalf("%d %s", code, body)
		}
		got := r.wait(t)
		if got.code != 0 || !strings.Contains(got.out, "long-lived token stored for #12") {
			t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
		}
		secretless(t, "login --token --web", got.out, got.err, body)
	})
	t.Run("an API key refused, five times", func(t *testing.T) {
		keyAPI(t, 401)
		t.Setenv("ROTA_HOME", t.TempDir())
		r := webCall(t, "login", "--key", "--web", "127.0.0.1:0")
		u := r.url(t)
		var bodies []string
		for range loginpage.MaxTries {
			_, body := post(t, u, testKey)
			bodies = append(bodies, body)
		}
		got := r.wait(t)
		if got.code != 1 || !strings.Contains(got.err, "five wrong tries closed the page; nothing was stored") ||
			strings.Contains(got.err, "rota login ") {
			t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
		}
		secretless(t, "a refused key", append(bodies, got.out, got.err)...)
	})
}

// A login another rota finishes meanwhile ends the page in the words the
// terminal wait uses: whether the page notices by itself, or when a code is
// given on it.
func TestLoginWebSaysWhenAnotherRotaFinishedIt(t *testing.T) {
	for _, c := range []struct {
		name  string
		check time.Duration
		post  bool
	}{
		{"noticed by itself", 20 * time.Millisecond, false},
		{"noticed at the next code", time.Hour, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := seedTwoClaude(t)
			approvalsByCode(t, known)
			was := pendingCheck
			pendingCheck = c.check
			t.Cleanup(func() { pendingCheck = was })
			duringWait(t, func(context.Context) {
				if out, errOut, code := call(t, "login", parkedID(t, home), "AS12"); code != 0 {
					t.Errorf("the other shell: %d %q %q", code, out, errOut)
				}
			})
			r := webCall(t, "login", "12", "--web", "127.0.0.1:0")
			u := r.url(t)
			if c.post {
				post(t, u, "AS12")
			}
			got := r.wait(t)
			if got.code != 0 || !strings.Contains(got.err, "another rota finished it") || strings.Contains(got.out+got.err, "expired") {
				t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
			}
		})
	}
}

// ^C ends the page and not the login, which stays parked with the line that
// finishes it later.
func TestLoginWebLeavesTheLoginOpenOnAnInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("an interrupt cannot be sent to this process on Windows")
	}
	home := seedTwoClaude(t)
	duringWait(t, func(context.Context) {
		p, err := os.FindProcess(os.Getpid())
		if err == nil {
			err = p.Signal(os.Interrupt)
		}
		if err != nil {
			t.Error(err)
		}
	})
	got := webCall(t, "login", "12", "--web", "127.0.0.1:0").wait(t)
	if got.code != 130 {
		t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
	}
	id := loginIDFrom(t, got.out)
	if !strings.Contains(got.err, "rota login "+id+" <code>") || !strings.Contains(parked(t, home), id) {
		t.Fatalf("not left open: %q %s", got.err, parked(t, home))
	}
}

// A parked login's own id serves a page for it.
func TestLoginWebServesAParkedLogin(t *testing.T) {
	home := seedTwoClaude(t)
	approvalsByCode(t, known)
	out, errOut, code := call(t, "login")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	id := loginIDFrom(t, out)
	r := webCall(t, "login", id, "--web", "127.0.0.1:0")
	if code, body := post(t, r.url(t), "STRANGER"); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if got := r.wait(t); got.code != 0 || !strings.Contains(got.out, "Added claude account 14") {
		t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
	}
	if p := parked(t, home); p != "" {
		t.Fatalf("still parked: %s", p)
	}
}

// Off loopback the page is HTTPS, and the fingerprint printed is the
// certificate's. Only with ROTA_TEST_LAN=1: it binds an address other
// machines can reach.
func TestLoginWebOnTheLAN(t *testing.T) {
	if os.Getenv("ROTA_TEST_LAN") != "1" {
		t.Skip("binds a non-loopback address; ROTA_TEST_LAN=1 runs it")
	}
	choices, err := loginpage.Choices()
	if err != nil || len(choices) < 3 {
		t.Skipf("no address but loopback and every interface: %v %v", choices, err)
	}
	seedTwoClaude(t)
	approvalsByCode(t, known)
	addr := choices[1].Addr
	if strings.Contains(addr, ":") {
		addr = "[" + addr + "]"
	}
	r := webCall(t, "login", "12", "--web", addr+":0")
	u := r.url(t)
	if !strings.HasPrefix(u, "https://") {
		t.Fatalf("%s", u)
	}
	m := regexp.MustCompile(`its SHA-256 fingerprint is (\S+) — check that the browser shows the same\.`).FindStringSubmatch(r.out.String())
	if m == nil {
		t.Fatalf("no fingerprint: %s", r.out.String())
	}
	resp, err := browser.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sum := sha256.Sum256(resp.TLS.PeerCertificates[0].Raw)
	var hex []string
	for _, b := range sum {
		hex = append(hex, fmt.Sprintf("%02X", b))
	}
	if !strings.EqualFold(strings.Join(hex, ":"), m[1]) {
		t.Fatalf("the fingerprint printed is not the certificate's: %s", m[1])
	}
	if strings.Contains(r.out.String(), "ssh -L") {
		t.Fatal("an ssh tunnel offered for a page off loopback")
	}
	post(t, u, "AS12")
	if got := r.wait(t); got.code != 0 {
		t.Fatalf("%d\n%s\n%s", got.code, got.out, got.err)
	}
}
