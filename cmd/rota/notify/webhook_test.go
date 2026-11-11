package notify

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/events"
)

// hookKeyFile writes key to a file only its owner may read, and gives its
// path.
func hookKeyFile(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.secret")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// hookNew makes a webhook sink to raw, signed with the key in keyFile when
// that is not "", and fails the test when it cannot be made.
func hookNew(t *testing.T, raw, keyFile string) *webhook {
	t.Helper()
	s, err := newWebhook(api.EventSink{Type: "webhook", URL: raw, SecretFile: keyFile}, env{version: "1.4.0-test"})
	if err != nil {
		t.Fatalf("newWebhook: %v", err)
	}
	w, ok := s.(*webhook)
	if !ok {
		t.Fatalf("newWebhook made a %T", s)
	}
	return w
}

// hookEvent is an event as a webhook gets one.
func hookEvent() events.Event {
	return events.Event{
		ID: 1762800000000123, Type: events.SessionAttention, Time: time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC),
		Account: 2, Session: "s-42", Name: "refactor", Text: "is waiting for your permission", Reason: "permission",
	}
}

// hookSend sends one event within five seconds.
func hookSend(t *testing.T, w *webhook) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return w.send(ctx, hookEvent())
}

// hookClosedPort is a loopback port nothing listens on.
func hookClosedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// hookSeams puts the resolver and proxy back after a test.
func hookSeams(t *testing.T) {
	t.Helper()
	r, p := hookResolver, hookProxy
	t.Cleanup(func() { hookResolver, hookProxy = r, p })
}

// A webhook posts the event's JSON, says who sends it and what it is, and
// signs the body with HMAC-SHA256 of the key, which is trimmed.
func TestAWebhookIsSignedAndLabelled(t *testing.T) {
	const key = "hook-key-0123456789"
	type seen struct {
		method, path, query string
		header              http.Header
		body                []byte
	}
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), b}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := hookNew(t, srv.URL+"/rota/hook?k=v", hookKeyFile(t, "  "+key+"\n"))
	if err := hookSend(t, w); err != nil {
		t.Fatalf("send: %v", err)
	}
	r := <-got
	if r.method != http.MethodPost || r.path != "/rota/hook" || r.query != "k=v" {
		t.Fatalf("the request was %s %s?%s", r.method, r.path, r.query)
	}
	for name, want := range map[string]string{
		"Content-Type":    "application/json",
		"User-Agent":      "rota/1.4.0-test",
		"X-Rota-Event":    events.SessionAttention,
		"X-Rota-Delivery": "1762800000000123",
	} {
		if g := r.header.Get(name); g != want {
			t.Errorf("%s is %q, want %q", name, g, want)
		}
	}
	sig, ok := strings.CutPrefix(r.header.Get("X-Rota-Signature"), "sha256=")
	if !ok {
		t.Fatalf("X-Rota-Signature is %q", r.header.Get("X-Rota-Signature"))
	}
	have, err := hex.DecodeString(sig)
	if err != nil {
		t.Fatalf("the signature is not hex: %q", sig)
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(r.body)
	if !hmac.Equal(have, mac.Sum(nil)) {
		t.Fatal("the signature is not HMAC-SHA256 of the body with the trimmed key")
	}
	var e events.Event
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("the body %q is not an event: %v", r.body, err)
	}
	want := hookEvent()
	if e.ID != want.ID || e.Type != want.Type || !e.Time.Equal(want.Time) || e.Account != want.Account ||
		e.Session != want.Session || e.Name != want.Name || e.Text != want.Text || e.Reason != want.Reason {
		t.Fatalf("the body is %+v, want %+v", e, want)
	}
	if strings.Contains(string(r.body), "\n") {
		t.Fatalf("the body %q is not one line", r.body)
	}
}

