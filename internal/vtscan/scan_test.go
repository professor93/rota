package vtscan

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestModesFollowTheStream(t *testing.T) {
	s := NewScanner()
	r := s.Feed([]byte("\x1b[?1049h\x1b[?1h\x1b[?2004h\x1b[?1002h\x1b[?1006h\x1b[?1004h\x1b[=5;1u\x1b[?25l"))
	want := Modes{AltScreen: true, CursorKeys: true, BracketedPaste: true, Focus: true,
		CursorHidden: true, Mouse: 1002, MouseEncoding: 1006, Kitty: 5}
	if s.Modes() != want || !r.ModesChanged {
		t.Errorf("Modes = %+v (changed %v), want %+v (changed)", s.Modes(), r.ModesChanged, want)
	}
	if r := s.Feed([]byte("plain text")); r.ModesChanged {
		t.Error("plain text reported a change of modes")
	}
	if r := s.Feed([]byte("\x1bc")); s.Modes() != (Modes{}) || !r.ModesChanged {
		t.Errorf("after RIS Modes = %+v (changed %v), want none (changed)", s.Modes(), r.ModesChanged)
	}

	// The kitty flags are a stack: a pop brings back what the push hid.
	s.Feed([]byte("\x1b[=1u\x1b[>3u"))
	if got := s.Modes().Kitty; got != 3 {
		t.Errorf("after a push of 3 Kitty = %d", got)
	}
	s.Feed([]byte("\x1b[<u"))
	if got := s.Modes().Kitty; got != 1 {
		t.Errorf("after the pop Kitty = %d, want the 1 from before the push", got)
	}
	// Popping more than was pushed leaves no flags.
	s.Feed([]byte("\x1b[<5u"))
	if got := s.Modes().Kitty; got != 0 {
		t.Errorf("after popping everything Kitty = %d, want 0", got)
	}
	// Each screen has its own flags, as the kitty protocol has it, so a
	// program on the alternate screen never inherits the main screen's.
	s.Feed([]byte("\x1b[>1u\x1b[?1049h"))
	if got := s.Modes().Kitty; got != 0 {
		t.Errorf("on the alternate screen Kitty = %d, want 0", got)
	}
	s.Feed([]byte("\x1b[=2;1u\x1b[?1049l"))
	if got := s.Modes().Kitty; got != 1 {
		t.Errorf("back on the main screen Kitty = %d, want 1", got)
	}

	// A reset of any mouse mode turns the mouse off, as xterm does; a reset
	// of an encoding only undoes that encoding.
	s = NewScanner()
	s.Feed([]byte("\x1b[?1003h\x1b[?1006h\x1b[?1000l\x1b[?1015l"))
	if m := s.Modes(); m.Mouse != 0 || m.MouseEncoding != 1006 {
		t.Errorf("Mouse = %d, MouseEncoding = %d, want 0 and 1006", m.Mouse, m.MouseEncoding)
	}
	// DECSTR puts the cursor keys back and shows the cursor, and nothing more.
	s.Feed([]byte("\x1b[?1h\x1b[?25l\x1b[?2004h\x1b[!p"))
	if m := s.Modes(); m.CursorKeys || m.CursorHidden || !m.BracketedPaste {
		t.Errorf("after DECSTR Modes = %+v", m)
	}

	// Set takes a fresh terminal to the modes, and ResetAll takes them away.
	fresh := NewScanner()
	fresh.Feed(append([]byte("\x1b[?1049h"), want.Set()...))
	if fresh.Modes() != want {
		t.Errorf("Set() reproduced %+v, want %+v", fresh.Modes(), want)
	}
	if bytes.Contains(want.Set(), []byte("1049")) {
		t.Errorf("Set() touched the alternate screen: %q", want.Set())
	}
	// ResetModes takes the modes away and stays on the alternate screen;
	// ResetAll is the same and then leaves it.
	fresh.Feed([]byte("\x1b[>4u"))
	fresh.Feed(ResetModes())
	if fresh.Modes() != (Modes{AltScreen: true}) {
		t.Errorf("after ResetModes Modes = %+v, want the alternate screen alone", fresh.Modes())
	}
	if bytes.Contains(ResetModes(), []byte("1049")) || bytes.Contains(ResetModes(), []byte("?47")) {
		t.Errorf("ResetModes() touched the screen: %q", ResetModes())
	}
	if got, want := string(ResetAll()), string(ResetModes())+"\x1b[?1049l\x1b[=0;1u"; got != want {
		t.Errorf("ResetAll() = %q, want ResetModes(), ESC[?1049l and ESC[=0;1u: %q", got, want)
	}
	fresh.Feed(append([]byte("\x1b[?1049h"), want.Set()...))
	fresh.Feed(ResetAll())
	if fresh.Modes() != (Modes{}) {
		t.Errorf("after ResetAll Modes = %+v, want none", fresh.Modes())
	}
	// The kitty flags are per screen, so ResetAll clears the main screen's
	// too: flags a CLI pushed before it entered the alternate screen do not
	// outlive it, and neither screen keeps any.
	s = NewScanner()
	s.Feed([]byte("\x1b[>1u\x1b[?1049h\x1b[>4u"))
	s.Feed(ResetAll())
	if s.Modes() != (Modes{}) {
		t.Errorf("after main-screen flags, the alternate screen and ResetAll, Modes = %+v, want none", s.Modes())
	}
	if s.Feed([]byte("\x1b[?1049h")); s.Modes() != (Modes{AltScreen: true}) {
		t.Errorf("back on the alternate screen after ResetAll, Modes = %+v, want no flags", s.Modes())
	}
	if len(Modes{}.Set()) != 0 {
		t.Errorf("Modes{}.Set() = %q, want nothing", Modes{}.Set())
	}
	if got := (Modes{Mouse: 25, MouseEncoding: 1049, Kitty: -1}).Set(); len(got) != 0 {
		t.Errorf("Set() of modes that are no mouse mode wrote %q", got)
	}

	doc, _ := json.Marshal(want)
	if got := string(doc); got != `{"alt_screen":true,"cursor_keys":true,"bracketed_paste":true,"focus":true,"cursor_hidden":true,"mouse":1002,"mouse_encoding":1006,"kitty":5}` {
		t.Errorf("Modes as JSON = %s", got)
	}
	if doc, _ := json.Marshal(Modes{}); string(doc) != "{}" {
		t.Errorf("no modes as JSON = %s, want {}", doc)
	}
}

