package api

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/share"
)

// A terminal this server did not start.
//
// The terminals elsewhere in this package are the server's own: it launched
// the CLI, it holds the pseudo-terminal, it is the only thing that can write
// to it. A shared terminal is the other way round. Somebody is sitting at a
// terminal on this machine, running `rota run --share`, and that rota is
// offering the terminal here so it can be watched — or typed into — from the
// page. This server holds a socket, not a process.
//
// Who may offer one is decided by the filesystem and by nothing else. The
// socket is <store>/terminals/share.sock, inside a directory only its owner
// can enter, so the only process that can connect is one run by the user
// this server runs as. That is the right answer for a link between two
// processes of the same program run by the same person: a token would be a
// second secret to leak, protecting something the kernel already protects.
//
// Everything above the backend is reused exactly: the ring, the fan-out, the
// slow reader closed with 1013, the replay, the one-holder keyboard, the
// audit, the recording, max_sessions, the listing. A shared terminal differs
// in three facts, and each is a fact about the far end rather than a rule of
// its own — its size follows a window this server cannot reach, it may be
// offered to be watched and never typed into, and the thing at the far end
// can simply go away.

// shareBeat is how often each end pings, and a third of how long a link may
// go without a frame before it is treated as gone. Any frame refreshes it,
// so a terminal anybody is using never reaches it; it is there for the link
// that is open onto a machine that has stopped answering.
var shareBeat = 30 * time.Second

// shareHandshake bounds the first two frames. A process that connects and
// then says nothing must not hold a goroutine, and it is the one moment in a
// link's life when there is something definite to wait for.
var shareHandshake = 10 * time.Second

/* ------------------------------------------------------------ the socket --- */

// shareSocket is the door local terminals are offered through: the listener,
// where it is, and the goroutine accepting on it.
type shareSocket struct {
	ln   net.Listener
	path string
	once sync.Once
}

// sharePath is where this server listens, given an open store. It is beside
// the recordings rather than beside the runs: what is on the other side of
// it is a terminal, and the directory is already made for its owner alone.
func sharePath(dir string) string { return filepath.Join(dir, share.SockName) }

// listenForShares opens that socket, or says in the log why there is none.
//
// A server without it is a server nobody can share a terminal to, and
// nothing else about it changes — which is why this warns rather than
// refusing to start. The commonest reason by far is a store directory whose
// path is longer than a unix socket address may be, and that is a fact about
// where somebody put their store rather than a mistake in their file.
func (s *Server) listenForShares() {
	if !s.opts.Routes.Terminal || !s.opts.Terminal.sharing() {
		return
	}
	st, err := s.openStore()
	if err != nil {
		s.log.Warn("no terminal can be shared to this server", "err", err)
		return
	}
	dir, err := st.TerminalDir()
	st.Close()
	if err != nil {
		s.log.Warn("no terminal can be shared to this server", "err", err)
		return
	}
	path := sharePath(dir)
	// A socket file left behind by a server that was killed is not a server.
	// Only one rota serves a store at a time, so a file at this path can
	// only be this server's own leftover — and a stale one that stayed would
	// make every sharer refuse to connect for as long as it sat there.
	if stale(path) {
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		s.log.Warn("no terminal can be shared to this server", "path", path, "err", err)
		return
	}
	// Belt and braces over the directory's own 0700: the umask decides what
	// net.Listen leaves behind, and a socket anyone could write to is a
	// terminal anyone could offer.
	if err := os.Chmod(path, 0o600); err != nil {
		s.log.Warn("the share socket could not be made private", "path", path, "err", err)
	}
	sock := &shareSocket{ln: ln, path: path}
	s.share = sock
	s.log.Info("a terminal may be shared to this server", "socket", path)
	go func() {
		<-s.ctx.Done()
		sock.close()
	}()
	go s.acceptShares(sock)
}

