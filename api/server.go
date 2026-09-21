// Package api serves rota's accounts over HTTP: one endpoint per account
// runs its vendor CLI with fully specified options, streaming or buffered,
// behind a single bearer token.
//
// It is a thin shell over the rota library — every credential decision still
// lives there — and it is built on net/http alone. That is deliberate: a
// router framework would have added a hundred packages and most of a
// binary's weight for pattern matching the standard library has done since
// Go 1.22.
package api

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/rotation"
	"github.com/professor93/rota/sessions"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

// Options configures a server. Token is mandatory; everything else has a
// safe default.
type Options struct {
	Dir            string   // store directory ("" = rota default)
	Token          string   // required bearer token
	Roots          []string // cwd/uploads confined here; empty = unconfined
	AllowDangerous bool     // permit permission-bypass / full-access options
	// AllowRawFlags re-opens the args passthrough. It is off by default:
	// every option this server gates has a vendor flag that undoes it, so
	// raw flags and the gates cannot both mean anything.
	AllowRawFlags bool
	Timeout       time.Duration // hard cap per run (default 10m)
	MaxConcurrent int           // concurrent child processes (default 8)
	Log           *slog.Logger  // where requests are recorded (default: discard)
	// InputTimeout is the hard cap on a run that stays open. It is separate
	// from Timeout because the two are asked for different things: a one-shot
	// run should answer in minutes, while a run somebody is talking to is
	// expected to sit idle between messages. Default one hour.
	InputTimeout time.Duration
	// InputGrace is how long an open run survives with nobody reading it
	// before it is interrupted and closed. A dropped connection is usually a
	// client about to reconnect, and killing the agent for a broken proxy
	// would throw away the conversation it is holding. Default 60s; negative
	// closes the run the moment its reader goes.
	InputGrace time.Duration
	// Replay is how many events an open run keeps, so a reader that
	// reattaches is given what it missed. Default 1000; the oldest are
	// dropped past it, and a reader that was away longer than that is told
	// where the stream is rather than everything it did.
	Replay int
	// RefreshEvery is how often the server rotates expiring tokens and
	// re-reads usage on its own, so a request never waits to find out that
	// its credential expired or that the rotation decided from an hour-old
	// number. Zero means the default; negative turns it off.
	RefreshEvery time.Duration
	// Routes is which groups of routes this server answers. Nil is all of
	// them, which is what every server answered before there was a way to
	// ask for fewer.
	Routes *Routes
	// Users are the people who may sign in on the page, each with a role and
	// a derived password. Empty means nobody can: the page then offers only
	// the bearer token it always did.
	Users []User
	// Tokens are bearer tokens besides Token, each with a role of its own.
	// Token itself is always control — it is the server's own key.
	Tokens []TokenPrincipal
	// SessionTTL is how long a page sign-in lasts. Zero is twelve hours.
	SessionTTL time.Duration
}

// User is one person who may sign in on the page. Password is the derived
// form `rota serve passwd` prints; the plain password is never here, and
// never anywhere else either.
type User struct {
	Name     string
	Role     Role
	Password string
}

// TokenPrincipal is one more bearer token, named and with a role. SHA256 is
// the hex digest of the token: the token itself is not stored, so a
// configuration somebody reads gives them nothing to send.
type TokenPrincipal struct {
	Name   string
	Role   Role
	SHA256 string
}

// user and token are the checked forms of the two above, built once by New
// so that no request pays for parsing a file.
type user struct {
	name string
	role Role
	pw   *password
}

type token struct {
	name string
	role Role
	sum  [32]byte
}

// Routes selects the groups of routes a server registers. A group that is
// off is not registered at all, so its paths answer 404 exactly as a path
// this server never had would: nothing tells a stranger that a door is
// there but shut.
//
// Playground covers the page, the version at the root and the invite
// landing; WebSocket covers the three socket routes; API covers everything
// else under /v1. Those two are the API in another shape, so neither means
// anything without it. Health is one route and depends on nothing, which is
// what makes it worth having: a probe that goes off with the page is not a
// probe.
type Routes struct {
	API        bool
	Playground bool
	WebSocket  bool
	Health     bool
}

// allRoutes is every group on.
func allRoutes() Routes {
	return Routes{API: true, Playground: true, WebSocket: true, Health: true}
}

