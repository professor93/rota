package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/pty"
	"github.com/professor93/rota/internal/share"
	"github.com/professor93/rota/store"
)

// Sharing the terminal you are sitting at.
//
// `rota run <id>` with no prompt hands this terminal to the account's CLI by
// replacing rota with it: after the exec there is no rota left, which is the
// right answer when nothing else wants the terminal. --share is for when
// something does. rota stays as the parent, runs the CLI on a pseudo-terminal
// of its own, copies the terminal both ways, and offers the same bytes to a
// rota server on this machine so the terminal can be watched — or typed into
// — from the page.
//
// One rule decides every design question here: the person sitting at this
// terminal must never be able to tell that it is being shared. Their
// keystrokes go straight into the CLI and are never carried anywhere near
// the link. What the CLI prints is written to their screen first and handed
// to the link afterwards, into a queue that drops rather than waits. A
// server that is slow, or that went away, or that was never there, costs
// them nothing but the bytes the server misses — which are counted and
// announced, because a watcher shown a terminal that looked continuous when
// it was not is worse off than one shown a hole.

// The refusals --share answers with. They are constants because they are
// what rota says and a test reads them.
const (
	shareNoTerm     = "--share needs a terminal to share"
	shareWithPrompt = "--share opens the account's CLI, so it takes no prompt: " +
		"leave the prompt out, or run the prompt without --share"
	shareBadMode = `--share is --share, --share=control or --share=watch`
)

// The queue between the local terminal and the link, and how long the two
// waits it needs are. They are variables so a test about what happens after
// them need not take that long.
var (
	// shareQueueMax is how many bytes of output may be waiting for the
	// server. Four mebibytes is several screens of a full-screen program
	// redrawing itself: enough that an ordinary stall costs nothing, small
	// enough that a server that has stopped reading cannot grow this
	// process without bound.
	shareQueueMax = 4 << 20
	// shareRetry is how often a link that is not there is tried again. It
	// is quiet: nothing is printed for a server that is simply not running,
	// because sharing is an extra and not a condition for running.
	shareRetry = 3 * time.Second
	// shareBeat is how often each end pings a link that is otherwise idle.
	shareBeat = 30 * time.Second
	// shareFlush is how long the exit frame is given to reach the server
	// before rota stops waiting for it and exits anyway.
	shareFlush = 2 * time.Second
	// shareHandshake bounds the hello and the answer to it, which happen
	// before the CLI takes the screen.
	shareHandshake = 5 * time.Second
)

// shareRead is one read of the CLI's terminal, the same size the server uses
// for the same job: a burst of output rather than a line.
const shareRead = 32 << 10

// shareMaxPath is the longest socket path both kernels take. Linux allows
// 108 bytes and the BSDs 104, and rather than being right per platform this
// is under both: what it guards is a message, and a message that is a little
// cautious is better than one that is wrong on one of the two.
const shareMaxPath = 100

/* ------------------------------------------------------- what was asked for --- */

// shareAsk is what --share asked for: whether at all, whether the page may
// type, and what to call the terminal.
type shareAsk struct {
	on    bool
	mode  string
	label string
}

// takeShareFlags lifts --share and --label out of what was typed, and hands
// back the rest untouched.
//
// It is done here rather than in a flag set because the arguments around it
// are not rota's: `rota run 2 -- --share` is a CLI's own flag and must
// survive, and `rota run 2 -i` never reaches a flag set at all. Only what
// comes before -- is looked at, for the same reason.
func takeShareFlags(args []string) ([]string, shareAsk, error) {
	var ask shareAsk
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(out, args[i:]...), ask, nil
		}
		name, value, hasValue := strings.Cut(a, "=")
		switch strings.TrimLeft(name, "-") {
		case "share":
			if !strings.HasPrefix(name, "-") {
				break
			}
			ask.on = true
			ask.mode = share.ModeControl
			switch {
			case !hasValue:
			case value == share.ModeControl || value == share.ModeWatch:
				ask.mode = value
			default:
				return nil, ask, usageErr("%s", shareBadMode)
			}
			continue
		case "label":
			if !strings.HasPrefix(name, "-") {
				break
			}
			if hasValue {
				ask.label = value
				continue
			}
			if i+1 >= len(args) {
				return nil, ask, usageErr("--label takes the name to give this terminal")
			}
			i++
			ask.label = args[i]
			continue
		}
		out = append(out, a)
	}
	return out, ask, nil
}

