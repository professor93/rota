package rota

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// This file is rota's core: values in, values out. Nothing here reads or
// writes rota's own storage — a caller keeps accounts wherever it likes (a
// file, a database, a request body) and hands them in.
//
// Two things still touch the world, unavoidably:
//   - the network, because logging in, refreshing and reading quota are
//     calls to the provider;
//   - staged credential files, because some vendor CLIs read credentials
//     only from a file (see Stage).
//
// A store — rota ships one outside this module — is an optional layer over these verbs.

// Login is a login in flight. Everything Complete needs is inside it, so a
// caller can park it anywhere — a session, a row, a JSON file — and hand it
// back later.
type Login struct {
	ID       string            `json:"id"`
	Provider string            `json:"provider"`
	URL      string            `json:"url"`
	Kind     string            `json:"kind"` // code | device | apikey | delegated
	State    map[string]string `json:"state"`
	// Delegated reports that this provider can also register an account
	// rota holds no credential for, signed in by the vendor CLI itself
	// inside the private directory rota gives it. Such a login is finished
	// with no code at all.
	Delegated bool `json:"delegated,omitzero"`
	// CreatedAt is unix ms, for callers that expire old logins.
	CreatedAt int64 `json:"createdAt"`
}

// Begin starts a login with a provider ("" for DefaultProvider). It returns
// the URL the user must approve and the state Complete needs.
func Begin(ctx context.Context, provider string) (*Login, error) {
	p, err := Lookup(provider)
	if err != nil {
		return nil, err
	}
	url, state, err := p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	l := &Login{ID: randID(), Provider: p.Name(), URL: url, Kind: "code", State: state, CreatedAt: nowMS()}
	if k := state["kind"]; k != "" {
		l.Kind = k
	}
	_, l.Delegated = p.(Delegator)
	return l, nil
}

// Complete exchanges what the user pasted (nothing, for device flows) for a
// token, and names the account when the provider can. ErrAuthPending means
// the login has not been approved yet: ask again with the same Login.
func (l *Login) Complete(ctx context.Context, code string) (*Token, error) {
	p, err := Lookup(l.Provider)
	if err != nil {
		return nil, err
	}
	t, err := p.Complete(ctx, trimSpace(code), l.State)
	if err != nil {
		return nil, err
	}
	if t.Access == "" && !t.Delegated {
		return nil, failf(ErrInvalidRequest, "%s returned no access token", l.Provider)
	}
	if t.Identity == nil {
		if ip, ok := p.(Identifier); ok {
			t.Identity, _ = ip.Identify(ctx, t.Access) // a nicety, never required
		}
	}
	return t, nil
}

// NewAccount builds an account from a finished login. The id is the
// caller's to assign — rota never invents one here.
func NewAccount(id int, provider string, t *Token) *Account {
	a := &Account{ID: id, Provider: provider, Staged: stagedNone}
	a.apply(t)
	return a
}

// Refresh rotates an account's access token in memory and reports whether
// anything changed — true even on a permanent failure, which marks the
// account dead. The caller decides whether to persist it, and must, because
// several providers reject a refresh token once it has been used.
func Refresh(ctx context.Context, a *Account) (changed bool, err error) {
	if !a.Expired() {
		return false, nil
	}
	p, err := Lookup(a.Provider)
	if err != nil {
		return false, err
	}
	r, ok := p.(Refresher)
	switch {
	case !ok:
		a.Dead = true
		return true, failf(ErrReauth, "%s: credential expired and %s cannot refresh one", a, p.Name())
	case a.Token.Refresh == "":
		a.Dead = true
		return true, failf(ErrReauth, "%s: no refresh token", a)
	}
	t, err := r.Refresh(ctx, a)
	switch {
	case errors.Is(err, ErrDeadToken):
		a.Dead = true
		return true, failf(ErrReauth, "%s: session expired", a)
	case err != nil:
		// Transient: leave the account alone so the next attempt retries.
		return false, fmt.Errorf("%s: refresh failed: %w", a, err)
	case t.Access == "":
		return false, fmt.Errorf("%s: refresh returned no access token", a)
	}
	a.apply(t)
	return true, nil
}

// Usage reads what an account has left, or (nil, nil) when its provider
// publishes no usage endpoint. It does not cache; a polling caller keeps its own reading.
func Usage(ctx context.Context, a *Account) (*Quota, error) {
	p, err := Lookup(a.Provider)
	if err != nil {
		return nil, err
	}
	m, ok := p.(Meter)
	if !ok {
		return nil, nil
	}
	return m.Quota(ctx, a.Token.Access)
}

// Metered reports whether a provider publishes a usage endpoint at all.
func Metered(provider string) bool {
	p, err := Lookup(provider)
	if err != nil {
		return false
	}
	_, ok := p.(Meter)
	return ok
}

