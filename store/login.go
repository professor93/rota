package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

// BeginLoginFor starts an ordinary login for the account with this id and
// parks it the same way, with the account's id on it (rota.Login.Account).
// FinishLogin then signs in that account or nothing: see boundAccount.
//
// It is for an account whose login rota holds, a dead one above all. What has
// no sign-in to start is refused here, before anybody is sent to a browser: an
// API key, a provider whose credential is a pasted key, a CLI that signs itself
// in, and an account with neither a uuid nor an e-mail, which no approval could
// be matched to. An account with an e-mail and no uuid is held to that e-mail.
// An account unknown here is rota.ErrNoAccount.
func (s *Store) BeginLoginFor(ctx context.Context, id int) (*rota.Login, error) {
	a := s.Find(id)
	if a == nil {
		return nil, rota.WrapNoAccount(id)
	}
	switch {
	case a.APIKey():
		return nil, rota.Invalid("account %d runs on an API key, so it has no sign-in to start; `rota login --key` takes a key: "+
			"this one again is this account, and a different one makes an account of its own", a.ID)
	case a.Delegated:
		return nil, signsItselfIn(a)
	case a.UUID == "" && a.Email == "":
		return nil, rota.Invalid("rota knows account %d by no uuid or e-mail, so no approval could be matched to it; "+
			"`rota login %s` signs in whoever approves, as an account of its own, and `rota remove %d` drops this one",
			a.ID, a.Provider, a.ID)
	}
	l, err := rota.Begin(ctx, a.Provider)
	if err != nil {
		return nil, err
	}
	switch l.Kind {
	case "apikey":
		return nil, rota.Invalid("account %d runs on a %s key, so it has no sign-in to start; `rota login %s` takes a key: "+
			"this one again is this account, and a different one makes an account of its own", a.ID, a.Provider, a.Provider)
	case "delegated":
		return nil, signsItselfIn(a)
	}
	l.Account = a.ID
	return s.park(l, nil)
}

func signsItselfIn(a *rota.Account) error {
	return rota.Invalid("account %d signs in through its own CLI, so rota has no sign-in to start for it", a.ID)
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
//
// A login begun for one account (BeginLoginFor) updates that account or
// nothing, however the approval went: see boundAccount.
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
	if a, err = s.boundAccount(l, tok, a); err != nil {
		return nil, false, err
	}
	if a == nil {
		a = s.add(l.Provider)
		added = true
		// A home left by an older account must never be adopted by this one.
		if err := os.RemoveAll(s.Home(a)); err != nil {
			return nil, false, err
		}
	}
	a.Apply(tok)
	forgetRefusals(a) // kept about the login this one replaces
	// The reading, and any wait asked of it, belonged to the login this one
	// replaces.
	a.Quota, a.QuotaAt, a.QuotaWait = nil, 0, rota.QuotaWait{}
	a.StagedSuperseded() // whatever is staged belongs to the previous login
	if err := s.Save(); err != nil {
		return nil, false, err
	}
	delete(m, id)
	return a, added, s.savePendings(m)
}

// boundAccount is the account a finished login signs in, given the account
// its approval resolved to by itself (approved, nil for none). A login that
// names no account takes that answer as it stands. One begun for an account
// signs in that account or is refused.
//
// Several accounts in one browser make approving as the wrong one the likely
// mistake, and taking it would leave the account that was asked for dead while
// the command looked done: the other account updated, or a new one made. So a
// refusal writes nothing and makes nothing.
//
// The parked login is left alone, as attachLong leaves it and for the same
// reason: approving again as the right account and pasting the new code
// finishes the same login.
//
// An account that has an e-mail and no uuid has nothing else to be held to, so
// its e-mail is enough here, and the approval gives it its uuid. MatchIdentity
// does not do that, and must not for a login that names nobody: there the
// e-mail would decide whose account a stranger's login lands on.
func (s *Store) boundAccount(l *rota.Login, tok *rota.Token, approved *rota.Account) (*rota.Account, error) {
	if l.Account == 0 {
		return approved, nil
	}
	named := s.Find(l.Account)
	if named == nil {
		return nil, rota.Invalid("%s account %d, which this login was begun for, is no longer here; nothing was stored, and `rota login %s` starts a login of its own",
			l.Provider, l.Account, l.Provider)
	}
	if approved == named {
		return named, nil
	}
	id := tok.Identity
	sameEmail := id != nil && id.Email != "" && id.Email == named.Email
	if sameEmail && approved == nil && named.UUID == "" {
		return named, nil
	}
	name := named.Email
	if name == "" {
		name = named.Label()
	}
	if id == nil || (id.UUID == "" && id.Email == "") {
		return nil, rota.Invalid("the %s approval named no account, so there is no telling whether it was account %d (%s); nothing was stored; %s",
			l.Provider, named.ID, name, approveAgain(l, name))
	}
	if sameEmail {
		// Approving again would only be told this again.
		out := fmt.Sprintf("`rota login %s` makes it an account of its own", l.Provider)
		if approved != nil {
			out = fmt.Sprintf("that approval is account %d's", approved.ID)
		}
		return nil, rota.Invalid("this login is for account %d, and the e-mail %s matches, but the provider's account id differs: %s here, %s approved; nothing was stored; %s, and `rota remove %d` drops the old one",
			named.ID, name, idOrNone(named.UUID), idOrNone(id.UUID), out, named.ID)
	}
	if approved != nil {
		return nil, rota.Invalid("this login is for account %d (%s), but it was approved as %s, which is account %d; nothing was stored; %s",
			named.ID, name, identityName(id), approved.ID, approveAgain(l, name))
	}
	return nil, rota.Invalid("this login is for account %d (%s), but it was approved as %s, which is not a %s account rota holds; nothing was stored and no account was added; %s",
		named.ID, name, identityName(id), l.Provider, approveAgain(l, name))
}

