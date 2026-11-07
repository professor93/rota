//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	nethttp "net/http"
	"os"
	"sync"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
)

// ctlOwner is the user the control socket is for: the one this process runs
// as. A variable so a test can make its own connection a stranger's.
var ctlOwner = os.Geteuid

// listenCtl serves srv on the control socket of the store whose terminals
// directory this is, until stop is called. Only the store's daemon listens
// there, so it is refused unless srv holds that store's daemon claim — which
// api.New takes for a server with HoldCommand — and only then is a file left
// at the path removed first: holding the claim, this process knows that no
// other daemon is behind it, and a daemon that crashed leaves one.
//
// The socket is its owner's three times over: the directory around it is
// closed to everybody else first, the socket itself is 0600, and a
// connection from any other user is closed at the door. The last is what
// covers the moment between making the socket and narrowing it, which the
// umask decides.
//
// stop shuts the socket down within serveGrace and takes the file with it.
// What it does not end are the sockets taken over as WebSockets; those are
// the server's to let go of, in srv.Stop.
func listenCtl(srv *api.Server, terminals string, log *slog.Logger) (stop func(), err error) {
	path := holdwire.CtlSock(terminals)
	if err := holdwire.SockFits(path); err != nil {
		return nil, err
	}
	if !srv.HoldsDaemon(terminals) {
		return nil, fmt.Errorf("the control socket %s is the daemon's, and this server does not hold %s",
			path, holdwire.DaemonLock(terminals))
	}
	if err := ownersOnly(terminals, log); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("the file left at %s could not be removed: %w", path, err)
		}
		log.Info("a control socket left behind was removed", "path", path)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("the control socket %s could not be made its owner's alone: %w", path, err)
	}
	server := httpServer(ctlHandler(srv), log)
	// Who the caller is, is asked once per connection and only when it first
	// asks for something: the accept loop never waits on the store.
	server.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		uc, _ := c.(*net.UnixConn)
		return context.WithValue(ctx, ctlMayKey{}, sync.OnceValue(func() bool { return mayControl(uc, terminals, log) }))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := server.Serve(ownerListener{UnixListener: ln, owner: ctlOwner(), log: log})
		if err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
			log.Error("the control socket stopped", "path", path, "err", err)
		}
	}()
	log.Info("control socket listening", "path", path)
	return sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.Background(), serveGrace)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = server.Close()
		<-done
	}), nil
}

// ownersOnly closes the terminals directory to the group and to others if
// somebody opened it. rota makes it 0700, but making a directory that is
// already there changes nothing about it. One this user cannot narrow —
// somebody else's — is refused.
func ownersOnly(terminals string, log *slog.Logger) error {
	fi, err := os.Stat(terminals)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", terminals)
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(terminals, 0o700); err != nil {
		return fmt.Errorf("%s is open to other users (%v) and could not be closed: %w", terminals, fi.Mode().Perm(), err)
	}
	log.Warn("the terminals directory was open to other users, and is closed now", "path", terminals, "was", fi.Mode().Perm().String())
	return nil
}

// ownerListener accepts only connections from the socket's owner, as the
// kernel names their user, and closes any other before it is served.
type ownerListener struct {
	*net.UnixListener
	owner int
	log   *slog.Logger
}

func (l ownerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := proc.PeerUID(c)
		if err == nil && uid == l.owner {
			return c, nil
		}
		l.log.Warn("refused a control socket connection from another user", "uid", uid, "err", err)
		_ = c.Close()
	}
}

// mayControl reports whether the process at the other end of a control
// socket connection may change anything: whether it is outside every held
// session. Its session is the one getsid says its pid is in, and a held
// session's processes are in its holder's session or its CLI's, each the
// leader of one. Any record whose lock is held counts — one this rota cannot
// read included, since its holder is running all the same — and one nobody
// holds is a session that has stopped. A peer that cannot be placed may not,
// and nobody can be placed in a store whose records cannot be listed.
func mayControl(c *net.UnixConn, terminals string, log *slog.Logger) bool {
	if c == nil {
		return false
	}
	pid, err := proc.PeerPID(c)
	if err != nil {
		log.Debug("a control socket caller that cannot be placed may only watch", "err", err)
		return false
	}
	sid, err := sessionOf(pid)
	if err != nil || sid <= 0 {
		log.Debug("a control socket caller that cannot be placed may only watch", "pid", pid, "err", err)
		return false
	}
	entries, err := os.ReadDir(terminals)
	if err != nil {
		log.Warn("the held sessions could not be listed, so a control socket caller may only watch", "pid", pid, "err", err)
		return false
	}
	for _, e := range entries {
		if !e.IsDir() || !holdwire.IsID(e.Name()) {
			continue
		}
		r := holdwire.Judge(terminals, e.Name())
		if r.Locked && r.Meta != nil && (r.Meta.HolderPID == sid || r.Meta.ChildPID == sid) {
			log.Debug("a control socket caller inside a held session may only watch", "pid", pid, "terminal", r.ID)
			return false
		}
	}
	return true
}
