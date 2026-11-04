package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// thePanic is what a launch made to panic panics with, so a test can tell the
// panic that reached it from any other.
var thePanic = errors.New("a launch panicking on purpose")

// panicSaves is a file backend whose next save panics once it is armed. Every
// launch saves the store after its claim is taken, whatever the account's CLI,
// so a save is where a launch is made to panic with its claim held.
type panicSaves struct {
	*FileBackend
	armed bool
}

func (p *panicSaves) Save(b []byte) error {
	if p.armed {
		p.armed = false
		panic(thePanic)
	}
	return p.FileBackend.Save(b)
}

func panickingStore(t *testing.T) (*Store, *panicSaves) {
	t.Helper()
	fb, err := NewFileBackend(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	ps := &panicSaves{FileBackend: fb}
	s, err := NewStore(ps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ps
}

// panicked runs do and returns what it panicked with, nil when it did not.
func panicked(do func()) (v any) {
	defer func() { v = recover() }()
	do()
	return nil
}

// stillHeld names a claim this process holds on the account's home, if any.
func stillHeld(s *Store, a *rota.Account) bool {
	mine := filepath.Join(s.Home(a), runLock)
	heldMu.Lock()
	defer heldMu.Unlock()
	for f := range heldClaims {
		if f.Name() == mine {
			return true
		}
	}
	return false
}

// launchKinds are the two claims a launch takes: shared for Claude Code, whose
// runs share a home, and exclusive for a CLI that keeps its credential file to
// one process at a time — codex, grok and kimi, stood in for here.
func launchKinds(t *testing.T) []struct {
	what string
	make func(s *Store) *rota.Account
} {
	claudeWorld(t)
	return []struct {
		what string
		make func(s *Store) *rota.Account
	}{
		{"a shared claude claim", func(s *Store) *rota.Account { return livingClaude(s, "u1") }},
		{"an exclusive claim", func(s *Store) *rota.Account {
			a := s.add("t-owns-creds")
			a.Token = rota.Token{Access: "tok", Refresh: "r1"}
			return a
		}},
	}
}

// A launch that panics with the account claimed lets the claim go on its way
// out, and the panic reaches the caller as it was. A server recovers a
// handler's panic and goes on serving, and an account left claimed by one
// would look busy until it restarted — and, for a CLI whose claim is
// exclusive, could not be run at all.
func TestAPanickingLaunchLetsGoOfTheAccount(t *testing.T) {
	launches := []struct {
		what string
		do   func(s *Store, a *rota.Account)
	}{
		{"prepare", func(s *Store, a *rota.Account) { _, _, _ = s.prepare(context.Background(), a, true) }},
		{"Prepare", func(s *Store, a *rota.Account) { _, _, _, _ = s.Prepare(context.Background(), a) }},
		{"Run", func(s *Store, a *rota.Account) {
			_, _ = s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil)
		}},
		{"Start", func(s *Store, a *rota.Account) {
			_, _ = s.Start(context.Background(), a, rota.Spec{Prompt: "hi", Input: true, Stream: true}, nil, nil)
		}},
	}
	for _, k := range launchKinds(t) {
		for _, l := range launches {
			t.Run(k.what+", "+l.what, func(t *testing.T) {
				s, ps := panickingStore(t)
				a := k.make(s)
				ps.armed = true
				if v := panicked(func() { l.do(s, a) }); v != thePanic {
					t.Fatalf("the launch's own panic reaches the caller, as it was: %v", v)
				}
				if s.claimed(a) || stillHeld(s, a) {
					t.Fatal("the account is let go of on the way out")
				}
				release, ok := s.Hold(a)
				release()
				if !ok {
					t.Fatal("so nothing is in the way of the next one")
				}
			})
		}
	}
}

// A launch that goes through keeps its claim until it is released, as ever:
// the CLI is about to run on the account.
func TestALaunchThatGoesThroughKeepsItsClaim(t *testing.T) {
	for _, k := range launchKinds(t) {
		t.Run(k.what, func(t *testing.T) {
			s, _ := panickingStore(t)
			a := k.make(s)
			_, release, err := s.prepare(context.Background(), a, true)
			if err != nil {
				t.Fatal(err)
			}
			if !s.claimed(a) || !stillHeld(s, a) {
				t.Fatal("held for the CLI")
			}
			release()
			if s.claimed(a) || stillHeld(s, a) {
				t.Fatal("and let go of when released")
			}
		})
	}
}

// panicsAdopting stands in for codex: its CLI owns its credential file, and
// reading that file back panics.
type panicsAdopting struct{ *fakeProvider }

func (panicsAdopting) Adopt(*rota.Account, string) error { panic(thePanic) }

func (panicsAdopting) Refresh(context.Context, *rota.Account) (*rota.Token, error) {
	return nil, errors.New("not asked")
}

func init() {
	rota.Register(panicsAdopting{&fakeProvider{name: "t-panics-adopting", launched: &rota.Command{Bin: "t-cli"}}})
}

// Maintenance claims an account whose CLI owns its credential file while it
// reads that file back, and a panic there lets the claim go too. A server's
// background sweep recovers its panics and goes on.
func TestAPanickingSweepLetsGoOfTheAccount(t *testing.T) {
	s := openTemp(t)
	a := s.add("t-panics-adopting")
	a.Token = rota.Token{Access: "tok", Refresh: "r1"}
	if v := panicked(func() { s.Maintain(context.Background()) }); v != thePanic {
		t.Fatalf("the panic reaches the caller, as it was: %v", v)
	}
	if s.claimed(a) || stillHeld(s, a) {
		t.Fatal("the account is let go of on the way out")
	}
}

// A write that panics before its new file is in place leaves no temporary copy
// of it behind: what is written there is a credential.
func TestAPanickingWriteLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	old := renameFile
	renameFile = func(string, string) error { panic(thePanic) }
	t.Cleanup(func() { renameFile = old })
	if v := panicked(func() { _ = writeAtomic(filepath.Join(dir, ".credentials.json"), []byte("secret")) }); v != thePanic {
		t.Fatalf("the panic reaches the caller: %v", v)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("a temporary file is left behind: %s", e.Name())
		}
	}
}
