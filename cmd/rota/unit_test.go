package main

import (
	"path/filepath"
	"strings"
	"testing"
)

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
	got := systemdUnit("/opt/my rota/rota", `/s/a "b" 100%`, []string{`PATH=/x\y$z`})
	for _, w := range []string{
		`ExecStart="/opt/my rota/rota" serve start --foreground`,
		`Description=rota daemon for /s/a "b" 100%%`,
		`Environment="ROTA_HOME=/s/a \"b\" 100%%"`,
		`Environment="PATH=/x\\y$$z"`,
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
	if l != "com.github.professor93.rota-"+h {
		t.Fatalf("label %q does not share hash %q", l, h)
	}
	if s2, _ := unitName(filepath.Join(home, "other2")); s2 == s {
		t.Fatal("two stores share a unit")
	}
}
