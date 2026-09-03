package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// A flood of failing addresses must not lift the blocks already in place:
// the table evicts the stalest address, never everyone.
func TestTheLimiterKeepsBlocksWhenItIsFull(t *testing.T) {
	l := newLimiter()
	for range failMax {
		l.fail("attacker")
	}
	for i := range maxTracked + 10 {
		l.fail(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if !l.blocked("attacker") {
		t.Fatal("a flood of other addresses lifted the attacker's own block")
	}
	if len(l.hits) > maxTracked {
		t.Fatalf("the table grew past its cap: %d", len(l.hits))
	}
}

// One IPv6 host holds a whole /64, so that is the unit an address is
// counted by; IPv4 stays per address.
func TestTheLimiterCountsAnIPv6HostByItsPrefix(t *testing.T) {
	key := func(addr string) string { return clientIP(&http.Request{RemoteAddr: addr}) }
	if key("[2001:db8:1:2::1]:40000") != key("[2001:db8:1:2:ffff:ffff:ffff:ffff]:1") {
		t.Fatal("two addresses in one /64 must share a counter")
	}
	if key("[2001:db8:1:2::1]:1") == key("[2001:db8:1:3::1]:1") {
		t.Fatal("different /64s are different hosts")
	}
	if key("10.0.0.1:1") == key("10.0.0.2:1") {
		t.Fatal("IPv4 stays per address")
	}
}

type explodingProvider struct{}

func (explodingProvider) Name() string { return "t-api-explode" }
func (explodingProvider) Begin(_ context.Context) (string, map[string]string, error) {
	return "https://example.test/x", map[string]string{"kind": "apikey"}, nil
}
func (explodingProvider) Complete(_ context.Context, _ string, _ map[string]string) (*rota.Token, error) {
	return nil, errors.New("secret detail: proxy 10.1.2.3 refused")
}
func (explodingProvider) Launch(_ *rota.Account, _ string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}

// An error nobody classified is an internal error, said as only that. The
// text goes to the log, where the operator reads it, not to a caller who
// would learn the operator's network and file layout from it.
func TestUnclassifiedErrorsSayOnlyThatToTheCaller(t *testing.T) {
	rota.Register(explodingProvider{})
	h := newHarness(t, Options{})
	resp, raw := h.do("POST", "/v1/login", map[string]any{"provider": "t-api-explode"})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var l struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &l)
	resp, raw = h.do("POST", "/v1/login/"+l.ID, map[string]any{"code": "k"})
	if resp.StatusCode != 500 || strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), "internal error") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
}

// A login body that is broken is refused; only an absent one means the
// default provider.
func TestABrokenLoginBodyIsRefused(t *testing.T) {
	h := newHarness(t, Options{})
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/login", strings.NewReader(`{"provider":"codex"`))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("a truncated body must be refused, not read as empty: %d %s", resp.StatusCode, raw)
	}
	if resp, _ := h.do("POST", "/v1/login", nil); resp.StatusCode != 200 {
		t.Fatalf("an empty body still means the default provider: %d", resp.StatusCode)
	}
}

// The request of a multipart run is the "request" part and nothing else: a
// query string would land prompts in every access log on the way.
func TestAMultipartRunIgnoresTheQueryString(t *testing.T) {
	h := newHarness(t, Options{})
	var buf bytes.Buffer
	mw := newMultipart(&buf)
	mw.file("files", "c.md", "# c")
	ct := mw.close()
	q := url.Values{"request": {`{"prompt":"from the query"}`}}
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/accounts/1/run?"+q.Encode(), &buf)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", ct)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(raw), "prompt") {
		t.Fatalf("a prompt in the query string must not count: %d %s", resp.StatusCode, raw)
	}
}

// Replies are not for caches, and the page is not for frames.
func TestRepliesAreNotCachedAndThePageIsNotFramed(t *testing.T) {
	h := newHarness(t, Options{})
	if resp, _ := h.do("GET", "/v1/accounts", nil); resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control: %q", resp.Header.Get("Cache-Control"))
	}
	resp, err := http.Get(h.srv.URL + "/playground")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Frame-Options") != "DENY" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("frame headers: %q %q", resp.Header.Get("X-Frame-Options"), resp.Header.Get("Content-Security-Policy"))
	}
}

