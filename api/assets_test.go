package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// vendored is every file carried in assets/xterm and the SHA-256 it must
// have. It is written out here rather than computed, because the point of it
// is that somebody editing a vendored file — or a build that quietly picked
// up a different one — fails a test instead of shipping. The same sums are
// in assets/xterm/README beside the tarball integrity strings they came
// with; updating the emulator means changing both, on purpose.
var vendored = map[string]string{
	"xterm.js":              "1f991ac3b4b283ebf96e60ae23a00a52765dd3a2e46fa6fdda9f1aab032f7495",
	"xterm.css":             "ba8e6985669488981ccf40c0cefe3aba80722cb6c92de7ad628b0bd717faf2b6",
	"addon-fit.js":          "bdaefa370b1bfc42ee88d46fe6072400902a4d4b2d45cd93438dda9b23c97089",
	"LICENSE-xterm.txt":     "b569f629d00f2626a8100df2a1798210535621e42164dfd426a6fe5aac7b0ccd",
	"LICENSE-addon-fit.txt": "e256f01188af527e4d06d21d06fbf785ae9c50d4b328bf03cbe0ba7f0aa4228f",
	// The README is provenance, not code: it changes whenever the versions
	// above do, so it is checked for being there rather than for its bytes.
	"README": "",
}