// A webhook with no secret_file sends no signature.
func TestAWebhookWithoutASecretIsUnsigned(t *testing.T) {
	got := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	w := hookNew(t, srv.URL+"/h", "")
	if err := hookSend(t, w); err != nil {
		t.Fatalf("send: %v", err)
	}
	h := <-got
	if _, ok := h["X-Rota-Signature"]; ok {
		t.Fatalf("an unsigned webhook sent X-Rota-Signature %q", h.Get("X-Rota-Signature"))
	}
	if h.Get("X-Rota-Event") != events.SessionAttention {
		t.Fatalf("X-Rota-Event is %q", h.Get("X-Rota-Event"))
	}
}

// 408, 429 and 5xx are tried again, after the server's Retry-After when it gives
// one, as seconds or a date, and never longer than maxAfter. Any other
// answer is dropped, a redirect too: none is followed.
func TestServerErrorsAreTriedAgainAndOthersAreNot(t *testing.T) {
	var followed atomic.Int32
	date := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/503":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/429":
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/429-date":
			w.Header().Set("Retry-After", date)
			w.WriteHeader(http.StatusTooManyRequests)
		case "/429-long":
			w.Header().Set("Retry-After", "999999")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/429-huge":
			w.Header().Set("Retry-After", "99999999999999999999")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/429-junk":
			w.Header().Set("Retry-After", "soon")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/502-past":
			w.Header().Set("Retry-After", "Mon, 01 Jan 2001 00:00:00 GMT")
			w.WriteHeader(http.StatusBadGateway)
		case "/408":
			w.WriteHeader(http.StatusRequestTimeout)
		case "/404":
			w.WriteHeader(http.StatusNotFound)
		case "/400":
			w.WriteHeader(http.StatusBadRequest)
		case "/302":
			http.Redirect(w, r, "/followed", http.StatusFound)
		case "/308":
			http.Redirect(w, r, "/followed", http.StatusPermanentRedirect)
		case "/followed":
			followed.Add(1)
			w.WriteHeader(http.StatusOK)
		case "/big":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(strings.Repeat("x", 1<<20)))
		}
	}))
	defer srv.Close()

	for _, c := range []struct {
		path  string
		again bool
		after func(time.Duration) bool
		says  string
	}{
		{"/503", true, func(d time.Duration) bool { return d == 0 }, "webhook answered 503 Service Unavailable"},
		{"/429", true, func(d time.Duration) bool { return d == 2*time.Second }, "webhook answered 429 Too Many Requests"},
		{"/429-date", true, func(d time.Duration) bool { return d > 80*time.Second && d <= 90*time.Second }, "webhook answered 429 Too Many Requests"},
		{"/429-long", true, func(d time.Duration) bool { return d == maxAfter }, "webhook answered 429 Too Many Requests"},
		{"/429-huge", true, func(d time.Duration) bool { return d == maxAfter }, "webhook answered 429 Too Many Requests"},
		{"/429-junk", true, func(d time.Duration) bool { return d == 0 }, "webhook answered 429 Too Many Requests"},
		{"/502-past", true, func(d time.Duration) bool { return d == 0 }, "webhook answered 502 Bad Gateway"},
		{"/big", true, func(d time.Duration) bool { return d == 0 }, "webhook answered 500 Internal Server Error"},
		{"/408", true, func(d time.Duration) bool { return d == 0 }, "webhook answered 408 Request Timeout"},
		{"/404", false, nil, "webhook answered 404 Not Found"},
		{"/400", false, nil, "webhook answered 400 Bad Request"},
		{"/302", false, nil, "webhook answered 302 Found"},
		{"/308", false, nil, "webhook answered 308 Permanent Redirect"},
	} {
		t.Run(strings.TrimPrefix(c.path, "/"), func(t *testing.T) {
			err := hookSend(t, hookNew(t, srv.URL+c.path, ""))
			if err == nil || err.Error() != c.says {
				t.Fatalf("send = %v, want %q", err, c.says)
			}
			var r *retry
			var d *drop
			if c.again {
				if !errors.As(err, &r) {
					t.Fatalf("%v is not tried again", err)
				}
				if !c.after(r.after) {
					t.Fatalf("%v waits %v", err, r.after)
				}
			} else if !errors.As(err, &d) {
				t.Fatalf("%v is not dropped", err)
			}
		})
	}
	if n := followed.Load(); n != 0 {
		t.Fatalf("a redirect was followed %d times", n)
	}
}

