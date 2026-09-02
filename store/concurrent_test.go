package store

import (
	"context"
	"errors"
	"github.com/professor93/rota/internal/fakecli"
	"os"
	"path/filepath"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// Two runs on one account, for a provider whose CLI owns its credential file,
// must not overlap.
//
// codex and grok hand the CLI a private home and let it rewrite auth.json as
// it goes, rotating the refresh token in place. Two runs on the same account
// therefore share one file that two processes each believe is theirs: the
// second staging overwrites the token the first CLI has already rotated to,
// and the next adoption reads back a spent one. These providers refuse a spent
// refresh token for good, so the account does not recover.
//
// rota cannot make that safe — the CLIs assume the home is theirs — so it
// refuses the second run. The first run's lock is taken here directly, which
// is what a run in another process looks like from this one.
func TestASecondRunOnACredentialOwningAccountIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-owns-creds","token":{"accessToken":"tok","refreshToken":"r1"}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := s.Find(1)

	// Somebody else is running this account.
	home := s.Home(a)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	held, ok, err := tryLockFile(filepath.Join(home, runLock))
	if err != nil || !ok {
		t.Fatalf("could not stand in for a run in flight: %v %v", ok, err)
	}
	defer held.Close()

	_, err = s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil)
	if !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("a second run must be refused rather than allowed to spend the same token: %v", err)
	}
	// And the refusal has to say which account and why, since the caller's
	// only sensible response is to wait or to name another one.
	if err == nil || !contains(err.Error(), "refresh token") {
		t.Fatalf("the refusal must say what it is protecting: %v", err)
	}

	// Once the other run is over, the account is free again.
	held.Close()
	_, err = s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil)
	if errors.Is(err, rota.ErrBusy) {
		t.Fatalf("a finished run must not keep the account locked: %v", err)
	}
}

// An account whose credential rota holds has nothing shared to protect: the
// token reaches the CLI in its environment, and two runs touch nothing in
// common. Refusing those would cost the rotation its whole point.
func TestARunIsNotRefusedWhenRotaHoldsTheCredential(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-rota-holds","token":{"accessToken":"tok"}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := s.Find(1)

	// Even with the same file locked, this provider is not held back by it.
	home := s.Home(a)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	held, ok, err := tryLockFile(filepath.Join(home, runLock))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer held.Close()

	// It fails later, for a reason of its own — this fake has no modelled
	// command line — but never for being busy.
	if _, err := s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil); errors.Is(err, rota.ErrBusy) {
		t.Fatalf("refused with nothing to protect: %v", err)
	}
}

// Every provider whose CLI keeps a credential file in the private home is
// claimed, and the guard is keyed off that rather than off adoption.
//
// The two are not the same question, which is what this had wrong. A provider
// adopts because rota wants something back out of that file; kimi's CLI owns
// and rotates its credentials exactly as codex's does, and rota adopts nothing
// from it only because it holds no token of its own to update. Keying the
// guard on adoption left kimi — whose access token lasts fifteen minutes, so
// whose CLI rewrites that file constantly — running two at a time in one home.
func TestOwnsCredentialsNamesTheProvidersWhoseCLIRewritesTheFile(t *testing.T) {
	for _, provider := range []string{"codex", "grok", "kimi"} {
		if !rota.OwnsCredentials(provider) {
			t.Fatalf("%s hands its CLI a private home and lets it rotate in place", provider)
		}
	}
	// rota holds Claude Code's token and passes it in the environment, so
	// nothing in the home is shared.
	if rota.OwnsCredentials("claude") {
		t.Fatal("claude's credential does not live in a file the CLI rewrites")
	}
	if rota.OwnsCredentials("nonesuch") {
		t.Fatal("an unknown provider owns nothing")
	}
}

