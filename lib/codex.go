package rota

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Codex (OpenAI) — OAuth2 with PKCE. The official CLI runs a loopback server
// on port 1455; rota does not, so the user copies the redirected URL out of
// the address bar instead. The redirect_uri still has to match exactly.
const (
	codexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexRedirectURI = "http://localhost:1455/auth/callback"
	codexScope       = "openid email profile offline_access"
	// The refresh grant asks for a narrower scope, mirroring the official client.
	codexRefreshScope = "openid profile email"
)

// CodexEndpoints are the vendor endpoints this provider calls, exported for
// the same reason as ClaudeEndpoints.
var CodexEndpoints = struct {
	Authorize, Token string
}{
	Authorize: "https://auth.openai.com/oauth/authorize",
	Token:     "https://auth.openai.com/oauth/token",
}

type codexProvider struct{}

func init() { Register(codexProvider{}) }

func (codexProvider) Name() string { return "codex" }

func (codexProvider) Begin(_ context.Context) (string, map[string]string, error) {
	verifier, challenge := pkce()
	state := randB64(24)
	q := url.Values{}
	q.Set("client_id", codexClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", codexRedirectURI)
	q.Set("scope", codexScope)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("prompt", "login")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	return CodexEndpoints.Authorize + "?" + q.Encode(), map[string]string{"verifier": verifier, "state": state}, nil
}

// extractCode accepts a bare authorization code or the whole redirect URL
// the browser landed on, which is what a user can copy when no loopback
// server is listening.
func extractCode(input string) string {
	input = strings.TrimSpace(input)
	if !strings.Contains(input, "://") {
		return input
	}
	if u, err := url.Parse(input); err == nil && u.Query().Get("code") != "" {
		return u.Query().Get("code")
	}
	return input
}

// codexToken carries the id_token and ChatGPT account id forward: the CLI's
// auth.json needs both, and neither is recoverable from the access token.
func codexToken(r *oauthTokenResp) *Token {
	t := r.token()
	if r.IDToken != "" {
		t.Extra = map[string]string{"id_token": r.IDToken}
		if acct := chatgptAccountID(r.IDToken); acct != "" {
			t.Extra["account_id"] = acct
		}
	}
	return t
}

func (codexProvider) Complete(ctx context.Context, code string, state map[string]string) (*Token, error) {
	var r oauthTokenResp
	err := postForm(ctx, CodexEndpoints.Token, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {codexClientID}, "code": {extractCode(code)},
		"redirect_uri": {codexRedirectURI}, "code_verifier": {state["verifier"]},
	}, &r, nil)
	if err := r.verdict(err, grantCode); err != nil {
		return nil, err
	}
	return codexToken(&r), nil
}

func (codexProvider) Refresh(ctx context.Context, a *Account) (*Token, error) {
	var r oauthTokenResp
	err := postForm(ctx, CodexEndpoints.Token, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {codexClientID},
		"refresh_token": {a.Token.Refresh}, "scope": {codexRefreshScope},
	}, &r, nil)
	if err := r.verdict(err, grantRefresh); err != nil {
		return nil, err
	}
	return codexToken(&r), nil
}

// Launch stages a private CODEX_HOME and points the CLI at it. The
// environment route does not work: CODEX_ACCESS_TOKEN belongs to a separate
// "Agent Identity" feature and a ChatGPT OAuth token placed there is refused
// against the API-key host. The API-key variables are still dropped so a
// stray one cannot take over.
//
// The CLI owns auth.json once it starts and rotates the refresh token in
// place, so anything it rotated is adopted before rota overwrites the file.
// Adopt reads back the auth.json the CLI owns while it runs.
func (c codexProvider) Adopt(a *Account, home string) error {
	return c.AdoptFS(a, os.DirFS(home))
}

// AdoptFS is Adopt through a filesystem value.
func (codexProvider) AdoptFS(a *Account, fsys fs.FS) error {
	adoptCodexFS(a, fsys)
	return nil
}

func (p codexProvider) Launch(a *Account, home string) (*Command, error) {
	if home == "" {
		return nil, errors.New("this provider keeps its credentials in a file and needs a private home directory")
	}
	adoptCodexFS(a, os.DirFS(home))
	cmd, files, err := p.Plan(context.Background(), a, home)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := stageRaw(a, filepath.Join(home, f.Path), f); err != nil {
			return nil, err
		}
	}
	return cmd, nil
}

