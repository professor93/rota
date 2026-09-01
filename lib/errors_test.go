package rota

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestVerdictsAreTypedNotStringMatched(t *testing.T) {
	// A provider whose CLI rota drives, but which has no effort setting.
	Register(fakeCatalog{fakeProvider: &fakeProvider{name: "claude-noeffort"},
		models: []Model{{ID: "m-1"}}, dm: "m-1"})
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"unknown model", mustErr(t, func() error { _, e := ResolveModel("claude", "gpt-5.6-sol"); return e }), ErrInvalidRequest},
		{"unknown effort", mustErr(t, func() error { _, e := ResolveEffort("claude", "nope"); return e }), ErrInvalidRequest},
		{"effort unsupported", mustErr(t, func() error { _, e := ResolveEffort("claude-noeffort", "low"); return e }), ErrInvalidRequest},
		{"no prompt", (Spec{}).Check("claude", nil), ErrInvalidRequest},
		{"bad enum", (Spec{Prompt: "p", PermissionMode: "yolo"}).Check("claude", nil), ErrInvalidRequest},
		{"reserved flag", (Spec{Prompt: "p", Extra: []string{"--output-format", "text"}}).Check("claude", nil), ErrInvalidRequest},
		{"negative timeout", (Spec{Prompt: "p", TimeoutSeconds: -1}).Check("claude", nil), ErrInvalidRequest},
		{"dangerous mode", (Spec{Prompt: "p", PermissionMode: "bypassPermissions"}).Check("claude", nil), ErrDangerous},
		{"dangerous flag", (Spec{Prompt: "p", DangerouslySkipPermissions: true}).Check("claude", nil), ErrDangerous},
		{"dangerous sandbox", (Spec{Prompt: "p", Sandbox: "danger-full-access"}).Check("codex", nil), ErrDangerous},
		{"cwd outside", (Spec{Prompt: "p", Cwd: "/etc"}).Check("claude", &Limits{Roots: []string{"/usr"}}), ErrOutsideRoots},
		{"missing cwd", (Spec{Prompt: "p", Cwd: "/no/such/dir"}).Check("claude", nil), ErrInvalidRequest},
		{"no headless interface", (Spec{Prompt: "p"}).Check("claude-noeffort", nil), ErrUnsupported},
	}
	for _, c := range cases {
		if c.err == nil {
			t.Fatalf("%s: expected an error", c.name)
		}
		if !errors.Is(c.err, c.want) {
			t.Fatalf("%s: %v is not %v", c.name, c.err, c.want)
		}
		// A caller must still get a message worth showing a person.
		if len(c.err.Error()) < 10 || strings.HasPrefix(c.err.Error(), "invalid request:") {
			t.Fatalf("%s: unhelpful message %q", c.name, c.err)
		}
	}
	// The kinds must not collapse into each other.
	if errors.Is(ErrDangerous, ErrInvalidRequest) || errors.Is(ErrOutsideRoots, ErrDangerous) {
		t.Fatal("verdict kinds must stay distinct")
	}
}

func TestDeadCredentialVerdictsAreTyped(t *testing.T) {
	Register(&fakeProvider{name: "t-err-static"})
	a := &Account{ID: 1, Provider: "t-err-static", Token: Token{Access: "x", ExpiresAt: 1}}
	_, err := Refresh(context.Background(), a)
	if !errors.Is(err, ErrReauth) {
		t.Fatalf("%v", err)
	}
	if _, err := Stage(&Account{Provider: "claude", Dead: true}, ""); !errors.Is(err, ErrReauth) {
		t.Fatalf("a dead account must report itself as needing re-auth: %v", err)
	}
}
