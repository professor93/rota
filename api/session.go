package api

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// A page cannot be given a bearer token. Whoever opens it has a password, or
// a link somebody sent them, and what they get back is a cookie — which the
// browser then attaches on its own, and which no script on the page can read
// or lose to one.
//
// Sessions live in memory and die with the server. That is a decision, not a
// gap: rota has no database, the alternative is a file of live credentials on
// disk, and a server that restarts has usually just been upgraded or
// reconfigured — signing in again is the honest cost of that.

const (
	// defaultSessionTTL is how long a page sign-in lasts when the file does
	// not say. Long enough for a working day, short enough that a laptop
	// left open overnight is signed out by morning.
	defaultSessionTTL = 12 * time.Hour
	// maxSessions bounds what the table can grow to. Every session here was
	// made by somebody who proved a password or spent an invite, so this is
	// not an attacker's lever so much as a cap on a script that signs in in
	// a loop. The ones expiring soonest go first.
	maxSessions = 1024
	// defaultInviteTTL and maxInviteTTL bound how long a link works for. A
	// day is the outside: an invite is a credential lying in somebody's
	// chat history, and it should stop being one.
	defaultInviteTTL = 10 * time.Minute
	maxInviteTTL     = 24 * time.Hour
)

// secret makes one credential: 32 random bytes, written the way a URL and a
// cookie both take without escaping.
func secret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

/* ------------------------------------------------------------- sessions --- */

// sessionTable is every live page session, by id. Expiry is absolute and
// checked on the way past: there is no sweeper goroutine, because a table
// nobody is reading is a table nobody is paying for either.
type sessionTable struct {
	mu   sync.Mutex
	rows map[string]*Principal
	ttl  time.Duration
	now  func() time.Time
}

func newSessions(ttl time.Duration) *sessionTable {
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	return &sessionTable{rows: map[string]*Principal{}, ttl: ttl, now: time.Now}
}

// start makes a session for one principal and returns it with its id and
// expiry filled in. cap shortens this one session's life below the
// configured one, which is what an invite does.
func (t *sessionTable) start(p Principal, limit time.Duration) (*Principal, error) {
	id, err := secret()
	if err != nil {
		return nil, err
	}
	ttl := t.ttl
	if limit > 0 && limit < ttl {
		ttl = limit
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweep()
	p.session, p.Expires = id, t.now().Add(ttl)
	row := p
	t.rows[id] = &row
	return &row, nil
}

// get is the session behind one cookie value, or nil when there is none, it
// has expired, or somebody is guessing.
func (t *sessionTable) get(id string) *Principal {
	t.mu.Lock()
	defer t.mu.Unlock()
	row := t.rows[id]
	if row == nil {
		return nil
	}
	if !t.now().Before(row.Expires) {
		delete(t.rows, id)
		return nil
	}
	copied := *row
	return &copied
}

// end forgets one session. Signing out has to be a fact on the server and
// not only a cookie the browser dropped: the cookie may already be
// somewhere else.
func (t *sessionTable) end(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.rows, id)
}

// sweep drops what has expired, and then, if the table is still full, the
// sessions closest to expiring anyway. Called with the lock held.
func (t *sessionTable) sweep() {
	now := t.now()
	for id, row := range t.rows {
		if !now.Before(row.Expires) {
			delete(t.rows, id)
		}
	}
	if len(t.rows) < maxSessions {
		return
	}
	ids := make([]string, 0, len(t.rows))
	for id := range t.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return t.rows[ids[i]].Expires.Before(t.rows[ids[j]].Expires) })
	for _, id := range ids[:len(ids)-maxSessions+1] {
		delete(t.rows, id)
	}
}

/* -------------------------------------------------------------- the API --- */

// sessionView is what the three session routes answer with.
type sessionView struct {
	Name    string    `json:"name"`
	Role    Role      `json:"role"`
	Via     string    `json:"via"`
	Expires time.Time `json:"expires,omitzero"`
}

func view(p *Principal) sessionView {
	return sessionView{Name: p.Name, Role: p.Role, Via: p.Via, Expires: p.Expires}
}

// session is GET, POST and DELETE on /v1/session in one handler, because the
// three are one thing seen three ways and the route is registered once,
// outside the guard: a sign-in cannot be asked to prove who it is first, and
// a sign-out has to work for whoever is signed in, watcher included.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.whoAmI(w, r)
	case http.MethodPost:
		s.signIn(w, r)
	case http.MethodDelete:
		s.signOut(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		fail(w, http.StatusMethodNotAllowed, "/v1/session takes GET, POST and DELETE")
	}
}

// whoAmI is what the page asks first: who am I, if anyone. A caller with no
// credential at all is told 401 in the usual words, and it is not counted as
// a guess — asking is how the page finds out it has to sign in.
func (s *Server) whoAmI(w http.ResponseWriter, r *http.Request) {
	p, guessed := s.resolve(r)
	if p == nil {
		s.refuseToken(w, r, guessed)
		return
	}
	writeJSON(w, http.StatusOK, view(p))
}

// signIn checks a name and a password against the users the file names, and
// on success sets the cookie.
//
// Every failure answers the same sentence, whether the name is unknown or
// the password is wrong, and costs the same derivation: the two are one fact
// to whoever is asking, and telling them apart is how a list of names is
// harvested. The limiter counts these exactly as it counts bad tokens.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	if crossSiteWrite(r) {
		fail(w, http.StatusForbidden, crossSiteNo)
		return
	}
	ip := clientIP(r)
	if s.limit.blocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many failed sign-ins; try again later"})
		return
	}
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := strictJSON(io.LimitReader(r.Body, maxMemory), &in); err != nil {
		fail(w, http.StatusBadRequest, `a sign-in is {"name":"…","password":"…"}`)
		return
	}
	// The name is looked up and the derivation run whatever the answer is:
	// an early return on an unknown name is a timing side channel with a
	// list of names on the other end of it.
	found := s.userNamed(in.Name)
	check := s.decoy
	if found != nil {
		check = found.pw
	}
	ok := check.matches(in.Password)
	if found == nil || !ok {
		s.limit.fail(ip)
		s.log.Warn("sign-in refused", "name", in.Name, "ip", ip)
		fail(w, http.StatusUnauthorized, "that name and password do not go together")
		return
	}
	p, err := s.sessions.start(Principal{Name: found.name, Role: found.role, Via: "user"}, 0)
	if err != nil {
		s.report(w, r, err)
		return
	}
	s.setCookie(w, r, p)
	s.log.Info("signed in", "name", p.Name, "role", p.Role, "ip", ip)
	writeJSON(w, http.StatusOK, view(p))
}

// signOut ends the session this request carries and clears the cookie. A
// request with no session is not an error: it already is what it asked for.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookie)
	if err == nil && c.Value != "" {
		if crossSiteWrite(r) {
			fail(w, http.StatusForbidden, crossSiteNo)
			return
		}
		if p := s.sessions.get(c.Value); p != nil {
			s.log.Info("signed out", "name", p.Name, "role", p.Role, "ip", clientIP(r))
		}
		s.sessions.end(c.Value)
	}
	s.clearCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"signed_out": true})
}

// userNamed finds one configured user by name. Names are compared exactly:
// a file says what it says, and folding case here would make two entries
// that look different the same one.
func (s *Server) userNamed(name string) *user {
	for i := range s.users {
		if s.users[i].name == name {
			return &s.users[i]
		}
	}
	return nil
}

