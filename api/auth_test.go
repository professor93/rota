package api

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// These are about who is asking and what that lets them do. The server
// underneath is the same one every other test here uses; what changes is the
// credential on the request.

const testPlain = "correct horse"

// cheapHash is testPlain derived once for the whole package, at the floor
// rota allows rather than the figure it writes. The shape is identical and
// the cost is a sixth: lowering it is exactly what a test may do and a file
// may not, and a sign-in is otherwise the slowest thing in this package.
//
// It is derived on first use and not at start-up, and that matters more than
// it looks: this test binary is also the fake vendor CLI, re-executed for
// every run, and a key derivation in package initialization would be paid by
// each of those children before they printed a word.
var cheapHash = sync.OnceValue(func() string {
	salt := []byte("sixteen bytes!!!")
	key, err := pbkdf2.Key(sha256.New, testPlain, salt, pwMinIterations, pwKeyLen)
	if err != nil {
		panic(err)
	}
	return (&password{iterations: pwMinIterations, salt: salt, hash: key}).String()
})

// withPeople is a harness whose server knows one watcher and one controller,
// by password and by token.
type people struct {
	*harness
	watchTok, controlTok string
}

func withPeople(t *testing.T) *people {
	t.Helper()
	watchTok, controlTok := "watch-token-aaaa", "control-token-bbbb"
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	h := newHarness(t, Options{
		Users: []User{
			{Name: "looker", Role: RoleWatch, Password: cheapHash()},
			{Name: "driver", Role: RoleControl, Password: cheapHash()},
		},
		Tokens: []TokenPrincipal{
			{Name: "ci-watch", Role: RoleWatch, SHA256: sum(watchTok)},
			{Name: "ci-control", Role: RoleControl, SHA256: sum(controlTok)},
		},
		SessionTTL: time.Hour,
	})
	return &people{harness: h, watchTok: watchTok, controlTok: controlTok}
}

// as sends one request carrying a bearer token of the caller's choosing.
func (p *people) as(token, method, path string, body any, hdr ...string) (*http.Response, []byte) {
	p.t.Helper()
	old := p.token
	p.token = token
	defer func() { p.token = old }()
	return p.do(method, path, body, hdr...)
}

/* ------------------------------------------------------- the role table --- */

