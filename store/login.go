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
	l, err := rota.Begin(ctx, provider)
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
