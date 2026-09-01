package rota

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeProvider is a configurable stand-in used across the test suite.
type fakeProvider struct {
	name        string
	kind        string
	identity    *Identity // carried in the token response
	completeErr error
	refreshTok  *Token
	refreshErr  error
	quota       *Quota
	quotaErr    error
	quotaCalls  int
	launched    *Command
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Begin(_ context.Context) (string, map[string]string, error) {
	st := map[string]string{"verifier": "v"}
	if f.kind != "" {
		st["kind"] = f.kind
	}
	return "https://x/auth", st, nil
}
func (f *fakeProvider) Complete(_ context.Context, code string, st map[string]string) (*Token, error) {
	if f.completeErr != nil || code == "bad" || st["verifier"] != "v" {
		if f.completeErr != nil {
			return nil, f.completeErr
		}
		return nil, errBadCode
	}
	return &Token{Access: code, Refresh: "r-" + code, Identity: f.identity, Extra: map[string]string{"seen": code}}, nil
}
func (f *fakeProvider) Launch(a *Account, home string) (*Command, error) {
	if f.launched != nil && f.launched.BaseEnv == nil {
		// Tests drive Run's nil-cmd path; a real application sets BaseEnv.
		f.launched.BaseEnv = []string{"PATH=/usr/bin:/bin"}
	}
	if f.launched != nil {
		return f.launched, nil
	}
	return &Command{Bin: "true", Env: []string{"FAKE_TOKEN=" + a.Token.Access}}, nil
}

type fakeRefresher struct{ *fakeProvider }

func (f fakeRefresher) Refresh(context.Context, *Account) (*Token, error) {
	return f.refreshTok, f.refreshErr
}

type fakeMeter struct{ fakeRefresher }

func (f fakeMeter) Quota(context.Context, string) (*Quota, error) {
	f.quotaCalls++
	return f.quota, f.quotaErr
}

var errBadCode = errors.New("code rejected")

func TestRegistryLookupAndNames(t *testing.T) {
	Register(&fakeProvider{name: "zz-fake"})
	Register(&fakeProvider{name: "aa-fake"})
	if p, err := Lookup("zz-fake"); err != nil || p.Name() != "zz-fake" {
		t.Fatal(err)
	}
	if _, err := Lookup("nope"); err == nil || !strings.Contains(err.Error(), "aa-fake") {
		t.Fatalf("unknown provider error must list known ones: %v", err)
	}
	names := Providers()
	if names[0] != "aa-fake" || !slices.Contains(names, "zz-fake") || slices.Contains(names, "nope") {
		t.Fatalf("names=%v", names)
	}
	old := DefaultProvider
	DefaultProvider = "aa-fake"
	defer func() { DefaultProvider = old }()
	if p, err := Lookup(""); err != nil || p.Name() != "aa-fake" {
		t.Fatalf("empty name must resolve to the default provider: %v", err)
	}
}