// sharePlan is everything --share needs once the account is ready: what to
// run, as whom, where, and what to tell a server about it. It is one struct
// because the platform that cannot do any of this still has to compile the
// signature.
type sharePlan struct {
	// who is the account as it prints, and bin the CLI's own name — the two
	// halves of the line every handover opens with.
	who  string
	bin  string
	path string
	args []string
	env  []string
	cwd  string
	// sock is where a server on this machine would be listening.
	sock string
	ask  shareAsk

	account  int
	label    string
	provider string
}

/* ------------------------------------------------------------- the queue --- */

// shareQueue is everything the CLI has printed that the server has not been
// given yet.
//
// It is bounded and it drops, which is the whole of how the local terminal is
// kept independent of the link. Pushing is a copy and a lock held for the
// length of an append; nothing in it can block, and nothing in it waits for
// a socket. When it is full the oldest bytes go — the newest are the ones
// somebody watching wants — and what went is counted, so the link can say so
// the moment it is able to say anything at all.
type shareQueue struct {
	mu   sync.Mutex
	wake chan struct{}
	bits [][]byte
	held int
	max  int
	// lost is what has been dropped and not yet announced, which take
	// clears as it hands it over. gone is the same number never cleared:
	// how much this server has missed altogether, which is a question with
	// an answer whether or not a link happens to be draining right now.
	lost int64
	gone int64
	// exit is the code the CLI ended with, put here rather than sent
	// directly so that it cannot overtake the output it comes after.
	exit *int
}

func newShareQueue(max int) *shareQueue {
	if max <= 0 {
		max = shareQueueMax
	}
	return &shareQueue{wake: make(chan struct{}, 1), max: max}
}

// push takes a copy of what was printed. The caller's buffer is its own
// again the moment this returns, which is what lets the output loop read
// straight back into it.
func (q *shareQueue) push(p []byte) {
	if len(p) == 0 {
		return
	}
	chunk := make([]byte, len(p))
	copy(chunk, p)
	q.mu.Lock()
	q.bits = append(q.bits, chunk)
	q.held += len(chunk)
	// The oldest go first, and everything still queued is therefore newer
	// than everything that was dropped: the gap always belongs in front of
	// what is about to be sent, which is why one number is enough to say it.
	for q.held > q.max && len(q.bits) > 0 {
		q.lost += int64(len(q.bits[0]))
		q.gone += int64(len(q.bits[0]))
		q.held -= len(q.bits[0])
		q.bits = q.bits[1:]
	}
	q.mu.Unlock()
	q.nudge()
}

// done says the CLI has ended. The code travels behind the output rather
// than beside it.
func (q *shareQueue) done(code int) {
	q.mu.Lock()
	q.exit = &code
	q.mu.Unlock()
	q.nudge()
}

func (q *shareQueue) nudge() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// missed is how much the server has missed altogether and how much of that
// is still waiting to be announced to it, for a test and for nothing else.
// The two differ because take clears the second as it hands it over, and a
// link that happened to pick the count up a moment ago has not made the
// bytes come back.
func (q *shareQueue) missed() (total, owed int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.gone, q.lost
}

