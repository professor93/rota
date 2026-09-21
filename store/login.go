package store

import (
	"context"
	"os"
	"path/filepath"
	"time"

	rota "github.com/professor93/rota/lib"
)

// This file is the persistence half of logging in. The verbs themselves —
// rota.Begin and Login.Complete — need no storage at all; a caller that keeps
// its own accounts uses those directly and never touches a Store.

// pendingTTL bounds how long a half-finished login is kept; the
// authorization code dies server-side within minutes anyway.
const pendingTTL = 15 * time.Minute

// BeginLogin starts a login with a provider ("" for DefaultProvider) and
// parks its state so a later FinishLogin — in another process — can pick it
// up by id.
func (s *Store) BeginLogin(ctx context.Context, provider string) (*rota.Login, error) {
	return s.park(rota.Begin(ctx, provider))
}

// BeginLongLogin starts a login for a long-lived credential and parks it the
// same way. FinishLogin finishes either: which one this is was decided here
// and is remembered in the parked login, so the person finishing it types
// the same thing in both cases.
func (s *Store) BeginLongLogin(ctx context.Context, provider string) (*rota.Login, error) {
	return s.park(rota.BeginLong(ctx, provider))
}

func (s *Store) park(l *rota.Login, err error) (*rota.Login, error) {
	if err != nil {
		return nil, err
	}
	m, err := s.loadPendings()
	if err != nil {
		return nil, err
	}
	m[l.ID] = l
	return l, s.savePendings(m)
}

// PendingLogin is the parked login with this id, or nil when there is none.
//
// It is a look rather than a step: the one thing a caller needs to know
// before finishing a login is which kind it is, so that it can say what
// happened afterwards in the right words.
func (s *Store) PendingLogin(id string) (*rota.Login, error) {
	m, err := s.loadPendings()
	if err != nil {
		return nil, err
	}
	return m[id], nil
}

// FinishLogin completes the parked login with this id, adds or updates the
// account it names, and saves the store. added reports whether a new
// account was created.
//
// A rejected code leaves the parked login alone, so a typo costs one retry
// rather than a whole login; ErrAuthPending is passed through unchanged.
func (s *Store) FinishLogin(ctx context.Context, id, code string) (a *rota.Account, added bool, err error) {
	m, err := s.loadPendings()
	if err != nil {
		return nil, false, err
	}
	l := m[id]
	if l == nil {
		return nil, false, rota.WrapNoLogin(id)
	}
	tok, err := l.Complete(ctx, code)
	if err != nil {
		return nil, false, err
	}
	if l.Long {
		a, err := s.attachLong(l, tok)
		if err != nil {
			return nil, false, err
		}
		delete(m, id)
		return a, false, s.savePendings(m)
	}
	a = rota.MatchIdentity(s.Accounts, l.Provider, tok.Identity)
	if a == nil {
		a = s.add(l.Provider)
		added = true
		// A home left by an older account must never be adopted by this one.
		if err := os.RemoveAll(s.Home(a)); err != nil {
			return nil, false, err
		}
	}
	a.Apply(tok)
	a.Quota, a.QuotaAt = nil, 0
	a.StagedSuperseded() // whatever is staged belongs to the previous login
	if err := s.Save(); err != nil {
		return nil, false, err
	}
	delete(m, id)
	return a, added, s.savePendings(m)
}

// attachLong puts a long-lived token on the account whose identity approved
// it, and never anywhere else.
//
// It creates nothing. A year-long credential is the most valuable thing rota
// ever writes down, and the account it belongs to is not a guess: the
// exchange says who approved, and either that is an account already logged
// in here or rota does not know whose token this is. The ordinary login is
// still what an account is made by — it is the one that can read a profile,
// a quota and a name.
//
// The parked login is left alone on a refusal, as it is for a rejected code:
// approving again as the right account and pasting the new code finishes the
// same login, which is exactly what somebody who approved as the wrong one
// wants to do next.
func (s *Store) attachLong(l *rota.Login, tok *rota.Token) (*rota.Account, error) {
	if tok.Identity == nil {
		return nil, rota.Invalid("the %s approval named no account, so there is no telling whose long-lived token this is; nothing was stored", l.Provider)
	}
	a := rota.MatchIdentity(s.Accounts, l.Provider, tok.Identity)
	if a == nil {
		return nil, rota.Invalid("the long-lived token was approved as %s, which is not a %s account rota holds; log it in first with `rota login %s`, then ask for the long token again",
			identityName(tok.Identity), l.Provider, l.Provider)
	}
	a.ApplyLong(tok)
	return a, s.Save()
}

// identityName is whoever approved, in the words a person would recognise:
// the e-mail when the provider sent one, the uuid when it did not.
func identityName(id *rota.Identity) string {
	if id.Email != "" {
		return id.Email
	}
	return id.UUID
}

// pendingPath is where half-finished logins are parked. They are short-lived
// scratch state, so they live beside the private homes rather than inside
// the backend's own blob.
func (s *Store) pendingPath() string {
	return filepath.Join(s.backend.HomeRoot(), "pending.json")
}

// loadPendings reads the parked logins, dropping any past their TTL. A
// corrupt file is treated as empty: nothing in it is worth more than a
// fresh login.
func (s *Store) loadPendings() (map[string]*rota.Login, error) {
	m := map[string]*rota.Login{}
	raw, err := os.ReadFile(s.pendingPath())
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if rota.UnmarshalLenient(raw, &m) != nil {
		return map[string]*rota.Login{}, nil
	}
	for id, l := range m {
		if l == nil || time.Since(time.UnixMilli(l.CreatedAt)) > pendingTTL {
			delete(m, id)
		}
	}
	return m, nil
}

func (s *Store) savePendings(m map[string]*rota.Login) error {
	if len(m) == 0 {
		err := os.Remove(s.pendingPath())
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	raw, err := rota.EncodeIndent(m)
	if err != nil {
		return err
	}
	return writeAtomic(s.pendingPath(), append(raw, '\n'))
}
