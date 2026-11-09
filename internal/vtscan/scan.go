// Package vtscan reads the bytes of a terminal without being one.
//
// A held session's CLI writes to a pty nobody may be looking at, and asks it
// questions a terminal answers: what it is, its colours, where the cursor is.
// The Scanner reads that output as a terminal's parser would, answers the
// questions with fixed values, and keeps what a viewer arriving later needs:
// the modes, the title, and where the stream stands between two sequences.
// The ReplyFilter reads the other way, a viewer's typing, and drops the
// answers that viewer's own terminal sends back to questions it saw replayed,
// so a CLI is never answered twice and a reply is never typing.
//
// Both use one incremental tokenizer, with the states of a VT500 parser:
// ground, escape, control sequence and the strings OSC, DCS, APC, PM and
// SOS. It keeps at most 4 KiB of an open sequence, so no stream can make it
// grow. Everything here is the standard library alone, because the holder
// leans on it after its emulator has failed.
package vtscan

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Modes are the terminal modes a CLI turns on that a viewer's terminal must
// match to show the session and send it the keys it expects. Mouse is 0, 9,
// 1000, 1002 or 1003, MouseEncoding 0, 1005, 1006 or 1015, and Kitty the
// kitty keyboard flags of the screen in use.
type Modes struct {
	AltScreen      bool `json:"alt_screen,omitempty"`
	CursorKeys     bool `json:"cursor_keys,omitempty"`
	BracketedPaste bool `json:"bracketed_paste,omitempty"`
	Focus          bool `json:"focus,omitempty"`
	CursorHidden   bool `json:"cursor_hidden,omitempty"`
	Mouse          int  `json:"mouse,omitempty"`
	MouseEncoding  int  `json:"mouse_encoding,omitempty"`
	Kitty          int  `json:"kitty,omitempty"`
}

// Set gives the sequences that take a reset terminal to m. The alternate
// screen is left out, since entering it clears it and the caller decides when
// that happens. Values that are no mode of their kind write nothing.
func (m Modes) Set() []byte {
	var b []byte
	on := func(n int) {
		b = append(b, "\x1b[?"...)
		b = strconv.AppendInt(b, int64(n), 10)
		b = append(b, 'h')
	}
	if m.CursorKeys {
		on(1)
	}
	if m.BracketedPaste {
		on(2004)
	}
	if m.Focus {
		on(1004)
	}
	switch m.Mouse {
	case 9, 1000, 1002, 1003:
		on(m.Mouse)
	}
	switch m.MouseEncoding {
	case 1005, 1006, 1015:
		on(m.MouseEncoding)
	}
	if m.CursorHidden {
		b = append(b, "\x1b[?25l"...)
	}
	if m.Kitty > 0 && m.Kitty <= kittyMask {
		b = append(b, "\x1b[="...)
		b = strconv.AppendInt(b, int64(m.Kitty), 10)
		b = append(b, ";1u"...)
	}
	return b
}

// resetModes is every tracked mode off and the cursor shown, on the screen in
// use.
const resetModes = "\x1b[=0;1u\x1b[?1l\x1b[?2004l\x1b[?1004l" +
	"\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l" +
	"\x1b[?25h"

// ResetModes turns every tracked mode off and shows the cursor. It acts on
// the screen in use only and never switches screens. The kitty flags are
// each screen's own, so a caller that switches screens afterwards clears the
// new screen's flags with ESC [ = 0 ; 1 u, as ResetAll does: Set writes
// nothing for no flags.
func ResetModes() []byte { return []byte(resetModes) }

// ResetAll is ResetModes, then leaving the alternate screen, then clearing
// the main screen's kitty flags, which are its own. It is what a viewer's
// terminal needs when a session lets go of it.
func ResetAll() []byte { return []byte(resetModes + "\x1b[?1049l\x1b[=0;1u") }

// The kitty keyboard protocol keeps seven bits of flags and a stack of eight
// entries per screen, the one in use among them; these are kitty's numbers.
const (
	kittyMask  = 0x7f
	kittyDepth = 7
)