func TestTheVendoredEmulatorIsTheOneItSaysItIs(t *testing.T) {
	seen := map[string]bool{}
	err := fs.WalkDir(assetFS, "assets/xterm", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		want, listed := vendored[name]
		if !listed {
			t.Errorf("%s is embedded and nothing says what it should be", path)
			return nil
		}
		seen[name] = true
		raw, err := assetFS.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); want != "" && got != want {
			t.Errorf("%s has been changed:\n  is   %s\n  want %s", path, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range vendored {
		if !seen[name] {
			t.Errorf("%s is listed and not embedded", name)
		}
		if _, served := assetTypes["xterm/"+name]; !served {
			t.Errorf("%s is embedded and not served", name)
		}
	}
}

// terminalRoutes is a server with the terminal page on, which is the only
// kind that serves an emulator.
func terminalRoutes() *Routes {
	return &Routes{API: true, Playground: true, WebSocket: true, Health: true, Terminal: true}
}

// The terminal page cannot draw a terminal with files it is refused, so
// every one of them answers, says what it is, and says it may be kept: the
// page asks under a version query, and a rota that changed is a new query.
func TestTheEmulatorIsServedBesideTheTerminalPage(t *testing.T) {
	h := newHarness(t, Options{Routes: terminalRoutes()})
	for _, c := range []struct{ file, ctype string }{
		{"xterm/xterm.js", "text/javascript; charset=utf-8"},
		{"xterm/xterm.css", "text/css; charset=utf-8"},
		{"xterm/addon-fit.js", "text/javascript; charset=utf-8"},
		{"xterm/LICENSE-xterm.txt", "text/plain; charset=utf-8"},
		{"xterm/LICENSE-addon-fit.txt", "text/plain; charset=utf-8"},
		{"page.css", "text/css; charset=utf-8"},
		{"page.js", "text/javascript; charset=utf-8"},
	} {
		resp, raw := h.do("GET", "/assets/"+c.file+"?v=1.2.0", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", c.file, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != c.ctype {
			t.Errorf("%s is served as %q, want %q", c.file, got, c.ctype)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s is served without nosniff: %q", c.file, got)
		}
		if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("%s is not cacheable: %q", c.file, got)
		}
		if want, ok := vendored[c.file[len("xterm/"):]]; ok && want != "" {
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != want {
				t.Errorf("%s came back as something else", c.file)
			}
		}
	}
	// Nothing else is reachable through those paths, whatever is in the
	// directory or on the disk.
	for _, name := range []string{"xterm/xterm.js.map", "xterm/nothing", "nothing", "page.css/.."} {
		if resp, _ := h.do("GET", "/assets/"+name, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET /assets/%s answered %d, want 404", name, resp.StatusCode)
		}
	}
}

// The emulator belongs to the terminal page, so it goes where that page
// goes; the two shared files stay for as long as either page is served.
func TestTheEmulatorIsGoneWithTheTerminalPage(t *testing.T) {
	h := newHarness(t, Options{})
	for _, file := range []string{"xterm/xterm.js", "xterm/xterm.css", "xterm/addon-fit.js"} {
		if resp, _ := h.do("GET", "/assets/"+file, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /assets/%s answered %d with no terminal, want 404", file, resp.StatusCode)
		}
	}
	for _, file := range []string{"page.css", "page.js"} {
		if resp, _ := h.do("GET", "/assets/"+file, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("the playground still needs /assets/%s: %d", file, resp.StatusCode)
		}
	}
	// And with no page of either kind, none of it is there.
	bare := newHarness(t, Options{Routes: &Routes{API: true, WebSocket: true}})
	for _, file := range []string{"page.css", "page.js", "xterm/xterm.js"} {
		if resp, _ := bare.do("GET", "/assets/"+file, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /assets/%s answered %d with no pages, want 404", file, resp.StatusCode)
		}
	}
}

// Two pages, two groups: either may be served without the other.
func TestEitherPageMayBeServedWithoutTheOther(t *testing.T) {
	both := newHarness(t, Options{Routes: terminalRoutes()})
	for _, path := range []string{"/playground", "/terminal"} {
		if resp, _ := both.do("GET", path, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", path, resp.StatusCode)
		}
	}
	// A server holding terminals and serving no playground still has the
	// page a terminal is watched from.
	onlyTerm := newHarness(t, Options{Routes: &Routes{API: true, WebSocket: true, Terminal: true}})
	if resp, _ := onlyTerm.do("GET", "/terminal", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /terminal with no playground: %d", resp.StatusCode)
	}
	for _, path := range []string{"/", "/playground"} {
		if resp, _ := onlyTerm.do("GET", path, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s answered %d with no playground, want 404", path, resp.StatusCode)
		}
	}
	// And the other way round: a playground, and no terminal to be had.
	onlyPage := newHarness(t, Options{})
	if resp, _ := onlyPage.do("GET", "/terminal", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /terminal answered %d with the group off, want 404", resp.StatusCode)
	}
}

// The pages are served with the headers a page that holds a credential
// needs, and a policy that admits this server's own assets and nothing else.
func TestBothPagesCarryTheSamePolicy(t *testing.T) {
	h := newHarness(t, Options{Routes: terminalRoutes()})
	for _, path := range []string{"/playground", "/terminal"} {
		resp, _ := h.do("GET", path, nil)
		csp := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self' 'unsafe-inline'",
			"style-src 'self' 'unsafe-inline'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: the policy is missing %q: %s", path, want, csp)
			}
		}
		for _, bad := range []string{"unsafe-eval", "http:", "https:", "*"} {
			if strings.Contains(csp, bad) {
				t.Errorf("%s: the policy admits %q: %s", path, bad, csp)
			}
		}
		if resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s may be framed", path)
		}
	}
}

// Each page offers the way to the other one only where the other one is
// there, and the schema is how either of them knows.
func TestTheSchemaSaysWhichPagesThereAre(t *testing.T) {
	for _, c := range []struct {
		what                        string
		opts                        Options
		terminal, shell, playground bool
	}{
		{"the terminal is off by default", Options{}, false, false, true},
		{"on, CLIs only", Options{Routes: terminalRoutes()}, true, false, true},
		{"on, with a shell", Options{
			Routes:   terminalRoutes(),
			Terminal: Terminal{Shell: true},
		}, true, true, true},
		{"a terminal and no playground", Options{
			Routes: &Routes{API: true, WebSocket: true, Terminal: true},
		}, true, false, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			h := newHarness(t, c.opts)
			_, raw := h.do("GET", "/v1/schema", nil)
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			for _, f := range []struct {
				name string
				want bool
			}{{"terminal", c.terminal}, {"shell", c.shell}, {"playground", c.playground}} {
				if doc[f.name] != f.want {
					t.Errorf("schema says %s %v, want %v", f.name, doc[f.name], f.want)
				}
			}
		})
	}
}
