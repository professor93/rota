package store

import (
	"slices"
	"strings"
	"testing"
)

// HostEnv is the process environment minus every registered secret. The
// registry lives here rather than in the SDK because which variables are
// secret is this application's fact: store declares ROTA_HOME itself, the
// command adds ROTA_TOKEN, and lib never hears either name.
func TestHostEnvHidesWhatWasRegistered(t *testing.T) {
	t.Setenv("ROTA_HOME", "/h")
	t.Setenv("T_APP_SECRET", "s")
	t.Setenv("T_APP_PLAIN", "p")
	HideFromAgents("T_APP_SECRET")

	env := HostEnv()
	for _, banned := range []string{"ROTA_HOME=", "T_APP_SECRET="} {
		if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, banned) }) {
			t.Fatalf("%s must not survive HostEnv", strings.TrimSuffix(banned, "="))
		}
	}
	if !slices.Contains(env, "T_APP_PLAIN=p") {
		t.Fatal("an unregistered variable must pass through")
	}
}
