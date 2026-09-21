package api

import (
	"embed"
	"net/http"
	"strings"
)

// What the two pages are made of, carried in the binary.
//
// page.css and page.js are everything the playground and the terminal have
// in common — the palette, the header strip, the credential, the sign-in —
// written once and served to both rather than kept in step in two files.
//
// assets/xterm is the terminal emulator. It is vendored rather than fetched:
// this server is usually on a loopback address with no route out, so a page
// that asked a CDN for its emulator would be a page that does not work here
// — and a third party who could change what runs in front of somebody's
// shell. assets/xterm/README says which packages those are, which versions,
// the sha512 each tarball answered, and the SHA-256 of every file, which a
// test recomputes from what is embedded here.
//
//go:embed assets
var assetFS embed.FS

// assetTypes is every file this server will hand out, and what it is. A
// table rather than a guess from the extension: what is served is a closed
// list, so a file that lands in that directory is not reachable until
// somebody says here that it should be.
var assetTypes = map[string]string{
	"page.css":                    "text/css; charset=utf-8",
	"page.js":                     "text/javascript; charset=utf-8",
	"xterm/xterm.js":              "text/javascript; charset=utf-8",
	"xterm/xterm.css":             "text/css; charset=utf-8",
	"xterm/addon-fit.js":          "text/javascript; charset=utf-8",
	"xterm/LICENSE-xterm.txt":     "text/plain; charset=utf-8",
	"xterm/LICENSE-addon-fit.txt": "text/plain; charset=utf-8",
	// Where all of that came from, readable from the server that serves it.
	"xterm/README": "text/plain; charset=utf-8",
}

// asset serves one of them, and is registered under two patterns: the two
// shared files, which exist wherever either page does, and the emulator,
// which belongs to the terminal group alone.
//
// It is behind no credential, like the pages themselves. The page asks for
// each file with ?v=<this server's version>, so the answer may be kept for
// as long as a browser likes: a rota that has changed serves a page that
// asks under a different query.
func (s *Server) assetsIn(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := dir + r.PathValue("file")
		ct, ok := assetTypes[name]
		if !ok || strings.Contains(name, "..") {
			fail(w, http.StatusNotFound, "no asset "+name)
			return
		}
		data, err := assetFS.ReadFile("assets/" + name)
		if err != nil {
			// Embedded at build time, so this is the table naming a file
			// that is not there — a mistake in this package, not the
			// request's.
			fail(w, http.StatusNotFound, "no asset "+name)
			return
		}
		h := w.Header()
		h.Set("Content-Type", ct)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}
