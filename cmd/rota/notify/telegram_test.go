package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/events"
)

// tgToken is the bot token every test here writes to its token file.
const tgToken = "123456:AAH-tgSecretPart_xyz"

// tgTokenFile writes body to a token file only its owner may read, and
// says where it is.
func tgTokenFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telegram.token")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tgAsked is one request the fake Bot API got.
type tgAsked struct {
	method, path, contentType string
	body                      map[string]json.RawMessage
}

// tgFake is a loopback Bot API: it keeps each request and answers with
// answer, or with {"ok":true} when answer is nil. telegramAPI points at it
// for the test.
type tgFake struct {
	mu     sync.Mutex
	asked  []tgAsked
	answer func(w http.ResponseWriter, r *http.Request)
}

func tgServe(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *tgFake {
	t.Helper()
	f := &tgFake{answer: answer}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		a := tgAsked{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type")}
		_ = json.Unmarshal(raw, &a.body)
		f.mu.Lock()
		f.asked = append(f.asked, a)
		f.mu.Unlock()
		if f.answer != nil {
			f.answer(w, r)
			return
		}
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	t.Cleanup(srv.Close)
	tgPoint(t, srv.URL)
	return f
}

// tgPoint points telegramAPI at base for one test.
func tgPoint(t *testing.T, base string) {
	t.Helper()
	was := telegramAPI
	t.Cleanup(func() { telegramAPI = was })
	telegramAPI = base
}

func (f *tgFake) requests() []tgAsked {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tgAsked(nil), f.asked...)
}

// tgText is the text of the last message the fake got.
func (f *tgFake) tgText(t *testing.T) string {
	t.Helper()
	got := f.requests()
	if len(got) == 0 {
		t.Fatal("telegram got nothing")
	}
	var s string
	if err := json.Unmarshal(got[len(got)-1].body["text"], &s); err != nil {
		t.Fatalf("text: %v", err)
	}
	return s
}

// tgSink makes a telegram sender with link, on a token file of its own.
func tgSink(t *testing.T, link string) sender {
	t.Helper()
	s, err := newTelegram(api.EventSink{Type: "telegram", BotTokenFile: tgTokenFile(t, tgToken+"\n"), ChatID: "123"}, env{link: link})
	if err != nil {
		t.Fatalf("newTelegram: %v", err)
	}
	return s
}

func tgAttention(session string) events.Event {
	return events.Event{ID: 1, Type: events.SessionAttention, Time: time.Now().UTC(), Session: session,
		Name: "refactor", Text: "is waiting for your permission", Reason: "permission"}
}

// A message is the event's sentence and, when [events] link is set and the
// event is a session's, a second line linking that session's terminal.
func TestAMessageIsTheSentenceAndTheLink(t *testing.T) {
	f := tgServe(t, nil)
	s := tgSink(t, "https://box:8787")
	if err := s.send(t.Context(), tgAttention("s1abc")); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := f.requests()
	if len(got) != 1 {
		t.Fatalf("telegram got %d requests, want 1", len(got))
	}
	a := got[0]
	if a.method != http.MethodPost || a.path != "/bot"+tgToken+"/sendMessage" {
		t.Fatalf("asked %s %s", a.method, a.path)
	}
	if !strings.HasPrefix(a.contentType, "application/json") {
		t.Fatalf("Content-Type %q", a.contentType)
	}
	if c := string(a.body["chat_id"]); c != "123" {
		t.Fatalf("chat_id %s, want 123", c)
	}
	if p := string(a.body["disable_web_page_preview"]); p != "true" {
		t.Fatalf("disable_web_page_preview %s, want true", p)
	}
	if len(a.body) != 3 {
		t.Fatalf("the body has more than chat_id, text and disable_web_page_preview: %v", a.body)
	}
	if want := "refactor is waiting for your permission\nhttps://box:8787/terminal#s1abc"; f.tgText(t) != want {
		t.Fatalf("text %q, want %q", f.tgText(t), want)
	}

	// A link written with its closing slash links the same place.
	if err := tgSink(t, "https://box:8787/").send(t.Context(), tgAttention("s1abc")); err != nil {
		t.Fatal(err)
	}
	if want := "refactor is waiting for your permission\nhttps://box:8787/terminal#s1abc"; f.tgText(t) != want {
		t.Fatalf("text %q, want %q", f.tgText(t), want)
	}

	// No link, or no session: one line.
	for _, c := range []struct {
		link, session string
	}{{"", "s1abc"}, {"https://box:8787", ""}} {
		if err := tgSink(t, c.link).send(t.Context(), tgAttention(c.session)); err != nil {
			t.Fatal(err)
		}
		if want := "refactor is waiting for your permission"; f.tgText(t) != want {
			t.Fatalf("link %q session %q: text %q, want %q", c.link, c.session, f.tgText(t), want)
		}
	}

	// A chat that is not a number, such as a channel's @name, is sent as
	// the string it is.
	s, err := newTelegram(api.EventSink{Type: "telegram", BotTokenFile: tgTokenFile(t, tgToken), ChatID: " @rota_news "}, env{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.send(t.Context(), tgAttention("")); err != nil {
		t.Fatal(err)
	}
	got = f.requests()
	if c := string(got[len(got)-1].body["chat_id"]); c != `"@rota_news"` {
		t.Fatalf("chat_id %s, want \"@rota_news\"", c)
	}

	// A number written with a sign or leading zeros is sent as JSON writes
	// it, not as typed, which JSON would refuse.
	for _, c := range []struct{ typed, sent string }{{"+123", "123"}, {"0123", "123"}, {"-1001234", "-1001234"}} {
		s, err := newTelegram(api.EventSink{Type: "telegram", BotTokenFile: tgTokenFile(t, tgToken), ChatID: c.typed}, env{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.send(t.Context(), tgAttention("")); err != nil {
			t.Fatalf("chat_id %q: %v", c.typed, err)
		}
		got = f.requests()
		if id := string(got[len(got)-1].body["chat_id"]); id != c.sent {
			t.Fatalf("chat_id %q was sent as %s, want %s", c.typed, id, c.sent)
		}
	}
}

// A message longer than Telegram takes is cut at 4096 runes, on a rune's
// boundary, never inside one.
func TestALongMessageIsCut(t *testing.T) {
	f := tgServe(t, nil)
	s := tgSink(t, "")
	long := string([]rune(strings.Repeat("жé世", 1667))[:5000])
	e := events.Event{ID: 1, Type: events.Test, Time: time.Now().UTC(), Text: long}
	if err := s.send(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	text := f.tgText(t)
	if !utf8.ValidString(text) {
		t.Fatal("the cut text is not UTF-8")
	}
	if n := utf8.RuneCountInString(text); n != 4096 {
		t.Fatalf("%d runes, want 4096", n)
	}
	if want := string([]rune(long)[:4095]); !strings.HasPrefix(text, want) {
		t.Fatal("the cut text is not the start of the message")
	}

	// 4096 runes exactly are sent whole.
	whole := string([]rune(long)[:4096])
	if err := s.send(t.Context(), events.Event{ID: 2, Type: events.Test, Time: time.Now().UTC(), Text: whole}); err != nil {
		t.Fatal(err)
	}
	if f.tgText(t) != whole {
		t.Fatal("a message of 4096 runes was changed")
	}

	// Telegram counts UTF-16 units, two for a rune beyond the BMP: 5000
	// emoji are cut to at most 4096 units, on a rune's boundary.
	emoji := strings.Repeat("😀", 5000)
	if err := s.send(t.Context(), events.Event{ID: 3, Type: events.Test, Time: time.Now().UTC(), Text: emoji}); err != nil {
		t.Fatal(err)
	}
	text = f.tgText(t)
	if units := len(utf16.Encode([]rune(text))); !utf8.ValidString(text) || units > 4096 || units < 4094 {
		t.Fatalf("5000 emoji were cut to %d UTF-16 units (valid UTF-8: %v)", units, utf8.ValidString(text))
	}
	if cut := strings.TrimSuffix(text, "…"); cut == text || !strings.HasPrefix(emoji, cut) {
		t.Fatal("the cut emoji are not the start of the message and an ellipsis")
	}
}

// tgAnswer answers with code and body.
func tgAnswer(code int, body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

// What Telegram answers says whether the message is tried again, and when:
// a 429 after its retry_after, up to maxAfter; a 5xx after backoff's wait;
// a refusal never.
func TestTelegramSaysWhenToTryAgain(t *testing.T) {
	type want int
	const (
		sent want = iota
		retried
		droppedIt
	)
	for _, c := range []struct {
		code  int
		body  string
		want  want
		after time.Duration
		says  string
	}{
		{200, `{"ok":true,"result":{}}`, sent, 0, ""},
		{200, `not json`, sent, 0, ""},
		{429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`, retried, 3 * time.Second, "telegram answered 429: Too Many Requests: retry after 3"},
		{429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 9999","parameters":{"retry_after":9999}}`, retried, maxAfter, ""},
		{429, `{"ok":false,"error_code":429,"parameters":{"retry_after":9223372037}}`, retried, maxAfter, ""},
		{429, `{"ok":false,"error_code":429}`, retried, 0, "telegram answered 429: Too Many Requests"},
		{500, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`, retried, 0, "telegram answered 500: Internal Server Error"},
		{408, `{"ok":false,"error_code":408,"description":"Request Timeout"}`, retried, 0, "telegram answered 408: Request Timeout"},
		// An answer is read to 64 KiB: past that its JSON is cut and not
		// read, and the status line says it.
		{500, `{"ok":false,"error_code":500,"description":"` + strings.Repeat("x", 70<<10) + `"}`, retried, 0, "telegram answered 500: Internal Server Error"},
		{502, `<html>bad gateway</html>`, retried, 0, "telegram answered 502: Bad Gateway"},
		{401, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, droppedIt, 0, "telegram answered 401: Unauthorized"},
		{400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, droppedIt, 0, "telegram answered 400: Bad Request: chat not found"},
		{403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`, droppedIt, 0, "telegram answered 403: Forbidden: bot was blocked by the user"},
		{404, `{"ok":false,"error_code":404,"description":"Not Found"}`, droppedIt, 0, "telegram answered 404: Not Found"},
		{200, `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`, droppedIt, 0, "telegram answered 400: Bad Request: message is too long"},
	} {
		tgServe(t, tgAnswer(c.code, c.body))
		s := tgSink(t, "")
		err := s.send(t.Context(), tgAttention("s1"))
		var r *retry
		var d *drop
		switch c.want {
		case sent:
			if err != nil {
				t.Fatalf("%d %s: %v, want sent", c.code, c.body, err)
			}
			continue
		case retried:
			if !errors.As(err, &r) {
				t.Fatalf("%d %s: %v, want again", c.code, c.body, err)
			}
			if r.after != c.after {
				t.Fatalf("%d %s: again after %v, want %v", c.code, c.body, r.after, c.after)
			}
		case droppedIt:
			if !errors.As(err, &d) {
				t.Fatalf("%d %s: %v, want dropped", c.code, c.body, err)
			}
		}
		if got := cleanErr(err, s.secrets()); c.says != "" && got != c.says {
			t.Fatalf("%d %s: says %q, want %q", c.code, c.body, got, c.says)
		}
	}

	// With no retry_after, a Retry-After header says the wait.
	tgServe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	var r *retry
	if err := tgSink(t, "").send(t.Context(), tgAttention("s1")); !errors.As(err, &r) || r.after != 7*time.Second {
		t.Fatalf("a Retry-After of 7 gave %v", err)
	}

	// A redirect is not followed: the token would go wherever it points.
	var followed bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer other.Close()
	tgServe(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	s := tgSink(t, "")
	err := s.send(t.Context(), tgAttention("s1"))
	var d *drop
	if !errors.As(err, &d) || followed {
		t.Fatalf("a redirect gave %v, followed %v; want dropped, not followed", err, followed)
	}
	if got := cleanErr(err, s.secrets()); got != "telegram answered 307: Temporary Redirect" {
		t.Fatalf("a redirect says %q", got)
	}
}

// tgNoToken fails the test if what holds any form of the token.
func tgNoToken(t *testing.T, where, what string) {
	t.Helper()
	for _, part := range []string{tgToken, "AAH-tgSecretPart_xyz", "123456%3A", "123456%3a", "123456:A", "bot1234"} {
		if strings.Contains(what, part) {
			t.Fatalf("%s holds the token (%q): %s", where, part, what)
		}
	}
}

// net/http repeats a request's whole URL in its errors, and the URL holds
// the token: no error the sink returns says it, before cleanErr or after.
func TestADialErrorDoesNotRepeatTheToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	ln.Close()
	tgPoint(t, closed)
	s := tgSink(t, "")
	err = s.send(t.Context(), tgAttention("s1"))
	if err == nil {
		t.Fatal("a closed port took the message")
	}
	var d *drop
	if errors.As(err, &d) {
		t.Fatalf("a dial error is dropped, not tried again: %v", err)
	}
	tgNoToken(t, "the dial error", err.Error())
	got := cleanErr(err, s.secrets())
	tgNoToken(t, "the cleaned dial error", got)
	if !strings.Contains(got, "telegram") {
		t.Fatalf("the error does not say whose it is: %q", got)
	}
	for _, x := range s.secrets() {
		if strings.Contains(got, x) {
			t.Fatalf("a secret is in %q", got)
		}
	}
	if !slices.Contains(s.secrets(), tgToken) || !slices.Contains(s.secrets(), "bot"+tgToken) {
		t.Fatalf("secrets() = %q, want the token and bot<token>", s.secrets())
	}
}

// No answer repeats the token either: not a 5xx whose body echoes the path,
// as a proxy's page may, not Telegram's own description quoting it, not a
// 401, and not an attempt that ran out of time.
func TestNoAnswerRepeatsTheToken(t *testing.T) {
	for _, answer := range []func(w http.ResponseWriter, r *http.Request){
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, "upstream failed for POST "+r.URL.Path)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			b, _ := json.Marshal(map[string]any{"ok": false, "error_code": 500, "description": "no route for " + r.URL.String()})
			w.Write(b)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			b, _ := json.Marshal(map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized: " + strings.TrimPrefix(r.URL.Path, "/")})
			w.Write(b)
		},
	} {
		tgServe(t, answer)
		s := tgSink(t, "")
		err := s.send(t.Context(), tgAttention("s1"))
		if err == nil {
			t.Fatal("the failure was taken for delivery")
		}
		tgNoToken(t, "the cleaned error", cleanErr(err, s.secrets()))
	}

	// Telegram's description is cut to be said, but only once the token is
	// out of it: a cut that falls anywhere inside bot<token> leaves no part
	// of it.
	for pad := 140; pad <= 200; pad++ {
		tgServe(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			b, _ := json.Marshal(map[string]any{"ok": false, "error_code": 401,
				"description": "Unauthorized: " + strings.Repeat("x", pad) + " " + strings.TrimPrefix(r.URL.Path, "/")})
			w.Write(b)
		})
		s := tgSink(t, "")
		err := s.send(t.Context(), tgAttention("s1"))
		tgNoToken(t, fmt.Sprintf("the error with %d bytes before the path", pad), err.Error())
		tgNoToken(t, fmt.Sprintf("the cleaned error with %d bytes before the path", pad), cleanErr(err, s.secrets()))
	}

	// An attempt bounded by its context ends when it does, and says no
	// token.
	release := make(chan struct{})
	defer close(release)
	tgServe(t, func(http.ResponseWriter, *http.Request) { <-release })
	s := tgSink(t, "")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.send(ctx, tgAttention("s1"))
	if took := time.Since(start); err == nil || took > 3*time.Second {
		t.Fatalf("a send that got no answer returned %v after %v", err, took)
	}
	tgNoToken(t, "the timed-out error", err.Error())
	tgNoToken(t, "the cleaned timed-out error", cleanErr(err, s.secrets()))
}

// A token file is its owner's alone, holds a token, and is a plain file;
// what it holds is never said when it is refused.
func TestATokenFileOthersCanReadIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no mode bits for owner.Only to read")
	}
	open := tgTokenFile(t, tgToken)
	if err := os.Chmod(open, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := newTelegram(api.EventSink{Type: "telegram", BotTokenFile: open, ChatID: "1"}, env{})
	if err == nil || !strings.Contains(err.Error(), "chmod 600 "+open) {
		t.Fatalf("a token file others can read gave %v", err)
	}
	tgNoToken(t, "the refusal", err.Error())

	// A link to a file others can read is refused too, by the link's name,
	// which chmod follows to the file.
	openLink := filepath.Join(t.TempDir(), "tg.token")
	if err := os.Symlink(open, openLink); err != nil {
		t.Fatal(err)
	}
	_, err = newTelegram(api.EventSink{Type: "telegram", BotTokenFile: openLink, ChatID: "1"}, env{})
	if err == nil || !strings.Contains(err.Error(), "chmod 600 "+openLink) {
		t.Fatalf("a link to a token file others can read gave %v", err)
	}
	tgNoToken(t, "the refusal", err.Error())

	// A link to a file of its owner's is followed, and read.
	target := tgTokenFile(t, tgToken)
	link := filepath.Join(t.TempDir(), "tg.token")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newTelegram(api.EventSink{Type: "telegram", BotTokenFile: link, ChatID: "1"}, env{}); err != nil {
		t.Fatalf("a link to an owner's file: %v", err)
	}
}

// A token file that is missing, empty, too long, or not a plain file makes
// no sink, and the error names the file but not what is in it.
func TestABadTokenFileMakesNoSink(t *testing.T) {
	empty := tgTokenFile(t, " \n\t\n")
	long := tgTokenFile(t, "123:"+strings.Repeat("Q", 300))
	odd := tgTokenFile(t, "123:abc/../x?y#z")
	huge := tgTokenFile(t, tgToken+strings.Repeat(" ", 5000)+"QQQQJUNK")
	missing := filepath.Join(t.TempDir(), "none")
	dir := t.TempDir()
	notToken := func(body string) string { return tgTokenFile(t, body) }
	prefixed, noID, noKey, wordID := notToken("bot123:ABC"), notToken(":abc"), notToken("123:"), notToken("abc:def")
	for _, c := range []struct {
		path, says string
	}{
		{empty, "bot_token_file " + empty + " holds no bot token"},
		{long, "bot_token_file " + long + " holds more than 256 bytes"},
		{huge, "bot_token_file " + huge + " holds more than 4096 bytes"},
		{odd, "bot_token_file " + odd + " does not hold a bot token"},
		{prefixed, "bot_token_file " + prefixed + " does not hold a bot token"},
		{noID, "bot_token_file " + noID + " does not hold a bot token"},
		{noKey, "bot_token_file " + noKey + " does not hold a bot token"},
		{wordID, "bot_token_file " + wordID + " does not hold a bot token"},
		{missing, missing},
		{dir, "bot_token_file " + dir + " is not a plain file"},
	} {
		s, err := newTelegram(api.EventSink{Type: "telegram", BotTokenFile: c.path, ChatID: "1"}, env{})
		if err == nil || s != nil {
			t.Fatalf("%s: %v, %v", c.path, s, err)
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Fatalf("%s: %q, want it to say %q", c.path, err, c.says)
		}
		for _, x := range []string{"QQQQ", "abc/../x"} {
			if strings.Contains(err.Error(), x) {
				t.Fatalf("the error quotes the file: %q", err)
			}
		}
	}
}

// Under the notifier, a Telegram sink delivers each event it is for, and
// one that cannot reach Telegram is failing with no token in the log or in
// sinks.json.
func TestATelegramSinkUnderTheNotifier(t *testing.T) {
	quick(t)
	f := tgServe(t, nil)
	sink := api.EventSink{Type: "telegram", BotTokenFile: tgTokenFile(t, tgToken), ChatID: "123", On: []string{"session.*"}}
	d := &dialer{plan: []any{newSource(tgAttention("s1"), ev(2, events.AccountDead))}}
	n := startNotifier(t, Options{Events: api.EventsSection{Link: "https://box:8787", Sinks: []api.EventSink{sink}}, Dial: d.dial})
	eventually(t, "the attention to be sent", func() bool { return statusOf(t, n, "telegram").Sent == 1 })
	settled(t, "one message", func() bool { return len(f.requests()) == 1 })
	if want := "refactor is waiting for your permission\nhttps://box:8787/terminal#s1"; f.tgText(t) != want {
		t.Fatalf("text %q, want %q", f.tgText(t), want)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tgPoint(t, "http://"+ln.Addr().String())
	ln.Close()
	log, buf := newLog()
	dir := t.TempDir()
	d = &dialer{plan: []any{newSource(tgAttention("s1"))}}
	n = startNotifier(t, Options{Terminals: dir, Log: log, Events: api.EventsSection{Sinks: []api.EventSink{sink}}, Dial: d.dial})
	eventually(t, "the attention to be dropped", func() bool { return statusOf(t, n, "telegram").Dropped == 1 })
	n.Wait(time.Now())
	if s := statusOf(t, n, "telegram"); s.State != stateFailing || !strings.HasPrefix(s.LastError, "telegram: ") {
		t.Fatalf("the sink: %+v", s)
	}
	raw, err := os.ReadFile(StatusPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "telegram") {
		t.Fatalf("the log says nothing of the sink: %s", buf)
	}
	tgNoToken(t, "the log", buf.String())
	tgNoToken(t, "sinks.json", string(raw))
}
