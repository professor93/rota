// Package store keeps rota accounts somewhere: on disk by default, or in
// whatever a Backend puts them.
//
// It is outside lib, and it is entirely optional. The library takes accounts
// as values and returns accounts as values; it neither knows nor cares where
// they were before the call or where they go after it, which is what lets an
// application that already has a database ignore this package and call
// rota.Begin, rota.Refresh, rota.Run and the rest directly.
//
// What this package adds is the bookkeeping such an application would
// otherwise write itself: allocating ids that are never reused, matching a
// fresh login to the account it belongs to, holding a lock so two processes
// cannot overwrite each other's rotated tokens, and saving before a run
// starts rather than after it fails. rota's own command and HTTP server use
// it; anything else is free to.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
)

// Store is the on-disk account list, held under an exclusive lock from Open
// until Close so concurrent rota processes cannot overwrite each other's
// rotated tokens.
type Store struct {
	Accounts []*rota.Account `json:"accounts"`
	// NextID only grows, so a removed account's id — and its private home —
	// can never be inherited by a later one.
	NextID int `json:"nextId,omitzero"`
	// Ordered records that an application has decided this store's rotation
	// order. It is carried, not read: without it a store where every account
	// is deliberately out of the rotation cannot be told from one written
	// before rotation existed, and whoever backfills would undo that choice
	// on every load.
	Ordered bool `json:"ordered,omitzero"`

	backend  Backend
	unlock   func()
	released bool
}

func nowMS() int64 { return time.Now().UnixMilli() }

// Open loads the store kept in dir ("" for the default directory) and takes
// its lock. Callers must Close it. It is shorthand for NewStore over a
// FileBackend.
func Open(dir string) (*Store, error) {
	b, err := NewFileBackend(dir)
	if err != nil {
		return nil, err
	}
	return NewStore(b)
}

// NewStore locks a backend and loads what it holds. Callers must Close it,
// which releases the lock.
func NewStore(b Backend) (*Store, error) {
	unlock, err := b.Lock()
	if err != nil {
		return nil, err
	}
	s := &Store{backend: b, unlock: unlock}
	raw, err := b.Load()
	if err != nil {
		s.Close()
		return nil, err
	}
	if len(raw) > 0 {
		if err := rota.UnmarshalLenient(raw, s); err != nil {
			s.Close()
			return nil, fmt.Errorf("stored accounts are corrupt: %w", err)
		}
	}
	for _, a := range s.Accounts {
		if a.Provider == "" {
			a.Provider = rota.DefaultProvider
		}
		s.NextID = max(s.NextID, a.ID+1)
	}
	return s, nil
}

// Backend is where this store keeps its bytes.
func (s *Store) Backend() Backend { return s.backend }

// Home is the private directory reserved for one account's CLI. It is not
// created here.
func (s *Store) Home(a *rota.Account) string {
	// An account told where its configuration lives keeps everything there,
	// credentials included: for codex, grok and kimi this directory is the
	// CLI's whole home, and splitting the two would leave the CLI reading
	// one and rota writing the other.
	if a.ConfigDir != "" {
		return a.ConfigDir
	}
	return s.ownHome(a)
}

// ownHome is the directory rota reserves for an account under HomeRoot.
func (s *Store) ownHome(a *rota.Account) string {
	return filepath.Join(s.backend.HomeRoot(), a.Provider+"-"+strconv.Itoa(a.ID))
}

// owns reports whether an account's home is rota's own to create and delete,
// rather than a directory the person chose.
func (s *Store) owns(a *rota.Account) bool {
	return a.ConfigDir == "" || realDir(a.ConfigDir) == realDir(s.ownHome(a))
}

// CheckHome refuses a ConfigDir that would put this account inside rota's
// own directories, and — given roots — one outside all of them.
//
// A run stages the account's credential into its ConfigDir, so a caller who
// could name a sibling's home would have that sibling's credential written
// over; and the store directory holds every account's refresh token, which
// is no place to hand a CLI as its home, nor to put inside one. The
// account's own default home is allowed, being the same place as no
// ConfigDir at all. Links are followed, so a path that merely points into
// these directories is refused too.
func (s *Store) CheckHome(a *rota.Account, roots ...string) error {
	if a.ConfigDir == "" {
		return nil
	}
	dir := realDir(a.ConfigDir)
	homes := realDir(s.backend.HomeRoot())
	if (within(homes, dir) && dir != realDir(s.ownHome(a))) || within(dir, filepath.Dir(homes)) {
		return rota.Invalid("config_dir %q: that directory is rota's own", a.ConfigDir)
	}
	if len(roots) == 0 {
		return nil
	}
	for _, root := range roots {
		if within(realDir(root), dir) {
			return nil
		}
	}
	return fmt.Errorf("%w: config_dir %q", rota.ErrOutsideRoots, a.ConfigDir)
}

// realDir is where a path leads once every link in it is followed, for as
// much of it as exists. A directory that is not there yet is still judged by
// where its nearest existing parent points, so a link into rota's homes
// cannot be named one component early — nor before the homes exist.
func realDir(p string) string { return resolveDir(p, 0) }

func resolveDir(p string, hops int) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		// A link to somewhere that does not exist yet is still a link. It is
		// followed up to a limit, because a link can lead back to itself.
		if target, err := os.Readlink(p); err == nil && hops < 40 {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(p), target)
			}
			return resolveDir(filepath.Join(target, rest), hops+1)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// within reports whether path is root or inside it, without being fooled by
// a shared name prefix.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Save writes the store through its backend.
func (s *Store) Save() error {
	if s.released {
		return errors.New("store: the lock was released; reopen before saving")
	}
	raw, err := rota.EncodeIndent(s)
	if err != nil {
		return err
	}
	return s.backend.Save(append(raw, '\n'))
}

// Close releases the lock. It is safe to call more than once.
func (s *Store) Close() error { return s.Release() }

// Release gives up the lock while keeping what was loaded readable.
//
// It is for the caller that has finished writing but is not finished
// working — running an agent for several minutes, say. Save after Release
// refuses rather than writing without the lock, because two processes
// writing this file is how a rotated refresh token gets lost.
func (s *Store) Release() error {
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
		s.released = true
	}
	return nil
}
