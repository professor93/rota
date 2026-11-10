package attach

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/professor93/rota/cmd/rota/client"
)

// onEvents does what the person's input means, in its order.
func (r *runner) onEvents(evs []chordEvent, now time.Time) {
	for _, e := range evs {
		if r.result != nil || r.phase == runLeaving {
			return
		}
		switch e.act {
		case actType:
			// A watcher's keys are dropped here, not refused there, and
			// nothing is typed but into the session shown and settled.
			if r.phase == runLive && !r.o.Watch && r.ln != nil && !r.eating {
				l, b := r.ln, e.bytes
				r.cur.work.do(func() { _ = l.Type(b) })
			}
		case actLeave:
			r.leave(now)
		case actHint:
			if r.phase == runLive {
				r.lay(runOverlay{kind: runHint})
			}
		case actCancel:
			// The key was not one of the chord's: the keys stay up a
			// while, so the person can read them.
			if r.phase == runLive {
				r.lay(runOverlay{kind: runHint, until: now.Add(noteFor)})
			}
		default:
			if r.phase == runLive {
				r.act(e.act, now)
			}
		}
	}
	if r.eating && r.ch.state == chordGround {
		// The key that took the card down has been read whole.
		r.eating = false
	}
	if r.ov.on && r.ov.kind == runHint && r.ov.until.IsZero() && !r.ch.armed() {
		// The chord the hint was for has ended.
		r.unlay()
	}
}

// act does the chord's second key, on a session shown.
func (r *runner) act(a action, now time.Time) {
	switch a {
	case actNext:
		r.move(1)
	case actPrev:
		r.move(-1)
	case actKeyboard:
		r.claim(now)
	case actFit:
		r.fit(now)
	case actHelp:
		r.lay(runOverlay{kind: runCard, lines: helpCard(r.key, false), key: true, eat: true})
	}
}

// leave stops typing and goes on showing the output for drainFor. An
// overlay is taken down first, so the screen is left as the session drew
// it.
func (r *runner) leave(now time.Time) {
	r.phase = runLeaving
	r.drainEnd = now.Add(drainFor)
	r.unlay()
}

// move asks for the sessions, for n (dir 1) or p (-1). The session shown
// stays until the next one's hello, and whatever is typed meanwhile is
// dropped.
func (r *runner) move(dir int) {
	r.ch.reset()
	r.phase = runListing
	gen, src := r.gen, r.src
	// On a goroutine of its own, so it waits neither behind this session's
	// typing nor holds up the loop.
	go func() {
		defer r.guard()
		all, err := src.list()
		select {
		case r.listed <- runListed{gen: gen, dir: dir, all: all, err: err}:
		case <-r.done:
		}
	}()
}