// TestEveryGuardedRouteHasARole is what keeps the table honest: a route
// added to the server without a row here would silently take the default,
// and the point of the table is that nobody has to remember which default
// that was.
func TestEveryGuardedRouteHasARole(t *testing.T) {
	s, err := New(Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	registered := map[string]bool{}
	for pattern := range s.guarded() {
		registered[pattern] = true
	}
	for pattern := range s.sockets() {
		registered[pattern] = true
	}
	for pattern := range registered {
		if _, ok := roleFor[pattern]; !ok {
			t.Errorf("%s is guarded but has no row in roleFor; say which role it takes", pattern)
		}
	}
	for pattern := range roleFor {
		if !registered[pattern] {
			t.Errorf("roleFor has a row for %s, which this server does not register", pattern)
		}
	}
	// And the rule the table is meant to encode, stated once more as a
	// check: nothing that changes anything is watch.
	for pattern, role := range roleFor {
		method, _, _ := strings.Cut(pattern, " ")
		if role == RoleWatch && method != "GET" && method != "HEAD" {
			t.Errorf("%s changes something and may not be watch", pattern)
		}
	}
}

func TestUnknownPatternTakesControl(t *testing.T) {
	if got := needs("GET /v1/something-new"); got != RoleControl {
		t.Fatalf("a route nobody wrote a row for is %q, want control", got)
	}
}

/* ------------------------------------------------------ what a role does --- */

func TestWatchTokenReadsAndDoesNotRun(t *testing.T) {
	p := withPeople(t)
	for _, path := range []string{"/v1/schema", "/v1/accounts", "/v1/runs", "/v1/accounts/1/schema"} {
		if resp, raw := p.as(p.watchTok, "GET", path, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("a watcher must be able to GET %s: %d %s", path, resp.StatusCode, raw)
		}
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/run"},
		{"POST", "/v1/accounts/1/run"},
		{"PATCH", "/v1/accounts/1"},
		{"DELETE", "/v1/accounts/1"},
		{"POST", "/v1/login"},
		{"POST", "/v1/invites"},
		{"POST", "/v1/runs/r-1/messages"},
	} {
		resp, raw := p.as(p.watchTok, c.method, c.path, map[string]any{"prompt": "x"})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s as a watcher: %d %s", c.method, c.path, resp.StatusCode, raw)
		}
		if !strings.Contains(string(raw), watchOnly) {
			t.Fatalf("%s %s must say why: %s", c.method, c.path, raw)
		}
	}
	// A wrong role is not a guess: the limiter must not have counted any of
	// those, or a watcher clicking about the page would lock the address
	// out of a server they are allowed to be on.
	if p.api.limit.blocked("127.0.0.1") {
		t.Fatal("role refusals must not count towards the brute-force block")
	}
	// And control still runs.
	if resp, raw := p.as(p.controlTok, "POST", "/v1/accounts/1/run",
		map[string]any{"prompt": "x"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("control must run: %d %s", resp.StatusCode, raw)
	}
}

func TestBearerTokenNamesItsPrincipal(t *testing.T) {
	p := withPeople(t)
	for _, c := range []struct {
		token, name string
		role        Role
	}{
		{p.token, "token", RoleControl},
		{p.watchTok, "ci-watch", RoleWatch},
		{p.controlTok, "ci-control", RoleControl},
	} {
		resp, raw := p.as(c.token, "GET", "/v1/session", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/session as %s: %d %s", c.name, resp.StatusCode, raw)
		}
		var got sessionView
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.Name != c.name || got.Role != c.role || got.Via != "token" {
			t.Fatalf("%s came back as %+v", c.name, got)
		}
	}
	// A token nobody knows is still a guess, answered as it always was.
	resp, raw := p.as("not-a-token", "GET", "/v1/accounts", nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(raw), "invalid or missing bearer token") {
		t.Fatalf("a wrong token: %d %s", resp.StatusCode, raw)
	}
}

/* -------------------------------------------------------------- sign in --- */

// browser is an http.Client that keeps cookies and sends an Origin, which is
// what a page does.
func (p *people) browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// page sends one request the way the playground does: no bearer token, the
// cookie jar, and this server's own Origin.
func (p *people) page(t *testing.T, c *http.Client, method, path, body string, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, p.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", p.srv.URL)
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := readAll(t, resp)
	return resp, raw
}

func TestSignInAndOut(t *testing.T) {
	p := withPeople(t)
	c := p.browser(t)

	// The page asks first, and is told it is nobody.
	if resp, _ := p.page(t, c, "GET", "/v1/session", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed out, GET /v1/session is %d", resp.StatusCode)
	}
	resp, raw := p.page(t, c, "POST", "/v1/session", `{"name":"driver","password":"`+testPlain+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signing in: %d %s", resp.StatusCode, raw)
	}
	var got sessionView
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "driver" || got.Role != RoleControl || got.Via != "user" || got.Expires.IsZero() {
		t.Fatalf("signed in as %+v", got)
	}
	// The cookie itself: nobody's script may read it, nobody else's page may
	// send it, and it is not Secure because this server is plain http.
	ck := cookieNamed(resp, sessionCookie)
	if ck == nil {
		t.Fatal("no cookie was set")
	}
	if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" {
		t.Fatalf("cookie attributes: %+v", ck)
	}
	if ck.Secure {
		t.Fatal("Secure on a plain http server would mean the browser never sent it back")
	}
	if strings.Contains(string(raw), ck.Value) {
		t.Fatal("the session id must not also be in the body")
	}

	// Now the cookie alone works, and says who it is.
	if resp, raw := p.page(t, c, "GET", "/v1/session", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("with the cookie: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := p.page(t, c, "GET", "/v1/accounts", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the API with the cookie: %d %s", resp.StatusCode, raw)
	}

	// And signing out ends it on the server, not only in the browser.
	before := p.stealCookie(t, c)
	if resp, raw := p.page(t, c, "DELETE", "/v1/session", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("signing out: %d %s", resp.StatusCode, raw)
	}
	resp, _ = p.withCookie(t, before, "GET", "/v1/session")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a session that was ended still works: %d", resp.StatusCode)
	}
}

func TestSignInRefusesTheSameWayWhoeverAsks(t *testing.T) {
	p := withPeople(t)
	c := p.browser(t)
	before := kdfRuns.Load()
	answers := map[string]bool{}
	for _, body := range []string{
		`{"name":"driver","password":"wrong"}`,
		`{"name":"nobody-at-all","password":"wrong"}`,
	} {
		resp, raw := p.page(t, c, "POST", "/v1/session", body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, raw)
		}
		if strings.Contains(string(raw), "nobody-at-all") || strings.Contains(string(raw), "driver") {
			t.Fatalf("the refusal must not repeat the name: %s", raw)
		}
		answers[string(raw)] = true
	}
	if len(answers) != 1 {
		t.Fatalf("a wrong password and an unknown name must read the same: %v", answers)
	}
	// Wall-clock timing is too noisy to assert on; what can be asserted is
	// the fact underneath it — both answers cost one derivation, so neither
	// is the fast one.
	if got := kdfRuns.Load() - before; got != 2 {
		t.Fatalf("two sign-ins ran the KDF %d times, want 2", got)
	}
}

