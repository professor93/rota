package rota

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
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

// claudeLongScopes is all a long-lived token asks for, and it is the whole
// difference at the authorize step: permission to run inference, and none of
// the rest. That is the provider's own design — the token it hands back can
// drive Claude Code and cannot read the profile or the usage endpoint — and
// it is read from Claude Code's `setup-token`, not chosen here.
var claudeLongScopes = []string{"user:inference"}

// claudeLongSeconds is the life the exchange asks for: one year, as a JSON
// number, which is the other half of the difference. The ordinary exchange
// asks for nothing and is given eight hours.
const claudeLongSeconds = 31536000

type claudeProvider struct{}

func init() { Register(claudeProvider{}) }

func (claudeProvider) Name() string { return "claude" }

func (claudeProvider) Begin(_ context.Context) (string, map[string]string, error) {
	return claudeBegin(claudeScopes)
}

// BeginLong is the same login asking to be allowed less: inference only, so
// what comes back is a token that runs the CLI and can do nothing else with
// the account.
func (claudeProvider) BeginLong(_ context.Context) (string, map[string]string, error) {
	return claudeBegin(claudeLongScopes)
}

func claudeBegin(scopes []string) (string, map[string]string, error) {
	verifier, challenge := pkce()
	state := randB64(24)
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", claudeClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", claudeRedirectURI)
	q.Set("scope", strings.Join(scopes, " "))
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

// Complete finishes an ordinary login, and then asks the profile once what
// the account's plan is.
//
// The exchange says who logged in but not on what terms, and Claude Code
// wants both in the login it is handed: the plan decides which models it
// offers and the limits it shows, and an organisation's name is what its
// Remote Control looks for. A profile that cannot be read never fails the
// login — the token is good either way, and Claude Code asks for itself.
func (p claudeProvider) Complete(ctx context.Context, code string, state map[string]string) (*Token, error) {
	t, err := p.exchange(ctx, code, state, nil)
	if err != nil {
		return nil, err
	}
	if prof, err := readClaudeProfile(ctx, t.Access); err == nil {
		prof.into(t)
	}
	// A fresh login is a new lineage, and when its refresh token ends is
	// not something the exchange says. Whatever the account remembers is the
	// last login's, and carried over it would both be written into Claude
	// Code's store as this login's and make a rotation of this login look
	// like somebody else's. An empty value is how a token says "forget it".
	if t.Extra == nil {
		t.Extra = map[string]string{}
	}
	t.Extra[claudeRefreshUntil] = ""
	return t, nil
}

