// Package sshd is rota's SSH server, on golang.org/x/crypto/ssh and nothing
// in between. It admits the public keys server.toml lists and nothing else:
// no password, no keyboard-interactive, three tries. A signed-in connection
// may open session channels alone, and a session runs one command from a
// fixed list (commands), reaching the API as the person the key belongs to
// through Options.Handler. Nothing is forwarded, no agent and no subsystem
// is served, and no environment but TERM is taken.
//
// A peer that has not signed in costs a bounded share of the server and
// learns nothing but that it is an SSH server: the version string names no
// version, a refusal says only which methods there are, and the log names
// a key by its fingerprint and never by the key. The server runs inside
// the daemon, so a panic anywhere in a connection ends that connection and
// nothing else.
package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
)

// User is one [[users]] entry that lists ssh_keys: its name, its role, and
// its keys as authorized_keys lines.
type User struct {
	Name string
	Role api.Role
	Keys []string
}

// Options is what a server is started with.
type Options struct {
	// Listen is the address to bind, as net.Listen takes it.
	Listen string
	// HostKey is the host key's absolute path. With NewHostKey, an ed25519
	// key is made there when there is none; without it, the file must exist.
	HostKey    string
	NewHostKey bool
	// Users are the people who may sign in, each by one of their keys.
	Users []User
	// Handler gives the API as one person: the server's LocalHandler.
	Handler func(p *api.Principal) http.Handler
	// Log is where sign-ins, refusals and commands are said; nil is nowhere.
	Log *slog.Logger
	// Key is the chord's key, zero meaning Ctrl-\, and Taught says whether a
	// person has been shown it; both are handed to each Channel.
	Key    byte
	Taught func(person string) bool
	// TUI, when set, is what a bare `ssh host` runs.
	TUI func(ctx context.Context, s TUISession) error
}

// TUISession is what Options.TUI is given: the person's terminal, its TERM
// (Env holds only TERM=…) and size, its resizes, a client for the API as
// that person, the chord's key, and whether a person has been shown it.
//
// Slot is taken before each attach the TUI makes and given back after it,
// by calling done: an attach keeps a copy of a session's screen in the
// daemon, so a TUI's attaches count against the same places an `ssh host
// attach` does. Its error is the sentence to show.
type TUISession struct {
	In         io.Reader
	Out        io.Writer
	Env        []string
	Cols, Rows int
	Resize     <-chan [2]int
	Client     *client.Client
	Key        byte
	Taught     func(person string) bool
	Slot       func() (done func(), err error)
}

// Timings, test knobs both: how long a connection has to sign in, and how
// often a signed-in one is asked whether it is still there.
var (
	handshakeWait  = 20 * time.Second
	keepaliveEvery = 30 * time.Second
)

// The limits (Ruling 7): connections still signing in, connections open,
// session channels on one connection, and bytes in one command line.
const (
	maxUnauthed = 32
	maxConns    = 64
	maxChannels = 8
	maxLine     = 4096
)

// More limits. One address may hold only a share of the connections still
// signing in, so a peer that opens them and says nothing cannot shut out
// everybody else. And each attach keeps a copy of a session's screen in the
// daemon, up to about 52 MB, so the server holds at most maxAttaches of
// them, and one connection at most maxAttachesPerConn; Channel.slot is how
// an attach takes its place.
const (
	maxUnauthedPerAddr = 8
	maxAttaches        = 16
	maxAttachesPerConn = 2
)

// One person's share, whatever their role: a quarter of the connections and
// half the attaches, so one key, even a watcher's, cannot hold every place
// and shut out everybody else.
const (
	maxConnsPerUser    = 16
	maxAttachesPerUser = 8
)

// sshdIdleWait is how long a signed-in connection may sit with no command
// running before it is closed: answering keepalives, or holding a channel
// that never asks for anything, is not using a place. A test knob.
var sshdIdleWait = 60 * time.Second

// sshdRefusedEvery is how often a refusal from one address is logged; the
// ones between are counted on the next line. A peer can connect and fail as
// fast as it likes, and the log is not its to fill. A test knob.
var sshdRefusedEvery = time.Minute

// sshdRefusedKept is how many addresses' refusal counts are kept; past it,
// those older than sshdRefusedEvery are forgotten.
const sshdRefusedKept = 1024

