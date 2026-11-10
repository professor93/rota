package client

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
)

// inprocName is the address of a connection in process, and inprocSaid
// what a client of a handler in this process says it is talking to, where
// a daemon's client names its socket.
const (
	inprocName = "in process"
	inprocSaid = "the rota daemon in this process"
)

// InProcess is a client of h itself, with no socket between: every request
// and every attach is a connection of an in-memory pipe that an
// http.Server serving h accepts, so h is reached exactly as the daemon's
// LocalHandler is over ctl.sock, hijacked WebSockets and all. stop closes
// the listener and every connection it accepted, the WebSockets included;
// a second stop does nothing.
func InProcess(h http.Handler) (c *Client, stop func()) {
	ln := &inprocListener{accept: make(chan net.Conn), done: make(chan struct{}), open: map[*inprocConn]struct{}{}}
	// net/http's own lines — a handler's panic, a write on a hijacked
	// connection — would go to this process's stderr, which may be a TUI's
	// screen.
	srv := &http.Server{Handler: h, ReadHeaderTimeout: requestWait, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(ln) }()
	c = dialledBy(inprocSaid, ln.dial)
	var once sync.Once
	return c, func() {
		once.Do(func() {
			_ = srv.Close()
			ln.closeAll()
		})
	}
}

// HTTP is the client's own HTTP client, whose every request reaches what
// this client serves: the daemon over its socket, or a handler in process.
// Its requests are to any host; the path is what counts.
func (c *Client) HTTP() *http.Client { return c.http }

// inprocListener hands the server one end of a pipe per dial, and keeps
// every end it handed over, so that stop can close the ones the server has
// let go of to a WebSocket as well as those it still tracks.
type inprocListener struct {
	accept chan net.Conn
	done   chan struct{}

	mu   sync.Mutex
	shut bool
	open map[*inprocConn]struct{}
}

func (l *inprocListener) dial(ctx context.Context) (net.Conn, error) {
	server, client := net.Pipe()
	sc := &inprocConn{Conn: server, l: l}
	l.mu.Lock()
	if l.shut {
		l.mu.Unlock()
		server.Close()
		client.Close()
		return nil, net.ErrClosed
	}
	l.open[sc] = struct{}{}
	l.mu.Unlock()
	select {
	case l.accept <- sc:
		return client, nil
	case <-l.done:
		sc.Close()
		client.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		sc.Close()
		client.Close()
		return nil, ctx.Err()
	}
}

func (l *inprocListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting. The connections already accepted are closeAll's.
func (l *inprocListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.shut {
		l.shut = true
		close(l.done)
	}
	return nil
}

func (l *inprocListener) Addr() net.Addr { return inprocAddr{} }

// closeAll stops accepting and closes every connection accepted.
func (l *inprocListener) closeAll() {
	l.Close()
	l.mu.Lock()
	open := make([]*inprocConn, 0, len(l.open))
	for c := range l.open {
		open = append(open, c)
	}
	l.mu.Unlock()
	for _, c := range open {
		c.Close()
	}
}

// inprocConn is the server's end of one pipe, which forgets itself when
// it is closed.
type inprocConn struct {
	net.Conn
	l    *inprocListener
	once sync.Once
}

func (c *inprocConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.l.mu.Lock()
		delete(c.l.open, c)
		c.l.mu.Unlock()
	})
	return err
}

type inprocAddr struct{}

func (inprocAddr) Network() string { return "pipe" }
func (inprocAddr) String() string  { return inprocName }
