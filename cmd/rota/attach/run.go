package attach

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"

	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/vtscan"
)

// The timings an attach keeps, each a knob for the tests.
var (
	// drainFor is how long a leave goes on showing the session's output,
	// so what it had under way reaches the screen whole.
	drainFor = 150 * time.Millisecond
	// noteFor is how long a note stays up.
	noteFor = 3 * time.Second
	// teachFor is how long the first-attach card stays up with no key.
	teachFor = 5 * time.Second
	// redialFor is how long a lost daemon is waited for, and redialEvery
	// how often it is dialled meanwhile.
	redialFor   = 60 * time.Second
	redialEvery = 500 * time.Millisecond
	// forceAfter is how long after asking for the keyboard k takes it. It
	// mirrors the server's claimWait.
	forceAfter = 10 * time.Second
	// settleFor is how long Settle waits for its DA1 answer.
	settleFor = time.Second
)

// Run hands the terminal t to the session id of the daemon c reaches, until
// the person leaves, the session ends, or the attach is lost or refused. It
// never resizes the session but at the chord's z, and on every end it puts
// t back as it found it: every mode the session set off, the main screen,
// the cursor shown and on the window's last row, where the caller prints
// Result.Line.
//
// id is the session's whole id: a caller resolves what a person typed with
// client.Match first. Match reads an id, or four or more characters that
// begin one, before a name, so a session named like hex, beef or cafe1, is
// reached by that word as the session whose id it begins, when there is
// one. That is decided, not missed: an id is what the daemon itself never
// reads two ways, and Word, the come-back word of Result.Line, gives the
// name only because a person reads it more easily. An attach given an id
// never meets the question.
func Run(ctx context.Context, t Terminal, c *client.Client, id string, o Options) Result {
	return run(ctx, t, linkClient{c}, id, o)
}

// The phases of an attach.
const (
	runOpening   = iota // the first open, before any hello
	runLive             // showing a session
	runSwitching        // n or p: the old link closed, the next not yet open
	runRedialing        // the link lost, the daemon dialled again
	runLeaving          // d: draining the output, typing nothing
)

// What an overlay draws.
const (
	runNote = iota // a row of words over the window's last row
	runHint        // the chord's hint row, as wide as the window is
	runCard        // a card in the middle of the window
)

// runOverlay is what the attach draws over the session: one at a time,
// drawn from the view's copy of the screen's state and repaired by a
// redraw from it once it goes.
type runOverlay struct {
	on    bool
	kind  int
	text  string    // a note's
	lines []string  // a card's
	until time.Time // when it goes; zero is when something else takes it down
	key   bool      // it goes at the next key, too
	shown bool      // it is on the screen
	stale bool      // what was drawn since may have covered it
	at    time.Time // when it was last drawn
}

// runInput is one read of the person's terminal.
type runInput struct {
	p   []byte
	err error
}

// runFrame is one Next of the link of generation gen.
type runFrame struct {
	gen int
	f   client.Frame
	err error
}

// runOpened is what an open, for a switch or a redial, came to.
type runOpened struct {
	gen  int
	src  source
	ln   link
	sess client.Session
	err  error
	// final, for a redial, is the end the session's own state calls for: it
	// is stopped, or the daemon refuses it for good.
	final *Result
	at    time.Time // when the attempt began
}

// runWriter is the person's screen, remembering its first failure: a write
// that fails ends the attach, and nothing more is written but the reset.
type runWriter struct {
	w   io.Writer
	err error
}