// defaultRefreshEvery is a little under the quota cache's lifetime, so a
// reading is renewed shortly after it goes stale rather than a whole period
// later, and a token is rotated well before its five-minute expiry buffer.
const defaultRefreshEvery = 2 * time.Minute

// Server is a configured HTTP handler over a rota store.
type Server struct {
	opts   Options
	sem    chan struct{}
	limit  *limiter
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	forced map[int]time.Time
	forcMu sync.Mutex
	// runs are the open runs this server is holding, by id: a run that takes
	// more messages outlives the request that started it, so something has to
	// know where it is. Ended ones stay a few minutes, for a reader that
	// reattaches late.
	runs   map[string]*liveRun
	runsMu sync.Mutex
	// keeperDone closes when the background refresher has returned, so Stop
	// can promise that nothing is still writing to the store afterwards.
	keeperDone chan struct{}
	// Who may ask for what: the users and tokens the configuration named,
	// checked once here, and the two tables of live credentials a page
	// hands out. All of it is read-only after New but the two tables, which
	// hold their own locks.
	users    []user
	tokens   []token
	decoy    *password
	sessions *sessionTable
	invites  *inviteTable
}

// New validates options and builds a server. It refuses to start without a
// token: an open account-runner on the network is not something to expose by
// accident.
func New(opts Options) (*Server, error) {
	rota.UserAgent = "rota/" + wire.Version // the server speaks as rota, not as the bare SDK
	if opts.Token == "" {
		return nil, errors.New("api: a bearer token is required (rota serve --token=...)")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 8
	}
	if opts.InputTimeout <= 0 {
		opts.InputTimeout = time.Hour
	}
	if opts.InputGrace == 0 {
		// Zero is "unset", not "no grace at all": a negative grace is how a
		// caller says it wants the run gone with its reader.
		opts.InputGrace = time.Minute
	}
	if opts.Replay <= 0 {
		opts.Replay = 1000
	}
	log := opts.Log
	if log == nil {
		log = slog.New(discardHandler{})
	}
	if opts.RefreshEvery == 0 {
		opts.RefreshEvery = defaultRefreshEvery
	}
	if opts.Routes == nil {
		r := allRoutes()
		opts.Routes = &r
	}
	users, err := checkUsers(opts.Users)
	if err != nil {
		return nil, err
	}
	tokens, err := checkTokens(opts.Tokens)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		opts: opts, sem: make(chan struct{}, opts.MaxConcurrent), limit: newLimiter(),
		log: log, ctx: ctx, cancel: cancel, forced: map[int]time.Time{},
		runs:     map[string]*liveRun{},
		users:    users,
		tokens:   tokens,
		decoy:    decoyFor(users),
		sessions: newSessions(opts.SessionTTL),
		invites:  newInvites(),
	}
	if opts.RefreshEvery > 0 {
		s.keeperDone = make(chan struct{})
		go s.keepFresh(opts.RefreshEvery)
	}
	return s, nil
}

// checkUsers and checkTokens turn what a caller wrote into what a request
// is answered from, refusing anything that cannot be. A server is not
// started with a principal it would never be able to admit: the file that
// named them is the thing to fix, and start-up is when somebody is looking.
func checkUsers(in []User) ([]user, error) {
	out := make([]user, 0, len(in))
	seen := map[string]bool{}
	for i, u := range in {
		at := entryName("users", i, u.Name)
		if u.Name == "" {
			return nil, fmt.Errorf("api: %s: name is empty", at)
		}
		if seen[u.Name] {
			return nil, fmt.Errorf("api: %s: there is already a user called %q", at, u.Name)
		}
		seen[u.Name] = true
		role, err := parseRole(string(u.Role))
		if err != nil {
			return nil, fmt.Errorf("api: %s: role: %w", at, err)
		}
		pw, err := parsePassword(u.Password)
		if err != nil {
			return nil, fmt.Errorf("api: %s: password: %w", at, err)
		}
		out = append(out, user{name: u.Name, role: role, pw: pw})
	}
	return out, nil
}

