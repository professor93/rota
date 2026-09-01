package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

func TestStoreRunsOnAnyBackend(t *testing.T) {
	b := &memBackend{home: t.TempDir()}
	s, err := NewStore(b)
	if err != nil || b.locks != 1 || b.loads != 1 {
		t.Fatalf("open: %v locks=%d loads=%d", err, b.locks, b.loads)
	}
	a := s.add("codex")
	a.Token.Access = "tok"
	if err := s.Save(); err != nil || b.saves != 1 || !strings.Contains(string(b.blob), `"nextId": 2`) {
		t.Fatalf("save: %v saves=%d blob=%s", err, b.saves, b.blob)
	}
	if home := s.Home(a); !strings.HasPrefix(home, b.home) || !strings.HasSuffix(home, "codex-1") {
		t.Fatalf("home: %q", home)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewStore(b) // the lock was released, the blob survived
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if len(s2.Accounts) != 1 || s2.Accounts[0].Token.Access != "tok" {
		t.Fatalf("reload: %+v", s2.Accounts)
	}
}

func TestNewStoreReportsBackendFailures(t *testing.T) {
	for _, stage := range []string{"lock", "load"} {
		if _, err := NewStore(&memBackend{failOn: stage}); err == nil {
			t.Fatalf("%s failure must surface", stage)
		}
	}
	s, err := NewStore(&memBackend{failOn: "save"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Save(); err == nil {
		t.Fatal("save failure must surface")
	}
}

func TestQuotaIsRecheckedEveryFiveMinutes(t *testing.T) {
	calls := 0
	if QuotaTTL != 5*time.Minute {
		t.Fatalf("QuotaTTL is %v", QuotaTTL)
	}
	s := openTemp(t)
	p := &fakeProvider{name: "t-ttl-meter", quotaCalls: &calls, refreshTok: &rota.Token{Access: "a"}, quota: &rota.Quota{Windows: []rota.Window{{Name: "w", Percent: 1, Primary: true}}}}
	rota.Register(fakeMeter{fakeRefresher{p}})
	a := s.add("t-ttl-meter")
	a.Token.Access = "a"

	s.Refresh(context.Background(), false)
	if calls != 1 || a.QuotaAt == 0 {
		t.Fatalf("first read: calls=%d at=%d", calls, a.QuotaAt)
	}
	a.QuotaAt = nowMS() - int64(4*time.Minute/time.Millisecond)
	s.Refresh(context.Background(), false)
	if calls != 1 {
		t.Fatal("a reading four minutes old must still be served from cache")
	}
	a.QuotaAt = nowMS() - int64(6*time.Minute/time.Millisecond)
	s.Refresh(context.Background(), false)
	if calls != 2 {
		t.Fatal("a reading six minutes old must be re-fetched")
	}
}

func TestBeginLoginParksStateAndReportsKind(t *testing.T) {
	rota.Register(&fakeProvider{name: "t-code"})
	rota.Register(&fakeProvider{name: "t-device", kind: "device"})
	s := openTemp(t)
	l, err := s.BeginLogin(context.Background(), "t-code")
	if err != nil || len(l.ID) != 6 || l.URL != "https://x/auth" || l.Kind != "code" || l.Provider != "t-code" {
		t.Fatalf("login=%+v err=%v", l, err)
	}
	if l2, _ := s.BeginLogin(context.Background(), "t-device"); l2.Kind != "device" || l2.ID == l.ID {
		t.Fatalf("device login=%+v", l2)
	}
	fi, err := os.Stat(s.pendingPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pending file: %v %v", err, fi)
	}
	if _, err := s.BeginLogin(context.Background(), "nope"); err == nil {
		t.Fatal("unknown provider must fail")
	}
}

func TestFinishLoginAddsAccountAndWipesStaleHome(t *testing.T) {
	rota.Register(&fakeProvider{name: "t-add"})
	s := openTemp(t)
	stale := filepath.Join(storeDir(s), "homes", "t-add-1")
	os.MkdirAll(stale, 0o700)
	os.WriteFile(filepath.Join(stale, "auth.json"), []byte("old"), 0o600)
	l, _ := s.BeginLogin(context.Background(), "t-add")
	a, added, err := s.FinishLogin(context.Background(), l.ID, " CODE1 ")
	if err != nil || !added || a.ID != 1 || a.Token.Access != "CODE1" || a.Token.Refresh != "r-CODE1" ||
		a.Extra["seen"] != "CODE1" || a.Staged != stagedNoneForTest {
		t.Fatalf("a=%+v added=%v err=%v", a, added, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale home must be wiped for a brand-new account")
	}
	if _, _, err := s.FinishLogin(context.Background(), l.ID, "CODE1"); !errors.Is(err, rota.ErrNoLogin) {
		t.Fatalf("pending must be consumed: %v", err)
	}
	s.Close()
	s2, _ := Open(storeDir(s))
	defer s2.Close()
	if s2.Find(1) == nil {
		t.Fatal("FinishLogin must persist the store")
	}
}

func TestFinishLoginReauthUpdatesInPlace(t *testing.T) {
	rota.Register(&fakeProvider{name: "t-re", identity: &rota.Identity{UUID: "u1", Email: "e@x"}})
	s := openTemp(t)
	old := s.add("t-re")
	old.UUID, old.Dead, old.Staged = "u1", true, "abcd"
	old.Quota = &rota.Quota{}
	l, _ := s.BeginLogin(context.Background(), "t-re")
	a, added, err := s.FinishLogin(context.Background(), l.ID, "NEW")
	if err != nil || added || a != old || a.Token.Access != "NEW" || a.Dead || a.Quota != nil ||
		a.Staged != stagedNoneForTest || a.Email != "e@x" || len(s.Accounts) != 1 {
		t.Fatalf("a=%+v added=%v err=%v", a, added, err)
	}
}

func TestFinishLoginUsesProfileWhenTokenNamesNobody(t *testing.T) {
	rota.Register(fakeIdentifier{&fakeProvider{name: "t-prof", profile: &rota.Identity{Email: "p@x"}}})
	s := openTemp(t)
	l, _ := s.BeginLogin(context.Background(), "t-prof")
	if a, _, err := s.FinishLogin(context.Background(), l.ID, "C"); err != nil || a.Email != "p@x" {
		t.Fatalf("a=%+v err=%v", a, err)
	}
}

func TestFinishLoginKeepsPendingOnRejectionAndPendingVerdict(t *testing.T) {
	rota.Register(&fakeProvider{name: "t-bad"})
	s := openTemp(t)
	l, _ := s.BeginLogin(context.Background(), "t-bad")
	if _, _, err := s.FinishLogin(context.Background(), l.ID, "bad"); err == nil || errors.Is(err, rota.ErrNoLogin) {
		t.Fatalf("rejected code: %v", err)
	}
	if _, _, err := s.FinishLogin(context.Background(), l.ID, "ok"); err != nil {
		t.Fatalf("retry after typo must work: %v", err)
	}
	rota.Register(&fakeProvider{name: "t-pend", completeErr: rota.ErrAuthPending})
	l, _ = s.BeginLogin(context.Background(), "t-pend")
	if _, _, err := s.FinishLogin(context.Background(), l.ID, ""); !errors.Is(err, rota.ErrAuthPending) {
		t.Fatalf("pending verdict must pass through: %v", err)
	}
	if _, _, err := s.FinishLogin(context.Background(), l.ID, ""); !errors.Is(err, rota.ErrAuthPending) {
		t.Fatalf("pending entry must survive a pending verdict: %v", err)
	}
}

func TestPendingLoginsExpireAndCorruptFileStartsOver(t *testing.T) {
	rota.Register(&fakeProvider{name: "t-ttl"})
	s := openTemp(t)
	l, _ := s.BeginLogin(context.Background(), "t-ttl")
	m, _ := s.loadPendings()
	m[l.ID].CreatedAt = time.Now().Add(-pendingTTL - time.Minute).UnixMilli()
	s.savePendings(m)
	if _, _, err := s.FinishLogin(context.Background(), l.ID, "x"); !errors.Is(err, rota.ErrNoLogin) {
		t.Fatalf("expired login must be gone: %v", err)
	}
	os.WriteFile(s.pendingPath(), []byte("garbage"), 0o600)
	if l, err := s.BeginLogin(context.Background(), "t-ttl"); err != nil || l.ID == "" {
		t.Fatalf("corrupt pending file must not block logins: %v", err)
	}
	if _, _, err := s.FinishLogin(context.Background(), "zzzzzz", ""); !errors.Is(err, rota.ErrNoLogin) || !strings.Contains(err.Error(), "zzzzzz") {
		t.Fatalf("unknown id: %v", err)
	}
}
func TestRefreshTouchesOnlyMeteredProvidersAndCachesQuota(t *testing.T) {
	calls := 0
	s := openTemp(t)
	metered := &fakeProvider{name: "t-meter", quotaCalls: &calls, refreshTok: &rota.Token{Access: "fresh", ExpiresAt: nowMS() + 3_600_000},
		quota: &rota.Quota{Windows: []rota.Window{win(7, time.Hour, true, false)}}}
	rota.Register(fakeMeter{fakeRefresher{metered}})
	plain := &fakeProvider{name: "t-plain", refreshTok: &rota.Token{Access: "fresh"}}
	rota.Register(fakeRefresher{plain})
	m := s.add("t-meter")
	m.Token = rota.Token{Access: "stale", Refresh: "r", ExpiresAt: 1}
	p := s.add("t-plain")
	p.Token = rota.Token{Access: "stale", Refresh: "r", ExpiresAt: 1}
	dead := s.add("t-meter")
	dead.Token, dead.Dead = rota.Token{Access: "x", Refresh: "r", ExpiresAt: 1}, true

	if errs := s.Refresh(context.Background(), false); len(errs) != 0 {
		t.Fatalf("errs=%v", errs)
	}
	if m.Token.Access != "fresh" || m.Quota == nil || m.Quota.Windows[0].Percent != 7 || m.QuotaAt == 0 {
		t.Fatalf("metered account not refreshed: %+v", m)
	}
	if p.Token.Access != "stale" || p.Quota != nil {
		t.Fatalf("unmetered account must not be touched by list-style refresh: %+v", p)
	}
	if dead.Token.Access != "x" || calls != 1 {
		t.Fatalf("dead account must be skipped; quotaCalls=%d", calls)
	}
	s.Refresh(context.Background(), false)
	if calls != 1 {
		t.Fatal("quota inside TTL must be served from cache")
	}
	s.Refresh(context.Background(), true)
	if calls != 2 {
		t.Fatal("force must re-fetch")
	}
	metered.quotaErr = errors.New("429")
	if errs := s.Refresh(context.Background(), true, m); len(errs) != 1 || !strings.Contains(errs[0].Error(), "quota") || m.Quota == nil {
		t.Fatalf("quota failure must be reported and keep the old reading: %v", errs)
	}
	s.Close()
	s2, _ := Open(storeDir(s))
	defer s2.Close()
	if s2.Find(m.ID).Quota == nil || s2.Find(m.ID).Token.Access != "fresh" {
		t.Fatal("rota.Refresh must persist what changed")
	}
}
func TestPrepareRefreshesStagesRecordsAndSaves(t *testing.T) {
	s := openTemp(t)
	rota.Register(fakeRefresher{&fakeProvider{name: "t-run", refreshTok: &rota.Token{Access: "fresh", ExpiresAt: nowMS() + 3_600_000}}})
	a := s.add("t-run")
	a.Token = rota.Token{Access: "stale", Refresh: "r", ExpiresAt: 1}
	path, env, _, err := s.Prepare(context.Background(), a)
	if err != nil || filepath.Base(path) != "true" || !slices.Contains(env, "FAKE_TOKEN=fresh") {
		t.Fatalf("path=%q err=%v env has token: %v", path, err, slices.Contains(env, "FAKE_TOKEN=fresh"))
	}
	s.Close()
	s2, _ := Open(storeDir(s))
	defer s2.Close()
	if b := s2.Find(a.ID); b.Token.Access != "fresh" {
		t.Fatal("Prepare must persist the rotated token before anything runs")
	}
}

func TestPrepareRefusesDeadAccountsAndPersistsTheVerdict(t *testing.T) {
	s := openTemp(t)
	rota.Register(fakeRefresher{&fakeProvider{name: "t-dead", refreshErr: rota.ErrDeadToken}})
	a := s.add("t-dead")
	a.Token = rota.Token{Access: "stale", Refresh: "r", ExpiresAt: 1}
	if _, _, _, err := s.Prepare(context.Background(), a); err == nil || !a.Dead {
		t.Fatalf("err=%v dead=%v", err, a.Dead)
	}
	s.Close()
	s2, _ := Open(storeDir(s))
	defer s2.Close()
	if !s2.Find(a.ID).Dead {
		t.Fatal("death not persisted")
	}
	if _, _, _, err := s2.Prepare(context.Background(), s2.Find(a.ID)); !errors.Is(err, rota.ErrReauth) {
		t.Fatalf("dead account must be refused up front: %v", err)
	}
}

func TestPrepareRefusesToRunWhenTheStoreCannotBeSaved(t *testing.T) {
	if os.Getuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs an unwritable directory")
	}
	s := openTemp(t)
	rota.Register(&fakeProvider{name: "t-nosave"})
	a := s.add("t-nosave")
	a.Token = rota.Token{Access: "key"}
	os.Chmod(storeDir(s), 0o500)
	defer os.Chmod(storeDir(s), 0o700)
	if _, _, _, err := s.Prepare(context.Background(), a); err == nil || !strings.Contains(err.Error(), "not saved") {
		t.Fatalf("err=%v", err)
	}
}

func TestPrepareReportsMissingBinary(t *testing.T) {
	s := openTemp(t)
	rota.Register(&fakeProvider{name: "t-nobin", launched: &rota.Command{Bin: "rota-no-such-binary-xyz"}})
	a := s.add("t-nobin")
	a.Token = rota.Token{Access: "key"}
	if _, _, _, err := s.Prepare(context.Background(), a); err == nil || !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("err=%v", err)
	}
}

// TestExecHandsTheProcessOver re-runs the test binary so Exec can replace it.

// TestReleaseFreesTheLockButKeepsWhatWasLoaded is the mechanism that stops
// one long agent run from queueing every other caller behind it: staging
// needs the lock, the run that follows does not.
func TestReleaseFreesTheLockButKeepsWhatWasLoaded(t *testing.T) {
	s := openTemp(t)
	a := s.add("claude")
	a.Token.Access = "tok"
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s.Release()

	// The accounts are still readable...
	if s.Find(a.ID) == nil || len(s.Accounts) != 1 {
		t.Fatal("releasing the lock must not lose what was loaded")
	}
	// ...but writing without the lock is refused rather than done unsafely.
	if err := s.Save(); err == nil {
		t.Fatal("saving after Release must be refused: two writers lose a rotated token")
	}
	// And another holder can take the lock immediately.
	done := make(chan struct{})
	go func() {
		unlock, err := s.Backend().Lock()
		if err == nil {
			unlock()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the lock was not released")
	}
	// Release is idempotent, and Close after it is harmless.
	s.Release()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