// take hands over everything waiting, and what was lost in front of it. It
// blocks until there is something or until stop is closed.
func (q *shareQueue) take(stop <-chan struct{}) (bits [][]byte, lost int64, exit *int, ok bool) {
	for {
		q.mu.Lock()
		if len(q.bits) > 0 || q.lost > 0 {
			bits, lost = q.bits, q.lost
			q.bits, q.held, q.lost = nil, 0, 0
			q.mu.Unlock()
			return bits, lost, nil, true
		}
		if q.exit != nil {
			exit = q.exit
			q.mu.Unlock()
			return nil, 0, exit, true
		}
		q.mu.Unlock()
		select {
		case <-q.wake:
		case <-stop:
			return nil, 0, nil, false
		}
	}
}

/* -------------------------------------------------- the two copying loops --- */

// shareTyped carries what is typed here into the CLI. Nothing about the
// server appears in it: a keystroke's whole journey is one read and one
// write, and no lock, queue or socket is on the way.
func shareTyped(from io.Reader, to io.Writer) {
	buf := make([]byte, 4096)
	for {
		n, err := from.Read(buf)
		if n > 0 {
			if _, werr := to.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// sharePrinted carries what the CLI prints to the screen, and then offers a
// copy to the server.
//
// The order is the point. The screen is written first and the queue is given
// the same bytes afterwards, by a call that cannot block: a server that has
// stopped reading, or that was never there, cannot delay one character of
// what the person in front of this terminal sees.
func sharePrinted(from io.Reader, to io.Writer, q *shareQueue) {
	buf := make([]byte, shareRead)
	for {
		n, err := from.Read(buf)
		if n > 0 {
			_, _ = to.Write(buf[:n])
			q.push(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

/* -------------------------------------------------------------- the link --- */

// shareLink is the connection to a rota serving this machine: where to find
// it, what to tell it, and what to do with what it says back.
//
// It may be nil-linked at any moment. Nothing above it knows or cares: the
// queue is written to whether or not anybody is draining it, and a link that
// comes back finds the recent output waiting and a count of what it missed.
type shareLink struct {
	path string
	q    *shareQueue
	err  io.Writer
	// hello is remade on every attempt, because the window may have been
	// dragged since the last one.
	hello func() share.HelloMsg
	// into is where input frames go, and kill what a kill frame means here.
	// into is nil for a terminal shared to be watched, which is the second
	// of the two places that rule is enforced.
	into func([]byte)
	kill func()

	stop   chan struct{}
	sent   chan struct{} // closed once the exit frame has gone out
	gone   chan struct{} // closed once nothing of this link is still running
	closed sync.Once
	told   sync.Once
	said   sync.Once

	mu      sync.Mutex
	conn    *share.Conn
	id      string
	running bool
}

func newShareLink(path string, q *shareQueue, errOut io.Writer, hello func() share.HelloMsg) *shareLink {
	return &shareLink{
		path: path, q: q, err: errOut, hello: hello,
		stop: make(chan struct{}), sent: make(chan struct{}), gone: make(chan struct{}),
	}
}

// start makes one attempt now — so that the line rota prints before the CLI
// takes the screen is true — and then keeps trying in the background for as
// long as the terminal lives.
func (l *shareLink) start() (id string, why string) {
	c, id, why, final := l.connect()
	if c != nil {
		l.set(c, id)
	}
	l.mu.Lock()
	l.running = true
	l.mu.Unlock()
	go func() {
		defer close(l.gone)
		l.keep(c, final)
	}()
	return id, why
}

// connect is one attempt: dial, say hello, and read the one frame that says
// whether this server will have it.
func (l *shareLink) connect() (c *share.Conn, id, why string, final bool) {
	conn, err := net.DialTimeout("unix", l.path, shareHandshake)
	if err != nil {
		return nil, "", "", false
	}
	k := share.NewConn(conn, shareBeat)
	k.Deadline(time.Now().Add(shareHandshake))
	if err := k.SendJSON(share.Hello, l.hello()); err != nil {
		k.Close()
		return nil, "", "", false
	}
	kind, payload, err := k.Read()
	if err != nil {
		k.Close()
		return nil, "", "", false
	}
	switch kind {
	case share.Welcome:
		var msg share.WelcomeMsg
		if share.Unmarshal(payload, &msg) != nil {
			k.Close()
			return nil, "", "", false
		}
		k.Clear()
		return k, msg.Terminal, "", false
	case share.Refused:
		var msg share.RefusedMsg
		_ = share.Unmarshal(payload, &msg)
		k.Close()
		return nil, "", msg.Message, msg.Final
	}
	k.Close()
	return nil, "", "", false
}

// keep serves the link it was given and then goes on trying, quietly, for as
// long as this terminal lives. A server that is not running is not a
// problem to report over and over: it is a server that is not running.
func (l *shareLink) keep(c *share.Conn, final bool) {
	for {
		if c != nil {
			done := l.serve(c)
			l.set(nil, "")
			c = nil
			if done {
				return
			}
		}
		if final {
			return
		}
		select {
		case <-l.stop:
			return
		case <-time.After(shareRetry):
		}
		var id, why string
		c, id, why, final = l.connect()
		if why != "" {
			l.say(why)
		}
		if c != nil {
			l.set(c, id)
		}
	}
}

// serve carries one link until it breaks: one goroutine draining the queue
// into it, one pinging it, and this one reading what the server says.
//
// It does not return until both of the others have. A link that is over has
// to be over completely, or a retry would have two goroutines writing to two
// sockets for the same terminal.
//
// done says this terminal is not to be offered again — the server ended it,
// or refused it. Coming back after either would be arguing with somebody who
// has already decided.
func (l *shareLink) serve(c *share.Conn) (done bool) {
	over := make(chan struct{})
	var busy sync.WaitGroup
	busy.Add(2)
	go func() { defer busy.Done(); l.drain(c, over) }()
	go func() { defer busy.Done(); l.beat(c, over) }()
	// In this order on the way out: the socket first, which frees a drain
	// that is blocked writing to a peer that stopped reading; then the
	// channel, which frees the two loops; then the wait.
	defer busy.Wait()
	defer close(over)
	defer c.Close()
	for {
		kind, payload, err := c.Read()
		if err != nil {
			return false
		}
		switch kind {
		case share.Input:
			// Never in watch mode: the sharer decided, where the terminal
			// is, that this is a window and not a keyboard.
			if l.into != nil && len(payload) > 0 {
				l.into(payload)
			}
		case share.Kill:
			// Somebody ended this terminal from the page. The CLI is hung
			// up here, and the link is not offered again: rota is on its
			// way out, and a reconnection in the meantime would put the
			// terminal back on the page that has just closed it.
			if l.kill != nil {
				l.kill()
			}
			return true
		case share.Ping:
			_ = c.Send(share.Pong, nil)
		case share.Pong:
		case share.Refused:
			var msg share.RefusedMsg
			if share.Unmarshal(payload, &msg) == nil && msg.Message != "" {
				l.say(msg.Message)
			}
			return true
		}
	}
}

// drain is the only thing that writes the terminal's output to a socket, and
// the only thing that ever waits for one.
func (l *shareLink) drain(c *share.Conn, done <-chan struct{}) {
	for {
		bits, lost, exit, ok := l.q.take(done)
		if !ok {
			return
		}
		if lost > 0 {
			// In front of the bytes that follow it, which is where it
			// happened: everything still queued is newer than everything
			// that was dropped.
			if c.SendJSON(share.Gap, share.GapMsg{Bytes: lost}) != nil {
				return
			}
		}
		for _, b := range bits {
			if c.Send(share.Output, b) != nil {
				return
			}
		}
		if exit != nil {
			_ = c.SendJSON(share.Exit, share.ExitMsg{Code: *exit})
			l.flushed()
			return
		}
	}
}

func (l *shareLink) beat(c *share.Conn, done <-chan struct{}) {
	t := time.NewTicker(shareBeat)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if c.Send(share.Ping, nil) != nil {
				return
			}
		}
	}
}

func (l *shareLink) flushed() { l.told.Do(func() { close(l.sent) }) }

// resized tells the server the window was dragged. It goes straight down the
// socket rather than through the queue: it is a fact about now, and a size
// held up behind a screenful of output would be a size that was true a
// moment ago.
func (l *shareLink) resized(cols, rows uint16) {
	if c := l.link(); c != nil {
		_ = c.SendJSON(share.Resized, share.ResizedMsg{Cols: cols, Rows: rows})
	}
}

// finished says the CLI has ended, and gives the exit frame a moment to
// leave before this process does. It waits for the frame and not for an
// answer: there is nothing to answer.
func (l *shareLink) finished(code int) {
	l.q.done(code)
	// Only where there is something to wait for. A terminal nobody was
	// watching must not add a pause to its own exit for a frame that has
	// nowhere to go.
	if l.link() != nil {
		select {
		case <-l.sent:
		case <-time.After(shareFlush):
		}
	}
	l.close()
}

// close ends the link and waits for what it started to have stopped, so that
// nothing of this terminal is still writing to a socket after the terminal
// is over. The wait is bounded: a dial that is half way through a connection
// nobody is going to answer is not worth holding an exit for.
func (l *shareLink) close() {
	l.closed.Do(func() {
		close(l.stop)
		if c := l.link(); c != nil {
			_ = c.Close()
		}
	})
	l.mu.Lock()
	running := l.running
	l.mu.Unlock()
	if !running {
		return
	}
	select {
	case <-l.gone:
	case <-time.After(shareFlush):
	}
}

func (l *shareLink) set(c *share.Conn, id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.conn, l.id = c, id
}

func (l *shareLink) link() *share.Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn
}

// say prints one thing the server refused with, once. A sentence repeated
// every three seconds on top of somebody's full-screen editor is worse than
// no sentence at all.
func (l *shareLink) say(msg string) {
	l.said.Do(func() {
		if l.err != nil {
			fmt.Fprintf(l.err, "rota: %s\n", msg)
		}
	})
}

/* ------------------------------------------------------------ where it is --- */

// shareSock is where a server on this machine listens, given the store. It
// is under the store's own terminals directory, which is made for its owner
// alone: who may share a terminal is decided by the filesystem.
func shareSock(s *store.Store) (string, error) {
	dir, err := s.TerminalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, share.SockName), nil
}

// shareLabel is what to call this terminal when nothing was said: the name
// of the folder it was started in, which is what somebody looking at a list
// of terminals recognizes it by.
func shareLabel(cwd string) string {
	base := filepath.Base(cwd)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return ""
	}
	return base
}

// shareOpening is the one line rota prints before the CLI takes the screen.
// Whichever of the two it is, it is the truth at that moment: there is a
// server and this terminal is on it, or there is not and it will be as soon
// as there is.
func shareOpening(who, bin, id, path, mode string) string {
	watching := ""
	if mode == share.ModeWatch {
		watching = ", to be watched only"
	}
	if id != "" {
		return fmt.Sprintf("rota: %s via %s — shared to the rota server on this machine as terminal %s%s\n",
			who, bin, id, watching)
	}
	if len(path) > shareMaxPath {
		return fmt.Sprintf("rota: %s via %s — this terminal cannot be shared: %s is too long for a unix socket\n",
			who, bin, path)
	}
	return fmt.Sprintf("rota: %s via %s — no rota server is listening at %s; sharing begins when one is\n",
		who, bin, path)
}

// shareStat is the store-side check rota makes before anything else, so the
// refusals arrive before the account is prepared rather than after.
func shareStat(in *os.File) error {
	// The platform first. Somewhere with no pseudo-terminal, standard input
	// is not a terminal either, and "this needs a terminal" would send
	// somebody looking for one rather than telling them what is true.
	if !pty.Supported {
		return fmt.Errorf("--share needs a pseudo-terminal, and %s has none; rota shares a terminal on linux and macOS",
			runtime.GOOS)
	}
	if !isTerminal(in) {
		return usageErr("%s", shareNoTerm)
	}
	return nil
}
