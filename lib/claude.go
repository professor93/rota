package rota

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Claude (Anthropic) — OAuth2 with PKCE. Constants come from the production
// config block inside the Claude Code binary, not guessed.
const (
	claudeClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeRedirectURI = "https://platform.claude.com/oauth/code/callback"
	claudeBeta        = "oauth-2025-04-20"
)

// ClaudeEndpoints are the vendor endpoints this provider calls, exported so
// an application can point them at a gateway or a test double. The defaults
// are the production service.
var ClaudeEndpoints = struct {
	Authorize, Token, Profile, Usage string
}{
	Authorize: "https://claude.com/cai/oauth/authorize",
	Token:     "https://platform.claude.com/v1/oauth/token",
	Profile:   "https://api.anthropic.com/api/oauth/profile",
	Usage:     "https://api.anthropic.com/api/oauth/usage",
}

var claudeScopes = []string{
	"org:create_api_key", "user:profile", "user:inference",
	"user:sessions:claude_code", "user:mcp_servers", "user:file_upload",
}

type claudeProvider struct{}

func init() { Register(claudeProvider{}) }

func (claudeProvider) Name() string { return "claude" }

func (claudeProvider) Begin(_ context.Context) (string, map[string]string, error) {
	verifier, challenge := pkce()
	state := randB64(24)
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", claudeClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", claudeRedirectURI)
	q.Set("scope", strings.Join(claudeScopes, " "))
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	return ClaudeEndpoints.Authorize + "?" + q.Encode(), map[string]string{"verifier": verifier, "state": state}, nil
}

type claudeTokenResp struct {
	oauthTokenResp
	Account *struct {
		UUID  string `json:"uuid"`
		Email string `json:"email_address"`
	} `json:"account"`
	Organization *struct {
		UUID string `json:"uuid"`
	} `json:"organization"`
}

func (r *claudeTokenResp) token() *Token {
	t := r.oauthTokenResp.token()
	if r.Account != nil && r.Account.UUID != "" {
		t.Identity = &Identity{UUID: r.Account.UUID, Email: r.Account.Email}
		if r.Organization != nil {
			t.Identity.Org = r.Organization.UUID
		}
	}
	return t
}

func (claudeProvider) Complete(ctx context.Context, code string, state map[string]string) (*Token, error) {
	st := state["state"]
	// The callback page renders the code as "code#state"; accept either.
	if c, s, ok := strings.Cut(code, "#"); ok {
		code, st = c, s
	}
	var r claudeTokenResp
	err := postJSON(ctx, ClaudeEndpoints.Token, map[string]string{
		"grant_type": "authorization_code", "code": code, "redirect_uri": claudeRedirectURI,
		"client_id": claudeClientID, "code_verifier": state["verifier"], "state": st,
	}, &r, nil)
	if err := r.verdict(err, grantCode); err != nil {
		return nil, err
	}
	return r.token(), nil
}

func (claudeProvider) Refresh(ctx context.Context, a *Account) (*Token, error) {
	var r claudeTokenResp
	err := postJSON(ctx, ClaudeEndpoints.Token, map[string]string{
		"grant_type": "refresh_token", "refresh_token": a.Token.Refresh, "client_id": claudeClientID,
	}, &r, nil)
	if err := r.verdict(err, grantRefresh); err != nil {
		return nil, err
	}
	return r.token(), nil
}

