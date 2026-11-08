package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
	"github.com/professor93/rota/internal/ring"
	"github.com/professor93/rota/internal/share"
)

// fakeHolder is `rota serve hold` played by this test binary, which TestMain
// turns into one when ROTA_FAKE_HOLDER=1. It does what the server needs of a
// holder — the request on stdin, the lock, the socket, meta.json, the Ready
// line, the link with its offsets — and runs no CLI: what it prints and how
// it misbehaves are its environment's to say. Its holder.log says when the
// daemon closed a link at its end.
//
//	FAKE_HOLDER_SLOW=<duration>   wait that long before anything else
//	FAKE_HOLDER_FAIL=<kind>       answer Ready with that kind of failure
//	FAKE_HOLDER_LOG=<path>        write the request and ROTA_HOME there
//	FAKE_HOLDER_PRINT=<text>      print that before anything links
//	FAKE_HOLDER_TITLE, _CONVERSATION, _WAITING   the State it reports
//	FAKE_HOLDER_SCREEN=none       answer a ScreenAsk with no screen
//	FAKE_HOLDER_SCREEN=empty      a screen with nothing to paint
//	FAKE_HOLDER_SCREEN_AT=<n>     a screen standing for offset n
//	FAKE_HOLDER_SCREEN_BEHIND=<n> a screen standing for n bytes before the
//	                              end, as an emulator that is behind
//	FAKE_HOLDER_SCREEN_AHEAD=<t>  print t, answer for the end, send t after
//	FAKE_HOLDER_REFUSE=<text>     refuse keystrokes holding that text
//	FAKE_HOLDER_DROP_AT=<n>       drop the link once, after count's line n
//	FAKE_HOLDER_GARBLE_AT=<n>     once, after count's line n, write the link
//	                              a frame that is not one, and keep it
//	FAKE_HOLDER_DEAF=1            ignore Stop, SIGHUP and SIGTERM
//	FAKE_HOLDER_SIGNALS=<path>    write down each SIGHUP and SIGTERM there
//	FAKE_HOLDER_UNLINKED=<text>   print that 200 ms after a link drops, once,
//	                              and say so in holder.log
//	FAKE_HOLDER_KEEP=<n>          keep only the last n bytes printed
//	FAKE_HOLDER_RECORD_LATE=<d>   record the ending that long after saying it
//	FAKE_HOLDER_REFUSE_LINKS=<n>  refuse the first n links from the daemon
//	FAKE_HOLDER_MAJOR=<n>         speak protocol n alone, and record it
//	FAKE_HOLDER_VERSION=<v>       the rota version meta.json names
//	FAKE_HOLDER_CONVERSATION_CWD  the folder the conversation is in
//	FAKE_HOLDER_CONVERSATION_NAME the name the CLI gave the conversation
//	FAKE_HOLDER_RECORD=<path>     write down each input, resize, send and
//	                              screen asked for there
//	FAKE_HOLDER_SEND=waiting      answer a send that the session waits on its
//	                              person, unless it is forced
//	FAKE_HOLDER_SEND=exit         take a send, answer nothing, and exit 0
//	                              200 ms later
//	FAKE_HOLDER_SEND_AFTER=<path> type a send only once path exists, as a
//	                              session that is busy for a while
//
// The conversation and its folder are written into meta.json as well, as a
// real holder keeps them there for a session that has stopped.
//
// Typed lines are its commands: "exit N" ends it with code N, "count N"
// prints N numbered lines, "flood N" prints N lines of a kilobyte as fast as
// it can and then "flood done", and anything else is answered "got:<line>". A send is
// typed as its text and Enter. Ctrl-C throws away the line so far and
// prints "^C".
func fakeHolder() {
	// However a test fails, no fake outlives it by much.
	time.AfterFunc(2*time.Minute, func() { os.Exit(3) })
	var req holdwire.Request
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&req); err != nil {
		fakeReady(holdwire.Ready{Error: "no request: " + err.Error(), Kind: holdwire.KindOther})
		os.Exit(1)
	}
	if d, err := time.ParseDuration(os.Getenv("FAKE_HOLDER_SLOW")); err == nil {
		time.Sleep(d)
	}
	if path := os.Getenv("FAKE_HOLDER_LOG"); path != "" {
		raw, _ := json.Marshal(map[string]any{"request": req, "rota_home": os.Getenv("ROTA_HOME")})
		_ = os.WriteFile(path, raw, 0o600)
	}
	if kind := os.Getenv("FAKE_HOLDER_FAIL"); kind != "" {
		fakeReady(holdwire.Ready{Error: "the holder says " + kind, Kind: kind})
		os.Exit(1)
	}
	terminals := filepath.Join(os.Getenv("ROTA_HOME"), "terminals")
	keep, err := strconv.Atoi(os.Getenv("FAKE_HOLDER_KEEP"))
	if err != nil {
		keep = 1 << 20
	}
	f := &fake{terminals: terminals, dir: holdwire.Dir(terminals, req.ID),
		ring: ring.New(keep), cols: req.Cols, rows: req.Rows}
	if err := f.open(&req); err != nil {
		fakeReady(holdwire.Ready{Error: err.Error(), Kind: holdwire.KindOther})
		os.Exit(1)
	}
	// Before the answer, as the real holder does: whoever reads it may
	// signal at once, and a hangup before Notify would end this process by
	// default rather than be recorded.
	f.signals()
	fakeReady(holdwire.Ready{OK: true, PID: os.Getpid()})
	if null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stdout.Close()
		os.Stdout = null
	}
	f.print([]byte(os.Getenv("FAKE_HOLDER_PRINT")))
	f.accept()
	os.Exit(0)
}

