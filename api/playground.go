package api

import (
	_ "embed"
	"net/http"
)

//go:embed playground.html
var playgroundHTML []byte

// playground serves a single self-contained page for trying this server by
// hand. It ships no token of its own: the page asks for one, checks it
// against /v1/accounts, and keeps it only in the browser.
func (s *Server) playground(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	// The page loads nothing from anywhere else, so say so: a strict policy
	// here is what stops an injected script from reaching the network with
	// the token the page holds.
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; "+
			"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(playgroundHTML)
}
