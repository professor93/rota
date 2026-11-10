package api

import (
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/professor93/rota/wire"
)

// termFileRe finds the files under /assets/term/ that terminal.html asks
// for, in the order it asks for them.
var termFileRe = regexp.MustCompile(`(?:src|href)="/assets/term/([^"?]+)\?v=\{\{v\}\}"`)

// termFiles is what terminal.html names under /assets/term/, in its order.
func termFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range termFileRe.FindAllSubmatch(terminalHTML, -1) {
		out = append(out, string(m[1]))
	}
	if len(out) == 0 {
		t.Fatal("terminal.html names no file under /assets/term/")
	}
	return out
}

// termPageSource is the terminal page as a browser gets it: the markup and
// every file of its own under /assets/term/.
func termPageSource(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.Write(terminalHTML)
	for _, name := range termFiles(t) {
		raw, err := assetFS.ReadFile("assets/term/" + name)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("\n")
		b.Write(raw)
	}
	return b.String()
}

// TestTheTerminalPageInNode runs every testdata/term/*.test.js against the
// real page files, in Node, on dom.js. Each is its own process, so one
// file's page cannot leak into the next one's.
func TestTheTerminalPageInNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pageFixtures(t, write)
	write("terminal.html", terminalHTML)
	err = fs.WalkDir(assetFS, "assets/term", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := assetFS.ReadFile(p)
		if err != nil {
			return err
		}
		write(p, raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "term"))
	if err != nil {
		t.Fatal(err)
	}
	var tests []string
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join("testdata", "term", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		write("term/"+e.Name(), raw)
		if strings.HasSuffix(e.Name(), ".test.js") {
			tests = append(tests, e.Name())
		}
	}
	sort.Strings(tests)
	if len(tests) == 0 {
		t.Fatal("testdata/term has no test")
	}
	for _, name := range tests {
		t.Run(strings.TrimSuffix(name, ".test.js"), func(t *testing.T) {
			cmd := exec.Command(node, filepath.Join(dir, "term", name))
			cmd.Env = append(os.Environ(), "T="+dir)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "TERM_TEST_OK") {
				t.Fatalf("%s failed:\n%s\n%v", name, out, err)
			}
		})
	}
}

// assetRefRe is every src or href a page's markup has.
var assetRefRe = regexp.MustCompile(`(?:src|href)="([^"]*)"`)

// Both pages' files are cached for a year, so each page asks for them under
// a digest of what is embedded: files that changed are a different URL, never
// a stale script, even where the version did not move.
func TestThePagesAskForTheirAssetsByDigest(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(assetsDigest()) {
		t.Fatalf("the assets' digest is %q, want 16 hex characters", assetsDigest())
	}
	h := newHarness(t, Options{Routes: terminalRoutes()})
	want := "?v=" + url.QueryEscape(assetsDigest())
	for _, p := range []string{"/playground", "/terminal"} {
		resp, raw := h.do("GET", p, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", p, resp.StatusCode)
		}
		page := string(raw)
		if strings.Contains(page, "{{v}}") {
			t.Errorf("%s is served with {{v}} left in it", p)
		}
		n := 0
		for _, m := range assetRefRe.FindAllStringSubmatch(page, -1) {
			if !strings.HasPrefix(m[1], "/assets/") {
				continue
			}
			n++
			if !strings.HasSuffix(m[1], want) {
				t.Errorf("%s asks for %q, which does not end in %q", p, m[1], want)
			}
		}
		if n == 0 {
			t.Errorf("%s asks for no asset", p)
		}
	}
}