// Stage prepares an account to run its CLI and returns how to start it.
//
// This is the one core verb that touches the filesystem, because some CLIs
// take credentials only as a file: home is a directory private to this
// account where such a file is written (0600). Providers that pass their
// credential in the environment — claude among them — write nothing, and
// home may be "".
//
// Stage may also change the account: it adopts a refresh token the CLI
// rotated on its own, and may refresh once to repair a stale record. The
// caller must persist the account afterwards, or that rotation is lost and
// the lineage dies.
//
// A provider that writes a credential file refuses an empty home rather
// than falling back to the working directory, which would scatter live
// tokens wherever the process happened to be started.
func Stage(a *Account, home string) (*Command, error) {
	if a.Dead {
		return nil, failf(ErrReauth, "%s: log in again", a)
	}
	p, err := Lookup(a.Provider)
	if err != nil {
		return nil, err
	}
	if home != "" {
		if err := os.MkdirAll(home, 0o700); err != nil {
			return nil, err
		}
	}
	return p.Launch(a, home)
}

// OwnsCredentials reports whether the provider's CLI, not rota, owns the
// credential file in an account's home.
//
// Two runs on such an account are two processes each believing the home is
// theirs: the second staging overwrites the token the first has already
// rotated to, and for these providers a spent refresh token is refused for
// good. A caller that keeps accounts must not let those overlap.
//
// Two kinds of provider answer yes, and it is worth saying why both do rather
// than only the first, which is what this used to ask. A provider that adopts
// does so precisely because its CLI rewrites that file as it goes. A provider
// that delegates hands the CLI the whole login: the credential it obtains
// lives in that home and nowhere else, and it is rewritten on every rotation.
// Kimi is the second kind without being the first — rota holds no token of its
// own there, so there is nothing to adopt — and its access token lasts fifteen
// minutes, which makes it the provider whose file is rewritten most often.
func OwnsCredentials(provider string) bool {
	p, err := Lookup(provider)
	if err != nil {
		return false
	}
	if _, ok := p.(Adopter); ok {
		return true
	}
	_, ok := p.(Delegator)
	return ok
}

// Adopter is implemented by a provider whose CLI keeps its credentials in a
// file that it rewrites as it goes.
type Adopter interface {
	// Adopt reads back what the CLI left in home and folds anything newer
	// into the account.
	Adopt(a *Account, home string) error
}

// Adopt takes back whatever the vendor CLI rotated during its last run.
//
// It must happen before any refresh. These CLIs rotate a refresh token in
// place, and the providers reject a reused one permanently — so refreshing
// from rota's copy while a newer one sits unread in the CLI's own file is
// how an account dies. The caller persists the account afterwards.
//
// For a delegated account there is no token to take back, but there is still
// something worth reading: who the CLI signed in as, so the account has a
// name rather than a random handle.
func Adopt(a *Account, home string) error {
	if home == "" {
		return nil
	}
	p, err := Lookup(a.Provider)
	if err != nil {
		return err
	}
	if ad, ok := p.(Adopter); ok {
		return ad.Adopt(a, home)
	}
	return nil
}

// StagedFile is one credential file as a value: where it belongs relative
// to the home, the mode it must carry, and its content. Returning files
// instead of writing them is how an application keeps storage decisions —
// where a file lives, what writes it, when — entirely on its own side.
type StagedFile struct {
	Path    string
	Mode    fs.FileMode
	Content []byte
}

// Planner is implemented by a provider whose CLI reads credentials from a
// file, for callers that want the file as a value rather than a write.
// home still shapes the command (the CLI's private-home variable points
// there), but Plan itself must touch no disk.
type Planner interface {
	Plan(ctx context.Context, a *Account, home string) (*Command, []StagedFile, error)
}

// StagePlan is Stage without the disk: how to start the CLI, plus the
// credential files as values for the application to store its own way.
// Nothing is written, created, or read from home.
//
// Adoption is the caller's step first — AdoptFrom, or Adopt for a local
// home — exactly as it is before Stage. A provider that stages nothing
// returns its command and no files.
func StagePlan(ctx context.Context, a *Account, home string) (*Command, []StagedFile, error) {
	if a.Dead {
		return nil, nil, failf(ErrReauth, "%s: log in again", a)
	}
	p, err := Lookup(a.Provider)
	if err != nil {
		return nil, nil, err
	}
	if pl, ok := p.(Planner); ok {
		return pl.Plan(ctx, a, home)
	}
	cmd, err := p.Launch(a, home)
	return cmd, nil, err
}

// FSAdopter is Adopter through a filesystem value, for applications whose
// account homes are not directories on this machine's disk.
type FSAdopter interface {
	AdoptFS(a *Account, fsys fs.FS) error
}

// AdoptFrom is Adopt reading through fsys instead of a local path: the
// caller hands lib the home's contents, wherever they actually live.
func AdoptFrom(a *Account, fsys fs.FS) error {
	p, err := Lookup(a.Provider)
	if err != nil {
		return err
	}
	if ad, ok := p.(FSAdopter); ok {
		return ad.AdoptFS(a, fsys)
	}
	return nil
}
