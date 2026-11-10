package attach

import (
	"bytes"
	"time"
)

// hintAfter is how long the chord waits for its second key before the hint
// row is drawn. holdFor is how long a lone ESC, or a CSI cut off at the end
// of a read, waits for the rest of itself.
var (
	hintAfter = 400 * time.Millisecond
	holdFor   = 25 * time.Millisecond
)

// action is what a chordEvent asks of the attach.
type action int

const (
	actType     action = iota // type bytes into the session
	actLeave                  // d: leave the session
	actNext                   // n: the next session
	actPrev                   // p: the previous session
	actKeyboard               // k: take the keyboard
	actFit                    // z: fit the session to this window
	actHelp                   // ?: show the keys
	actHint                   // no second key yet: draw the hint row
	actCancel                 // an unknown second key, dropped
)

// chordEvent is one thing the person's input means. bytes is set for
// actType alone, and never shares memory with a read's buffer.
type chordEvent struct {
	act   action
	bytes []byte
}

// chordMax is the most of an open escape sequence the chord keeps. Anything
// longer is typed as it came.
const chordMax = 64

// The states of the input decoder. A paste is a flag beside them, since
// inside one every byte is typed at once.
const (
	chordGround = iota
	chordEsc    // held: a lone ESC
	chordCSI    // held: ESC [ and its parameters so far
)

var (
	chordPasteStart = []byte("\x1b[200~")
	chordPasteEnd   = []byte("\x1b[201~")
)

// The kitty keyboard protocol's modifier bits, as mods-1 carries them, and
// the codepoints of the modifier keys themselves.
const (
	chordShift   = 1
	chordCtrl    = 4
	chordLocks   = 64 | 128 // caps lock, num lock
	chordModLow  = 57441    // left shift
	chordModHigh = 57452    // right meta
	chordRelease = 3
)

// chord reads the person's input for the attach's prefix key and the key
// after it. It is one incremental state machine: bytes go in as they are
// read, and events come out in input order. Only a trailing lone ESC or a
// cut-off CSI is ever held back, until the next read completes it or tick
// finds holdFor has passed.
type chord struct {
	key byte // the prefix as a control byte
	cp  int  // the prefix as a kitty or modifyOtherKeys codepoint, 0 if none

	state  int
	held   []byte
	heldAt time.Time

	paste   bool
	pasteAt int // bytes of chordPasteEnd matched so far

	on      bool // the prefix was read, and the second key is awaited
	armedAt time.Time
	hinted  bool

	skip int // UTF-8 continuation bytes of a dropped key still to drop

	out []chordEvent
}

func newChord(key byte) *chord {
	return &chord{key: key, cp: chordCodepoint(key)}
}

// chordCodepoint is the character that, with Ctrl, gives the control byte k.
func chordCodepoint(k byte) int {
	switch {
	case k >= 0x01 && k <= 0x1a:
		return int(k) + 'a' - 1
	case k >= 0x1c && k <= 0x1f: // \ ] ^ _
		return int(k) + 0x40
	}
	return 0
}

// feed reads p, which may be any part of the input, and says what it means.
func (c *chord) feed(p []byte, now time.Time) []chordEvent {
	for _, b := range p {
		c.step(b, now)
	}
	if len(p) > 0 && c.state != chordGround {
		c.heldAt = now
	}
	return c.take()
}

// tick lets held bytes go once holdFor has passed, and gives the hint once
// the chord has waited hintAfter, each in the order of its deadline.
func (c *chord) tick(now time.Time) []chordEvent {
	for {
		at, hold, ok := c.next()
		if !ok || now.Before(at) {
			break
		}
		if hold {
			c.flush()
		} else {
			c.hinted = true
			c.emit(actHint)
		}
	}
	return c.take()
}

// due is the next time tick has something to do.
func (c *chord) due() (time.Time, bool) {
	at, _, ok := c.next()
	return at, ok
}

// armed says whether the prefix was read and the second key is awaited.
func (c *chord) armed() bool { return c.on }

// reset forgets held bytes and the armed chord, for a switch.
func (c *chord) reset() {
	*c = chord{key: c.key, cp: c.cp, held: c.held[:0]}
}

func (c *chord) next() (at time.Time, hold, ok bool) {
	if c.state != chordGround {
		at, hold, ok = c.heldAt.Add(holdFor), true, true
	}
	if c.on && !c.hinted {
		if h := c.armedAt.Add(hintAfter); !ok || h.Before(at) {
			at, hold, ok = h, false, true
		}
	}
	return at, hold, ok
}

func (c *chord) take() []chordEvent {
	out := c.out
	c.out = nil
	return out
}

func (c *chord) emit(a action) { c.out = append(c.out, chordEvent{act: a}) }

// typed adds b to the input order, joined to the typed bytes before it.
func (c *chord) typed(b []byte) {
	if n := len(c.out); n > 0 && c.out[n-1].act == actType {
		c.out[n-1].bytes = append(c.out[n-1].bytes, b...)
		return
	}
	c.out = append(c.out, chordEvent{act: actType, bytes: append([]byte(nil), b...)})
}

func (c *chord) arm(now time.Time) {
	c.on, c.armedAt, c.hinted = true, now, false
}

func (c *chord) disarm() { c.on, c.hinted = false, false }

// done ends the chord with a.
func (c *chord) done(a action) {
	c.disarm()
	c.emit(a)
}

// drop ends the chord on an unknown key, lead, and drops the rest of its
// character when it is UTF-8.
func (c *chord) drop(lead byte) {
	c.done(actCancel)
	switch {
	case lead >= 0xf0 && lead <= 0xf7:
		c.skip = 3
	case lead >= 0xe0:
		c.skip = 2
	case lead >= 0xc0:
		c.skip = 1
	}
}

func (c *chord) step(b byte, now time.Time) {
	if c.paste {
		c.typed([]byte{b})
		switch {
		case b == chordPasteEnd[c.pasteAt]:
			if c.pasteAt++; c.pasteAt == len(chordPasteEnd) {
				c.paste, c.pasteAt = false, 0
			}
		case b == 0x1b:
			c.pasteAt = 1
		default:
			c.pasteAt = 0
		}
		return
	}
	switch c.state {
	case chordEsc:
		switch b {
		case '[':
			c.held = append(c.held, b)
			c.state = chordCSI
		case 0x1b, c.key:
			c.state, c.held = chordGround, c.held[:0]
			c.loneEsc()
			c.step(b, now)
		default:
			c.state, c.held = chordGround, c.held[:0]
			if c.on {
				c.drop(b) // alt and a key
			} else {
				c.typed([]byte{0x1b, b})
			}
		}
	case chordCSI:
		switch {
		case b >= 0x40 && b <= 0x7e:
			seq := append(c.held, b)
			c.state, c.held = chordGround, c.held[:0]
			c.csi(seq, now)
		case b >= 0x20 && b <= 0x3f && len(c.held) < chordMax:
			c.held = append(c.held, b)
		default: // too long, or broken by a byte no CSI holds
			c.noise()
			c.step(b, now)
		}
	default:
		c.ground(b, now)
	}
}

func (c *chord) ground(b byte, now time.Time) {
	if c.skip > 0 {
		if b&0xc0 == 0x80 {
			c.skip--
			return
		}
		c.skip = 0
	}
	switch {
	case b == 0x1b:
		c.state, c.held = chordEsc, append(c.held[:0], b)
	case !c.on && b == c.key:
		c.arm(now)
	case !c.on:
		c.typed([]byte{b})
	case b == c.key:
		c.disarm()
		c.typed([]byte{b})
	default:
		if a := chordSecond(b); a != actCancel {
			c.done(a)
		} else {
			c.drop(b)
		}
	}
}

// loneEsc is the ESC key on its own.
func (c *chord) loneEsc() {
	if c.on {
		c.done(actCancel)
		return
	}
	c.typed([]byte{0x1b})
}

// noise lets held bytes go as they came: a CSI cut short, broken or too
// long. It is not a key, so it ends a chord and is typed.
func (c *chord) noise() {
	held := c.held
	c.state, c.held = chordGround, c.held[:0]
	if c.on {
		c.done(actCancel)
	}
	c.typed(held)
}

// flush lets the held bytes go once holdFor has passed.
func (c *chord) flush() {
	if c.state == chordEsc {
		c.state, c.held = chordGround, c.held[:0]
		c.loneEsc()
		return
	}
	c.noise()
}

// csi takes one complete CSI sequence.
func (c *chord) csi(seq []byte, now time.Time) {
	if bytes.Equal(seq, chordPasteStart) {
		if c.on {
			c.done(actCancel)
		}
		c.typed(seq)
		c.paste, c.pasteAt = true, 0
		return
	}
	k, ok := chordParse(seq)
	if !c.on {
		if ok && c.prefix(k) {
			c.arm(now)
			return
		}
		c.typed(seq)
		return
	}
	switch {
	case ok && (k.event == chordRelease || k.cp >= chordModLow && k.cp <= chordModHigh):
		// A release, or a modifier key on its own, is not the second key.
	case ok && c.prefix(k):
		c.disarm()
		c.typed(seq)
	case ok && chordSecondKey(k) != actCancel:
		c.done(chordSecondKey(k))
	default:
		c.done(actCancel)
	}
}

// prefix says whether k is the chord's key pressed or repeated with Ctrl.
func (c *chord) prefix(k chordKey) bool {
	if k.mods&chordCtrl == 0 || k.event != 1 && k.event != 2 || c.cp == 0 {
		return false
	}
	switch {
	case k.cp == c.cp:
		return true
	case c.cp == '^' && k.cp == '6', c.cp == '_' && k.cp == '-':
		return k.mods&chordShift != 0
	}
	return false
}

// chordSecond is what the plain byte b means as the second key.
func chordSecond(b byte) action {
	switch {
	case b == '?':
		return actHelp
	case b >= 0x01 && b <= 0x1a:
		b += 'a' - 1
	case b >= 'A' && b <= 'Z':
		b += 'a' - 'A'
	}
	return chordLetter(int(b))
}

// chordSecondKey is what the kitty or modifyOtherKeys key k means as the
// second key: unmodified, with Shift, or with Ctrl.
func chordSecondKey(k chordKey) action {
	m := k.mods &^ chordLocks
	if m&^chordShift == 0 && (k.cp == '?' || k.shifted == '?') {
		return actHelp
	}
	cp := k.cp
	if cp >= 'A' && cp <= 'Z' {
		cp += 'a' - 'A'
	}
	if m&^chordShift == 0 || m&chordCtrl != 0 && m&^(chordCtrl|chordShift) == 0 {
		return chordLetter(cp)
	}
	return actCancel
}

func chordLetter(cp int) action {
	switch cp {
	case 'd':
		return actLeave
	case 'n':
		return actNext
	case 'p':
		return actPrev
	case 'k':
		return actKeyboard
	case 'z':
		return actFit
	}
	return actCancel
}

// chordKey is a key from a kitty CSI-u or a modifyOtherKeys sequence. mods
// is the modifier bits (the sent value less one), and event is 1 for a
// press, 2 for a repeat and 3 for a release.
type chordKey struct {
	cp, shifted, mods, event int
}

// chordParse reads ESC [ cp[:shifted[:base]] [; mods[:event] [; text]] u
// and ESC [ 27 ; mods ; cp ~.
func chordParse(seq []byte) (chordKey, bool) {
	final := seq[len(seq)-1]
	params := seq[2 : len(seq)-1]
	for _, b := range params {
		if (b < '0' || b > '9') && b != ';' && b != ':' {
			return chordKey{}, false
		}
	}
	k := chordKey{event: 1}
	mods := 1
	switch final {
	case 'u':
		first, rest, more := bytes.Cut(params, []byte{';'})
		cp, alts, _ := bytes.Cut(first, []byte{':'})
		var ok bool
		if k.cp, ok = chordNum(cp); !ok {
			return chordKey{}, false
		}
		if shifted, _, _ := bytes.Cut(alts, []byte{':'}); len(shifted) > 0 {
			k.shifted, _ = chordNum(shifted)
		}
		if more {
			field, _, _ := bytes.Cut(rest, []byte{';'}) // the text field is not needed
			m, ev, hasEv := bytes.Cut(field, []byte{':'})
			if len(m) > 0 {
				if mods, ok = chordNum(m); !ok {
					return chordKey{}, false
				}
			}
			if hasEv {
				if k.event, ok = chordNum(ev); !ok {
					return chordKey{}, false
				}
			}
		}
	case '~':
		if bytes.IndexByte(params, ':') >= 0 {
			return chordKey{}, false
		}
		f0, rest, _ := bytes.Cut(params, []byte{';'})
		f1, f2, ok2 := bytes.Cut(rest, []byte{';'})
		if n, ok := chordNum(f0); !ok || n != 27 || !ok2 || bytes.IndexByte(f2, ';') >= 0 {
			return chordKey{}, false
		}
		var ok bool
		if mods, ok = chordNum(f1); !ok {
			return chordKey{}, false
		}
		if k.cp, ok = chordNum(f2); !ok {
			return chordKey{}, false
		}
	default:
		return chordKey{}, false
	}
	if mods < 1 {
		return chordKey{}, false
	}
	k.mods = mods - 1
	return k, true
}

// chordNum reads a decimal number of at most a few million.
func chordNum(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n := 0
	for _, d := range b {
		if d < '0' || d > '9' {
			return 0, false
		}
		if n = n*10 + int(d-'0'); n > 1<<22 {
			return 0, false
		}
	}
	return n, true
}
