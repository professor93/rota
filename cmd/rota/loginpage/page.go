// Package loginpage serves the one-time login page of `rota login --web`: a
// page on a port of this machine where one login's code, API key or setup
// token is pasted from another device.
//
// The page answers only on a path holding 256 random bits; every other path
// is 404 and counts nothing. It ends after one accepted value, after five
// rejected ones, at the earlier of ten minutes and the login's own end, or
// when it is stopped, and its listener and every connection close with it.
// Nothing submitted is logged, echoed or cached. Off loopback it speaks only
// TLS 1.3, with a certificate made for this run whose fingerprint the caller
// prints; plain HTTP there is never served.
package loginpage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/professor93/rota/wire"
)

// Want is what the page asks for.
type Want string

const (
	Code  Want = "code"  // a login code, in a plain field
	Key   Want = "key"   // an API key, in a password field
	Token Want = "token" // a setup token, in a password field
)

// Options say where the page listens, what it asks for and what it does
// with what it is given.
type Options struct {
	// Addr is "host:port", "host" (port DefaultPort, or the kernel's when that
	// is busy), ":port" (127.0.0.1), or empty (127.0.0.1 on DefaultPort). The
	// host is an IP address or localhost, never a name.
	Addr string
	Want Want
	// Provider, Approve and LoginURL are what the page says: whose login it
	// is, who to approve as, and the link to approve at (empty for Key and
	// Token).
	Provider, Approve, LoginURL string
	// Ends is the login's own end; the page ends at the earlier of this and
	// Lifetime from now. Zero is no end of its own.
	Ends time.Time
	// Submit is given each value pasted, trimmed and never empty, one at a
	// time. Its done is shown and ends the page as Accepted. An error is
	// shown, with the value taken out of it, and counts a try, unless it is
	// Final, which ends the page as Refused. ctx ends with the page.
	Submit func(ctx context.Context, value string) (done string, err error)
	// Now is the clock the page's end and certificate read; nil is time.Now.
	Now func() time.Time
}

// Page is a page being served.
type Page struct {
	// URL is the link to open, secret included. Off loopback it is https.
	URL string
	// Fingerprint is the SHA-256 of the certificate in colon hex, or empty
	// on loopback, where the page is plain HTTP.
	Fingerprint string
	// Done delivers how the page ended, once.
	Done <-chan End
	// Close ends the page as Stopped, if it has not ended, and returns once
	// its listener is closed. A Submit under way is cancelled and waited for.
	Close func()

	s *state
}

// End is how a page ended.
type End struct {
	Why Why
	// Message is Submit's done for Accepted, its error for Refused and the
	// last one for Tries, with the value taken out; empty otherwise.
	Message string
}

// Why a page ended.
type Why int

const (
	Accepted Why = iota // a value was accepted
	Tries               // MaxTries values were rejected
	TimedOut            // its time ran out
	Stopped             // its context was cancelled, or Close was called
	Refused             // Submit returned a Final error
)

func (w Why) String() string {
	switch w {
	case Accepted:
		return "accepted"
	case Tries:
		return "tries"
	case TimedOut:
		return "timed out"
	case Stopped:
		return "stopped"
	case Refused:
		return "refused"
	}
	return "why(" + strconv.Itoa(int(w)) + ")"
}

const (
	// DefaultPort is where the page listens when Addr names no port, so that
	// an `ssh -L` can be set up before it starts.
	DefaultPort = 8788
	// MaxTries rejected values end the page.
	MaxTries = 5
	// Lifetime is the longest a page lasts.
	Lifetime = 10 * time.Minute
)

// maxBody is the most a POST's body may hold.
const maxBody = 64 << 10

// Seams for tests.
var (
	lifetime    = Lifetime
	defaultPort = DefaultPort
	// tlsFor says whether a page on host speaks TLS: everywhere but loopback.
	tlsFor = func(host netip.Addr) bool { return !host.IsLoopback() }
)

// Final marks a Submit error that ends the page as Refused instead of
// counting a try: the login cannot be finished any more.
func Final(err error) error {
	if err == nil {
		return nil
	}
	return &final{err}
}

type final struct{ err error }

