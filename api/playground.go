package api

import (
	_ "embed"
	"net/http"
)

// The two pages this server serves, each in its own route group: the
// playground, which is a form over the API, and the terminal, which is a
// terminal. They are two pages rather than two halves of one because they
// are two different things to be doing, and a server may serve either
// without the other. What they have in common is in assets/page.css and
// assets/page.js, which both of them load.

//go:embed playground.html
var playgroundHTML []byte

//go:embed terminal.html
var terminalHTML []byte

// playground serves a single page for trying this server by hand. It ships
// no token of its own: the page asks for one, checks it against
// /v1/accounts, and keeps it only in the browser.
func (s *Server) playground(w http.ResponseWriter, _ *http.Request) {
	writePage(w, playgroundHTML)
}

// terminalPage serves the page a terminal is watched and typed at from. It
// is in the terminal group, so a server that holds no terminals does not
// have it, and a server that holds them without a playground does.
func (s *Server) terminalPage(w http.ResponseWriter, _ *http.Request) {
	writePage(w, terminalHTML)
}

func writePage(w http.ResponseWriter, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	// A page loads nothing from anywhere else, so say so: a strict policy
	// here is what stops an injected script from reaching the network with
	// the token the page holds. 'self' is this server's own /assets — the
	// shared script and stylesheet, and the terminal emulator — which are in
	// this binary and served by this server. No remote host, and no
	// 'unsafe-eval': nothing here needs either.
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; "+
			"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY") // no framing: the page holds the token
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
