package vtscan

import (
	"strings"
	"testing"
)

// answers is every query the holder answers, each with the one reply it
// gets. Each query holds exactly one question, so a test may cut it anywhere
// and still expect that reply once, after the last byte. FuzzScanner starts
// from these too.
var answers = []struct{ name, query, reply string }{
	{"DA1", "\x1b[c", "\x1b[?62;22c"},
	{"DA1 with its zero", "\x1b[0c", "\x1b[?62;22c"},
	{"DA2", "\x1b[>c", "\x1b[>1;10;0c"},
	{"XTVERSION", "\x1b[>q", "\x1bP>|xterm(390)\x1b\\"},
	{"DSR status", "\x1b[5n", "\x1b[0n"},
	{"foreground ended by BEL", "\x1b]10;?\a", "\x1b]10;rgb:d0d0/d0d0/d0d0\a"},
	{"foreground ended by ST", "\x1b]10;?\x1b\\", "\x1b]10;rgb:d0d0/d0d0/d0d0\x1b\\"},
	{"background", "\x1b]11;?\x1b\\", "\x1b]11;rgb:0000/0000/0000\x1b\\"},
	{"palette entry", "\x1b]4;1;?\a", "\x1b]4;1;rgb:cdcd/0000/0000\a"},
	{"XTGETTCAP TN", "\x1bP+q544e\x1b\\", "\x1bP1+r544e=787465726d2d323536636f6c6f72\x1b\\"},
	{"XTGETTCAP unknown", "\x1bP+q5a5a\x1b\\", "\x1bP0+r5a5a\x1b\\"},
	{"DECRQM of a mode that is off", "\x1b[?2004$p", "\x1b[?2004;2$y"},
	{"DECRQM of a mode that is on", "\x1b[?2004h\x1b[?2004$p", "\x1b[?2004;1$y"},
	{"DECRQM of an unknown mode", "\x1b[?9999$p", "\x1b[?9999;0$y"},
}

func joined(r Result) string {
	var b strings.Builder
	for _, x := range r.Replies {
		b.Write(x)
	}
	return b.String()
}

func TestAnswersAreFixed(t *testing.T) {
	for _, c := range answers {
		r := NewScanner().Feed([]byte(c.query))
		if len(r.Replies) != 1 || joined(r) != c.reply {
			t.Errorf("%s: %q answered %q, want %q once", c.name, c.query, r.Replies, c.reply)
		}
	}

	s := NewScanner()
	r := s.Feed([]byte("\x1b[?2004$p\x1b[?2004h\x1b[?2004$p"))
	if got, want := joined(r), "\x1b[?2004;2$y\x1b[?2004;1$y"; got != want {
		t.Errorf("DECRQM before and after setting 2004 answered %q, want %q", got, want)
	}

	// A query cut across two reads of the pty is answered once, after the
	// read that completes it, wherever the cut falls.
	for _, c := range answers {
		for i := 1; i < len(c.query); i++ {
			s := NewScanner()
			first, second := s.Feed([]byte(c.query[:i])), s.Feed([]byte(c.query[i:]))
			if len(first.Replies) != 0 || len(second.Replies) != 1 || joined(second) != c.reply {
				t.Errorf("%s cut at %d: answered %q then %q, want nothing then %q",
					c.name, i, first.Replies, second.Replies, c.reply)
			}
		}
	}

	// A capability name that is not hex is never echoed: the reply is typed
	// into the CLI, and whatever printed the query does not get to choose
	// bytes that are typed.
	r = NewScanner().Feed([]byte("\x1bP+q\rrm -rf ~\r;544e\x1b\\"))
	if got, want := joined(r), "\x1bP0+r\x1b\\\x1bP1+r544e=787465726d2d323536636f6c6f72\x1b\\"; got != want {
		t.Errorf("XTGETTCAP with a name that is not hex answered %q, want %q", got, want)
	}

	// A reply is its receiver's own: writing over one changes no later answer.
	fixed := "\x1b[c\x1b[>c\x1b[>q\x1b[5n"
	want := "\x1b[?62;22c\x1b[>1;10;0c\x1bP>|xterm(390)\x1b\\\x1b[0n"
	for _, x := range NewScanner().Feed([]byte(fixed)).Replies {
		for i := range x {
			x[i] = 'X'
		}
	}
	if got := joined(NewScanner().Feed([]byte(fixed))); got != want {
		t.Errorf("after a holder wrote over its replies, the fixed answers became %q, want %q", got, want)
	}

	// Plain output and sequences that ask nothing get nothing.
	r = NewScanner().Feed([]byte("hello \x1b[1;31mred\x1b[0m\r\n\x1b]0;title\a\x1b[=c\x1bP$qm\x1b\\"))
	if len(r.Replies) != 0 {
		t.Errorf("output without a query answered %q", r.Replies)
	}
}

