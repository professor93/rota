package api

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// warnings is a log a test can read back. slog's own handlers write
// somewhere; this one remembers, which is what an assertion needs.
type warnings struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnings) Enabled(context.Context, slog.Level) bool { return true }

func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, r.Message)
	return nil
}

func (w *warnings) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnings) WithGroup(string) slog.Handler      { return w }

func (w *warnings) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.msgs)
}

// A claude account runs in a mirror of the person's Claude Code directory so
// that it gets a daemon of its own. When that mirror cannot be built the run
// still happens — the token is what decides who pays, and the caller asked
// for an answer, not for a daemon — but the server is the only one who can
// notice, so it says so in its log rather than losing it.
func TestARunWhoseMirrorFailsSucceedsAndIsLogged(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", notADir)
	var log warnings
	h := newHarness(t, Options{Log: slog.New(&log)})

	code, out, raw := h.run(1, map[string]any{"prompt": "hi"})
	if code != 200 || out.IsError {
		t.Fatalf("the run must go ahead: %d %s", code, raw)
	}
	if !slices.ContainsFunc(log.all(), func(m string) bool { return strings.Contains(m, "could not mirror") }) {
		t.Fatalf("the server must record what the run lost: %v", log.all())
	}
}
