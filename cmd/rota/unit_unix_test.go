//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndRemoveUnitRunTheSupervisor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	var calls []string
	old := unitRun
	unitRun = func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	defer func() { unitRun = old }()
	store := filepath.Join(home, ".rota")
	path, err := writeUnit("/u/rota", store, []string{"PATH=/p"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, home) {
		t.Fatalf("unit outside the temp home: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err := removeUnit(store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unit still there: %v", err)
	}
	if len(calls) < 2 || !strings.HasPrefix(calls[0], "systemctl --user daemon-reload") && !strings.HasPrefix(calls[0], "launchctl bootstrap") {
		t.Fatalf("calls: %q", calls)
	}
}
