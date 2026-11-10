package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Who is making a request, and what that lets them do.
//
// Everything this server answers is done by somebody: the one bearer token
// it was started with, another token the file names, a person who signed in
// on the page, or a watcher who followed an invite link. They differ in two
// facts — a name to put in the log, and a role that decides what they may
// ask for — so they are one type with those two fields rather than four
// paths through the middleware.

// Role is what a principal may do. There are two, and there is no third for
// "a bit more than watch": every route is either reading or changing, and a
// role that sat between them would have to be decided route by route.
type Role string

const (
	// RoleWatch may read everything and change nothing.
	RoleWatch Role = "watch"
	// RoleControl may do anything this server does — which includes running
	// a coding agent on this machine, so it is the whole of it.
	RoleControl Role = "control"
)

// allows reports whether a principal holding have may do what needs asks.
func (have Role) allows(need Role) bool {
	return have == RoleControl || have == need
}

// Principal is one request's identity: a name for the log, the role that
// decides what it may ask for, and how it arrived.
type Principal struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
	// Via is "token" for a bearer token, "user" for a page sign-in,
	// "invite" for a watcher who followed a link, and "local" for this
	// machine's own user on the control socket (LocalHandler).
	Via string `json:"via"`
	// Expires is when a session dies. A bearer token has none and leaves it
	// zero: it lasts as long as the file that names it.
	Expires time.Time `json:"expires,omitzero"`
	// session is the id of the server-side session backing this principal,
	// empty for a bearer token. It never leaves the server.
	session string
}

// cookie is a session identity carried by a browser. The name is prefixed
// because everything else about the cookie is fixed too: one server, one
// path, and nobody else's page may read it.
const sessionCookie = "rota_session"

type ctxKey struct{}

// withPrincipal is how the middleware hands what it resolved to the handler
// behind it.
func withPrincipal(r *http.Request, p *Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, p))
}

// who is the principal behind this request, or nil on a route that has
// none. A handler registered under the guard always has one.
func who(r *http.Request) *Principal {
	p, _ := r.Context().Value(ctxKey{}).(*Principal)
	return p
}

/* ------------------------------------------------------- the role table --- */

// roleFor is the one place that says what each route takes. It is a table
// rather than a check inside each handler so that the answer to "who may do
// this?" is read in one screen, and so that a route added later is a row
// here — which a test insists on, by comparing this table against the
// patterns actually registered.
//
// The shape of the rule is simple enough to state in a sentence: reading is
// watch, and everything that changes anything is control. It is written out
// per route anyway, because that sentence is a summary of the table and not
// a substitute for it — a GET that starts something would be the exception
// that a rule by method could not express.
var roleFor = map[string]Role{
	// Reading: the schema, the accounts, the runs and their streams.
	"GET /v1/schema":               RoleWatch,
	"GET /v1/accounts":             RoleWatch,
	"GET /v1/accounts/{id}/schema": RoleWatch,
	"GET /v1/runs":                 RoleWatch,
	"GET /v1/runs/{id}":            RoleWatch,
	"GET /v1/runs/{id}/events":     RoleWatch,
	// Running something, and talking to what is running.
	"POST /v1/run":                 RoleControl,
	"POST /v1/accounts/{id}/run":   RoleControl,
	"POST /v1/runs/{id}/messages":  RoleControl,
	"POST /v1/runs/{id}/interrupt": RoleControl,
	"POST /v1/runs/{id}/close":     RoleControl,
	// Changing the accounts themselves, and signing new ones in.
	"PATCH /v1/accounts/{id}":  RoleControl,
	"DELETE /v1/accounts/{id}": RoleControl,
	"POST /v1/login":           RoleControl,
	"POST /v1/login/{id}":      RoleControl,
	"POST /v1/auth":            RoleControl,
	"POST /v1/auth/{id}":       RoleControl,
	// Handing somebody else a way in.
	"POST /v1/invites": RoleControl,
	// The terminals. Looking at one is reading; starting one runs a CLI on
	// this machine, and ending one takes it away from whoever is using it.
	"GET /v1/terminals":         RoleWatch,
	"GET /v1/terminals/{id}":    RoleWatch,
	"POST /v1/terminals":        RoleControl,
	"DELETE /v1/terminals/{id}": RoleControl,
	// Typing a message into a held session is typing.
	"POST /v1/terminals/{id}/messages": RoleControl,
	// The sockets. Attaching is reading a run; starting one is running it.
	"GET /v1/runs/{id}/ws":     RoleWatch,
	"GET /v1/accounts/{id}/ws": RoleControl,
	"GET /v1/ws":               RoleControl,
	// Attaching to a terminal is reading it. What may be sent back into one
	// is decided per frame, by the table below.
	"GET /v1/terminals/{id}/ws": RoleWatch,
}

