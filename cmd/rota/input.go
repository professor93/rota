package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/holdwire"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/store"
)

// stdin is this process's own standard input, taken once by run into cli.in.
// It is a variable so a test can feed a run without a terminal.
var stdin io.Reader = os.Stdin

// maxLine bounds one line read from stdin or from a socket. A message is
// capped far lower in lib; this only keeps a mistake from filling memory
// before anything has looked at it.
const maxLine = 1 << 20

// newRunID names one open run. Eight random bytes: short enough to retype,
// and nothing anyone could guess their way onto — the socket it names takes
// messages into somebody's agent.
func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform rota runs on, and a run
		// id that is not random is one another local user could address.
		return ""
	}
	return hex.EncodeToString(b[:])
}

// firstLine reads one line from r, a byte at a time.
//
// A buffered reader would take whatever follows with it, and that belongs to
// the run: the first line is the prompt, and the messages after it arrive
// while the agent is answering it. Reading byte by byte is slower than it has
// to be and happens exactly once, for a line somebody typed.
func firstLine(r io.Reader) (string, error) {
	var line []byte
	var b [1]byte
	for len(line) <= maxLine {
		n, err := r.Read(b[:])
		if n > 0 && b[0] == '\n' {
			break
		}
		if n > 0 {
			line = append(line, b[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
	}
	text := strings.TrimSpace(strings.TrimSuffix(string(line), "\r"))
	if text == "" {
		return "", usageErr("a session needs an opening message: give one as the prompt, or type it on the first line of stdin")
	}
	return text, nil
}

// session runs one open run: the CLI stays up, more messages arrive from
// rota's own stdin and from the socket another terminal sends into, and what
// became of each is reported in the stream.
//
// It returns what the run produced, exactly as Run would have.
func (c *cli) session(s *store.Store, a *rota.Account, spec rota.Spec, reach *sends, watch, live *eventStream, verbose bool) (*rota.Result, error) {
	sess, err := s.Start(context.Background(), a, spec, nil, watch)
	if err != nil {
		return nil, err
	}
	reach.serve(sess)
	go c.feed(sess)
	// Drained to the end before the run is reported over, so every message's
	// fate is in the stream before the event that closes it.
	told := make(chan struct{})
	go func() {
		defer close(told)
		c.notices(sess, live, verbose)
	}()
	res, err := sess.Wait()
	<-told
	// What the session's end owes the store — what it said about an API
	// key's credit — is written by a goroutine, and this process exits soon
	// after returning: it is waited for here rather than cut short.
	s.Wait()
	return res, err
}

// feed reads more messages from rota's own stdin for as long as the run takes
// them: one message per line, and a few words that are commands rather than
// messages.
//
// Nothing here ends the process. A line the session refuses — too long, or
// sent after a close — is reported and the next line is read, because the run
// is still going and the person is still typing.
func (c *cli) feed(sess *rota.Session) {
	sc := bufio.NewScanner(c.in)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		// The scanner has taken the newline; a terminal on Windows leaves the
		// carriage return in front of it, and that is part of the same ending.
		line := strings.TrimSuffix(sc.Text(), "\r")
		var err error
		switch text, steer := strings.CutPrefix(line, "/steer "); {
		case line == "":
			continue
		case line == "/interrupt":
			_, err = sess.Interrupt()
		case line == "/close":
			if cerr := sess.Close(); cerr != nil {
				fmt.Fprintf(c.err, "rota: %s\n", printable(cerr.Error()))
			}
			return // nothing more can be sent, so nothing more is read
		case line == "/steer":
			err = fmt.Errorf("/steer takes the message to steer with")
		case steer:
			_, err = sess.Steer(strings.TrimSpace(text))
		case line[0] == '{':
			// A caller writing the CLI's own input vocabulary is taken at its
			// word and the line goes down the pipe as it is.
			err = sess.SendRaw([]byte(line))
		default:
			_, err = sess.Send(line)
		}
		if err != nil {
			fmt.Fprintf(c.err, "rota: %s\n", printable(err.Error()))
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(c.err, "rota: %s\n", printable(err.Error()))
	}
	// Nothing more is coming, which is what closing stdin says to the CLI:
	// it finishes the turn it is on and exits.
	if err := sess.Close(); err != nil {
		fmt.Fprintf(c.err, "rota: %s\n", printable(err.Error()))
	}
}

// notices carries what became of each message into the stream the caller is
// already reading. In text mode the answer is the only thing printed, as it
// is for any other run; -v puts the bookkeeping on stderr for someone
// watching one go by.
func (c *cli) notices(sess *rota.Session, live *eventStream, verbose bool) {
	for n := range sess.Notices() {
		switch {
		case live != nil && live.json:
			_ = live.emit(message.FromNotice(n))
		case verbose:
			fmt.Fprintf(c.err, "rota: %s %s\n", n.Kind, oneLine(n.ID))
		}
	}
}

/* --------------------------------------- the socket another terminal uses --- */

// sendRequest is one thing another terminal asks of an open run: one JSON
// object on the wire, one reply, then the connection closes.
type sendRequest struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	// Line is a message in the CLI's own input vocabulary, for a caller that
	// speaks it; rota passes it on rather than composing one.
	Line jsontext.Value `json:"line,omitzero"`
}

// sendReply is what the run says back: the id a message was given, the state
// it reached, or why nothing happened.
type sendReply struct {
	ID    string `json:"id,omitempty"`
	State string `json:"state,omitempty"`
	OK    bool   `json:"ok,omitzero"`
	Error string `json:"error,omitempty"`
}

// sockPath is where a run of this id listens.
func sockPath(s *store.Store, id string) (string, error) {
	dir, err := s.RunDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".sock"), nil
}

// sends is the socket an open run is reached at. A nil one is a run nobody
// can send into, which is how a platform or a directory that will not take a
// socket is carried: the run itself is unaffected.
type sends struct {
	ln   net.Listener
	path string
}

// listenForSends opens that socket.
//
// It happens before the run says its id, and not after the CLI has started,
// because an id announced a moment before anything is listening is an id
// whose first `rota send` is refused for no reason anyone could see.
func listenForSends(s *store.Store, id string) (*sends, error) {
	path, err := sockPath(s, id)
	if err != nil {
		return nil, err
	}
	// A socket file left behind by a killed run is not a run; the id is
	// random, so a file at this path can only be this run's own leftover.
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return &sends{ln: ln, path: path}, nil
}

// serve answers what arrives, one request per connection, until close.
func (l *sends) serve(sess *rota.Session) {
	if l == nil {
		return
	}
	go func() {
		for {
			conn, err := l.ln.Accept()
			if err != nil {
				return // the listener was closed, which is how this ends
			}
			go serveSend(sess, conn)
		}
	}()
}

// close takes the socket down and the file with it, so nothing answers for a
// run that is over.
func (l *sends) close() {
	if l == nil {
		return
	}
	_ = l.ln.Close()
	_ = os.Remove(l.path)
}

// serveSend answers one connection: one line in, one line out.
func serveSend(sess *rota.Session, conn net.Conn) {
	defer conn.Close()
	// A client that connects and then says nothing must not hold a goroutine
	// for the length of the run.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	if !sc.Scan() {
		return
	}
	raw, err := rota.Encode(answerSend(sess, sc.Bytes()))
	if err != nil {
		return
	}
	_, _ = conn.Write(append(raw, '\n'))
}