func checkTokens(in []TokenPrincipal) ([]token, error) {
	out := make([]token, 0, len(in))
	seen := map[string]bool{}
	for i, t := range in {
		at := entryName("tokens", i, t.Name)
		if t.Name == "" {
			return nil, fmt.Errorf("api: %s: name is empty", at)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("api: %s: there is already a token called %q", at, t.Name)
		}
		seen[t.Name] = true
		role, err := parseRole(string(t.Role))
		if err != nil {
			return nil, fmt.Errorf("api: %s: role: %w", at, err)
		}
		sum, err := parseSum(t.SHA256)
		if err != nil {
			return nil, fmt.Errorf("api: %s: sha256: %w", at, err)
		}
		out = append(out, token{name: t.Name, role: role, sum: sum})
	}
	return out, nil
}

// keepFresh rotates expiring tokens and re-reads usage until the server
// stops. The first sweep is one interval in, not at startup: a server that
// has just been started is being started to serve, and the first request
// refreshes what it needs anyway.
func (s *Server) keepFresh(every time.Duration) {
	defer close(s.keeperDone)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.maintain()
		}
	}
}

// maintain is one sweep. It is deliberately unremarkable about failure: a
// provider that is down leaves its account as it was, and the next sweep
// tries again.
func (s *Server) maintain() {
	defer func() {
		// A provider panicking on a timer would otherwise take the server
		// down, and with it every run in flight.
		if v := recover(); v != nil {
			s.log.Error("panic while refreshing in the background", "panic", v)
		}
	}()
	st, err := s.openStore()
	if err != nil {
		s.log.Warn("background refresh: the store could not be opened", "err", err)
		return
	}
	defer st.Close()
	errs := st.Maintain(s.ctx)
	for _, e := range errs {
		s.log.Warn("background refresh", "err", e)
	}
	s.log.Debug("background refresh", "accounts", len(st.Accounts), "problems", len(errs))
}

// Stop ends every run still in flight.
//
// Shutting the HTTP server down waits for handlers but never cancels them,
// and each vendor CLI runs in a process group of its own precisely so that
// a signal to rota does not reach it. Without this, exiting would leave
// agents running: still spending, still writing to a staged credential file
// whose store no longer holds the lock.
// It also waits, briefly, for the background refresher to return. A sweep
// that is still running after Stop holds the store's lock and is about to
// write to it, which is exactly the state a caller thinks it has left behind
// — and, for a test, is a directory that will not delete.
func (s *Server) Stop() {
	s.cancel()
	if s.keeperDone == nil {
		return
	}
	select {
	case <-s.keeperDone:
	case <-time.After(5 * time.Second):
		// A sweep can be inside a provider call with its own timeout. Waiting
		// for that is worse than leaving it: it will find the cancelled
		// context on its next turn round the loop.
		s.log.Warn("background refresh did not stop in time")
	}
}

// Handler returns the router. Mount it on any net/http server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The root answers rather than redirects: something reaching a rota
	// server first wants to know it is alive and which version it is
	// talking to, and neither answer is worth a round trip to find. It says
	// only that — the name is in the address just asked, and the page is one
	// level of help away. It is also the only such path: a second one saying
	// the same thing is a second thing to keep true, and this one is
	// unauthenticated and outside the rate limiter, which is everything a
	// watchdog needs.
	if s.opts.Routes.Playground {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "version": wire.Version})
		})
		mux.HandleFunc("GET /playground", s.playground)
		// Where an invite link lands. It is in this group because what it
		// leads to is the page: a server with no page has nothing to invite
		// anyone to.
		mux.HandleFunc("GET /invite/{code}", s.useInvite)
	}
	if s.opts.Routes.Health {
		mux.HandleFunc("GET /v1/health", s.health)
	}
	if s.opts.Routes.API {
		// The three session routes are registered without the guard, and
		// each decides for itself: signing in cannot be asked to prove who
		// it is first, asking who you are is how a page finds out it is
		// nobody, and signing out has to work for a watcher too.
		mux.HandleFunc("/v1/session", s.session)
	}

	if s.opts.Routes.API {
		for pattern, h := range s.guarded() {
			mux.Handle(pattern, s.guard(pattern, h))
		}
	}
	if s.opts.Routes.WebSocket {
		for pattern, h := range s.sockets() {
			mux.Handle(pattern, s.wsAuth(pattern, h))
		}
	}
	return s.recover(mux)
}