// kittyFlags is one screen's flags and the entries pushed beneath them.
type kittyFlags struct {
	flags int
	saved [kittyDepth]int
	n     int
}

func (k *kittyFlags) set(flags, mode int) {
	switch mode {
	case 1:
		k.flags = flags
	case 2:
		k.flags |= flags
	case 3:
		k.flags &^= flags
	}
}

func (k *kittyFlags) push(flags int) {
	if k.n == kittyDepth {
		copy(k.saved[:], k.saved[1:])
		k.n--
	}
	k.saved[k.n] = k.flags
	k.n++
	k.flags = flags
}

func (k *kittyFlags) pop(n int) {
	if n > k.n {
		*k = kittyFlags{}
		return
	}
	k.n -= n
	k.flags = k.saved[k.n]
}

// Result is what one read of a CLI's output held.
type Result struct {
	// Replies answer the queries in order; the holder writes them to the pty.
	Replies [][]byte
	// Ground is how many bytes of the read end between sequences and on a
	// whole rune, the last such point, or -1 if no such point falls inside it.
	// A read that begins in ground has the point 0.
	Ground int
	// CPR marks each cursor position query, left for the emulator to answer.
	CPR []CPR
	// Events are titles, bells and notifications, in order. A burst of bells
	// in one read is one bell.
	Events []Event
	// ModesChanged says the modes after the read differ from those before.
	ModesChanged bool
}

// CPR marks a CSI 6 n, or with Private a CSI ? 6 n, ending at byte End of
// the read, so the emulator can answer it with the cursor as of that byte.
type CPR struct {
	End     int
	Private bool
}

// EventKind says what an Event is.
type EventKind uint8

// The kinds of Event.
const (
	EventTitle  EventKind = iota + 1 // OSC 0 or 2 set the window title to Text
	EventBell                        // a BEL outside a string
	EventNotify                      // OSC 9 or OSC 777 asked for a notification saying Text
)

// Event is something the CLI said about itself rather than drew.
type Event struct {
	Kind EventKind
	Text string
}

// Scanner reads a CLI's output. It is not safe for concurrent use; the holder
// feeds it from the one goroutine that reads the pty.
type Scanner struct {
	t     tokenizer
	rn    int // bytes of an unfinished rune, in rbuf
	rbuf  [utf8.UTFMax]byte
	m     Modes
	kitty [2]kittyFlags // main screen, alternate screen
	title string
	rang  bool // a bell is already among this read's events
}

// NewScanner gives a Scanner for a terminal that has just been reset.
func NewScanner() *Scanner { return &Scanner{} }

// Modes are the modes as of the last byte read.
func (s *Scanner) Modes() Modes { return s.m }

// Title is the last window title the CLI set.
func (s *Scanner) Title() string { return s.title }

func (s *Scanner) ground() bool { return s.t.st == stGround && s.rn == 0 }

// Feed reads the next bytes of output.
func (s *Scanner) Feed(p []byte) Result {
	r := Result{Ground: -1}
	if s.ground() {
		r.Ground = 0
	}
	before := s.m
	s.rang = false
	for i := 0; i < len(p); i++ {
		b := p[i]
		if b < 0x80 {
			s.rn = 0
		}
		if s.t.st == stGround && b >= 0x20 && b < 0x7f {
			r.Ground = i + 1
			continue
		}
		for again := true; again; {
			var tok uint8
			tok, again = s.t.step(b)
			s.take(tok, b, i+1, &r)
		}
		if s.ground() {
			r.Ground = i + 1
		}
	}
	r.ModesChanged = s.m != before
	return r
}

// take acts on one token; end is where in the read its last byte ends.
func (s *Scanner) take(tok uint8, b byte, end int, r *Result) {
	switch tok {
	case tokText:
		if b >= 0x80 {
			s.rune(b)
		}
	case tokCtrl:
		if b == 0x07 && !s.rang {
			s.rang = true
			r.Events = append(r.Events, Event{Kind: EventBell})
		}
	case tokEsc:
		if string(s.t.seq) == "\x1bc" {
			s.m, s.kitty = Modes{}, [2]kittyFlags{}
		}
	case tokCSI:
		if !s.t.over {
			s.csi(s.t.seq[2:], end, r)
		}
	case tokStr:
		if !s.t.over {
			switch s.t.kind {
			case ']':
				s.osc(s.t.body(), r)
			case 'P':
				s.dcs(s.t.body(), r)
			}
		}
	}
}

