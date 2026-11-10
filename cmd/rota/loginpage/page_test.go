package loginpage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// rejectAll is a Submit that turns every value down without quoting it.
func rejectAll(context.Context, string) (string, error) {
	return "", errors.New("that code is not right")
}

// serve starts a page on loopback with a kernel port, closing it with the
// test.
func serve(t *testing.T, o Options) *Page {
	t.Helper()
	if o.Addr == "" {
		o.Addr = "127.0.0.1:0"
	}
	if o.Want == "" {
		o.Want = Code
	}
	if o.Submit == nil {
		o.Submit = rejectAll
	}
	if o.Provider == "" {
		o.Provider = "claude"
	}
	p, err := Serve(context.Background(), o)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// client is an HTTP client that reuses no connection and trusts only the
// page's pinned certificate when there is one.
func client(p *Page) *http.Client {
	tr := &http.Transport{DisableKeepAlives: true}
	if p.Fingerprint != "" {
		tr.TLSClientConfig = pinned(p.Fingerprint, nil)
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// pinned is a TLS client config that accepts exactly the certificate whose
// SHA-256 is fingerprint, recording the connection when state is not nil.
func pinned(fingerprint string, state *tls.ConnectionState) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // the pin below is the check
		VerifyConnection: func(cs tls.ConnectionState) error {
			if got := colonHex(cs.PeerCertificates[0].Raw); got != fingerprint {
				return fmt.Errorf("served %s, pinned %s", got, fingerprint)
			}
			if state != nil {
				*state = cs
			}
			return nil
		},
	}
}

// colonHex is the test's own SHA-256 fingerprint of der.
func colonHex(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func do(t *testing.T, c *http.Client, method, u, body string) (int, http.Header, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, r)
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func submit(t *testing.T, p *Page, value string) (int, string) {
	t.Helper()
	code, _, body := do(t, client(p), http.MethodPost, p.URL, "value="+url.QueryEscape(value))
	return code, body
}

func ended(t *testing.T, p *Page) End {
	t.Helper()
	select {
	case e := <-p.Done:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("the page did not end")
		return End{}
	}
}

func open(t *testing.T, p *Page) {
	t.Helper()
	select {
	case e := <-p.Done:
		t.Fatalf("the page ended: %+v", e)
	default:
	}
}

// refused says no connection reaches the page's port any more.
func refused(t *testing.T, p *Page) {
	t.Helper()
	u, err := url.Parse(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err == nil {
		c.Close()
		t.Fatalf("%s still takes connections", u.Host)
	}
}

func secretOf(t *testing.T, p *Page) string {
	t.Helper()
	u, err := url.Parse(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(u.Path, "/")
}

func base(p *Page) string {
	u, _ := url.Parse(p.URL)
	return u.Scheme + "://" + u.Host
}

func tries(p *Page) int {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	return p.s.tries
}

var wantHeaders = map[string]string{
	"Cache-Control":           "no-store",
	"Pragma":                  "no-cache",
	"Referrer-Policy":         "no-referrer",
	"X-Content-Type-Options":  "nosniff",
	"X-Frame-Options":         "DENY",
	"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
}

func TestTheLinkCarriesTheSecretAndNothingElseAnswers(t *testing.T) {
	p := serve(t, Options{LoginURL: "https://claude.ai/oauth/authorize?x=1"})
	secret := secretOf(t, p)
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(raw) != 32 {
		t.Fatalf("the secret %q decodes to %d bytes (%v), want 32", secret, len(raw), err)
	}
	b := base(p)
	c := client(p)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/", 404},
		{"GET", "/x", 404},
		{"GET", "/" + secret + "x", 404},
		{"GET", "/" + secret + "/", 404},
		{"GET", "//" + secret, 404},
		{"GET", "/" + secret[:len(secret)-1], 404},
		{"POST", "/", 404},
		{"GET", "/" + secret + "?value=1", 200},
		{"HEAD", "/" + secret, 200},
		{"PUT", "/" + secret, 405},
		{"DELETE", "/" + secret, 405},
	} {
		code, h, _ := do(t, c, tc.method, b+tc.path, "")
		if code != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, code, tc.want)
		}
		for k, v := range wantHeaders {
			if got := h.Get(k); got != v {
				t.Errorf("%s %s: %s is %q, want %q", tc.method, tc.path, k, got, v)
			}
		}
	}
	if n := tries(p); n != 0 {
		t.Errorf("the wrong paths counted %d tries", n)
	}
	open(t, p)

	_, _, page := do(t, c, "GET", p.URL, "")
	for _, want := range []string{
		`rel="noreferrer"`, `autocomplete="off"`, `method="post"`,
		`name="value"`, "claude",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s:\n%s", want, page)
		}
	}
	if strings.Contains(strings.ToLower(page), "<script") {
		t.Errorf("the page carries a script:\n%s", page)
	}
}