func TestFailedSignInsAreCountedLikeBadTokens(t *testing.T) {
	p := withPeople(t)
	c := p.browser(t)
	var last int
	for range failMax + 1 {
		resp, _ := p.page(t, c, "POST", "/v1/session", `{"name":"driver","password":"wrong"}`)
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong passwords: %d, want 429", failMax+1, last)
	}
	// And the right password is refused too while the block lasts: this is
	// the one place a block outlives being right, because the alternative is
	// an unlimited password oracle.
	if resp, _ := p.page(t, c, "POST", "/v1/session", `{"name":"driver","password":"`+testPlain+`"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("while blocked, the right password answered %d", resp.StatusCode)
	}
}

func TestSessionExpires(t *testing.T) {
	p := withPeople(t)
	c := p.browser(t)
	if resp, raw := p.page(t, c, "POST", "/v1/session", `{"name":"looker","password":"`+testPlain+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("signing in: %d %s", resp.StatusCode, raw)
	}
	// Move the table's clock past the session rather than waiting an hour.
	p.api.sessions.mu.Lock()
	p.api.sessions.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	p.api.sessions.mu.Unlock()
	if resp, _ := p.page(t, c, "GET", "/v1/session", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an expired session still answered %d", resp.StatusCode)
	}
}

func TestCookieIsSecureOnlyUnderTLS(t *testing.T) {
	h := newHarness(t, Options{Users: []User{{Name: "driver", Role: RoleControl, Password: cheapHash()}}})
	tls := httptest.NewTLSServer(h.handler)
	defer tls.Close()
	c := tls.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequest("POST", tls.URL+"/v1/session",
		strings.NewReader(`{"name":"driver","password":"`+testPlain+`"}`))
	req.Header.Set("Origin", tls.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signing in over TLS: %d %s", resp.StatusCode, readAll(t, resp))
	}
	ck := cookieNamed(resp, sessionCookie)
	if ck == nil || !ck.Secure {
		t.Fatalf("a cookie earned over TLS must be Secure: %+v", ck)
	}
}

/* ----------------------------------------------------------- cross-site --- */

func TestCookieWritesNeedTheirOwnOrigin(t *testing.T) {
	p := withPeople(t)
	c := p.browser(t)
	if resp, raw := p.page(t, c, "POST", "/v1/session", `{"name":"driver","password":"`+testPlain+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("signing in: %d %s", resp.StatusCode, raw)
	}
	// Reading is fine from anywhere the browser will send the cookie at all.
	if resp, _ := p.page(t, c, "GET", "/v1/accounts", "", "Origin", "https://evil.example"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a GET must not be refused over Origin: %d", resp.StatusCode)
	}
	for _, origin := range []string{"https://evil.example", ""} {
		resp, raw := p.page(t, c, "POST", "/v1/accounts/1/run", `{"prompt":"x"}`, "Origin", origin)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(raw), crossSiteNo) {
			t.Fatalf("a cookie write with Origin %q: %d %s", origin, resp.StatusCode, raw)
		}
	}
	// A Referer says the same thing when there is no Origin.
	resp, raw := p.page(t, c, "POST", "/v1/accounts/1/run", `{"prompt":"x"}`,
		"Origin", "", "Referer", p.srv.URL+"/playground")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a Referer from this server must do: %d %s", resp.StatusCode, raw)
	}
	// And a bearer token is exempt: nothing attaches one by accident.
	if resp, raw := p.as(p.controlTok, "POST", "/v1/accounts/1/run", map[string]any{"prompt": "x"},
		"Origin", "https://evil.example"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a token is not a cookie: %d %s", resp.StatusCode, raw)
	}
}

/* ------------------------------------------------------------- invites --- */

