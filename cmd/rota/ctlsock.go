package main

import (
	nethttp "net/http"

	"github.com/professor93/rota/api"
)

// The control socket is ctl.sock in a store's terminals directory: the rota
// daemon's API for this machine's own user. It serves every route there is,
// the terminals and their send included, to one fixed principal, local, and
// it reads no credential at all. Its authorization is the socket's own
// permissions — 0600, in a directory that is 0700 — so whoever can connect is
// the user the daemon runs as, and nobody else can.
//
// One kind of caller is that user and still may only watch: a process
// running inside a held session, which is the agent in it or something the
// agent started. It is told apart by its session, which the kernel says and
// it cannot: the peer's pid comes from the socket, and the session that pid
// is in from getsid. Inside a session whose leader is a running holder or its
// CLI, the caller is local with the watch role. A caller the daemon cannot
// place is given the same, since it cannot be shown to be outside.
//
// That is a guard against an agent changing sessions by mistake — starting,
// typing into or ending one from inside another — and not a wall: a process
// running as this user has other ways to do what the daemon does, and one
// that leaves its session (setsid) is no longer told apart.

// The two principals the control socket serves. Both are local, so both are
// the same person at a held session's keyboard; they differ only in what they
// may do.
var (
	ctlControl = api.Principal{Name: "local", Role: api.RoleControl, Via: "local"}
	ctlWatch   = api.Principal{Name: "local", Role: api.RoleWatch, Via: "local"}
)

// ctlMayKey is where a connection's context keeps whether its caller may
// change anything: a func() bool, asked once per connection.
type ctlMayKey struct{}

// ctlHandler is the control socket's handler: srv's LocalHandler for each of
// the two principals, chosen per connection by what its context says. A
// request whose connection says nothing is the watcher's, so a mistake here
// can only ever take a change away.
func ctlHandler(srv *api.Server) nethttp.Handler {
	control, watch := srv.LocalHandler(&ctlControl), srv.LocalHandler(&ctlWatch)
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if may, ok := r.Context().Value(ctlMayKey{}).(func() bool); ok && may() {
			control.ServeHTTP(w, r)
			return
		}
		watch.ServeHTTP(w, r)
	})
}
