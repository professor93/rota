package rota

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
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
	// Long marks a login for a long-lived credential rather than the usual
	// one. It is remembered here because the two halves of a login are
	// often two processes: whoever finishes it reads this rather than being
	// told again, and cannot finish a long login as an ordinary one by
	// forgetting to say so.
	Long bool `json:"long,omitzero"`
	// CreatedAt is unix ms, for callers that expire old logins.
	CreatedAt int64 `json:"createdAt"`
}

// Begin starts a login with a provider ("" for DefaultProvider). It returns
// the URL the user must approve and the state Complete needs.
func Begin(ctx context.Context, provider string) (*Login, error) {
	return begin(ctx, provider, false)
}

// BeginLong starts a login for a long-lived credential — a second key to an
// account that is already logged in, for processes that outlive an ordinary
// token. A provider that issues no such token is refused by name.
//
// It is a second function rather than an argument to Begin because Begin is
// published API, and a signature is somebody else's build.
func BeginLong(ctx context.Context, provider string) (*Login, error) {
	return begin(ctx, provider, true)
}

func begin(ctx context.Context, provider string, long bool) (*Login, error) {
	p, err := Lookup(provider)
	if err != nil {
		return nil, err
	}
	var (
		url   string
		state map[string]string
	)
	if long {
		lp, ok := p.(LongLived)
		if !ok {
			return nil, failf(ErrInvalidRequest, "%s issues no long-lived token", p.Name())
		}
		url, state, err = lp.BeginLong(ctx)
	} else {
		url, state, err = p.Begin(ctx)
	}
	if err != nil {
		return nil, err
	}
	l := &Login{ID: randID(), Provider: p.Name(), URL: url, Kind: "code", State: state, Long: long, CreatedAt: nowMS()}
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
	t, err := l.exchange(ctx, p, code)
	if err != nil {
		return nil, err
	}
	if t.Access == "" && !t.Delegated {
		return nil, failf(ErrInvalidRequest, "%s returned no access token", l.Provider)
	}
	// A long token is never asked whose it is. It is issued for inference
	// and nothing else, so the profile endpoint would refuse it; the
	// identity the exchange itself carried is the only one there is, and
	// the caller decides what to do when there is none.
	if t.Identity == nil && !l.Long {
		if ip, ok := p.(Identifier); ok {
			t.Identity, _ = ip.Identify(ctx, t.Access) // a nicety, never required
		}
	}
	return t, nil
}

func (l *Login) exchange(ctx context.Context, p Provider, code string) (*Token, error) {
	if !l.Long {
		return p.Complete(ctx, trimSpace(code), l.State)
	}
	lp, ok := p.(LongLived)
	if !ok {
		return nil, failf(ErrInvalidRequest, "%s issues no long-lived token", l.Provider)
	}
	return lp.CompleteLong(ctx, trimSpace(code), l.State)
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
		a.Dead, a.DeadReason = true, p.Name()+" cannot refresh a credential"
		return true, failf(ErrReauth, "%s: credential expired and %s cannot refresh one", a, p.Name())
	case a.Token.Refresh == "":
		a.Dead, a.DeadReason = true, "no refresh token"
		return true, failf(ErrReauth, "%s: no refresh token", a)
	}
	t, err := r.Refresh(ctx, a)
	switch {
	case errors.Is(err, ErrDeadToken):
		// The provider's own refusal, kept: it is the only record of which
		// of the few causes ended this lineage.
		a.Dead, a.DeadReason = true, err.Error()
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
	if err := launchable(a); err != nil {
		return nil, err
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
	cmd, err := p.Launch(a, home)
	if err != nil {
		return nil, err
	}
	return identify(a, cmd), nil
}

// launchable refuses an account nothing can be launched with.
//
// A dead lineage is normally exactly that: the refresh token is spent and
// only a fresh login revives it. A long-lived token is the one exception,
// and a narrow one — it is a credential of its own, owing nothing to the
// lineage that died, so the CLI starts and the provider bills the right
// account. What stays lost is everything the ordinary login gives: usage, a
// place in the rotation, the account's name. An application that lets such a
// run happen should say so; refusing it outright would be refusing something
// that works.
func launchable(a *Account) error {
	if a.Dead && a.LongAccess() == "" {
		return WrapReauth(a)
	}
	return nil
}

// identify tells the child which account it is running as.
//
// The credential alone does not say. Claude Code, for one, takes the token
// from the environment and bills the right account — but its own display,
// and every status line or hook that reads its shared ~/.claude.json, reports
// the e-mail of whoever last signed in through the keychain, which is a
// different account entirely. Nothing else in the child's world contradicts
// that. These three variables are the truthful answer: the provider, the
// account's id in the caller's store, and the label a person recognises.
// Anything that wants to name the account should read them and not the
// vendor's config file.
//
// They are appended after the provider's own variables — a caller that pins
// Env[0] to the credential keeps it — and they are set for every provider,
// not only the ones whose CLI is known to be confused, because a status line
// should not have to ask which vendor it is looking at.
//
// The command comes back as a copy, because a provider is free to hand out
// the same value twice — an Env built once and returned from every Launch is
// a reasonable thing to write — and staging twice must not leave the second
// child with two of each.
func identify(a *Account, cmd *Command) *Command {
	if cmd == nil {
		return nil
	}
	env := make([]string, 0, len(cmd.Env)+3)
	env = append(env, cmd.Env...)
	env = append(env,
		"ROTA_PROVIDER="+a.Provider,
		"ROTA_ACCOUNT_ID="+strconv.Itoa(a.ID),
		"ROTA_ACCOUNT="+a.Label(),
	)
	out := *cmd
	out.Env = env
	return &out
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
	if err := launchable(a); err != nil {
		return nil, nil, err
	}
	p, err := Lookup(a.Provider)
	if err != nil {
		return nil, nil, err
	}
	if pl, ok := p.(Planner); ok {
		cmd, files, err := pl.Plan(ctx, a, home)
		if err != nil {
			return nil, nil, err
		}
		return identify(a, cmd), files, nil
	}
	cmd, err := p.Launch(a, home)
	if err != nil {
		return nil, nil, err
	}
	return identify(a, cmd), nil, nil
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
