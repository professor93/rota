package api

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type stampLog struct {
	mu    sync.Mutex
	lines *[]string
	start time.Time
}

func (l *stampLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *stampLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "+%v %s", time.Since(l.start).Round(time.Millisecond), r.Message)
	r.Attrs(func(a slog.Attr) bool { fmt.Fprintf(&b, " %s=%v", a.Key, a.Value); return true })
	*l.lines = append(*l.lines, b.String())
	return nil
}
func (l *stampLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *stampLog) WithGroup(string) slog.Handler      { return l }

func TestZZDiagTimeout(t *testing.T) {
	var report []string
	for i := 0; i < 25; i++ {
		for _, per := range []bool{false, true} {
			mark := t.TempDir()
			var lines []string
			lg := &stampLog{lines: &lines}
			opts := Options{Timeout: time.Second, Log: slog.New(lg)}
			body := map[string]any{"prompt": "p"}
			if per {
				opts.Timeout = 0
				body["timeout_seconds"] = 1
			}
			h := newHarness(t, opts)
			t.Setenv("FAKE_SLEEP", "3s")
			t.Setenv("FAKE_MARK", mark)
			start := time.Now()
			lg.start = start
			code, _, _ := h.run(1, body)
			took := time.Since(start)
			var spawned string
			slept := false
			ents, _ := os.ReadDir(mark)
			for _, e := range ents {
				raw, _ := os.ReadFile(filepath.Join(mark, e.Name()))
				at, _ := time.Parse(time.RFC3339Nano, string(raw))
				if strings.HasPrefix(e.Name(), "start-") {
					spawned = at.Sub(start).Round(time.Millisecond).String()
				}
			}
			// Was the child still alive to finish its sleep?
			time.Sleep(3 * time.Second)
			ents, _ = os.ReadDir(mark)
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), "slept-") {
					slept = true
				}
			}
			mu := &lg.mu
			mu.Lock()
			report = append(report, fmt.Sprintf("i=%d per=%v code=%d took=%v child-started-at=+%s child-finished-sleep=%v log=%q",
				i, per, code, took.Round(time.Millisecond), spawned, slept, lines))
			mu.Unlock()
		}
	}
	t.Errorf("diagnostic, always fails to show its log:\n%s", strings.Join(report, "\n"))
}