// net/http repeats the whole URL in its errors; what a webhook says of a
// failure holds none of it, and a refused connection is tried again.
func TestAWebhookErrorHoldsNoURL(t *testing.T) {
	raw := "http://127.0.0.1:" + strconv.Itoa(hookClosedPort(t)) + "/services/T0/XYZ?k=SECRET"
	w := hookNew(t, raw, hookKeyFile(t, "KEYKEYKEY"))
	err := hookSend(t, w)
	if err == nil {
		t.Fatal("a closed port took the event")
	}
	var r *retry
	if !errors.As(err, &r) {
		t.Fatalf("%v is not tried again", err)
	}
	for _, said := range []string{err.Error(), cleanErr(err, w.secrets())} {
		for _, secret := range []string{"XYZ", "SECRET", "services/T0", "KEYKEYKEY"} {
			if strings.Contains(said, secret) {
				t.Fatalf("%q holds %q", said, secret)
			}
		}
	}
	keys := w.secrets()
	for _, want := range []string{raw, "/services/T0/XYZ", "k=SECRET", "KEYKEYKEY"} {
		if !slices.Contains(keys, want) {
			t.Fatalf("secrets() %q lack %q", keys, want)
		}
	}
}

// A secret file others may read is refused with owner.Only's own words, and
// the sink holds no key; one that holds nothing, or too much, is refused
// too, and none of it is quoted.
func TestASecretFileOthersCanReadIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no group or other bits")
	}
	path := hookKeyFile(t, "OPENKEY-123")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := newWebhook(api.EventSink{Type: "webhook", URL: "https://hooks.example/h", SecretFile: path}, env{})
	if err == nil || !strings.Contains(err.Error(), path+" is readable or writable by other users (mode 0644)") ||
		!strings.Contains(err.Error(), "chmod 600 "+path) {
		t.Fatalf("newWebhook = %v", err)
	}
	if s != nil {
		t.Fatalf("a refused sink was made: %+v", s)
	}
	if strings.Contains(err.Error(), "OPENKEY") {
		t.Fatalf("%v quotes the key", err)
	}

	for _, c := range []struct{ body, says string }{
		{" \n\t", "holds no secret"},
		{strings.Repeat("k", 4097), "holds more than 4096 bytes"},
	} {
		p := hookKeyFile(t, c.body)
		s, err := newWebhook(api.EventSink{Type: "webhook", URL: "https://hooks.example/h", SecretFile: p}, env{})
		if s != nil || err == nil || err.Error() != "secret_file "+p+" "+c.says {
			t.Fatalf("newWebhook = %v, %v; want %q", s, err, c.says)
		}
	}
	missing := filepath.Join(t.TempDir(), "none")
	if s, err := newWebhook(api.EventSink{Type: "webhook", URL: "https://hooks.example/h", SecretFile: missing}, env{}); s != nil || err == nil {
		t.Fatalf("a missing secret file gave %v, %v", s, err)
	}
}

// Only an http or https address is a webhook, and no error quotes it.
func TestAWebhookIsAnHTTPAddress(t *testing.T) {
	for _, raw := range []string{"ftp://h.example/KEY123", "https:///KEY123", "://KEY123", "https://u:KEY123@h.example/x", ""} {
		s, err := newWebhook(api.EventSink{Type: "webhook", URL: raw}, env{})
		if s != nil || err == nil {
			t.Fatalf("newWebhook(%q) = %v, %v", raw, s, err)
		}
		if strings.Contains(err.Error(), "KEY123") {
			t.Fatalf("%v quotes the address", err)
		}
	}
}

