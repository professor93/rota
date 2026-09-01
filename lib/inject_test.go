package rota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// The HTTP client is the application's: proxies, TLS, instrumentation and
// timeouts are its decisions. The SDK ships a default and uses whatever the
// caller put in its place — which is also how these tests stub the network,
// through the same seam every consumer has.
func TestTheHTTPClientIsTheCallers(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	old := *HTTPClient
	HTTPClient.Timeout = time.Second
	t.Cleanup(func() { *HTTPClient = old })

	// A caller that names itself is taken at its word...
	var out map[string]any
	if err := getJSON(context.Background(), srv.URL, "", &out, map[string]string{"User-Agent": "myapp/9"}); err != nil {
		t.Fatal(err)
	}
	if got != "myapp/9" {
		t.Fatalf("a caller-set User-Agent must survive: %q", got)
	}
	// ...and one that says nothing gets the SDK's own name, not rota's.
	if err := getJSON(context.Background(), srv.URL, "", &out, nil); err != nil {
		t.Fatal(err)
	}
	if got != UserAgent || !strings.Contains(got, Version) {
		t.Fatalf("the default identifies this SDK: %q vs %q", got, UserAgent)
	}
}

// A failing status is machine-readable: the status and body ride a typed
// error, so a consumer tells 429 from 401 with errors.As instead of parsing
// "http 429: ..." prose.
func TestHTTPFailuresAreMachineReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	t.Cleanup(srv.Close)
	err := getJSON(context.Background(), srv.URL, "", nil, nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 429 || !strings.Contains(he.Body, "rate_limited") {
		t.Fatalf("want a typed 429 with its body, got %T: %v", err, err)
	}
}

// A terminal OAuth refusal keeps its RFC 6749 code. The prose stays for
// people; the code is for the application, which may localize, branch, or
// map the outcome onto its own vocabulary.
func TestOAuthRefusalsCarryTheirCode(t *testing.T) {
	for _, c := range []struct{ code, wantIn string }{
		{"access_denied", "denied"},
		{"expired_token", "expired"},
		{"some_new_code", "some_new_code"},
	} {
		r := &oauthTokenResp{Error: c.code}
		err := r.verdict(nil, grantCode)
		var oe *OAuthError
		if !errors.As(err, &oe) || oe.Code != c.code {
			t.Fatalf("%s: want a typed refusal carrying the code, got %T: %v", c.code, err, err)
		}
		if !strings.Contains(err.Error(), c.wantIn) {
			t.Fatalf("%s: the prose stays for people: %v", c.code, err)
		}
	}
	// The two verdicts applications already branch on stay sentinels.
	if err := (&oauthTokenResp{Error: "authorization_pending"}).verdict(nil, grantCode); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("pending stays a sentinel: %v", err)
	}
}

// The clock is injectable. Expiry, staleness and stamps all flow from Now,
// so an application (or a test) can hold time still.
func TestTheClockIsTheCallers(t *testing.T) {
	old := Now
	t.Cleanup(func() { Now = old })
	frozen := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	Now = func() time.Time { return frozen }

	a := &Account{Token: Token{Access: "t", ExpiresAt: frozen.UnixMilli() + ExpiryBuffer.Milliseconds() - 1}}
	if !a.Expired() {
		t.Fatal("inside the buffer is expired")
	}
	Now = func() time.Time { return frozen.Add(-time.Hour) }
	if a.Expired() {
		t.Fatal("an hour earlier it is not")
	}
}

// A token survives its own encoding whole. Delegated, Identity and Extra
// used to be json:"-", so an application persisting the *Token it was handed
// silently lost the very facts that make a delegated account work.
func TestTokenRoundTripsItsWholeSelf(t *testing.T) {
	in := &Token{Access: "", Delegated: true,
		Identity: &Identity{UUID: "u-1", Email: "e@x"},
		Extra:    map[string]string{"id_token": "jwt"}}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Token
	if err := UnmarshalLenient(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Delegated || out.Identity == nil || out.Identity.UUID != "u-1" || out.Extra["id_token"] != "jwt" {
		t.Fatalf("fields lost in the round trip: %+v", out)
	}
}

// What every provider drops is data a caller may read, not a global a caller
// can corrupt: the list comes back as a copy.
func TestNetworkRedirectingReturnsACopy(t *testing.T) {
	one := NetworkRedirecting()
	if !slices.Contains(one, "HTTPS_PROXY") {
		t.Fatalf("the known list: %v", one)
	}
	one[0] = "TAMPERED"
	if slices.Contains(NetworkRedirecting(), "TAMPERED") {
		t.Fatal("a caller's mutation must not reach the next caller")
	}
}

// A model catalog is data, not shared state: mutating what Models returns
// must not poison the next caller's view.
func TestModelCatalogsComeBackAsCopies(t *testing.T) {
	p, _ := Lookup("claude")
	c := p.(Catalog)
	first := c.Models()
	first[0].ID = "tampered"
	if c.Models()[0].ID == "tampered" {
		t.Fatal("a caller's mutation must not reach the next caller")
	}
}