// sshdPeer is the address a connection's share of the unauthenticated
// places is counted by: its host, whatever its port, and for IPv6 its /64,
// which is what one host is given. A test knob.
var sshdPeer = func(a net.Addr) string {
	var ip net.IP
	if t, ok := a.(*net.TCPAddr); ok {
		ip = t.IP
	} else if host, _, err := net.SplitHostPort(a.String()); err == nil {
		ip = net.ParseIP(host)
		if ip == nil {
			return host
		}
	} else {
		return a.String()
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// sshdKeepalives is how many keepalive intervals may pass with one
// unanswered before the connection is closed.
const sshdKeepalives = 3

// sshdOffered is how many refused keys' fingerprints a refusal names.
const sshdOffered = 3

// sshdKeyExchanges are the key exchanges the server takes: elliptic curves
// and ML-KEM, and none of the finite-field Diffie-Hellman groups, which cost
// the server the most for a peer that has not signed in.
var sshdKeyExchanges = []string{
	ssh.KeyExchangeMLKEM768X25519,
	ssh.KeyExchangeCurve25519, "curve25519-sha256@libssh.org",
	ssh.KeyExchangeECDHP256, ssh.KeyExchangeECDHP384, ssh.KeyExchangeECDHP521,
}

// sshdMACs are the MACs a cipher that is not an AEAD may use: SHA-2 alone,
// never SHA-1 and never one cut to 96 bits.
var sshdMACs = []string{ssh.HMACSHA256ETM, ssh.HMACSHA512ETM, ssh.HMACSHA256, ssh.HMACSHA512}

// sshdDefaultKey is Ctrl-\, the chord's key when Options names none.
const sshdDefaultKey = 0x1C

// Server is one SSH listener and the connections it has taken.
type Server struct {
	o        Options
	log      *slog.Logger
	ln       net.Listener
	key      ssh.Signer
	fp       string
	known    map[string]int     // a key's wire form → its person's index in o.Users
	commands map[string]command // commands when it started
	wait     time.Duration      // handshakeWait when it started
	every    time.Duration      // keepaliveEvery when it started
	idle     time.Duration      // sshdIdleWait when it started
	refusing time.Duration      // sshdRefusedEvery when it started
	peer     func(net.Addr) string
	ctx      context.Context
	cancel   context.CancelFunc

	mu       sync.Mutex
	closed   bool
	conns    map[net.Conn]struct{}
	unauthed int
	peers    map[string]int // unauthenticated connections by sshdPeer
	people   map[string]int // signed-in connections by person
	attached int
	attachAs map[string]int // attaches by person
	refused  map[string]*sshdRefusal
	wg       sync.WaitGroup
}

// sshdRefusal is one address's refusals since its last log line.
type sshdRefusal struct {
	at         time.Time
	suppressed int
}

// sshdConn is one signed-in connection, as its channels need it.
type sshdConn struct {
	s       *Server
	sc      *ssh.ServerConn
	user    User
	login   string
	handler http.Handler

	mu       sync.Mutex
	channels int // open
	running  int // running a command
	attached int
	ended    bool
	idle     *time.Timer
}

// Listen checks every person's keys, loads or makes the host key, and only
// then binds, so a refused start leaves nothing listening. The commands are
// the ones registered when it is called.
func Listen(o Options) (*Server, error) {
	known, err := keysParse(o.Users)
	if err != nil {
		return nil, err
	}
	key, err := hostkeyLoad(o.HostKey, o.NewHostKey)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return nil, err
	}
	o.Users = slices.Clone(o.Users)
	if o.Key == 0 {
		o.Key = sshdDefaultKey
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		o: o, log: log, ln: ln, key: key, fp: ssh.FingerprintSHA256(key.PublicKey()),
		known: known, commands: maps.Clone(commands),
		wait: handshakeWait, every: keepaliveEvery, idle: sshdIdleWait, refusing: sshdRefusedEvery,
		peer: sshdPeer, ctx: ctx, cancel: cancel,
		conns: map[net.Conn]struct{}{}, peers: map[string]int{}, people: map[string]int{},
		attachAs: map[string]int{}, refused: map[string]*sshdRefusal{},
	}, nil
}

// Addr is the address the server listens on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Fingerprint is the host key's SHA256 fingerprint, as ssh prints it.
func (s *Server) Fingerprint() string { return s.fp }

// Serve takes connections until Close, and then returns nil. A connection
// past maxConns open, past maxUnauthed still signing in, or past
// maxUnauthedPerAddr from its address, is closed as soon as it is taken,
// before a byte is written to it.
func (s *Server) Serve() error {
	var delay time.Duration
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			if s.sshdClosed() {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Out of file descriptors, say: wait, rather than spin.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			time.Sleep(delay)
			continue
		}
		delay = 0
		peer := s.peer(nc.RemoteAddr())
		s.mu.Lock()
		if s.closed || len(s.conns) >= maxConns || s.unauthed >= maxUnauthed || s.peers[peer] >= maxUnauthedPerAddr {
			s.mu.Unlock()
			nc.Close()
			continue
		}
		s.conns[nc] = struct{}{}
		s.unauthed++
		s.peers[peer]++
		s.wg.Add(1)
		s.mu.Unlock()
		go s.sshdServe(nc, peer)
	}
}

