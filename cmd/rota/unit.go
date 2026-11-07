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

// runSupervisor runs one supervisor command, and when it fails says what
// the supervisor said, not only its exit status.
func runSupervisor(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		if m := strings.TrimSpace(string(out)); m != "" {
			return fmt.Errorf("%s: %w: %s", name, err, m)
		}
	}
	return err
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
// and CLAUDE_CONFIG_DIR when it is set, by absolute path, since a daemon
// runs in /. Nothing else is, ROTA_TOKEN least of all; ROTA_HOME is the
// store's, written by the renderers.
func unitEnv() []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		if abs, err := filepath.Abs(d); err == nil {
			d = abs
		}
		env = append(env, "CLAUDE_CONFIG_DIR="+d)
	}
	return env
}

// supervise runs one supervisor command written as a line, so its option
// words are not mistaken for flags of rota's own.
func supervise(line string) error {
	f := strings.Fields(line)
	return unitRun(f[0], f[1:]...)
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

func unitWord(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\%$;") {
		return s
	}
	return `"` + strings.ReplaceAll(unitEscape(s), "$", "$$") + `"`
}

func systemdUnit(exe, store string, env []string) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=rota daemon for " + strings.NewReplacer("%", "%%", "\n", " ").Replace(store) + "\n\n")
	b.WriteString("[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + unitWord(exe) + " serve start --foreground\n")
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
	for _, a := range append([]string{exe}, strings.Fields("serve start --foreground")...) {
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
	b.WriteString("\t<key>AbandonProcessGroup</key>\n\t<true/>\n")
	str("ProcessType", "Background")
	str("StandardOutPath", "/dev/null")
	str("StandardErrorPath", "/dev/null")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}