// onListed opens the next live, attachable session, by when each started,
// or the one before it, round from the last to the first.
func (r *runner) onListed(l runListed, now time.Time) {
	if l.gen != r.gen || r.phase != runListing {
		return
	}
	r.phase = runLive
	if l.err != nil {
		r.note("rota: "+l.err.Error(), now)
		return
	}
	var live []client.Session
	for _, s := range l.all {
		if s.State == "live" && s.Attachable {
			live = append(live, s)
		}
	}
	slices.SortStableFunc(live, func(a, b client.Session) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	at := slices.IndexFunc(live, func(s client.Session) bool { return s.ID == r.sess.ID })
	var next client.Session
	switch {
	case at >= 0 && len(live) > 1:
		next = live[(at+l.dir+len(live))%len(live)]
	case at < 0 && len(live) > 0:
		// This session is no longer in the list: the first or the last.
		next = live[0]
		if l.dir < 0 {
			next = live[len(live)-1]
		}
	default:
		r.note("rota: no other live session", now)
		return
	}
	r.phase = runSwitching
	r.target = next
	r.start(next.ID)
}

// switchFailed keeps the session shown when the next one cannot be opened.
// The daemon's no is said in a note. A transport that failed is a lost
// link, since the daemon may be going: the session shown is redialled, or
// the attach is lost.
func (r *runner) switchFailed(op runOpened, now time.Time) {
	if runRefusal(op.err) == nil {
		r.lose(op.err, now)
		return
	}
	r.phase = runLive
	name := r.target.Name
	if name == "" {
		name = r.target.ID
	}
	r.note("rota: "+OneLine(name)+": "+op.err.Error(), now)
}

// holds says whether this connection holds the keyboard.
func (r *runner) holds() bool {
	return r.holderConn != "" && r.holderConn == r.hello.You.Conn
}

// claim asks for the keyboard, and at a k forceAfter or more after asking,
// takes it.
func (r *runner) claim(now time.Time) {
	l := r.ln
	switch {
	case r.o.Watch:
		r.note("rota: watching; this window cannot take the keyboard", now)
	case r.holds():
		r.note("rota: this window holds the keyboard", now)
	case r.askedAt.IsZero():
		r.cur.work.do(func() { _ = l.Claim(false) })
		r.askedAt = now
	case now.Sub(r.askedAt) >= forceAfter:
		r.cur.work.do(func() { _ = l.Claim(true) })
		r.askedAt = time.Time{}
	default:
		holder := OneLine(r.holder)
		if holder == "" {
			holder = "whoever holds it"
		}
		r.note("rota: asked "+holder+" for the keyboard; "+KeyName(r.key)+" k again after "+actionsSecs(forceAfter)+" takes it", now)
	}
}

// actionsSecs is d in whole seconds, rounded up, as a note says it.
func actionsSecs(d time.Duration) string {
	return strconv.Itoa(max(int(math.Ceil(d.Seconds())), 1)) + "s"
}

// fit asks for the session to be this window's size.
func (r *runner) fit(now time.Time) {
	cols, rows := r.t.Size()
	switch {
	case r.o.Watch:
		r.note("rota: watching; this window cannot fit the session", now)
	case cols == r.view.sessCols && rows == r.view.sessRows:
		r.note("rota: the session is already this size", now)
	default:
		l := r.ln
		r.cur.work.do(func() { _ = l.Fit(actionsCells(cols), actionsCells(rows)) })
	}
}

// actionsCells is n as a size the wire carries.
func actionsCells(n int) uint16 { return uint16(min(max(n, 1), math.MaxUint16)) }

// onFrame takes what the link sent.
func (r *runner) onFrame(f runFrame, now time.Time) {
	if f.err != nil {
		r.lose(f.err, now)
		return
	}
	d := f.f.Doc
	if d == nil {
		r.view.output(f.f.Output)
		r.ov.stale = r.ov.stale || r.ov.shown
		return
	}
	switch d.Type {
	case "resized", "repaint":
		if d.Cols > 0 && d.Rows > 0 {
			r.view.resized(int(d.Cols), int(d.Rows))
			r.ov.stale = r.ov.stale || r.ov.shown
		}
	case "keyboard":
		r.holder, r.holderConn = d.Holder, d.HolderConn
		if r.holds() {
			r.askedAt = time.Time{}
		}
	case "error":
		r.say("rota: " + d.Message)
	case "claim_request":
		if r.phase == runLive {
			r.note("rota: "+OneLine(d.By)+" asks for the keyboard; they may take it in "+actionsSecs(forceAfter), now)
		}
	case "exit":
		r.end(Ended, d.Code, nil)
	}
	// gap, viewers, pong, and whatever a newer daemon says, are nobody's
	// to show.
}

// say notes text, but not twice within noteFor: a daemon that says the
// same no to every key is said once.
func (r *runner) say(text string) {
	now := time.Now()
	for t, at := range r.said {
		if now.Sub(at) >= noteFor {
			delete(r.said, t)
		}
	}
	if _, ok := r.said[text]; ok {
		return
	}
	r.said[text] = now
	if r.phase == runLive {
		r.note(text, now)
	}
}

// note shows text over the window's last row for noteFor.
func (r *runner) note(text string, now time.Time) {
	r.lay(runOverlay{kind: runNote, text: text, until: now.Add(noteFor)})
}

// lay puts ov up in place of whatever overlay was. It is drawn once the
// view can repair what it covers.
func (r *runner) lay(ov runOverlay) {
	if r.ov.shown {
		r.wantRedraw = true
	}
	ov.on, ov.shown, ov.stale = true, false, false
	r.ov = ov
}

// unlay takes the overlay down, and has the screen drawn again beneath it.
func (r *runner) unlay() {
	if r.ov.shown {
		r.wantRedraw = true
	}
	r.ov = runOverlay{}
}

// overlay is the overlay drawn. drawRow and drawCard save the cursor with
// ESC 7 and put it back with ESC 8, which brings back the CLI's own pen
// with it, so what the CLI writes next while the overlay is up is in its
// colours. ESC 7 shares its slot with the CLI's own saves: a CLI that
// restores a cursor it saved earlier goes back to where the overlay was
// drawn, even after the overlay goes. And the redraw that takes an overlay
// down ends on the default pen, as Screen.Repaint does.
func (r *runner) overlay() []byte {
	v := r.view
	cols, rows := v.winCols, v.winRows
	switch r.ov.kind {
	case runCard:
		return drawCard(r.ov.lines, cols, rows)
	case runHint:
		return drawRow(rows, hintRow(r.key, cols), cols)
	}
	return drawRow(rows, r.ov.text, cols)
}

// onOpened takes what an open came to.
func (r *runner) onOpened(op runOpened, now time.Time) {
	if r.phase == runRedialing && op.gen == r.gen {
		r.dialling = false
	}
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
	case runOpening:
		r.sess = op.sess
		r.end(Refused, 0, op.err)
	case runSwitching:
		r.switchFailed(op, now)
	case runRedialing:
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
		r.drop()
		if r.view != nil {
			r.view.close()
			r.view = nil
			r.write(runPutBack())
		}
	}
	r.ln, r.hello, r.sess, r.id = l, l.Hello(), sess, sess.ID
	r.cur = linkHold(l, r.fail)
	r.held = append(r.held, r.cur)
	r.holder, r.holderConn = r.hello.Holder, r.hello.HolderConn
	r.askedAt = time.Time{}
	r.ov, r.wantRedraw, r.wantWindow, r.eating = runOverlay{}, false, false, false
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
	if r.o.Teach != nil {
		r.lay(runOverlay{kind: runCard, lines: helpCard(key, true), key: true, teach: true})
	}
	if early := r.early; len(early) > 0 {
		r.early = nil
		r.onInput(early, nil, now)
	}
}

