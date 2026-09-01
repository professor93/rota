package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// openTemp opens a file-backed store in a directory of its own.
func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// storeDir is where a file-backed test store keeps its bytes.
func storeDir(s *Store) string { return s.Backend().(*FileBackend).Dir }

// stagedNoneForTest mirrors the marker rota writes for a superseded staged
// credential file; the store must round-trip it unchanged.
const stagedNoneForTest = "-"

// memBackend is the smallest possible Backend: what a database
// implementation would look like, minus the database.
type memBackend struct {
	mu     sync.Mutex
	blob   []byte
	home   string
	loads  int
	saves  int
	locks  int
	failOn string
}

func (m *memBackend) Load() ([]byte, error) {
	m.loads++
	if m.failOn == "load" {
		return nil, errors.New("boom")
	}
	return m.blob, nil
}

func (m *memBackend) Save(b []byte) error {
	if m.failOn == "save" {
		return errors.New("boom")
	}
	m.saves++
	m.blob = append([]byte(nil), b...)
	return nil
}

func (m *memBackend) Lock() (func(), error) {
	if m.failOn == "lock" {
		return nil, errors.New("boom")
	}
	m.locks++
	m.mu.Lock()
	return m.mu.Unlock, nil
}

func (m *memBackend) HomeRoot() string { return m.home }

// fakeProvider stands in for a vendor: no network, no CLI.
type fakeProvider struct {
	name        string
	kind        string
	identity    *rota.Identity
	profile     *rota.Identity
	completeErr error
	refreshTok  *rota.Token
	refreshErr  error
	quota       *rota.Quota
	quotaErr    error
	quotaCalls  *int
	launched    *rota.Command
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Begin(_ context.Context) (string, map[string]string, error) {
	st := map[string]string{"verifier": "v"}
	if f.kind != "" {
		st["kind"] = f.kind
	}
	return "https://x/auth", st, nil
}

func (f *fakeProvider) Complete(_ context.Context, code string, st map[string]string) (*rota.Token, error) {
	if f.completeErr != nil {
		return nil, f.completeErr
	}
	if code == "bad" || st["verifier"] != "v" {
		return nil, errors.New("code rejected")
	}
	return &rota.Token{Access: code, Refresh: "r-" + code, Identity: f.identity,
		Extra: map[string]string{"seen": code}}, nil
}

func (f *fakeProvider) Launch(a *rota.Account, home string) (*rota.Command, error) {
	if f.launched != nil {
		return f.launched, nil
	}
	return &rota.Command{Bin: "true", Env: []string{"FAKE_TOKEN=" + a.Token.Access}}, nil
}

type fakeIdentifier struct{ *fakeProvider }

func (f fakeIdentifier) Identify(context.Context, string) (*rota.Identity, error) {
	return f.profile, nil
}

type fakeRefresher struct{ *fakeProvider }

func (f fakeRefresher) Refresh(context.Context, *rota.Account) (*rota.Token, error) {
	return f.refreshTok, f.refreshErr
}

type fakeMeter struct{ fakeRefresher }

func (f fakeMeter) Quota(context.Context, string) (*rota.Quota, error) {
	if f.quotaCalls != nil {
		*f.quotaCalls++
	}
	return f.quota, f.quotaErr
}

// win builds one quota window for a test.
func win(pct float64, resetIn time.Duration, primary, scoped bool) rota.Window {
	w := rota.Window{Name: "w", Percent: pct, Primary: primary, Scoped: scoped}
	if resetIn != 0 {
		w.ResetsAt = rota.When{Time: time.Now().Add(resetIn)}
	}
	return w
}