func (w *runWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.w.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

// runner is one attach. Everything in it belongs to the goroutine in run:
// the person's input and the link's frames are read by goroutines of their
// own and handed over on channels, so that one goroutine alone writes to
// the terminal, drives the view and the chord, and types, and no read is
// ever waited on while it does.
type runner struct {
	ctx context.Context
	t   Terminal
	out *runWriter
	src source
	o   Options
	key byte

	done   chan struct{}
	input  chan runInput
	frames chan runFrame
	opened chan runOpened

	ch    *chord
	early []byte // typing that settle let through, for the first session
	phase int
	id    string
	sess  client.Session
	ln    link
	gen   int
	hello client.Hello
	view  *view

	holder, holderConn string
	askedAt            time.Time
	said               map[string]time.Time

	ov         runOverlay
	wantRedraw bool
	wantWindow bool
	unground   time.Time
	lastFlush  time.Time

	redialStart, redialNext time.Time
	dialling                bool
	lastErr                 error
	drainEnd                time.Time

	result *Result
}

// run is Run over any source.
func run(ctx context.Context, t Terminal, src source, id string, o Options) Result {
	key := o.Key
	if key == 0 {
		key = DefaultKey
	}
	o.Key = key
	r := &runner{
		ctx: ctx, t: t, out: &runWriter{w: t}, src: src, o: o, key: key,
		done:   make(chan struct{}),
		input:  make(chan runInput),
		frames: make(chan runFrame),
		opened: make(chan runOpened),
		ch:     newChord(key),
		id:     id,
		sess:   client.Session{ID: id},
		said:   map[string]time.Time{},
	}
	defer close(r.done)
	go r.read()
	if !o.Settle || r.settle() {
		r.write([]byte("\x1b[H\x1b[2J"))
		r.start(id)
		for r.result == nil {
			r.step()
		}
	}
	return r.finish()
}

// read hands what the person types to the loop until a read fails or the
// attach is over.
func (r *runner) read() {
	buf := make([]byte, 4096)
	for {
		n, err := r.t.Read(buf)
		if n > 0 {
			select {
			case r.input <- runInput{p: bytes.Clone(buf[:n])}:
			case <-r.done:
				return
			}
		}
		if err != nil {
			select {
			case r.input <- runInput{err: err}:
			case <-r.done:
			}
			return
		}
	}
}

// next hands the link's frames to the loop until it ends or the attach is
// over.
func (r *runner) next(gen int, l link) {
	for {
		f, err := l.Next()
		select {
		case r.frames <- runFrame{gen: gen, f: f, err: err}:
		case <-r.done:
			return
		}
		if err != nil {
			return
		}
	}
}

// start opens id for the phase the attach is in, away from the loop, which
// meanwhile goes on reading the person's keys.
func (r *runner) start(id string) {
	gen, src, watch := r.gen, r.src, r.o.Watch
	go func() {
		op := runOpened{gen: gen, src: src, at: time.Now()}
		op.ln, op.err = src.open(id, watch)
		op.sess = runFind(src, id)
		r.deliver(op)
	}()
}

// deliver hands op to the loop, or closes what it opened once the attach is
// over.
func (r *runner) deliver(op runOpened) {
	select {
	case r.opened <- op:
	case <-r.done:
		if op.ln != nil {
			op.ln.Close()
		}
	}
}

// runFind is what src lists of the session id, or only its id.
func runFind(src source, id string) client.Session {
	all, err := src.list()
	if err == nil {
		for _, s := range all {
			if s.ID == id {
				return s
			}
		}
	}
	return client.Session{ID: id}
}

// step waits for one thing to happen, acts on it, and draws what is due.
func (r *runner) step() {
	now := time.Now()
	wait := time.Hour
	if at, ok := r.deadline(now); ok {
		wait = max(at.Sub(now), 0)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	input := r.input
	if r.phase == runOpening {
		// What is typed before the first hello waits for it.
		input = nil
	}
	select {
	case <-r.ctx.Done():
		r.end(Lost, 0, r.ctx.Err())
		return
	case in := <-input:
		r.onInput(in, time.Now())
	case f := <-r.frames:
		if f.gen == r.gen {
			r.onFrame(f, time.Now())
		}
	case op := <-r.opened:
		r.onOpened(op, time.Now())
	case <-r.t.Resized():
		if r.view != nil {
			r.wantWindow = true
		}
	case <-timer.C:
	}
	if r.result != nil {
		return
	}
	now = time.Now()
	r.tick(now)
	if r.result == nil {
		r.paint(now)
	}
	if r.result == nil && r.out.err != nil {
		r.end(Lost, 0, r.out.err)
	}
}

// deadline is the next moment something is due without anything
// happening: a chord timing, an overlay's end, a crop to draw, a redial or
// the end of a drain.
func (r *runner) deadline(now time.Time) (time.Time, bool) {
	var at time.Time
	ok := false
	soon := func(t time.Time) {
		if !ok || t.Before(at) {
			at, ok = t, true
		}
	}
	if r.phase != runOpening && r.phase != runLeaving {
		if t, due := r.ch.due(); due {
			soon(t)
		}
	}
	if r.ov.on && !r.ov.until.IsZero() {
		soon(r.ov.until)
	}
	if r.phase == runLeaving {
		soon(r.drainEnd)
	}
	if r.phase == runRedialing && !r.dialling {
		soon(r.redialNext)
	}
	if r.view != nil && r.painting() {
		soon(now.Add(cropEvery))
	}
	return at, ok
}

// painting says whether paint has something waiting to draw.
func (r *runner) painting() bool {
	v := r.view
	return r.wantRedraw || r.wantWindow || v.crop && v.dirty ||
		r.ov.on && (!r.ov.shown || r.ov.stale) && v.screenOK()
}

// tick acts on what has come due.
func (r *runner) tick(now time.Time) {
	if r.phase != runOpening && r.phase != runLeaving {
		r.onEvents(r.ch.tick(now), now)
	}
	if r.result != nil {
		return
	}
	if r.ov.on && !r.ov.until.IsZero() && !now.Before(r.ov.until) {
		r.unlay()
	}
	switch r.phase {
	case runLeaving:
		if !now.Before(r.drainEnd) {
			r.end(Detached, 0, nil)
		}
	case runRedialing:
		if !r.dialling && !now.Before(r.redialNext) {
			r.dial()
		}
	}
}

// paint draws what is waiting: the window's new size, a repaint, the crop
// and the overlay, in that order. Passing through, it waits for the
// session's output to be between sequences, so nothing of the attach's own
// lands inside one of the CLI's; a sequence that never ends is waited for
// only drainFor.
func (r *runner) paint(now time.Time) {
	v := r.view
	if v == nil || !r.painting() {
		r.unground = time.Time{}
		return
	}
	if !v.crop && len(v.pend) > 0 {
		if r.unground.IsZero() {
			r.unground = now
		}
		if now.Sub(r.unground) < drainFor {
			return
		}
	}
	r.unground = time.Time{}
	if r.wantWindow {
		r.wantWindow = false
		cols, rows := r.t.Size()
		if r.ov.shown {
			// The overlay is where the old window had its last row.
			r.wantRedraw = true
		}
		v.window(cols, rows)
		r.lastFlush = now
	}
	if r.wantRedraw {
		r.wantRedraw = false
		v.redraw()
		r.lastFlush = now
		r.ov.stale = true
	}
	if v.crop && v.dirty && now.Sub(r.lastFlush) >= cropEvery {
		if v.flush() {
			r.ov.stale = true
		}
		r.lastFlush = now
	}
	// An overlay the session drew over is drawn again, at most every
	// cropEvery, so a CLI that redraws all the time cannot hide it.
	if r.ov.on && (!r.ov.shown || r.ov.stale && now.Sub(r.ov.at) >= cropEvery) && v.screenOK() && !(v.crop && v.dirty) {
		r.write(r.overlay())
		r.ov.shown, r.ov.stale, r.ov.at = true, false, now
	}
}

// onInput takes one read of the person's terminal.
func (r *runner) onInput(in runInput, now time.Time) {
	if r.phase == runLeaving {
		// Dropped: whatever comes now, a key's release or a late reply, is
		// for nobody, and a failed read ends nothing the drain is not
		// already ending.
		if in.err != nil {
			r.input = nil
		}
		return
	}
	if in.err != nil {
		r.end(Lost, 0, in.err)
		return
	}
	if r.ov.on && r.ov.key {
		// A card that goes at the next key goes, and the key is the
		// person's as usual.
		r.unlay()
	}
	r.onEvents(r.ch.feed(in.p, now), now)
}

// onOpened takes what an open came to.
func (r *runner) onOpened(op runOpened, now time.Time) {
	if op.gen != r.gen || r.phase == runLeaving {
		// Stale, or the person left while it was opening: not shown.
		if op.ln != nil {
			op.ln.Close()
		}
		return
	}
	if op.err == nil {
		r.src = op.src
		r.show(op.ln, op.sess, now)
		return
	}
	switch r.phase {
	case runOpening, runSwitching:
		r.sess = op.sess
		r.end(Refused, 0, op.err)
	case runRedialing:
		r.dialling = false
		if op.final != nil {
			r.sess = op.sess
			r.end(op.final.Why, op.final.Code, op.final.Err)
			return
		}
		r.lastErr = op.err
		if !now.Before(r.redialStart.Add(redialFor)) {
			r.end(Lost, 0, r.lastErr)
			return
		}
		r.redialNext = op.at.Add(redialEvery)
	}
}

// show makes l the session shown: a fresh view at the session's size in
// this window, the link read, and, before the first session, the card for
// a person who has not seen the chord.
func (r *runner) show(l link, sess client.Session, now time.Time) {
	first := r.phase == runOpening
	if !first {
		// A switch or a redial: nothing typed for the old link, or on the
		// way here, is typed into this one, not even the tail of a key
		// begun before its hello.
		r.ch.reset()
		if r.view != nil {
			r.view.close()
			r.view = nil
			r.write(runPutBack())
		}
	}
	r.ln, r.hello, r.sess, r.id = l, l.Hello(), sess, sess.ID
	r.holder, r.holderConn = r.hello.Holder, r.hello.HolderConn
	r.askedAt = time.Time{}
	r.ov, r.wantRedraw, r.wantWindow = runOverlay{}, false, false
	r.phase = runLive
	name := sess.Name
	if name == "" {
		name = sess.ID
	}
	key := r.key
	cols, rows := r.t.Size()
	r.view = newView(r.out, int(r.hello.Cols), int(r.hello.Rows), cols, rows, func(sc, sr, wc, wr int) string {
		return cropNote(name, key, sc, sr, wc, wr)
	})
	r.lastFlush = now
	go r.next(r.gen, l)
	if !first {
		return
	}
	if r.o.Teach != nil && r.view.screenOK() && r.o.Teach(r.hello.You.Name) {
		r.lay(runOverlay{kind: runCard, lines: helpCard(key, true), until: now.Add(teachFor), key: true})
	}
	if early := r.early; len(early) > 0 {
		r.early = nil
		r.onInput(runInput{p: early}, now)
	}
}

// runPutBack is what a window is given between one session and the next:
// every mode the last one set off, the main screen, and a clear.
func runPutBack() []byte {
	return append(vtscan.ResetAll(), "\x1b[0m\x1b[r\x1b[H\x1b[2J"...)
}

// end ends the attach, the first time it is called.
func (r *runner) end(why Why, code int, err error) {
	if r.result != nil {
		return
	}
	r.result = &Result{Why: why, Code: code, Err: err}
}

// finish lets go of the link and the view, puts the terminal back, and
// says how the attach ended.
func (r *runner) finish() Result {
	res := *r.result
	res.Session = r.sess
	if r.ln != nil {
		r.ln.Close()
		r.ln = nil
	}
	if r.view != nil {
		r.view.close()
		r.view = nil
	}
	// Written past a failed write too: a terminal that failed once may yet
	// take the reset.
	_, rows := r.t.Size()
	b := append([]byte("\x1b[0m\x1b[r"), vtscan.ResetAll()...)
	b = append(b, "\x1b["+strconv.Itoa(max(rows, 1))+";1H"...)
	_, _ = r.t.Write(b)
	return res
}

// write writes p to the person's screen.
func (r *runner) write(p []byte) {
	if len(p) > 0 {
		_, _ = r.out.Write(p)
	}
}