// answerSend does what one request asks and says what happened.
func answerSend(sess *rota.Session, line []byte) sendReply {
	var req sendRequest
	if err := rota.UnmarshalLenient(line, &req); err != nil {
		return sendReply{Error: "the request is not one JSON object: " + err.Error()}
	}
	fail := func(err error) sendReply { return sendReply{Error: err.Error()} }
	switch req.Kind {
	case "message":
		id, err := sess.Send(req.Text)
		if err != nil {
			return fail(err)
		}
		return sendReply{ID: id, State: "accepted"}
	case "steer":
		id, err := sess.Steer(req.Text)
		if err != nil {
			return fail(err)
		}
		return sendReply{ID: id, State: "accepted"}
	case "interrupt":
		id, err := sess.Interrupt()
		if err != nil {
			return fail(err)
		}
		return sendReply{ID: id}
	case "close":
		if err := sess.Close(); err != nil {
			return fail(err)
		}
		return sendReply{OK: true}
	case "raw":
		if err := sess.SendRaw(req.Line); err != nil {
			return fail(err)
		}
		return sendReply{OK: true}
	}
	return sendReply{Error: fmt.Sprintf("%q is not something to send: message, steer, interrupt, close or raw", req.Kind)}
}

/* ------------------------------------------------- sending from elsewhere --- */

