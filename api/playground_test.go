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

var scriptRe = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// TestPlaygroundScriptRuns drives the real playground code against the real
// schema this server serves, in Node with a minimal DOM. A page is otherwise
// the one part of a Go project that no test touches, and it is the part most
// likely to break silently when a field or a response shape changes.
func TestPlaygroundScriptRuns(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	m := scriptRe.FindSubmatch(playgroundHTML)
	if m == nil {
		t.Fatal("the playground has no script")
	}

	h := newHarness(t, Options{})
	dir := t.TempDir()
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("pg.js", m[1])
	for _, f := range []string{"dom.js", "harness.js"} {
		raw, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		write(f, raw)
	}
	for _, e := range []struct{ file, path string }{{"schema.json", "/v1/schema"}, {"accounts.json", "/v1/accounts"}} {
		resp, raw := h.do("GET", e.path, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", e.path, resp.StatusCode)
		}
		var pretty any
		json.Unmarshal(raw, &pretty)
		write(e.file, raw)
	}

	cmd := exec.Command(node, filepath.Join(dir, "harness.js"))
	cmd.Env = append(os.Environ(), "T="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PLAYGROUND_OK") {
		t.Fatalf("the playground failed against this server's own schema:\n%s\n%v", out, err)
	}
}

// TestPlaygroundIsSelfContained keeps the page from growing a dependency on
// anything it cannot reach: it is served by rota itself, often on a loopback
// address with no route to the internet.
func TestPlaygroundIsSelfContained(t *testing.T) {
	page := string(playgroundHTML)
	for _, bad := range []string{"http://", "https://cdn", "<script src", "<link rel=\"stylesheet\"", "@import"} {
		if strings.Contains(page, bad) {
			t.Fatalf("the playground reaches outside itself: %q", bad)
		}
	}
	for _, want := range []string{"/v1/schema", "/v1/accounts", "rota.history"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the playground no longer uses %q", want)
		}
	}
}