// stale reports whether a socket file has nothing behind it. Dialing is the
// only honest test: a file is there either way, and a live one belongs to a
// server that is still running.
func stale(path string) bool {
	if _, err := os.Lstat(path); err != nil {
		return false
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return true
	}
	c.Close()
	return false
}

func (k *shareSocket) close() {
	k.once.Do(func() {
		_ = k.ln.Close()
		_ = os.Remove(k.path)
	})
}

func (s *Server) acceptShares(sock *shareSocket) {
	for {
		conn, err := sock.ln.Accept()
		if err != nil {
			return // the listener was closed, which is how this ends
		}
		go s.takeShare(conn)
	}
}

/* --------------------------------------------------------- the handshake --- */

// takeShare reads one hello and either starts a terminal for it or says, in
// a sentence, why not. Either way this connection is finished with here: a
// terminal that was taken is carried by its session from now on.
func (s *Server) takeShare(conn net.Conn) {
	c := share.NewConn(conn, shareBeat)
	c.Deadline(time.Now().Add(shareHandshake))
	var hello share.HelloMsg
	kind, err := share.ReadJSON(c, &hello)
	if err != nil || kind != share.Hello {
		// Not something speaking this protocol. Nothing is said back: the
		// only thing that can reach this socket is another rota, and a rota
		// that said this was one that is broken rather than one to inform.
		c.Close()
		return
	}
	if why, final := s.mayShare(&hello); why != "" {
		_ = c.SendJSON(share.Refused, share.RefusedMsg{Message: why, Final: final})
		c.Close()
		return
	}
	ts := s.newShared(c, &hello)
	if err := c.SendJSON(share.Welcome, share.WelcomeMsg{Version: share.Version, Terminal: ts.id}); err != nil {
		s.dropTerminal(ts.id)
		c.Close()
		return
	}
	c.Clear()
	s.addTerminal(ts)
	ts.audit("terminal shared", "account", hello.Account, "label", ts.name,
		"pid", hello.PID, "mode", ts.shareMode())
	go ts.run()
}

// mayShare is the two things that can refuse a hello, in the words the
// sharer prints. The second is worth coming back for and the first is not.
func (s *Server) mayShare(hello *share.HelloMsg) (why string, final bool) {
	if hello.Version != share.Version {
		return fmt.Sprintf(
			"this rota serves version %d of the sharing protocol and yours speaks %d; the two are the same program, so upgrade whichever is older",
			share.Version, hello.Version), true
	}
	if n := s.liveTerminals(); n >= s.opts.Terminal.MaxSessions {
		return fmt.Sprintf(
			"this server is holding %d terminals, which is terminal.max_sessions; end one and this terminal will be shared on the next try", n), false
	}
	return "", false
}

// newShared builds the session for one hello. Nothing about it is registered
// yet: the welcome has to reach the sharer first, because a terminal listed
// on a link that turned out to be broken is a terminal nobody can end.
func (s *Server) newShared(c *share.Conn, hello *share.HelloMsg) *termSession {
	mode := share.ModeControl
	if hello.Mode == share.ModeWatch {
		mode = share.ModeWatch
	}
	cols, rows := boundSize(hello.Cols, hello.Rows)
	back := &sharedTerm{conn: c, mode: mode, pid: hello.PID}
	ts := &termSession{
		s: s, id: runID(), log: s.log.Info,
		kind: termKindShared, account: hello.Account, label: hello.AccountLabel, provider: hello.Provider,
		name: strings.TrimSpace(hello.Label), cwd: hello.Cwd, started: s.now(),
		back: back, lookOnly: mode == share.ModeWatch,
		cols: cols, rows: rows, ring: newOutRing(s.opts.Terminal.Scrollback),
	}
	ts.idleAt = ts.started
	back.ts = ts
	s.recordTerminal(ts)
	return ts
}

// shareMode is "control" or "watch", for the audit line and the listing.
func (ts *termSession) shareMode() string {
	if sh, ok := ts.back.(*sharedTerm); ok {
		return sh.mode
	}
	return ""
}

/* ------------------------------------------------- the far end on a link --- */