func TestThePageSaysWhatItIsFor(t *testing.T) {
	ends := time.Now().Add(time.Minute)
	p := serve(t, Options{
		Want:     Code,
		Provider: "claude",
		Approve:  "ana@example.com",
		LoginURL: "https://claude.ai/oauth/authorize?code=true&client_id=x",
		Ends:     ends,
	})
	_, _, page := do(t, client(p), "GET", p.URL, "")
	for _, want := range []string{
		"claude", "ana@example.com",
		`href="https://claude.ai/oauth/authorize?code=true&amp;client_id=x"`,
		`type="text"`,
		ends.Format("15:04"),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s:\n%s", want, page)
		}
	}

	for _, w := range []Want{Key, Token} {
		p := serve(t, Options{Want: w, Provider: "claude"})
		_, _, page := do(t, client(p), "GET", p.URL, "")
		if !strings.Contains(page, `type="password"`) {
			t.Errorf("%s: the field is not a password field:\n%s", w, page)
		}
		if strings.Contains(page, "<a ") {
			t.Errorf("%s: the page links somewhere:\n%s", w, page)
		}
	}
}

func TestProviderTextIsEscapedAndAScriptLinkIsDefused(t *testing.T) {
	p := serve(t, Options{
		Provider: `<b>claude</b>`,
		Approve:  `"><script>alert(1)</script>`,
		LoginURL: "javascript:alert(1)",
	})
	_, _, page := do(t, client(p), "GET", p.URL, "")
	for _, bad := range []string{"<b>claude", "<script>", "javascript:"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page carries %q:\n%s", bad, page)
		}
	}
}

func TestTheValueGoesInTheBodyAndIsNeverEchoed(t *testing.T) {
	const value = "CODE-123"
	var mu sync.Mutex
	var got []string
	answers := []struct {
		done string
		err  error
	}{
		{"", fmt.Errorf("the provider says %s is not a code it knows (%q)", value, value)},
		{"", errors.New(`{"error":"invalid_grant","code":"` + value + `"}`)},
		{"Signed in: ana@example.com, " + value, nil},
	}
	p := serve(t, Options{Submit: func(_ context.Context, v string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, v)
		a := answers[0]
		answers = answers[1:]
		return a.done, a.err
	}})
	var bodies []string
	for range 3 {
		_, body := submit(t, p, value)
		bodies = append(bodies, body)
	}
	e := ended(t, p)
	if e.Why != Accepted {
		t.Fatalf("ended %+v, want Accepted", e)
	}
	for i, body := range bodies {
		if strings.Contains(body, value) {
			t.Errorf("answer %d echoes the value:\n%s", i, body)
		}
		if strings.Contains(body, url.QueryEscape(value)) {
			t.Errorf("answer %d echoes the escaped value:\n%s", i, body)
		}
	}
	if !strings.Contains(bodies[0], "is not a code it knows") {
		t.Errorf("a rejection does not show the error:\n%s", bodies[0])
	}
	if !strings.Contains(bodies[2], "Signed in: ana@example.com") {
		t.Errorf("the acceptance does not show done:\n%s", bodies[2])
	}
	if strings.Contains(e.Message, value) {
		t.Errorf("the end's message echoes the value: %q", e.Message)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, v := range got {
		if v != value {
			t.Errorf("Submit %d got %q, want %q", i, v, value)
		}
	}

	// A value in the query is never read: none in the body is nothing at all.
	calls := 0
	q := serve(t, Options{Submit: func(_ context.Context, v string) (string, error) {
		calls++
		return "", fmt.Errorf("got %q", v)
	}})
	code, _, body := do(t, client(q), http.MethodPost, q.URL+"?value=1", "other=2")
	if calls != 0 {
		t.Errorf("Submit ran %d times for a value in the query", calls)
	}
	if code == http.StatusOK || strings.Contains(body, `got "1"`) {
		t.Errorf("a value in the query was taken: %d\n%s", code, body)
	}
	if n := tries(q); n != 0 {
		t.Errorf("an empty value counted %d tries", n)
	}
	open(t, q)
}

