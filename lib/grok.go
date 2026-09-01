package rota

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Grok (xAI) — an API key, not OAuth.
//
// Grok Build, the CLI xAI ships as `grok`, takes a credential three ways: a
// session token it obtains itself through `grok login`, an external auth
// helper it executes, or the XAI_API_KEY environment variable. Only the last
// is something rota can hold and hand over, and it is the one xAI documents
// for machines: "If you prefer API key authentication (e.g., for CI/CD or
// environments without a browser), set the XAI_API_KEY environment
// variable."
//
// rota previously implemented xAI's OAuth device flow here. That was a dead
// end, and measurably so: the token it produced could only have been passed
// as GROK_API_KEY, a variable the grok binary does not read at all. A
// subscription login still works — run `grok login --device-code` yourself
// with GROK_HOME pointed at this account's directory — but the credential
// then belongs to grok rather than to rota.
const (
	grokKeyPage = "https://console.x.ai"
	// grokAuthFile is where the CLI keeps the credentials it obtained
	// itself, inside whatever GROK_HOME points at.
	grokAuthFile = "auth.json"
)

type grokProvider struct{}

func init() { Register(grokProvider{}) }

func (grokProvider) Name() string { return "grok" }

// Begin has no authorization round trip: the user creates a key on the web
// and pastes it. Returning the key page keeps the two-step shape every other
// provider has.
func (grokProvider) Begin(_ context.Context) (string, map[string]string, error) {
	return grokKeyPage, map[string]string{"kind": "apikey"}, nil
}

// Complete takes either an API key or nothing at all.
//
// Nothing means a delegated login: rota registers the account and hands it a
// private home, and `grok login --device-code` run in that home does the rest.
// That is how a Grok subscription is used, since a subscription session is
// not something any environment variable can carry.
//
// A key's identity is a fingerprint of the key itself, so the same key
// re-pasted lands on the same account and a different key gets its own — no
// network call needed to tell them apart.
func (grokProvider) Complete(_ context.Context, key string, _ map[string]string) (*Token, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return &Token{Delegated: true, Identity: &Identity{UUID: "grok-" + randID()}}, nil
	}
	if !strings.HasPrefix(key, "xai-") {
		return nil, failf(ErrInvalidRequest, "an xAI API key starts with \"xai-\"; get one at %s", grokKeyPage)
	}
	return &Token{Access: key, Identity: &Identity{UUID: "key-" + fingerprint(key)}}, nil
}

// Adopt reads back what the CLI knows about this account.
//
// rota holds no credential for a delegated grok account, so there is no
// token to take; what there is, is the identity it signed in as. Without
// this a person sees a random handle in `list` and cannot tell one account
// from another — which is the whole point of having several.
//
// The file is a map keyed by issuer and client id, and rota reads only the
// entry's name fields. The tokens beside them stay where they are.
func (g grokProvider) Adopt(a *Account, home string) error {
	return g.AdoptFS(a, os.DirFS(home))
}

// AdoptFS is Adopt through a filesystem value: the caller hands the home's
// contents in, wherever they live.
func (grokProvider) AdoptFS(a *Account, fsys fs.FS) error {
	if !a.Delegated {
		return nil
	}
	var doc map[string]struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
	}
	if !readJSONFS(fsys, grokAuthFile, &doc) {
		return nil
	}
	for _, entry := range doc {
		if entry.Email == "" && entry.UserID == "" {
			continue
		}
		if entry.Email != "" {
			a.Email = entry.Email
		}
		if entry.UserID != "" {
			a.UUID = entry.UserID
		}
		return nil
	}
	return nil
}

// SignedIn reports whether this account can authenticate yet. The CLI's own
// answer — "run `grok login`" — is the one thing that will not work: run
// bare it signs into the person's global home, and this account stays empty.
func (grokProvider) SignedIn(a *Account, home string) error {
	if !a.Delegated {
		return nil
	}
	if _, err := os.Stat(filepath.Join(home, grokAuthFile)); err != nil {
		return failf(ErrReauth,
			"%s has not been signed in yet: its delegated login has not run in its private home", a)
	}
	return nil
}

