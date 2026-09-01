package rota

import (
	"path/filepath"
	"strconv"
	"time"
)

// Account is one authenticated identity with one provider.
type Account struct {
	ID       int    `json:"id"`
	Provider string `json:"provider"`
	Email    string `json:"email,omitempty"`
	UUID     string `json:"uuid,omitempty"`
	Org      string `json:"org,omitempty"`
	Token    Token  `json:"token"`
	// Dead marks a lineage the provider rejected permanently: re-auth is
	// the only fix, so runs skip it instead of retrying forever.
	Dead bool `json:"dead,omitzero"`
	// Delegated marks an account whose credential belongs to the vendor CLI
	// rather than to rota: rota gives it a private directory and the CLI
	// signs itself in there. There is no token here to expire or refresh.
	Delegated bool `json:"delegated,omitzero"`
	// Extra holds provider-specific state that must survive restarts — a
	// device id, an id_token. Untyped, so adding a provider never changes
	// this struct.
	Extra map[string]string `json:"extra,omitempty"`
	// Staged fingerprints the refresh token rota last wrote into this
	// account's private CLI home, so a later run can tell whether the CLI
	// rotated it. stagedNone marks a home older than the current login.
	Staged string `json:"staged,omitempty"`
	// Order and Threshold are carried, not interpreted. A store has to
	// persist an account's place in whatever queue an application keeps and
	// the usage it should be retired at, but what an order of 0 or an unset
	// threshold means is that application's rule, not this package's. See
	// one consumer keeps such a rule in its own rotation package.
	Order     int `json:"order,omitzero"`
	Threshold int `json:"threshold,omitzero"`
	// Cwd is where a run on this account starts when the request names no
	// directory of its own. An account kept for one project is how a person
	// stops having to say which project every time.
	Cwd string `json:"cwd,omitempty"`
	// ConfigDir is this account's own CLI configuration — its memory files,
	// skills and settings, and the private home its credentials are staged
	// in. Empty leaves both where they were: rota's own per-account
	// directory for the CLIs that keep a home, and the person's own
	// ~/.claude for Claude Code, which is shared until it is told otherwise.
	ConfigDir string `json:"config_dir,omitempty"`
	Quota     *Quota `json:"quota,omitempty"`
	QuotaAt   int64  `json:"quotaAt,omitzero"` // unix ms of last quota fetch
}

// Label is the account's display name: email, else a uuid prefix, else id.
func (a *Account) Label() string {
	switch {
	case a.Email != "":
		return a.Email
	case a.UUID != "":
		return truncate(a.UUID, 12)
	}
	return "account-" + strconv.Itoa(a.ID)
}

// String is "provider/label".
func (a *Account) String() string { return a.Provider + "/" + a.Label() }

// ExpiryBuffer refreshes a token slightly before it actually expires, so a
// run never starts with seconds left on the clock. A variable rather than a
// constant: how much margin a deployment wants is its own call.
var ExpiryBuffer = 5 * time.Minute

// Expired reports whether the access token is past or within five minutes
// of its expiry. A token with no expiry never expires.
func (a *Account) Expired() bool {
	if a.Delegated {
		return false // the CLI owns the credential and its expiry
	}
	return a.Token.ExpiresAt != 0 && nowMS()+ExpiryBuffer.Milliseconds() >= a.Token.ExpiresAt
}

// apply folds a provider's token response into the account. An absent
// refresh token means "keep the old one", never "clear it".
func (a *Account) apply(t *Token) {
	a.Delegated = t.Delegated
	a.Token.Access = t.Access
	if t.Refresh != "" {
		a.Token.Refresh = t.Refresh
	}
	if t.ExpiresAt > 0 {
		a.Token.ExpiresAt = t.ExpiresAt
	}
	if len(t.Scopes) > 0 {
		a.Token.Scopes = t.Scopes
	}
	if id := t.Identity; id != nil {
		if id.UUID != "" {
			a.UUID = id.UUID
		}
		if id.Email != "" {
			a.Email = id.Email
		}
		if id.Org != "" {
			a.Org = id.Org
		}
	}
	for k, v := range t.Extra {
		a.setExtra(k, v)
	}
	a.Dead = false
}

func (a *Account) setExtra(k, v string) {
	if a.Extra == nil {
		a.Extra = map[string]string{}
	}
	a.Extra[k] = v
}

// NowMS is the clock rota stamps its records with: unix milliseconds.
// Exported so a store writing those fields uses the same one.
func NowMS() int64 { return nowMS() }

// Now is the clock everything in this package reads — expiry, staleness,
// record stamps. A variable so an application (or a test) can supply its
// own; the SDK takes no other time.
var Now = time.Now

func nowMS() int64 { return Now().UnixMilli() }

// stagedNone marks a staged credential file as belonging to a login that has
// since been replaced, so nothing in it is ever adopted.
const stagedNone = "-"

// CheckProject refuses a project setting that would go wrong quietly.
//
// Both directories are resolved by whatever started the process, so a
// relative one means a different place depending on where rota was launched
// from — and for a server, an unpredictable place to keep credentials. The
// config directory is also where a credential file is written, so it must
// not be the project itself: a token in a repository is a token in a commit.
func (a *Account) CheckProject() error {
	for _, d := range []struct{ what, path string }{
		{"config_dir", a.ConfigDir},
		{"cwd", a.Cwd},
	} {
		if d.path != "" && !filepath.IsAbs(d.path) {
			return failf(ErrInvalidRequest, "%s must be an absolute path, got %q", d.what, d.path)
		}
	}
	if a.ConfigDir != "" && a.Cwd != "" && filepath.Clean(a.ConfigDir) == filepath.Clean(a.Cwd) {
		return failf(ErrInvalidRequest,
			"config_dir is where this account's credential file is written, so it must not be its working directory (%s)", a.Cwd)
	}
	return nil
}

// Percent is the account's headline usage: the fullest window that covers
// the whole account. Scoped windows are left out — they cover one model, and
// nobody knows which model the next session will use, so a spent one is not
// a reason to move on.
//
// An account nobody has a reading for reports 0. That is the useful answer
// rather than the cautious one: an unmetered provider publishes no usage at
// all, and treating "unknown" as "spent" would take every such account out
// of whatever is deciding.
func (a *Account) Percent() float64 {
	var top float64
	if a.Quota == nil {
		return 0
	}
	for _, w := range a.Quota.Windows {
		if !w.Scoped && w.Percent > top {
			top = w.Percent
		}
	}
	return top
}
