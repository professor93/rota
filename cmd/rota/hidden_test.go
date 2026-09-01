package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/professor93/rota/store"
)

// rota's own secrets never reach a vendor CLI. The registry lives in store —
// the application layer — because which variables are secret is the
// application's fact: store seeds ROTA_HOME, this command registers
// ROTA_TOKEN in its init, and the SDK hears neither name (it never reads the
// environment at all). The guarantee is asserted here because the command is
// the one place that has both secrets.
func TestRotaOwnSecretsNeverReachTheChild(t *testing.T) {
	t.Setenv("ROTA_TOKEN", "t")
	t.Setenv("ROTA_HOME", "/h")
	t.Setenv("T_ORDINARY", "x")
	env := store.HostEnv()
	for _, secret := range []string{"ROTA_TOKEN", "ROTA_HOME"} {
		if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, secret+"=") }) {
			t.Fatalf("%s must not be inherited by an agent", secret)
		}
	}
	if !slices.Contains(env, "T_ORDINARY=x") {
		t.Fatal("everything else still passes through")
	}
}
