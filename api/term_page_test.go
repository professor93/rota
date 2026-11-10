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

// Both pages are cached for a year, so each asks for its files under this
// server's version: an upgraded rota is a different URL, never a stale script.
func TestThePagesAskForTheirAssetsByVersion(t *testing.T) {
	h := newHarness(t, Options{Routes: terminalRoutes()})
	want := "?v=" + url.QueryEscape(wire.Version)
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

// Every file the terminal page names answers, with its type and the year's
// cache; nothing else is reachable under /assets/term/, and none of it is
// there without the terminal group.
func TestTheTerminalPagesOwnFilesAreServed(t *testing.T) {
	h := newHarness(t, Options{Routes: terminalRoutes()})
	named := termFiles(t)
	embedded, err := fs.ReadDir(assetFS, "assets/term")
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded) != len(named) {
		t.Errorf("assets/term holds %d files and terminal.html names %d", len(embedded), len(named))
	}
	for _, name := range named {
		ctype := map[string]string{".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8"}[path.Ext(name)]
		resp, _ := h.do("GET", "/assets/term/"+name+"?v="+url.QueryEscape(wire.Version), nil)
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
	for _, p := range []string{"/assets/term/nothing.js", "/assets/term/..%2fpage.js"} {
		if resp, _ := h.do("GET", p, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s answered %d, want 404", p, resp.StatusCode)
		}
	}
	off := newHarness(t, Options{})
	if resp, _ := off.do("GET", "/assets/term/core.js", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /assets/term/core.js answered %d with the terminal group off, want 404", resp.StatusCode)
	}
}

var (
	bareScriptRe = regexp.MustCompile(`<script(?:\s[^>]*)?>`)
	styleTagRe   = regexp.MustCompile(`<style[\s>]`)
)

// The terminal page has no script of its own in its markup, so its policy
// admits none; the playground still has one, and keeps 'unsafe-inline'.
func TestTheTerminalPageRunsNoInlineScript(t *testing.T) {
	for _, tag := range bareScriptRe.FindAllString(string(terminalHTML), -1) {
		if !strings.Contains(tag, "src=") {
			t.Errorf("terminal.html has a script with no src: %s", tag)
		}
	}
	if styleTagRe.Match(terminalHTML) {
		t.Error("terminal.html has a <style> of its own")
	}
	h := newHarness(t, Options{Routes: terminalRoutes()})
	resp, _ := h.do("GET", "/terminal", nil)
	csp := resp.Header.Get("Content-Security-Policy")
	i := strings.Index(csp, "script-src 'self';")
	if i < 0 {
		t.Fatalf("the terminal page's policy has no `script-src 'self';`: %s", csp)
	}
	directive := csp[i:]
	directive = directive[:strings.Index(directive, ";")]
	if strings.Contains(directive, "unsafe-inline") {
		t.Errorf("the terminal page's script-src admits inline script: %s", directive)
	}
	resp, _ = h.do("GET", "/playground", nil)
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("the playground's policy lost its inline script: %s", csp)
	}
}
