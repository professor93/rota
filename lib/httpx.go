package rota

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Version is this SDK's own release number. It names the library, not any
// program built on it: an application has a version of its own and says so
// through UserAgent.
const Version = "1.0.4"

// UserAgent is what requests identify themselves as when the caller has not
// said otherwise. Applications set their own — `rota.UserAgent = "myapp/2.1"`
// — or override per call through a request's own header; the default names
// this SDK, because the SDK cannot know what it was built into.
var UserAgent = "rota-lib/" + Version

// HTTPClient makes every network call this SDK issues. It is exported
// because the client is the application's decision — proxies, mTLS,
// instrumentation, pooling, a different timeout — and a library that hides
// its client leaves every consumer without a seam. The default is deliberate:
// a plain client with a 30-second ceiling, so nothing hangs forever out of
// the box.
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

// maxBody bounds what will be read from any provider reply; the largest
// legitimate one is a few kilobytes.
const maxBody = 1 << 22

// randB64 returns n random bytes, URL-safe base64 encoded. crypto/rand
// cannot fail on a supported platform, so no error is threaded through.
func randB64(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// randID is a short, typeable handle for one in-flight login: six hex chars.
func randID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// pkce returns (verifier, challenge) for an S256 exchange.
func pkce() (string, string) {
	verifier := randB64(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func trimSpace(s string) string { return strings.TrimSpace(s) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// HTTPError is a reply with a failing status, carrying the status and body
// so a caller can classify the failure — 429 from 401 from 500 — with
// errors.As instead of parsing the formatted text.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, truncate(strings.TrimSpace(e.Body), 300))
}

// do performs one request and returns the body. A non-2xx status is an
// *HTTPError that still carries the body, because OAuth verdicts
// (authorization_pending, invalid_grant) arrive as JSON with a 4xx status.
// The User-Agent is filled in only when the caller set none.
func do(req *http.Request) ([]byte, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", UserAgent)
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return raw, &HTTPError{Status: resp.StatusCode, Body: string(raw)}
	}
	return raw, nil
}

// send performs the request and decodes whatever JSON came back into out —
// on a failing status too, because OAuth verdicts (authorization_pending,
// invalid_grant) arrive as JSON with a 4xx. The status error still wins.
func send(req *http.Request, out any, headers map[string]string) error {
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	raw, err := do(req)
	if out != nil && len(raw) > 0 {
		if derr := decodeLenient(raw, out); derr != nil && err == nil {
			return fmt.Errorf("decoding reply: %w", derr)
		}
	}
	return err
}

// postJSON sends a JSON body and decodes a 2xx JSON reply.
func postJSON(ctx context.Context, endpoint string, body, out any, headers map[string]string) error {
	buf, err := Encode(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return send(req, out, headers)
}

// postForm sends application/x-www-form-urlencoded, which OAuth token
// endpoints require.
func postForm(ctx context.Context, endpoint string, form url.Values, out any, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return send(req, out, headers)
}

func getJSON(ctx context.Context, endpoint, bearer string, out any, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return send(req, out, headers)
}