// Close stops listening, closes every connection, and returns once every
// connection's goroutines have. A command is told by its context.
func (s *Server) Close() error {
	s.mu.Lock()
	var err error
	if !s.closed {
		s.closed = true
		err = s.ln.Close()
		for nc := range s.conns {
			nc.Close()
		}
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return err
}

func (s *Server) sshdClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// sshdRecover, deferred directly, turns a panic into one log line with its
// stack and the end of the connection end closes; the daemon carries on.
func (s *Server) sshdRecover(end func()) {
	v := recover()
	if v == nil {
		return
	}
	s.log.Error("ssh panic", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
	if end != nil {
		end()
	}
}

// sshdRefused logs a refusal from peer, at most once every s.refusing for
// each peer; the line says how many were not logged since the last one.
func (s *Server) sshdRefused(peer string, args ...any) {
	now := time.Now()
	s.mu.Lock()
	r := s.refused[peer]
	if r != nil && now.Sub(r.at) < s.refusing {
		r.suppressed++
		s.mu.Unlock()
		return
	}
	suppressed := 0
	if r != nil {
		suppressed = r.suppressed
	}
	if len(s.refused) >= sshdRefusedKept {
		for p, old := range s.refused {
			if now.Sub(old.at) >= s.refusing {
				delete(s.refused, p)
			}
		}
	}
	if r != nil || len(s.refused) < sshdRefusedKept {
		s.refused[peer] = &sshdRefusal{at: now}
	}
	s.mu.Unlock()
	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	s.log.Info("ssh refused", args...)
}

// sshdConfig is the server's configuration for one connection: offered
// gathers the fingerprints of the keys it refused. The person and the key
// that signed in are carried only in the Permissions the accepting callback
// returns, which the library hands back for the key whose signature it
// checked.
func (s *Server) sshdConfig(offered *[]string) *ssh.ServerConfig {
	c := &ssh.ServerConfig{
		Config:                  ssh.Config{KeyExchanges: sshdKeyExchanges, MACs: sshdMACs},
		MaxAuthTries:            3,
		ServerVersion:           "SSH-2.0-rota",
		PublicKeyAuthAlgorithms: keysAlgorithms,
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			fp := ssh.FingerprintSHA256(k)
			if ui, ok := s.known[string(k.Marshal())]; ok {
				return &ssh.Permissions{Extensions: map[string]string{
					"rota-user": s.o.Users[ui].Name, "rota-fp": fp,
				}}, nil
			}
			if len(*offered) < sshdOffered && !slices.Contains(*offered, fp) {
				*offered = append(*offered, fp)
			}
			return nil, errors.New("no")
		},
	}
	c.AddHostKey(s.key)
	return c
}

// sshdServe is one connection, from the handshake to its end.
func (s *Server) sshdServe(nc net.Conn, peer string) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, nc)
		s.mu.Unlock()
		nc.Close()
	}()
	// The place it holds among those signing in is given back once, after
	// the handshake, or by this if the handshake panics.
	var signedIn sync.Once
	giveBack := func() {
		signedIn.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.unauthed--
			if s.peers[peer]--; s.peers[peer] <= 0 {
				delete(s.peers, peer)
			}
		})
	}
	defer giveBack()
	defer s.sshdRecover(nil)
	addr := nc.RemoteAddr().String()
	var offered []string
	_ = nc.SetDeadline(time.Now().Add(s.wait))
	sc, chans, reqs, err := ssh.NewServerConn(nc, s.sshdConfig(&offered))
	giveBack()
	if err != nil {
		if !s.sshdClosed() {
			s.sshdRefused(peer, "addr", addr, "fingerprints", offered)
		}
		return
	}
	defer sc.Close()
	_ = nc.SetDeadline(time.Time{})

	ext := sc.Permissions.Extensions
	u := s.sshdUser(ext["rota-user"])
	s.mu.Lock()
	if s.people[u.Name] >= maxConnsPerUser {
		s.mu.Unlock()
		s.sshdRefused(peer, "addr", addr, "name", u.Name, "fingerprint", ext["rota-fp"],
			"reason", fmt.Sprintf("too many connections (%d)", maxConnsPerUser))
		return
	}
	s.people[u.Name]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.people[u.Name]--; s.people[u.Name] <= 0 {
			delete(s.people, u.Name)
		}
	}()

	c := &sshdConn{s: s, sc: sc, user: u, login: sc.User()}
	c.idle = time.AfterFunc(s.idle, func() { sc.Close() })
	ctx, cancel := context.WithCancel(s.ctx)
	var wg sync.WaitGroup
	// Registered before anything that may panic, so a panic still waits for
	// this connection's goroutines before the server counts it gone.
	defer func() {
		cancel()
		sc.Close()
		c.mu.Lock()
		c.ended = true
		c.idle.Stop()
		c.mu.Unlock()
		wg.Wait()
		s.log.Info("ssh signed out", "name", u.Name)
	}()
	s.log.Info("ssh signed in", "name", u.Name, "login", sshdLogin(c.login), "fingerprint", ext["rota-fp"], "addr", addr)
	if s.o.Handler != nil {
		c.handler = s.o.Handler(&api.Principal{Name: u.Name, Role: u.Role, Via: "user"})
	}
	end := func() { sc.Close() }
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer s.sshdRecover(end)
		// The client's own keepalives are answered; anything else it asks of
		// the connection, forwarding first of all, is refused.
		for r := range reqs {
			if r.WantReply {
				_ = r.Reply(r.Type == "keepalive@openssh.com", nil)
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer s.sshdRecover(end)
		s.sshdKeepalive(ctx, sc, &wg)
	}()
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.Prohibited, "rota opens session channels alone")
			continue
		}
		if !c.sshdOpen() {
			_ = nch.Reject(ssh.ResourceShortage, fmt.Sprintf("rota opens at most %d sessions on one connection", maxChannels))
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			c.sshdShut()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.sshdShut()
			defer s.sshdRecover(end)
			channelServe(ctx, c, ch, creqs)
		}()
	}
}