func TestInviteIsOneWatcherOnce(t *testing.T) {
	p := withPeople(t)
	resp, raw := p.do("POST", "/v1/invites", map[string]any{"ttl": "5m"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("making an invite: %d %s", resp.StatusCode, raw)
	}
	var made struct {
		URL     string    `json:"url"`
		Expires time.Time `json:"expires"`
	}
	if err := json.Unmarshal(raw, &made); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(made.URL, p.srv.URL+"/invite/") {
		t.Fatalf("the link is %q", made.URL)
	}
	if d := time.Until(made.Expires); d < 4*time.Minute || d > 6*time.Minute {
		t.Fatalf("it expires in %v", d)
	}

	c := p.browser(t)
	resp, _ = p.follow(t, c, made.URL)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("following the link: %d", resp.StatusCode)
	}
	if to := resp.Header.Get("Location"); to != "/playground" {
		t.Fatalf("it led to %q", to)
	}
	// Signed in, as a watcher, with a name that says which invite it was and
	// is not the invite.
	resp, raw = p.page(t, c, "GET", "/v1/session", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after the invite: %d %s", resp.StatusCode, raw)
	}
	var got sessionView
	json.Unmarshal(raw, &got)
	if got.Role != RoleWatch || got.Via != "invite" || !strings.HasPrefix(got.Name, "invite-") {
		t.Fatalf("the invited principal is %+v", got)
	}
	if len(got.Name) > len("invite-")+6 {
		t.Fatalf("the name carries more of the code than it should: %q", got.Name)
	}
	// And a watcher it is.
	if resp, _ := p.page(t, c, "POST", "/v1/accounts/1/run", `{"prompt":"x"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an invited watcher ran something: %d", resp.StatusCode)
	}

	// The same link a second time is nothing.
	resp, _ = p.follow(t, p.browser(t), made.URL)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a spent invite answered %d", resp.StatusCode)
	}
}

func TestInviteTTLIsCappedAndPositive(t *testing.T) {
	p := withPeople(t)
	for _, c := range []struct{ ttl, want string }{
		{"48h", "an invite may last a day at most"},
		{"-1m", "an invite has to last longer than nothing"},
		{"soon", "ttl is a length of time"},
	} {
		resp, raw := p.do("POST", "/v1/invites", map[string]any{"ttl": c.ttl})
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), c.want) {
			t.Fatalf("ttl %q: %d %s", c.ttl, resp.StatusCode, raw)
		}
	}
	// No body at all is the default.
	if resp, raw := p.do("POST", "/v1/invites", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("an invite with no body: %d %s", resp.StatusCode, raw)
	}
}

func TestInviteExpires(t *testing.T) {
	p := withPeople(t)
	_, raw := p.do("POST", "/v1/invites", map[string]any{"ttl": "1m"})
	var made struct {
		URL string `json:"url"`
	}
	json.Unmarshal(raw, &made)
	p.api.invites.mu.Lock()
	p.api.invites.now = func() time.Time { return time.Now().Add(time.Hour) }
	p.api.invites.mu.Unlock()
	if resp, _ := p.follow(t, p.browser(t), made.URL); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an expired invite answered %d", resp.StatusCode)
	}
}

func TestGuessingAnInviteIsCounted(t *testing.T) {
	p := withPeople(t)
	c := p.browser(t)
	var last int
	for i := range failMax + 1 {
		resp, _ := p.follow(t, c, fmt.Sprintf("%s/invite/nothing-like-a-real-code-%d", p.srv.URL, i))
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after %d guesses: %d, want 429", failMax+1, last)
	}
}

/* -------------------------------------------------------------- health --- */

func TestHealthIsOpenAndOutlivesThePage(t *testing.T) {
	h := newHarness(t, Options{Routes: &Routes{API: true, Health: true}})
	resp, err := http.Get(h.srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/health with no credential: %d", resp.StatusCode)
	}
	body := readAll(t, resp)
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["ok"] != true || len(doc) != 1 {
		t.Fatalf("health says %s; it must say one thing and no version", body)
	}
	// The page is off, so / is not there — which is the whole reason this
	// route exists.
	if resp, _ := h.do("GET", "/", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET / with the page off: %d", resp.StatusCode)
	}
	// And a server with health off has nothing open at all.
	off := newHarness(t, Options{Routes: &Routes{API: true, Playground: true, WebSocket: true}})
	if resp, _ := off.do("GET", "/v1/health", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("health off: %d", resp.StatusCode)
	}
}

/* ---------------------------------------------------------------- helpers --- */

func cookieNamed(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// readAll is one response body, which every helper here wants and nothing
// here wants to fail over.
func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// stealCookie is the session cookie a jar is holding, so a test can go on
// using it after the page has been told to forget it.
func (p *people) stealCookie(t *testing.T, c *http.Client) string {
	t.Helper()
	u, err := url.Parse(p.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == sessionCookie {
			return ck.Value
		}
	}
	t.Fatal("the jar holds no session cookie")
	return ""
}

func (p *people) withCookie(t *testing.T, value, method, path string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, p.srv.URL+path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
	req.Header.Set("Origin", p.srv.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp, readAll(t, resp)
}

// follow opens one URL as a browser would, without following the redirect.
func (p *people) follow(t *testing.T, c *http.Client, link string) (*http.Response, []byte) {
	t.Helper()
	resp, err := c.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp, readAll(t, resp)
}
