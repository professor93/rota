package rota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeServer answers by path; each handler receives the parsed form/JSON body.
type fakeServer struct {
	*httptest.Server
	t     *testing.T
	calls map[string]int
	reply map[string]func(r *http.Request, body map[string]any) (int, any)
}

func newFakeServer(t *testing.T, tls bool) *fakeServer {
	t.Helper()
	f := &fakeServer{t: t, calls: map[string]int{}, reply: map[string]func(*http.Request, map[string]any) (int, any){}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls[r.URL.Path]++
		h := f.reply[r.URL.Path]
		if h == nil {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		body := map[string]any{}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			json.NewDecoder(r.Body).Decode(&body)
		} else if r.Method == "POST" {
			r.ParseForm()
			for k := range r.Form {
				body[k] = r.Form.Get(k)
			}
		}
		status, out := h(r, body)
		w.WriteHeader(status)
		if s, ok := out.(string); ok {
			w.Write([]byte(s))
		} else if out != nil {
			json.NewEncoder(w).Encode(out)
		}
	})
	if tls {
		f.Server = httptest.NewTLSServer(handler)
		old := HTTPClient
		HTTPClient = f.Client()
		t.Cleanup(func() { HTTPClient = old })
	} else {
		f.Server = httptest.NewServer(handler)
	}
	t.Cleanup(f.Close)
	return f
}