func fakeReady(r holdwire.Ready) {
	raw, _ := json.Marshal(r)
	os.Stdout.Write(append(raw, '\n'))
}

type fake struct {
	terminals, dir string
	ln             net.Listener
	meta           *holdwire.Meta
	release        func()
	log            *os.File

	mu         sync.Mutex
	ring       *ring.Ring
	cols, rows uint16
	link       *share.Conn
	raw        net.Conn // link's socket, for a frame that is not one
	dropped    bool
	garbled    bool
	unlinked   bool
	refused    int
	line       []byte
}

// major is the protocol this fake speaks: this rota's, or the one its
// environment names.
func fakeMajor() int {
	if n, err := strconv.Atoi(os.Getenv("FAKE_HOLDER_MAJOR")); err == nil {
		return n
	}
	return holdwire.Major
}

// open is steps 3 to 7 of the real holder: the directory, the lock, the
// socket and the record.
func (f *fake) open(req *holdwire.Request) error {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	f.log, _ = os.OpenFile(holdwire.LogPath(f.dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	release, err := holdwire.Lock(f.dir)
	if err != nil {
		return err
	}
	f.release = release
	if f.ln, err = net.Listen("unix", holdwire.SockPath(f.dir)); err != nil {
		return err
	}
	start, _ := proc.StartTime(os.Getpid())
	boot, _ := proc.BootID()
	version := os.Getenv("FAKE_HOLDER_VERSION")
	if version == "" {
		version = "fake"
	}
	f.meta = &holdwire.Meta{ID: req.ID, Name: req.Name, Kind: req.Kind, Account: req.Account,
		Provider: "claude", HolderPID: os.Getpid(), HolderStart: start, BootID: boot,
		Protocol: fakeMajor(), Version: version, Cwd: req.Cwd, Args: req.Args, Started: time.Now(),
		Conversation: os.Getenv("FAKE_HOLDER_CONVERSATION"), ConversationCwd: os.Getenv("FAKE_HOLDER_CONVERSATION_CWD"),
		ConversationName: os.Getenv("FAKE_HOLDER_CONVERSATION_NAME")}
	return holdwire.WriteMeta(f.dir, f.meta)
}

func (f *fake) logf(format string, args ...any) {
	if f.log != nil {
		fmt.Fprintf(f.log, format+"\n", args...)
	}
}

func (f *fake) signals() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		for sig := range ch {
			if path := os.Getenv("FAKE_HOLDER_SIGNALS"); path != "" {
				if w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
					fmt.Fprintln(w, sig)
					w.Close()
				}
			}
			if os.Getenv("FAKE_HOLDER_DEAF") == "" {
				f.exit(129)
			}
		}
	}()
}