const sendUsage = `rota send <run-or-session> [text] [flags]

Sends one more message into a run started with --input, from another terminal
or another program. The run id is the one that run printed when it started,
and is in its opening event as run_id.

The message is folded into the conversation the run is already having: the
same process, the same context, no resume. The answer comes out where the run
is printing, not here — here is only what became of the message.

  rota send 7f3c1a "also check the tests"   one more message
  rota send 7f3c1a "do this instead" --steer  interrupt first, then send
  rota send 7f3c1a --interrupt              stop what it is doing
  rota send 7f3c1a --close                  no more messages; let it finish

With no run of that id, it types into a held session — one rota run --detach
handed to this store's daemon — named by its id, the start of one, or its
name: the text, then Enter, once the CLI is ready for one. A CLI waiting on
its person for a permission or a question is not typed into, since Enter
there may approve a tool, unless --force says to.

  rota send api "and the tests?"            type into the held session api
  rota send api "yes" --force               even while it waits on its person

Flags:
`

// sendShort is what a mistyped send says: the shape, and where the rest is.
const sendShort = usageError("usage: send <run-or-session> <text>   (or --interrupt, --steer, --close for a run; `rota send -h` for the whole of it)")

// sendRunOnly is --interrupt, --steer or --close given for a held session.
const sendRunOnly = "those are for a run started with --input"

// send is the other end of --input: one request over the run's socket, one
// reply, printed.
func (c *cli) send(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var (
		interrupt = fs.Bool("interrupt", false, "stop the tool the agent is running")
		steer     = fs.Bool("steer", false, "interrupt first, so the message starts the next turn instead of joining this one")
		shut      = fs.Bool("close", false, "say there is nothing more: the run finishes its turn and exits")
		force     = fs.Bool("force", false, "type into a held session even while it waits on its person, where Enter may approve a tool")
	)
	words, err := parseFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(c.out, sendUsage)
			fs.SetOutput(c.out)
			fs.PrintDefaults()
			return nil
		}
		return usageErr("%v", err)
	}
	if len(words) == 0 {
		return usageError(sendShort)
	}
	id, text := words[0], strings.TrimSpace(strings.Join(words[1:], " "))
	if err := keyInArgs(id); err != nil {
		return err // a run id is eight hex characters; this is something else
	}
	req := sendRequest{Kind: "message", Text: text}
	switch {
	case *shut:
		req = sendRequest{Kind: "close"}
	case *interrupt:
		req = sendRequest{Kind: "interrupt"}
	case *steer:
		if text == "" {
			return usageErr("--steer needs the message to steer with")
		}
		req.Kind = "steer"
	case text == "":
		return usageError(sendShort)
	}

	s, err := c.openStore()
	if err != nil {
		return err
	}
	path, err := sockPath(s, id)
	terminals, terr := s.TerminalDir()
	s.Close() // the run itself holds no store lock, and neither should this
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		// No such run: a held session, then, which is typed into.
		if terr != nil {
			return terr
		}
		return c.sendHeld(terminals, id, text, *force, *interrupt || *steer || *shut)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		// A socket file with nothing behind it is what a killed run leaves;
		// it is cleared here so it stops answering for a run that is over.
		_ = os.Remove(path)
		return c.sendFailed("run %s is not running", id)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	raw, err := rota.Encode(req)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return c.sendFailed("run %s did not take the message: %v", id, err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	if !sc.Scan() {
		return c.sendFailed("run %s said nothing back", id)
	}
	line := sc.Bytes()
	var rep sendReply
	if err := rota.UnmarshalLenient(line, &rep); err != nil {
		return c.sendFailed("run %s answered something unreadable: %v", id, err)
	}
	if rep.Error != "" {
		return c.sendFailed("%s", rep.Error)
	}
	if c.json {
		_, err := c.out.Write(append(append([]byte(nil), line...), '\n'))
		return err
	}
	switch req.Kind {
	case "interrupt":
		fmt.Fprintln(c.out, "interrupted", oneLine(rep.ID))
	case "close":
		fmt.Fprintln(c.out, "closed")
	default:
		fmt.Fprintln(c.out, "accepted", oneLine(rep.ID))
	}
	return nil
}

