//go:build linux || darwin

package api

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/professor93/rota/store"
)

// panicOn is a log handler that panics on one message: a way to make a
// terminal's start panic at the moment that message is logged.
type panicOn string

func (p panicOn) Enabled(context.Context, slog.Level) bool { return true }

func (p panicOn) Handle(_ context.Context, r slog.Record) error {
	if r.Message == string(p) {
		panic("a terminal's start panicking on purpose")
	}
	return nil
}

func (p panicOn) WithAttrs([]slog.Attr) slog.Handler { return p }
func (p panicOn) WithGroup(string) slog.Handler      { return p }

// idleAgain waits until nothing holds the account, the server's slots
// included.
func idleAgain(t *testing.T, h *harness, id int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := store.Open(h.dir)
		if err != nil {
			t.Fatal(err)
		}
		release, idle := st.Hold(st.Find(id))
		release()
		st.Close()
		if idle && len(h.api.sem) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the account is let go of (%v) and so is the slot (%d taken)", idle, len(h.api.sem))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A run that stays open and whose start panics once its CLI is running is
// cancelled and ended as any other run is: the account and the request's slot
// are let go of, rather than held until InputTimeout by a run nobody can reach.
func TestAnOpenRunWhoseStartPanicsLetsGoOfTheAccount(t *testing.T) {
	h := newHarness(t, Options{Log: slog.New(panicOn("run started"))})
	resp, raw := h.do("POST", "/v1/accounts/1/run",
		map[string]any{"prompt": "p", "stream": true, "input": true}, "Accept", "application/x-ndjson")
	if resp.StatusCode != 500 {
		t.Fatalf("the panic was recovered and answered: %d %s", resp.StatusCode, raw)
	}
	idleAgain(t, h, 1)
}

// A terminal whose start panics after its CLI is running — the server recovers
// the panic and goes on serving — ends that CLI and lets go of the account,
// rather than leaving it claimed until the server restarts: for codex, whose
// claim is exclusive, no run could use the account at all. Nor is it left
// counted among the live terminals.
func TestATerminalWhoseStartPanicsLetsGoOfTheAccount(t *testing.T) {
	for _, c := range []struct {
		what string
		id   int
	}{{"a claude account", 1}, {"a codex account", 2}} {
		t.Run(c.what, func(t *testing.T) {
			p := withTerminals(t, func(o *Options) { o.Log = slog.New(panicOn("terminal created")) })
			resp, raw := p.do("POST", "/v1/terminals", map[string]any{"account": c.id, "cols": 80, "rows": 24})
			if resp.StatusCode != 500 {
				t.Fatalf("the panic was recovered and answered: %d %s", resp.StatusCode, raw)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				st, err := store.Open(p.dir)
				if err != nil {
					t.Fatal(err)
				}
				release, idle := st.Hold(st.Find(c.id))
				release()
				st.Close()
				if idle && p.api.liveTerminals() == 0 {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("the account is let go of (%v) and no terminal is live (%d)", idle, p.api.liveTerminals())
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
