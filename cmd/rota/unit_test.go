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
TimeoutStopSec=60
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
		`ExecStart="/opt/my rota/r$ota" serve start --foreground`,
		`Description=rota daemon for /s/a "b" 100%%`,
		`Environment="ROTA_HOME=/s/a \"b\" 100%%"`,
		`Environment="PATH=/x\\y$z"`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	// systemd execs the program's path as written, and expands variables
	// in no value here: a $ is one $ everywhere.
	if strings.Contains(got, "$$") {
		t.Errorf("a $ doubled in:\n%s", got)
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
		"<key>ExitTimeOut</key>\n\t<integer>60</integer>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ProcessType</key>\n\t<string>Interactive</string>",
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
	// Interactive, once: with none, the daemon and every held CLI ran at a
	// lower priority than under a daemon started by hand.
	if n := strings.Count(got, "<key>ProcessType</key>"); n != 1 {
		t.Errorf("the agent names a ProcessType %d times", n)
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

// Without the bus, a systemd user manager for this user is looked for in
// /proc — a process of this user's whose argv is systemd --user — and in
// the cgroup tree, where user@UID.service is its cgroup on either layout.
// Only when neither shows one is it taken to be gone; a /proc that cannot
// be read, or that does not show this process — empty, or not mounted —
// could be hiding one.
func TestAUserManagerIsLookedForWithoutTheBus(t *testing.T) {
	const uid = 1000
	proc := func(t *testing.T, root string, pid, uid int, argv ...string) {
		t.Helper()
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		status := fmt.Sprintf("Name:\tx\nUmask:\t0022\nState:\tS (sleeping)\nUid:\t%d\t%d\t%d\t%d\nGid:\t1\t1\t1\t1\n", uid, uid, uid, uid)
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	slice := fmt.Sprintf("user.slice/user-%d.slice/user@%d.service", uid, uid)
	for _, c := range []struct {
		name string
		set  func(t *testing.T, proc0, cg string)
		runs bool
	}{
		{"nothing", func(*testing.T, string, string) {}, false},
		{"an empty /proc", func(t *testing.T, p, _ string) {
			if err := os.Remove(filepath.Join(p, "self")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"this user's manager", func(t *testing.T, p, _ string) {
			proc(t, p, 1, 0, "/sbin/init")
			proc(t, p, 812, uid, "/usr/lib/systemd/systemd", "--user")
		}, true},
		{"another user's manager", func(t *testing.T, p, _ string) { proc(t, p, 812, uid+1, "/usr/lib/systemd/systemd", "--user") }, false},
		{"a manager whose uid begins with this one's", func(t *testing.T, p, _ string) {
			proc(t, p, 812, uid*10, "/usr/lib/systemd/systemd", "--user")
		}, false},
		{"this user's other programs", func(t *testing.T, p, _ string) {
			proc(t, p, 900, uid, "/usr/lib/systemd/systemd-logind")
			proc(t, p, 901, uid, "/usr/bin/sleep", "--user")
			proc(t, p, 902, uid, "systemd")
			if err := os.MkdirAll(filepath.Join(p, "903"), 0o755); err != nil { // gone meanwhile
				t.Fatal(err)
			}
		}, false},
		{"its cgroup", func(t *testing.T, _, cg string) {
			if err := os.MkdirAll(filepath.Join(cg, slice), 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"its cgroup on the v1 layout", func(t *testing.T, _, cg string) {
			if err := os.MkdirAll(filepath.Join(cg, "systemd", slice), 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"no /proc to read", func(t *testing.T, p, _ string) {
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			p, cg := filepath.Join(root, "proc"), filepath.Join(root, "cgroup")
			for _, d := range []string{p, filepath.Join(p, "self"), cg} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			c.set(t, p, cg)
			if got := userManagerRuns(p, cg, uid); got != c.runs {
				t.Fatalf("userManagerRuns = %v, want %v", got, c.runs)
			}
		})
	}
}
