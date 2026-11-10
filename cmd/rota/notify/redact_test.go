package notify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/events"
)

// What a sink reports is its secrets taken out wherever they stand, and the
// URL a net/http error repeats taken out whole: nothing of a token or a
// webhook's key reaches the log or the file (Review Focus 2).
func TestSecretsNeverReachTheLogOrTheFile(t *testing.T) {
	quick(t)
	f := &fakeSender{keys: []string{"TOKEN123", "/services/T0/XYZ"}, answer: func(_ context.Context, call int, _ events.Event) error {
		if call == 1 {
			return again(&url.Error{Op: "Post", URL: "https://h/botTOKEN123/x", Err: errors.New("dial tcp: refused")}, 0)
		}
		return dropped(errors.New("the hook at https://h/services/T0/XYZ/abc said no to TOKEN123"))
	}}
	// The first webhook cannot be made and says its URL; the second is made,
	// says no secret of its own, and fails saying its URL, its key alone, a
	// path segment alone and its path as JSON escapes it.
	hooks := func(s api.EventSink, _ env) (sender, error) {
		if strings.Contains(s.URL, "QWERTY") {
			return nil, fmt.Errorf("cannot post to %s: %w", s.URL, &url.Error{Op: "Post", URL: s.URL, Err: errors.New("no route")})
		}
		return &fakeSender{answer: func(context.Context, int, events.Event) error {
			return dropped(fmt.Errorf(`502 from %s: {"error":"no route to \/services\/T2\/ASDFGH","key":"ZSECRET"}; no channel ASDFGH`, s.URL))
		}}, nil
	}
	useSenders(t, map[string]any{"telegram": f, "webhook": hooks})
	hook := api.EventSink{Type: "webhook", URL: "https://hooks.example/services/T1/QWERTY?key=QSECRET"}
	hook2 := api.EventSink{Type: "webhook", URL: "https://hooks.example/services/T2/ASDFGH?token=ZSECRET"}
	log, buf := newLog()
	dir := t.TempDir()
	d := &dialer{plan: []any{newSource(ev(1, events.AccountDead), ev(2, events.Test))}}
	n := startNotifier(t, Options{Terminals: dir, Events: api.EventsSection{Sinks: []api.EventSink{telegramSink, hook, hook2}}, Dial: d.dial, Log: log})

	eventually(t, "both to be dropped by both", func() bool {
		return statusOf(t, n, "telegram").Dropped == 2 && statusOf(t, n, "webhook-2").Dropped == 2
	})
	n.Wait(time.Now())
	raw, err := os.ReadFile(StatusPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, where := range []struct{ name, text string }{{"the log", buf.String()}, {"sinks.json", string(raw)}} {
		for _, secret := range []string{"TOKEN123", "services/T0/XYZ", "T1/QWERTY", "QSECRET", "botTOKEN", "ASDFGH", "ZSECRET"} {
			if strings.Contains(where.text, secret) {
				t.Errorf("%s holds %q: %s", where.name, secret, where.text)
			}
		}
	}
	if s := statusOf(t, n, "webhook-2"); !strings.HasPrefix(s.LastError, "502 from [redacted]: ") || s.State != stateFailing {
		t.Fatalf("the second webhook: %+v", s)
	}
	if s := statusOf(t, n, "telegram"); s.LastError != "the hook at https://h[redacted]/abc said no to [redacted]" {
		t.Fatalf("telegram's last error: %q", s.LastError)
	}
	if s := statusOf(t, n, "webhook"); s.LastError != "cannot post to [redacted]: no route" || s.Target != "https://hooks.example" {
		t.Fatalf("the webhook: %+v", s)
	}
	if !strings.Contains(buf.String(), "events: telegram failing: dial tcp: refused") {
		t.Fatalf("the log: %s", buf)
	}
}

// redact takes out the longest secret first, and none shorter than four
// bytes, which would take words out of every sentence.
func TestRedact(t *testing.T) {
	if got := redact("dial / x TOKEN123", []string{"", "/", "TOKEN123"}); got != "dial / x [redacted]" {
		t.Fatalf("redact = %q", got)
	}
	if got := redact("ABCDEFG and ABCD", []string{"ABCD", "BCDEFG"}); got != "A[redacted] and [redacted]" {
		t.Fatalf("redact = %q", got)
	}
	if got := redact("a 12345%3A67 b", []string{"12345:67"}); got != "a [redacted] b" {
		t.Fatalf("an escaped secret: %q", got)
	}
	if got := redact(`{"to":"\/hooks\/KEY1"}`, []string{"/hooks/KEY1"}); got != `{"to":"[redacted]"}` {
		t.Fatalf("a secret as JSON escapes it: %q", got)
	}
	if got := redact("x acte TOKEN1", []string{"TOKEN1", "acte"}); got != "x [redacted] [redacted]" {
		t.Fatalf("a secret inside the marker: %q", got)
	}
	if got := redact("p %2Fa q", []string{"/a"}); got != "p %2Fa q" {
		t.Fatalf("a short secret is not looked for escaped either: %q", got)
	}
}

// A sink's build error is cleaned with its own sender's secrets when the
// constructor returns one with the error, and with the secrets of every
// sink that was made; such a sender is sent nothing.
func TestABuildErrorIsCleanedWithEverySecret(t *testing.T) {
	quick(t)
	made := &fakeSender{keys: []string{"CROSSKEY1"}}
	half := &fakeSender{keys: []string{"TOKENABC"}}
	useSenders(t, map[string]any{
		"command": made,
		"telegram": func(api.EventSink, env) (sender, error) {
			return half, errors.New("the token TOKENABC is not a bot token, nor is CROSSKEY1")
		},
	})
	cmd := api.EventSink{Type: "command", Command: []string{"notify-send"}}
	d := &dialer{plan: []any{newSource(ev(1, events.AccountDead))}}
	n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{telegramSink, cmd}}, Dial: d.dial})
	eventually(t, "the command sink to send", func() bool { return len(made.delivered()) == 1 })
	if s := statusOf(t, n, "telegram"); s.LastError != "the token [redacted] is not a bot token, nor is [redacted]" || s.State != stateFailing {
		t.Fatalf("%+v", s)
	}
	if half.tries() != 0 {
		t.Fatalf("a sink that could not be made was tried %d times", half.tries())
	}
}

