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
// a key by its fingerprint and never by the key.
package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

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
	// Key is the chord's key, and Taught says whether a person has been
	// shown it; both are handed to each Channel.
	Key    byte
	Taught func(person string) bool
	// TUI, when set, is what a bare `ssh host` runs.
	TUI func(ctx context.Context, s TUISession) error
}

// TUISession is what Options.TUI is given: the person's terminal, its TERM
// (Env holds only TERM=…) and size, its resizes, a client for the API as
// that person, and the chord's key.
type TUISession struct {
	In         io.Reader
	Out        io.Writer
	Env        []string
	Cols, Rows int
	Resize     <-chan [2]int
	Client     *client.Client
	Key        byte
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

// sshdKeepalives is how many keepalive intervals may pass with one
// unanswered before the connection is closed.
const sshdKeepalives = 3

// sshdOffered is how many refused keys' fingerprints a refusal names.
const sshdOffered = 3

// Server is one SSH listener and the connections it has taken.
type Server struct {
	o      Options
	log    *slog.Logger
	ln     net.Listener
	key    ssh.Signer
	fp     string
	known  map[string]int // a key's wire form → its person's index in o.Users
	wait   time.Duration  // handshakeWait when it started
	every  time.Duration  // keepaliveEvery when it started
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	conns    map[net.Conn]struct{}
	unauthed int
	wg       sync.WaitGroup
}

// sshdConn is one signed-in connection, as its channels need it.
type sshdConn struct {
	s       *Server
	user    User
	login   string
	handler http.Handler
}

// Listen checks every person's keys, loads or makes the host key, and only
// then binds, so a refused start leaves nothing listening.
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
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		o: o, log: log, ln: ln, key: key, fp: ssh.FingerprintSHA256(key.PublicKey()),
		known: known, wait: handshakeWait, every: keepaliveEvery,
		ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{},
	}, nil
}

// Addr is the address the server listens on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Fingerprint is the host key's SHA256 fingerprint, as ssh prints it.
func (s *Server) Fingerprint() string { return s.fp }

// Serve takes connections until Close, and then returns nil. A connection
// past maxConns open, or past maxUnauthed still signing in, is closed as
// soon as it is taken, before a byte is written to it.
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
		s.mu.Lock()
		if s.closed || len(s.conns) >= maxConns || s.unauthed >= maxUnauthed {
			s.mu.Unlock()
			nc.Close()
			continue
		}
		s.conns[nc] = struct{}{}
		s.unauthed++
		s.wg.Add(1)
		s.mu.Unlock()
		go s.sshdServe(nc)
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

// sshdConfig is the server's configuration for one connection: offered
// gathers the fingerprints of the keys it refused, and signed is the one
// key that signed in.
func (s *Server) sshdConfig(offered *[]string, signed *string) *ssh.ServerConfig {
	c := &ssh.ServerConfig{
		MaxAuthTries:            3,
		ServerVersion:           "SSH-2.0-rota",
		PublicKeyAuthAlgorithms: keysAlgorithms,
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if ui, ok := s.known[string(k.Marshal())]; ok {
				return &ssh.Permissions{Extensions: map[string]string{"rota-user": s.o.Users[ui].Name}}, nil
			}
			if fp := ssh.FingerprintSHA256(k); len(*offered) < sshdOffered && !slices.Contains(*offered, fp) {
				*offered = append(*offered, fp)
			}
			return nil, errors.New("no")
		},
		VerifiedPublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey, p *ssh.Permissions, _ string) (*ssh.Permissions, error) {
			*signed = ssh.FingerprintSHA256(k)
			return p, nil
		},
	}
	c.AddHostKey(s.key)
	return c
}

// sshdServe is one connection, from the handshake to its end.
func (s *Server) sshdServe(nc net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, nc)
		s.mu.Unlock()
		nc.Close()
	}()
	addr := nc.RemoteAddr().String()
	var offered []string
	var signed string
	_ = nc.SetDeadline(time.Now().Add(s.wait))
	sc, chans, reqs, err := ssh.NewServerConn(nc, s.sshdConfig(&offered, &signed))
	s.mu.Lock()
	s.unauthed--
	closed := s.closed
	s.mu.Unlock()
	if err != nil {
		if !closed {
			s.log.Info("ssh refused", "addr", addr, "fingerprints", offered)
		}
		return
	}
	_ = nc.SetDeadline(time.Time{})

	u := s.sshdUser(sc.Permissions.Extensions["rota-user"])
	c := &sshdConn{s: s, user: u, login: sc.User()}
	s.log.Info("ssh signed in", "name", u.Name, "login", sshdLogin(c.login), "fingerprint", signed, "addr", addr)
	if s.o.Handler != nil {
		c.handler = s.o.Handler(&api.Principal{Name: u.Name, Role: u.Role, Via: "user"})
	}
	ctx, cancel := context.WithCancel(s.ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
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
		s.sshdKeepalive(ctx, sc, &wg)
	}()
	var open atomic.Int32
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.Prohibited, "rota opens session channels alone")
			continue
		}
		if open.Load() >= maxChannels {
			_ = nch.Reject(ssh.ResourceShortage, fmt.Sprintf("rota opens at most %d sessions on one connection", maxChannels))
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			continue
		}
		open.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer open.Add(-1)
			channelServe(ctx, c, ch, creqs)
		}()
	}
	cancel()
	sc.Close()
	wg.Wait()
	s.log.Info("ssh signed out", "name", u.Name)
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
// cut short. It decides nothing, so it is never worth a long line.
func sshdLogin(login string) string {
	const most = 64
	if len(login) > most {
		return login[:most] + "…"
	}
	return login
}
