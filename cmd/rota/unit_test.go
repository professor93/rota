package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No systemctl or launchctl ever runs from this binary: neither in a test,
// nor in a rota it runs as — whose calls a test reads from the file
// ROTA_UNIT_RECORD names — nor in a session probe, which answers from an
// init and never reaches TestMain. A package's variables are set before any
// of its init functions runs.
var _ = func() bool { unitRun = standInSupervisor; return true }()

// standInSupervisor is every test's systemctl and launchctl: it runs
// nothing. In a rota this binary runs as, it writes each call as a line to
// the file ROTA_UNIT_RECORD names; anywhere else it refuses.
func standInSupervisor(name string, args ...string) error {
	if path := os.Getenv("ROTA_UNIT_RECORD"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, strings.Join(append([]string{name}, args...), " "))
		return err
	}
	return fmt.Errorf("a test ran %s %s; recordUnits(t) watches the supervisor instead", name, strings.Join(args, " "))
}

// recordUnits has the supervisor answer with answer and writes down each
// call, as its words joined by spaces, until the test ends.
func recordUnits(t *testing.T, answer func(call string) error) *[]string {
	t.Helper()
	var calls []string
	old := unitRun
	t.Cleanup(func() { unitRun = old })
	unitRun = func(name string, args ...string) error {
		call := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, call)
		if answer != nil {
			return answer(call)
		}
		return nil
	}
	return &calls
}

func TestTheUnitKeepsSessions(t *testing.T) {
	env := []string{"HOME=/h", "PATH=/usr/bin:/bin"}
	want := `[Unit]
Description=rota daemon for /s/.rota

[Service]
Type=simple
ExecStart=/usr/local/bin/rota serve start --foreground
Environment="PATH=/usr/bin:/bin"
Environment="ROTA_HOME=/s/.rota"
KillMode=process
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`
	if got := systemdUnit("/usr/local/bin/rota", "/s/.rota", env); got != want {
		t.Fatalf("unit:\n%s\nwant:\n%s", got, want)
	}
	got := systemdUnit("/usr/local/bin/rota", "/s/.rota", append(env, "CLAUDE_CONFIG_DIR=/c d"))
	if !strings.Contains(got, "Environment=\"ROTA_HOME=/s/.rota\"\nEnvironment=\"CLAUDE_CONFIG_DIR=/c d\"\nKillMode=process") {
		t.Fatalf("config dir line missing or misplaced:\n%s", got)
	}
}

func TestTheUnitQuotesOddPaths(t *testing.T) {
	got := systemdUnit("/opt/my rota/r$ota", `/s/a "b" 100%`, []string{`PATH=/x\y$z`})
	for _, w := range []string{
		`ExecStart="/opt/my rota/r$$ota" serve start --foreground`,
		`Description=rota daemon for /s/a "b" 100%%`,
		`Environment="ROTA_HOME=/s/a \"b\" 100%%"`,
		`Environment="PATH=/x\\y$z"`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

func TestThePlistKeepsSessions(t *testing.T) {
	env := []string{"PATH=/usr/bin", "CLAUDE_CONFIG_DIR=/c&d"}
	got := launchdPlist("com.github.professor93.rota", "/u/rota", "/s/<r>", env)
	for _, w := range []string{
		"<key>Label</key>\n\t<string>com.github.professor93.rota</string>",
		"<string>/u/rota</string>\n\t\t<string>serve</string>\n\t\t<string>start</string>\n\t\t<string>--foreground</string>",
		"<key>PATH</key>\n\t\t<string>/usr/bin</string>",
		"<key>ROTA_HOME</key>\n\t\t<string>/s/&lt;r&gt;</string>",
		"<key>CLAUDE_CONFIG_DIR</key>\n\t\t<string>/c&amp;d</string>",
		"<key>AbandonProcessGroup</key>\n\t<true/>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ProcessType</key>\n\t<string>Background</string>",
		"<key>StandardOutPath</key>\n\t<string>/dev/null</string>",
		"<key>StandardErrorPath</key>\n\t<string>/dev/null</string>",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(launchdPlist("l", "/u/rota", "/s", []string{"PATH=/p"}), "CLAUDE_CONFIG_DIR") {
		t.Error("config dir written while unset")
	}
}

func TestAnotherStoreGetsAnotherUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if s, l := unitName(filepath.Join(home, ".rota")); s != "rota.service" || l != "com.github.professor93.rota" {
		t.Fatalf("default store: %s %s", s, l)
	}
	other := filepath.Join(home, "elsewhere")
	s, l := unitName(other)
	if len(s) != len("rota-xxxxxxxx.service") || !strings.HasPrefix(s, "rota-") || !strings.HasSuffix(s, ".service") {
		t.Fatalf("unit %q", s)
	}
	h := strings.TrimSuffix(strings.TrimPrefix(s, "rota-"), ".service")
	sum := sha256.Sum256([]byte(other))
	if want := hex.EncodeToString(sum[:])[:8]; h != want {
		t.Fatalf("suffix %q is not hex(sha256(store))[:8], %q", h, want)
	}
	if l != "com.github.professor93.rota-"+h {
		t.Fatalf("label %q does not share hash %q", l, h)
	}
	if s2, _ := unitName(filepath.Join(home, "other2")); s2 == s {
		t.Fatal("two stores share a unit")
	}
	// A store named relatively is hashed by its absolute path: the same
	// store from wherever it is named, and ~/.rota the default from home.
	t.Chdir(home)
	if s2, l2 := unitName("elsewhere"); s2 != s || l2 != l {
		t.Fatalf("relative store: %s %s, want %s %s", s2, l2, s, l)
	}
	if s2, _ := unitName(".rota"); s2 != "rota.service" {
		t.Fatalf("relative default store: %s", s2)
	}
}