// termRoleFor is the same table for the frames a terminal's socket takes,
// and it is here rather than beside the socket for the same reason: the
// answer to "who may do this?" is read in one screen.
//
// "input" is the binary frame — the bytes of somebody typing — which has no
// type field of its own because a frame that carried keystrokes as JSON
// would have to escape every one of them. Only ping is watch: a connection
// that may not change the terminal may still ask whether it is there.
var termRoleFor = map[string]Role{
	"input":   RoleControl,
	"resize":  RoleControl,
	"claim":   RoleControl,
	"release": RoleControl,
	"grant":   RoleControl,
	"deny":    RoleControl,
	"ping":    RoleWatch,
}

// termNeeds is the role one frame takes. A type nobody wrote a row for takes
// control, for the same reason a route does.
func termNeeds(kind string) Role {
	if r, ok := termRoleFor[kind]; ok {
		return r
	}
	return RoleControl
}

// needs is the role one route takes. A pattern nobody wrote a row for takes
// control: a route whose rule was forgotten must not be the open one.
func needs(pattern string) Role {
	if r, ok := roleFor[pattern]; ok {
		return r
	}
	return RoleControl
}

// localName is the control socket's principal, and the person it is for the
// keyboard when nothing else is said.
const localName = "local"

// ReservedName refuses a name that no user and no token of a daemon may
// have: local, the control socket's own principal. A user called that would
// be the same person as the socket at a held session's keyboard, and either
// would read as the socket wherever a name is shown — who holds a keyboard,
// and every audit line. It is one sentence wherever a name is chosen: for a
// daemon (ReservedNames), and in the commands that print a block for the
// file. Any other name is nil.
func ReservedName(name string) error {
	if name == localName {
		return fmt.Errorf("the name %q is reserved for the control socket's own principal; choose another", localName)
	}
	return nil
}

// ReservedNames refuses a user or a token called local, naming the entry,
// for a server that has a control socket: one with HoldCommand, which New
// asks this of, and a daemon's file before it is started. A server without
// one, foreground `rota serve`, has no principal called local, and a file
// whose user or token has that name serves there as it did before the
// daemon.
func ReservedNames(users []User, tokens []TokenPrincipal) error {
	for i, u := range users {
		if err := ReservedName(u.Name); err != nil {
			return fmt.Errorf("%s: %w", entryName("users", i, u.Name), err)
		}
	}
	for i, t := range tokens {
		if err := ReservedName(t.Name); err != nil {
			return fmt.Errorf("%s: %w", entryName("tokens", i, t.Name), err)
		}
	}
	return nil
}

// personOf is who a principal is as a person, for the keyboard of a held
// session: the same person on several devices types on whichever of them
// without asking. A user signed in on the page is its name. The local
// principal — the control socket, this machine's own terminal — is the
// person Options.LocalUser names when it names one, else the one user who
// may control when there is exactly one, and local itself when nobody says.
// LocalUser comes first because it may name a person Users does not hold,
// such as one who signs in by SSH key alone. A bearer
// token or an invite is "", which is never a person: a token is carried by
// a program, and an invite by whoever was handed the link.
func (s *Server) personOf(p *Principal) string {
	if p == nil {
		return ""
	}
	switch p.Via {
	case "user":
		return p.Name
	case "local":
		only, n := "", 0
		for _, u := range s.users {
			if u.role == RoleControl {
				only, n = u.name, n+1
			}
		}
		switch {
		case s.opts.LocalUser != "":
			return s.opts.LocalUser
		case n == 1:
			return only
		}
		return localName
	}
	return ""
}

// The sentences a principal is refused with. They are constants because
// they are part of what this server answers, and a test reads them.
// watchOnly is for a sign-in — a token, a page's user, an invite — and
// callerWatchOnly for the local principal on the control socket, which is a
// process of this machine's user and signed in to nothing.
const (
	watchOnly       = "this sign-in may only watch; nothing here can be changed from it"
	callerWatchOnly = "this caller may only watch; nothing here can be changed from it"
	crossSiteNo     = "a request from another site cannot use this session"
)

// watchOnlyFor is the sentence p is refused a change in.
func watchOnlyFor(p *Principal) string {
	if p != nil && p.Via == "local" {
		return callerWatchOnly
	}
	return watchOnly
}

/* --------------------------------------------------- resolving a request --- */

// resolve says who is making this request, and whether a bearer credential
// was offered and refused — which is the thing the limiter counts. Nothing
// is written to the response; that is the caller's to do, in the words it
// has always used.
//
// The order is the credential a caller chose most deliberately first: a
// bearer token is put on the request by whoever wrote it, while a cookie
// rides along on anything a browser sends.
func (s *Server) resolve(r *http.Request) (p *Principal, guessed bool) {
	if tok := bearerToken(r.Header.Get("Authorization")); tok != "" {
		if p := s.byToken(tok); p != nil {
			return p, false
		}
		return nil, true
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if p := s.sessions.get(c.Value); p != nil {
			return p, false
		}
	}
	return nil, false
}

