package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/notify"
)

// A server.toml with no sink says nothing of events; one with a sink makes
// a foreground serve say, once, that it sends none.
func TestEventNotesSpeakOnlyOfSinks(t *testing.T) {
	if got := eventNotes(nil); got != nil {
		t.Fatalf("no file: %q", got)
	}
	if got := eventNotes(&api.Config{}); got != nil {
		t.Fatalf("no sinks: %q", got)
	}
	cfg := &api.Config{Events: api.EventsSection{Sinks: []api.EventSink{{Type: "webhook", URL: "https://example.com/XYZSECRET"}}}}
	want := []string{"events: [[events.sink]] is sent by the rota daemon (rota serve start); this foreground server sends none"}
	if got := eventNotes(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("one sink: %q, want %q", got, want)
	}
}

// Each sink is one line of serve status, in the three shapes it can be in
// while a daemon runs it, and with nothing said of it that is not there.
func TestSinkLines(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 20, 0, 0, time.Local)
	for _, c := range []struct {
		s    notify.Status
		want string
	}{
		{notify.Status{Name: "telegram", State: "ok", Sent: 12, LastSent: now.Add(-3 * time.Minute)},
			"events: telegram ok, 12 sent, last 3m ago"},
		{notify.Status{Name: "telegram", State: "ok", Sent: 1, LastSent: now.Add(-5 * time.Second)},
			"events: telegram ok, 1 sent, last 5s ago"},
		{notify.Status{Name: "telegram", State: "ok", Sent: 3, Dropped: 2, LastSent: now.Add(-2 * time.Hour)},
			"events: telegram ok, 3 sent, 2 dropped, last 2h ago"},
		{notify.Status{Name: "webhook", State: "failing", LastError: "webhook answered 502 Bad Gateway",
			LastErrorAt: time.Date(2026, 11, 11, 10, 14, 0, 0, time.Local)},
			"events: webhook failing since 10:14: webhook answered 502 Bad Gateway"},
		// Failing since the first failure in a row, however often it was
		// tried since.
		{notify.Status{Name: "webhook", State: "failing", LastError: "webhook answered 502 Bad Gateway",
			FailingSince: time.Date(2026, 11, 11, 8, 0, 0, 0, time.Local), LastErrorAt: time.Date(2026, 11, 11, 10, 19, 0, 0, time.Local)},
			"events: webhook failing since 08:00: webhook answered 502 Bad Gateway"},
		{notify.Status{Name: "webhook-2", State: "failing", LastError: "a\x1b]0;x\x07b",
			LastErrorAt: time.Date(2026, 11, 9, 8, 5, 0, 0, time.Local)},
			"events: webhook-2 failing since 2026-11-09 08:05: a ]0;x b"},
		{notify.Status{Name: "command", State: "waiting"}, "events: command waiting"},
		{notify.Status{Name: "command", State: "off"}, "events: command off"},
	} {
		if got := sinkLine(c.s, now); got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c.s, got, c.want)
		}
	}
}

// Each sink's answer to --test is one line, in one of three shapes.
func TestOutcomeLines(t *testing.T) {
	for _, c := range []struct {
		o    notify.Outcome
		want string
	}{
		{notify.Outcome{Sink: "webhook", Answered: true, OK: true}, "webhook: delivered"},
		{notify.Outcome{Sink: "webhook-2", Answered: true, Error: "dial tcp 127.0.0.1:9: connect: connection refused"},
			"webhook-2: failed: dial tcp 127.0.0.1:9: connect: connection refused"},
		{notify.Outcome{Sink: "telegram", Answered: true, Error: "a\nb\x1b"}, "telegram: failed: a b "},
		{notify.Outcome{Sink: "command"}, "command: no answer within 20s"},
	} {
		if got := outcomeLine(c.o); got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c.o, got, c.want)
		}
	}
}

// A command sink's program sees neither ROTA_TOKEN nor the variable
// auth.token_env names, and each is named once.
func TestHiddenEnv(t *testing.T) {
	for _, c := range []struct {
		cfg  *api.Config
		want []string
	}{
		{nil, []string{"ROTA_TOKEN"}},
		{&api.Config{}, []string{"ROTA_TOKEN"}},
		{&api.Config{Auth: api.AuthSection{TokenEnv: "ROTA_TOKEN"}}, []string{"ROTA_TOKEN"}},
		{&api.Config{Auth: api.AuthSection{TokenEnv: "MY_TOK"}}, []string{"ROTA_TOKEN", "MY_TOK"}},
	} {
		if got := hiddenEnv(c.cfg); !reflect.DeepEqual(got, c.want) {
			t.Errorf("token_env %v: %q, want %q", c.cfg, got, c.want)
		}
	}
}

// A foreground serve whose file names a sink says it sends none, and sends
// none: it writes no sinks.json and starts nothing that would.
func TestAForegroundServeSaysItSendsNone(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "server.toml")
	body := "[[events.sink]]\ntype = \"webhook\"\nurl = \"http://127.0.0.1:1/hook/XYZSECRET\"\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := serveFor(t, []string{"--config", conf}, func(string) {})
	if code != 0 {
		t.Fatalf("serve exited %d: %q", code, stderr)
	}
	note := "events: [[events.sink]] is sent by the rota daemon (rota serve start); this foreground server sends none\n"
	if strings.Count(stderr, note) != 1 {
		t.Fatalf("serve said %q; want the note once", stderr)
	}
	if strings.Contains(stderr, "XYZSECRET") {
		t.Fatalf("serve repeated the webhook's address: %q", stderr)
	}
	home := os.Getenv("ROTA_HOME")
	_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "sinks.json" {
			t.Errorf("a foreground serve wrote %s", path)
		}
		return nil
	})
}