func (f *final) Error() string { return f.err.Error() }
func (f *final) Unwrap() error { return f.err }

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// Serve starts the page and returns once it listens. It ends as Stopped
// when ctx is cancelled.
func Serve(ctx context.Context, o Options) (*Page, error) {
	switch o.Want {
	case Code, Key, Token:
	default:
		return nil, fmt.Errorf("loginpage: unknown want %q", o.Want)
	}
	if o.Submit == nil {
		return nil, errors.New("loginpage: no Submit")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	host, port, explicit, err := parseAddr(o.Addr)
	if err != nil {
		return nil, err
	}

	start := o.Now()
	end := start.Add(lifetime)
	if !o.Ends.IsZero() && o.Ends.Before(end) {
		end = o.Ends
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw[:])

	scheme, urlHost := "http", host
	var cfg *tls.Config
	var fp string
	if tlsFor(host) {
		sans, err := sansFor(host)
		if err != nil {
			return nil, err
		}
		cert, sum, err := newCert(sans, start.Add(-time.Minute), end.Add(time.Minute))
		if err != nil {
			return nil, fmt.Errorf("loginpage: make a certificate: %w", err)
		}
		cfg = &tls.Config{
			MinVersion:   tls.VersionTLS13,
			MaxVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
		}
		scheme, fp = "https", sum
		if host.IsUnspecified() {
			urlHost = linkHost(sans, host)
		}
	}

	ln, err := listen(host, port, explicit)
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		ln = tls.NewListener(ln, cfg)
	}

	pageCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan End, 1)
	s := &state{
		o:      o,
		path:   "/" + secret,
		end:    end,
		ctx:    pageCtx,
		cancel: cancel,
		done:   done,
		served: make(chan struct{}),
	}
	s.srv = newServer(s, cfg)

	go func() {
		defer close(s.served)
		_ = s.srv.Serve(ln)
	}()
	go func() {
		t := time.NewTimer(end.Sub(start))
		defer t.Stop()
		select {
		case <-ctx.Done():
			s.stop(Stopped)
		case <-t.C:
			s.stop(TimedOut)
		case <-pageCtx.Done():
		}
	}()

	u := url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(urlHost.String(), strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)),
		Path:   s.path,
	}
	return &Page{
		URL:         u.String(),
		Fingerprint: fp,
		Done:        done,
		Close: func() {
			s.stop(Stopped)
			<-s.served
		},
		s: s,
	}, nil
}

// parseAddr reads Options.Addr. explicit says a port was named.
func parseAddr(addr string) (host netip.Addr, port int, explicit bool, err error) {
	h, p := strings.TrimSpace(addr), ""
	if hh, pp, err := net.SplitHostPort(h); err == nil {
		h, p, explicit = hh, pp, true
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	port = defaultPort
	if explicit {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return netip.Addr{}, 0, false, fmt.Errorf("the page's port is not a port: %q", p)
		}
		port = n
	}
	if h == "" || strings.EqualFold(h, "localhost") {
		return netip.AddrFrom4([4]byte{127, 0, 0, 1}), port, explicit, nil
	}
	host, err = netip.ParseAddr(h)
	if err != nil {
		return netip.Addr{}, 0, false, fmt.Errorf("give the page an address, not a name: %s", h)
	}
	return host.Unmap(), port, explicit, nil
}

// listen opens the page's port. A busy default port falls back to the
// kernel's; a port that was asked for is the only one taken.
func listen(host netip.Addr, port int, explicit bool) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host.String(), strconv.Itoa(port)))
	if err != nil && !explicit && port != 0 {
		ln, err = net.Listen("tcp", net.JoinHostPort(host.String(), "0"))
	}
	return ln, err
}

// linkHost is the address the link names for a page on every interface:
// the first certified one of host's family, else the first.
func linkHost(sans []netip.Addr, host netip.Addr) netip.Addr {
	for _, a := range sans {
		if a.Is4() == host.Is4() {
			return a
		}
	}
	return sans[0]
}

// newServer is the page's http.Server: short timeouts, small headers, no
// HTTP/2 and no log at all.
func newServer(h http.Handler, cfg *tls.Config) *http.Server {
	return &http.Server{
		Handler:           h,
		TLSConfig:         cfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
}

// state is a page being served.
type state struct {
	o    Options
	path string
	end  time.Time
	srv  *http.Server

	ctx    context.Context // ends with the page; Submit runs under it
	cancel context.CancelFunc
	done   chan End
	served chan struct{} // closed once Serve returns

	// mu is held while a value is being checked, and by stop while it ends
	// the page, so a Submit under way finishes before the page's end is said.
	mu      sync.Mutex
	tries   int
	closing atomic.Bool
	once    sync.Once
}

// conclude ends the page once: it closes the listener and every connection
// and says why on done.
func (s *state) conclude(why Why, msg string) {
	s.once.Do(func() {
		s.closing.Store(true)
		s.cancel()
		_ = s.srv.Close()
		s.done <- End{Why: why, Message: msg}
	})
}

// stop ends the page from outside a request: the listener closes and a
// Submit under way is cancelled, and once it returns, an acceptance it
// made still ends the page as Accepted.
func (s *state) stop(why Why) {
	s.closing.Store(true)
	s.cancel()
	_ = s.srv.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conclude(why, "")
}

var hardening = [][2]string{
	{"Cache-Control", "no-store"},
	{"Pragma", "no-cache"},
	{"Referrer-Policy", "no-referrer"},
	{"X-Content-Type-Options", "nosniff"},
	{"X-Frame-Options", "DENY"},
	{"Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"},
}

func (s *state) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	for _, kv := range hardening {
		h.Set(kv[0], kv[1])
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Path), []byte(s.path)) != 1 {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.render(w, http.StatusOK, view{})
	case http.MethodPost:
		s.post(w, r)
	default:
		h.Set("Allow", "GET, HEAD, POST")
		http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
	}
}

