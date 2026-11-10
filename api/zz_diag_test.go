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

type diagRow struct {
	i     int
	per   bool
	code  int
	took  time.Duration
	mark  string
	start time.Time
	lines *[]string
}

func TestZZDiagTimeout(t *testing.T) {
	var rows []diagRow
	for i := 0; i < 60; i++ {
		for _, per := range []bool{false, true} {
			mark := t.TempDir()
			lines := &[]string{}
			lg := &stampLog{lines: lines}
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
			rows = append(rows, diagRow{i, per, code, time.Since(start), mark, start, lines})
		}
	}
	time.Sleep(4 * time.Second)
	var report []string
	slow := 0
	for _, r := range rows {
		var spawned []string
		slept := 0
		ents, _ := os.ReadDir(r.mark)
		for _, e := range ents {
			raw, _ := os.ReadFile(filepath.Join(r.mark, e.Name()))
			at, _ := time.Parse(time.RFC3339Nano, string(raw))
			if strings.HasPrefix(e.Name(), "start-") {
				spawned = append(spawned, at.Sub(r.start).Round(time.Millisecond).String())
			}
			if strings.HasPrefix(e.Name(), "slept-") {
				slept++
			}
		}
		if r.took > 1500*time.Millisecond || slept > 0 || r.code != 504 {
			slow++
		}
		report = append(report, fmt.Sprintf("i=%d per=%v code=%d took=%v child-started-at=%v slept=%d log=%q",
			r.i, r.per, r.code, r.took.Round(time.Millisecond), spawned, slept, *r.lines))
	}
	t.Errorf("diagnostic, always fails to show its log; %d odd rows:\n%s", slow, strings.Join(report, "\n"))
}