func writeAccounts(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ownsCreds stands in for codex and grok: its CLI is handed a private home and
// rewrites the credential file in it as it goes. It counts the refreshes it is
// asked for, because the point of the guard is that some do not happen.
type ownsCreds struct {
	*fakeProvider
	refreshes *int
}

func (ownsCreds) Adopt(a *rota.Account, home string) error { return nil }

func (o ownsCreds) Refresh(context.Context, *rota.Account) (*rota.Token, error) {
	if o.refreshes != nil {
		*o.refreshes++
	}
	return &rota.Token{Access: "new", Refresh: "r-new", ExpiresAt: rota.NowMS() + 3_600_000}, nil
}

func (ownsCreds) Quota(context.Context, string) (*rota.Quota, error) { return &rota.Quota{}, nil }

// ordersCalls records the sequence rota asks for, so a test can watch the one
// ordering that decides whether an account survives.
type ordersCalls struct {
	*fakeProvider
	calls *[]string
}

func (o ordersCalls) Adopt(*rota.Account, string) error {
	*o.calls = append(*o.calls, "adopt")
	return nil
}

func (o ordersCalls) Refresh(context.Context, *rota.Account) (*rota.Token, error) {
	*o.calls = append(*o.calls, "refresh")
	return &rota.Token{Access: "new", Refresh: "r-new", ExpiresAt: rota.NowMS() + 3_600_000}, nil
}

var (
	countedRefreshes int
	orderedCalls     []string
)

func init() {
	rota.Register(ownsCreds{fakeProvider: &fakeProvider{
		name:     "t-owns-creds",
		launched: &rota.Command{Bin: "t-cli"},
	}, refreshes: &countedRefreshes})
	rota.Register(&fakeProvider{
		name:     "t-rota-holds",
		launched: &rota.Command{Bin: "t-cli"},
	})
	rota.Register(ordersCalls{fakeProvider: &fakeProvider{
		name:     "t-orders",
		launched: &rota.Command{Bin: "t-cli"},
	}, calls: &orderedCalls})
}

// expiredAccount opens a store holding one account of a provider whose CLI
// owns the credential file, with a token old enough that a refresh is due.
// Both callers below need a refresh to actually happen: rota.Refresh returns
// without asking the provider anything while a token is still good.
func expiredAccount(t *testing.T, provider string) (*Store, *rota.Account) {
	t.Helper()
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"`+provider+
		`","token":{"accessToken":"old","refreshToken":"r-old","expiresAt":1}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, s.Find(1)
}

// Adoption must happen before any refresh, on every path that does both.
//
// The vendor CLI rotates the refresh token inside its own home as it runs. If
// rota refreshes from its own older copy before reading back what the CLI
// wrote, it presents a token that is already spent — and codex and kimi reject
// a reused refresh token permanently, so the account does not come back.
//
// lib has a test that adoption picks up a rotated token, but it calls Adopt
// itself: it proves the mechanism and cannot see a caller doing the two in the
// wrong order. These watch the callers. Swapping the two statements in either
// Run or Prepare must fail here, and did when it was tried.
func TestARunAdoptsBeforeItRefreshes(t *testing.T) {
	orderedCalls = nil
	s, a := expiredAccount(t, "t-orders")
	// It fails afterwards, for a reason of its own — this fake models no
	// command line — long after the order has been decided.
	_, _ = s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil)
	assertAdoptedFirst(t, "a run")
}

func TestTheHandoverAdoptsBeforeItRefreshes(t *testing.T) {
	orderedCalls = nil
	s, a := expiredAccount(t, "t-orders")
	// Fails at the missing binary, again after the order is settled.
	_, _, release, _ := s.Prepare(context.Background(), a)
	if release != nil {
		release()
	}
	assertAdoptedFirst(t, "the handover")
}

func assertAdoptedFirst(t *testing.T, who string) {
	t.Helper()
	if len(orderedCalls) != 2 || orderedCalls[0] != "adopt" || orderedCalls[1] != "refresh" {
		t.Fatalf("%s must read back what the CLI rotated before presenting rota's own copy, got %v",
			who, orderedCalls)
	}
}

// The background maintenance must leave a running account alone.
//
// Maintain adopts and refreshes every account, and the server runs it every
// two minutes. For a provider whose CLI owns the credential file, refreshing
// while that CLI is running rotates the token underneath it: the provider
// invalidates the copy the CLI is still holding, and the next thing the CLI
// does with it is refused for good. The comment in Maintain has always said
// that presenting an older copy kills a lineage; this is the same death from
// the other side.
func TestMaintenanceLeavesARunningAccountAlone(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-owns-creds","token":{"accessToken":"tok","refreshToken":"r1","expiresAt":1}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := s.Find(1)

	home := s.Home(a)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	held, ok, err := tryLockFile(filepath.Join(home, runLock))
	if err != nil || !ok {
		t.Fatal(err)
	}

	// An expired token is exactly what maintenance exists to renew, so this
	// account would certainly be refreshed if anything were going to.
	countedRefreshes = 0
	s.Maintain(context.Background())
	if countedRefreshes != 0 {
		t.Fatalf("a running account must not be refreshed underneath its CLI: %d refreshes", countedRefreshes)
	}

	// Once the run is over it is maintained again, or the guard would be a
	// way to stop an account being renewed at all.
	held.Close()
	countedRefreshes = 0
	s.Maintain(context.Background())
	if countedRefreshes == 0 {
		t.Fatal("an idle account must still be maintained")
	}
}

// The same for the usage refresh, which also rotates a token on its way to
// reading a quota.
func TestUsageRefreshLeavesARunningAccountAlone(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-owns-creds","token":{"accessToken":"tok","refreshToken":"r1","expiresAt":1}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := s.Find(1)

	home := s.Home(a)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	held, ok, err := tryLockFile(filepath.Join(home, runLock))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer held.Close()

	countedRefreshes = 0
	s.Refresh(context.Background(), true, a)
	if countedRefreshes != 0 {
		t.Fatalf("a running account must not be refreshed for a usage reading: %d", countedRefreshes)
	}
}

// Busy is what the rotation asks so it can step past an account instead of
// handing out one that will only be refused.
func TestBusyIsOnlyTrueForACredentialOwningAccountThatIsRunning(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[
		{"id":1,"provider":"t-owns-creds","token":{"accessToken":"a"}},
		{"id":2,"provider":"t-rota-holds","token":{"accessToken":"b"}}]}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	owns, holds := s.Find(1), s.Find(2)
	if s.Busy(owns) || s.Busy(holds) {
		t.Fatal("nothing is running yet")
	}
	for _, a := range []*rota.Account{owns, holds} {
		if err := os.MkdirAll(s.Home(a), 0o700); err != nil {
			t.Fatal(err)
		}
		held, ok, err := tryLockFile(filepath.Join(s.Home(a), runLock))
		if err != nil || !ok {
			t.Fatal(err)
		}
		defer held.Close()
	}
	if !s.Busy(owns) {
		t.Fatal("an account whose CLI owns its credential file is busy while it runs")
	}
	if s.Busy(holds) {
		t.Fatal("an account rota holds the credential for shares nothing, so it is never busy")
	}
}

// The interactive handover stages a credential exactly as a run does, and
// must claim the account the same way.
//
// `rota run 3` with no prompt adopts, refreshes and stages, then replaces
// rota with the CLI. Without the lock that is the same collision as two runs,
// reached by the one path that does not go through Run.
func TestPrepareClaimsTheAccountLikeARunDoes(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-owns-creds","token":{"accessToken":"tok","refreshToken":"r1"}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := s.Find(1)
	onPath(t, "t-cli")

	_, _, release, err := s.Prepare(context.Background(), a)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !s.Busy(a) {
		t.Fatal("a prepared account is claimed until its CLI is done")
	}
	release()
	if s.Busy(a) {
		t.Fatal("and released when the handover does not happen")
	}

	// A second handover while the first holds it is refused, not allowed to
	// stage over a live credential file.
	_, _, release, err = s.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, _, _, err := s.Prepare(context.Background(), a); !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("the second handover must be refused: %v", err)
	}
}

// Removing an account deletes the credential directory its CLI is using, so
// it waits rather than pulling the ground out from under a run.
func TestRemovingARunningAccountIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-owns-creds","token":{"accessToken":"tok"}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := s.Find(1)

	home := s.Home(a)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "auth.json")
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, ok, err := tryLockFile(filepath.Join(home, runLock))
	if err != nil || !ok {
		t.Fatal(err)
	}

	if err := s.Remove(1); !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("a running account must not be removed from under its CLI: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("and nothing of it may be deleted on the way to refusing: %v", err)
	}
	if s.Find(1) == nil {
		t.Fatal("nor may it be forgotten")
	}

	// Once the run is over it goes.
	held.Close()
	if err := s.Remove(1); err != nil {
		t.Fatal(err)
	}
	if s.Find(1) != nil {
		t.Fatal("an idle account is removed as before")
	}
}

// A login that lands on a new account clears whatever home is at its id
// first. That cannot be a live one, because a removed id is retired rather
// than reissued — which is the thing worth pinning, since the safety of that
// deletion rests on it entirely.
func TestARetiredIDIsNeverHandedOutAgain(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-rota-holds","token":{"accessToken":"a"}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Remove(1); err != nil {
		t.Fatal(err)
	}
	fresh := s.add("t-rota-holds")
	if fresh.ID == 1 {
		t.Fatal("a retired id must not come back: a new account would inherit the old one's home")
	}
	if s.Home(fresh) == filepath.Join(s.Backend().HomeRoot(), "t-rota-holds-1") {
		t.Fatalf("and with it the old home: %s", s.Home(fresh))
	}
}

// onPath puts a do-nothing executable of this name where LookPath will find
// it, for the parts of Prepare that come after the credential work.
func onPath(t *testing.T, name string) {
	t.Helper()
	bin := t.TempDir()
	fakecli.Install(t, bin, name, fakecli.Spec{})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