// print keeps p in the ring and sends it down the link, under one lock, so
// the offsets a daemon is sent are always in order.
func (f *fake) print(p []byte) {
	if len(p) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	off := f.ring.End()
	f.ring.Write(p)
	if f.link != nil {
		_ = f.link.Send(holdwire.Output, holdwire.PutOutput(off, p))
	}
}

func (f *fake) accept() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serve(c)
	}
}

// serve is one link: the handshake, then frames until it breaks. Only the
// process holding the store's daemon lock may link, as with a real holder.
func (f *fake) serve(c net.Conn) {
	conn := share.NewConn(c, 0)
	defer conn.Close()
	conn.Deadline(time.Now().Add(10 * time.Second))
	var hello holdwire.HelloMsg
	if kind, err := share.ReadJSON(conn, &hello); err != nil || kind != holdwire.Hello {
		return
	}
	if uc, ok := c.(*net.UnixConn); ok {
		pid, err := proc.PeerPID(uc)
		d, held := holdwire.RunningDaemon(f.terminals)
		if err != nil || !held || d.PID != pid {
			_ = conn.SendJSON(holdwire.Refused, holdwire.RefusedMsg{Message: "only the rota daemon serving this store may link to a session"})
			f.logf("refused a link from pid %d (%v)", pid, err)
			return
		}
	}
	major, ok := holdwire.Agree([]int{fakeMajor()}, hello.Majors)
	if !ok {
		return
	}
	n, _ := strconv.Atoi(os.Getenv("FAKE_HOLDER_REFUSE_LINKS"))
	f.mu.Lock()
	refuse := f.refused < n
	if refuse {
		f.refused++
	}
	f.mu.Unlock()
	if refuse {
		_ = conn.SendJSON(holdwire.Refused, holdwire.RefusedMsg{Message: "not now"})
		return
	}
	f.mu.Lock()
	w := holdwire.WelcomeMsg{Major: major, Features: []string{holdwire.FeatScreen, holdwire.FeatState, holdwire.FeatSend, holdwire.FeatStop},
		Version: "fake", Oldest: f.ring.Oldest(), End: f.ring.End(), Cols: f.cols, Rows: f.rows}
	f.mu.Unlock()
	if conn.SendJSON(holdwire.Welcome, w) != nil || conn.SendJSON(holdwire.State, f.state()) != nil {
		return
	}
	conn.Clear()
	defer f.unlink(conn)
	for {
		kind, payload, err := conn.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				f.logf("the daemon closed a link")
			}
			return
		}
		f.frame(conn, c, kind, payload)
	}
}

// unlink is a link gone from under this fake: whatever it was told to
// print once nobody is linked is printed a moment later, once.
func (f *fake) unlink(conn *share.Conn) {
	text := os.Getenv("FAKE_HOLDER_UNLINKED")
	f.mu.Lock()
	gone := f.link == conn && !f.unlinked && text != ""
	if gone {
		f.unlinked = true
	}
	f.mu.Unlock()
	if gone {
		time.AfterFunc(200*time.Millisecond, func() {
			f.print([]byte(text))
			f.logf("printed with nobody linked")
		})
	}
}

func (f *fake) state() holdwire.StateMsg {
	st := holdwire.StateMsg{Title: os.Getenv("FAKE_HOLDER_TITLE")}
	conv, wait, name := os.Getenv("FAKE_HOLDER_CONVERSATION"), os.Getenv("FAKE_HOLDER_WAITING"), os.Getenv("FAKE_HOLDER_CONVERSATION_NAME")
	if conv != "" || wait != "" || name != "" {
		st.Registry = &holdwire.Registry{SessionID: conv, Name: name, Status: "idle", WaitingFor: wait,
			Cwd: os.Getenv("FAKE_HOLDER_CONVERSATION_CWD")}
	}
	return st
}