// hookDNS is a name server a resolver reaches through Dial, in memory: it
// answers each A question from names, and every other with no answer. It
// counts the questions it gets, so a test knows the resolver asked it.
type hookDNS struct {
	names map[string]netip.Addr
	asked atomic.Int32
}

func (d *hookDNS) resolver() *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		go d.serve(b)
		return a, nil
	}}
}

// serve answers DNS over a stream: each message is its length in two bytes,
// then the message.
func (d *hookDNS) serve(c net.Conn) {
	defer c.Close()
	for {
		var n [2]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(n[:]))
		if _, err := io.ReadFull(c, q); err != nil || len(q) < 12 {
			return
		}
		d.asked.Add(1)
		// The question: labels from byte 12 to a zero, then type and class.
		i, labels := 12, []string{}
		for i < len(q) && q[i] != 0 {
			l := int(q[i])
			if i+1+l > len(q) {
				return
			}
			labels = append(labels, string(q[i+1:i+1+l]))
			i += 1 + l
		}
		end := i + 5
		if end > len(q) {
			return
		}
		qtype := binary.BigEndian.Uint16(q[i+1 : i+3])
		addr, known := d.names[strings.ToLower(strings.Join(labels, "."))]
		answer := known && qtype == 1 && addr.Is4()
		m := []byte{q[0], q[1], 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}
		if answer {
			m[7] = 1
		}
		m = append(m, q[12:end]...)
		if answer {
			ip := addr.As4()
			m = append(m, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
			m = append(m, ip[:]...)
		}
		out := binary.BigEndian.AppendUint16(nil, uint16(len(m)))
		if _, err := c.Write(append(out, m...)); err != nil {
			return
		}
	}
}

// Plain http goes only to this machine, and that is checked at the address
// dialled, after the name is resolved: a name that resolves elsewhere sends
// nothing, and is dropped, since trying again would not mend it. The name
// is resolved by a name server in memory and the refused address is one no
// packet is sent to.
func TestPlainHTTPIsOnlyForThisMachine(t *testing.T) {
	hookSeams(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	dns := &hookDNS{names: map[string]netip.Addr{
		"rota-away.example": netip.MustParseAddr("192.0.2.7"),
		"rota-here.example": netip.MustParseAddr("127.0.0.1"),
	}}
	hookResolver = dns.resolver()
	hookProxy = func(*http.Request) (*url.URL, error) {
		t.Error("a plain http webhook asked for a proxy")
		return nil, nil
	}

	for _, raw := range []string{
		"http://rota-away.example:" + strconv.Itoa(port) + "/h",
		"http://192.0.2.7:" + strconv.Itoa(port) + "/h",
	} {
		err := hookSend(t, hookNew(t, raw, ""))
		var d *drop
		if err == nil || !errors.As(err, &d) || !strings.Contains(err.Error(), "webhook: plain http only to this machine") {
			t.Fatalf("%s: send = %v; want it dropped as off this machine", raw, err)
		}
		if strings.Contains(err.Error(), "/h") || strings.Contains(err.Error(), "rota-away") {
			t.Fatalf("%v holds the URL", err)
		}
	}
	if dns.asked.Load() == 0 {
		t.Fatal("the name server in memory was never asked")
	}

	if err := hookSend(t, hookNew(t, "http://rota-here.example:"+strconv.Itoa(port)+"/h", "")); err != nil {
		t.Fatalf("a name that resolves to this machine: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the server here got %d requests, want 1", n)
	}
}

// An https webhook goes through the proxy the environment names, checks
// the certificate it is shown, and is held to no address of its own. No
// name is resolved but by a name server in memory that knows none, so a
// webhook that skipped its proxy fails here without leaving the machine.
func TestAnHTTPSWebhookUsesTheEnvironmentsProxy(t *testing.T) {
	hookSeams(t)
	if reflect.ValueOf(hookProxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Fatal("hookProxy is not http.ProxyFromEnvironment")
	}

	var mu sync.Mutex
	var asked []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	hookResolver = (&hookDNS{}).resolver()
	var calls atomic.Int32
	hookProxy = func(r *http.Request) (*url.URL, error) {
		calls.Add(1)
		return pu, nil
	}
	err := hookSend(t, hookNew(t, "https://rota-hook.example/services/T0/XYZ", ""))
	if err == nil {
		t.Fatal("a proxy that refused passed the event on")
	}
	if strings.Contains(err.Error(), "XYZ") {
		t.Fatalf("%v holds the URL", err)
	}
	mu.Lock()
	got := strings.Join(asked, ", ")
	mu.Unlock()
	if calls.Load() == 0 || got != "CONNECT rota-hook.example:443" {
		t.Fatalf("the proxy was asked %d times and got %q", calls.Load(), got)
	}

	// A certificate nobody vouches for is refused before a byte of the
	// request is sent, and tried again: a clock not yet set at boot, or a
	// captive portal, mends itself.
	hookProxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	var reached atomic.Int32
	tlsSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	err = hookSend(t, hookNew(t, tlsSrv.URL+"/h", ""))
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("a certificate nobody vouches for gave %v", err)
	}
	var r *retry
	if !errors.As(err, &r) {
		t.Fatalf("%v is not tried again", err)
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the server behind a refused certificate got %d requests", n)
	}
}