// byToken matches a bearer token against the one this server was started
// with and then against every token the file names. Both comparisons are on
// SHA-256 of what arrived: a plain comparison returns at once on a length
// mismatch, and that alone would tell a guesser how long the token is.
func (s *Server) byToken(got string) *Principal {
	sum := sha256.Sum256([]byte(got))
	if s.opts.Token != "" {
		want := sha256.Sum256([]byte(s.opts.Token))
		if subtle.ConstantTimeCompare(sum[:], want[:]) == 1 {
			// The token rota has always had. It is the server's own key, so
			// it is control, and it is called what it is.
			return &Principal{Name: "token", Role: RoleControl, Via: "token"}
		}
	}
	// Every configured token is compared, not just up to the first match:
	// stopping early would make the time say which entry answered.
	var found *Principal
	for _, t := range s.tokens {
		if subtle.ConstantTimeCompare(sum[:], t.sum[:]) == 1 {
			found = &Principal{Name: t.name, Role: t.role, Via: "token"}
		}
	}
	return found
}

/* --------------------------------------------------------- the middleware --- */

// guard is auth and authorization for one HTTP route: who is asking, whether
// the request is theirs to make at all, and whether their role covers it.
//
// local is the principal LocalHandler serves every request as, and nil on
// Handler. With one, nothing on the request is read at all — no token, no
// cookie, and no Origin, whatever the principal is — so nothing on it is a
// guess either. The Origin rule is skipped because of where the guard was
// installed, never because of who the principal says it is: what reaches
// LocalHandler is the control socket or an SSH channel, which no browser can
// open, and there is no cookie for a page to ride on.
//
// The block on guesses applies to guesses, never to a credential that is
// right: an address is shared by everyone behind a proxy, and the loopback
// is reachable from any web page the operator visits, so a block that
// refused the right token would let a stranger lock the operator out of
// their own server. A wrong role is not a guess either — it is somebody who
// proved who they are and asked for something else.
func (s *Server) guard(pattern string, local *Principal, next http.HandlerFunc) http.HandlerFunc {
	need := needs(pattern)
	return func(w http.ResponseWriter, r *http.Request) {
		p, guessed := local, false
		if p == nil {
			p, guessed = s.resolve(r)
		}
		if p == nil {
			s.refuseToken(w, r, guessed)
			return
		}
		if local == nil && !s.sameSite(w, r, p) {
			return
		}
		if !p.Role.allows(need) {
			s.refuseRole(w, r, p, need)
			return
		}
		next(w, withPrincipal(r, p))
	}
}

// sameSite is the CSRF rule, and it applies to exactly one kind of request:
// one that changes something and is authorized by a cookie.
//
// A cookie is sent by the browser whether or not the page that caused the
// request is this server's. SameSite=Strict already stops the cross-site
// cases browsers agree on; this is the second lock, for the ones they do not
// — and it is cheap, because a browser puts Origin on every request that is
// not a plain navigation. Who is exempt is originFree's to say.
func (s *Server) sameSite(w http.ResponseWriter, r *http.Request, p *Principal) bool {
	if originFree(p) || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if sameOrigin(r) {
		return true
	}
	s.log.Warn("refused a cross-site request", "name", p.Name, "role", p.Role,
		"method", r.Method, "path", r.URL.Path, "ip", clientIP(r))
	fail(w, http.StatusForbidden, crossSiteNo)
	return false
}

// originFree reports whether a principal is exempt from the Origin rule on
// Handler, on a request and on a socket alike: it is one no browser can have
// attached to a request by itself, so no page can have tricked it into one.
// Only a bearer token is: it is put on a request only by the code that meant
// to make it. LocalHandler never asks, since it reads no Origin for anybody,
// so no principal is exempt here for arriving there.
func originFree(p *Principal) bool { return p.Via == "token" }

// sameOrigin reports whether this request says it came from this server's
// own page. Origin is what a browser sends; Referer is the fallback for the
// few requests that carry one and not the other.
func sameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || origin == "null" {
		origin = strings.TrimSpace(r.Header.Get("Referer"))
	}
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// crossSiteWrite reports whether a request that is not authorized by a
// cookie nevertheless arrived from somewhere else. It is the softer half of
// the rule, for the one route a browser reaches without a session yet — the
// sign-in itself. A caller with no Origin at all is a program, not a page,
// and has nothing to be tricked into.
func crossSiteWrite(r *http.Request) bool {
	if r.Header.Get("Origin") == "" && r.Header.Get("Referer") == "" {
		return false
	}
	return !sameOrigin(r)
}

// refuseRole is the answer to somebody who is who they say they are and
// asked for something their role does not cover. It is 403 and not 401:
// there is nothing to try again with.
func (s *Server) refuseRole(w http.ResponseWriter, r *http.Request, p *Principal, need Role) {
	s.log.Warn("refused by role", "name", p.Name, "role", p.Role, "needed", need,
		"method", r.Method, "path", r.URL.Path, "ip", clientIP(r))
	fail(w, http.StatusForbidden, watchOnlyFor(p))
}