func (f *fake) frame(conn *share.Conn, c net.Conn, kind byte, payload []byte) {
	switch kind {
	case holdwire.Since:
		var msg holdwire.SinceMsg
		_ = share.Unmarshal(payload, &msg)
		f.mu.Lock()
		if f.link != nil && f.link != conn {
			f.link.Close() // a new link replaces the old
		}
		f.link, f.raw = conn, c
		if data, at := f.ring.Read(msg.Offset); len(data) > 0 {
			_ = conn.Send(holdwire.Output, holdwire.PutOutput(at, data))
		}
		f.mu.Unlock()
	case holdwire.Input:
		seq, p, err := holdwire.InputOf(payload)
		if err != nil {
			return
		}
		f.record("input %q", p)
		if no := os.Getenv("FAKE_HOLDER_REFUSE"); no != "" && bytes.Contains(p, []byte(no)) {
			_ = conn.SendJSON(holdwire.InputRefused, holdwire.InputRefusedMsg{Seq: seq,
				Message: "the session is not taking keystrokes this fast; that input was not typed"})
			return
		}
		f.typed(p)
	case holdwire.Resize:
		var msg holdwire.SizeMsg
		_ = share.Unmarshal(payload, &msg)
		f.record("resize %dx%d", msg.Cols, msg.Rows)
		f.mu.Lock()
		f.cols, f.rows = msg.Cols, msg.Rows
		f.mu.Unlock()
		_ = conn.SendJSON(holdwire.Resized, msg)
	case holdwire.ScreenAsk:
		var msg holdwire.ScreenAskMsg
		_ = share.Unmarshal(payload, &msg)
		f.screen(conn, msg.Seq)
	case holdwire.Send:
		var msg holdwire.SendMsg
		_ = share.Unmarshal(payload, &msg)
		f.sent(conn, &msg)
	case holdwire.Stop:
		if os.Getenv("FAKE_HOLDER_DEAF") == "" {
			go f.exit(129)
		}
	case holdwire.Ping:
		_ = conn.Send(holdwire.Pong, nil)
	}
}

// sent answers a Send the way the environment says. Typed, it is its text
// and Enter, as the real holder types it.
func (f *fake) sent(conn *share.Conn, msg *holdwire.SendMsg) {
	f.record("send %q force=%t wait=%d", msg.Text, msg.Force, msg.WaitMS)
	switch os.Getenv("FAKE_HOLDER_SEND") {
	case "exit":
		time.AfterFunc(200*time.Millisecond, func() { f.exit(0) })
		return
	case "waiting":
		if !msg.Force {
			_ = conn.SendJSON(holdwire.Sent, holdwire.SentMsg{Seq: msg.Seq, Code: holdwire.SentWaiting,
				Message: "the session is waiting for the person (permission)"})
			return
		}
	}
	if path := os.Getenv("FAKE_HOLDER_SEND_AFTER"); path != "" {
		go func() {
			for {
				if _, err := os.Stat(path); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			f.typed([]byte(msg.Text + "\r"))
			_ = conn.SendJSON(holdwire.Sent, holdwire.SentMsg{Seq: msg.Seq})
		}()
		return
	}
	f.typed([]byte(msg.Text + "\r"))
	_ = conn.SendJSON(holdwire.Sent, holdwire.SentMsg{Seq: msg.Seq})
}

// record writes one line about what the daemon sent, for a test to read.
func (f *fake) record(format string, args ...any) {
	path := os.Getenv("FAKE_HOLDER_RECORD")
	if path == "" {
		return
	}
	if w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		fmt.Fprintf(w, format+"\n", args...)
		w.Close()
	}
}

