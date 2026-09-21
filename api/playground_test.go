package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var scriptRe = regexp.MustCompile(`(?s)<script>\n(.*?)\n</script>`)

// pageScript is what a page runs of its own, past the shared script it loads
// first. A page with none, or with two, is a page this test cannot drive.
func pageScript(t *testing.T, page []byte, what string) []byte {
	t.Helper()
	m := scriptRe.FindAllSubmatch(page, -1)
	if len(m) != 1 {
		t.Fatalf("%s has %d scripts of its own, want one", what, len(m))
	}
	return m[0][1]
}

// TestThePagesRunTheirOwnScripts drives the real page code against the real
// schema this server serves, in Node with a minimal DOM. A page is otherwise
// the one part of a Go project that no test touches, and the part most
// likely to break silently when a field or a response shape changes.
//
// Both pages are run: they share a script, so a change to it that suits one
// of them and breaks the other is a failure here rather than in a browser.
func TestThePagesRunTheirOwnScripts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	shared, err := assetFS.ReadFile("assets/page.js")
	if err != nil {
		t.Fatal(err)
	}
	write("page.js", shared)
	write("pg.js", pageScript(t, playgroundHTML, "the playground"))
	write("term.js", pageScript(t, terminalHTML, "the terminal page"))
	for _, f := range []string{"dom.js", "harness.js", "terminal-harness.js"} {
		raw, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		write(f, raw)
	}

	// Two servers, because the schema is what each page is built from and
	// the terminal group is off unless it was asked for.
	plain := newHarness(t, Options{})
	withTerm := newHarness(t, Options{Routes: terminalRoutes(), Terminal: Terminal{Shell: true}})
	for _, e := range []struct {
		file, path string
		h          *harness
	}{
		{"schema.json", "/v1/schema", plain},
		{"accounts.json", "/v1/accounts", plain},
		{"schema-terminal.json", "/v1/schema", withTerm},
	} {
		resp, raw := e.h.do("GET", e.path, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", e.path, resp.StatusCode)
		}
		var pretty any
		json.Unmarshal(raw, &pretty)
		write(e.file, raw)
	}

	for _, run := range []struct{ harness, ok string }{
		{"harness.js", "PLAYGROUND_OK"},
		{"terminal-harness.js", "TERMINAL_OK"},
	} {
		cmd := exec.Command(node, filepath.Join(dir, run.harness))
		cmd.Env = append(os.Environ(), "T="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), run.ok) {
			t.Fatalf("%s failed against this server's own schema:\n%s\n%v", run.harness, out, err)
		}
	}
}

// TestThePagesAreSelfContained keeps either page from growing a dependency
// on anything it cannot reach: they are served by rota itself, often on a
// loopback address with no route to the internet. What they do load, they
// load from this server, which is what 'self' in the policy means.
func TestThePagesAreSelfContained(t *testing.T) {
	for _, p := range []struct {
		what string
		body []byte
		uses []string
	}{
		{"the playground", playgroundHTML, []string{"/v1/schema", "/v1/accounts", "rota.history"}},
		{"the terminal page", terminalHTML, []string{"/v1/terminals", "/assets/xterm/xterm.js", "new WebSocket"}},
	} {
		page := string(p.body)
		for _, bad := range []string{"http://", "https://", "//cdn", "@import"} {
			if strings.Contains(page, bad) {
				t.Errorf("%s reaches outside itself: %q", p.what, bad)
			}
		}
		for _, src := range regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(page, -1) {
			if !strings.HasPrefix(src[1], "/assets/") && !strings.HasPrefix(src[1], "#") {
				t.Errorf("%s loads %q, which is not this server's own", p.what, src[1])
			}
		}
		for _, want := range p.uses {
			if !strings.Contains(page, want) {
				t.Errorf("%s no longer uses %q", p.what, want)
			}
		}
	}
	// And the emulator is named in one place, so a test can put another
	// terminal there.
	term := string(terminalHTML)
	if n := strings.Count(term, "new Terminal("); n != 1 {
		t.Errorf("xterm is constructed in %d places, want one", n)
	}
	if n := strings.Count(term, "new FitAddon"); n != 1 {
		t.Errorf("the fit addon is made in %d places, want one", n)
	}
}