func TestKittyQueryGoesUnanswered(t *testing.T) {
	r := NewScanner().Feed([]byte("\x1b[?u"))
	if len(r.Replies) != 0 || len(r.CPR) != 0 {
		t.Errorf("the kitty keyboard query got %q and %v, want no answer", r.Replies, r.CPR)
	}
}

func TestCPRIsLeftToTheEmulator(t *testing.T) {
	r := NewScanner().Feed([]byte("abc\x1b[6n"))
	if len(r.Replies) != 0 {
		t.Errorf("CPR was answered with %q; only the emulator knows the cursor", r.Replies)
	}
	if want := []CPR{{End: 7}}; !equalCPR(r.CPR, want) {
		t.Errorf("CPR = %v, want %v", r.CPR, want)
	}

	r = NewScanner().Feed([]byte("\x1b[?6nx\x1b[6n"))
	if want := []CPR{{End: 5, Private: true}, {End: 10}}; !equalCPR(r.CPR, want) {
		t.Errorf("CPR = %v, want %v", r.CPR, want)
	}

	for _, c := range []struct {
		row, col int
		private  bool
		want     string
	}{
		{3, 4, false, "\x1b[3;4R"},
		{24, 80, true, "\x1b[?24;80R"},
		{0, -2, false, "\x1b[1;1R"},
	} {
		if got := string(CPRReply(c.row, c.col, c.private)); got != c.want {
			t.Errorf("CPRReply(%d, %d, %v) = %q, want %q", c.row, c.col, c.private, got, c.want)
		}
	}
}

