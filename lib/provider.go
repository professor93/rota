// Package rota runs several AI coding CLIs across several accounts without
// ever switching the account you are logged into. It is not a proxy: it
// launches each vendor's own CLI with a credential you own, and implements
// only what no CLI exposes — logging in, refreshing a token, reading quota.
package rota

import (
	"context"
	"slices"
	"strings"
	"sync"
)

// Provider is what rota needs to know about one vendor. Optional abilities —
// refreshing, identity lookup, quota — are separate interfaces so a provider
// that lacks one simply does not implement it.
type Provider interface {
	// Name is the unique key callers register, look up and persist this provider under.
	Name() string
	// Begin starts a login: the URL the user must open, and the opaque state
	// Complete will need. The reserved key "kind" in state may be "code"
	// (default; the user pastes a code), "device" (the user only approves in
	// the browser), "apikey" (the user pastes a key) or "delegated" (nothing
	// is pasted: the vendor CLI signs itself in, in the account's own home).
	Begin(ctx context.Context) (url string, state map[string]string, err error)
	// Complete turns what the user pasted (or nothing, for device flows)
	// into a token. ErrAuthPending means "not approved yet, ask again".
	Complete(ctx context.Context, code string, state map[string]string) (*Token, error)
	// Launch says how to start this provider's CLI for one account. home is
	// a private directory reserved for that account; providers whose CLI
	// reads credentials only from a file stage them there.
	Launch(a *Account, home string) (*Command, error)
}

// Refresher rotates an expiring token. ErrDeadToken means the lineage is
// finished and only a fresh login revives it. Providers whose credential
// never expires (an API key) do not implement it.
type Refresher interface {
	Refresh(ctx context.Context, a *Account) (*Token, error)
}

// Identifier resolves a token to whoever owns it, for providers with a
// profile endpoint. Identity is a nicety, never a requirement.
type Identifier interface {
	Identify(ctx context.Context, accessToken string) (*Identity, error)
}

// Delegator is implemented by a provider whose CLI can sign itself in and
// keep its own credentials. rota then holds no token at all — it supplies
// only the private directory the CLI stores them in, which is what keeps one
// account from treading on another.
//
// A delegated login is finished with no code to paste; LoginPlan says what
// must be run once, in that account's own home.
type Delegator interface {
	LoginPlan(a *Account, home string) LoginPlan
}

// LoginPlan is how a delegated account is signed in: a command run with the
// terminal attached, because the flow needs a person at it. rota runs this
// itself rather than printing it, since the one thing that must not go wrong
// — the private home — is the part a person would have to retype.
type LoginPlan struct {
	Bin  string
	Args []string
	Env  []string
	// Drop lists inherited variables the login must not see — the same
	// proxies, CA overrides and competing credentials every run drops.
	Drop []string
}

// SignInChecker is implemented by a provider that can tell, without asking
// the network, whether an account is usable yet.
//
// It exists because the two questions differ. Handing someone the CLI's own
// session must work whatever the credential state — that is how a delegated
// account gets signed in at all — while building a command line for a
// headless run against an account that cannot authenticate only produces a
// vendor error nobody can act on.
type SignInChecker interface {
	SignedIn(a *Account, home string) error
}

// Meter reads remaining quota, for the few providers that publish one.
type Meter interface {
	Quota(ctx context.Context, accessToken string) (*Quota, error)
}

// Command is how a vendor CLI must be started for one account.
type Command struct {
	// Bin is the executable to find on PATH.
	Bin string
	// Env entries carry the credential; they replace any inherited value.
	Env []string
	// Drop lists inherited variables that would outrank the credential and
	// must not reach the child.
	Drop []string
	// BaseEnv is the environment the child starts from, before Env replaces
	// and Drop removes. The SDK never reads the process environment: what a
	// child may inherit is the application's decision, so the application
	// passes it — usually its process environment with its own secrets removed.
	// A nil BaseEnv means exactly that: nothing inherited.
	BaseEnv []string
}

// DefaultProvider is what an empty provider name resolves to.
var DefaultProvider = "claude"

// Registry is a set of providers an application composes. Most programs use
// DefaultRegistry through the package-level functions, which the builtin
// vendors register themselves into; a program that wants a different set —
// fewer vendors, its own, two sets side by side — builds its own value and
// carries it. It is guarded because Register may run while requests are
// served, and Lookup runs on every run, refresh and form description.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Provider
	// Default is what an empty provider name resolves to; empty refuses
	// empty names.
	Default string
}

// NewRegistry is an empty registry: no builtins, no default.
func NewRegistry() *Registry { return &Registry{m: map[string]Provider{}} }

// Register adds a provider under its Name, replacing any previous one.
//
// Replacing wraps nothing: a provider registered over a builtin answers
// alone, and its optional abilities — Refresher, Identifier, Meter,
// Delegator, SignInChecker, the catalogs — are only those it implements
// itself. A wrapper that means to keep the builtin's abilities must embed
// or forward them deliberately.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[p.Name()] = p
}

// Lookup resolves a provider name; "" means the registry's Default.
func (r *Registry) Lookup(name string) (Provider, error) {
	r.mu.RLock()
	if name == "" {
		name = r.Default
	}
	p, ok := r.m[name]
	r.mu.RUnlock()
	if !ok {
		return nil, failf(ErrInvalidRequest, "unknown provider %q; known: %s", name, strings.Join(r.Providers(), ", "))
	}
	return p, nil
}

// Providers lists registered provider names, sorted.
func (r *Registry) Providers() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.m))
	for n := range r.m {
		names = append(names, n)
	}
	r.mu.RUnlock()
	slices.Sort(names)
	return names
}

// DefaultRegistry is the registry the package-level functions speak for,
// and the one the builtin vendors register into at init.
var DefaultRegistry = &Registry{m: map[string]Provider{}, Default: "claude"}

// Register adds a provider to DefaultRegistry.
func Register(p Provider) { DefaultRegistry.Register(p) }

// Lookup resolves a provider name in DefaultRegistry; "" means
// DefaultProvider.
func Lookup(name string) (Provider, error) {
	if name == "" {
		name = DefaultProvider
	}
	return DefaultRegistry.Lookup(name)
}

// Providers lists DefaultRegistry's provider names, sorted.
func Providers() []string { return DefaultRegistry.Providers() }

// networkRedirecting can send a credential somewhere other than the provider
// it belongs to, whichever CLI is running. A proxy and a trusted certificate
// together are a complete interception of an OAuth token, and neither is
// named by any vendor's own configuration — so every provider drops them.
var networkRedirecting = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy",
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
}

// NetworkRedirecting is that list, as a copy a caller may keep or extend
// without corrupting anyone else's view of it.
func NetworkRedirecting() []string { return append([]string(nil), networkRedirecting...) }

// dropList is a provider's own list plus the ones every provider drops.
func dropList(own ...string) []string {
	out := make([]string, 0, len(own)+len(networkRedirecting))
	return append(append(out, own...), networkRedirecting...)
}