// claudeProfile is the part of the profile endpoint's reply rota reads: who
// the token belongs to, and the plan and name of the organisation it is in.
type claudeProfile struct {
	Account *struct {
		UUID        string `json:"uuid"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	} `json:"account"`
	Organization *struct {
		UUID          string `json:"uuid"`
		Name          string `json:"name"`
		Type          string `json:"organization_type"`
		RateLimitTier string `json:"rate_limit_tier"`
	} `json:"organization"`
}

func readClaudeProfile(ctx context.Context, access string) (*claudeProfile, error) {
	var p claudeProfile
	if err := getJSON(ctx, ClaudeEndpoints.Profile, access, &p, nil); err != nil {
		return nil, err
	}
	return &p, nil
}

// claudePlans maps the profile's organisation type onto the word Claude Code
// stores as subscriptionType. Anything else is left unsaid rather than
// guessed: an absent plan is one Claude Code looks up, a wrong one is a
// model list it believes.
var claudePlans = map[string]string{
	"claude_max": "max", "claude_pro": "pro", "claude_enterprise": "enterprise", "claude_team": "team",
}

// The account keeps what Claude Code's login carries beyond the tokens, so a
// login rota writes into a home later says everything the last one said.
const (
	claudeSubscription  = "subscription_type"
	claudeRateLimitTier = "rate_limit_tier"
	claudeRefreshUntil  = "refresh_token_expires_at"
	claudeClient        = "client_id"
	claudeOrgName       = "organization_name"
	claudeDisplayName   = "display_name"
)

// into folds a profile into a fresh token: the plan for the credential
// store, the names for the identity Claude Code shows, and the identity
// itself when the exchange carried none.
func (p *claudeProfile) into(t *Token) {
	if t.Identity == nil && p.Account != nil && p.Account.UUID != "" {
		t.Identity = &Identity{UUID: p.Account.UUID, Email: p.Account.Email}
		if p.Organization != nil {
			t.Identity.Org = p.Organization.UUID
		}
	}
	set := func(k, v string) {
		if v == "" {
			return
		}
		if t.Extra == nil {
			t.Extra = map[string]string{}
		}
		t.Extra[k] = v
	}
	if o := p.Organization; o != nil {
		set(claudeSubscription, claudePlans[o.Type])
		set(claudeRateLimitTier, o.RateLimitTier)
		set(claudeOrgName, o.Name)
	}
	if p.Account != nil {
		set(claudeDisplayName, p.Account.DisplayName)
	}
}

// CompleteLong finishes a long login. One field more than the ordinary
// exchange — the life asked for — and what comes back is a token good for
// that long with no refresh anyone should keep.
func (p claudeProvider) CompleteLong(ctx context.Context, code string, state map[string]string) (*Token, error) {
	t, err := p.exchange(ctx, code, state, map[string]any{"expires_in": claudeLongSeconds})
	if err != nil {
		return nil, err
	}
	// The reply need not echo the life it granted. What was asked for is the
	// honest fallback: it is what the token was issued as, and an expiry
	// guessed too early only costs a token that still worked.
	if t.ExpiresAt == 0 {
		t.ExpiresAt = nowMS() + claudeLongSeconds*1000
	}
	// Dropped rather than stored: a refresh here belongs to a lineage
	// nothing will ever rotate, and a secret kept for no reason is a secret
	// that can leak for no reason.
	t.Refresh = ""
	return t, nil
}

// exchange is the authorization-code request both logins make. The body is
// map[string]any only because one caller adds a number to it; everything a
// provider sends is still written out in one place, where the shape of the
// request can be read at a glance.
func (claudeProvider) exchange(ctx context.Context, code string, state map[string]string, extra map[string]any) (*Token, error) {
	st := state["state"]
	// The callback page renders the code as "code#state"; accept either.
	if c, s, ok := strings.Cut(code, "#"); ok {
		code, st = c, s
	}
	body := map[string]any{
		"grant_type": "authorization_code", "code": code, "redirect_uri": claudeRedirectURI,
		"client_id": claudeClientID, "code_verifier": state["verifier"], "state": st,
	}
	for k, v := range extra {
		body[k] = v
	}
	var r claudeTokenResp
	err := postJSON(ctx, ClaudeEndpoints.Token, body, &r, nil)
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
	p, err := readClaudeProfile(ctx, access)
	if err != nil {
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

// Launch starts Claude Code on one of two routes, and claudehome.go is where
// both are described: a login of the account's own kept in home, which
// Claude Code refreshes for itself, or a token in the environment when there
// is no home to keep one in.
//
// On the first route Launch reads home before anything else — Claude Code
// may have rotated the login since — and writes the account's login there
// when the home does not already hold it. Writing is only safe while no
// Claude Code process is alive in that home, which this package cannot see;
// an application that runs several processes per home plans instead
// (StagePlan) and writes when it knows. rota's store does exactly that.
func (p claudeProvider) Launch(a *Account, home string) (*Command, error) {
	if !claudeStored(a, home) {
		return claudeEnvCommand(a), nil
	}
	if err := p.AdoptFS(a, os.DirFS(home)); err != nil {
		return nil, err
	}
	// Adoption may have found that Claude Code blanked this very login, and
	// a dead login launches nothing — unless a long token takes over, which
	// is the environment route Plan now chooses.
	if err := launchable(a); err != nil {
		return nil, err
	}
	cmd, files, err := p.Plan(context.Background(), a, home)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := stageMerged(a, home, f); err != nil {
			return nil, err
		}
	}
	return cmd, nil
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
