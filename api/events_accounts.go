package api

import "github.com/professor93/rota/store"

// accountWatch is what the account events are computed against: what the
// last sweep read of every account. It is empty until the account events
// are written; until then a sweep says nothing.
type accountWatch struct{}

// lookAtAccounts compares what a sweep has just read of the accounts with
// what the one before it read, and says what changed. The first look is a
// baseline and says nothing. It is called with the store a sweep has open.
func (s *Server) lookAtAccounts(st *store.Store) {}