// config_dir is where a run stages this account's credential and what a
// DELETE removes, so a caller may not point it at rota's own directories:
// the store, or another account's home.
func TestAConfigDirMayNotBeOneOfRotasOwnDirectories(t *testing.T) {
	h := newHarness(t, Options{Roots: []string{}}) // unconfined: the store's own rule is what answers
	for _, dir := range []string{h.dir, filepath.Join(h.dir, "homes", "claude-3")} {
		resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"config_dir": dir})
		if resp.StatusCode != 400 || !strings.Contains(string(raw), "rota's own") {
			t.Fatalf("%s: %d %s", dir, resp.StatusCode, raw)
		}
	}
}

// With roots, an account cannot be pointed at a config directory the
// server was told to stay out of — not even through a link inside them.
func TestAConfigDirIsConfinedToTheRoots(t *testing.T) {
	h := newHarness(t, Options{})
	outside := t.TempDir()
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"config_dir": outside}); resp.StatusCode != 400 ||
		!strings.Contains(string(raw), "outside") {
		t.Fatalf("outside the roots: %d %s", resp.StatusCode, raw)
	}
	link := filepath.Join(h.root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"config_dir": link}); resp.StatusCode != 400 {
		t.Fatalf("a link leading outside the roots: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"config_dir": filepath.Join(h.root, "cfg")}); resp.StatusCode != 200 {
		t.Fatalf("inside a root: %d %s", resp.StatusCode, raw)
	}
}

// Removing an account deletes the home rota made for it, and nothing else:
// a directory the caller chose holds their memory and skills.
func TestRemovingAnAccountLeavesAChosenConfigDirInPlace(t *testing.T) {
	h := newHarness(t, Options{})
	mine := filepath.Join(h.root, "mine")
	own := filepath.Join(h.dir, "homes", "claude-3")
	for _, dir := range []string{mine, own} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# mine"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"config_dir": mine}); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	for _, id := range []string{"1", "3"} {
		if resp, raw := h.do("DELETE", "/v1/accounts/"+id, nil); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", id, resp.StatusCode, raw)
		}
	}
	if _, err := os.Stat(filepath.Join(mine, "CLAUDE.md")); err != nil {
		t.Fatalf("the directory the caller chose must survive its account: %v", err)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatalf("rota's own home must go with its account: %v", err)
	}
	if _, raw := h.do("GET", "/v1/accounts", nil); strings.Contains(string(raw), `"id": 1`) || strings.Contains(string(raw), `"id": 3`) {
		t.Fatalf("both accounts must be gone: %s", raw)
	}
}

// A resume copies the conversation into the target's home so it can follow
// the rotation, but only for a run that will happen: a refused request must
// leave that home exactly as it was.
func TestARefusedRunCopiesNoTranscript(t *testing.T) {
	h := newHarness(t, Options{})
	from, to := filepath.Join(h.root, "cfg1"), filepath.Join(h.root, "cfg3")
	for id, dir := range map[string]string{"1": from, "3": to} {
		if resp, raw := h.do("PATCH", "/v1/accounts/"+id, map[string]any{"config_dir": dir}); resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, raw)
		}
	}
	id := "01a00000-0000-7000-8000-00000000cafe"
	rel := filepath.Join("projects", "-tmp-x", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(from, rel)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, rel), []byte(`{"type":"user","cwd":"/tmp/x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, raw := h.run(3, map[string]any{"prompt": "p", "resume": id, "permission_mode": "bypassPermissions"})
	if code != 403 {
		t.Fatalf("%d %s", code, raw)
	}
	if _, err := os.Stat(filepath.Join(to, rel)); !os.IsNotExist(err) {
		t.Fatalf("the run was refused, yet the transcript was copied: %v", err)
	}
	// The same request, allowed, does copy it: that is what a resume is for.
	if code, _, raw := h.run(3, map[string]any{"prompt": "p", "resume": id}); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if _, err := os.Stat(filepath.Join(to, rel)); err != nil {
		t.Fatalf("an allowed resume must bring the transcript along: %v", err)
	}
}
