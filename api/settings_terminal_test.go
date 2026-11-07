//go:build linux || darwin

package api

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A terminal's Claude Code reads the project settings of the directory the
// terminal starts in, not of wherever the server was started, so that is
// the directory the warning about them names — in the server's log, where a
// launch's warnings go. The value is never written.
func TestATerminalWarnsAboutTheSettingsOfItsOwnDirectory(t *testing.T) {
	var out logBuf
	p := withTerminals(t, func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	put := func(path, body string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	server := t.TempDir()
	put(filepath.Join(server, ".claude", "settings.json"), `{"env":{"CLAUDE_CODE_USE_VERTEX":"SECRET-1"}}`)
	t.Chdir(server)
	sub := put(filepath.Join(p.root, "sub", ".claude", "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://SECRET.example"}}`)
	root := put(filepath.Join(p.root, ".claude", "settings.json"), `{"apiKeyHelper":"SECRET-helper"}`)

	said := func(cwd string) string {
		t.Helper()
		before := len(out.String())
		body := map[string]any{"account": 1, "cols": 80, "rows": 24}
		if cwd != "" {
			body["cwd"] = cwd
		}
		id := p.start(body)["id"].(string)
		p.do("DELETE", "/v1/terminals/"+id, nil)
		// Prepare warns before the terminal is created, inside the request.
		var lines []string
		for _, l := range strings.Split(out.String()[before:], "\n") {
			if strings.Contains(l, "settings Claude Code reads") {
				lines = append(lines, l)
			}
		}
		if len(lines) != 1 {
			t.Fatalf("one warning for the terminal in %q: %q", cwd, lines)
		}
		if strings.Contains(lines[0], "SECRET") || strings.Contains(lines[0], "VERTEX") {
			t.Fatalf("the server's own directory, or a value, in the warning: %s", lines[0])
		}
		return lines[0]
	}

	if got := said(filepath.Join(p.root, "sub")); !strings.Contains(got, sub+" sets env.ANTHROPIC_BASE_URL") ||
		strings.Contains(got, "apiKeyHelper") {
		t.Fatalf("the terminal's own directory, and only its: %s", got)
	}
	// No directory asked for is the first root, which is where it starts.
	if got := said(""); !strings.Contains(got, root+" sets apiKeyHelper") || strings.Contains(got, "ANTHROPIC_BASE_URL") {
		t.Fatalf("the first root: %s", got)
	}
}