func TestAKeyShapedErrorIsUnsecreted(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("A", 90)
	other := "sk-ant-oat01-" + strings.Repeat("B", 90)
	p := serve(t, Options{Want: Key, Submit: func(context.Context, string) (string, error) {
		return "", errors.New("refused " + other)
	}})
	_, body := submit(t, p, key)
	if strings.Contains(body, other) || strings.Contains(body, key) {
		t.Errorf("a key reached the page:\n%s", body)
	}
}

func TestSubmissionsAreOneAtATime(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p := serve(t, Options{Submit: func(context.Context, string) (string, error) {
		once.Do(func() { close(entered) })
		<-release
		return "", errors.New("wrong")
	}})
	first := make(chan int)
	go func() {
		code, _ := submit(t, p, "one")
		first <- code
	}()
	<-entered
	for i := range 10 {
		code, body := submit(t, p, fmt.Sprintf("parallel-%d", i))
		if code != http.StatusConflict {
			t.Errorf("a POST beside one running answered %d, want 409", code)
		}
		if !strings.Contains(body, "one at a time") {
			t.Errorf("the 409 does not say why:\n%s", body)
		}
	}
	close(release)
	<-first
	if n := tries(p); n != 1 {
		t.Errorf("%d tries counted, want 1", n)
	}
	open(t, p)
}

func TestFiveWrongTriesEndThePage(t *testing.T) {
	p := serve(t, Options{})
	for i := 1; i <= MaxTries; i++ {
		code, body := submit(t, p, fmt.Sprintf("wrong-%d", i))
		if code == http.StatusOK {
			t.Errorf("try %d answered 200", i)
		}
		if !strings.Contains(body, "that code is not right") {
			t.Errorf("try %d does not show the error:\n%s", i, body)
		}
		if i < MaxTries {
			open(t, p)
		}
	}
	e := ended(t, p)
	if e.Why != Tries {
		t.Fatalf("ended %+v, want Tries", e)
	}
	refused(t, p)
}

func TestWrongPathsDoNotCountAsTries(t *testing.T) {
	p := serve(t, Options{Submit: func(_ context.Context, v string) (string, error) {
		if v == "right" {
			return "signed in", nil
		}
		return "", errors.New("wrong")
	}})
	b := base(p)
	c := client(p)
	for i := range 200 {
		buf := make([]byte, 8)
		rand.Read(buf)
		path := "/" + base64.RawURLEncoding.EncodeToString(buf)
		method := "GET"
		if i%2 == 1 {
			method = "POST"
		}
		if code, _, _ := do(t, c, method, b+path, "value=x"); code != http.StatusNotFound {
			t.Fatalf("%s %s: %d, want 404", method, path, code)
		}
	}
	open(t, p)
	if code, body := submit(t, p, "right"); code != http.StatusOK || !strings.Contains(body, "signed in") {
		t.Fatalf("the right value answered %d:\n%s", code, body)
	}
	if e := ended(t, p); e.Why != Accepted || e.Message != "signed in" {
		t.Fatalf("ended %+v, want Accepted with signed in", e)
	}
}

func TestAnEmptyValueCountsNoTry(t *testing.T) {
	calls := 0
	p := serve(t, Options{Submit: func(context.Context, string) (string, error) {
		calls++
		return "", errors.New("wrong")
	}})
	for _, v := range []string{"", "   ", "\r\n"} {
		submit(t, p, v)
	}
	if calls != 0 || tries(p) != 0 {
		t.Errorf("an empty value ran Submit %d times and counted %d tries", calls, tries(p))
	}
	open(t, p)
}

func TestAPastedValueIsTrimmed(t *testing.T) {
	var got string
	p := serve(t, Options{Submit: func(_ context.Context, v string) (string, error) {
		got = v
		return "ok", nil
	}})
	submit(t, p, "  abc#def\r\n")
	ended(t, p)
	if got != "abc#def" {
		t.Errorf("Submit got %q, want abc#def", got)
	}
}

func TestAFinalRefusalEndsThePage(t *testing.T) {
	p := serve(t, Options{Submit: func(context.Context, string) (string, error) {
		return "", Final(errors.New("this login is no longer open"))
	}})
	_, body := submit(t, p, "anything")
	if !strings.Contains(body, "this login is no longer open") {
		t.Errorf("the refusal is not shown:\n%s", body)
	}
	e := ended(t, p)
	if e.Why != Refused || e.Message != "this login is no longer open" {
		t.Fatalf("ended %+v, want Refused with the message", e)
	}
	refused(t, p)
	if Final(nil) != nil {
		t.Error("Final(nil) is not nil")
	}
	inner := errors.New("inner")
	if !errors.Is(Final(inner), inner) {
		t.Error("Final does not wrap its error")
	}
}

