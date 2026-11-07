//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// An agent install booted out may still be on its way out when it is
// bootstrapped again, so a bootstrap that fails after a bootout that
// unloaded something is tried again; one that fails with nothing booted
// out is said at once.
func TestABootstrapAfterABootoutIsTriedAgain(t *testing.T) {
	old := bootstrapRetry
	bootstrapRetry = time.Millisecond
	t.Cleanup(func() { bootstrapRetry = old })
	home := unitHome(t, true)
	store := filepath.Join(home, ".rota")
	path := filepath.Join(home, "Library", "LaunchAgents", "com.github.professor93.rota.plist")
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	bootout := "launchctl bootout " + domain + "/com.github.professor93.rota"
	bootstrap := "launchctl bootstrap " + domain + " " + path

	failures := 2
	calls := recordUnits(t, func(call string) error {
		if call == bootstrap && failures > 0 {
			failures--
			return errors.New("Bootstrap failed: 5: Input/output error")
		}
		return nil
	})
	if _, err := writeUnit("/u/rota", store, nil, true); err != nil {
		t.Fatalf("a bootstrap that succeeds on its third try: %v", err)
	}
	if want := []string{bootout, bootstrap, bootstrap, bootstrap}; !slices.Equal(*calls, want) {
		t.Fatalf("asked %q, want %q", *calls, want)
	}

	calls = recordUnits(t, func(call string) error {
		return errors.New("Bootstrap failed: 5: Input/output error")
	})
	if _, err := writeUnit("/u/rota", store, nil, false); err == nil {
		t.Fatal("a bootstrap that fails is not an error")
	}
	if want := []string{bootout, bootstrap}; !slices.Equal(*calls, want) {
		t.Fatalf("with nothing booted out asked %q, want %q", *calls, want)
	}
}
