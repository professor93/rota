package sshd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/attach"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/vtscan"
)

// `attach <session>` and `watch <session>`: the person's terminal, at the
// other end of the channel, on a session the daemon holds or lists, as
// rota run --attach puts this machine's terminal on one. What the session
// draws reaches their screen, at the session's own size: a smaller window
// is shown a crop, and nothing but the chord's z ever resizes the session.
// What they type reaches it, unless they only watch: watch, or attach by a
// sign-in that may only watch, drops every key here but the chord's. Ctrl-\
// then d leaves it running.

// The refusals attach and watch answer with, each said with its own word.
const (
	attachTakesOne    = "%[1]s takes one session: %[1]s <session>, by its name, label, folder or id\n"
	attachOffTerminal = "%[1]s needs a terminal: ssh -t <host> %[1]s <session>\n"
	attachHasEnded    = "session %s has ended\n"
)

// attachLoop is attach.Run, and a variable only so that a test can panic in
// its place, which nothing outside can make the attach do.
var attachLoop = attach.Run

// attachRun is `attach <session>`: look and type.
func attachRun(ctx context.Context, ch *Channel, args []string) int {
	return attachCommand(ctx, ch, args, "attach")
}

// attachWatch is `watch <session>`: look only.
func attachWatch(ctx context.Context, ch *Channel, args []string) int {
	return attachCommand(ctx, ch, args, "watch")
}

// attachCommand is attach or watch, by word. It takes one session and a
// terminal, each a usage error without, and finds the session by
// client.Match's rule among those the daemon lists: one there is none of,
// or that has ended, is exit 1. Only then does it take an attach's place,
// which it gives back when the attach ends.
func attachCommand(ctx context.Context, ch *Channel, args []string, word string) int {
	if len(args) != 1 {
		dispatchText(ch.Err, ch.PTY, fmt.Sprintf(attachTakesOne, word)+usage)
		return 2
	}
	if !ch.PTY {
		dispatchText(ch.Err, ch.PTY, fmt.Sprintf(attachOffTerminal, word))
		return 2
	}
	c, stop := clientFor(ctx, ch)
	defer stop()
	all, err := c.List()
	if err != nil {
		listFailed(ch, err)
		return 1
	}
	s, err := attachFind(all, args[0])
	if err != nil {
		listFailed(ch, err)
		return 1
	}
	if s.Ended != nil {
		dispatchText(ch.Err, ch.PTY, fmt.Sprintf(attachHasEnded, attach.OneLine(listName(s))))
		return 1
	}
	done, ok := attachPlace(ch)
	if !ok {
		return 1
	}
	defer done()
	return attachTo(ctx, ch, c, s.ID, word)
}

// attachFind is the session word means among all, by client.Match's rule: an
// id or the start of one, then the label, name or folder of a live session.
// A terminal the server runs itself says no state, and is live until it has
// ended, as list says it is running; Match is shown it so, and so finds it
// by its label as it finds a held one.
func attachFind(all []client.Session, word string) (client.Session, error) {
	live := slices.Clone(all)
	for i := range live {
		if live[i].State == "" && live[i].Ended == nil {
			live[i].State = "live"
		}
	}
	return client.Match(live, word)
}

// attachPlace takes one of the places an attach needs (Channel.slot), or
// says on stderr why there is none.
func attachPlace(ch *Channel) (done func(), ok bool) {
	done, err := ch.slot()
	if err != nil {
		listFailed(ch, err)
		return nil, false
	}
	return done, true
}

// attachTo puts the channel's terminal on the session id of c's daemon
// until the person leaves, the session ends, or the attach is lost, which
// it is when ctx, the command's, ends with the channel or the connection.
// word, attach or watch, is what the person typed, and what the last line
// says to type to come back. A watch, or any attach by a sign-in that is not
// control, types nothing: its keys are dropped here, and the daemon is
// asked for a watcher's place.
//
// It reads what is typed through the channel's one reader, made here: a
// channel runs one command, so that is one for the channel's life. On every
// end the terminal is put back, the cursor on its last row, and the line
// saying why goes below it on stderr; the exit is the attach's. A panic
// still puts the terminal back before it ends the connection.
func attachTo(ctx context.Context, ch *Channel, c *client.Client, id, word string) int {
	in := inputPump(ch.In)
	defer in.stop()
	t := attachChan(ch, in)
	defer t.stop()
	defer attachRescue(t)
	o := attach.Options{
		Key:   ch.Key,
		Watch: word == "watch" || ch.User.Role != api.RoleControl,
		Teach: ch.Taught,
		Again: func(w string) string { return word + " " + w },
	}
	res := attachLoop(ctx, t, c, id, o)
	if res.Session.Name == "" {
		// A terminal the server runs itself has no name: it is called what
		// list calls it, which attach finds it by.
		res.Session.Name = listName(res.Session)
	}
	_, _ = io.WriteString(ch.Out, "\r\n")
	dispatchText(ch.Err, ch.PTY, res.Line(o)+"\n")
	return res.ExitCode()
}

// attachRescue, deferred while an attach has the channel's terminal, puts
// it back from a panic before the panic goes on to end the connection:
// every mode the session set off, the pen and the scroll region reset, and
// the cursor below the window's last row. The person's own ssh gives their
// terminal its settings back as it goes.
func attachRescue(t *chanTerm) {
	p := recover()
	if p == nil {
		return
	}
	_, rows := t.Size()
	_, _ = t.Write(append(vtscan.ResetAll(), "\x1b[0m\x1b[r\x1b["+strconv.Itoa(max(rows, 1))+";1H\r\n"...))
	panic(p)
}

// chanTerm is a session channel with a pty as an attach's terminal: what the
// person types, from the channel's one reader; their screen, the channel's
// stdout; and their window, the pty's size and then each window-change.
type chanTerm struct {
	in      *input
	out     io.Writer
	resize  <-chan [2]int
	resized chan struct{}
	quit    chan struct{}
	done    chan struct{} // follow has returned
	once    sync.Once

	mu         sync.Mutex
	cols, rows int
}

// attachChan makes ch, read through in, an attach's terminal, which follows
// the channel's window-changes until stop.
func attachChan(ch *Channel, in *input) *chanTerm {
	t := &chanTerm{
		in: in, out: ch.Out, resize: ch.Resize, resized: make(chan struct{}, 1),
		quit: make(chan struct{}), done: make(chan struct{}),
		cols: ch.Cols, rows: ch.Rows,
	}
	go t.follow()
	return t
}

// follow takes each window-change as the window's size, and says it on
// Resized. Two that come before the first is read are said once: Size gives
// the newest.
func (t *chanTerm) follow() {
	defer close(t.done)
	for {
		select {
		case z := <-t.resize:
			t.mu.Lock()
			t.cols, t.rows = z[0], z[1]
			t.mu.Unlock()
			select {
			case t.resized <- struct{}{}:
			default:
			}
		case <-t.quit:
			return
		}
	}
}

// stop stops following the window, and returns once it has. A
// window-change after it waits for nobody.
func (t *chanTerm) stop() {
	t.once.Do(func() { close(t.quit) })
	<-t.done
}

// Input is each chunk the person types, from the channel's one reader.
func (t *chanTerm) Input() <-chan []byte { return t.in.Input() }

// Write is the person's screen.
func (t *chanTerm) Write(p []byte) (int, error) { return t.out.Write(p) }

// Size is the person's window: the pty's size until the first
// window-change, and then the last.
func (t *chanTerm) Size() (cols, rows int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows
}

// Resized fires after a window-change, once Size gives the new size.
func (t *chanTerm) Resized() <-chan struct{} { return t.resized }
