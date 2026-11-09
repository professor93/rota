//go:build linux || darwin

package main

import (
	"encoding/json"
	nethttp "net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/termenv"
)

// insideARotaCLI sets this process's environment as a CLI rota launched for
// a claude account on an env token has it, typed at iTerm inside tmux, and
// says the person's own Claude Code directory.
func insideARotaCLI(t *testing.T) (person string) {
	t.Helper()
	person, account := t.TempDir(), t.TempDir()
	for k, v := range map[string]string{
		"ROTA_ACCOUNT_ID": "7", "ROTA_ACCOUNT": "work", "ROTA_PROVIDER": "claude",
		"ROTA_CLAUDE_HOME": person, "CLAUDE_CONFIG_DIR": account,
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-the-accounts", "CODEX_HOME": account,
		"TMUX": "/tmp/tmux-1/default,1,0", "TERM_PROGRAM": "iTerm.app", "CLAUDECODE": "1",
		"ROTA_KEPT": "kept",
	} {
		t.Setenv(k, v)
	}
	return person
}

// What a daemon is started with keeps nothing of the terminal it was typed
// at, which it outlives, and, started from inside a CLI rota launched,
// nothing that CLI carries for its account: CLAUDE_CONFIG_DIR is the
// person's own, which ROTA_CLAUDE_HOME names there. Everything else is kept.
// Outside such a CLI, a CLAUDE_CONFIG_DIR is the person's, and is kept with
// whatever else the person set.
func TestADaemonIsStartedWithNothingOfAnAccountOrATerminal(t *testing.T) {
	person := insideARotaCLI(t)
	env := daemonEnv("/s/.rota")
	for _, name := range []string{"ROTA_ACCOUNT_ID", "ROTA_ACCOUNT", "ROTA_PROVIDER", "CLAUDE_CODE_OAUTH_TOKEN",
		"CODEX_HOME", "TMUX", "TERM_PROGRAM", "CLAUDECODE"} {
		if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, name+"=") }) {
			t.Errorf("a daemon started inside a CLI rota launched has %s", name)
		}
	}
	for name, want := range map[string]string{"CLAUDE_CONFIG_DIR": person, "ROTA_CLAUDE_HOME": person, "ROTA_HOME": "/s/.rota", "ROTA_KEPT": "kept"} {
		if got := envValue(env, name); got != want {
			t.Errorf("%s is %q, want %q", name, got, want)
		}
	}

	// Outside such a CLI, the person's own variables are the person's.
	for _, k := range []string{"ROTA_ACCOUNT_ID", "ROTA_ACCOUNT", "ROTA_PROVIDER", "ROTA_CLAUDE_HOME"} {
		os.Unsetenv(k)
	}
	env = daemonEnv("/s/.rota")
	for name, want := range map[string]string{"CLAUDE_CONFIG_DIR": envValue(os.Environ(), "CLAUDE_CONFIG_DIR"),
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-the-accounts", "CODEX_HOME": envValue(os.Environ(), "CODEX_HOME")} {
		if got := envValue(env, name); got != want || want == "" {
			t.Errorf("outside a CLI rota launched, %s is %q, want %q", name, got, want)
		}
	}
	if envValue(env, "TMUX") != "" {
		t.Error("outside a CLI rota launched, the daemon has the terminal's TMUX")
	}
}

// And so a shell the daemon holds, started by serve start typed inside a
// CLI rota launched, does not run as that CLI's account: it has neither the
// account's home nor its token, and claude typed there reads the person's
// own directory.
func TestAShellADaemonHoldsIsNotTheAccountItWasStartedFrom(t *testing.T) {
	r := newDaemonRig(t)
	if err := os.WriteFile(filepath.Join(r.home, "server.toml"), []byte("[terminal]\nshell = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, _, _ := strings.Cut(envValue(r.env, "PATH"), string(os.PathListSeparator))
	person := insideARotaCLI(t)
	t.Setenv("SHELL", filepath.Join(bin, "claude"))
	cmd := r.cmd("serve", "start")
	cmd.Env = os.Environ()
	for _, e := range r.env {
		k, v, _ := strings.Cut(e, "=")
		if k == "ROTA_AS_RUN" || k == "PATH" || k == "ROTA_HOME" {
			cmd.Env = termenv.Set(cmd.Env, k, v)
		}
	}
	r.startWith(cmd)

	resp, err := ctlClient(r.sock).Post("http://rota/v1/terminals", "application/json", strings.NewReader(`{"kind":"shell","cols":100,"rows":30}`))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	id, _ := doc["id"].(string)
	if resp.StatusCode != nethttp.StatusCreated || !holdwire.IsID(id) {
		t.Fatalf("a shell over ctl.sock: %d %v\nlog:\n%s", resp.StatusCode, doc, r.log())
	}
	p := r.attach(id)
	p.waitOut("ready 100x30")
	for _, ask := range []struct{ name, want string }{
		{"ROTA_ACCOUNT_ID", "env ROTA_ACCOUNT_ID unset"},
		{"ROTA_PROVIDER", "env ROTA_PROVIDER unset"},
		{"CLAUDE_CODE_OAUTH_TOKEN", "env CLAUDE_CODE_OAUTH_TOKEN unset"},
		{"CODEX_HOME", "env CODEX_HOME unset"},
		{"CLAUDE_CONFIG_DIR", "env CLAUDE_CONFIG_DIR=" + person + "\r"},
		{"ROTA_KEPT", "env ROTA_KEPT=kept\r"},
	} {
		p.send(0x2, []byte("env "+ask.name+"\r"))
		p.waitOut(ask.want)
	}
}
