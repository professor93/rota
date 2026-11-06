package vtscan

import (
	"encoding/hex"
	"strconv"
)

// The fixed answers, those of a common modern terminal, the same whether or
// not anybody is attached: a VT220 with ANSI colour, xterm 390 by its own
// name.
var (
	da1       = []byte("\x1b[?62;22c")
	da2       = []byte("\x1b[>1;10;0c")
	xtversion = []byte("\x1bP>|xterm(390)\x1b\\")
	dsrOK     = []byte("\x1b[0n")
)

// decrpm answers DECRQM for mode n with value v: 1 set, 2 reset, 0 unknown.
// Only private modes rota tracks are known; an ANSI mode is always 0.
func decrpm(n int, private bool, v int) []byte {
	b := []byte("\x1b[")
	if private {
		b = append(b, '?')
	}
	b = strconv.AppendInt(b, int64(n), 10)
	b = append(b, ';')
	b = strconv.AppendInt(b, int64(v), 10)
	return append(b, "$y"...)
}

// color answers an OSC colour query: code is what follows ESC ], up to the
// colour, and each channel is written twice over as sixteen bits, as xterm
// does.
func color(code string, c [3]byte, term string) []byte {
	b := append([]byte("\x1b]"), code...)
	b = append(b, ";rgb:"...)
	for i, v := range c {
		if i > 0 {
			b = append(b, '/')
		}
		b = hex.AppendEncode(b, []byte{v, v})
	}
	return append(b, term...)
}

// tcap answers XTGETTCAP for one hex-encoded capability name. Only TN, Co
// and colors are known. A name that is not hex is not echoed: the answer is
// typed into the CLI, and whatever printed the query must not choose bytes
// that get typed.
func tcap(name []byte) []byte {
	decoded, err := hex.DecodeString(string(name))
	if err != nil {
		return []byte("\x1bP0+r\x1b\\")
	}
	var value string
	switch string(decoded) {
	case "TN":
		value = "xterm-256color"
	case "Co", "colors":
		value = "256"
	default:
		b := append([]byte("\x1bP0+r"), name...)
		return append(b, "\x1b\\"...)
	}
	b := append([]byte("\x1bP1+r"), name...)
	b = append(b, '=')
	b = hex.AppendEncode(b, []byte(value))
	return append(b, "\x1b\\"...)
}

// CPRReply answers a cursor position query with the cursor at row and col,
// counted from 1. Private answers CSI ? 6 n, as DECXCPR.
func CPRReply(row, col int, private bool) []byte {
	b := []byte("\x1b[")
	if private {
		b = append(b, '?')
	}
	b = strconv.AppendInt(b, int64(max(row, 1)), 10)
	b = append(b, ';')
	b = strconv.AppendInt(b, int64(max(col, 1)), 10)
	return append(b, 'R')
}

// ReplyFilter drops, from one viewer's typing, the answers that viewer's own
// terminal sends to queries it saw replayed from the CLI. The holder has
// answered them already. The zero value is ready; keep one per connection,
// since a reply can be split across its frames.
//
// Each frame is filtered on its own, except that an OSC, DCS or APC begun and
// not ended is held into the next frame, up to 4 KiB, past which it is typed
// after all. A lone ESC or a control sequence cut off at the end of a frame
// is typed as it is, so the Escape key is never delayed.
type ReplyFilter struct{ t tokenizer }

// Strip gives p without the replies in it. It does not keep p.
func (f *ReplyFilter) Strip(p []byte) []byte {
	t := &f.t
	t.keys = true
	out := make([]byte, 0, len(t.seq)+len(p))
	typed := func() {
		out = append(out, t.seq...)
		t.st = stGround
	}
	for _, b := range p {
		switch {
		case t.st != stGround && len(t.seq) >= maxSeq:
			typed()
		case t.st == stStr && len(t.seq) == 2 && !replyStart(t.kind, b):
			typed()
		}
		for again := true; again; {
			var tok uint8
			tok, again = t.step(b)
			switch tok {
			case tokText, tokCtrl:
				out = append(out, b)
			case tokEsc, tokCut:
				out = append(out, t.seq...)
			case tokCSI:
				if !isReply(t.seq[2:]) {
					out = append(out, t.seq...)
				}
			case tokStr:
				if t.term == termESC {
					out = append(out, t.seq...)
				}
			}
		}
	}
	switch t.st {
	case stEsc, stCSI:
		typed()
	case stStr, stStrEsc:
		if t.kind == '^' || t.kind == 'X' {
			typed()
		}
	}
	return out
}

// replyStart says whether a string whose content begins with b can be a
// terminal's reply: every OSC reply begins with its number, every DCS reply
// with a digit, '>' or '!', and a kitty graphics reply with G. Anything else
// is somebody typing Alt+] or Alt+P, and goes through at once.
func replyStart(kind, b byte) bool {
	switch kind {
	case ']':
		return b >= '0' && b <= '9'
	case 'P':
		return b >= '0' && b <= '?' || b == '!'
	case '_':
		return b == 'G'
	}
	return false
}

// isReply says whether a control sequence, given after its ESC [, has the
// shape of a terminal's reply rather than of a key. No key ends in c with a
// marker, in n, $y, ?u, t, x, ?S or >m. A CPR, CSI row;col R, is one, except
// CSI 1;2 R to CSI 1;16 R, which are F3 with modifiers and win.
func isReply(b []byte) bool {
	c, ok := parseCSI(b)
	if !ok {
		return false
	}
	inter := string(c.inter)
	if inter == "$" {
		return c.final == 'y'
	}
	if inter != "" {
		return false
	}
	switch c.final {
	case 'c':
		return c.marker == '?' || c.marker == '>'
	case 'n', 't':
		return true
	case 'x':
		return c.marker == 0
	case 'u', 'S':
		return c.marker == '?'
	case 'm':
		return c.marker == '>'
	case 'R':
		if c.marker == '?' {
			return true
		}
		ps, ok := nums(c.params)
		if c.marker != 0 || !ok || len(ps) != 2 || ps[0] < 0 || ps[1] < 0 {
			return false
		}
		return !(string(c.params[:2]) == "1;" && ps[1] >= 2 && ps[1] <= 16)
	}
	return false
}
