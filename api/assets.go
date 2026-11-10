package api

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/professor93/rota/wire"
)

// What the two pages are made of, carried in the binary.
//
// page.css and page.js are everything the playground and the terminal have
// in common — the palette, the header strip, the credential, the sign-in —
// written once and served to both rather than kept in step in two files.
//
// assets/term is the terminal page's own script and styles, one file per
// region of the page, so that the page runs no script inline.
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
	// The terminal page's own script and styles, in the order it loads them.
	"term/view.js":    "text/javascript; charset=utf-8",
	"term/words.js":   "text/javascript; charset=utf-8",
	"term/core.js":    "text/javascript; charset=utf-8",
	"term/list.js":    "text/javascript; charset=utf-8",
	"term/new.js":     "text/javascript; charset=utf-8",
	"term/info.js":    "text/javascript; charset=utf-8",
	"term/size.js":    "text/javascript; charset=utf-8",
	"term/dock.js":    "text/javascript; charset=utf-8",
	"term/actions.js": "text/javascript; charset=utf-8",
	"term/events.js":  "text/javascript; charset=utf-8",
	"term/boot.js":    "text/javascript; charset=utf-8",
	"term/term.css":   "text/css; charset=utf-8",
	"term/list.css":   "text/css; charset=utf-8",
	"term/info.css":   "text/css; charset=utf-8",
	"term/size.css":   "text/css; charset=utf-8",
	"term/dock.css":   "text/css; charset=utf-8",
}

// asset serves one of them, and is registered under three patterns: the two
// shared files, which exist wherever either page does, and the emulator and
// the terminal page's own files, which belong to the terminal group alone.
//
// It is behind no credential, like the pages themselves. A page asks for
// each of its files with ?v=<the assets' digest>, and for the emulator, whose
// files are pinned by their sums, with ?v=<this server's version>. Under that
// query the answer may be kept for as long as a browser likes: a rota whose
// files have changed serves a page that asks under a different one. Under any
// other query, or none, the bytes are today's and are not to be kept unasked:
// a page from another build would otherwise pin them under its own URL.
func (s *Server) assetsIn(dir string) http.HandlerFunc {
	current := assetsDigest
	if dir == "xterm/" {
		current = func() string { return wire.Version }
	}
	return func(w http.ResponseWriter, r *http.Request) {
		file := r.PathValue("file")
		name := dir + file
		ct, ok := assetTypes[name]
		// One segment only: an escaped slash would otherwise reach a file of
		// another directory through this one's pattern, past the group that
		// directory belongs to.
		if !ok || strings.ContainsAny(file, `/\`) || strings.Contains(name, "..") {
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
		if r.URL.Query().Get("v") == current() {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		h.Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// assetsDigest names what is embedded under assets/. The pages ask for their
// files under it, so files that changed are a new URL even where the version
// did not move, as between two builds of one beta. It is worked out the first
// time a page or an asset is served, not when the program starts: every rota
// command links this package, and most of them serve nothing.
var assetsDigest = sync.OnceValue(func() string { return digestOf(assetFS, "assets") })

// digestOf is the first 16 hex characters of a SHA-256 over every file under
// root, walked in name order, each as its path, a NUL and its bytes.
func digestOf(fsys fs.FS, root string) string {
	h := sha256.New()
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(raw)
		return nil
	})
	if err != nil {
		// Embedded at build time, so this does not fail on assetFS; a page
		// would still get a query, only one that never changed.
		return "0"
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