// guarded is every route under /v1 that takes a principal, HTTP side. It is
// a method rather than a literal inside Handler so that the role table can
// be checked against it: a route added here without a row there is a failing
// test rather than a route that quietly takes control.
func (s *Server) guarded() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /v1/schema":               s.schema,
		"GET /v1/accounts":             s.listAccounts,
		"GET /v1/accounts/{id}/schema": s.accountSchema,
		"POST /v1/accounts/{id}/run":   s.run,
		// The same run, without naming an account: the rotation chooses.
		"POST /v1/run": s.run,
		// A run started with "input" stays open, and these reach it by the id
		// its opening event carried.
		"GET /v1/runs":                 s.listRuns,
		"GET /v1/runs/{id}":            s.describeRun,
		"GET /v1/runs/{id}/events":     s.runEvents,
		"POST /v1/runs/{id}/messages":  s.runSend,
		"POST /v1/runs/{id}/interrupt": s.runInterrupt,
		"POST /v1/runs/{id}/close":     s.runClose,
		"PATCH /v1/accounts/{id}":      s.patchAccount,
		"DELETE /v1/accounts/{id}":     s.removeAccount,
		"POST /v1/login":               s.loginBegin,
		"POST /v1/login/{id}":          s.loginFinish,
		// What the same two were called before the API and the CLI agreed on
		// one word for one act. Kept because a published path that starts
		// answering 404 breaks whoever was calling it.
		"POST /v1/auth":      s.loginBegin,
		"POST /v1/auth/{id}": s.loginFinish,
		// A link for somebody who should see and not touch.
		"POST /v1/invites": s.makeInvite,
	}
}

// sockets is the same three doors as a WebSocket: one run, carried both
// ways. They are guarded apart because a browser cannot put a header on a
// WebSocket, so the credential may arrive as a subprotocol or a cookie.
func (s *Server) sockets() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /v1/runs/{id}/ws":     s.attachWS,
		"GET /v1/accounts/{id}/ws": s.startWS,
		"GET /v1/ws":               s.startWS,
	}
}

// recover keeps one panicking request from taking the server down, and
// records it. A half-written streaming response cannot be turned into an
// error page, so the status is only set when nothing has been sent yet.
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", v)
				if !tw.wrote {
					writeJSON(tw, http.StatusInternalServerError, map[string]any{"error": "internal error"})
				}
			}
		}()
		next.ServeHTTP(tw, r)
	})
}

// trackingWriter remembers whether anything reached the client, and carries
// Flush through for streaming responses.
type trackingWriter struct {
	http.ResponseWriter
	wrote  bool
	status int
}

func (t *trackingWriter) WriteHeader(code int) {
	if t.wrote {
		return
	}
	t.wrote, t.status = true, code
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

func (t *trackingWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands the connection over, which is how a request becomes a
// WebSocket. Nothing may be written as a response afterwards, so the
// connection counts as written to: a panic after this must not try to turn it
// into an error page.
func (t *trackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("this connection cannot be taken over")
	}
	t.wrote = true
	return hj.Hijack()
}

// refuseToken is the answer to a request with no principal behind it, and
// the record of it. guessed says a bearer credential was offered and was
// wrong, which is the only thing the limiter counts: a page asking who it is
// before it has signed in is not a guess, and neither is a cookie that has
// simply expired.
//
// The block applies to guesses, never to a credential that is right: an
// address is shared by everyone behind a proxy, and the loopback is
// reachable from any web page the operator visits, so a block that refused
// the right token would let a stranger lock the operator out of their own
// server. Guessing is still throttled — a wrong token from a blocked address
// answers 429 — and the token itself, 256 random bits, is what makes
// guessing hopeless.
func (s *Server) refuseToken(w http.ResponseWriter, r *http.Request, guessed bool) {
	ip := clientIP(r)
	if s.limit.blocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many bad tokens; try again later"})
		return
	}
	if guessed {
		s.limit.fail(ip)
	}
	s.log.Warn("rejected request", "ip", ip, "path", r.URL.Path)
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or missing bearer token"})
}

// clientIP is the address the connection came from, as the limiter counts
// it. Forwarded headers are ignored on purpose: anyone can send them, and
// trusting them would let a caller shed the rate limit by inventing an
// address. An IPv6 host holds a whole /64, so that is the unit there —
// otherwise one host has more addresses than the table has rows.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, doc any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store") // account emails and answers are nobody's to keep
	w.WriteHeader(code)
	// v2 does not escape HTML, which is what this always wanted: an authorize
	// URL stays readable instead of arriving full of \u0026.
	_ = rota.EncodeTo(w, doc)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// report answers with an error, showing its text only when rota named the