func setURL(t *testing.T, v *string, to string) {
	t.Helper()
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

func TestOAuthVerdicts(t *testing.T) {
	he := &HTTPError{Status: 400, Body: `{"error":"invalid_grant"}`}
	cases := []struct {
		r    oauthTokenResp
		err  error
		g    grant
		want error // nil = success; errAny = some other error
	}{
		{oauthTokenResp{AccessToken: "a"}, nil, grantCode, nil},
		{oauthTokenResp{Error: "invalid_grant"}, he, grantRefresh, ErrDeadToken},
		{oauthTokenResp{Error: "refresh_token_reused"}, he, grantRefresh, ErrDeadToken},
		{oauthTokenResp{}, he, grantRefresh, ErrDeadToken}, // verdict only in the raw body
		{oauthTokenResp{Error: "invalid_grant"}, he, grantCode, errAny},
		{oauthTokenResp{}, nil, grantCode, errAny}, // 200 without a token
		{oauthTokenResp{Error: "server_error"}, nil, grantRefresh, errAny},
		{oauthTokenResp{}, &HTTPError{Status: 502, Body: "<html>"}, grantRefresh, errAny},
	}
	for i, c := range cases {
		got := c.r.verdict(c.err, c.g)
		switch {
		case c.want == nil && got != nil, c.want == errAny && (got == nil || errors.Is(got, ErrDeadToken) || errors.Is(got, ErrAuthPending)),
			c.want != nil && c.want != errAny && !errors.Is(got, c.want):
			t.Fatalf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

var errAny = errors.New("any other error")

func TestClaudeFlow(t *testing.T) {
	f := newFakeServer(t, false)
	setURL(t, &ClaudeEndpoints.Token, f.URL+"/token")
	setURL(t, &ClaudeEndpoints.Profile, f.URL+"/profile")
	setURL(t, &ClaudeEndpoints.Usage, f.URL+"/usage")
	p, _ := Lookup("claude")

	authURL, state, err := p.Begin(context.Background())
	u, _ := url.Parse(authURL)
	q := u.Query()
	if err != nil || q.Get("code_challenge") == "" || q.Get("state") != state["state"] || state["verifier"] == "" || q.Get("client_id") != claudeClientID {
		t.Fatalf("begin: %v %v", authURL, err)
	}
	f.reply["/token"] = func(r *http.Request, b map[string]any) (int, any) {
		switch b["grant_type"] {
		case "authorization_code":
			if b["code"] != "CODE" || b["state"] != "ST" || b["code_verifier"] != state["verifier"] {
				return 400, map[string]string{"error": "invalid_grant"}
			}
			return 200, map[string]any{"access_token": "A1", "refresh_token": "R1", "expires_in": 3600, "scope": "a b",
				"account": map[string]string{"uuid": "u1", "email_address": "e@x"}, "organization": map[string]string{"uuid": "o1"}}
		case "refresh_token":
			if b["refresh_token"] == "R1" {
				return 200, map[string]any{"access_token": "A2", "expires_in": 60}
			}
			return 400, map[string]any{"error": map[string]string{"type": "invalid_grant", "message": "gone"}}
		}
		return 500, nil
	}
	tok, err := p.Complete(context.Background(), "CODE#ST", state)
	if err != nil || tok.Access != "A1" || tok.Refresh != "R1" || tok.Identity.UUID != "u1" || tok.Identity.Email != "e@x" ||
		tok.Identity.Org != "o1" || tok.ExpiresAt < nowMS()+3_000_000 || len(tok.Scopes) != 2 {
		t.Fatalf("complete: %+v %v", tok, err)
	}
	if _, err := p.Complete(context.Background(), "WRONG", state); err == nil || errors.Is(err, ErrDeadToken) {
		t.Fatalf("rejected code must be a plain error: %v", err)
	}
	a := &Account{Token: Token{Refresh: "R1"}}
	if tok, err := p.(Refresher).Refresh(context.Background(), a); err != nil || tok.Access != "A2" {
		t.Fatalf("refresh: %+v %v", tok, err)
	}
	a.Token.Refresh = "dead"
	if _, err := p.(Refresher).Refresh(context.Background(), a); !errors.Is(err, ErrDeadToken) {
		t.Fatalf("object-shaped invalid_grant must still read as dead: %v", err)
	}

	f.reply["/profile"] = func(r *http.Request, _ map[string]any) (int, any) {
		if r.Header.Get("Authorization") != "Bearer A1" {
			return 401, nil
		}
		return 200, map[string]any{"account": map[string]string{"uuid": "u1", "email": "e@x"}, "organization": map[string]string{"uuid": "o1"}}
	}
	if id, err := p.(Identifier).Identify(context.Background(), "A1"); err != nil || id.UUID != "u1" || id.Email != "e@x" || id.Org != "o1" {
		t.Fatalf("identify: %+v %v", id, err)
	}
	f.reply["/usage"] = func(r *http.Request, _ map[string]any) (int, any) {
		if r.Header.Get("anthropic-beta") != claudeBeta {
			return 400, nil
		}
		return 200, `{"five_hour":{"utilization":12.5,"resets_at":"2099-01-01T00:00:00Z"},
		  "seven_day":{"utilization":40,"resets_at":"not a date"},
		  "limits":[{"kind":"weekly","percent":91,"resets_at":"2099-01-02T00:00:00+00:00","scope":{"model":{"display_name":"Fable"},"surface":{"weird":[1]}}},
		            {"kind":"weekly","percent":1,"scope":null}],
		  "extra_usage":{"is_enabled":true,"used_credits":1250,"monthly_limit":10000,"currency":"USD"}}`
	}
	q2, err := p.(Meter).Quota(context.Background(), "A1")
	if err != nil || len(q2.Windows) != 3 {
		t.Fatalf("quota: %+v %v", q2, err)
	}
	w := q2.Windows
	if w[0].Name != "5h" || w[0].Percent != 12.5 || !w[0].Primary || w[0].ResetsAt.IsZero() ||
		w[1].Name != "7d" || !w[1].ResetsAt.IsZero() || w[2].Name != "Fable" || !w[2].Scoped || w[2].Percent != 91 ||
		q2.Note != "extra usage 12.50 / 100.00 USD" ||
		q2.Extra == nil || q2.Extra.Used != 12.50 || q2.Extra.Limit != 100 || q2.Extra.Currency != "USD" {
		t.Fatalf("windows: %+v note=%q", w, q2.Note)
	}
	cmd, err := p.Launch(&Account{Token: Token{Access: "A1"}}, "")
	if err != nil || cmd.Bin != "claude" || cmd.Env[0] != "CLAUDE_CODE_OAUTH_TOKEN=A1" ||
		!strings.Contains(strings.Join(cmd.Drop, ","), "ANTHROPIC_BASE_URL") || !strings.Contains(strings.Join(cmd.Drop, ","), "ANTHROPIC_API_KEY") {
		t.Fatalf("launch: %+v %v", cmd, err)
	}
}

func TestCodexFlowAndStaging(t *testing.T) {
	f := newFakeServer(t, false)
	setURL(t, &CodexEndpoints.Token, f.URL+"/token")
	p, _ := Lookup("codex")
	idTok := jwtWith(t, `{"email":"c@x","sub":"s1","https://api.openai.com/auth":{"chatgpt_account_id":"acct"}}`, false)
	access := jwtWith(t, `{"exp":4102444800}`, false)
	_, state, _ := p.Begin(context.Background())
	f.reply["/token"] = func(r *http.Request, b map[string]any) (int, any) {
		switch {
		case b["grant_type"] == "authorization_code" && b["code"] == "C1" && b["code_verifier"] == state["verifier"] && b["redirect_uri"] == codexRedirectURI:
			return 200, map[string]any{"access_token": access, "refresh_token": "R1", "id_token": idTok, "expires_in": 864000}
		case b["grant_type"] == "authorization_code":
			return 400, map[string]string{"error": "invalid_grant", "error_description": "bad code"}
		case b["grant_type"] == "refresh_token" && b["refresh_token"] == "R1" && b["scope"] == codexRefreshScope:
			return 200, map[string]any{"access_token": access, "refresh_token": "R2", "id_token": idTok, "expires_in": 864000}
		}
		return 400, map[string]string{"error": "refresh_token_reused"}
	}
	if extractCode("http://localhost:1455/auth/callback?code=C1&state=x") != "C1" || extractCode(" C1 ") != "C1" || extractCode("http://x/?nocode=1") != "http://x/?nocode=1" {
		t.Fatal("extractCode")
	}
	tok, err := p.Complete(context.Background(), "http://localhost:1455/auth/callback?code=C1", state)
	if err != nil || tok.Refresh != "R1" || tok.Identity.Email != "c@x" || tok.Extra["id_token"] != idTok || tok.Extra["account_id"] != "acct" {
		t.Fatalf("complete: %+v %v", tok, err)
	}
	if _, err := p.Complete(context.Background(), "nope", state); err == nil || errors.Is(err, ErrDeadToken) || !strings.Contains(err.Error(), "bad code") {
		t.Fatalf("exchange rejection: %v", err)
	}
	a := &Account{ID: 3, Provider: "codex", Token: Token{Access: "old", Refresh: "R1"}}
	if tok, err := p.(Refresher).Refresh(context.Background(), a); err != nil || tok.Refresh != "R2" {
		t.Fatalf("refresh: %+v %v", tok, err)
	}
	a.Token.Refresh = "spent"
	if _, err := p.(Refresher).Refresh(context.Background(), a); !errors.Is(err, ErrDeadToken) {
		t.Fatalf("reused refresh must be dead: %v", err)
	}

	// Launch: account with no id_token repairs itself with one refresh, then stages auth.json.
	home := filepath.Join(t.TempDir(), "codex-3")
	a = &Account{ID: 3, Provider: "codex", Token: Token{Access: "old", Refresh: "R1"}, Staged: stagedNone}
	cmd, err := p.Launch(a, home)
	if err != nil || cmd.Bin != "codex" || cmd.Env[0] != "CODEX_HOME="+home || a.Token.Refresh != "R2" || a.Extra["id_token"] != idTok {
		t.Fatalf("launch: %+v %v a=%+v", cmd, err, a)
	}
	var doc struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			IDToken, AccessToken, RefreshToken, AccountID string `json:"-"`
			ID                                            string `json:"id_token"`
			Access                                        string `json:"access_token"`
			Refresh                                       string `json:"refresh_token"`
			Account                                       string `json:"account_id"`
		} `json:"tokens"`
	}
	if !readJSON(filepath.Join(home, "auth.json"), &doc) || doc.AuthMode != "chatgpt" || doc.Tokens.Refresh != "R2" || doc.Tokens.Account != "acct" || doc.Tokens.ID != idTok {
		t.Fatalf("auth.json: %+v", doc)
	}
	if a.Staged != fingerprint("R2") {
		t.Fatal("staged marker not recorded")
	}

	// The CLI rotates the file: next launch adopts it. rota's own later refresh must not be undone by the file.
	rotated := map[string]any{"tokens": map[string]string{"refresh_token": "R3", "access_token": access, "id_token": idTok, "account_id": "acct"}}
	stageDoc(&Account{}, filepath.Join(home, "auth.json"), rotated)
	f.reply["/token"] = func(r *http.Request, b map[string]any) (int, any) { t.Error("no refresh expected"); return 500, nil }
	if _, err := p.Launch(a, home); err != nil || a.Token.Refresh != "R3" || a.Token.ExpiresAt != 4102444800000 {
		t.Fatalf("adopt rotation: %v a=%+v", err, a.Token)
	}
	a.Token.Refresh = "R4" // rota refreshed since staging R3
	if _, err := p.Launch(a, home); err != nil || a.Token.Refresh != "R4" {
		t.Fatalf("store must win over the file rota itself wrote: %v %+v", err, a.Token)
	}
	foreign := map[string]any{"tokens": map[string]string{"refresh_token": "RX", "access_token": access, "id_token": idTok, "account_id": "someone-else"}}
	stageDoc(&Account{}, filepath.Join(home, "auth.json"), foreign)
	if _, err := p.Launch(a, home); err != nil || a.Token.Refresh != "R4" {
		t.Fatalf("a file for another identity must never be adopted: %v %+v", err, a.Token)
	}
}

func TestKimiIsDelegatedToItsOwnCLI(t *testing.T) {
	p, _ := Lookup("kimi")
	url, state, err := p.Begin(context.Background())
	if err != nil || url == "" {
		t.Fatalf("begin: %q %v", url, err)
	}
	tok, err := p.Complete(context.Background(), "", state)
	if err != nil || !tok.Delegated || tok.Access != "" {
		t.Fatalf("kimi holds its own credential: %+v %v", tok, err)
	}
	if tok.Identity == nil || tok.Identity.UUID == "" {
		t.Fatal("each account still needs an identity of its own")
	}

	a := &Account{ID: 4, Provider: "kimi"}
	a.apply(tok)
	home := t.TempDir()

	// Nothing to *ask* until the CLI has signed itself in there; opening
	// its session is how that happens, and stays allowed.
	if err := p.(SignInChecker).SignedIn(a, home); !errors.Is(err, ErrReauth) {
		t.Fatalf("an unsigned account must not be asked a question: %v", err)
	}
	if _, err := p.Launch(a, ""); err == nil {
		t.Fatal("kimi needs a private home")
	}

	// The command that signs it in points at that home.
	d, ok := p.(Delegator)
	if !ok {
		t.Fatal("kimi delegates")
	}
	plan := d.LoginPlan(a, home)
	if plan.Bin != "kimi" || !slices.Contains(plan.Args, "login") ||
		!slices.Contains(plan.Env, "KIMI_CODE_HOME="+home) {
		t.Fatalf("login plan: %+v", plan)
	}

	// A login that stored the token but not the model entries is told
	// apart from one that never happened.
	if err := os.MkdirAll(filepath.Join(home, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "credentials", "kimi-code.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = p.(SignInChecker).SignedIn(a, home)
	if !errors.Is(err, ErrReauth) || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("a half-finished login must say so: %v", err)
	}

	// Once it has, rota gets out of the way.
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("default_model = \"k2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, err := p.Launch(a, home)
	if err != nil || cmd.Bin != "kimi" {
		t.Fatalf("%+v %v", cmd, err)
	}
	if !slices.Contains(cmd.Env, "KIMI_CODE_HOME="+home) {
		t.Fatalf("env: %v", cmd.Env)
	}
	if slices.ContainsFunc(cmd.Env, func(e string) bool { return strings.HasPrefix(e, "KIMI_API_KEY=") }) {
		t.Fatalf("rota holds no key to give: %v", cmd.Env)
	}

	// An account from the former device flow cannot work, and says why.
	old := &Account{ID: 4, Provider: "kimi", Token: Token{Access: "old", Refresh: "r"}}
	if _, err := p.Launch(old, home); !errors.Is(err, ErrReauth) || !strings.Contains(err.Error(), "register it again") {
		t.Fatalf("an account rota can no longer use must say what to do: %v", err)
	}
}
func TestGrokTakesAnAPIKeyAndKeepsKeysApart(t *testing.T) {
	p, _ := Lookup("grok")
	u, state, err := p.Begin(context.Background())
	if err != nil || !strings.Contains(u, "console.x.ai") || state["kind"] != "apikey" {
		t.Fatalf("begin: %q %v %v", u, state, err)
	}
	// Nothing pasted is not an error here: it asks for a delegated login,
	// which TestGrokDelegatedLoginLetsTheCLIKeepTheCredential covers.
	if tok, err := p.Complete(context.Background(), "  ", state); err != nil || !tok.Delegated {
		t.Fatalf("an empty key means delegation: %+v %v", tok, err)
	}
	// A pasted OAuth token or a key from elsewhere is a mistake worth
	// catching before a run spends a slot on it.
	if _, err := p.Complete(context.Background(), "sk-not-an-xai-key", state); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a key of the wrong shape must be refused: %v", err)
	}
	a, err := p.Complete(context.Background(), "  xai-one  ", state)
	if err != nil || a.Access != "xai-one" || a.ExpiresAt != 0 {
		t.Fatalf("a key is taken as given and never expires: %+v %v", a, err)
	}
	b, _ := p.Complete(context.Background(), "xai-two", state)
	again, _ := p.Complete(context.Background(), "xai-one", state)
	if a.Identity.UUID == b.Identity.UUID || a.Identity.UUID != again.Identity.UUID ||
		strings.Contains(a.Identity.UUID, "xai-one") {
		t.Fatalf("keys must be told apart without being stored twice: %q %q", a.Identity.UUID, b.Identity.UUID)
	}
	if _, err := p.Launch(&Account{Provider: "grok"}, ""); err == nil {
		t.Fatal("grok must refuse to run without a private home")
	}
}

func TestGrokExplainsAnOldDeviceFlowAccount(t *testing.T) {
	p, _ := Lookup("grok")
	// What rota's former OAuth device flow left behind: a JWT, not a key.
	a := &Account{ID: 5, Provider: "grok", Email: "you@example.com",
		Token: Token{Access: "eyJ0eXAiOiJKV1QifQ.e30.sig", Refresh: "r"}}
	_, err := p.Launch(a, t.TempDir())
	if !errors.Is(err, ErrReauth) {
		t.Fatalf("an unusable credential must ask for re-auth, not run: %v", err)
	}
	for _, want := range []string{"console.x.ai", "delegated login"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the message must name both fixes, missing %q: %v", want, err)
		}
	}
	a.Token.Access = "xai-real"
	if _, err := p.Launch(a, t.TempDir()); err != nil {
		t.Fatalf("a real key runs: %v", err)
	}
}

func TestGrokDelegatedLoginLetsTheCLIKeepTheCredential(t *testing.T) {
	p, _ := Lookup("grok")
	_, state, _ := p.Begin(context.Background())

	// Finishing with nothing to paste means: rota holds no credential, grok
	// signs itself in inside the private home rota gives it.
	tok, err := p.Complete(context.Background(), "", state)
	if err != nil || !tok.Delegated || tok.Access != "" {
		t.Fatalf("an empty finish must delegate: %+v %v", tok, err)
	}
	if tok.Identity == nil || tok.Identity.UUID == "" {
		t.Fatalf("a delegated account still needs an identity of its own: %+v", tok.Identity)
	}

	a := &Account{ID: 6, Provider: "grok"}
	a.apply(tok)
	if !a.Delegated {
		t.Fatal("delegation must survive onto the account")
	}
	home := t.TempDir()
	// Pretend the CLI has already signed itself in here; the refusal before
	// that is TestDelegatedGrokRefusesUntilItIsSignedIn's business.
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, err := p.Launch(a, home)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(cmd.Env, " ")
	if !strings.Contains(env, "GROK_HOME="+home) {
		t.Fatalf("the private home is the whole point: %v", cmd.Env)
	}
	if strings.Contains(env, "XAI_API_KEY=") {
		t.Fatalf("rota has no key to give, and must not send an empty one: %v", cmd.Env)
	}
	if !containsString(cmd.Drop, "XAI_API_KEY") {
		t.Fatalf("a stray key from the shell must not outrank grok's own session: %v", cmd.Drop)
	}

	// rota must be able to say exactly how to sign that account in.
	d, ok := p.(Delegator)
	if !ok {
		t.Fatal("grok must advertise that it can delegate")
	}
	plan := d.LoginPlan(a, home)
	if plan.Bin != "grok" || !slices.Contains(plan.Args, "login") || !slices.Contains(plan.Args, "--device-code") {
		t.Fatalf("the login must run the CLI's own device flow: %+v", plan)
	}
	if !slices.Contains(plan.Env, "GROK_HOME="+home) {
		t.Fatalf("and in this account's own home: %v", plan.Env)
	}

	// A pasted key still works, and is not delegated.
	keyed, err := p.Complete(context.Background(), "xai-real", state)
	if err != nil || keyed.Delegated || keyed.Access != "xai-real" {
		t.Fatalf("%+v %v", keyed, err)
	}
	// Two delegated accounts must not collapse into one.
	other, _ := p.Complete(context.Background(), "", state)
	if other.Identity.UUID == tok.Identity.UUID {
		t.Fatal("each delegated login is its own account")
	}
}

func TestDelegatedAccountsSurviveTheStoreAndNeverRefresh(t *testing.T) {
	a := &Account{ID: 6, Provider: "grok", Delegated: true}
	if a.Expired() {
		t.Fatal("rota holds no token for a delegated account, so nothing expires")
	}
	if changed, err := Refresh(context.Background(), a); changed || err != nil {
		t.Fatalf("there is nothing for rota to refresh: %v %v", changed, err)
	}
	if a.Status() != StatusOK {
		t.Fatalf("status: %s", a.Status())
	}
	raw, err := json.Marshal(a)
	if err != nil || !strings.Contains(string(raw), `"delegated":true`) {
		t.Fatalf("delegation must be written down: %s %v", raw, err)
	}
}

func TestDelegatedGrokRefusesUntilItIsSignedIn(t *testing.T) {
	p, _ := Lookup("grok")
	a := &Account{ID: 6, Provider: "grok", Delegated: true, Email: "you@example.com"}
	home := t.TempDir()

	err := p.(SignInChecker).SignedIn(a, home)
	if !errors.Is(err, ErrReauth) {
		t.Fatalf("an unsigned delegated account must not be asked a question: %v", err)
	}
	// The message states the condition in SDK terms; which command fixes it
	// is the application's sentence to write, not the SDK's.
	for _, want := range []string{"signed in", "delegated"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in: %v", want, err)
		}
	}
	// Once the CLI has signed itself in, rota gets out of the way.
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.(SignInChecker).SignedIn(a, home); err != nil {
		t.Fatalf("signed in: %v", err)
	}
	cmd, err := p.Launch(a, home)
	if err != nil || cmd.Bin != "grok" {
		t.Fatalf("%+v %v", cmd, err)
	}
}

// TestAdoptionHappensBeforeRefresh guards the ordering that decides whether
// a codex or kimi account survives.
//
// The CLI rotates its refresh token in place during a run. If rota refreshes
// from its own copy before reading back what the CLI wrote, it presents a
// token that is already spent — and these providers reject a reused refresh
// token permanently. Kimi makes this the common case rather than the rare
// one: its access token lasts fifteen minutes, so nearly every run begins
// with a refresh.
func TestAdoptionHappensBeforeRefresh(t *testing.T) {
	home := t.TempDir()
	// What the CLI left behind: a newer refresh token than rota's.
	rotated := map[string]any{"tokens": map[string]string{
		"refresh_token": "R-from-cli", "access_token": "A-from-cli",
		"id_token": "idt", "account_id": "acct",
	}}
	stageDoc(&Account{}, filepath.Join(home, "auth.json"), rotated)
	a := &Account{ID: 3, Provider: "codex", Token: Token{Access: "old", Refresh: "R-spent", ExpiresAt: 1},
		Extra: map[string]string{"id_token": "idt", "account_id": "acct"}}

	// Adopt first; only then may anything refresh.
	if err := Adopt(a, home); err != nil {
		t.Fatal(err)
	}
	if a.Token.Refresh != "R-from-cli" {
		t.Fatalf("the token the CLI rotated must be adopted before a refresh is attempted, got %q", a.Token.Refresh)
	}
}

func TestDelegatedLoginIsSomethingRotaCanRun(t *testing.T) {
	p, _ := Lookup("grok")
	d, ok := p.(Delegator)
	if !ok {
		t.Fatal("grok delegates")
	}
	home := t.TempDir()
	a := &Account{ID: 6, Provider: "grok", Delegated: true}
	plan := d.LoginPlan(a, home)

	if plan.Bin != "grok" {
		t.Fatalf("bin: %q", plan.Bin)
	}
	if !slices.Contains(plan.Args, "login") || !slices.Contains(plan.Args, "--device-code") {
		t.Fatalf("args: %v", plan.Args)
	}
	if !slices.Contains(plan.Env, "GROK_HOME="+home) {
		t.Fatalf("the login must land in this account's own home: %v", plan.Env)
	}
	// An account rota holds a credential for has nothing to run.
	if _, ok := LoginPlanFor(&Account{Provider: "grok"}, home); ok {
		t.Fatal("only a delegated account has a login to run")
	}
	if _, ok := LoginPlanFor(&Account{Provider: "claude", Delegated: true}, home); ok {
		t.Fatal("a provider that cannot delegate offers no plan")
	}
}

func TestDelegatedGrokLearnsWhoItIs(t *testing.T) {
	home := t.TempDir()
	// The shape the CLI actually writes: a map keyed by issuer and client
	// id, holding the account it signed in as.
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{
	  "https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828": {
	    "auth_mode": "oauth",
	    "user_id": "00000000-0000-4000-8000-000000000001",
	    "email": "someone@example.com",
	    "refresh_token": "not-rota's-to-hold"
	  }
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Account{ID: 8, Provider: "grok", Delegated: true}
	if err := Adopt(a, home); err != nil {
		t.Fatal(err)
	}
	if a.Email != "someone@example.com" || a.UUID != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("a delegated account should still know whose it is: %+v", a)
	}
	if a.Label() != "someone@example.com" {
		t.Fatalf("label: %q", a.Label())
	}
	// rota still holds no credential for it.
	if a.Token.Access != "" || a.Token.Refresh != "" {
		t.Fatalf("a delegated account's tokens belong to the CLI: %+v", a.Token)
	}
	// A home with nothing in it changes nothing and is not an error.
	empty := &Account{ID: 9, Provider: "grok", Delegated: true}
	if err := Adopt(empty, t.TempDir()); err != nil || empty.Email != "" {
		t.Fatalf("%v %+v", err, empty)
	}
}

// TestAnUnsignedAccountCanStillOpenItsCLI covers the way out of a login that
// will not complete: the CLI's own session, where a person can sign in by
// hand. Refusing that because rota can see no credential yet would leave no
// way to obtain one.
func TestAnUnsignedAccountCanStillOpenItsCLI(t *testing.T) {
	for _, provider := range []string{"kimi", "grok"} {
		p, _ := Lookup(provider)
		a := &Account{ID: 1, Provider: provider, Delegated: true}
		home := t.TempDir()

		// Handing the terminal over is allowed: that is how you sign in.
		cmd, err := p.Launch(a, home)
		if err != nil {
			t.Fatalf("%s: opening the CLI must not require being signed in first: %v", provider, err)
		}
		if cmd.Bin != provider {
			t.Fatalf("%s: %+v", provider, cmd)
		}
		// Asking it a question is not: rota would build a command line for
		// a session that cannot authenticate.
		c, ok := p.(SignInChecker)
		if !ok {
			t.Fatalf("%s must be able to say whether it is signed in", provider)
		}
		if err := c.SignedIn(a, home); !errors.Is(err, ErrReauth) {
			t.Fatalf("%s: %v", provider, err)
		}
	}
}

// stageDoc writes a credential fixture the way the old stage helper did:
// the document, indented, at 0600, with the staged marker recorded.
func stageDoc(a *Account, path string, doc any) {
	raw, _ := EncodeIndent(doc)
	_ = stageRaw(a, path, StagedFile{Mode: 0o600, Content: raw})
}