// sendHeld types text and Enter into the held session target names, as
// client.Resolve finds one, once its CLI is ready: nothing printed when it
// is typed, or {"ok":true} with --json, and every refusal exits 1 in
// sendRefusal's words. Only reaching the session needs the daemon: with
// none running, the store's records say whether target names a held
// session, so that is said rather than that nothing does. runOnly is
// --interrupt, --steer or --close, which a held session has no use for.
func (c *cli) sendHeld(terminals, target, text string, force, runOnly bool) error {
	cl, err := rotaclient.Dial(terminals)
	if errors.Is(err, rotaclient.ErrNoDaemon) {
		return c.sendUnserved(terminals, target, runOnly)
	}
	if err != nil {
		return c.sendFailed("%v", err)
	}
	sess, err := cl.Resolve(target)
	switch {
	case errors.Is(err, rotaclient.ErrNoSession):
		return c.sendFailed(noRunOrSession, target)
	case err != nil:
		return c.sendFailed("%v", err)
	case runOnly:
		return usageErr("%s", sendRunOnly)
	}
	if err := cl.Send(sess.ID, rotaclient.SendReq{Text: text, Force: force}); err != nil {
		return c.sendFailed("%s", sendRefusal(heldCell(sess.Name), err))
	}
	if c.json {
		return c.emit(map[string]bool{"ok": true})
	}
	return nil
}

// noRunOrSession is a send to something that is neither.
const noRunOrSession = "no run or session %s; rota list --sessions shows what is running"

// sendUnserved is a send with no daemon to reach the session through. The
// records are read the way Resolve reads the daemon's listing: a whole id,
// the start of one, or the name of one that is running. A running one is
// said to want a daemon; a stopped one, which only an id finds, has ended.
func (c *cli) sendUnserved(terminals, target string, runOnly bool) error {
	var running, stopped *holdwire.Record
	for _, r := range holdwire.Scan(terminals) {
		if r.State == holdwire.StateBroken {
			continue
		}
		live := r.State == holdwire.StateLive || r.State == holdwire.StateUnreadable
		named := r.ID == target || len(target) >= 4 && strings.HasPrefix(r.ID, target) ||
			live && target != "" && (heldName(&r) == target || r.Meta.Name == target)
		switch {
		case !named:
		case live && running == nil:
			running = &r
		case !live && stopped == nil:
			stopped = &r
		}
	}
	switch {
	case running == nil && stopped == nil:
		return c.sendFailed(noRunOrSession, target)
	case runOnly:
		return usageErr("%s", sendRunOnly)
	case running != nil:
		return c.sendFailed("session %s is running, but no rota daemon serves %s to type into it; rota serve start starts one",
			heldCell(heldName(running)), filepath.Dir(terminals))
	}
	return c.sendFailed("session %s has ended", heldCell(heldName(stopped)))
}

// sendRefusal is why a send into the held session called name was not
// typed, in rota's own words where the daemon gave a reason it knows, and
// in the daemon's otherwise.
func sendRefusal(name string, err error) string {
	var no *rotaclient.Refusal
	if !errors.As(err, &no) {
		return err.Error()
	}
	switch no.Reason {
	case "waiting":
		return "session " + name + " is waiting for a permission or a question, and Enter there may approve a tool; --force sends anyway"
	case "keyboard":
		if no.Holder != "" {
			return heldCell(no.Holder) + " holds the keyboard of " + name
		}
	case "timeout":
		return "session " + name + " did not become ready within 30s; nothing was typed"
	case "ended":
		return "session " + name + " has ended"
	}
	return no.Message
}

// sendFailed says what went wrong in rota's own voice and exits 1. It is
// separate from the usual error path because these are facts about a run on
// this machine — no such run, a run that has ended — rather than conditions
// the SDK stated, and they read better as themselves. What they carry, the
// id that was typed, the run's reply, a socket's path, is said with each
// control character a space (see printable); JSON carries it as it is.
func (c *cli) sendFailed(format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	if c.json {
		_ = c.emit(map[string]string{"error": msg})
	} else {
		fmt.Fprintln(c.err, "rota: "+printable(msg))
	}
	return exitCode(1)
}
