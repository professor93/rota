package rota

// Account collections. These are plain functions over a slice because rota
// does not own where accounts live: an application may keep them in a file,
// a database, or a request body, and still wants the two rules that are
// genuinely rota's — how an identity matches an existing account, and that
// an id is never reused.

// MatchIdentity finds the account an identity belongs to, so re-authing the
// same account updates it in place instead of piling up duplicates.
//
// Identity is only ever compared within one provider: two providers can hand
// out the same email address and they are still two separate accounts.
func MatchIdentity(accounts []*Account, provider string, id *Identity) *Account {
	if id == nil {
		return nil
	}
	for _, a := range accounts {
		if a.Provider != provider {
			continue
		}
		if id.UUID != "" && a.UUID == id.UUID {
			return a
		}
		if id.UUID == "" && id.Email != "" && a.Email == id.Email {
			return a
		}
	}
	return nil
}

// FindID returns the account with this id, or nil.
func FindID(accounts []*Account, id int) *Account {
	for _, a := range accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// StagedSuperseded marks an account's staged credential file as belonging to
// a login that has been replaced, so a later run never adopts a token from
// it. A fresh login sets this.
func (a *Account) StagedSuperseded() { a.Staged = stagedNone }

// Apply folds a provider's token response into the account: a new access
// token, a rotated refresh token, a name, whatever extra state the provider
// asked to keep. An absent refresh token means "keep the old one", never
// "clear it".
func (a *Account) Apply(t *Token) { a.apply(t) }
