package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/professor93/rota/store"
)

// The daemon as a supervised service. These renderers are pure: env is the
// installer's environment as KEY=VALUE entries, of which only PATH and
// CLAUDE_CONFIG_DIR are read; ROTA_HOME is always the store.

const (
	defaultUnit  = "rota.service"
	defaultLabel = "com.github.professor93.rota"
)

// unitRun runs a supervisor command (systemctl, launchctl). Tests replace it.
var unitRun = runSupervisor

// runSupervisor runs one supervisor command, and when it fails names the
// command and says what the supervisor said, not only its exit status: of
// several that failed, each is told apart. The command, which names a unit
// file, is said on one line and what the supervisor said on as many as it
// took, each with a space for a control character (see printable).
func runSupervisor(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	cmd := oneLine(strings.Join(append([]string{name}, args...), " "))
	if m := strings.TrimSpace(string(out)); m != "" {
		return fmt.Errorf("%s: %w: %s", cmd, err, printable(m))
	}
	return fmt.Errorf("%s: %w", cmd, err)
}

// serveInstall is `rota serve install`: this store's daemon as a systemd
// user unit or a launchd agent, which starts it at login and keeps its
// sessions through a restart.
func (c *cli) serveInstall(args []string) error {
	fs := flag.NewFlagSet("serve install", flag.ContinueOnError)
	if ok, err := c.daemonFlags(fs, args); !ok {
		return err
	}
	return c.installDaemon()
}

// serveUninstall is `rota serve uninstall`: the unit or agent taken away,
// and the daemon it ran stopped; every session goes on.
func (c *cli) serveUninstall(args []string) error {
	fs := flag.NewFlagSet("serve uninstall", flag.ContinueOnError)
	if ok, err := c.daemonFlags(fs, args); !ok {
		return err
	}
	return c.uninstallDaemon()
}

// unitEnv is what of this environment a supervised daemon is given: PATH,
// and the person's own Claude Code directory as CLAUDE_CONFIG_DIR when a
// variable names one, by absolute path, since a daemon runs in /. Which
// directory is the person's is the store's rule, PersonalClaudeWorld:
// inside a CLI rota launched, CLAUDE_CONFIG_DIR is the account's mirror,
// never written here. Nothing else is, ROTA_TOKEN least of all; ROTA_HOME
// is the store's, written by the renderers.
func unitEnv() []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if d, named := store.PersonalClaudeWorld(); named && d != "" {
		if abs, err := filepath.Abs(d); err == nil {
			d = abs
		}
		env = append(env, "CLAUDE_CONFIG_DIR="+d)
	}
	return env
}

// unitName gives the systemd unit and the launchd label for a store: the
// plain names for ~/.rota, and for any other store the same names with the
// first 8 hex of sha256(absolute store) added, so two stores never collide.
func unitName(store string) (systemd, label string) {
	abs, err := filepath.Abs(store)
	if err != nil {
		abs = store
	}
	if home, err := os.UserHomeDir(); err == nil && abs == filepath.Join(home, ".rota") {
		return defaultUnit, defaultLabel
	}
	sum := sha256.Sum256([]byte(abs))
	h := hex.EncodeToString(sum[:])[:8]
	return "rota-" + h + ".service", defaultLabel + "-" + h
}

func envValue(env []string, key string) string {
	v := ""
	for _, e := range env {
		if k, val, ok := strings.Cut(e, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// unitEscape makes s safe inside a double-quoted systemd word: backslash and
// quote are escaped, % doubled (specifiers), and a newline written as \n.
// $ has no meaning in Environment=, so it is left alone there.
func unitEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "\n", `\n`).Replace(s)
}

// unitProgram is the program ExecStart runs, quoted where it needs to be.
// Its $ is left single: systemd expands variables in the arguments only,
// and execs the program's path as written, so a $$ there would be two.
func unitProgram(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\%;") {
		return s
	}
	return `"` + unitEscape(s) + `"`
}

func systemdUnit(exe, store string, env []string) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=rota daemon for " + strings.NewReplacer("%", "%%", "\n", " ").Replace(store) + "\n\n")
	b.WriteString("[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + unitProgram(exe) + " serve start --foreground\n")
	b.WriteString(`Environment="PATH=` + unitEscape(envValue(env, "PATH")) + "\"\n")
	b.WriteString(`Environment="ROTA_HOME=` + unitEscape(store) + "\"\n")
	if d := envValue(env, "CLAUDE_CONFIG_DIR"); d != "" {
		b.WriteString(`Environment="CLAUDE_CONFIG_DIR=` + unitEscape(d) + "\"\n")
	}
	b.WriteString("KillMode=process\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func xmlEsc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func launchdPlist(label, exe, store string, env []string) string {
	var b strings.Builder
	str := func(k, v string) {
		b.WriteString("\t<key>" + xmlEsc(k) + "</key>\n\t<string>" + xmlEsc(v) + "</string>\n")
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	str("Label", label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range []string{exe, "serve", "start", "--foreground"} {
		b.WriteString("\t\t<string>" + xmlEsc(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	ev := func(k, v string) { b.WriteString("\t\t<key>" + k + "</key>\n\t\t<string>" + xmlEsc(v) + "</string>\n") }
	ev("PATH", envValue(env, "PATH"))
	ev("ROTA_HOME", store)
	if d := envValue(env, "CLAUDE_CONFIG_DIR"); d != "" {
		ev("CLAUDE_CONFIG_DIR", d)
	}
	b.WriteString("\t</dict>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	// Interactive, as measured: with no ProcessType, launchd's default, the
	// daemon, its holders and every held CLI, which inherit it, ran at
	// priority 20 under a ceiling of 20 at UTILITY QoS; with Interactive at
	// 31 under 63, as under a daemon started by hand. Held CLIs are typed
	// into by people.
	str("ProcessType", "Interactive")
	b.WriteString("\t<key>AbandonProcessGroup</key>\n\t<true/>\n")
	str("StandardOutPath", "/dev/null")
	str("StandardErrorPath", "/dev/null")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}