// rune adds a byte at or above 0x80 to the rune being read, and lets go of
// every rune it completes, valid or not, with the standard decoder's rules.
func (s *Scanner) rune(b byte) {
	s.rbuf[s.rn] = b
	s.rn++
	for s.rn > 0 && utf8.FullRune(s.rbuf[:s.rn]) {
		_, n := utf8.DecodeRune(s.rbuf[:s.rn])
		copy(s.rbuf[:], s.rbuf[n:s.rn])
		s.rn -= n
	}
}

// csi acts on a control sequence. b is what follows ESC [.
func (s *Scanner) csi(b []byte, end int, r *Result) {
	c, ok := parseCSI(b)
	if !ok || strings.IndexByte("cqnphlu", c.final) < 0 {
		return // most sequences, colours and cursor moves, are none of rota's business
	}
	ps, ok := nums(c.params)
	if !ok {
		return
	}
	inter := string(c.inter)
	switch {
	case c.final == 'c' && inter == "" && isZero(ps):
		switch c.marker {
		case 0:
			r.reply([]byte(da1))
		case '>':
			r.reply([]byte(da2))
		}
	case c.final == 'q' && c.marker == '>' && inter == "" && isZero(ps):
		r.reply([]byte(xtversion))
	case c.final == 'n' && inter == "" && len(ps) == 1:
		switch {
		case c.marker == 0 && ps[0] == 5:
			r.reply([]byte(dsrOK))
		case (c.marker == 0 || c.marker == '?') && ps[0] == 6:
			r.CPR = append(r.CPR, CPR{End: end, Private: c.marker == '?'})
		}
	case c.final == 'p' && inter == "$" && (c.marker == 0 || c.marker == '?'):
		n, v := param(ps, 0, 0), 0
		if c.marker == '?' {
			v = s.mode(n)
		}
		r.reply(decrpm(n, c.marker == '?', v))
	case c.final == 'p' && inter == "!" && c.marker == 0:
		s.m.CursorKeys, s.m.CursorHidden = false, false
	case (c.final == 'h' || c.final == 'l') && c.marker == '?' && inter == "":
		for _, n := range ps {
			s.private(n, c.final == 'h')
		}
	case c.final == 'u' && inter == "":
		k := &s.kitty[s.screen()]
		switch c.marker {
		case '=':
			k.set(param(ps, 0, 0)&kittyMask, param(ps, 1, 1))
		case '>':
			k.push(param(ps, 0, 0) & kittyMask)
		case '<':
			k.pop(max(param(ps, 0, 1), 1))
		}
		s.m.Kitty = k.flags
	}
}

func (s *Scanner) screen() int {
	if s.m.AltScreen {
		return 1
	}
	return 0
}

// private acts on DECSET or DECRST of one mode. A reset of any mouse mode
// turns the mouse off, and a reset of an encoding undoes only that encoding,
// both as xterm does.
func (s *Scanner) private(n int, on bool) {
	switch n {
	case 1:
		s.m.CursorKeys = on
	case 25:
		s.m.CursorHidden = !on
	case 47, 1047, 1049:
		s.m.AltScreen = on
		s.m.Kitty = s.kitty[s.screen()].flags
	case 9, 1000, 1002, 1003:
		s.m.Mouse = 0
		if on {
			s.m.Mouse = n
		}
	case 1004:
		s.m.Focus = on
	case 1005, 1006, 1015:
		if on {
			s.m.MouseEncoding = n
		} else if s.m.MouseEncoding == n {
			s.m.MouseEncoding = 0
		}
	case 2004:
		s.m.BracketedPaste = on
	}
}