func TestTheFirstAcceptedLoginEndsIt(t *testing.T) {
	p := serve(t, Options{Submit: func(context.Context, string) (string, error) {
		return "Signed in as ana@example.com.", nil
	}})
	code, body := submit(t, p, "good")
	if code != http.StatusOK || !strings.Contains(body, "Signed in as ana@example.com.") {
		t.Fatalf("the acceptance answered %d:\n%s", code, body)
	}
	if strings.Contains(body, "<form") {
		t.Errorf("the accepted page still offers the form:\n%s", body)
	}
	e := ended(t, p)
	if e.Why != Accepted || e.Message != "Signed in as ana@example.com." {
		t.Fatalf("ended %+v", e)
	}
	refused(t, p)
}

func TestTheTimeLimitEndsIt(t *testing.T) {
	old := lifetime
	lifetime = 100 * time.Millisecond
	start := time.Now()
	p := serve(t, Options{})
	e := ended(t, p)
	lifetime = old
	if e.Why != TimedOut {
		t.Fatalf("ended %+v, want TimedOut", e)
	}
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Errorf("ended after %v, before its lifetime", took)
	}
	refused(t, p)

	start = time.Now()
	p = serve(t, Options{Ends: time.Now().Add(50 * time.Millisecond)})
	if e := ended(t, p); e.Why != TimedOut {
		t.Fatalf("ended %+v, want TimedOut", e)
	}
	if took := time.Since(start); took < 50*time.Millisecond || took > 5*time.Second {
		t.Errorf("ended after %v, want the login's end", took)
	}

	ctx, cancel := context.WithCancel(context.Background())
	q, err := Serve(ctx, Options{Addr: "127.0.0.1:0", Want: Code, Submit: rejectAll})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	cancel()
	if e := ended(t, q); e.Why != Stopped {
		t.Fatalf("ended %+v, want Stopped", e)
	}
	refused(t, q)

	r := serve(t, Options{})
	r.Close()
	if e := ended(t, r); e.Why != Stopped {
		t.Fatalf("ended %+v, want Stopped", e)
	}
	r.Close() // twice is harmless
}

func TestAnEndWhileSubmittingKeepsAnAcceptance(t *testing.T) {
	entered := make(chan struct{})
	p := serve(t, Options{Submit: func(ctx context.Context, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "accepted at the wire", nil
	}})
	go submit2(p, "good")
	<-entered
	p.Close()
	if e := ended(t, p); e.Why != Accepted {
		t.Fatalf("ended %+v, want the acceptance that happened", e)
	}
}

// submit2 posts without failing the test, for a request the page may cut.
func submit2(p *Page, value string) {
	resp, err := client(p).PostForm(p.URL, url.Values{"value": {value}})
	if err == nil {
		resp.Body.Close()
	}
}

func TestLoopbackIsPlainAndElsewhereIsTLS(t *testing.T) {
	p := serve(t, Options{})
	if !strings.HasPrefix(p.URL, "http://127.0.0.1:") || p.Fingerprint != "" {
		t.Fatalf("loopback serves %s with fingerprint %q", p.URL, p.Fingerprint)
	}

	old := tlsFor
	tlsFor = func(netip.Addr) bool { return true }
	before := time.Now()
	tp := serve(t, Options{Ends: before.Add(3 * time.Minute)})
	tlsFor = old
	checkTLS(t, tp, netip.MustParseAddr("127.0.0.1"), before, before.Add(3*time.Minute))

	if os.Getenv("ROTA_TEST_LAN") == "" {
		t.Log("ROTA_TEST_LAN is unset: no address off loopback is bound")
		return
	}
	lan, ok := lanAddr()
	if !ok {
		t.Skip("this machine has no address off loopback")
	}
	before = time.Now()
	lp := serve(t, Options{Addr: net.JoinHostPort(lan.String(), "0")})
	checkTLS(t, lp, lan, before, before.Add(lifetime))
}

func lanAddr() (netip.Addr, bool) {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if ip.Is4() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip, true
		}
	}
	return netip.Addr{}, false
}