// condition itself. Anything else — a filesystem path, a provider's raw
// reply — is logged and replaced, because a 500 is exactly where internals
// leak to whoever is asking.
func (s *Server) report(w http.ResponseWriter, r *http.Request, err error) {
	code := statusFor(err)
	// A refusal this package wrote is always said as it was written: it was
	// written for whoever is asking, and leaks nothing it did not mean to.
	var no *refusal
	if errors.As(err, &no) {
		fail(w, no.code, no.msg)
		return
	}
	if code >= http.StatusInternalServerError {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		fail(w, code, "internal error")
		return
	}
	fail(w, code, err.Error())
}

// pathID reads the {id} the route captured. A route with no {id} at all —
// the one that leaves the choice to the rotation — yields zero.
func pathID(r *http.Request) (int, error) {
	raw := r.PathValue("id")
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id < 1 {
		return 0, errors.New("bad account id")
	}
	return id, nil
}

// openStore takes the store, wherever the server takes it, and gives it
// somewhere to put a warning.
//
// A warning is what the store says when a run will happen anyway but not
// quite as asked — a Claude Code configuration that could not be mirrored,
// so the account shares the person's daemon after all. Nobody is refused
// over it, and the log is the only place a server has to say so. Opening
// the store is still the caller's failure to report: each of the three has
// a different way of failing a caller.
func (s *Server) openStore() (*store.Store, error) {
	st, err := store.Open(s.opts.Dir)
	if err != nil {
		return nil, err
	}
	st.Warn = func(msg string) { s.log.Warn(msg) }
	return st, nil
}

// open takes the store for one request, reporting the failure itself.
func (s *Server) open(w http.ResponseWriter) (*store.Store, bool) {
	st, err := s.openStore()
	if err != nil {
		s.log.Error("opening the store", "err", err)
		fail(w, http.StatusInternalServerError, "the account store could not be opened")
		return nil, false
	}
	// A store written before rotation existed is numbered here rather than
	// by the store itself, which has no opinion about queues.
	rotation.Backfill(st)
	return st, true
}

// account resolves the {id} in the path against the store, or asks the
// rotation when the route carried none.
func (s *Server) account(w http.ResponseWriter, r *http.Request, st *store.Store) (*rota.Account, bool) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	a, err := rotation.Choose(r.Context(), st, id)
	if err != nil {
		s.report(w, r, err)
		return nil, false
	}
	return a, true
}

// schema tells a client exactly what this server accepts: which providers
// exist, which fields each takes, and which values are allowed. The
// playground is built from it, and so is any other generated form.
func (s *Server) schema(w http.ResponseWriter, _ *http.Request) {
	providers := map[string]any{}
	for _, name := range rota.Providers() {
		dm, de := rota.Defaults(name)
		providers[name] = map[string]any{
			"flavor":   rota.Flavor(name),
			"models":   rota.Models(name),
			"efforts":  rota.Efforts(name),
			"defaults": map[string]any{"model": dm, "effort": de},
			"metered":  rota.Metered(name),
			"fields":   wire.Fields(name),
			"hidden":   wire.Hidden(name),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         wire.Version,
		"providers":       providers,
		"fields":          wire.Fields(""),
		"allow_dangerous": s.opts.AllowDangerous,
		"allow_raw_flags": s.opts.AllowRawFlags,
		"roots":           s.opts.Roots,
		// A run that stays open is carried on a socket, so a page that is
		// told there are none knows not to offer one.
		"websocket": s.opts.Routes.WebSocket,
	})
}