func equalCPR(a, b []CPR) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRepliesAreStrippedAndKeysAreNot(t *testing.T) {
	for _, c := range []struct{ name, in, out string }{
		{"DA1 reply between typing", "x\x1b[?62;22cy", "xy"},
		{"CPR", "\x1b[24;80R", ""},
		{"Shift+F3", "\x1b[1;2R", "\x1b[1;2R"},
		{"Up", "\x1b[A", "\x1b[A"},
		{"a lone Escape", "\x1b", "\x1b"},
		{"background reply", "\x1b]11;rgb:0000/0000/0000\x1b\\", ""},
		{"DECRPM", "\x1b[?2004;1$y", ""},
		{"kitty flags reply", "\x1b[?1u", ""},
		{"XTVERSION reply", "\x1bP>|xterm(390)\x1b\\", ""},
		{"XTGETTCAP reply", "\x1bP1+r544e=787465726d2d323536636f6c6f72\x1b\\", ""},

		{"Ctrl+F3", "\x1b[1;5R", "\x1b[1;5R"},
		{"DECXCPR", "\x1b[?24;80R", ""},
		{"DSR reply", "\x1b[0n", ""},
		{"DA2 reply", "\x1b[>1;10;0c", ""},
		{"palette reply", "\x1b]4;1;rgb:cdcd/0000/0000\a", ""},
		{"window size report", "\x1b[8;40;120t", ""},
		{"a reply then a key", "\x1b[?62;22c\x1b[A", "\x1b[A"},
		{"SGR mouse", "\x1b[<0;10;5M\x1b[<0;10;5m", "\x1b[<0;10;5M\x1b[<0;10;5m"},
		{"a paste", "\x1b[200~hi\x1b[201~", "\x1b[200~hi\x1b[201~"},
		{"a kitty key", "\x1b[97;5u", "\x1b[97;5u"},
		{"focus", "\x1b[I\x1b[O", "\x1b[I\x1b[O"},
		{"Shift+Tab", "\x1b[Z", "\x1b[Z"},
		{"rxvt Shift+Right", "\x1b[c", "\x1b[c"},
		{"Alt+Backspace", "\x1b\x7f", "\x1b\x7f"},
		{"Escape twice", "\x1b\x1b", "\x1b\x1b"},
		{"Escape then Ctrl+C", "\x1b\x03", "\x1b\x03"},
		{"Alt+é", "\x1bé", "\x1bé"},
		{"Alt+]", "\x1b]a", "\x1b]a"},
		{"Alt+] alone", "\x1b]", "\x1b]"},
		{"Alt+Shift+P alone", "\x1bP", "\x1bP"},
		{"Alt+_ alone", "\x1b_", "\x1b_"},
		{"Alt+] 1 Enter", "\x1b]1\r", "\x1b]1\r"},
		{"Alt+Shift+P 1 Ctrl+C", "\x1bP1\x03", "\x1bP1\x03"},
		{"Alt+] 1 Backspace", "\x1b]1\x7f", "\x1b]1\x7f"},
		{"Alt+_ G Tab", "\x1b_G\t", "\x1b_G\t"},
		{"Alt+Shift+P 1 Ctrl+G", "\x1bP1\a", "\x1bP1\a"},
		{"title report", "\x1b]lsome title\x1b\\", ""},
		{"icon label report", "\x1b]Licon\x1b\\", ""},
		{"Alt+Space", "\x1b ", "\x1b "},
		{"a CSI cut off", "\x1b[1;", "\x1b[1;"},
		{"typing", "héllo\r", "héllo\r"},
	} {
		var f ReplyFilter
		if got := f.Strip([]byte(c.in)); string(got) != c.out {
			t.Errorf("%s: Strip(%q) = %q, want %q", c.name, c.in, got, c.out)
		}
	}

	// Keys are never held behind the start of a string. One alone in its
	// frame is typed at once, like a lone Escape, and a control character or
	// DEL inside a held string, which no reply holds, shows it was typing:
	// the held bytes and that key are typed then, in order.
	for _, frames := range [][2][]string{
		{{"\x1b]", "1", "\r", "\x03"}, {"\x1b]", "1", "\r", "\x03"}},
		{{"\x1bP", "1", "\r", "\x03"}, {"\x1bP", "1", "\r", "\x03"}},
		{{"\x1b]1", "\r", "a"}, {"", "\x1b]1\r", "a"}},
		{{"\x1b]1", "\x03"}, {"", "\x1b]1\x03"}},
		{{"\x1bP1", "\x7f"}, {"", "\x1bP1\x7f"}},
		{{"\x1b]12", "3\r"}, {"", "\x1b]123\r"}},
		{{"\x1b]1\x1b", "\r"}, {"", "\x1b]1\x1b\r"}},
		{{"\x1b_G", "\x03"}, {"", "\x1b_G\x03"}},
	} {
		var f ReplyFilter
		for i, in := range frames[0] {
			if got := f.Strip([]byte(in)); string(got) != frames[1][i] {
				t.Errorf("frames %q: frame %d gave %q, want %q", frames[0], i, got, frames[1][i])
			}
		}
	}

	// An OSC reply split across two frames is held, then dropped whole.
	// A cut right after its ESC, or right after its ESC ], is the one place
	// it cannot be: a lone ESC at the end of a frame is the Escape key and
	// a bare ESC ] is Alt+], and neither is delayed.
	reply := "\x1b]11;rgb:0000/0000/0000\x1b\\"
	for i := 3; i < len(reply); i++ {
		var f ReplyFilter
		a, b := f.Strip([]byte(reply[:i])), f.Strip([]byte(reply[i:]))
		if len(a) != 0 || len(b) != 0 {
			t.Errorf("reply cut at %d: Strip gave %q then %q, want nothing both times", i, a, b)
		}
		if got := f.Strip([]byte("a")); string(got) != "a" {
			t.Errorf("after a reply cut at %d, typing a gave %q", i, got)
		}
	}

	// A DCS left open past 4 KiB is not a reply: it is held up to then and
	// typed from there, so nothing is lost.
	var f ReplyFilter
	open := "\x1bP1+r" + strings.Repeat("6", 3000)
	if got := f.Strip([]byte(open)); len(got) != 0 {
		t.Errorf("an open DCS under 4 KiB was not held: %d bytes passed", len(got))
	}
	more := strings.Repeat("6", 2000) + "q"
	if got := f.Strip([]byte(more)); string(got) != open+more {
		t.Errorf("an open DCS past 4 KiB gave %d bytes, want the %d held and typed", len(got), len(open+more))
	}
	if got := f.Strip([]byte("\x1b[?62;22cz")); string(got) != "z" {
		t.Errorf("after an overlong DCS, Strip gave %q, want %q", got, "z")
	}
	if got := f.Strip([]byte("\x1bP>|")); len(got) != 0 {
		t.Errorf("a DCS reply's start was not held: %q", got)
	}
	if got := f.Strip([]byte("xterm(390)\x1b\\k")); string(got) != "k" {
		t.Errorf("the rest of a held DCS reply gave %q, want %q", got, "k")
	}
}
