package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// sessionBin is where the echoing fake lives for the test below. The provider
// is registered once, for the whole binary; the CLI it launches is installed
// per test, so the name has to be looked up rather than baked in.
var sessionBin string

// ownsSession is a provider whose CLI both owns its credential file and
// speaks Claude Code's vocabulary, which is the pair a session on a claimed
// account needs.
type ownsSession struct{ *fakeProvider }

func (ownsSession) Adopt(*rota.Account, string) error { return nil }
func (ownsSession) Flavor() string                    { return "claude" }

func (ownsSession) Launch(*rota.Account, string) (*rota.Command, error) {
	return &rota.Command{Bin: sessionBin}, nil
}

func init() {
	rota.Register(ownsSession{&fakeProvider{name: "t-owns-session"}})
}

// A session holds the account for as long as it is open, not until Start
// returns.
//
// Start returns while the CLI is still running, so releasing the claim there
// would leave the one thing the claim protects — a credential file the CLI is
// rewriting as it goes — open to the next run's staging. A second run is
// refused for as long as the session lives, and the account comes back the
// moment it is over.
func TestASessionHoldsTheAccountUntilItEnds(t *testing.T) {
	sessionBin = fakecli.Install(t, t.TempDir(), "claude", fakecli.Spec{Echo: true})
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-owns-session","token":{"accessToken":"tok"}}],"nextId":2}`)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := s.Find(1)

	sess, err := s.Start(context.Background(), a, rota.Spec{Prompt: "hi", Input: true, Stream: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil); !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("a run must not stage over the credential file an open session's CLI is using: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Wait(); err != nil {
		t.Fatal(err)
	}
	// The claim is let go as the session ends, which is a goroutine waking
	// rather than a value returned, so this waits for it instead of racing
	// it: what is pinned is that it goes, and promptly.
	for deadline := time.Now().Add(10 * time.Second); s.Busy(a); {
		if time.Now().After(deadline) {
			t.Fatal("a finished session must not keep the account claimed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.Run(context.Background(), a, rota.Spec{Prompt: "hi"}, nil, nil); errors.Is(err, rota.ErrBusy) {
		t.Fatalf("and a released account runs again: %v", err)
	}
}