// sharedTerm is a terminal whose other end is a process this server did not
// start and cannot signal: everything it can do to it, it does by writing a
// frame down a socket and hoping the rota at the other end is still there.
type sharedTerm struct {
	conn *share.Conn
	mode string
	pid  int
	ts   *termSession

	mu sync.Mutex
	// lost is the link having broken without an exit frame. It is the one
	// ending this server cannot tell anything else about, and a description
	// that did not say so would read as an ordinary failed command.
	lost bool
}

// typeInto sends what somebody on the page typed to the machine the terminal
// is on. In watch mode it never does: the sharer said the terminal was to be
// read, and that is decided here as well as there, so a server that had been
// tampered with would still have to get it past the sharer.
func (sh *sharedTerm) typeInto(p []byte) error {
	if sh.mode == share.ModeWatch {
		return errSharedWatch
	}
	if err := sh.conn.Send(share.Input, p); err != nil {
		if errors.Is(err, share.ErrClosed) {
			return errTermEnded
		}
		return err
	}
	return nil
}

// resize refuses, always. The terminal's size is the size of a window
// somebody is sitting at, and nothing on this server can reach across and
// drag it.
func (sh *sharedTerm) resize(uint16, uint16) error { return errSharedSize }

// hangUp asks the sharer to hang up the CLI it is holding, which is what
// DELETE on a shared terminal means: this server has no process to signal.
func (sh *sharedTerm) hangUp() { _ = sh.conn.Send(share.Kill, nil) }

// end is all this server can insist with: letting go of the link. Whatever
// is running is on somebody else's machine, and killing it is the sharer's
// to do — which it does, three seconds after the frame above.
func (sh *sharedTerm) end() { _ = sh.conn.Close() }

func (sh *sharedTerm) close() { _ = sh.conn.Close() }

// describe says what only a shared terminal has: the rota answering for it,
// whether it may be typed into, and whether that rota is still there.
func (sh *sharedTerm) describe(doc map[string]any) {
	doc["mode"] = sh.mode
	if sh.pid != 0 {
		doc["pid"] = sh.pid
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.lost {
		doc["lost"] = true
		doc["ending"] = lostSharer
	}
}

// lostSharer is how a terminal whose link broke is described. It is a
// sentence rather than a code because there is no code: nothing exited, the
// rota holding the terminal stopped answering.
const lostSharer = "the sharing process went away"

// run reads the link until it ends, which is the whole life of this
// terminal as far as this server is concerned.
//
// Two things end it, and they are told apart on purpose. An exit frame is
// the CLI having exited, with its code. The link closing without one is the
// rota that was holding the terminal having gone — killed, or its machine
// having gone to sleep, or the socket having broken — and there is no code
// to report for it, so it reports the one that means "no answer" and says so
// in the description.
func (sh *sharedTerm) run(sink termSink) int {
	beat := time.NewTicker(shareBeat)
	defer beat.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-beat.C:
				_ = sh.conn.Send(share.Ping, nil)
			}
		}
	}()
	for {
		kind, payload, err := sh.conn.Read()
		if err != nil {
			sh.mu.Lock()
			sh.lost = true
			sh.mu.Unlock()
			return -1
		}
		switch kind {
		case share.Output:
			if len(payload) > 0 {
				sink.output(payload)
			}
		case share.Resized:
			var msg share.ResizedMsg
			if share.Unmarshal(payload, &msg) == nil {
				sink.resizedBy(msg.Cols, msg.Rows)
			}
		case share.Gap:
			var msg share.GapMsg
			if share.Unmarshal(payload, &msg) == nil {
				sink.gapOf(msg.Bytes)
			}
		case share.Exit:
			var msg share.ExitMsg
			if share.Unmarshal(payload, &msg) != nil {
				return -1
			}
			return msg.Code
		case share.Ping:
			_ = sh.conn.Send(share.Pong, nil)
		case share.Pong:
			// Nothing to do: the read itself is what the ping was for.
		}
	}
}
