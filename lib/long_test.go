package rota

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A long login differs from the ordinary one in exactly two places, and both
// are worth pinning: the scope it asks the browser for, and the life it asks
// the token endpoint for. Everything else — the client id, the redirect, the
// PKCE pair — has to stay the same, or the provider is looking at two
// different applications.
func TestClaudeLongLoginAsksForInferenceOnlyAndAYear(t *testing.T) {
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Token, f.URL+"/token")
	p, _ := Lookup("claude")
	lp, ok := p.(LongLived)
	if !ok {
		t.Fatal("claude must be able to issue a long-lived token")
	}

	authURL, state, err := lp.BeginLong(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	if q.Get("scope") != "user:inference" {
		t.Fatalf("a long login must ask for inference and nothing else, got %q", q.Get("scope"))
	}
	if q.Get("client_id") != claudeClientID || q.Get("redirect_uri") != claudeRedirectURI ||
		q.Get("code_challenge") == "" || q.Get("state") != state["state"] || state["verifier"] == "" {
		t.Fatalf("the rest of the login must be unchanged: %s", authURL)
	}
	if ordinary, _, _ := p.Begin(context.Background()); !strings.Contains(ordinary, "user%3Aprofile") {
		t.Fatalf("the ordinary login must still ask for the rest: %s", ordinary)
	}

	var asked []any
	f.reply["/token"] = func(_ *http.Request, b map[string]any) (int, any) {
		asked = append(asked, b["expires_in"])
		return 200, map[string]any{"access_token": "LONG", "refresh_token": "NOPE",
			"account": map[string]string{"uuid": "u1", "email_address": "e@x"}}
	}
	tok, err := lp.CompleteLong(context.Background(), "CODE#ST", state)
	if err != nil || tok.Access != "LONG" {
		t.Fatalf("complete long: %+v %v", tok, err)
	}
	// A JSON number, not the string "31536000": the provider reads it as a
	// number and a string is simply a different request.
	n, isNumber := asked[0].(float64)
	if !isNumber || int64(n) != claudeLongSeconds {
		t.Fatalf("expires_in must travel as the number %d, got %#v", claudeLongSeconds, asked[0])
	}
	if tok.Refresh != "" {
		t.Fatal("a long token's refresh is a lineage nothing rotates; it must not be kept")
	}
	if tok.Identity == nil || tok.Identity.UUID != "u1" {
		t.Fatalf("the exchange's identity is the only one a long token has: %+v", tok.Identity)
	}
	// No expiry in the reply: the life asked for is what was granted.
	want := nowMS() + claudeLongSeconds*1000
	if tok.ExpiresAt < want-60_000 || tok.ExpiresAt > want+60_000 {
		t.Fatalf("expiry must fall back to the year that was asked for: %d want ~%d", tok.ExpiresAt, want)
	}

	if _, err := p.Complete(context.Background(), "CODE#ST", state); err != nil {
		t.Fatal(err)
	}
	if asked[1] != nil {
		t.Fatalf("the ordinary exchange must ask for no particular life, got %#v", asked[1])
	}
}

// Every provider but claude has no such token to give, and says so by name
// rather than by starting a login that cannot end.
func TestLongLoginRefusedByProvidersThatHaveNone(t *testing.T) {
	for _, name := range []string{"codex", "grok"} {
		_, err := BeginLong(context.Background(), name)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	l := &Login{Provider: "codex", Long: true, State: map[string]string{}}
	if _, err := l.Complete(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("finishing a long login on such a provider must refuse too: %v", err)
	}
}

func TestClaudeLaunchPrefersALongLivedToken(t *testing.T) {
	p, _ := Lookup("claude")
	ms := func(d time.Duration) int64 { return time.Now().Add(d).UnixMilli() }
	token := func(a *Account) string {
		cmd, err := p.Launch(a, "")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimPrefix(cmd.Env[0], "CLAUDE_CODE_OAUTH_TOKEN=")
	}
	short := Token{Access: "SHORT", ExpiresAt: ms(time.Hour)}

	for _, c := range []struct {
		what string
		long *LongToken
		want string
	}{
		{"none", nil, "SHORT"},
		{"a year left", &LongToken{Access: "LONG", ExpiresAt: ms(365 * 24 * time.Hour)}, "LONG"},
		{"no expiry at all", &LongToken{Access: "LONG"}, "LONG"},
		{"two days left", &LongToken{Access: "LONG", ExpiresAt: ms(48 * time.Hour)}, "LONG"},
		// Within a day is already gone: what this token exists for is a
		// process that runs for hours.
		{"an hour left", &LongToken{Access: "LONG", ExpiresAt: ms(time.Hour)}, "SHORT"},
		{"expired", &LongToken{Access: "LONG", ExpiresAt: ms(-time.Hour)}, "SHORT"},
		{"stored empty", &LongToken{}, "SHORT"},
	} {
		if got := token(&Account{Token: short, Long: c.long}); got != c.want {
			t.Fatalf("%s: launched with %q, want %q", c.what, got, c.want)
		}
	}
}

// A dead lineage stops a launch, as it always did — unless the account holds
// a credential that lineage has nothing to do with.
func TestADeadAccountStillStagesWithALongLivedToken(t *testing.T) {
	a := &Account{ID: 4, Provider: "claude", Dead: true, DeadReason: "invalid_grant",
		Token: Token{Access: "SHORT"}}
	if _, err := Stage(a, ""); err == nil {
		t.Fatal("a dead account with nothing else must still be refused")
	}
	a.Long = &LongToken{Access: "LONG", ExpiresAt: time.Now().Add(300 * 24 * time.Hour).UnixMilli()}
	cmd, err := Stage(a, "")
	if err != nil || cmd.Env[0] != "CLAUDE_CODE_OAUTH_TOKEN=LONG" {
		t.Fatalf("stage: %+v %v", cmd, err)
	}
	if _, _, err := StagePlan(context.Background(), a, ""); err != nil {
		t.Fatalf("plan: %v", err)
	}
	a.Long.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	if _, err := Stage(a, ""); err == nil {
		t.Fatal("a long token about to expire is no reason to run a dead account")
	}
}

func TestLongTokenReadings(t *testing.T) {
	a := &Account{}
	if a.LongValid() || a.LongAccess() != "" || !a.LongUntil().IsZero() {
		t.Fatal("an account with no long token has nothing to say about one")
	}
	a.ApplyLong(&Token{Access: "LONG", Refresh: "R", ExpiresAt: 1_700_000_000_000,
		Identity: &Identity{UUID: "u1"}})
	if a.Long.Access != "LONG" || a.Long.ExpiresAt != 1_700_000_000_000 {
		t.Fatalf("applied: %+v", a.Long)
	}
	if a.Token.Access != "" || a.UUID != "" {
		t.Fatal("a long token must not touch the ordinary credential or the account's name")
	}
	if !a.LongUntil().Equal(time.UnixMilli(1_700_000_000_000)) {
		t.Fatalf("until: %v", a.LongUntil())
	}
}
