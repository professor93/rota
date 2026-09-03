package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// A flood of failing addresses must not lift the blocks already in place:
// the table evicts the stalest address, never everyone.
func TestTheLimiterKeepsBlocksWhenItIsFull(t *testing.T) {
	l := newLimiter()
	for range failMax {
		l.fail("attacker")
	}
	for i := range maxTracked + 10 {
		l.fail(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if !l.blocked("attacker") {
		t.Fatal("a flood of other addresses lifted the attacker's own block")
	}
	if len(l.hits) > maxTracked {
		t.Fatalf("the table grew past its cap: %d", len(l.hits))
	}
}

// One IPv6 host holds a whole /64, so that is the unit an address is
// counted by; IPv4 stays per address.
func TestTheLimiterCountsAnIPv6HostByItsPrefix(t *testing.T) {
	key := func(addr string) string { return clientIP(&http.Request{RemoteAddr: addr}) }
	if key("[2001:db8:1:2::1]:40000") != key("[2001:db8:1:2:ffff:ffff:ffff:ffff]:1") {
		t.Fatal("two addresses in one /64 must share a counter")
	}
	if key("[2001:db8:1:2::1]:1") == key("[2001:db8:1:3::1]:1") {
		t.Fatal("different /64s are different hosts")
	}
	if key("10.0.0.1:1") == key("10.0.0.2:1") {
		t.Fatal("IPv4 stays per address")
	}
}

type explodingProvider struct{}

func (explodingProvider) Name() string { return "t-api-explode" }
func (explodingProvider) Begin(_ context.Context) (string, map[string]string, error) {
	return "https://example.test/x", map[string]string{"kind": "apikey"}, nil
}
func (explodingProvider) Complete(_ context.Context, _ string, _ map[string]string) (*rota.Token, error) {
	return nil, errors.New("secret detail: proxy 10.1.2.3 refused")
}
func (explodingProvider) Launch(_ *rota.Account, _ string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}

// An error nobody classified is an internal error, said as only that. The
// text goes to the log, where the operator reads it, not to a caller who
// would learn the operator's network and file layout from it.
func TestUnclassifiedErrorsSayOnlyThatToTheCaller(t *testing.T) {
	rota.Register(explodingProvider{})
	h := newHarness(t, Options{})
	resp, raw := h.do("POST", "/v1/login", map[string]any{"provider": "t-api-explode"})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var l struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &l)
	resp, raw = h.do("POST", "/v1/login/"+l.ID, map[string]any{"code": "k"})
	if resp.StatusCode != 500 || strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), "internal error") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
}

// A login body that is broken is refused; only an absent one means the
// default provider.
func TestABrokenLoginBodyIsRefused(t *testing.T) {
	h := newHarness(t, Options{})
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/login", strings.NewReader(`{"provider":"codex"`))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("a truncated body must be refused, not read as empty: %d %s", resp.StatusCode, raw)
	}
	if resp, _ := h.do("POST", "/v1/login", nil); resp.StatusCode != 200 {
		t.Fatalf("an empty body still means the default provider: %d", resp.StatusCode)
	}
}

// The request of a multipart run is the "request" part and nothing else: a
// query string would land prompts in every access log on the way.
func TestAMultipartRunIgnoresTheQueryString(t *testing.T) {
	h := newHarness(t, Options{})
	var buf bytes.Buffer
	mw := newMultipart(&buf)
	mw.file("files", "c.md", "# c")
	ct := mw.close()
	q := url.Values{"request": {`{"prompt":"from the query"}`}}
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/accounts/1/run?"+q.Encode(), &buf)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", ct)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(raw), "prompt") {
		t.Fatalf("a prompt in the query string must not count: %d %s", resp.StatusCode, raw)
	}
}

// Replies are not for caches, and the page is not for frames.
func TestRepliesAreNotCachedAndThePageIsNotFramed(t *testing.T) {
	h := newHarness(t, Options{})
	if resp, _ := h.do("GET", "/v1/accounts", nil); resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control: %q", resp.Header.Get("Cache-Control"))
	}
	resp, err := http.Get(h.srv.URL + "/playground")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Frame-Options") != "DENY" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("frame headers: %q %q", resp.Header.Get("X-Frame-Options"), resp.Header.Get("Content-Security-Policy"))
	}
}