func checkTLS(t *testing.T, p *Page, host netip.Addr, before, end time.Time) {
	t.Helper()
	if !strings.HasPrefix(p.URL, "https://") {
		t.Fatalf("%s serves %s", host, p.URL)
	}
	if len(p.Fingerprint) != 32*3-1 {
		t.Fatalf("the fingerprint %q is not colon hex of SHA-256", p.Fingerprint)
	}
	var cs tls.ConnectionState
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   pinned(p.Fingerprint, &cs),
	}}
	resp, err := c.Get(p.URL)
	if err != nil {
		t.Fatalf("a client pinning the fingerprint: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the pinned client got %d", resp.StatusCode)
	}
	if cs.Version != tls.VersionTLS13 {
		t.Errorf("negotiated %s, want TLS 1.3", tls.VersionName(cs.Version))
	}
	leaf := cs.PeerCertificates[0]
	if colonHex(leaf.Raw) != p.Fingerprint {
		t.Errorf("the served leaf is not the fingerprinted one")
	}
	if !containsIP(leaf, host) {
		t.Errorf("the certificate names %v, not %s", leaf.IPAddresses, host)
	}
	if len(leaf.DNSNames) != 0 {
		t.Errorf("the certificate names hosts %v", leaf.DNSNames)
	}
	if d := before.Add(-time.Minute).Sub(leaf.NotBefore); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("NotBefore is %v, want a minute before %v", leaf.NotBefore, before)
	}
	if d := end.Add(time.Minute).Sub(leaf.NotAfter); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("NotAfter is %v, want a minute after %v", leaf.NotAfter, end)
	}
	if k, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P256() {
		t.Errorf("the key is %v, want ECDSA P-256", leaf.PublicKeyAlgorithm)
	}

	old := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig: func() *tls.Config {
			cfg := pinned(p.Fingerprint, nil)
			cfg.MaxVersion = tls.VersionTLS12
			return cfg
		}(),
	}}
	if resp, err := old.Get(p.URL); err == nil {
		resp.Body.Close()
		t.Error("a TLS 1.2 client was served")
	}

	plain := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err = plain.Get("http" + strings.TrimPrefix(p.URL, "https"))
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK || bytes.Contains(b, []byte("<form")) {
			t.Errorf("plain HTTP was served the page: %d", resp.StatusCode)
		}
	}
}

func containsIP(c *x509.Certificate, a netip.Addr) bool {
	for _, ip := range c.IPAddresses {
		if got, ok := netip.AddrFromSlice(ip); ok && got.Unmap() == a {
			return true
		}
	}
	return false
}

func TestEveryInterfaceIsNamedForAnUnspecifiedAddress(t *testing.T) {
	old := interfaceAddrs
	defer func() { interfaceAddrs = old }()
	interfaceAddrs = func() ([]net.Addr, error) {
		var out []net.Addr
		for _, s := range []string{"127.0.0.1/8", "::1/128", "192.168.1.20/24", "10.0.0.5/8", "fe80::1/64", "2001:db8::7/64"} {
			ip, n, _ := net.ParseCIDR(s)
			n.IP = ip
			out = append(out, n)
		}
		return out, nil
	}
	for _, tc := range []struct {
		host string
		want []string
	}{
		{"0.0.0.0", []string{"192.168.1.20", "10.0.0.5", "2001:db8::7"}},
		{"::", []string{"192.168.1.20", "10.0.0.5", "2001:db8::7"}},
		{"192.168.1.20", []string{"192.168.1.20"}},
	} {
		got, err := sansFor(netip.MustParseAddr(tc.host))
		if err != nil {
			t.Fatalf("%s: %v", tc.host, err)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s names %v, want %v", tc.host, got, tc.want)
		}
	}
	interfaceAddrs = func() ([]net.Addr, error) { return nil, nil }
	if _, err := sansFor(netip.MustParseAddr("0.0.0.0")); err == nil {
		t.Error("an unspecified address with no interface to name was taken")
	}
}