// accountSchema describes one account: the same shape as a provider entry,
// but with the models that account may actually use.
func (s *Server) accountSchema(w http.ResponseWriter, r *http.Request) {
	st, ok := s.open(w)
	if !ok {
		return
	}
	defer st.Close()
	a, ok := s.account(w, r, st)
	if !ok {
		return
	}
	dm, de := rota.Defaults(a.Provider)
	writeJSON(w, http.StatusOK, map[string]any{
		"account": a.ID, "provider": a.Provider, "flavor": rota.Flavor(a.Provider),
		"models": rota.ModelsFor(a, st.Home(a)), "efforts": rota.Efforts(a.Provider),
		"defaults": map[string]any{"model": dm, "effort": de},
		"fields":   wire.Fields(a.Provider),
	})
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("refresh")
	force := q == "1" || q == "true"
	if force {
		// A forced refresh goes to the network, so it takes a run slot —
		// and takes it before the store, so waiting for one holds nothing.
		if !s.acquire(r.Context()) {
			return
		}
		defer s.release()
	}
	st, ok := s.open(w)
	if !ok {
		return
	}
	defer st.Close()
	if force {
		// A forced refresh skips the quota cache, so left ungoverned it is
		// a lever any caller can pull to get the account rate-limited by
		// the provider. One forced read per account per minute is plenty
		// for a person watching a dashboard.
		if accounts := s.mayForce(st.Accounts); len(accounts) > 0 {
			for _, err := range st.Refresh(r.Context(), true, accounts...) {
				s.log.Warn("refreshing usage", "err", err)
			}
		}
	}
	// Rotation order is the listing's order, so a client that renders the
	// rows in the order it received them shows the queue as it will be
	// spent. Accounts left out of the rotation come last.
	rotation.Sort(st.Accounts)
	out := make([]wire.Account, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		view := wire.Describe(a)
		view.Threshold = rotation.Cutoff(a)
		out = append(out, view)
	}
	doc := map[string]any{"accounts": out}
	if pick, err := rotation.Pick(st.Accounts); err == nil {
		doc["default"] = pick.ID
	}
	// ?sessions=1 adds what the CLIs are doing: which are running, and which
	// conversations could be resumed. It is asked for rather than always
	// sent, because answering it reads directories that belong to other
	// programs, and a dashboard polling this endpoint should not pay for
	// that on every tick. recent=0 lifts the per-account limit.
	if q := r.URL.Query().Get("sessions"); q == "1" || q == "true" {
		recent := 5
		if n, err := strconv.Atoi(r.URL.Query().Get("recent")); err == nil && n >= 0 {
			recent = n
		}
		rep := sessions.Scan(st, recent)
		doc["instances"] = rep.Instances
		doc["sessions"] = rep.Sessions
		if rep.Shared != nil {
			doc["shared"] = rep.Shared
		}
		if len(rep.Notes) > 0 {
			doc["notes"] = rep.Notes
		}
	}
	writeJSON(w, http.StatusOK, doc)
}