func TestGroundIsBetweenSequencesAndWholeRunes(t *testing.T) {
	if g := NewScanner().Feed([]byte("abc\x1b[3")).Ground; g != 3 {
		t.Errorf("abc ESC[3: Ground = %d, want 3", g)
	}

	s := NewScanner()
	if g := s.Feed([]byte("ok\xe2\x9c")).Ground; g != 2 {
		t.Errorf("ok and half a rune: Ground = %d, want 2", g)
	}
	if g := s.Feed([]byte("\xb3")).Ground; g != 1 {
		t.Errorf("the rest of the rune: Ground = %d, want 1", g)
	}

	// A read that starts and ends inside one sequence has no ground in it.
	s = NewScanner()
	if g := s.Feed([]byte("\x1b]0;ti")).Ground; g != 0 {
		t.Errorf("a title begun from ground: Ground = %d, want 0", g)
	}
	if g := s.Feed([]byte("tle")).Ground; g != -1 {
		t.Errorf("the middle of a title: Ground = %d, want -1", g)
	}
	if g := s.Feed([]byte("\a")).Ground; g != 1 {
		t.Errorf("the end of a title: Ground = %d, want 1", g)
	}

	// A byte that cannot continue a rune ends it there, as a decoder does.
	s = NewScanner()
	if g := s.Feed([]byte("\xe2x")).Ground; g != 2 {
		t.Errorf("a broken rune: Ground = %d, want 2", g)
	}
	if g := s.Feed(nil).Ground; g != 0 {
		t.Errorf("nothing, in ground: Ground = %d, want 0", g)
	}
}