// mode is DECRPM's answer for a private mode: 1 set, 2 reset, 0 not one
// rota tracks.
func (s *Scanner) mode(n int) int {
	var on bool
	switch n {
	case 1:
		on = s.m.CursorKeys
	case 25:
		on = !s.m.CursorHidden
	case 47, 1047, 1049:
		on = s.m.AltScreen
	case 9, 1000, 1002, 1003:
		on = s.m.Mouse == n
	case 1004:
		on = s.m.Focus
	case 1005, 1006, 1015:
		on = s.m.MouseEncoding == n
	case 2004:
		on = s.m.BracketedPaste
	default:
		return 0
	}
	if on {
		return 1
	}
	return 2
}

// osc acts on an OSC string: a title, a notification or a colour query.
func (s *Scanner) osc(body []byte, r *Result) {
	ps, pt, found := bytes.Cut(body, []byte{';'})
	if !found {
		return
	}
	term := s.term()
	switch string(ps) {
	case "0", "2":
		s.title = text(pt)
		r.Events = append(r.Events, Event{Kind: EventTitle, Text: s.title})
	case "4":
		items := bytes.Split(pt, []byte{';'})
		for i := 0; i+1 < len(items); i += 2 {
			if n, ok := small(items[i]); ok && n < 256 && string(items[i+1]) == "?" {
				r.reply(color("4;"+strconv.Itoa(n), xtermColor(n), term))
			}
		}
	case "9":
		// ConEmu numbers its OSC 9 commands, a progress bar among them;
		// iTerm2's OSC 9 is a notification, which is any other text.
		i := 0
		for i < len(pt) && pt[i] >= '0' && pt[i] <= '9' {
			i++
		}
		if i > 0 && (i == len(pt) || pt[i] == ';') {
			return
		}
		s.notify(text(pt), r)
	case "777":
		rest, ok := bytes.CutPrefix(pt, []byte("notify;"))
		if !ok {
			return
		}
		title, body, _ := bytes.Cut(rest, []byte{';'})
		t, b := text(title), text(body)
		if t != "" && b != "" {
			t += ": "
		}
		s.notify(t+b, r)
	default:
		// OSC 10 to 19 query the dynamic colours, each item the next one.
		n, ok := small(ps)
		if !ok || n < 10 || n > 19 {
			return
		}
		for i, item := range bytes.Split(pt, []byte{';'}) {
			if c := n + i; string(item) == "?" && (c == 10 || c == 11) {
				r.reply(color(strconv.Itoa(c), dynamicColor(c), term))
			}
		}
	}
}

func (s *Scanner) notify(text string, r *Result) {
	if text != "" {
		r.Events = append(r.Events, Event{Kind: EventNotify, Text: text})
	}
}

// term ends a reply as the query was ended.
func (s *Scanner) term() string {
	if s.t.term == termBEL {
		return "\a"
	}
	return "\x1b\\"
}

// dcs acts on a DCS string, of which only XTGETTCAP is answered.
func (s *Scanner) dcs(body []byte, r *Result) {
	names, ok := bytes.CutPrefix(body, []byte("+q"))
	if !ok {
		return
	}
	for _, name := range bytes.Split(names, []byte{';'}) {
		if len(name) > 0 {
			r.reply(tcap(name))
		}
	}
}

func (r *Result) reply(b []byte) { r.Replies = append(r.Replies, b) }

// text is a string's content as words: control characters go, as a
// terminal drops them inside a string, and bad UTF-8 is replaced.
func text(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= 0x20 && c != 0x7f {
			out = append(out, c)
		}
	}
	return strings.ToValidUTF8(string(out), "�")
}

// CleanTitle strips one leading spinner or idle glyph and the space after it
// from a window title: Claude Code shows ✳ when idle and a spinner while it
// works. A glyph that runs into a word is part of the title and stays.
func CleanTitle(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	switch {
	case r == '✳', r == '✢', r == '✶', r == '✻', r == '✽', r == '·', r >= 0x2800 && r <= 0x28ff:
	default:
		return s
	}
	switch rest := s[n:]; {
	case rest == "":
		return ""
	case rest[0] == ' ':
		return rest[1:]
	}
	return s
}
