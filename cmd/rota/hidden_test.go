package main

import (
	"strings"
	"testing"
)

// kimi is hidden from every login surface until its service works: naming
// it is refused with the reason, and the provider lists shown to a person
// leave it out. The SDK still knows it, so an account already on it runs.
func TestHiddenProviderIsNotOfferedForLogin(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	for _, argv := range [][]string{{"login", "kimi"}, {"login", "--provider=kimi"}} {
		_, errOut, code := call(t, argv...)
		if code != 2 || !strings.Contains(errOut, "not offered") || strings.Contains(errOut, "grok, kimi") {
			t.Fatalf("%v: code %d %q", argv, code, errOut)
		}
	}
	// The hint on an empty store and the unknown-provider message both list
	// what can be logged into, and that list has no kimi in it.
	out, _, _ := call(t, "list", "--short")
	if !strings.Contains(out, "Providers: claude, codex, grok") || strings.Contains(out, "kimi") {
		t.Fatalf("%q", out)
	}
	if _, errOut, _ := call(t, "login", "zzz"); !strings.Contains(errOut, "claude, codex, grok") || strings.Contains(errOut, "kimi") {
		t.Fatalf("%q", errOut)
	}
}