// Plan is Launch as values: the command, and auth.json as content rather
// than a write. Adoption is the caller's prior step (AdoptFrom / Adopt), as
// it is for every run; the id_token repair may refresh over the network —
// bounded by HTTPClient's own timeout — and the caller persists the account
// afterwards either way.
func (p codexProvider) Plan(ctx context.Context, a *Account, home string) (*Command, []StagedFile, error) {
	if home == "" {
		return nil, nil, errors.New("codex keeps its credentials in CODEX_HOME and needs a private home")
	}
	// The CLI refuses an auth.json without an id_token, and accounts stored
	// before rota captured it have none. One refresh grant returns a fresh
	// one, so such an account repairs itself instead of demanding a login.
	if a.Extra["id_token"] == "" {
		if a.Token.Refresh == "" {
			return nil, nil, failf(ErrReauth, "%s: no id_token and no refresh token", a)
		}
		t, err := p.Refresh(ctx, a)
		switch {
		case errors.Is(err, ErrDeadToken):
			a.Dead = true
			return nil, nil, failf(ErrReauth, "%s: session expired", a)
		case err != nil:
			return nil, nil, fmt.Errorf("%s: refresh failed: %w", a, err)
		}
		a.apply(t)
	}

	// Shape mirrors the CLI's own auth.json; auth_mode selects the ChatGPT
	// OAuth path over the API-key one.
	doc := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]string{
			"id_token": a.Extra["id_token"], "access_token": a.Token.Access,
			"refresh_token": a.Token.Refresh, "account_id": a.Extra["account_id"],
		},
		"last_refresh": Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	raw, err := EncodeIndent(doc)
	if err != nil {
		return nil, nil, err
	}
	return &Command{
		Bin:  "codex",
		Env:  []string{"CODEX_HOME=" + home},
		Drop: dropList("OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_BASE_URL"),
	}, []StagedFile{{Path: "auth.json", Mode: 0o600, Content: raw}}, nil
}

// adoptCodex pulls a token the CLI rotated back into the account. A file
// for a different ChatGPT account is never adopted.
func adoptCodexFS(a *Account, fsys fs.FS) {
	var doc struct {
		Tokens struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if !readJSONFS(fsys, "auth.json", &doc) {
		return
	}
	tok := doc.Tokens
	if tok.AccountID != "" && a.Extra["account_id"] != "" && tok.AccountID != a.Extra["account_id"] {
		return
	}
	if !a.cliRotated(tok.RefreshToken) {
		return
	}
	a.Token.Refresh = tok.RefreshToken
	if tok.AccessToken != "" {
		a.Token.Access = tok.AccessToken
		a.Token.ExpiresAt = jwtExpiryMS(tok.AccessToken) // the CLI records no expiry
	}
	if tok.IDToken != "" {
		a.setExtra("id_token", tok.IDToken)
	}
	if tok.AccountID != "" {
		a.setExtra("account_id", tok.AccountID)
	}
}

// Models are what this Codex CLI actually offers; the list is its own,
// printed by `codex debug models`, in the order its picker uses. The hidden
// entries are accepted but not advertised, so they are not listed here.
func (codexProvider) Models() []Model { return copyModels(codexModels) }

// codexModels is built once. Models is called several times per run — by the
// command line, by the request check, by the form description — and
// rebuilding the table each time is pure waste; callers get a copy.
var codexModels = []Model{
	{ID: "gpt-5.6-sol", Label: "GPT-5.6 Sol"},
	{ID: "gpt-5.6-terra", Label: "GPT-5.6 Terra"},
	{ID: "gpt-5.6-luna", Label: "GPT-5.6 Luna"},
	{ID: "gpt-5.5", Label: "GPT-5.5"},
	{ID: "gpt-5.2", Label: "GPT-5.2"},
}

// Efforts is the union across those models. "ultra" is accepted only by the
// two top models; asking any other for it is the CLI's error to give, since
// only it knows which model a config profile finally selected.
func (codexProvider) Efforts() []string {
	return []string{"low", "medium", "high", "xhigh", "max", "ultra"}
}

// Defaults name no model on purpose. Which models a codex account may use
// depends on its ChatGPT plan (see ModelsFor), so the CLI is better placed
// to choose than any fixed answer here; the effort default still applies.
func (codexProvider) Defaults() (string, string) { return "", "medium" }

// ModelsFor reads the models this account may actually use. The CLI caches
// them per home, because a ChatGPT plan decides the list — asking for one
// outside it fails only after the session has started, with "not supported
// when using Codex with a ChatGPT account".
func (codexProvider) ModelsFor(_ *Account, home string) []Model {
	var doc struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if !readJSON(filepath.Join(home, "models_cache.json"), &doc) {
		return nil
	}
	out := make([]Model, 0, len(doc.Models))
	for _, m := range doc.Models {
		if m.Slug == "" || m.Visibility != "list" {
			continue // hidden entries exist but are not offered
		}
		out = append(out, Model{ID: m.Slug, Label: m.DisplayName})
	}
	return out
}