// cleanErr unwraps net/http's URL from an error, takes the secrets out,
// and makes it one line of at most 300 bytes.
func TestCleanErr(t *testing.T) {
	if cleanErr(nil, nil) != "" {
		t.Fatal("a nil error is not empty")
	}
	ue := &url.Error{Op: "Post", URL: "https://api.telegram.org/bot1:SECRET/sendMessage", Err: errors.New("dial tcp: refused")}
	if got := cleanErr(ue, nil); got != "dial tcp: refused" {
		t.Fatalf("a url.Error: %q", got)
	}
	if got := cleanErr(fmt.Errorf("telegram: %w", ue), nil); got != "telegram: dial tcp: refused" {
		t.Fatalf("a wrapped url.Error: %q", got)
	}
	ue2 := &url.Error{Op: "Get", URL: "https://api.telegram.org/bot1:SECRET/getMe", Err: errors.New("timeout")}
	if got := cleanErr(errors.Join(ue, fmt.Errorf("again: %w", ue2)), nil); got != "dial tcp: refused; again: timeout" {
		t.Fatalf("joined url.Errors: %q", got)
	}
	nested := &url.Error{Op: "Post", URL: "https://a.example/one", Err: fmt.Errorf("redirect: %w", ue2)}
	if got := cleanErr(nested, nil); got != "redirect: timeout" {
		t.Fatalf("a url.Error inside another: %q", got)
	}
	if got := cleanErr(errors.Join(errors.New("first\r"), errors.New("  second\tthird ")), nil); got != "first; second third" {
		t.Fatalf("lines: %q", got)
	}
	long := cleanErr(errors.New(strings.Repeat("é", 400)), nil)
	if len(long) > 300 || !strings.HasSuffix(long, "…") {
		t.Fatalf("a long error is %d bytes: %q", len(long), long)
	}
}