func TestTitleBellAndNotify(t *testing.T) {
	s := NewScanner()
	if r := s.Feed([]byte("\x1b]0;work\a")); !equalEvents(r.Events, []Event{{Kind: EventTitle, Text: "work"}}) {
		t.Errorf("title: Events = %v", r.Events)
	}
	if s.Title() != "work" {
		t.Errorf("Title() = %q, want work", s.Title())
	}
	if r := s.Feed([]byte("\a")); !equalEvents(r.Events, []Event{{Kind: EventBell}}) {
		t.Errorf("bell: Events = %v", r.Events)
	}
	if r := s.Feed([]byte("\x1b]9;done\x1b\\")); !equalEvents(r.Events, []Event{{Kind: EventNotify, Text: "done"}}) {
		t.Errorf("OSC 9: Events = %v", r.Events)
	}
	if r := s.Feed([]byte("\x1b]777;notify;T;B\a")); !equalEvents(r.Events, []Event{{Kind: EventNotify, Text: "T: B"}}) {
		t.Errorf("OSC 777: Events = %v", r.Events)
	}

	// A burst of bells in one read is one bell.
	if r := s.Feed([]byte("\a\a\a")); !equalEvents(r.Events, []Event{{Kind: EventBell}}) {
		t.Errorf("three bells: Events = %v", r.Events)
	}
	// ConEmu's numbered OSC 9 commands, a progress bar among them, are not
	// notifications.
	if r := s.Feed([]byte("\x1b]9;4;1;50\a")); len(r.Events) != 0 {
		t.Errorf("OSC 9;4: Events = %v, want none", r.Events)
	}
	// Control characters never reach a title.
	s.Feed([]byte("\x1b]2;a\x01b\a"))
	if s.Title() != "ab" {
		t.Errorf("Title() = %q, want ab", s.Title())
	}

	for in, want := range map[string]string{
		"✳ Fix tests": "Fix tests",
		"⠋ Fix tests": "Fix tests",
		"· Fix tests": "Fix tests",
		"✳":           "",
		"Fix tests":   "Fix tests",
		"✳✳ Fix":      "✳✳ Fix",
		"":            "",
	} {
		if got := CleanTitle(in); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func equalEvents(a, b []Event) bool {
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

func FuzzScanner(f *testing.F) {
	for _, c := range answers {
		f.Add([]byte(c.query), uint16(len(c.query)/2))
	}
	for _, s := range []string{
		"\x1b[?u", "abc\x1b[6n", "\x1b[?6n",
		"\x1b[?1049h\x1b[?1h\x1b[?2004h\x1b[?1002h\x1b[?1006h\x1b[?1004h\x1b[=5;1u\x1b[?25l\x1bc",
		"\x1b[=1u\x1b[>3u\x1b[<u", "abc\x1b[3", "ok\xe2\x9c\xb3",
		"\x1b]0;work\a\a\x1b]9;done\x1b\\\x1b]777;notify;T;B\a",
		"x\x1b[?62;22cy\x1b[24;80R\x1b[1;2R\x1b[A\x1b",
		"\x1b]11;rgb:0000/0000/0000\x1b\\\x1b[?2004;1$y\x1b[?1u\x1bP>|xterm(390)\x1b\\",
		"\x1b]1\r\x03", "\x1bP1\x7f\x1b_G\t", "\x1b]1\x1b\r", "\x1b]ltitle\x1b\\\x1b[>1u\x1b[?1049h\x1b[>4u",
	} {
		f.Add([]byte(s), uint16(len(s)/2))
	}
	f.Fuzz(func(t *testing.T, p []byte, cut uint16) {
		whole := NewScanner()
		r := whole.Feed(p)
		checkResult(t, r, len(p))

		k := int(cut) % (len(p) + 1)
		split := NewScanner()
		r1 := split.Feed(p[:k])
		r2 := split.Feed(p[k:])
		checkResult(t, r1, k)
		checkResult(t, r2, len(p)-k)
		if joined(r) != joined(r1)+joined(r2) {
			t.Errorf("answers depend on where the reads fall: %q, but %q and %q", r.Replies, r1.Replies, r2.Replies)
		}
		if whole.Modes() != split.Modes() || whole.Title() != split.Title() {
			t.Errorf("state depends on where the reads fall: %+v %q, but %+v %q",
				whole.Modes(), whole.Title(), split.Modes(), split.Title())
		}

		var f ReplyFilter
		a, b := f.Strip(p[:k]), f.Strip(p[k:])
		out := append(a, b...)
		if len(out) > len(p) {
			t.Errorf("Strip gave %d bytes for %d", len(out), len(p))
		}
		// A key no reply holds, Enter, Ctrl+C or Backspace among them, is
		// typed in the frame it arrives in: never dropped, never held.
		if !bytes.Equal(keyControls(a), keyControls(p[:k])) || !bytes.Equal(keyControls(b), keyControls(p[k:])) {
			t.Errorf("Strip held or dropped a control key: %q and %q became %q and %q", p[:k], p[k:], a, b)
		}
		if !bytes.Contains(p, []byte{0x1b}) && !bytes.Equal(out, p) {
			t.Errorf("Strip changed input without an escape: %q became %q", p, out)
		}
	})
}

// keyControls is the C0 controls and DELs in p other than ESC and BEL, the
// keys no terminal's reply holds.
func keyControls(p []byte) []byte {
	var out []byte
	for _, b := range p {
		if b < 0x20 && b != 0x1b && b != 0x07 || b == 0x7f {
			out = append(out, b)
		}
	}
	return out
}

func checkResult(t *testing.T, r Result, n int) {
	t.Helper()
	if r.Ground < -1 || r.Ground > n {
		t.Errorf("Ground = %d for %d bytes", r.Ground, n)
	}
	for _, c := range r.CPR {
		if c.End < 1 || c.End > n {
			t.Errorf("CPR.End = %d for %d bytes", c.End, n)
		}
	}
}