// Models are what `grok models` lists. The CLI shows more once an account is
// signed in, so this is the floor rather than the whole truth.
// CatalogIsFloor says exactly that, so an id beyond the list passes through.
func (grokProvider) CatalogIsFloor() bool { return true }

func (grokProvider) Models() []Model { return append([]Model(nil), grokModels...) }

// grokModels is built once. Models is called several times per run — by the
// command line, by the request check, by the form description — and
// rebuilding the table each time is pure waste; callers get a copy.
var grokModels = []Model{
	{ID: "grok-4.6", Label: "Grok 4.6"},
	{ID: "grok-4.5", Label: "Grok 4.5"},
}

// Efforts are the levels xAI documents for its reasoning models. A model
// that does not support one treats xhigh as high rather than failing.
func (grokProvider) Efforts() []string { return []string{"low", "medium", "high", "xhigh"} }

// Defaults follow the CLI's own default model, with a mid-range effort.
func (grokProvider) Defaults() (string, string) { return "grok-4.6", "high" }

// Launch gives the CLI the key and a directory of its own.
//
// The private GROK_HOME is what keeps accounts apart: grok keeps its session,
// config, memory and worktree registry there, and pointing two accounts at
// one directory would have them share all of it. Everything that could
// redirect the credential elsewhere — another home, a config overlay, an auth
// helper — is dropped, so a shell profile cannot quietly bill someone else.
func (grokProvider) Launch(a *Account, home string) (*Command, error) {
	if home == "" {
		return nil, errors.New("grok needs a private home directory")
	}
	if a.Delegated {
		// rota holds nothing here: the CLI signed itself in inside this home
		// and keeps its own tokens there. Everything that could point it at
		// another credential is dropped, including a stray key.
		//
		return &Command{
			Bin:  "grok",
			Env:  []string{"GROK_HOME=" + home},
			Drop: append(append([]string(nil), grokCompeting...), "XAI_API_KEY"),
		}, nil
	}
	// An account registered by the old OAuth device flow holds a session
	// token, not an API key. Handing it over as XAI_API_KEY earns a flat
	// "Incorrect API key provided" from xAI, which says nothing about how to
	// fix it — so state the condition here, where both fixes are known: a
	// real key from the console, or a fresh delegated login in this
	// account's own home.
	if !strings.HasPrefix(a.Token.Access, "xai-") {
		return nil, failf(ErrReauth,
			"%s holds a session token rather than an xAI API key: supply a key from %s, "+
				"or redo the delegated login in this account's private home", a, grokKeyPage)
	}
	return &Command{
		Bin:  "grok",
		Env:  []string{"XAI_API_KEY=" + a.Token.Access, "GROK_HOME=" + home},
		Drop: append([]string(nil), grokCompeting...),
	}, nil
}

// grokCompeting are the variables that could send this run's work to some
// other credential or configuration.
var grokCompeting = dropList(
	"GROK_CONFIG", "GROK_CONFIG_PATH", "GROK_AUTH_PROVIDER_COMMAND",
	"GROK_AUTH_PROVIDER_ACCESS_TOKEN", "GROK_AUTH_PROVIDER_REFRESH_TOKEN",
	"GROK_AUTH_PROVIDER_LABEL", "GROK_CODE_XAI_API_KEY", "GROK_DEPLOYMENT_KEY",
	"GROK_SANDBOX", "GROK_API_KEY", "XAI_API_BASE_URL",
)

// LoginPlan is what signs a delegated grok account in. The device flow needs
// a browser somewhere and a person to approve in it; the session it stores
// belongs to grok, inside the home rota reserved for it.
func (grokProvider) LoginPlan(_ *Account, home string) LoginPlan {
	return LoginPlan{
		Bin:  "grok",
		Args: []string{"login", "--device-code"},
		Env:  []string{"GROK_HOME=" + home},
		Drop: append(append([]string(nil), grokCompeting...), "XAI_API_KEY"),
	}
}
