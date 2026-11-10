package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/logins"
	rota "github.com/professor93/rota/lib"
)

// boundWorld is a store with two accounts on a provider of its own, the first
// of them dead, and a login begun for that first one.
type boundWorld struct {
	s            *Store
	p            *fakeProvider
	named, other *rota.Account
	login        *rota.Login
}

func newBoundWorld(t *testing.T, provider string) *boundWorld {
	t.Helper()
	return newBoundWorldOfKind(t, provider, "")
}

func newBoundWorldOfKind(t *testing.T, provider, kind string) *boundWorld {
	t.Helper()
	p := &fakeProvider{name: provider, kind: kind}
	rota.Register(p)
	s := openTemp(t)
	named := s.add(provider)
	named.UUID, named.Email, named.Dead, named.DeadReason = "u-named", "named@x", true, "its login ended"
	named.Token.Access, named.Token.Refresh = "old-access", "old-refresh"
	named.Quota = &rota.Quota{}
	other := s.add(provider)
	other.UUID, other.Email = "u-other", "other@x"
	other.Token.Access, other.Token.Refresh = "other-access", "other-refresh"
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	l, err := s.BeginLoginFor(context.Background(), named.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &boundWorld{s: s, p: p, named: named, other: other, login: l}
}

// saved is the accounts as they stand on disk.
func (w *boundWorld) saved(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(storeDir(w.s), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// refused finishes the world's login as this identity and insists it is turned
// down without one thing changing: no account touched or added, nothing saved,
// no credential in the words, and the parked login still there to finish. It
// returns the refusal, for what a test wants to find absent from it.
func (w *boundWorld) refused(t *testing.T, as *rota.Identity, words ...string) error {
	t.Helper()
	before, accounts, next := w.saved(t), len(w.s.Accounts), w.s.NextID
	namedAccess, otherAccess := w.named.Token.Access, w.other.Token.Access
	w.p.identity = as
	a, added, err := w.s.FinishLogin(context.Background(), w.login.ID, "SECRET-CODE")
	if err == nil || a != nil || added {
		t.Fatalf("finished: a=%+v added=%v err=%v", a, added, err)
	}
	if !errors.Is(err, rota.ErrInvalidRequest) {
		t.Fatalf("not rota.Invalid: %v", err)
	}
	for _, word := range words {
		if !strings.Contains(err.Error(), word) {
			t.Errorf("the refusal must say %q: %v", word, err)
		}
	}
	if strings.Contains(err.Error(), "SECRET-CODE") {
		t.Fatalf("a credential in the refusal: %v", err)
	}
	if after := w.saved(t); !bytes.Equal(before, after) {
		t.Fatalf("the store was written:\n%s\n%s", before, after)
	}
	if len(w.s.Accounts) != accounts || w.s.NextID != next {
		t.Fatalf("an account was made: %d %d", len(w.s.Accounts), w.s.NextID)
	}
	if w.named.Token.Access != namedAccess || !w.named.Dead || w.named.Quota == nil {
		t.Fatalf("the named account moved: %+v", w.named)
	}
	if w.other.Token.Access != otherAccess || w.other.Dead {
		t.Fatalf("the other account moved: %+v", w.other)
	}
	if l, err := w.s.PendingLogin(w.login.ID); err != nil || l == nil {
		t.Fatalf("the parked login must stay: %v %v", l, err)
	}
	return err
}

// What a login was begun for is kept beside it, in a file of the store's own;
// the parked login is the login and nothing else, and only a login begun for
// an account is bound to one.
func TestBeginLoginForKeepsTheAccountBesideTheLogin(t *testing.T) {
	w := newBoundWorld(t, "t-bound-park")
	if w.login.Provider != "t-bound-park" || w.login.Kind != "code" {
		t.Fatalf("%+v", w.login)
	}
	b, err := w.s.loadBound()
	if err != nil || b[w.login.ID] != w.named.ID {
		t.Fatalf("not bound to account %d: %v %v", w.named.ID, b, err)
	}
	raw, _ := os.ReadFile(w.s.pendingPath())
	if bytes.Contains(raw, []byte(`"account"`)) {
		t.Fatalf("the binding is in the parked login: %s", raw)
	}
	again, err := w.s.PendingLogin(w.login.ID)
	if err != nil || again == nil || !reflect.DeepEqual(again, w.login) {
		t.Fatalf("the login read back is not the login begun: %+v %v", again, err)
	}
	if _, err := w.s.BeginLoginFor(context.Background(), 99); !errors.Is(err, rota.ErrNoAccount) {
		t.Fatalf("no such account: %v", err)
	}
	l, err := w.s.BeginLogin(context.Background(), "t-bound-park")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := w.s.loadBound(); len(b) != 1 || b[l.ID] != 0 {
		t.Fatalf("an ordinary login is bound to nobody: %v", b)
	}

	// Finished, the binding goes with the login.
	w.p.identity = &rota.Identity{UUID: "u-named", Email: "named@x"}
	if _, _, err := w.s.FinishLogin(context.Background(), w.login.ID, "NEW"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.s.boundPath()); !os.IsNotExist(err) {
		t.Fatalf("the binding of a finished login is kept: %v", err)
	}
}

// A rota from before bound logins reads the parked logins and writes them
// back as the logins it knows. That cannot drop a binding, which is not in
// that file.
func TestABindingOutlivesAnOlderRotaRewritingTheParkedLogins(t *testing.T) {
	w := newBoundWorld(t, "t-bound-older")
	raw, err := os.ReadFile(w.s.pendingPath())
	if err != nil {
		t.Fatal(err)
	}
	var older map[string]*rota.Login // what an older rota decodes it into
	if err := json.Unmarshal(raw, &older); err != nil {
		t.Fatal(err)
	}
	again, _ := rota.EncodeIndent(older)
	if err := os.WriteFile(w.s.pendingPath(), append(again, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	w.refused(t, &rota.Identity{UUID: "u-new", Email: "stranger@x"}, "stranger@x", "account 1")
}

// A binding that cannot be read lets no login finish, rather than letting a
// login begun for one account be finished by any.
func TestABindingThatCannotBeReadLetsNothingFinish(t *testing.T) {
	w := newBoundWorld(t, "t-bound-torn")
	if err := os.WriteFile(w.s.boundPath(), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := w.saved(t)
	w.p.identity = &rota.Identity{UUID: "u-new", Email: "stranger@x"}
	_, added, err := w.s.FinishLogin(context.Background(), w.login.ID, "CODE")
	if err == nil || added || !strings.Contains(err.Error(), "pending-for.json") {
		t.Fatalf("added=%v err=%v", added, err)
	}
	if !bytes.Equal(before, w.saved(t)) || len(w.s.Accounts) != 2 {
		t.Fatalf("written: %s", w.saved(t))
	}
	if _, err := w.s.BeginLoginFor(context.Background(), w.named.ID); err == nil {
		t.Fatal("a binding written over one that cannot be read")
	}
}

func TestFinishLoginForTheNamedAccountUpdatesItInPlace(t *testing.T) {
	w := newBoundWorld(t, "t-bound-ok")
	w.p.identity = &rota.Identity{UUID: "u-named", Email: "named@x"}
	a, added, err := w.s.FinishLogin(context.Background(), w.login.ID, "NEW")
	if err != nil || added || a != w.named {
		t.Fatalf("a=%+v added=%v err=%v", a, added, err)
	}
	if a.Dead || a.DeadReason != "" || a.Token.Access != "NEW" || a.Token.Refresh != "r-NEW" || a.Quota != nil {
		t.Fatalf("not signed in again: %+v", a)
	}
	if len(w.s.Accounts) != 2 || w.other.Token.Access != "other-access" {
		t.Fatalf("somebody else moved: %+v", w.s.Accounts)
	}
	if l, _ := w.s.PendingLogin(w.login.ID); l != nil {
		t.Fatalf("the login was finished and is still parked: %+v", l)
	}
}

// Three accounts in one browser make approving as the wrong one the likely
// mistake, and it must not look done with the named account still dead.
func TestFinishLoginRefusesApprovalAsAnotherAccount(t *testing.T) {
	w := newBoundWorld(t, "t-bound-other")
	w.refused(t, &rota.Identity{UUID: "u-other", Email: "other@x"},
		"other@x", "account 2", "account 1", "named@x", w.login.ID)

	// The same login still finishes, approved again as the right one.
	w.p.identity = &rota.Identity{UUID: "u-named", Email: "named@x"}
	a, added, err := w.s.FinishLogin(context.Background(), w.login.ID, "RIGHT")
	if err != nil || added || a != w.named || a.Dead || a.Token.Access != "RIGHT" || w.other.Token.Access != "other-access" {
		t.Fatalf("retry: a=%+v added=%v err=%v", a, added, err)
	}
}

func TestFinishLoginRefusesApprovalByAnAccountRotaDoesNotHold(t *testing.T) {
	w := newBoundWorld(t, "t-bound-stranger")
	w.refused(t, &rota.Identity{UUID: "u-new", Email: "stranger@x"}, "stranger@x", "account 1", "named@x", "added")
}

func TestFinishLoginRefusesAnApprovalThatNamesNobody(t *testing.T) {
	w := newBoundWorld(t, "t-bound-nil")
	w.refused(t, nil, "named no account", "account 1", "named@x")
	w.refused(t, &rota.Identity{}, "named no account", "account 1", "named@x")
}

func TestFinishLoginRefusesWhenTheNamedAccountIsGone(t *testing.T) {
	w := newBoundWorld(t, "t-bound-gone")
	w.s.Accounts = []*rota.Account{w.other}
	if err := w.s.Save(); err != nil {
		t.Fatal(err)
	}
	before := w.saved(t)
	w.p.identity = &rota.Identity{UUID: "u-named", Email: "named@x"}
	_, added, err := w.s.FinishLogin(context.Background(), w.login.ID, "NEW")
	if !errors.Is(err, rota.ErrInvalidRequest) || added || !strings.Contains(err.Error(), "account 1") ||
		!strings.Contains(err.Error(), "no longer") {
		t.Fatalf("added=%v err=%v", added, err)
	}
	if !bytes.Equal(before, w.saved(t)) || len(w.s.Accounts) != 1 {
		t.Fatalf("written: %s", w.saved(t))
	}
	if !logins.Unfinishable(err) {
		t.Fatalf("no approval can finish a login for an account that is gone: %v", err)
	}
	if l, _ := w.s.PendingLogin(w.login.ID); l == nil {
		t.Fatal("the parked login must stay")
	}
}

// An account known by its e-mail alone is held to that e-mail, since no uuid
// could ever be compared with it, and it learns its uuid from the approval.
func TestFinishLoginTakesTheNamedEmailWhenTheAccountHasNoUUID(t *testing.T) {
	w := newBoundWorld(t, "t-bound-emailonly")
	w.named.UUID = ""
	if err := w.s.Save(); err != nil {
		t.Fatal(err)
	}
	w.refused(t, &rota.Identity{UUID: "u-new", Email: "stranger@x"}, "stranger@x", "account 1")

	w.p.identity = &rota.Identity{UUID: "u-learned", Email: "named@x"}
	a, added, err := w.s.FinishLogin(context.Background(), w.login.ID, "NEW")
	if err != nil || added || a != w.named || a.Dead || a.UUID != "u-learned" || a.Token.Access != "NEW" {
		t.Fatalf("a=%+v added=%v err=%v", a, added, err)
	}
	if len(w.s.Accounts) != 2 || w.s.NextID != 3 {
		t.Fatalf("an account was made: %+v", w.s.Accounts)
	}
}

// That is for the login that names the account. One that names nobody goes on
// matching as it always did, and an approval by an account that is here
// already is that account's, whatever its e-mail.
func TestTheEmailAloneCountsOnlyForTheNamedAccount(t *testing.T) {
	w := newBoundWorld(t, "t-bound-emailbare")
	w.named.UUID = ""
	w.other.Email = "named@x"
	if err := w.s.Save(); err != nil {
		t.Fatal(err)
	}
	w.refused(t, &rota.Identity{UUID: "u-other", Email: "named@x"}, "account 2", "account 1")

	l, err := w.s.BeginLogin(context.Background(), "t-bound-emailbare")
	if err != nil {
		t.Fatal(err)
	}
	w.p.identity = &rota.Identity{UUID: "u-third", Email: "named@x"}
	a, added, err := w.s.FinishLogin(context.Background(), l.ID, "BARE")
	if err != nil || !added || a == w.named || w.named.UUID != "" {
		t.Fatalf("a bare login must still make an account: a=%+v added=%v err=%v", a, added, err)
	}
}

// The same e-mail under another of the provider's ids is not the account, and
// approving again would only say so again: the refusal shows the two ids and
// the way out there really is.
func TestFinishLoginSaysSoWhenOnlyTheEmailMatches(t *testing.T) {
	w := newBoundWorld(t, "t-bound-recreated")
	err := w.refused(t, &rota.Identity{UUID: "u-recreated", Email: "named@x"},
		"named@x", "u-named", "u-recreated", "`rota login t-bound-recreated`", "`rota remove 1`")
	if strings.Contains(err.Error(), "approve again") {
		t.Fatalf("telling them to do again what cannot work: %v", err)
	}
	if !logins.Unfinishable(err) || !errors.Is(err, rota.ErrInvalidRequest) {
		t.Fatalf("a refusal approving again cannot put right: %v", err)
	}
}

// A refusal approving again can put right is not one that ends the login,
// and neither is anything else FinishLogin says.
func TestOnlyWhatNoApprovalCanFinishIsUnfinishable(t *testing.T) {
	w := newBoundWorld(t, "t-bound-finishable")
	for _, as := range []*rota.Identity{{UUID: "u-other", Email: "other@x"}, {UUID: "u-new", Email: "s@x"}, nil} {
		if err := w.refused(t, as); logins.Unfinishable(err) {
			t.Fatalf("approving again can finish this: %v", err)
		}
	}
	if _, _, err := w.s.FinishLogin(context.Background(), w.login.ID, "bad"); err == nil || logins.Unfinishable(err) {
		t.Fatalf("a rejected code: %v", err)
	}
	if _, _, err := w.s.FinishLogin(context.Background(), "zzzzzz", ""); logins.Unfinishable(err) || logins.Unfinishable(nil) {
		t.Fatalf("no such login: %v", err)
	}
}

// When the same e-mail is another account already, the way out is that
// account, and a login that would make one is not offered.
func TestFinishLoginNamesTheAccountThatHasTheSameEmail(t *testing.T) {
	w := newBoundWorld(t, "t-bound-twin")
	w.other.Email = "named@x"
	if err := w.s.Save(); err != nil {
		t.Fatal(err)
	}
	err := w.refused(t, &rota.Identity{UUID: "u-other", Email: "named@x"}, "u-named", "u-other", "account 2", "`rota remove 1`")
	if strings.Contains(err.Error(), "of its own") || strings.Contains(err.Error(), "approve again") {
		t.Fatalf("offers what would not happen: %v", err)
	}
	if !logins.Unfinishable(err) {
		t.Fatalf("a refusal approving again cannot put right: %v", err)
	}
}

// Whoever approved wrongly needs the page again, and a step that fits the
// login: a device login has no code to paste.
func TestTheRefusalGivesTheLinkAndTheStepForThisKindOfLogin(t *testing.T) {
	stranger := &rota.Identity{UUID: "u-new", Email: "stranger@x"}

	w := newBoundWorldOfKind(t, "t-bound-step", "")
	w.refused(t, stranger, w.login.URL, "rota login "+w.login.ID+" <code>", "new code")

	d := newBoundWorldOfKind(t, "t-bound-device", "device")
	if d.login.Kind != "device" {
		t.Fatalf("%+v", d.login)
	}
	err := d.refused(t, stranger, d.login.URL, "rota login "+d.login.ID)
	if strings.Contains(err.Error(), "<code>") || strings.Contains(err.Error(), "new code") {
		t.Fatalf("a device login has no code: %v", err)
	}
}

// Not everything an id can name has a sign-in to start, and the answer is
// given before anybody is sent to a browser for nothing.
func TestBeginLoginForRefusesWhatHasNoSignIn(t *testing.T) {
	rota.Register(&fakeProvider{name: "t-bound-none"})
	rota.Register(&fakeProvider{name: "t-bound-pasted", kind: "apikey"})
	rota.Register(&fakeProvider{name: "t-bound-own", kind: "delegated"})
	s := openTemp(t)
	acct := func(provider, uuid, credential string, delegated bool) *rota.Account {
		a := s.add(provider)
		a.UUID, a.Credential, a.Delegated = uuid, credential, delegated
		return a
	}
	for _, c := range []struct {
		name  string
		a     *rota.Account
		words []string
	}{
		{"api key", acct("t-bound-none", "key-1", "apikey", false), []string{"API key", "rota login --key", "an account of its own"}},
		{"a provider's pasted key", acct("t-bound-pasted", "key-2", "", false), []string{"key", "rota login t-bound-pasted"}},
		{"delegated", acct("t-bound-none", "d", "", true), []string{"its own CLI"}},
		{"a provider whose CLI signs in", acct("t-bound-own", "d2", "", false), []string{"its own CLI"}},
		{"nothing to know it by", acct("t-bound-none", "", "", false), []string{"rota login t-bound-none", "rota remove"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, err := s.BeginLoginFor(context.Background(), c.a.ID)
			if !errors.Is(err, rota.ErrInvalidRequest) || l != nil {
				t.Fatalf("l=%+v err=%v", l, err)
			}
			for _, word := range c.words {
				if !strings.Contains(err.Error(), word) {
					t.Errorf("must say %q: %v", word, err)
				}
			}
			if m, _ := s.loadPendings(); len(m) != 0 {
				t.Fatalf("parked anyway: %+v", m)
			}
		})
	}
}

// The remedy an account is left dead with is a command that does what it
// says: begin a login for that account, by the id the sentence gives.
func TestTheOlderCopyRemedyBeginsALogin(t *testing.T) {
	w := newBoundWorld(t, "t-bound-remedy")
	m := regexp.MustCompile(`rota login (\d+) makes a new login`).FindStringSubmatch(olderCopyWords(w.named))
	if m == nil {
		t.Fatal("the sentence no longer names the command")
	}
	id, _ := strconv.Atoi(m[1])
	l, err := w.s.BeginLoginFor(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := w.s.loadBound(); b[l.ID] != w.named.ID {
		t.Fatalf("not begun for account %d: %v", w.named.ID, b)
	}
}