// patchAccount changes what a caller may change about an account: its place
// in the rotation, the usage at which the rotation moves past it, its
// project directories, and where its conversations live.
//
// All are optional, and all are rejected rather than clamped when they cannot
// mean anything: a threshold of 300 means the caller believes something that
// is not true, and silently storing a different number would leave that
// belief in place.
//
// order is a place, the same ones `rota set --order` takes: a number, or
// first, last, up, down, before:<id>, after:<id>, or 0 / out. A number may
// be sent as JSON number or string. Moving one account shifts the others, so
// the queue always reads 1, 2, 3 afterwards.
func (s *Server) patchAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order     jsontext.Value `json:"order"`
		Threshold *int           `json:"threshold"`
		Cwd       *string        `json:"cwd"`
		ConfigDir *string        `json:"config_dir"`
		Sessions  *string        `json:"sessions"`
		Long      *string        `json:"long"`
	}
	if err := decodeJSON(r, &body); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Order == nil && body.Threshold == nil && body.Cwd == nil && body.ConfigDir == nil && body.Sessions == nil && body.Long == nil {
		fail(w, http.StatusBadRequest, "nothing to change: send order, threshold, cwd, config_dir, sessions or long")
		return
	}
	// forget is all this field ever says. A long-lived token arrives by
	// somebody approving it in a browser, never in a request body, so the
	// only thing a caller can do to one here is throw it away.
	if body.Long != nil && *body.Long != "forget" {
		fail(w, http.StatusBadRequest, `long takes "forget" and nothing else`)
		return
	}
	var place rotation.Place
	if body.Order != nil {
		p, err := parsePlace(body.Order)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		place = p
	}
	if body.Threshold != nil && (*body.Threshold < 1 || *body.Threshold > 100) {
		fail(w, http.StatusBadRequest, "threshold must be a percentage from 1 to 100")
		return
	}
	st, ok := s.open(w)
	if !ok {
		return
	}
	defer st.Close()
	a, ok := s.account(w, r, st)
	if !ok {
		return
	}
	// The account is edited on a copy first: a config directory that would
	// hold credentials in a repository must be refused without having
	// already been written into the account.
	want := *a
	if body.Cwd != nil {
		want.Cwd = *body.Cwd
	}
	if body.ConfigDir != nil {
		want.ConfigDir = *body.ConfigDir
	}
	if body.Sessions != nil {
		// The same three answers the command line takes — shared, own, or a
		// directory — read the same way, and a directory judged by the same
		// rules as config_dir below: rota creates folders there and links to
		// them, so a caller who could name one outside them could have rota
		// write where the server was told not to.
		want.Sessions = wire.Sessions(*body.Sessions)
	}
	if err := want.CheckProject(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// Nor may it be rota's own directory — a sibling's home, the store —
	// nor, under roots, anywhere the server was told to stay out of.
	if err := st.CheckHome(&want, s.opts.Roots...); err != nil {
		s.report(w, r, err)
		return
	}
	// The move is made before anything is written into the account, so a
	// refused move leaves the store as it was.
	if body.Order != nil {
		if _, err := rotation.Move(st.Accounts, a, place); err != nil {
			s.report(w, r, err)
			return
		}
	}
	a.Cwd, a.ConfigDir, a.Sessions = want.Cwd, want.ConfigDir, want.Sessions
	if body.Threshold != nil {
		a.Threshold = *body.Threshold
	}
	if body.Long != nil {
		a.Long = nil
	}
	// A deliberate choice settles the rotation for this store, so a later
	// load never renumbers what was chosen here.
	st.Ordered = true
	if err := st.Save(); err != nil {
		s.report(w, r, err)
		return
	}
	s.log.Info("rotation changed", "account", a.ID, "order", a.Order, "threshold", rotation.Cutoff(a))
	view := wire.Describe(a)
	view.Threshold = rotation.Cutoff(a)
	writeJSON(w, http.StatusOK, view)
}

// parsePlace reads the order a PATCH sent — a JSON number or a string — as
// the text somebody would have typed, so the rotation package has one
// grammar to keep.
func parsePlace(raw jsontext.Value) (rotation.Place, error) {
	var s string
	if err := jsonv2.Unmarshal(raw, &s); err != nil {
		var n int
		if err := jsonv2.Unmarshal(raw, &n); err != nil {
			return rotation.Place{}, errors.New("order must be a whole number or a place: first, last, up, down, before:<id>, after:<id>, or 0 / out")
		}
		s = strconv.Itoa(n)
	}
	return rotation.ParsePlace(s)
}

func (s *Server) removeAccount(w http.ResponseWriter, r *http.Request) {
	st, ok := s.open(w)
	if !ok {
		return
	}
	defer st.Close()
	a, ok := s.account(w, r, st)
	if !ok {
		return
	}
	view := map[string]any{"id": a.ID, "provider": a.Provider, "email": a.Email}
	if err := st.Remove(a.ID); err != nil {
		s.report(w, r, err)
		return
	}
	if err := st.Save(); err != nil {
		s.report(w, r, err)
		return
	}
	s.log.Info("account removed", "account", a.ID, "provider", a.Provider)
	writeJSON(w, http.StatusOK, map[string]any{"removed": view})
}