// setCookie writes the session cookie. Every attribute is the strict one:
// no script may read it, no other site may cause it to be sent, it belongs
// to this whole server and nothing above it, and it is marked Secure exactly
// when the request that earned it arrived over TLS — marking it Secure on a
// plain http server would mean the browser never sent it back.
func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, p *Principal) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    p.session,
		Path:     "/",
		Expires:  p.Expires,
		MaxAge:   int(time.Until(p.Expires).Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

/* -------------------------------------------------------------- invites --- */

// inviteTable is the codes handed out and not yet spent. A code is one use:
// taking it out of the table is what spending it means, so two browsers
// racing the same link cannot both get in.
type inviteTable struct {
	mu   sync.Mutex
	rows map[string]time.Time
	now  func() time.Time
}

func newInvites() *inviteTable {
	return &inviteTable{rows: map[string]time.Time{}, now: time.Now}
}

func (t *inviteTable) make(ttl time.Duration) (string, time.Time, error) {
	code, err := secret()
	if err != nil {
		return "", time.Time{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for c, at := range t.rows {
		if !now.Before(at) {
			delete(t.rows, c)
		}
	}
	at := now.Add(ttl)
	t.rows[code] = at
	return code, at, nil
}

// spend consumes one code, and reports whether it was there and still good.
func (t *inviteTable) spend(code string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.rows[code]
	if !ok {
		return false
	}
	delete(t.rows, code)
	return t.now().Before(at)
}

// makeInvite is POST /v1/invites: a link somebody with control can send to
// somebody who should see and not touch.
func (s *Server) makeInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TTL string `json:"ttl"`
	}
	if err := decodeOptional(r, &in); err != nil {
		fail(w, http.StatusBadRequest, `an invite is {"ttl":"10m"}, or nothing at all`)
		return
	}
	ttl := defaultInviteTTL
	if in.TTL != "" {
		d, err := time.ParseDuration(in.TTL)
		if err != nil {
			fail(w, http.StatusBadRequest, "ttl is a length of time, like \"10m\"")
			return
		}
		ttl = d
	}
	switch {
	case ttl <= 0:
		fail(w, http.StatusBadRequest, "an invite has to last longer than nothing")
		return
	case ttl > maxInviteTTL:
		fail(w, http.StatusBadRequest, "an invite may last a day at most")
		return
	}
	code, at, err := s.invites.make(ttl)
	if err != nil {
		s.report(w, r, err)
		return
	}
	p := who(r)
	s.log.Info("invite made", "by", p.Name, "ttl", ttl.String(), "ip", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"url": inviteURL(r, code), "expires": at})
}

// inviteURL is the link as the person who asked for it would type it: the
// address they reached this server on, which is the one that works from
// wherever they are.
func inviteURL(r *http.Request, code string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/invite/" + code
}

// useInvite is GET /invite/{code}: the landing a watcher opens. It spends
// the code, signs them in as a watcher, and sends them to the page. A code
// that is not there — spent, expired, or invented — answers 404 as a plain
// page, and counts as a guess, because guessing one is the only way in that
// does not need a password.
func (s *Server) useInvite(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	ip := clientIP(r)
	if s.limit.blocked(ip) {
		plainPage(w, http.StatusTooManyRequests, "Too many tries. Try again later.")
		return
	}
	if !s.invites.spend(code) {
		s.limit.fail(ip)
		// The code is never logged: it is a credential, and a log is read by
		// more people than a session is.
		s.log.Warn("invite refused", "ip", ip)
		plainPage(w, http.StatusNotFound, "This invite link has been used already, or has expired.")
		return
	}
	// The name says what it is and which invite it was, without being the
	// invite: six characters of a spent code identify the visit in the log
	// and open nothing.
	p, err := s.sessions.start(Principal{Name: "invite-" + code[:6], Role: RoleWatch, Via: "invite"}, maxInviteTTL)
	if err != nil {
		s.report(w, r, err)
		return
	}
	s.setCookie(w, r, p)
	s.log.Info("invite used", "name", p.Name, "role", p.Role, "ip", ip)
	http.Redirect(w, r, "/playground", http.StatusSeeOther)
}

// plainPage is the one HTML this server writes that is not the playground:
// a sentence for somebody who followed a link that did not work. It carries
// the page's own headers, because it is a page.
func plainPage(w http.ResponseWriter, code int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>rota</title>` +
		`<style>body{font:15px/1.5 system-ui,sans-serif;margin:12vh auto;max-width:34em;padding:0 1em}</style>` +
		`<p>` + msg + `</p>`))
}

/* --------------------------------------------------------------- health --- */

// health is the liveness probe, and it is deliberately the least
// interesting route here: no credential, no rate limit, and one fact. It is
// its own group so that a server with its page switched off — which takes
// GET / with it — still has something a watchdog can read, and it says
// nothing else, not even a version, because whoever can reach it has proved
// nothing.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
