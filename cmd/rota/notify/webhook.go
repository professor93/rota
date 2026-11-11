package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/events"
)

// The seams of a webhook's way out, variables so a test can stand in for
// them. Neither is a key of server.toml.
var (
	// hookProxy is the proxy an https webhook goes through: the one the
	// daemon's environment names. Plain http never goes through one.
	hookProxy = http.ProxyFromEnvironment
	// hookResolver finds the addresses of a webhook's host; nil is the
	// system's.
	hookResolver *net.Resolver
)

const (
	// hookKeyMax is the most a secret_file may hold.
	hookKeyMax = 4096
	// hookReadMax is the most of an answer's body that is read, and only
	// so that its connection can be used again: nothing in it is said.
	hookReadMax = 4096
)

// errHookPlain is a plain http webhook whose host is not this machine.
var errHookPlain = errors.New("webhook: plain http only to this machine")

// webhook posts each event's JSON to one address.
type webhook struct {
	url      string
	agent    string
	key      []byte // nil: unsigned
	client   *http.Client
	keys     []string
	maxAfter time.Duration
}

// newWebhook makes a webhook sink. Its error may name the secret file but
// never quotes what is in it (newSender's contract), nor the address.
//
// It goes over https anywhere, through the environment's proxy and with
// the certificate checked as usual. It goes over http only to this
// machine, with no proxy, and that is checked at each address it dials,
// after its name is resolved, so a name that resolves elsewhere sends
// nothing in clear text. It follows no redirect.
func newWebhook(s api.EventSink, e env) (sender, error) {
	u, err := parseURL(s.URL)
	switch {
	case s.URL == "":
		return nil, errors.New("url is empty; a webhook sink posts to one address")
	case err != nil:
		return nil, errors.New("url is not an address")
	case u.Scheme != "https" && u.Scheme != "http":
		return nil, errors.New("a webhook goes over https, or over http only to this machine (127.0.0.1, ::1, localhost)")
	case u.Hostname() == "":
		return nil, errors.New("url has no host")
	case u.User != nil:
		return nil, errors.New("url holds a user or password; a webhook's key goes in secret_file")
	}
	w := &webhook{url: s.URL, agent: "rota", keys: configSecrets(s), maxAfter: maxAfter}
	if e.version != "" {
		w.agent = "rota/" + e.version
	}
	if s.SecretFile != "" {
		key, err := hookKey(s.SecretFile)
		if err != nil {
			return nil, err
		}
		w.key = key
		w.keys = append(w.keys, string(key))
	}
	w.client = &http.Client{
		Transport: hookTransport(u.Scheme == "http"),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return w, nil
}

// hookKey reads a webhook's key from path as every sink reads its secret
// file (readSecretFile). No error quotes what the file holds.
func hookKey(path string) ([]byte, error) {
	key, err := readSecretFile("secret_file", path, "secret", hookKeyMax)
	if err != nil {
		return nil, err
	}
	return []byte(key), nil
}

// hookTransport is one webhook's own way out. Its attempts are bounded by
// their context alone, attemptTimeout, so it sets no timeout of its own.
func hookTransport(plain bool) *http.Transport {
	d := &net.Dialer{KeepAlive: 30 * time.Second, Resolver: hookResolver}
	t := &http.Transport{
		DialContext:         d.DialContext,
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     30 * time.Second,
	}
	if plain {
		d.Control = hookHere
	} else {
		t.Proxy = hookProxy
		t.ForceAttemptHTTP2 = true
	}
	return t
}

// hookHere refuses to connect a plain http webhook to an address that is
// not this machine's. It runs after the name is resolved and before the
// connection is made, so nothing is sent to the address it refuses.
func hookHere(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errHookPlain
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Unmap().IsLoopback() {
		return errHookPlain
	}
	return nil
}

// send posts e. A 2xx is delivered; 408, 429 and 5xx are tried again, as
// Telegram's are, after the server's Retry-After when it gives one; a
// failure to reach the server is tried again; any other answer, a redirect
// among them, is dropped, and so is plain http to an address that is not
// this machine. Nothing of the answer's body is said, and no error holds
// the address.
func (w *webhook) send(ctx context.Context, e events.Event) error {
	body := events.JSON(e)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return dropped(w.clean(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", w.agent)
	req.Header.Set("X-Rota-Event", events.Clean(e.Type, 0))
	req.Header.Set("X-Rota-Delivery", strconv.FormatInt(e.ID, 10))
	if w.key != nil {
		mac := hmac.New(sha256.New, w.key)
		mac.Write(body)
		req.Header.Set("X-Rota-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		if errors.Is(err, errHookPlain) {
			return dropped(w.clean(err))
		}
		return again(w.clean(err), 0)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, hookReadMax))
	resp.Body.Close()
	code := resp.StatusCode
	if code >= 200 && code < 300 {
		return nil
	}
	err = fmt.Errorf("webhook answered %s", hookStatus(code))
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500 {
		return again(err, hookAfter(resp.Header.Get("Retry-After"), time.Now(), w.maxAfter))
	}
	return dropped(err)
}

func (w *webhook) secrets() []string { return w.keys }

// clean is err as it may be said: without the address, which net/http
// repeats, or any other secret of this sink.
func (w *webhook) clean(err error) error { return errors.New(cleanErr(err, w.keys)) }

// hookStatus is a status code as Go names it, not as the server's reason
// phrase does: "302 Found".
func hookStatus(code int) string {
	if text := http.StatusText(code); text != "" {
		return strconv.Itoa(code) + " " + text
	}
	return strconv.Itoa(code)
}

// hookAfter is how long a Retry-After of v asks for, as seconds or as an
// HTTP date, at most limit; a count of seconds too long to read is limit.
// One that cannot be read, or has passed, is 0: backoff's own wait. The
// queue caps it again with its own maxAfter; this cap is the sink's own.
func hookAfter(v string, now time.Time, limit time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if errors.Is(err, strconv.ErrRange) && n > 0 {
		return limit
	}
	if err == nil {
		if n <= 0 {
			return 0
		}
		if n >= int64(limit/time.Second) {
			return limit
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(t.Sub(now), 0), limit)
	}
	return 0
}