func (s *Server) loginBegin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		// Long asks for a credential that outlives an ordinary token, the
		// same one the command line asks for with --long. It is finished at
		// the same endpoint with the same code: which login this is was
		// settled here and is remembered with it.
		Long bool `json:"long"`
	}
	// An empty body is fine: it means the default provider. A broken one
	// is not read as empty.
	if err := decodeOptional(r, &body); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if wire.Hidden(body.Provider) {
		fail(w, http.StatusBadRequest, body.Provider+" is not offered for login yet")
		return
	}
	st, ok := s.open(w)
	if !ok {
		return
	}
	defer st.Close()
	beginning := st.BeginLogin
	if body.Long {
		beginning = st.BeginLongLogin
	}
	l, err := beginning(r.Context(), body.Provider)
	if err != nil {
		s.report(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) loginFinish(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	// Device and delegated logins carry no code, so an empty body is
	// expected here. A broken one is still refused.
	if err := decodeOptional(r, &body); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	st, ok := s.open(w)
	if !ok {
		return
	}
	defer st.Close()
	id := r.PathValue("id")
	// Which login this is has to be read before it is finished: afterwards
	// the parked state is gone, and a long login ends in a different answer.
	pending, _ := st.PendingLogin(id)
	long := pending != nil && pending.Long
	a, added, err := st.FinishLogin(r.Context(), id, body.Code)
	switch {
	case errors.Is(err, rota.ErrAuthPending):
		// Not a failure: the person has not approved it yet.
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "pending"})
	case err != nil:
		s.report(w, r, err)
	case long:
		s.log.Info("long-lived token stored", "account", a.ID, "provider", a.Provider,
			"until", a.LongUntil().UTC().Format(time.RFC3339))
		writeJSON(w, http.StatusOK, map[string]any{"id": a.ID, "provider": a.Provider, "email": a.Email,
			"uuid": a.UUID, "status": "long", "long_until": a.LongUntil().UTC().Format(time.RFC3339)})
	default:
		status := "refreshed"
		if added {
			status = "added"
		}
		doc := map[string]any{"id": a.ID, "provider": a.Provider, "email": a.Email, "uuid": a.UUID, "status": status}
		if a.Delegated {
			// rota holds no credential for this one: say so, and say what
			// signs it in.
			doc["delegated"] = true
			doc["loginCommand"] = wire.LoginCommand(a, st.Home(a))
		}
		s.log.Info("login finished", "account", a.ID, "provider", a.Provider, "status", status)
		writeJSON(w, http.StatusOK, doc)
	}
}

// mayForce returns the accounts whose usage has not been force-read too
// recently.
func (s *Server) mayForce(accounts []*rota.Account) []*rota.Account {
	const cooldown = time.Minute
	now := time.Now()
	s.forcMu.Lock()
	defer s.forcMu.Unlock()
	out := make([]*rota.Account, 0, len(accounts))
	for _, a := range accounts {
		if last, seen := s.forced[a.ID]; seen && now.Sub(last) < cooldown {
			continue
		}
		s.forced[a.ID] = now
		out = append(out, a)
	}
	return out
}

// acquire takes a run slot, or reports that the caller stopped waiting.
// A run that gives up in the queue leaves nothing behind.
func (s *Server) acquire(ctx context.Context) bool {
	select {
	case s.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Server) release() { <-s.sem }

// limiter blocks an address for an hour once it sends failMax bad tokens
// within an hour. Deliberately tiny: a per-address ring of recent failure
// times, pruned on read, with a cap on how many addresses are remembered so
// a flood of forged sources cannot grow it without bound.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
	now  func() time.Time
}

const (
	failWindow = time.Hour
	failMax    = 10
	// maxTracked bounds the limiter's memory. Reaching it means thousands
	// of distinct addresses failed within the hour, which is an attack; the
	// table is dropped rather than grown.
	maxTracked = 4096
)

func newLimiter() *limiter {
	return &limiter{hits: make(map[string][]time.Time), now: time.Now}
}

func (l *limiter) recent(ip string) []time.Time {
	cut := l.now().Add(-failWindow)
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, ip)
		return nil
	}
	l.hits[ip] = kept
	return kept
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) >= maxTracked {
		l.sweep()
	}
	l.hits[ip] = append(l.recent(ip), l.now())
}

func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip)) >= failMax
}

// sweep drops every entry whose failures have all aged out, then evicts
// the addresses that failed longest ago until the table fits. Dropping
// everyone would let a flood of forged sources lift every block, its own
// included.
func (l *limiter) sweep() {
	cut := l.now().Add(-failWindow)
	for ip, times := range l.hits {
		if len(times) == 0 || !times[len(times)-1].After(cut) {
			delete(l.hits, ip)
		}
	}
	// Evict the addresses with the fewest failures first, and among those
	// the one silent longest: a block already earned outlives a flood of
	// single failures, which is the flood an attacker can afford.
	for len(l.hits) >= maxTracked {
		victim, fewest, at := "", 0, l.now()
		for ip, times := range l.hits {
			last := times[len(times)-1]
			if victim == "" || len(times) < fewest || (len(times) == fewest && last.Before(at)) {
				victim, fewest, at = ip, len(times), last
			}
		}
		delete(l.hits, victim)
	}
}

// discardHandler is the logger a caller gets when it supplies none.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }
