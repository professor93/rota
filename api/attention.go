package api

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/professor93/rota/internal/events"
	"github.com/professor93/rota/internal/holdwire"
)

// A held session that wants its person says so on the server's events, as
// session.attention, with why; one that has printed nothing for a while
// after printing something says that, as session.quiet. Nothing here
// changes how the CLI runs: it is read from what the holder already says
// (the CLI's registry, its title and its bells) and from when its output
// came, looked at every attendEvery by keepAttending.
//
// The reasons come in this order, and a look says at most one:
//
//   - the registry waiting for its person: permission when what it waits
//     for names a permission, question otherwise;
//   - the registry's status going from anything but idle to idle: done;
//   - with no registry that has said anything of its own: a bell, at most
//     one each attnBell, and one more as that window ends for what rang
//     within it;
//   - with no registry that has said a status other than idle: the title
//     going from a working glyph to ✳ and staying there attnTitle: title;
//   - with neither such a registry nor a title that speaks: quiet.
//
// A registry that has waited for something speaks for the bell; one that
// has said a status other than idle, which is when done can be said, speaks
// for the title and for quiet as well. One that has only ever said idle, as
// Claude Code 2.1.318's does, leaves the title to say that a turn ended,
// after a permission as before one. While a registry waits, the title is
// that wait's and says nothing of its own.
//
// Each thing is said once. A daemon that takes up a running session starts
// from what it finds, so a restart says nothing again. While somebody has
// typed into the session in the last attnTyping, nothing is said of either
// kind, and nothing is lost by it: what would have been said is said once
// the typing is that long ago.

const (
	// attnStart is how long after a session's output begins here that its
	// output is its start, or what a new link redraws, and not activity.
	attnStart = 10 * time.Second
	// attnFit is how long after a resize its output is the redraw.
	attnFit = 2 * time.Second
	// attnEcho is how long after a keystroke its output is the echo.
	attnEcho = 50 * time.Millisecond
	// attnTyping is how long after somebody typed into a session it says
	// nothing that wants them: they are there.
	attnTyping = 20 * time.Second
	// attnTitle is how long a title must stay idle to say so.
	attnTitle = 2 * time.Second
	// attnBell is how often a bell is said at most.
	attnBell = 30 * time.Second
)

// The words that follow a session's name, by reason (events.Sentence).
var attnWords = map[string]string{
	"permission": "is waiting for your permission",
	"question":   "is asking you something",
	"done":       "has finished and is waiting for you",
	"bell":       "rang its bell",
	"title":      "is idle again",
	"quiet":      "has gone quiet",
}

// attnQuietWords is what session.quiet says.
var attnQuietWords = "has printed nothing for " + strconv.Itoa(int(heldQuiet/time.Second)) + "s"

// attnIn is what one look at a session goes by. LastActive is its last
// output that was activity, zero for none; TypedAt the last time a viewer
// typed into it, zero for never. Registry is nil when the holder reports
// none. Bells is how many bells its CLI has rung. Adopted is a session this
// server took up rather than started, whose first look is its baseline.
type attnIn struct {
	Now, LastActive, TypedAt time.Time
	Registry                 *holdwire.Registry
	Title                    string
	Bells                    uint64
	Adopted                  bool
}

// attention is one session's latches: what has been seen and what has been
// said, so each thing is said once. Its zero value is a session nothing has
// been seen of. The heldTerm's mu guards it.
type attention struct {
	// looked is a first look taken.
	looked bool
	// asks is a registry that has waited for something, works one that has
	// said a status other than idle, and titled a title that has ever had a
	// glyph. Each speaks for the session in its way, and what they do not
	// say is not guessed at: either registry turns the bell off, works the
	// title too, and works or titled the quiet reason.
	asks, works, titled bool
	// asked is the kind of wait last said, "" once it stopped waiting.
	asked string
	// busy is a status other than idle seen since done was last said.
	busy bool
	// bells is how many bells have been weighed, and rang when one was last
	// said; owed is a ring weighed within attnBell of that, said when the
	// window ends.
	bells uint64
	rang  time.Time
	owed  bool
	// spun is a working glyph seen in the title since title was last said,
	// and idleSince when the title has been idle since, zero while not.
	spun      bool
	idleSince time.Time
	// quietOf is the activity a quiet was last said after.
	quietOf time.Time
}

// step is one look at a session, at in.Now: the reason it wants its
// person, "" for none, and whether it has just gone quiet. What it sees it
// keeps whatever it says, so a thing seen while it may not be said is said
// later rather than lost; what it says, and everything else that was due
// in the same look, is not said again.
func (a *attention) step(in attnIn) (reason string, quiet bool) {
	var reg holdwire.Registry
	if in.Registry != nil {
		reg = *in.Registry
	}
	if reg.Status != "" && reg.Status != "idle" {
		a.works, a.busy = true, true
	}
	if reg.WaitingFor != "" {
		a.asks = true
	}
	asked := attnKind(reg.WaitingFor)
	if asked == "" {
		a.asked = ""
	}
	switch attnGlyph(in.Title) {
	case attnWorking:
		a.titled, a.spun, a.idleSince = true, true, time.Time{}
	case attnIdle:
		a.titled = true
		if a.idleSince.IsZero() {
			a.idleSince = in.Now
		}
	default:
		a.idleSince = time.Time{}
	}
	if asked != "" {
		// While the registry waits, the title is that wait's prompt and no
		// signal of its own: the work after the answer arms it again.
		a.spun, a.idleSince = false, time.Time{}
	}

	if in.Adopted && !a.looked {
		// What a session taken up is doing already, the daemon before said.
		// What is under way goes on: a busy registry may still finish, a
		// working title still go idle.
		a.looked = true
		a.asked, a.bells, a.quietOf = asked, in.Bells, in.LastActive
		return "", false
	}
	a.looked = true
	if !in.TypedAt.IsZero() && in.Now.Sub(in.TypedAt) < attnTyping {
		return "", false
	}

	waiting := asked != "" && asked != a.asked
	done := a.busy && reg.Status == "idle"
	rings := !a.asks && !a.works && in.Bells > a.bells
	bell := (rings || a.owed) && !a.asks && !a.works && (a.rang.IsZero() || in.Now.Sub(a.rang) >= attnBell)
	title := !a.works && a.spun && !a.idleSince.IsZero() && in.Now.Sub(a.idleSince) >= attnTitle
	quiet = !in.LastActive.IsZero() && !in.LastActive.Equal(a.quietOf) && in.Now.Sub(in.LastActive) >= heldQuiet
	switch {
	case waiting:
		reason = asked
	case done:
		reason = "done"
	case bell:
		reason = "bell"
	case title:
		reason = "title"
	case quiet && !a.works && !a.titled:
		reason = "quiet"
	}
	if waiting {
		a.asked = asked
	}
	if done {
		a.busy = false
	}
	if rings {
		a.bells = in.Bells
	}
	// A ring within attnBell of the last one said is owed, and said once as
	// the window ends: a CLI ringing in a loop is said once each attnBell,
	// and a second ring that meant something is late, not lost.
	switch {
	case bell:
		a.rang, a.owed = in.Now, false
	case rings:
		a.owed = true
	}
	if title {
		a.spun, a.idleSince = false, time.Time{}
	}
	if quiet {
		a.quietOf = in.LastActive
	}
	return reason, quiet
}

// attnKind is what a registry's waitingFor asks of the person: permission
// when it names one, question for anything else, "" for nothing.
func attnKind(waitingFor string) string {
	switch {
	case waitingFor == "":
		return ""
	case strings.Contains(strings.ToLower(waitingFor), "permission"):
		return "permission"
	}
	return "question"
}

// The glyphs a title may begin with: a CLI at work (a braille spinner, or
// one of Claude Code's), a CLI idle (Claude Code's ✳), or neither.
const (
	attnNoGlyph = iota
	attnWorking
	attnIdle
)

// attnGlyph is which of the glyphs title begins with, by vtscan.CleanTitle's
// rule: one standing alone, or before a space. One that runs into a word is
// part of the word.
func attnGlyph(title string) int {
	r, n := utf8.DecodeRuneInString(title)
	if rest := title[n:]; rest != "" && rest[0] != ' ' {
		return attnNoGlyph
	}
	switch {
	case r == '✳':
		return attnIdle
	case r == '✢', r == '✶', r == '✻', r == '✽', r == '·', r >= 0x2800 && r <= 0x28ff:
		return attnWorking
	}
	return attnNoGlyph
}

// attnReportsOnly reports whether p is nothing but what a terminal says by
// itself rather than what somebody types: focus in and out (CSI I, CSI O)
// and mouse reports, X10 or UTF-8 (CSI M and three characters), SGR
// (CSI < n;n;n M or m) or urxvt (CSI n;n;n M). A viewer whose page sends
// only those is not typing, and puts nothing off.
func attnReportsOnly(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	for len(p) > 0 {
		n := attnReport(p)
		if n == 0 {
			return false
		}
		p = p[n:]
	}
	return true
}

// attnReport is the length of the report p begins with, 0 for none.
func attnReport(p []byte) int {
	if len(p) < 3 || p[0] != 0x1b || p[1] != '[' {
		return 0
	}
	switch p[2] {
	case 'I', 'O':
		return 3
	case 'M':
		n := 3
		for range 3 {
			if n >= len(p) {
				return 0
			}
			_, w := utf8.DecodeRune(p[n:])
			n += w
		}
		return n
	}
	i := 2
	sgr := p[i] == '<'
	if sgr {
		i++
	}
	for param := 1; ; param++ {
		start := i
		for i < len(p) && p[i] >= '0' && p[i] <= '9' {
			i++
		}
		if i == start || i == len(p) {
			return 0
		}
		if p[i] != ';' {
			if param != 3 {
				return 0
			}
			break
		}
		i++
	}
	if p[i] == 'M' || (sgr && p[i] == 'm') {
		return i + 1
	}
	return 0
}

// noteOutput weighs output at offset off, arriving at now, as activity or
// not: not in the first attnStart after the session's output began here,
// within attnFit of a resize, within attnEcho of a keystroke, nor below the
// offset adoption replays. A session started here begins at its first
// output, so its start window runs from its CLI's first bytes. The caller
// holds h.mu.
func (h *heldTerm) noteOutput(off int64, now time.Time) {
	if h.linkedAt.IsZero() {
		h.linkedAt = now
	}
	switch {
	case off < h.replayTo:
	case now.Sub(h.linkedAt) < attnStart:
	case !h.fitAt.IsZero() && now.Sub(h.fitAt) < attnFit:
	case !h.keyAt.IsZero() && now.Sub(h.keyAt) < attnEcho:
	default:
		h.activeAt = now
	}
}

// attend is one look at h's attention, at now: what it wants its person
// for, and whether it has gone quiet. Only a live session whose holder is
// linked is looked at, since what one that is not says may be old; and an
// adopted one only once its holder has said its state, so that its baseline
// is what the holder says rather than what its record guessed.
func (h *heldTerm) attend(now time.Time) (reason string, quiet bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != heldLive || h.conn == nil || (h.adopted && !h.stated) {
		return "", false
	}
	in := attnIn{Now: now, LastActive: h.activeAt, TypedAt: h.typedAt,
		Title: h.title, Bells: h.bells, Adopted: h.adopted}
	if h.reg != (holdwire.Registry{}) {
		reg := h.reg
		in.Registry = &reg
	}
	return h.attn.step(in)
}

// attendAll looks at every live held session once, at now, and says which
// of them want their person, and which have gone quiet. It is
// keepAttending's tick, and never a state frame's: quiet, an idle title
// held and typing put off all turn on time passing, which no frame says.
//
// The lock order: it takes each heldTerm's mu alone, and reads of its
// termSession only what never changes once the session is made (id, name,
// cwd, account). It never takes the session's mu while holding the
// heldTerm's, which elsewhere is taken the other way round.
func (s *Server) attendAll(now time.Time) {
	for _, ts := range s.everyTerminal() {
		h, ok := ts.back.(*heldTerm)
		if !ok || h.gone() {
			continue
		}
		reason, quiet := h.attend(now)
		if reason == "" && !quiet {
			continue
		}
		e := events.Event{Account: ts.account, Session: ts.id, Name: ts.eventName()}
		if reason != "" {
			at := e
			at.Type, at.Reason, at.Text = events.SessionAttention, reason, attnWords[reason]
			s.emit(at)
		}
		if quiet {
			e.Type, e.Text = events.SessionQuiet, attnQuietWords
			s.emit(e)
		}
	}
}
