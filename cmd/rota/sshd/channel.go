package sshd

import (
	"context"
	"io"
	"net/http"

	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/internal/holdwire"
)

// Channel is one session channel, as the command it runs sees it.
type Channel struct {
	// In is what the person types; Out and Err are their stdout and stderr.
	In       io.Reader
	Out, Err io.Writer
	// PTY says the client asked for a terminal; Term is its TERM, from the
	// pty-req or an env request, and Cols and Rows its size, held to what a
	// terminal can be. Without a pty they are zero.
	PTY        bool
	Term       string
	Cols, Rows int
	// Resize carries each window-change, columns first. It holds the newest
	// four; an older one nobody read is dropped.
	Resize <-chan [2]int
	// User is the person the key signed in, and Login the name the client
	// typed, which decides nothing (Ruling 5).
	User  User
	Login string
	// Handler is the API as User: Options.Handler of the person, made once
	// per connection.
	Handler http.Handler
	// Key is the chord's key and Taught says whether a person has been shown
	// it, both from Options.
	Key    byte
	Taught func(string) bool
	// slot takes one of the places an attach needs, since each keeps a copy
	// of a session's screen in the daemon: at most maxAttaches on the
	// server, maxAttachesPerUser for one person and maxAttachesPerConn on one
	// connection. Its error is the sentence to print, and done gives the
	// place back. A bare command hands it on as TUISession.Slot.
	slot func() (done func(), err error)
}

// channelResizes is how many window-changes wait for a command to read them.
const channelResizes = 4

// channelServe answers one session channel's requests until the client or
// the connection closes it. It runs one command, on the first shell or exec,
// and when that returns sends its exit-status and closes the channel. A
// request it does not name is refused, and so is anything that would change
// the command once it is running.
func channelServe(ctx context.Context, sc *sshdConn, ch ssh.Channel, reqs <-chan *ssh.Request) {
	ctx, cancel := context.WithCancel(ctx)
	resize := make(chan [2]int, channelResizes)
	c := &Channel{
		In: ch, Out: ch, Err: ch.Stderr(), Resize: resize,
		User: sc.user, Login: sc.login, Handler: sc.handler,
		Key: sc.s.o.Key, Taught: sc.s.o.Taught,
		slot: sc.sshdSlot,
	}
	var done chan struct{}
	// However this loop ends, the client closing the channel, the connection
	// going or a panic here, the command is told and waited for, so Close
	// never returns with one still running.
	defer func() {
		cancel()
		_ = ch.Close()
		if done != nil {
			<-done
		}
	}()
	for req := range reqs {
		ok, run, bare, line := false, false, false, ""
		switch req.Type {
		case "pty-req":
			var p struct {
				Term       string
				Cols, Rows uint32
				W, H       uint32
				Modes      string
			}
			// A TERM that is not a terminal's name leaves the terminal
			// without one; a size of zero is the ordinary 80×24.
			if done == nil && ssh.Unmarshal(req.Payload, &p) == nil {
				ok, c.PTY, c.Term = true, true, ""
				if channelTerm(p.Term) {
					c.Term = p.Term
				}
				c.Cols, c.Rows = channelSize(p.Cols, p.Rows)
			}
		case "window-change":
			// A zero here is no size at all, and is dropped.
			var z struct{ Cols, Rows, W, H uint32 }
			if ssh.Unmarshal(req.Payload, &z) == nil && z.Cols > 0 && z.Rows > 0 {
				cols, rows := channelSize(z.Cols, z.Rows)
				channelResize(resize, [2]int{cols, rows})
				ok = true
			}
		case "env":
			var e struct{ Name, Value string }
			if done == nil && ssh.Unmarshal(req.Payload, &e) == nil && e.Name == "TERM" && channelTerm(e.Value) {
				ok, c.Term = true, e.Value
			}
		case "shell":
			ok, run, bare = done == nil, done == nil, true
		case "exec":
			var x struct{ Command string }
			if done == nil && ssh.Unmarshal(req.Payload, &x) == nil {
				ok, run, line = true, true, x.Command
			}
		}
		if req.WantReply {
			_ = req.Reply(ok, nil)
		}
		if run {
			done = make(chan struct{})
			// A command running is the connection in use: the idle clock
			// stops until it ends.
			sc.sshdBusy()
			go func() {
				defer close(done)
				defer sc.sshdRest()
				// A command that panics ends its connection, not the daemon.
				defer sc.s.sshdRecover(func() { sc.sc.Close() })
				code := dispatchRun(ctx, c, line, bare, sc.s.commands, sc.s.log)
				_ = ch.CloseWrite()
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{channelStatus(code)}))
				_ = ch.Close()
			}()
		}
	}
}

// channelTerm says a TERM is a terminal's name and nothing else: letters,
// digits and . _ + -, at most 64 of them. It goes on to a CLI's environment.
func channelTerm(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	for i := 0; i < len(t); i++ {
		switch c := t[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '+', c == '-':
		default:
			return false
		}
	}
	return true
}

// channelSize holds a size the client sent to one a terminal can be.
func channelSize(cols, rows uint32) (int, int) {
	c, r := holdwire.BoundSize(uint16(min(cols, 0xFFFF)), uint16(min(rows, 0xFFFF)))
	return int(c), int(r)
}

// channelResize sends a size, dropping the oldest waiting when the buffer
// is full. The channel's request loop is the only sender.
func channelResize(r chan [2]int, z [2]int) {
	for {
		select {
		case r <- z:
			return
		default:
		}
		select {
		case <-r:
		default:
		}
	}
}

// channelStatus is a command's code as an exit-status: 0 to 255, with a
// negative code, which is no code at all, as 1.
func channelStatus(code int) uint32 {
	switch {
	case code < 0:
		return 1
	case code > 255:
		return 255
	}
	return uint32(code)
}
