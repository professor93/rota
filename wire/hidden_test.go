package wire

import (
	"slices"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// A hidden provider is still a provider — the SDK carries it, an account
// on it still runs — but no login surface offers it.
func TestHiddenProvidersAreLeftOutOfTheLoginList(t *testing.T) {
	if !Hidden("kimi") || Hidden("claude") || Hidden("") {
		t.Fatal("kimi is hidden, claude and the default are not")
	}
	got := LoginProviders()
	if slices.Contains(got, "kimi") || !slices.Contains(got, "claude") || !slices.IsSorted(got) {
		t.Fatalf("%v", got)
	}
	if len(got) != len(rota.Providers())-1 {
		t.Fatalf("one provider hidden: %v vs %v", got, rota.Providers())
	}
}