// The digest follows the bytes: any file's change, a file added, or a file
// renamed is a new digest, and the same files are the same one.
func TestTheAssetsDigestFollowsTheirBytes(t *testing.T) {
	base := func() fstest.MapFS {
		return fstest.MapFS{
			"assets/page.js":       {Data: []byte("one")},
			"assets/term/core.js":  {Data: []byte("two")},
			"assets/term/term.css": {Data: []byte("three")},
		}
	}
	was := digestOf(base(), "assets")
	if again := digestOf(base(), "assets"); again != was {
		t.Fatalf("the same files give %s and then %s", was, again)
	}
	for what, change := range map[string]func(fstest.MapFS){
		"a byte changed": func(m fstest.MapFS) { m["assets/term/core.js"] = &fstest.MapFile{Data: []byte("twO")} },
		"a file added":   func(m fstest.MapFS) { m["assets/term/new.js"] = &fstest.MapFile{Data: []byte("")} },
		"a file renamed": func(m fstest.MapFS) {
			m["assets/term/main.js"] = m["assets/term/core.js"]
			delete(m, "assets/term/core.js")
		},
		"bytes moved between files": func(m fstest.MapFS) {
			m["assets/page.js"] = &fstest.MapFile{Data: []byte("on")}
			m["assets/term/core.js"] = &fstest.MapFile{Data: []byte("etwo")}
		},
	} {
		m := base()
		change(m)
		if got := digestOf(m, "assets"); got == was {
			t.Errorf("%s and the digest is still %s", what, got)
		}
	}
}

// Every file the terminal page names answers, with its type and the year's
// cache; nothing else is reachable under /assets/term/, and none of it is
// there without the terminal group, by any spelling of its path.
func TestTheTerminalPagesOwnFilesAreServed(t *testing.T) {
	h := newHarness(t, Options{Routes: terminalRoutes()})
	named := termFiles(t)
	embedded, err := fs.ReadDir(assetFS, "assets/term")
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, e := range embedded {
		have = append(have, e.Name())
	}
	want := append([]string(nil), named...)
	sort.Strings(want)
	if strings.Join(have, " ") != strings.Join(want, " ") {
		t.Errorf("assets/term holds %v and terminal.html names %v", have, want)
	}
	for _, name := range named {
		ctype := map[string]string{".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8"}[path.Ext(name)]
		resp, _ := h.do("GET", "/assets/term/"+name+"?v="+url.QueryEscape(assetsDigest()), nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /assets/term/%s: %d", name, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Content-Type"); got != ctype {
			t.Errorf("%s is served as %q, want %q", name, got, ctype)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s is served without nosniff: %q", name, got)
		}
		if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("%s is not cacheable: %q", name, got)
		}
	}
	for _, p := range []string{"/assets/term/nothing.js", "/assets/term/..%2fpage.js",
		"/assets/term%2Fcore.js", "/assets/term%5Ccore.js", "/assets/xterm%2Fxterm.js"} {
		if resp, _ := h.do("GET", p, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s answered %d, want 404", p, resp.StatusCode)
		}
	}
	off := newHarness(t, Options{})
	for _, p := range []string{"/assets/term/core.js", "/assets/term%2Fcore.js", "/assets/term%2fcore.js",
		"/assets/xterm%2Fxterm.js", "/assets/term%5Ccore.js"} {
		if resp, _ := off.do("GET", p, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s answered %d with the terminal group off, want 404", p, resp.StatusCode)
		}
	}
}

// Only the query a page asks under may be kept for a year. A request under
// another one, or none, is answered with today's bytes and kept by nobody,
// or a page from another build would pin them under its own URL for good.
func TestAStaleAssetQueryIsNotKept(t *testing.T) {
	h := newHarness(t, Options{Routes: terminalRoutes()})
	const year = "public, max-age=31536000, immutable"
	for _, c := range []struct{ path, cache string }{
		{"/assets/page.js?v=" + assetsDigest(), year},
		{"/assets/term/core.js?v=" + assetsDigest(), year},
		{"/assets/xterm/xterm.js?v=" + url.QueryEscape(wire.Version), year},
		{"/assets/page.js", "no-cache"},
		{"/assets/page.js?v=0123456789abcdef", "no-cache"},
		{"/assets/page.js?v=" + url.QueryEscape(wire.Version), "no-cache"},
		{"/assets/term/core.js?v=", "no-cache"},
		{"/assets/term/core.js?v=" + assetsDigest() + "&v=x", year},
		{"/assets/xterm/xterm.js?v=" + assetsDigest(), "no-cache"},
		{"/assets/xterm/xterm.js", "no-cache"},
	} {
		resp, _ := h.do("GET", c.path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d", c.path, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Cache-Control"); got != c.cache {
			t.Errorf("GET %s is kept as %q, want %q", c.path, got, c.cache)
		}
	}
}

