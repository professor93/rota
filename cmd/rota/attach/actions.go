package attach

import (
	"bytes"
	"errors"
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
			// nothing is typed but into the session shown.
			if r.phase == runLive && !r.o.Watch && r.ln != nil {
				_ = r.ln.Type(e.bytes)
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
		r.lay(runOverlay{kind: runCard, lines: helpCard(r.key, false), key: true})
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

// move shows the next live, attachable session, by when each started, or
// the one before it, round from the last to the first. Whatever is typed
// until its hello is dropped.
func (r *runner) move(dir int) {
	all, err := r.src.list()
	if err != nil {
		r.say("rota: " + err.Error())
		return
	}
	var live []client.Session
	for _, s := range all {
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
		next = live[(at+dir+len(live))%len(live)]
	case at < 0 && len(live) > 0:
		// This session is no longer in the list: the first or the last.
		next = live[0]
		if dir < 0 {
			next = live[len(live)-1]
		}
	default:
		r.note("rota: no other live session", time.Now())
		return
	}
	r.ch.reset()
	r.drop()
	r.view.close()
	r.view = nil
	r.ov, r.wantRedraw, r.wantWindow = runOverlay{}, false, false
	// The window is given back before the next session is shown, since a
	// crop's alternate screen and the last session's modes are not the
	// next one's.
	r.write(runPutBack())
	r.phase = runSwitching
	r.id = next.ID
	r.start(next.ID)
}

// holds says whether this connection holds the keyboard.
func (r *runner) holds() bool {
	return r.holderConn != "" && r.holderConn == r.hello.You.Conn
}

// claim asks for the keyboard, and at a k forceAfter or more after asking,
// takes it.
func (r *runner) claim(now time.Time) {
	switch {
	case r.o.Watch:
		r.note("rota: watching; this window cannot take the keyboard", now)
	case r.holds():
		r.note("rota: this window holds the keyboard", now)
	case r.askedAt.IsZero():
		_ = r.ln.Claim(false)
		r.askedAt = now
	case now.Sub(r.askedAt) >= forceAfter:
		_ = r.ln.Claim(true)
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
		_ = r.ln.Fit(actionsCells(cols), actionsCells(rows))
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

// overlay is the overlay drawn, then the cursor put back where the session
// has it. The cursor is moved there rather than restored with ESC 8, since
// ESC 7 and ESC 8 share one slot with the CLI's own saves.
func (r *runner) overlay() []byte {
	v := r.view
	cols, rows := v.winCols, v.winRows
	var b []byte
	switch r.ov.kind {
	case runCard:
		b = drawCard(r.ov.lines, cols, rows)
	case runHint:
		b = drawRow(rows, hintRow(r.key, cols), cols)
	default:
		b = drawRow(rows, r.ov.text, cols)
	}
	if b == nil {
		return nil
	}
	b = bytes.TrimPrefix(b, []byte("\x1b7"))
	b = bytes.TrimSuffix(b, []byte("\x1b8"))
	return append(b, r.cursor()...)
}

// cursor moves the window's cursor to where the session's is: the same
// cell passing through, and its place in the crop while cropped, which
// flush has hidden when it is out of the window.
func (r *runner) cursor() []byte {
	v := r.view
	row, col := v.screen.Cursor()
	if v.crop {
		x, y := viewport(v.sessCols, v.sessRows, v.winCols, v.winRows, row-1, col-1, v.x)
		row, col = row-y, col-x
		if row < 1 || row >= v.winRows || col < 1 || col > v.winCols {
			return nil
		}
	}
	return []byte("\x1b[" + strconv.Itoa(row) + ";" + strconv.Itoa(col) + "H")
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
	r.redialStart, r.redialNext, r.lastErr = now, now, err
	r.lay(runOverlay{kind: runNote, text: "rota: the rota daemon went away; waiting for it (" + KeyName(r.key) + " d leaves)"})
}

// drop closes the link, and has whatever it still sends ignored.
func (r *runner) drop() {
	if r.ln != nil {
		r.ln.Close()
		r.ln = nil
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
		var refused *client.Refusal
		if op.err != nil && errors.As(op.err, &refused) {
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