// screen answers a ScreenAsk the way the environment says.
func (f *fake) screen(conn *share.Conn, seq uint64) {
	f.record("screen")
	head := holdwire.ScreenHead{Seq: seq, OK: os.Getenv("FAKE_HOLDER_SCREEN") != "none"}
	repaint := []byte("SCREEN")
	if os.Getenv("FAKE_HOLDER_SCREEN") == "empty" {
		repaint = nil
	}
	f.mu.Lock()
	head.Cols, head.Rows = f.cols, f.rows
	head.Offset = f.ring.End()
	if at, err := strconv.ParseInt(os.Getenv("FAKE_HOLDER_SCREEN_AT"), 10, 64); err == nil {
		head.Offset = at
	}
	if n, err := strconv.ParseInt(os.Getenv("FAKE_HOLDER_SCREEN_BEHIND"), 10, 64); err == nil {
		head.Offset = max(f.ring.Oldest(), f.ring.End()-n)
	}
	// Ahead: bytes the screen already shows, sent down the link only after
	// the screen has gone, and late enough that the viewer is attached.
	ahead := []byte(os.Getenv("FAKE_HOLDER_SCREEN_AHEAD"))
	aheadAt := f.ring.End()
	if len(ahead) > 0 {
		f.ring.Write(ahead)
		head.Offset = f.ring.End()
	}
	f.mu.Unlock()
	raw, _ := holdwire.PutScreen(head, repaint)
	_ = conn.Send(holdwire.Screen, raw)
	if len(ahead) > 0 {
		time.Sleep(200 * time.Millisecond)
		_ = conn.Send(holdwire.Output, holdwire.PutOutput(aheadAt, ahead))
	}
}

// floodLine fills out a flood's line to a kilobyte, so that a flood of tens
// of thousands is more than any kernel buffers for a reader that stopped
// reading.
var floodLine = strings.Repeat(".", 1000)

// typed takes keystrokes a line at a time. A Ctrl-C throws away the line
// typed so far and prints ^C, as a CLI's prompt does.
func (f *fake) typed(p []byte) {
	f.mu.Lock()
	var lines []string // each line typed, and "\x03" for each interrupt
	for _, b := range p {
		switch b {
		case '\r', '\n':
			lines = append(lines, string(f.line))
			f.line = f.line[:0]
		case 0x03:
			lines = append(lines, "\x03")
			f.line = f.line[:0]
		default:
			f.line = append(f.line, b)
		}
	}
	f.mu.Unlock()
	for _, l := range lines {
		f.command(l)
	}
}

func (f *fake) command(line string) {
	word, arg, _ := strings.Cut(line, " ")
	n, _ := strconv.Atoi(arg)
	switch word {
	case "\x03":
		f.print([]byte("^C\r\n"))
	case "":
	case "exit":
		go f.exit(n)
	case "count":
		go f.count(n)
	case "flood":
		go func() {
			for i := 1; i <= n; i++ {
				f.print(fmt.Appendf(nil, "flood-%06d %s\r\n", i, floodLine))
			}
			f.print([]byte("flood done\r\n"))
		}()
	default:
		f.print([]byte("got:" + line + "\r\n"))
	}
}

// count prints n numbered lines, slowly enough that a dropped link is
// dialled again while it is still printing.
func (f *fake) count(n int) {
	drop, _ := strconv.Atoi(os.Getenv("FAKE_HOLDER_DROP_AT"))
	garble, _ := strconv.Atoi(os.Getenv("FAKE_HOLDER_GARBLE_AT"))
	for i := 1; i <= n; i++ {
		f.print(fmt.Appendf(nil, "line-%04d\r\n", i))
		if i == garble {
			// A length of zero, which no frame has: the daemon's reading of
			// this link breaks while this end goes on as if nothing had.
			f.mu.Lock()
			if !f.garbled && f.raw != nil {
				f.garbled = true
				_, _ = f.raw.Write([]byte{0, 0, 0, 0})
			}
			f.mu.Unlock()
		}
		if i == drop {
			f.mu.Lock()
			if !f.dropped && f.link != nil {
				f.dropped = true
				f.link.Close()
				f.link = nil
			}
			f.mu.Unlock()
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// exit is the CLI having ended with code: the link is told, the record says
// so, and the lock goes, in the real holder's order.
func (f *fake) exit(code int) {
	f.mu.Lock()
	if f.link != nil {
		_ = f.link.SendJSON(holdwire.Exit, holdwire.ExitMsg{Code: code})
	}
	if d, err := time.ParseDuration(os.Getenv("FAKE_HOLDER_RECORD_LATE")); err == nil {
		time.Sleep(d)
	}
	f.meta.Ended, f.meta.Exit = time.Now(), &code
	_ = holdwire.WriteMeta(f.dir, f.meta)
	f.release()
	_ = os.Remove(holdwire.SockPath(f.dir))
	os.Exit(0)
}
