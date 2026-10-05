package store

import (
	"os"
	"slices"

	rota "github.com/professor93/rota/lib"
)

// Find returns the account with this id, or nil.
func (s *Store) Find(id int) *rota.Account { return rota.FindID(s.Accounts, id) }

// Remove forgets an account. The private home rota made for it goes too,
// staged credentials included; a directory the person chose as its
// ConfigDir holds their memory and skills and is left alone. Call Save
// afterwards.
//
// A claude account in a home rota made is more than its directory. Its
// daemon may be running there with no rota run in sight, hosting background
// sessions, and is stopped first, the home given a few seconds to go quiet;
// and on macOS its login lives in a keychain item named for the directory,
// which goes before the directory does — afterwards nothing would name it
// again.
func (s *Store) Remove(id int) error {
	for i, a := range s.Accounts {
		if a.ID != id {
			continue
		}
		// Not while a run is using it. Deleting the directory a running
		// agent authenticates from is worse than making somebody wait, and
		// nothing here is undoable once the files are gone.
		if err := s.Removable(a); err != nil {
			return err
		}
		if rota.SharedHome(a.Provider) && s.owns(a) {
			// Whatever still runs once the daemon is stopped, or a claim
			// still held, refuses the removal.
			h := s.claudeHome(a, false)
			if err := h.quiesce(); err != nil {
				return err
			}
			if s.claimed(a) {
				return errRunning(a)
			}
			if err := h.dropOwnLogin(); err != nil {
				return err
			}
		}
		if s.owns(a) {
			if err := os.RemoveAll(s.Home(a)); err != nil {
				return err
			}
		}
		// slices.Delete zeroes the vacated slot, so the removed account —
		// and the live refresh token it holds — is not left reachable from
		// the backing array.
		s.Accounts = slices.Delete(s.Accounts, i, i+1)
		// The id is retired, never handed out again: a later account must
		// not inherit this one's home.
		s.NextID = max(s.NextID, id+1)
		return nil
	}
	return rota.WrapNoAccount(id)
}

// add appends a fresh account with an id no earlier account has used.
func (s *Store) add(provider string) *rota.Account {
	id := nextID(s.Accounts, s.NextID)
	s.NextID = id + 1
	// A new account joins the end of the queue rather than sitting outside
	// it: somebody who adds a second account means it to be used. Numbering
	// it is the same bookkeeping as allocating its id — what the number
	// means, and which account gets picked, is the rotation package's
	// business rather than this one's.
	a := &rota.Account{ID: id, Provider: provider, Order: endOfQueue(s.Accounts)}
	s.Accounts = append(s.Accounts, a)
	return a
}

// endOfQueue is one past the last place anything holds.
func endOfQueue(accounts []*rota.Account) int {
	next := 1
	for _, a := range accounts {
		if a.Order >= next {
			next = a.Order + 1
		}
	}
	return next
}

// nextID is the id to give a new account: one past the highest ever used.
// Reusing a removed account's id would let a new account inherit the private
// directory — and the credentials — of the old one, so ids only ever grow.
// Pass the highest id previously handed out, which a store must remember
// even after that account is gone.
func nextID(accounts []*rota.Account, highestEverUsed int) int {
	next := max(highestEverUsed, 1)
	for _, a := range accounts {
		if a.ID >= next {
			next = a.ID + 1
		}
	}
	return next
}