// post takes one value from the form body. Only a value Submit rejects
// counts a try: an empty one, an oversized body or one sent beside another
// counts nothing.
func (s *state) post(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		var big *http.MaxBytesError
		if errors.As(err, &big) {
			http.Error(w, "413 the form is too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "400 the form cannot be read", http.StatusBadRequest)
		return
	}
	value := strings.TrimSpace(r.PostForm.Get("value"))
	if value == "" {
		s.render(w, http.StatusBadRequest, view{Error: "Nothing was entered."})
		return
	}
	if !s.mu.TryLock() {
		http.Error(w, "409 one at a time: another value is being checked", http.StatusConflict)
		return
	}
	defer s.mu.Unlock()
	if s.closing.Load() {
		s.render(w, http.StatusGone, view{Closed: "This page has closed."})
		return
	}

	done, err := s.o.Submit(s.ctx, value)
	if err == nil {
		msg := scrub(done, value)
		s.last(w, http.StatusOK, view{Done: msg})
		s.conclude(Accepted, msg)
		return
	}
	msg := scrub(err.Error(), value)
	var f *final
	if errors.As(err, &f) {
		s.last(w, http.StatusForbidden, view{Error: msg, Closed: "This login cannot be finished here any more."})
		s.conclude(Refused, msg)
		return
	}
	if s.closing.Load() {
		s.render(w, http.StatusGone, view{Closed: "This page has closed."})
		return
	}
	s.tries++
	if s.tries >= MaxTries {
		s.last(w, http.StatusForbidden, view{Error: msg, Closed: "Five wrong tries closed this page."})
		s.conclude(Tries, msg)
		return
	}
	s.render(w, http.StatusBadRequest, view{Error: msg, Left: MaxTries - s.tries})
}

// last writes the page's final answer to the wire before the page ends.
func (s *state) last(w http.ResponseWriter, code int, v view) {
	w.Header().Set("Connection", "close")
	s.render(w, code, v)
	_ = http.NewResponseController(w).Flush()
}

// view is what one rendering of the page shows.
type view struct {
	Provider, Approve, LoginURL string
	Action, Label, Type         string
	Ends, In                    string
	Error, Done, Closed         string
	Left                        int
}

func (s *state) render(w http.ResponseWriter, code int, v view) {
	v.Provider, v.Approve = s.o.Provider, s.o.Approve
	v.Action = s.path
	switch s.o.Want {
	case Code:
		v.LoginURL, v.Label, v.Type = s.o.LoginURL, "Code", "text"
	case Key:
		v.Label, v.Type = "API key", "password"
	case Token:
		v.Label, v.Type = "Setup token", "password"
	}
	v.Ends = s.end.Local().Format("15:04")
	switch left := s.end.Sub(s.o.Now()); {
	case left >= time.Minute:
		v.In = "in " + strconv.Itoa(int((left+time.Minute-1)/time.Minute)) + " min"
	case left > 0:
		v.In = "in under a minute"
	}
	var b bytes.Buffer
	if err := pageTmpl.Execute(&b, v); err != nil {
		http.Error(w, "500 the page cannot be drawn", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(code)
	_, _ = w.Write(b.Bytes())
}

// scrub is msg with value taken out, in the spellings an error may quote it
// in, and with anything shaped like an Anthropic credential removed.
func scrub(msg, value string) string {
	quoted := strconv.Quote(value)
	js, _ := json.Marshal(value)
	for _, v := range []string{
		value,
		quoted[1 : len(quoted)-1],
		strings.Trim(string(js), `"`),
		url.QueryEscape(value),
		url.PathEscape(value),
		html.EscapeString(value),
	} {
		if v != "" {
			msg = strings.ReplaceAll(msg, v, "…")
		}
	}
	return wire.Unsecret(msg)
}
