package rota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Made-up credentials in the shapes Claude Code itself tells apart: it reads
// the three characters after "sk-ant-" as "api" for a Console key and "oat"
// for an OAuth token. None of them is anybody's.
const (
	testKey      = "sk-ant-api03-TESTONLY-not-a-real-key-0000000000-WXYZ"
	testOtherKey = "sk-ant-api03-TESTONLY-another-made-up-key-1111-ABCD"
	testSetup    = "sk-ant-oat01-TESTONLY-not-a-real-setup-token-2222"
)

// noSecret fails when a credential appears in something a person or a log
// could read.
func noSecret(t *testing.T, where, s string, secrets ...string) {
	t.Helper()
	for _, x := range secrets {
		if strings.Contains(s, x) || strings.Contains(s, x[len(x)/2:]) {
			t.Fatalf("%s carries the credential: %q", where, s)
		}
	}
}

// A key is asked about once, at login, the way the API itself is called: the
// key in x-api-key and a version header, never as a bearer token.
func TestClaudeKeyLoginAsksTheAPIOnce(t *testing.T) {
	f := newFakeServer(t, false)
	setURL(t, &ClaudeModelsEndpoint, f.URL+"/v1/models")
	f.reply["/v1/models"] = func(r *http.Request, _ map[string]any) (int, any) {
		if k := r.Header.Get("x-api-key"); r.Method != "GET" || (k != testKey && k != testOtherKey) ||
			r.Header.Get("anthropic-version") == "" || r.Header.Get("Authorization") != "" {
			t.Errorf("the key must travel as x-api-key with a version and no bearer (%s)", r.Method)
		}
		return 200, map[string]any{"data": []any{}}
	}
	tok, unverified, err := LoginKey(context.Background(), "claude", "  "+testKey+"\n")
	if err != nil || unverified != nil {
		t.Fatalf("a good key must be taken: %v %v", err, unverified)
	}
	if tok.Access != testKey || tok.Credential != CredentialAPIKey || tok.Refresh != "" || tok.ExpiresAt != 0 {
		t.Fatalf("the token is the key itself, which never expires: %+v", tok)
	}
	if f.count("/v1/models") != 1 {
		t.Fatalf("one question, got %d", f.count("/v1/models"))
	}
	again, _, _ := LoginKey(context.Background(), "claude", testKey)
	other, _, _ := LoginKey(context.Background(), "claude", testOtherKey)
	if tok.Identity == nil || !strings.HasPrefix(tok.Identity.UUID, "apikey-") || tok.Identity.Email != "" {
		t.Fatalf("a key's identity is a fingerprint of it, and it has no e-mail: %+v", tok.Identity)
	}
	if again.Identity.UUID != tok.Identity.UUID || other.Identity.UUID == tok.Identity.UUID {
		t.Fatalf("the same key is the same account and another is another: %s %s %s",
			tok.Identity.UUID, again.Identity.UUID, other.Identity.UUID)
	}
	noSecret(t, "the uuid", tok.Identity.UUID, testKey)
}

// Only the API's own refusal refuses a key. Anything else — a server error,
// a timeout, no network — takes it, and says it was not checked.
func TestClaudeKeyIsRefusedOnlyBy401And403(t *testing.T) {
	for _, status := range []int{401, 403} {
		f := newFakeServer(t, false)
		setURL(t, &ClaudeModelsEndpoint, f.URL+"/v1/models")
		f.reply["/v1/models"] = func(*http.Request, map[string]any) (int, any) {
			return status, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key ` + testKey + `"}}`
		}
		tok, _, err := LoginKey(context.Background(), "claude", testKey)
		if tok != nil || !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%d must refuse the key: %+v %v", status, tok, err)
		}
		if !strings.Contains(err.Error(), "refused") {
			t.Fatalf("the refusal must say so: %v", err)
		}
		noSecret(t, "the refusal", err.Error(), testKey)
	}

	// The slow handler outlives the check that gave up on it, on a goroutine
	// of its own; it is counted atomically and waited for before the test
	// moves on, so nothing it touches is touched by the next case too.
	var slow atomic.Int64
	slowDone := make(chan struct{}, 4)
	for _, c := range []struct {
		name  string
		reply func(*http.Request, map[string]any) (int, any)
	}{
		{"500", func(*http.Request, map[string]any) (int, any) { return 500, testKey }},
		{"429", func(*http.Request, map[string]any) (int, any) { return 429, nil }},
		{"slow", func(*http.Request, map[string]any) (int, any) {
			defer func() { slowDone <- struct{}{} }()
			slow.Add(1)
			time.Sleep(300 * time.Millisecond)
			return 200, nil
		}},
	} {
		f := newFakeServer(t, false)
		setURL(t, &ClaudeModelsEndpoint, f.URL+"/v1/models")
		f.reply["/v1/models"] = c.reply
		// The caller's deadline bounds the check, as much as lib's own does.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		tok, unverified, err := LoginKey(ctx, "claude", testKey)
		cancel()
		if err != nil || tok == nil || unverified == nil {
			t.Fatalf("%s must take the key unchecked: %+v %v %v", c.name, tok, unverified, err)
		}
		noSecret(t, c.name+"'s warning", unverified.Error(), testKey)
		if c.name == "slow" {
			<-slowDone
			if n := slow.Load(); n != 1 {
				t.Fatalf("the slow check was asked %d times", n)
			}
		}
	}
	// Nothing answers at all.
	setURL(t, &ClaudeModelsEndpoint, "http://127.0.0.1:1/unreachable")
	tok, unverified, err := LoginKey(context.Background(), "claude", testKey)
	if err != nil || tok == nil || unverified == nil {
		t.Fatalf("an unreachable API must take the key unchecked: %v %v", unverified, err)
	}
}

// A redirect is not followed: a header carrying the key would go along to
// wherever it points.
func TestClaudeKeyCheckFollowsNoRedirect(t *testing.T) {
	elsewhere := 0
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		w.WriteHeader(200)
	}))
	t.Cleanup(away.Close)
	here := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, away.URL+"/steal", http.StatusFound)
	}))
	t.Cleanup(here.Close)
	setURL(t, &ClaudeModelsEndpoint, here.URL+"/v1/models")
	_, unverified, err := LoginKey(context.Background(), "claude", testKey)
	if err != nil || unverified == nil {
		t.Fatalf("a redirect is an answer that checks nothing: %v %v", unverified, err)
	}
	if elsewhere != 0 {
		t.Fatal("the key followed a redirect to another server")
	}
}

// The prefix says which credential was pasted, and the wrong one is refused
// with a sentence that names both — never by echoing what was pasted.
func TestClaudeTellsAKeyFromASetupToken(t *testing.T) {
	cases := []struct {
		key  bool
		in   string
		want string
	}{
		{true, testSetup, "setup-token"},
		{true, "sk-proj-TESTONLY", "sk-ant-api"},
		{true, "", "nothing"},
		{true, "sk-ant-api03-TESTONLY with a space", "letters"},
		{true, "sk-ant-api03-TESTONLY\nX=1", "letters"},
		{false, testKey, "API key"},
		{false, "nonsense-TESTONLY", "sk-ant-oat"},
		{false, "", "nothing"},
	}
	for _, c := range cases {
		var err error
		if c.key {
			_, _, err = LoginKey(context.Background(), "claude", c.in)
		} else {
			_, err = PasteLong("claude", c.in)
		}
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%q (key=%v): want a refusal naming %q, got %v", c.in, c.key, c.want, err)
		}
		if c.in != "" {
			noSecret(t, "the refusal", err.Error(), c.in)
		}
	}
	if _, _, err := LoginKey(context.Background(), "codex", testKey); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a provider that takes no pasted key says so: %v", err)
	}
	if _, err := PasteLong("grok", testSetup); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a provider that takes no pasted token says so: %v", err)
	}
}

// A setup token is the long-lived credential `rota login --long` would have
// obtained: a year from now, no refresh, no identity of its own.
func TestClaudePastedSetupTokenIsALongToken(t *testing.T) {
	tok, err := PasteLong("claude", " "+testSetup+"\n")
	if err != nil {
		t.Fatal(err)
	}
	want := nowMS() + claudeLongSeconds*1000
	if tok.Access != testSetup || tok.Refresh != "" || tok.Identity != nil || tok.ExpiresAt < want-60_000 || tok.ExpiresAt > want+60_000 {
		t.Fatalf("a pasted setup token is a year-long credential and nothing else: %+v", tok)
	}
	a := &Account{ID: 1, Provider: "claude", Email: "a@b.c"}
	a.ApplyLong(tok)
	if a.LongAccess() != testSetup {
		t.Fatal("it is stored where a long login stores its token")
	}
}

func keyAccount(t *testing.T) *Account {
	t.Helper()
	tok := &Token{Access: testKey, Credential: CredentialAPIKey, Identity: &Identity{UUID: "apikey-" + fingerprint(testKey)}}
	return NewAccount(7, "claude", tok)
}

// What an account made by a key is: unmetered, named by its last four
// characters unless named, and never asked for usage.
func TestAKeyAccountIsUnmeteredAndNamedByItsEnd(t *testing.T) {
	a := keyAccount(t)
	if !a.APIKey() || a.Credential != CredentialAPIKey {
		t.Fatalf("the account must know it runs on a key: %+v", a)
	}
	if a.Label() != "api key ...WXYZ" || a.String() != "claude/api key ...WXYZ" {
		t.Fatalf("label: %q", a.Label())
	}
	a.Name = "work credit"
	if a.Label() != "work credit" {
		t.Fatalf("a name wins: %q", a.Label())
	}
	if a.Metered() || !Metered("claude") {
		t.Fatal("a key account has no usage to read; the provider still does")
	}
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Usage, f.URL+"/usage")
	q, err := Usage(context.Background(), a)
	if q != nil || err != nil || f.count("/usage") != 0 {
		t.Fatalf("the OAuth usage endpoint is never asked about a key: %+v %v %d", q, err, f.count("/usage"))
	}
	if a.Expired() {
		t.Fatal("a key does not expire")
	}
	if changed, err := Refresh(context.Background(), a); changed || err != nil || a.Dead {
		t.Fatalf("a key is never refreshed and never dies of it: %v %v %v", changed, err, a.Dead)
	}
	// An ordinary account is untouched by all of this.
	o := &Account{ID: 1, Provider: "claude", Email: "a@b.c"}
	if o.APIKey() || !o.Metered() || o.Label() != "a@b.c" {
		t.Fatalf("an ordinary account: %v %v %q", o.APIKey(), o.Metered(), o.Label())
	}
}

// A key account launches with the key and nothing else that could
// authenticate: every competing credential is dropped, a long-lived token it
// somehow holds included, and no login is written anywhere.
func TestAKeyAccountLaunchesOnTheKeyAlone(t *testing.T) {
	a := keyAccount(t)
	a.Long = &LongToken{Access: testSetup, ExpiresAt: nowMS() + 1e10}
	home := t.TempDir()
	for name, stage := range map[string]func() (*Command, error){
		"Stage":         func() (*Command, error) { return Stage(a, home) },
		"Stage no home": func() (*Command, error) { return Stage(a, "") },
		"StagePlan": func() (*Command, error) {
			cmd, files, err := StagePlan(context.Background(), a, home)
			if len(files) != 0 {
				t.Fatalf("a key account plans no credential file: %d", len(files))
			}
			return cmd, err
		},
	} {
		cmd, err := stage()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		env := Environ([]string{
			"CLAUDE_CODE_OAUTH_TOKEN=inherited", "ANTHROPIC_API_KEY=inherited", "ANTHROPIC_AUTH_TOKEN=inherited",
			"CLAUDE_CODE_USE_BEDROCK=1", "ANTHROPIC_BASE_URL=https://elsewhere.test", "PATH=/bin",
		}, cmd)
		var creds []string
		for _, e := range env {
			k, _, _ := strings.Cut(e, "=")
			if k == "CLAUDE_CODE_OAUTH_TOKEN" || k == "ANTHROPIC_AUTH_TOKEN" || k == "CLAUDE_CODE_USE_BEDROCK" ||
				k == "ANTHROPIC_BASE_URL" || k == "ANTHROPIC_API_KEY" {
				creds = append(creds, e)
			}
		}
		if !slices.Equal(creds, []string{"ANTHROPIC_API_KEY=" + testKey}) {
			t.Fatalf("%s: the key must be the only credential the child sees: %v", name, creds)
		}
		if slices.Contains(cmd.Drop, "ANTHROPIC_API_KEY") {
			t.Fatalf("%s: the variable the command sets is not one it drops: %v", name, cmd.Drop)
		}
		for _, v := range ClaudeCredentialVars() {
			if v != "ANTHROPIC_API_KEY" && !slices.Contains(cmd.Drop, v) {
				t.Fatalf("%s: %s must be dropped", name, v)
			}
		}
	}
	if StoresLogin(a, home) || JoinHome(a, home) != nil {
		t.Fatal("a key account keeps no login in its home and joins none")
	}
	// A hermetic run gives the command a throwaway home and checks it still
	// carries a credential: one key, not two.
	cmd, _ := Stage(a, "")
	got := withToken(a, cmd)
	n := 0
	for _, e := range got.Env {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
			n++
		}
		if strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Fatalf("a hermetic key run must not be handed a token: %v", got.Env)
		}
	}
	if n != 1 {
		t.Fatalf("one key, got %d", n)
	}
}

// Remote Control refuses an API key, so the setting is refused by name
// rather than stored to fail later.
func TestAKeyAccountCannotHaveRemoteControl(t *testing.T) {
	a := keyAccount(t)
	a.RemoteControl = true
	err := a.CheckProject()
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("want a refusal naming the API key, got %v", err)
	}
}