// sshdOpen counts a channel open, if there is room for it.
func (c *sshdConn) sshdOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channels >= maxChannels {
		return false
	}
	c.channels++
	return true
}

// sshdShut counts a channel closed.
func (c *sshdConn) sshdShut() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channels--
}

// sshdBusy counts a command started, which stops the idle clock.
func (c *sshdConn) sshdBusy() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running++
	c.idle.Stop()
}

// sshdRest counts a command ended, and starts the idle clock when it was
// the last running, unless the connection has ended meanwhile.
func (c *sshdConn) sshdRest() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running--; c.running == 0 && !c.ended {
		c.idle.Reset(c.s.idle)
	}
}

// sshdSlot takes one attach's place, on the connection, for the person and
// on the server, and gives back the func that returns it, once however
// often it is called.
func (c *sshdConn) sshdSlot() (func(), error) {
	s, name := c.s, c.user.Name
	s.mu.Lock()
	defer s.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.attached >= maxAttachesPerConn:
		return nil, fmt.Errorf("rota over ssh has %d sessions attached on this connection; leave one first", maxAttachesPerConn)
	case s.attachAs[name] >= maxAttachesPerUser:
		return nil, fmt.Errorf("rota over ssh has %d sessions attached as %s; leave one first", maxAttachesPerUser, name)
	case s.attached >= maxAttaches:
		return nil, fmt.Errorf("rota over ssh has %d sessions attached; leave one first", maxAttaches)
	}
	s.attached++
	s.attachAs[name]++
	c.attached++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			c.mu.Lock()
			defer c.mu.Unlock()
			s.attached--
			if s.attachAs[name]--; s.attachAs[name] <= 0 {
				delete(s.attachAs, name)
			}
			c.attached--
		})
	}, nil
}

// sshdUser is the person a sign-in named.
func (s *Server) sshdUser(name string) User {
	for _, u := range s.o.Users {
		if u.Name == name {
			return u
		}
	}
	return User{Name: name, Role: api.RoleWatch}
}

// sshdKeepalive asks the client every s.every whether it is there, one
// question at a time, and closes the connection once sshdKeepalives
// intervals have passed with a question unanswered. A refusal is an answer:
// the client is there.
func (s *Server) sshdKeepalive(ctx context.Context, sc *ssh.ServerConn, wg *sync.WaitGroup) {
	t := time.NewTicker(s.every)
	defer t.Stop()
	answered := make(chan struct{}, 1)
	asking, missed := false, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-answered:
			asking, missed = false, 0
		case <-t.C:
			if !asking {
				asking = true
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer s.sshdRecover(func() { sc.Close() })
					if _, _, err := sc.SendRequest("keepalive@openssh.com", true, nil); err == nil {
						answered <- struct{}{}
					}
				}()
				continue
			}
			if missed++; missed >= sshdKeepalives {
				sc.Close()
				return
			}
		}
	}
}

// sshdLogin is the login name as the log gives it: what the client typed,
// cut short at a character's edge. It decides nothing, so it is never worth
// a long line.
func sshdLogin(login string) string {
	const most = 64
	if len(login) <= most {
		return login
	}
	cut := most
	for cut > 0 && !utf8.RuneStart(login[cut]) {
		cut--
	}
	return login[:cut] + "…"
}
