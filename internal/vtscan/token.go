package vtscan

// maxSeq is the most of one open sequence either direction keeps. Nothing
// either direction acts on comes near it; a sixel image or a pasted blob
// does, and is not kept.
const maxSeq = 4096

// The tokenizer's states.
const (
	stGround     uint8 = iota
	stEsc              // after ESC, perhaps with intermediates
	stCSI              // after ESC [
	stStr              // inside an OSC, DCS, APC, PM or SOS string
	stStrEsc           // an ESC inside a string, which may be the first half of ST
	stEscPending       // a string just ended at an ESC, which begins the next sequence
)

// What one byte did. tokNone means it went into an open sequence.
const (
	tokNone uint8 = iota
	tokText       // a byte in ground: printable, DEL or part of a rune
	tokCtrl       // a C0 control, acted on where it stands
	tokEsc        // ESC, its intermediates and a final byte
	tokCSI        // a whole control sequence
	tokStr        // a whole string; term says how it ended
	tokCut        // an open sequence ended by a byte outside its grammar
)

// How a string ended.
const (
	termBEL uint8 = iota + 1
	termST
	termESC // an ESC not followed by a backslash, which ends a string all the same
)

// tokenizer is the parser both directions share. step reads one byte; when
// it returns again, that byte ended what was open and must be read once more
// from the state it left. seq holds the open sequence from its ESC, and after
// a token it holds that token's sequence until the next ESC begins another.
type tokenizer struct {
	keys bool // reading typing: anything outside a sequence's grammar ends it
	st   uint8
	kind byte  // for strings: ']' OSC, 'P' DCS, '_' APC, '^' PM, 'X' SOS
	term uint8 // how the last string ended
	seq  []byte
	over bool // the open sequence outgrew maxSeq and seq stopped growing
}

func (t *tokenizer) begin() {
	t.seq = append(t.seq[:0], 0x1b)
	t.over = false
	t.st = stEsc
}

func (t *tokenizer) push(b byte) {
	if len(t.seq) < maxSeq {
		t.seq = append(t.seq, b)
	} else {
		t.over = true
	}
}

func (t *tokenizer) step(b byte) (tok uint8, again bool) {
	switch t.st {
	case stGround:
		switch {
		case b == 0x1b:
			t.begin()
			return tokNone, false
		case b < 0x20:
			return tokCtrl, false
		}
		return tokText, false
	case stEscPending:
		t.begin()
		return t.step(b)
	case stEsc:
		switch {
		case b >= 0x20 && b <= 0x2f:
			t.push(b)
			return tokNone, false
		case b >= 0x30 && b <= 0x7e:
			t.push(b)
			if len(t.seq) == 2 {
				switch b {
				case '[':
					t.st = stCSI
					return tokNone, false
				case ']', 'P', '_', '^', 'X':
					t.st, t.kind = stStr, b
					return tokNone, false
				}
			}
			t.st = stGround
			return tokEsc, false
		}
		return t.stray(b)
	case stCSI:
		switch {
		case b >= 0x20 && b <= 0x3f:
			t.push(b)
			return tokNone, false
		case b >= 0x40 && b <= 0x7e:
			t.push(b)
			t.st = stGround
			return tokCSI, false
		}
		return t.stray(b)
	case stStr:
		switch {
		case b == 0x07 && t.kind == ']':
			t.push(b)
			t.st, t.term = stGround, termBEL
			return tokStr, false
		case b == 0x1b:
			t.push(b)
			t.st = stStrEsc
			return tokNone, false
		case b == 0x18 || b == 0x1a:
			t.st = stGround
			return tokCut, true
		}
		t.push(b)
		return tokNone, false
	case stStrEsc:
		if b == '\\' {
			t.push(b)
			t.st, t.term = stGround, termST
			return tokStr, false
		}
		if !t.over {
			t.seq = t.seq[:len(t.seq)-1]
		}
		t.st, t.term = stEscPending, termESC
		return tokStr, true
	}
	return tokNone, false
}

// stray reads a byte that cannot continue an escape or control sequence. A
// terminal acts on a C0 control where it stands and ignores DEL; anything
// else, and anything at all in typing, ends the sequence and is read afresh.
func (t *tokenizer) stray(b byte) (uint8, bool) {
	if !t.keys {
		switch {
		case b == 0x7f:
			return tokNone, false
		case b < 0x20 && b != 0x1b && b != 0x18 && b != 0x1a:
			return tokCtrl, false
		}
	}
	t.st = stGround
	return tokCut, true
}

// body is the last string's content, without its introducer or terminator.
func (t *tokenizer) body() []byte {
	end := len(t.seq)
	switch t.term {
	case termBEL:
		end--
	case termST:
		end -= 2
	}
	return t.seq[2:end]
}

// csiParts is a control sequence taken apart.
type csiParts struct {
	marker byte // '<', '=', '>' or '?', or 0
	params []byte
	inter  []byte
	final  byte
}

// parseCSI takes apart what follows ESC [, and reports false when the bytes
// break the grammar, which a terminal reads to the end and then ignores.
func parseCSI(b []byte) (c csiParts, ok bool) {
	if len(b) == 0 {
		return c, false
	}
	c.final, b = b[len(b)-1], b[:len(b)-1]
	if len(b) > 0 && b[0] >= '<' && b[0] <= '?' {
		c.marker, b = b[0], b[1:]
	}
	i := 0
	for i < len(b) && b[i] >= '0' && b[i] <= ';' {
		i++
	}
	c.params, c.inter = b[:i], b[i:]
	for _, x := range c.inter {
		if x < 0x20 || x > 0x2f {
			return c, false
		}
	}
	return c, true
}

// maxParam is where a parameter stops growing, so no digit string overflows.
const maxParam = 65535

// nums reads parameters separated by semicolons; an empty one is -1. A colon
// is a sub-parameter, which nothing here takes, so it reports false.
func nums(p []byte) ([]int, bool) {
	var out []int
	n := -1
	for _, b := range p {
		switch {
		case b == ';':
			out = append(out, n)
			n = -1
		case b >= '0' && b <= '9':
			n = min(max(n, 0)*10+int(b-'0'), maxParam)
		default:
			return nil, false
		}
	}
	return append(out, n), true
}

// param is parameter i, or def when it is missing or empty.
func param(ps []int, i, def int) int {
	if i < len(ps) && ps[i] >= 0 {
		return ps[i]
	}
	return def
}

func isZero(ps []int) bool { return len(ps) == 1 && ps[0] <= 0 }

// small reads one to five digits and nothing else.
func small(b []byte) (int, bool) {
	if len(b) == 0 || len(b) > 5 {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