// lose takes a link that failed: a redial when there is one to make, and
// otherwise the end.
func (r *runner) lose(err error, now time.Time) {
	if r.phase == runLeaving {
		r.end(Detached, 0, nil)
		return
	}
	if r.o.Redial == nil {
		r.end(Lost, 0, err)
		return
	}
	r.drop()
	r.phase = runRedialing
	r.redialStart, r.redialNext, r.lastErr, r.dialling = now, now, err, false
	r.lay(runOverlay{kind: runNote, text: "rota: the rota daemon went away; waiting for it (" + KeyName(r.key) + " d leaves)"})
}

// drop closes the link, after whatever was asked of it before, and has
// whatever it still sends ignored.
func (r *runner) drop() {
	if r.ln != nil {
		r.cur.drop()
		r.held = slices.DeleteFunc(r.held, func(h *linkHeld) bool { return h.closed.Load() })
		r.ln, r.cur = nil, nil
	}
	r.gen++
}

// dial makes one redial: the daemon dialled, then the session opened. A
// session the daemon refuses is looked up: stopped, it has ended; anything
// else is the refusal.
func (r *runner) dial() {
	r.dialling = true
	gen, src, redial, id, watch := r.gen, r.src, r.o.Redial, r.id, r.o.Watch
	go func() {
		op := runOpened{gen: gen, src: src, at: time.Now()}
		defer r.guardOpen(&op)
		c, err := redial()
		if err != nil {
			op.err = err
			r.deliver(op)
			return
		}
		if rd, ok := src.(linkRedialer); ok {
			op.src = rd.redial(c)
		}
		op.ln, op.err = op.src.open(id, watch)
		op.sess = runFind(op.src, id)
		if op.err != nil && runRefusal(op.err) != nil {
			if e := op.sess.Ended; e != nil {
				code := -1
				if op.sess.Exit != nil {
					code = *op.sess.Exit
				}
				op.final = &Result{Why: Ended, Code: code}
			} else {
				op.final = &Result{Why: Refused, Err: op.err}
			}
		}
		r.deliver(op)
	}()
}