// The rule of plain http, address by address: this machine's own loopback
// and nothing else, not a private, link-local, shared or unspecified one,
// nor a v4 address mapped into or kept in v6.
func TestPlainHTTPIsLoopbackOnly(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:80": true, "127.255.255.254:80": true, "[::1]:80": true, "[::ffff:127.0.0.1]:80": true,
		"[::ffff:10.0.0.1]:80": false, "0.0.0.0:80": false, "[::]:80": false, "[::ffff:0.0.0.0]:80": false,
		"169.254.169.254:80": false, "[fe80::1%lo0]:80": false, "10.0.0.1:80": false,
		"192.168.1.1:80": false, "172.16.0.1:80": false, "100.64.0.1:80": false, "[fc00::1]:80": false,
		"[::127.0.0.1]:80": false, "[64:ff9b::7f00:1]:80": false, "localhost:80": false, "garbage": false,
	} {
		if err := hookHere("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("%s: %v, want allowed=%v", addr, err, ok)
		}
	}
}

// An answer's body is read only in part, so one that never ends holds no
// attempt.
func TestAnAnswersBodyIsReadOnlyInPart(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(strings.Repeat("x", 8192)))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	if err := hookSend(t, hookNew(t, srv.URL+"/h", "")); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("an endless body held the send %v", d)
	}
}

// What the server says after its status code is never said: the code is,
// by Go's name for it.
func TestTheServersReasonIsNotSaid(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				bufio.NewReader(c).ReadString('\n')
				io.WriteString(c, "HTTP/1.1 503 QSECRET99 down\r\nContent-Length: 0\r\n\r\n")
			}()
		}
	}()
	err = hookSend(t, hookNew(t, "http://"+l.Addr().String()+"/h", ""))
	if err == nil || err.Error() != "webhook answered 503 Service Unavailable" {
		t.Fatalf("send = %v", err)
	}
}

// Each answer is read and closed, so one connection carries many events.
func TestOneConnectionCarriesManyEvents(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	w := hookNew(t, srv.URL+"/h", "")
	for range 3 {
		if err := hookSend(t, w); err != nil {
			t.Fatal(err)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("3 events took %d connections", n)
	}
}

// A send ends when its attempt does, though the server never answers.
func TestAWebhookSendEndsWithItsAttempt(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	w := hookNew(t, srv.URL+"/h", "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := w.send(ctx, hookEvent())
	if err == nil {
		t.Fatal("a server that never answered took the event")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("send took %v after its attempt ended", d)
	}
}