// approveAgain is the next step after a refused approval, for this kind of
// login: the page is said again because the first time may be long scrolled
// away, and a device login has no code to paste.
func approveAgain(l *rota.Login, name string) string {
	if l.Kind == "device" {
		return fmt.Sprintf("approve again as %s at %s, then run it again: rota login %s", name, l.URL, l.ID)
	}
	return fmt.Sprintf("approve again as %s at %s, then finish it with the new code: rota login %s <code>", name, l.URL, l.ID)
}

// idOrNone is a provider's account id in a sentence, where nothing at all
// would read as a mistake in the sentence.
func idOrNone(uuid string) string {
	if uuid == "" {
		return "none"
	}
	return uuid
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

// LoginKey adds or updates the account an API key belongs to, and saves the
// store. Nothing is parked: there is no second step. added reports whether a
// new account was made, and unverified why the key could not be checked,
// when it could not — it is stored all the same (rota.Paste).
//
// A new key account starts out of the rotation, at order 0: every run on it
// is paid for, so it runs only when somebody names it, until somebody puts it
// in. Pasting the same key again is the same account, and changes nothing
// anybody chose about it; a label renames it.
func (s *Store) LoginKey(ctx context.Context, provider, key, label string) (a *rota.Account, added bool, unverified, err error) {
	label = strings.TrimSpace(label)
	if err := checkLabel(label); err != nil {
		return nil, false, nil, err
	}
	tok, unverified, err := rota.Paste(ctx, provider, key)
	if err != nil {
		return nil, false, nil, err
	}
	if tok.Credential != pastedKey {
		return nil, false, nil, rota.Invalid("that is a token from `claude setup-token`, not a Console API key: it is a " +
			"claude.ai login's long-lived token, and goes on an account already here with `rota login <id> --token`")
	}
	if provider == "" {
		provider = rota.DefaultProvider
	}
	a = rota.MatchIdentity(s.Accounts, provider, tok.Identity)
	if a == nil {
		a = s.add(provider)
		added = true
		a.Order = 0
		// Order 0 is a decision from here on, which a store that has never
		// been ordered would otherwise reverse the first time it is opened.
		s.Ordered = true
		// A home left by an older account must never be this one's.
		if err := os.RemoveAll(s.Home(a)); err != nil {
			return nil, false, nil, err
		}
	}
	a.Apply(tok)
	forgetRefusals(a) // kept about the login this one replaces
	// The reading, and any wait asked of it, belonged to the login this one
	// replaces.
	a.Quota, a.QuotaAt, a.QuotaWait = nil, 0, rota.QuotaWait{}
	if label != "" {
		a.Name = label
	}
	return a, added, unverified, s.Save()
}

// checkLabel refuses a name that would not read as one: a line of its own,
// short enough for a table.
func checkLabel(label string) error {
	if utf8.RuneCountInString(label) > 64 || strings.ContainsFunc(label, unicode.IsControl) {
		return rota.Invalid("a label is one line of at most 64 characters")
	}
	return nil
}

// PastedLongCaveat is what a person is told when a pasted token is stored, in
// one line: nothing about such a token can be checked.
const PastedLongCaveat = "rota cannot tell which claude.ai account this token belongs to or when it was made: " +
	"one from another account would bill that account, and its year is counted from now."

// PasteLong puts a long-lived token somebody pasted — `claude setup-token`
// printed it — on the account with this id, as `rota login --long` would, and
// saves the store. It creates nothing: such a token cannot say whose it is,
// so it goes where it is put and nowhere else. An account that runs on an API
// key takes none: the token is a claude.ai login's, and the key is billed to
// somebody else. Nothing pasted here leaves the machine: an API key is
// refused by its shape, unasked about (rota.PasteLong).
func (s *Store) PasteLong(_ context.Context, id int, token string) (*rota.Account, error) {
	a := s.Find(id)
	if a == nil {
		return nil, rota.WrapNoAccount(id)
	}
	if a.APIKey() {
		return nil, rota.Invalid("%s runs on an API key; a token from `claude setup-token` belongs on the claude.ai account it was made for", a)
	}
	tok, err := rota.PasteLong(a.Provider, token)
	if err != nil {
		return nil, err
	}
	a.ApplyLong(tok)
	return a, s.Save()
}

// pastedKey is what rota.Paste names an API key in Token.Credential.
const pastedKey = "apikey"

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
	return filepath.Join(s.homeRoot, "pending.json")
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