func TestANameIsRefusedAndLocalhostIsLoopback(t *testing.T) {
	for _, name := range []string{"example.com", "example.com:8788", "my-laptop"} {
		_, err := Serve(context.Background(), Options{Addr: name, Want: Code, Submit: rejectAll})
		host := strings.TrimSuffix(name, ":8788")
		if err == nil || err.Error() != "give the page an address, not a name: "+host {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, addr := range []string{"localhost:0", ":0", "127.0.0.1:0"} {
		p := serve(t, Options{Addr: addr})
		if !strings.HasPrefix(p.URL, "http://127.0.0.1:") || strings.HasSuffix(base(p), ":0") {
			t.Errorf("%s serves %s", addr, p.URL)
		}
	}
	if ln, err := net.Listen("tcp", "[::1]:0"); err == nil {
		ln.Close()
		p := serve(t, Options{Addr: "[::1]:0"})
		if !strings.HasPrefix(p.URL, "http://[::1]:") {
			t.Errorf("::1 serves %s", p.URL)
		}
	}
	for _, bad := range []string{"127.0.0.1:x", "127.0.0.1:70000"} {
		if _, err := Serve(context.Background(), Options{Addr: bad, Want: Code, Submit: rejectAll}); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}
	if _, err := Serve(context.Background(), Options{Addr: "127.0.0.1:0", Want: "pin", Submit: rejectAll}); err == nil {
		t.Error("an unknown Want was taken")
	}
	if _, err := Serve(context.Background(), Options{Addr: "127.0.0.1:0", Want: Code}); err == nil {
		t.Error("no Submit was taken")
	}
}

func TestABusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port

	if _, err := Serve(context.Background(), Options{Addr: ln.Addr().String(), Want: Code, Submit: rejectAll}); err == nil {
		t.Error("an explicit busy port was taken")
	}

	old := defaultPort
	defaultPort = busy
	defer func() { defaultPort = old }()
	for _, addr := range []string{"127.0.0.1", "localhost", ""} {
		p := serve(t, Options{Addr: addr})
		u, _ := url.Parse(p.URL)
		if u.Port() == fmt.Sprint(busy) || u.Port() == "0" || u.Port() == "" {
			t.Errorf("%q with the default port busy serves %s", addr, p.URL)
		}
	}
	if DefaultPort != 8788 {
		t.Errorf("DefaultPort is %d", DefaultPort)
	}
}

func TestAnOversizedBodyCountsNoTry(t *testing.T) {
	calls := 0
	p := serve(t, Options{Submit: func(context.Context, string) (string, error) {
		calls++
		return "", errors.New("wrong")
	}})
	body := "value=" + strings.Repeat("a", 65<<10)
	code, _, _ := do(t, client(p), http.MethodPost, p.URL, body)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("65 KiB answered %d, want 413", code)
	}
	if calls != 0 || tries(p) != 0 {
		t.Errorf("an oversized body ran Submit %d times and counted %d tries", calls, tries(p))
	}
	open(t, p)

	// Just under the limit is read.
	body = "value=" + strings.Repeat("a", 64<<10-len("value=")-1)
	if code, _, _ := do(t, client(p), http.MethodPost, p.URL, body); code == http.StatusRequestEntityTooLarge {
		t.Error("a body under 64 KiB was refused as too large")
	}
	if calls != 1 {
		t.Errorf("Submit ran %d times, want 1", calls)
	}
}

func TestNothingIsLogged(t *testing.T) {
	srv := newServer(http.NotFoundHandler(), nil)
	if srv.ErrorLog == nil || srv.ErrorLog.Writer() != io.Discard {
		t.Error("the server's ErrorLog does not write to io.Discard")
	}
	if srv.ReadHeaderTimeout != 10*time.Second || srv.ReadTimeout != 30*time.Second ||
		srv.WriteTimeout != 30*time.Second || srv.IdleTimeout != 30*time.Second ||
		srv.MaxHeaderBytes != 8<<10 {
		t.Errorf("timeouts %v %v %v %v, header limit %d", srv.ReadHeaderTimeout,
			srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout, srv.MaxHeaderBytes)
	}

	var buf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(b)
	}))
	defer log.SetOutput(os.Stderr)

	old := tlsFor
	tlsFor = func(netip.Addr) bool { return true }
	p := serve(t, Options{Submit: func(context.Context, string) (string, error) {
		panic("SECRET-VALUE")
	}})
	tlsFor = old
	if l := p.s.srv.ErrorLog; l == nil || l.Writer() != io.Discard {
		t.Error("the page's server logs")
	}

	// A broken handshake, which net/http logs by default.
	u, _ := url.Parse(p.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("\x16\x03\x01\x00\x05hello"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.Copy(io.Discard, conn)
	conn.Close()
	// A handler that panics, which net/http logs with a stack by default.
	submit2(p, "SECRET-VALUE")

	mu.Lock()
	defer mu.Unlock()
	if buf.Len() != 0 {
		t.Errorf("something was logged:\n%s", buf.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