func (claudeProvider) Identify(ctx context.Context, access string) (*Identity, error) {
	var p struct {
		Account *struct {
			UUID  string `json:"uuid"`
			Email string `json:"email"`
		} `json:"account"`
		Organization *struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if err := getJSON(ctx, ClaudeEndpoints.Profile, access, &p, nil); err != nil {
		return nil, err
	}
	if p.Account == nil || p.Account.UUID == "" {
		return nil, errors.New("profile carried no account uuid")
	}
	id := &Identity{UUID: p.Account.UUID, Email: p.Account.Email}
	if p.Organization != nil {
		id.Org = p.Organization.UUID
	}
	return id, nil
}

// claudeUsage mirrors the usage endpoint. Only scope.model is decoded from
// `limits`: every other field is left alone so an unexpected shape there
// cannot fail the whole reading.
type claudeUsage struct {
	FiveHour *struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    When    `json:"resets_at"`
	} `json:"five_hour"`
	SevenDay *struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    When    `json:"resets_at"`
	} `json:"seven_day"`
	Limits []struct {
		Percent  float64 `json:"percent"`
		ResetsAt When    `json:"resets_at"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
	Extra *struct {
		Enabled      bool     `json:"is_enabled"`
		UsedCredits  *float64 `json:"used_credits"`
		MonthlyLimit *float64 `json:"monthly_limit"`
		Currency     string   `json:"currency"`
	} `json:"extra_usage"`
}

func (claudeProvider) Quota(ctx context.Context, access string) (*Quota, error) {
	var u claudeUsage
	if err := getJSON(ctx, ClaudeEndpoints.Usage, access, &u, map[string]string{"anthropic-beta": claudeBeta}); err != nil {
		return nil, err
	}
	q := &Quota{}
	if u.FiveHour != nil {
		q.Windows = append(q.Windows, Window{Name: "5h", Percent: u.FiveHour.Utilization, ResetsAt: u.FiveHour.ResetsAt, Primary: true})
	}
	if u.SevenDay != nil {
		q.Windows = append(q.Windows, Window{Name: "7d", Percent: u.SevenDay.Utilization, ResetsAt: u.SevenDay.ResetsAt})
	}
	for _, l := range u.Limits {
		if l.Scope == nil || l.Scope.Model == nil || l.Scope.Model.DisplayName == "" {
			continue // unscoped limits already appear as 5h / 7d
		}
		q.Windows = append(q.Windows, Window{Name: l.Scope.Model.DisplayName, Percent: l.Percent, ResetsAt: l.ResetsAt, Scoped: true})
	}
	if e := u.Extra; e != nil && e.Enabled && e.UsedCredits != nil && e.MonthlyLimit != nil {
		q.Note = fmt.Sprintf("extra usage %.2f / %.2f %s", *e.UsedCredits/100, *e.MonthlyLimit/100, e.Currency)
		q.Extra = &ExtraUsage{Used: *e.UsedCredits / 100, Limit: *e.MonthlyLimit / 100, Currency: e.Currency}
	}
	return q, nil
}

// Launch hands the token to Claude Code through the environment. Anything
// that would outrank it — or redirect it to another host — is dropped: a
// stray ANTHROPIC_BASE_URL would send this OAuth token to a third party.
func (claudeProvider) Launch(a *Account, home string) (*Command, error) {
	env := []string{"CLAUDE_CODE_OAUTH_TOKEN=" + a.Token.Access}
	if a.ConfigDir != "" {
		// Claude Code keeps memory, skills and settings here. Unset, it
		// finds the person's own — which is right until an account is meant
		// for one project, and then it is exactly wrong. The value is the
		// account's own field, not the home argument: the two usually agree,
		// but only because rota's store passes ConfigDir as the home, and an
		// SDK must not depend on one caller's habit.
		env = append(env, "CLAUDE_CONFIG_DIR="+a.ConfigDir)
	}
	return &Command{
		Bin: "claude",
		Env: env,
		Drop: dropList("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
			"ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_VERTEX_BASE_URL",
			"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"),
	}, nil
}

// Models are the Claude 5 family plus the aliases Claude Code resolves to
// "the latest of that line". Aliases are what people type; rota sends the
// full id so a run is reproducible even after an alias moves.
func (claudeProvider) Models() []Model { return copyModels(claudeModels) }

// claudeModels is built once. Models is called several times per run — by the
// command line, by the request check, by the form description — and
// rebuilding the table each time is pure waste; callers get a copy.
var claudeModels = []Model{
	{ID: "claude-opus-5", Aliases: []string{"opus"}, Label: "Opus 5"},
	{ID: "claude-fable-5", Aliases: []string{"fable"}, Label: "Fable 5"},
	{ID: "claude-sonnet-5", Aliases: []string{"sonnet"}, Label: "Sonnet 5"},
	{ID: "claude-haiku-4-5-20251001", Aliases: []string{"haiku"}, Label: "Haiku 4.5"},
}

// Efforts are Claude Code's --effort levels.
func (claudeProvider) Efforts() []string {
	return []string{"low", "medium", "high", "xhigh", "max"}
}

// Defaults are deliberately mid-range: capable enough for real work,
// predictable in cost, and unaffected by the CLI changing its own default.
func (claudeProvider) Defaults() (string, string) { return "claude-opus-5", "high" }