var (
	bareScriptRe = regexp.MustCompile(`<script(?:\s[^>]*)?>`)
	styleTagRe   = regexp.MustCompile(`<style[\s>]`)
	inlineOnRe   = regexp.MustCompile(`(?i)\son[a-z]+\s*=|javascript:`)
)

// scriptDirectives is every directive of a policy whose name starts
// script-src, with its sources.
func scriptDirectives(csp string) map[string][]string {
	out := map[string][]string{}
	for _, d := range strings.Split(csp, ";") {
		f := strings.Fields(d)
		if len(f) > 0 && strings.HasPrefix(strings.ToLower(f[0]), "script-src") {
			out[strings.ToLower(f[0])] = append(out[strings.ToLower(f[0])], f[1:]...)
		}
	}
	return out
}

// The terminal page has no script of its own in its markup, so its policy
// admits none, by any directive; the playground still has one, and keeps
// 'unsafe-inline'.
func TestTheTerminalPageRunsNoInlineScript(t *testing.T) {
	for _, tag := range bareScriptRe.FindAllString(string(terminalHTML), -1) {
		if !strings.Contains(tag, "src=") {
			t.Errorf("terminal.html has a script with no src: %s", tag)
		}
	}
	if styleTagRe.Match(terminalHTML) {
		t.Error("terminal.html has a <style> of its own")
	}
	// The policy would block an inline handler without a word, so none is
	// written: el() binds every handler with addEventListener.
	for _, m := range inlineOnRe.FindAllString(string(terminalHTML), -1) {
		t.Errorf("terminal.html has inline script: %q", m)
	}
	h := newHarness(t, Options{Routes: terminalRoutes()})
	resp, _ := h.do("GET", "/terminal", nil)
	csp := resp.Header.Get("Content-Security-Policy")
	got := scriptDirectives(csp)
	if len(got) != 1 || strings.Join(got["script-src"], " ") != "'self'" {
		t.Errorf("the terminal page's script directives are %v, want only script-src 'self': %s", got, csp)
	}
	for name, sources := range got {
		for _, src := range sources {
			l := strings.ToLower(src)
			if strings.Contains(l, "unsafe-") || strings.HasPrefix(l, "'nonce-") || strings.HasPrefix(l, "'sha") {
				t.Errorf("the terminal page's %s admits inline script: %s", name, src)
			}
		}
	}
	resp, _ = h.do("GET", "/playground", nil)
	if got := scriptDirectives(resp.Header.Get("Content-Security-Policy")); strings.Join(got["script-src"], " ") != "'self' 'unsafe-inline'" {
		t.Errorf("the playground's policy lost its inline script: %v", got)
	}
}

// scriptDirectives itself, so the check above can fail: a policy that lets
// inline script in by any of the three names is seen to.
func TestTheScriptDirectivesAreReadWhole(t *testing.T) {
	for csp, want := range map[string]string{
		"default-src 'none'; script-src 'self'; connect-src 'self'":                            "script-src='self'",
		"script-src 'self' 'unsafe-inline'":                                                    "script-src='self' 'unsafe-inline'",
		"script-src 'self'; script-src-elem 'unsafe-inline'":                                   "script-src='self' script-src-elem='unsafe-inline'",
		"script-src 'self';script-src-attr 'unsafe-hashes' 'sha256-x'; Script-Src-Elem 'self'": "script-src='self' script-src-attr='unsafe-hashes' 'sha256-x' script-src-elem='self'",
	} {
		got := scriptDirectives(csp)
		var names []string
		for n := range got {
			names = append(names, n)
		}
		sort.Strings(names)
		var parts []string
		for _, n := range names {
			parts = append(parts, n+"="+strings.Join(got[n], " "))
		}
		if strings.Join(parts, " ") != want {
			t.Errorf("%q reads as %q, want %q", csp, strings.Join(parts, " "), want)
		}
	}
}
