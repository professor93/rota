package attach

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

var chordT0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func chordTy(s string) chordEvent { return chordEvent{act: actType, bytes: []byte(s)} }

func chordAct(a action) chordEvent { return chordEvent{act: a} }

var (
	chordLeave = chordAct(actLeave)
	chordNext  = chordAct(actNext)
	chordPrev  = chordAct(actPrev)
	chordKbd   = chordAct(actKeyboard)
	chordFit   = chordAct(actFit)
	chordHelp  = chordAct(actHelp)
	chordHint  = chordAct(actHint)
	chordCxl   = chordAct(actCancel)
)

func chordShow(evs []chordEvent) string {
	names := [...]string{"Type", "Leave", "Next", "Prev", "Keyboard", "Fit", "Help", "Hint", "Cancel"}
	var parts []string
	for _, e := range evs {
		n := fmt.Sprintf("action(%d)", e.act)
		if int(e.act) >= 0 && int(e.act) < len(names) {
			n = names[e.act]
		}
		if e.act == actType || e.bytes != nil {
			n += fmt.Sprintf(" %q", e.bytes)
		}
		parts = append(parts, n)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// chordJoin merges adjacent actType events, as a reader of the events would.
func chordJoin(evs []chordEvent) []chordEvent {
	var out []chordEvent
	for _, e := range evs {
		if e.act == actType && len(out) > 0 && out[len(out)-1].act == actType {
			last := &out[len(out)-1]
			last.bytes = append(append([]byte(nil), last.bytes...), e.bytes...)
			continue
		}
		out = append(out, e)
	}
	return out
}

func chordSame(a, b []chordEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].act != b[i].act || !bytes.Equal(a[i].bytes, b[i].bytes) {
			return false
		}
	}
	return true
}

type chordCase struct {
	name string
	key  byte
	in   string
	want []chordEvent
}

var chordCases = []chordCase{
	// The brief's table.
	{"typed around a chord", 0x1c, "ab\x1cdcd", []chordEvent{chordTy("ab"), chordLeave, chordTy("cd")}},
	{"the key twice", 0x1c, "\x1c\x1c", []chordEvent{chordTy("\x1c")}},
	{"kitty prefix", 0x1c, "\x1b[92;5u" + "d", []chordEvent{chordLeave}},
	{"kitty press and release", 0x1c, "\x1b[92;5:1u\x1b[92;5:3ud", []chordEvent{chordLeave}},
	{"modifyOtherKeys prefix", 0x1c, "\x1b[27;5;92~n", []chordEvent{chordNext}},
	{"kitty unmodified second key", 0x1c, "\x1c\x1b[100u", []chordEvent{chordLeave}},
	{"ctrl second key", 0x1c, "\x1c\x04", []chordEvent{chordLeave}},
	{"capital second key", 0x1c, "\x1cZ", []chordEvent{chordFit}},
	{"help", 0x1c, "\x1c?", []chordEvent{chordHelp}},
	{"unknown second key", 0x1c, "\x1cx", []chordEvent{chordCxl}},
	{"send through as it came", 0x1c, "\x1c\x1b[92;5u", []chordEvent{chordTy("\x1b[92;5u")}},
	{"another key", 0x1d, "\x1c\x1dd", []chordEvent{chordTy("\x1c"), chordLeave}},

	// Every second key.
	{"D", 0x1c, "\x1cD", []chordEvent{chordLeave}},
	{"n", 0x1c, "\x1cn", []chordEvent{chordNext}},
	{"N", 0x1c, "\x1cN", []chordEvent{chordNext}},
	{"^N", 0x1c, "\x1c\x0e", []chordEvent{chordNext}},
	{"p", 0x1c, "\x1cp", []chordEvent{chordPrev}},
	{"P", 0x1c, "\x1cP", []chordEvent{chordPrev}},
	{"^P", 0x1c, "\x1c\x10", []chordEvent{chordPrev}},
	{"k", 0x1c, "\x1ck", []chordEvent{chordKbd}},
	{"K", 0x1c, "\x1cK", []chordEvent{chordKbd}},
	{"^K", 0x1c, "\x1c\x0b", []chordEvent{chordKbd}},
	{"z", 0x1c, "\x1cz", []chordEvent{chordFit}},
	{"^Z", 0x1c, "\x1c\x1a", []chordEvent{chordFit}},
	{"kitty ;1u", 0x1c, "\x1c\x1b[100;1u", []chordEvent{chordLeave}},
	{"kitty ;1:1u", 0x1c, "\x1c\x1b[110;1:1u", []chordEvent{chordNext}},
	{"kitty shift", 0x1c, "\x1c\x1b[112;2u", []chordEvent{chordPrev}},
	{"kitty ctrl", 0x1c, "\x1c\x1b[107;5u", []chordEvent{chordKbd}},
	{"modifyOtherKeys ctrl", 0x1c, "\x1c\x1b[27;5;122~", []chordEvent{chordFit}},
	{"kitty 63", 0x1c, "\x1c\x1b[63u", []chordEvent{chordHelp}},
	{"kitty alt is not the letter", 0x1c, "\x1c\x1b[100;3u", []chordEvent{chordCxl}},
	{"kitty second key repeat", 0x1c, "\x1c\x1b[100;1:2u", []chordEvent{chordLeave}},

	// The prefix in its encodings.
	{"kitty repeat is a press", 0x1c, "\x1b[92;5:2uz", []chordEvent{chordFit}},
	{"kitty with num lock", 0x1c, "\x1b[92;133ud", []chordEvent{chordLeave}},
	{"kitty alternate keys", 0x1c, "\x1b[92:124;5ud", []chordEvent{chordLeave}},
	{"kitty without ctrl is typed", 0x1c, "\x1b[92u", []chordEvent{chordTy("\x1b[92u")}},
	{"kitty release alone is typed", 0x1c, "\x1b[92;5:3u", []chordEvent{chordTy("\x1b[92;5:3u")}},
	{"modifyOtherKeys without ctrl is typed", 0x1c, "\x1b[27;3;92~", []chordEvent{chordTy("\x1b[27;3;92~")}},
	{"modifyOtherKeys sent through", 0x1c, "\x1c\x1b[27;5;92~", []chordEvent{chordTy("\x1b[27;5;92~")}},
	{"kitty then byte sent through", 0x1c, "\x1b[92;5u\x1c", []chordEvent{chordTy("\x1c")}},
	{"modifier keys ignored while armed", 0x1c,
		"\x1b[57442;5u\x1b[92;5u\x1b[92;5:3u\x1b[57442;1:3u\x1b[100u",
		[]chordEvent{chordTy("\x1b[57442;5u"), chordLeave}},
	{"ctrl-^ as ctrl-shift-6", 0x1e, "\x1b[54;6ud", []chordEvent{chordLeave}},
	{"ctrl-6 without shift is typed", 0x1e, "\x1b[54;5u", []chordEvent{chordTy("\x1b[54;5u")}},
	{"ctrl-^ as 94", 0x1e, "\x1b[94;5un", []chordEvent{chordNext}},
	{"ctrl-_ as ctrl-shift-minus", 0x1f, "\x1b[45;6uk", []chordEvent{chordKbd}},
	{"ctrl-] as 93", 0x1d, "\x1b[93;5ud\x1d\x1d", []chordEvent{chordLeave, chordTy("\x1d")}},
	{"a letter key", 0x01, "\x1b[97;5ud\x01\x01", []chordEvent{chordLeave, chordTy("\x01")}},
	{"a letter key's own letter is the prefix", 0x04, "\x04\x04\x04d", []chordEvent{chordTy("\x04"), chordLeave}},

	// What passes, and what a chord drops.
	{"plain keys pass", 0x1c, "\x1b[A\x1bOB\x1bx\x1b[200", []chordEvent{chordTy("\x1b[A\x1bOB\x1bx"), chordTy("\x1b[200")}},
	{"an arrow ends the chord", 0x1c, "\x1c\x1b[Aa", []chordEvent{chordCxl, chordTy("a")}},
	{"an alt key ends the chord", 0x1c, "\x1c\x1bxy", []chordEvent{chordCxl, chordTy("y")}},
	{"a UTF-8 key is dropped whole", 0x1c, "\x1c\xc3\xa9a\x1c\xe2\x82\xacb", []chordEvent{chordCxl, chordTy("a"), chordCxl, chordTy("b")}},
	{"escape before the key", 0x1c, "\x1b\x1cd", []chordEvent{chordTy("\x1b"), chordLeave}},
	{"escape escape", 0x1c, "\x1b\x1b[A", []chordEvent{chordTy("\x1b\x1b[A")}},
	{"a terminal reply is typed while armed", 0x1c, "\x1c\x1b[?1ud", []chordEvent{chordTy("\x1b[?1u"), chordLeave}},
	{"a mouse report is typed while armed", 0x1c, "\x1c\x1b[<35;10;5Md", []chordEvent{chordTy("\x1b[<35;10;5M"), chordLeave}},
	{"focus in is typed while armed", 0x1c, "\x1c\x1b[Id", []chordEvent{chordTy("\x1b[I"), chordLeave}},
	{"focus out is typed while armed", 0x1c, "\x1c\x1b[Od", []chordEvent{chordTy("\x1b[O"), chordLeave}},
	{"an SS3 arrow is dropped whole", 0x1c, "\x1c\x1bOA", []chordEvent{chordCxl}},
	{"F1 is dropped whole", 0x1c, "\x1c\x1bOP", []chordEvent{chordCxl}},
	{"an SS3 key with a parameter is dropped whole", 0x1c, "\x1c\x1bO5P", []chordEvent{chordCxl}},
	{"alt-O then a control byte", 0x1c, "\x1c\x1bO\x03", []chordEvent{chordCxl, chordTy("\x03")}},
	{"alt-O then the key", 0x1c, "\x1c\x1bO\x1cd", []chordEvent{chordCxl, chordLeave}},
	{"alt-O alone", 0x1c, "\x1c\x1bO", []chordEvent{chordCxl}},
	{"an SS3 key is typed unarmed", 0x1c, "\x1bOP\x1bO", []chordEvent{chordTy("\x1bOP\x1bO")}},
	{"a Linux console F1 is dropped whole", 0x1c, "\x1c\x1b[[Aa", []chordEvent{chordCxl, chordTy("a")}},
	{"a Linux console F1 is typed unarmed", 0x1c, "\x1b[[A", []chordEvent{chordTy("\x1b[[A")}},
	{"kitty text field ?", 0x1c, "\x1c\x1b[47;2;63u", []chordEvent{chordHelp}},
	{"kitty shift-slash without text is unknown", 0x1c, "\x1c\x1b[47;2u", []chordEvent{chordCxl}},
	{"kitty base layout key", 0x1c, "\x1c\x1b[1074::100u", []chordEvent{chordLeave}},
	{"kitty base layout key with shift", 0x1c, "\x1c\x1b[1090:1058:110;2u", []chordEvent{chordNext}},
	{"kitty base layout prefix", 0x1c, "\x1b[1105::92;5ud", []chordEvent{chordLeave}},
	{"Ukrainian ctrl-ghe is the prefix", 0x1c, "\x1b[1169::92;5ud", []chordEvent{chordLeave}},
	{"Dvorak e is not d", 0x1c, "\x1c\x1b[101::100u", []chordEvent{chordCxl}},
	{"Dvorak t is not k", 0x1c, "\x1c\x1b[116::107u", []chordEvent{chordCxl}},
	{"QWERTZ y is not z", 0x1c, "\x1c\x1b[121::122u", []chordEvent{chordCxl}},
	{"AZERTY w is not z", 0x1c, "\x1c\x1b[119::122u", []chordEvent{chordCxl}},
	{"German ctrl-# is not the prefix", 0x1c, "\x1b[35::92;5ux", []chordEvent{chordTy("\x1b[35::92;5ux")}},
	{"French ctrl-* is not the prefix", 0x1c, "\x1b[42::92;5ud", []chordEvent{chordTy("\x1b[42::92;5ud")}},
	{"a held prefix's repeat is not sent through", 0x1c, "\x1b[92;5u\x1b[92;5:2ud", []chordEvent{chordLeave}},
	{"a held prefix's repeats are not sent through", 0x1c, "\x1b[92;5u\x1b[92;5:2u\x1b[92;5:2ud", []chordEvent{chordLeave}},
	{"level shifts and locks ignored while armed", 0x1c, "\x1c\x1b[57453u\x1b[57454u\x1b[57358u\x1b[57360;129ud", []chordEvent{chordLeave}},
	{"an unknown kitty event is not a key", 0x1c, "\x1c\x1b[100;1:9u", []chordEvent{chordCxl}},
	{"a broken CSI is typed", 0x1c, "\x1b[1\x03x", []chordEvent{chordTy("\x1b[1\x03x")}},
	{"a broken CSI ends the chord and is typed", 0x1c, "\x1c\x1b[1\x03", []chordEvent{chordCxl, chordTy("\x1b[1\x03")}},
	{"an overlong CSI is typed", 0x1c, "\x1b[" + strings.Repeat("1", 70) + "u", []chordEvent{chordTy("\x1b[" + strings.Repeat("1", 70) + "u")}},
	{"an overlong CSI ends the chord", 0x1c, "\x1c\x1b[" + strings.Repeat(";", 70) + "ud", []chordEvent{chordCxl, chordTy("\x1b[" + strings.Repeat(";", 70) + "ud")}},
	{"a 64-byte CSI is still read", 0x1c, "\x1b[" + strings.Repeat("0", 57) + "92;5u" + "d", []chordEvent{chordLeave}},

	// Bracketed paste.
	{"a paste is never a chord", 0x1c, "\x1b[200~a\x1cdb\x1b[201~\x1cd", []chordEvent{chordTy("\x1b[200~a\x1cdb\x1b[201~"), chordLeave}},
	{"a paste ends the chord and is typed", 0x1c, "\x1c\x1b[200~d\x1b[201~d", []chordEvent{chordCxl, chordTy("\x1b[200~d\x1b[201~d")}},
	{"a paste with escapes inside", 0x1c, "\x1b[200~\x1b[92;5u\x1b\x1b[201\x1b[201~\x1cn", []chordEvent{chordTy("\x1b[200~\x1b[92;5u\x1b\x1b[201\x1b[201~"), chordNext}},
}

func TestTheChordInEveryEncoding(t *testing.T) {
	for _, tc := range chordCases {
		c := newChord(tc.key)
		got := c.feed([]byte(tc.in), chordT0)
		got = append(got, c.tick(chordT0.Add(holdFor))...)
		if !chordSame(got, tc.want) {
			t.Errorf("%s: key %#x, feed(%q)\n got %s\nwant %s", tc.name, tc.key, tc.in, chordShow(got), chordShow(tc.want))
		}
		if c.armed() {
			t.Errorf("%s: still armed", tc.name)
		}
	}
}

func TestChordJoinsTypedBytes(t *testing.T) {
	c := newChord(0x1c)
	got := c.feed([]byte("a\x1b[Ab\x1b[92;5:3uc"), chordT0)
	if want := []chordEvent{chordTy("a\x1b[Ab\x1b[92;5:3uc")}; !chordSame(got, want) {
		t.Fatalf("got %s, want %s", chordShow(got), chordShow(want))
	}
}

func TestEverySplitGivesTheSameEvents(t *testing.T) {
	for _, tc := range chordCases {
		for i := 0; i <= len(tc.in); i++ {
			c := newChord(tc.key)
			got := c.feed([]byte(tc.in[:i]), chordT0)
			got = append(got, c.feed([]byte(tc.in[i:]), chordT0)...)
			got = append(got, c.tick(chordT0.Add(holdFor))...)
			if got := chordJoin(got); !chordSame(got, chordJoin(tc.want)) {
				t.Errorf("%s: split %q | %q\n got %s\nwant %s", tc.name, tc.in[:i], tc.in[i:], chordShow(got), chordShow(tc.want))
			}
		}
		// Byte by byte, too.
		c := newChord(tc.key)
		var got []chordEvent
		for i := 0; i < len(tc.in); i++ {
			got = append(got, c.feed([]byte{tc.in[i]}, chordT0)...)
		}
		got = append(got, c.tick(chordT0.Add(holdFor))...)
		if got := chordJoin(got); !chordSame(got, chordJoin(tc.want)) {
			t.Errorf("%s: byte by byte\n got %s\nwant %s", tc.name, chordShow(got), chordShow(tc.want))
		}
	}
}

func TestAPasteIsNeverAChord(t *testing.T) {
	c := newChord(0x1c)
	got := c.feed([]byte("\x1b[200~a\x1cdb\x1b[201~\x1cd"), chordT0)
	want := []chordEvent{chordTy("\x1b[200~a\x1cdb\x1b[201~"), chordLeave}
	if !chordSame(got, want) {
		t.Fatalf("got %s, want %s", chordShow(got), chordShow(want))
	}
	// A paste that has not ended by the end of a read is not held.
	c = newChord(0x1c)
	if got := c.feed([]byte("\x1b[200~\x1cd"), chordT0); !chordSame(got, []chordEvent{chordTy("\x1b[200~\x1cd")}) {
		t.Fatalf("open paste: got %s", chordShow(got))
	}
	if d, ok := c.due(); !ok || !d.Equal(chordT0.Add(pasteIdle)) {
		t.Fatalf("an open paste: due() = %v %v, want %v", d, ok, chordT0.Add(pasteIdle))
	}
	if got := c.feed([]byte("\x1b[20"), chordT0); !chordSame(got, []chordEvent{chordTy("\x1b[20")}) {
		t.Fatalf("a cut paste end: got %s", chordShow(got))
	}
	if got := c.feed([]byte("1~\x1cd"), chordT0); !chordSame(got, []chordEvent{chordTy("1~"), chordLeave}) {
		t.Fatalf("the paste's end: got %s", chordShow(got))
	}
}

func TestChordEndsAPasteThatGoesIdle(t *testing.T) {
	c := newChord(0x1c)
	if got := c.feed([]byte("\x1b[200~ab"), chordT0); !chordSame(got, []chordEvent{chordTy("\x1b[200~ab")}) {
		t.Fatalf("feed: got %s", chordShow(got))
	}
	if d, ok := c.due(); !ok || !d.Equal(chordT0.Add(pasteIdle)) {
		t.Fatalf("due() = %v %v, want %v", d, ok, chordT0.Add(pasteIdle))
	}
	at := chordT0.Add(900 * time.Millisecond)
	if got := c.feed([]byte("c\x1cd"), at); !chordSame(got, []chordEvent{chordTy("c\x1cd")}) {
		t.Fatalf("feed at +900ms: got %s", chordShow(got))
	}
	if d, _ := c.due(); !d.Equal(at.Add(pasteIdle)) {
		t.Fatalf("due() = %v, want the deadline pushed on to %v", d, at.Add(pasteIdle))
	}
	if got := c.tick(chordT0.Add(pasteIdle)); len(got) != 0 {
		t.Fatalf("tick before the deadline: got %s", chordShow(got))
	}
	if got := c.tick(at.Add(pasteIdle)); len(got) != 0 {
		t.Fatalf("tick at the deadline: got %s", chordShow(got))
	}
	if _, ok := c.due(); ok {
		t.Fatal("still due after the paste ended")
	}
	if got := c.feed([]byte("\x1cd"), at.Add(pasteIdle)); !chordSame(got, []chordEvent{chordLeave}) {
		t.Fatalf("after the paste: got %s", chordShow(got))
	}

	// A read after the deadline, with no tick between, ends the paste first.
	c = newChord(0x1c)
	c.feed([]byte("\x1b[200~ab"), chordT0)
	if got := c.feed([]byte("\x1cd"), chordT0.Add(pasteIdle)); !chordSame(got, []chordEvent{chordLeave}) {
		t.Fatalf("late read: got %s", chordShow(got))
	}

	// The tail of a stalled paste is still typed byte for byte.
	c = newChord(0x1c)
	c.feed([]byte("\x1b[200~ab"), chordT0)
	c.tick(chordT0.Add(pasteIdle))
	if got := c.feed([]byte("cd\x1b[201~"), chordT0.Add(2*pasteIdle)); !chordSame(got, []chordEvent{chordTy("cd\x1b[201~")}) {
		t.Fatalf("the tail: got %s", chordShow(got))
	}
}

func TestALoneEscapeGoesAfterHoldFor(t *testing.T) {
	c := newChord(0x1c)
	if got := c.feed([]byte("\x1b"), chordT0); len(got) != 0 {
		t.Fatalf("feed: got %s", chordShow(got))
	}
	if d, ok := c.due(); !ok || !d.Equal(chordT0.Add(holdFor)) {
		t.Fatalf("due() = %v %v, want %v", d, ok, chordT0.Add(holdFor))
	}
	if got := c.tick(chordT0.Add(24 * time.Millisecond)); len(got) != 0 {
		t.Fatalf("tick(+24ms): got %s", chordShow(got))
	}
	if got := c.tick(chordT0.Add(25 * time.Millisecond)); !chordSame(got, []chordEvent{chordTy("\x1b")}) {
		t.Fatalf("tick(+25ms): got %s", chordShow(got))
	}
	if _, ok := c.due(); ok {
		t.Fatal("due() after the escape went")
	}

	// A cut CSI goes the same way, as it came.
	c = newChord(0x1c)
	c.feed([]byte("x\x1b[1;"), chordT0)
	if got := c.tick(chordT0.Add(holdFor)); !chordSame(got, []chordEvent{chordTy("\x1b[1;")}) {
		t.Fatalf("cut CSI: got %s", chordShow(got))
	}

	// The wait counts from the read that last added to it.
	c = newChord(0x1c)
	c.feed([]byte("\x1b"), chordT0)
	c.feed([]byte("[9"), chordT0.Add(20*time.Millisecond))
	if got := c.tick(chordT0.Add(holdFor)); len(got) != 0 {
		t.Fatalf("extended hold went early: %s", chordShow(got))
	}
	if got := c.feed([]byte("2;5ud"), chordT0.Add(40*time.Millisecond)); !chordSame(got, []chordEvent{chordLeave}) {
		t.Fatalf("completed late: got %s", chordShow(got))
	}

	// An escape that goes while armed is the second key, and ends the chord.
	c = newChord(0x1c)
	c.feed([]byte("\x1c\x1b"), chordT0)
	if got := c.tick(chordT0.Add(holdFor)); !chordSame(got, []chordEvent{chordCxl}) {
		t.Fatalf("armed escape: got %s", chordShow(got))
	}
	if c.armed() {
		t.Fatal("still armed after an escape")
	}
}

func TestChordHoldsOnlyEscapes(t *testing.T) {
	for _, in := range []string{"a", "\xc3", "\x1b[200~\x1b", "\x1bO", "\x1b[A"} {
		c := newChord(0x1c)
		if got := c.feed([]byte(in), chordT0); !chordSame(got, []chordEvent{chordTy(in)}) {
			t.Errorf("feed(%q) = %s, want it typed at once", in, chordShow(got))
		}
		if len(c.held) != 0 || c.state != chordGround {
			t.Errorf("feed(%q) held %q", in, c.held)
		}
	}
	c := newChord(0x1c)
	if got := c.feed([]byte("\x1c"), chordT0); len(got) != 0 || !c.armed() {
		t.Fatalf("the key: got %s, armed %v", chordShow(got), c.armed())
	}
}

func TestTheHintComesOnceAfter400ms(t *testing.T) {
	c := newChord(0x1c)
	if got := c.feed([]byte("\x1c"), chordT0); len(got) != 0 {
		t.Fatalf("feed: got %s", chordShow(got))
	}
	if d, ok := c.due(); !ok || !d.Equal(chordT0.Add(hintAfter)) {
		t.Fatalf("due() = %v %v, want %v", d, ok, chordT0.Add(hintAfter))
	}
	if got := c.tick(chordT0.Add(399 * time.Millisecond)); len(got) != 0 {
		t.Fatalf("tick(+399ms): got %s", chordShow(got))
	}
	if got := c.tick(chordT0.Add(400 * time.Millisecond)); !chordSame(got, []chordEvent{chordHint}) {
		t.Fatalf("tick(+400ms): got %s", chordShow(got))
	}
	if !c.armed() {
		t.Fatal("not armed after the hint")
	}
	if _, ok := c.due(); ok {
		t.Fatal("due() after the hint")
	}
	if got := c.tick(chordT0.Add(2 * time.Second)); len(got) != 0 {
		t.Fatalf("tick(+2s): got %s", chordShow(got))
	}
	if got := c.feed([]byte("d"), chordT0.Add(2*time.Second)); !chordSame(got, []chordEvent{chordLeave}) {
		t.Fatalf("feed(d): got %s", chordShow(got))
	}
	if _, ok := c.due(); ok || c.armed() {
		t.Fatal("still waiting after the chord ended")
	}

	// A held escape and the hint come due in the order of their deadlines.
	c = newChord(0x1c)
	c.feed([]byte("\x1c"), chordT0)
	c.feed([]byte("\x1b"), chordT0.Add(390*time.Millisecond))
	if d, _ := c.due(); !d.Equal(chordT0.Add(hintAfter)) {
		t.Fatalf("due() = %v, want the hint first", d)
	}
	if got := c.tick(chordT0.Add(time.Second)); !chordSame(got, []chordEvent{chordHint, chordCxl}) {
		t.Fatalf("both due: got %s", chordShow(got))
	}
	// A new chord shows its own hint.
	c.feed([]byte("\x1c"), chordT0.Add(2*time.Second))
	if got := c.tick(chordT0.Add(2*time.Second + hintAfter)); !chordSame(got, []chordEvent{chordHint}) {
		t.Fatalf("second chord: got %s", chordShow(got))
	}
}

func TestResetForgetsTheChord(t *testing.T) {
	c := newChord(0x1c)
	c.feed([]byte("\x1c"), chordT0)
	c.reset()
	if c.armed() {
		t.Fatal("armed after reset")
	}
	if got := c.feed([]byte("d"), chordT0); !chordSame(got, []chordEvent{chordTy("d")}) {
		t.Fatalf("got %s, want [Type \"d\"]", chordShow(got))
	}
	if _, ok := c.due(); ok {
		t.Fatal("a hint is still due after reset")
	}
}

func TestChordResetKeepsAPaste(t *testing.T) {
	c := newChord(0x1c)
	if got := c.feed([]byte("\x1cn\x1b[200~abc"), chordT0); !chordSame(got, []chordEvent{chordNext, chordTy("\x1b[200~abc")}) {
		t.Fatalf("got %s", chordShow(got))
	}
	c.reset()
	if got := c.feed([]byte("\x1cd x\x1b[201~"), chordT0); !chordSame(got, []chordEvent{chordTy("\x1cd x\x1b[201~")}) {
		t.Fatalf("the rest of the paste: got %s", chordShow(got))
	}
	if c.armed() {
		t.Fatal("a pasted key armed the chord")
	}
	if got := c.feed([]byte("\x1cd"), chordT0); !chordSame(got, []chordEvent{chordLeave}) {
		t.Fatalf("after the paste: got %s", chordShow(got))
	}

	// A paste that starts across the reset is still a paste.
	c = newChord(0x1c)
	c.feed([]byte("\x1cn\x1b[20"), chordT0)
	c.reset()
	if got := c.feed([]byte("0~\x1cd\x1b[201~"), chordT0); !chordSame(got, []chordEvent{chordTy("\x1b[200~\x1cd\x1b[201~")}) {
		t.Fatalf("a paste cut by the reset: got %s", chordShow(got))
	}
}

func TestChordResetDropsAnOpenSequenceWhole(t *testing.T) {
	c := newChord(0x1c)
	c.feed([]byte("\x1cn\x1b[9"), chordT0)
	c.reset()
	if got := c.feed([]byte("2;5u"), chordT0); len(got) != 0 {
		t.Fatalf("the tail of the sequence: got %s", chordShow(got))
	}
	if got := c.feed([]byte("d"), chordT0); !chordSame(got, []chordEvent{chordTy("d")}) {
		t.Fatalf("after it: got %s", chordShow(got))
	}

	for _, tc := range []struct{ before, after, want string }{
		{"\x1cn\x1b", "xy", "y"},            // alt-x
		{"\x1cn\x1b", "\x1b[Az", "\x1b[Az"}, // a lone ESC, then an arrow of its own
		{"\x1cn\x1b", "\x1cd", ""},          // a lone ESC, then a chord
		{"\x1cn\x1b[1", "\x03z", "\x03z"},   // broken by a control byte
		{"\x1c\x1bO", "Pz", "z"},            // SS3
		{"\x1cn\x1b[<35;1", "0;5Mz", "z"},   // a mouse report
		{"\x1c\xc3", "\xa9z", "z"},          // the rest of a dropped character
	} {
		c := newChord(0x1c)
		c.feed([]byte(tc.before), chordT0)
		c.reset()
		var want []chordEvent
		if tc.want != "" {
			want = []chordEvent{chordTy(tc.want)}
		}
		if tc.after == "\x1cd" {
			want = []chordEvent{chordLeave}
		}
		got := c.feed([]byte(tc.after), chordT0)
		if !chordSame(got, want) {
			t.Errorf("%q | reset | %q: got %s, want %s", tc.before, tc.after, chordShow(got), chordShow(want))
		}
	}

	// It goes silently at holdFor, too.
	c = newChord(0x1c)
	c.feed([]byte("\x1cn\x1b[9"), chordT0)
	c.reset()
	if got := c.tick(chordT0.Add(holdFor)); len(got) != 0 {
		t.Fatalf("tick: got %s", chordShow(got))
	}
	if _, ok := c.due(); ok {
		t.Fatal("still held after holdFor")
	}
	if got := c.feed([]byte("2;5u"), chordT0.Add(holdFor)); !chordSame(got, []chordEvent{chordTy("2;5u")}) {
		t.Fatalf("after the hold went: got %s", chordShow(got))
	}
}

func TestChordLateReadDoesNotCompleteAStaleHold(t *testing.T) {
	c := newChord(0x1c)
	c.feed([]byte("\x1c\x1b"), chordT0)
	if got := c.feed([]byte("d"), chordT0.Add(200*time.Millisecond)); !chordSame(got, []chordEvent{chordCxl, chordTy("d")}) {
		t.Fatalf("armed: got %s", chordShow(got))
	}
	// The hint coming due first does not hide a stale hold.
	c = newChord(0x1c)
	c.feed([]byte("\x1c"), chordT0)
	c.feed([]byte("\x1b"), chordT0.Add(390*time.Millisecond))
	if got := c.feed([]byte("d"), chordT0.Add(time.Second)); !chordSame(got, []chordEvent{chordCxl, chordTy("d")}) {
		t.Fatalf("armed, hint due first: got %s", chordShow(got))
	}
	c = newChord(0x1c)
	c.feed([]byte("\x1b[92"), chordT0)
	if got := c.feed([]byte(";5ud"), chordT0.Add(holdFor)); !chordSame(got, []chordEvent{chordTy("\x1b[92;5ud")}) {
		t.Fatalf("unarmed: got %s", chordShow(got))
	}
	if c.armed() {
		t.Fatal("a stale hold armed the chord")
	}
}

func TestChordNonKeyKeepsItArmed(t *testing.T) {
	c := newChord(0x1c)
	c.feed([]byte("\x1c"), chordT0)
	for _, in := range []string{"\x1b[?1u", "\x1b[<35;10;5M", "\x1b[I", "\x1b[>1;2c"} {
		if got := c.feed([]byte(in), chordT0); !chordSame(got, []chordEvent{chordTy(in)}) {
			t.Errorf("%q: got %s", in, chordShow(got))
		}
		if !c.armed() {
			t.Fatalf("%q disarmed the chord", in)
		}
	}
	if got := c.tick(chordT0.Add(hintAfter)); !chordSame(got, []chordEvent{chordHint}) {
		t.Fatalf("the hint: got %s", chordShow(got))
	}
}

func TestChordHoldsSS3OnlyWhileArmed(t *testing.T) {
	c := newChord(0x1c)
	if got := c.feed([]byte("\x1c\x1bO"), chordT0); len(got) != 0 {
		t.Fatalf("armed: got %s", chordShow(got))
	}
	if d, ok := c.due(); !ok || !d.Equal(chordT0.Add(holdFor)) {
		t.Fatalf("due() = %v %v", d, ok)
	}
	if got := c.tick(chordT0.Add(holdFor)); !chordSame(got, []chordEvent{chordCxl}) {
		t.Fatalf("at holdFor: got %s", chordShow(got))
	}
}

func TestChordEventsDoNotShareTheReadBuffer(t *testing.T) {
	c := newChord(0x1c)
	buf := []byte("abc\x1c\x1c")
	got := c.feed(buf, chordT0)
	copy(buf, "xxxxx")
	if want := []chordEvent{chordTy("abc\x1c")}; !chordSame(got, want) {
		t.Fatalf("got %s, want %s", chordShow(got), chordShow(want))
	}
}

func FuzzChord(f *testing.F) {
	for _, tc := range chordCases {
		f.Add([]byte(tc.in), tc.key, uint8(len(tc.in)/2))
	}
	f.Fuzz(func(t *testing.T, in []byte, key byte, cut uint8) {
		keys := []byte{0x1c, 0x1d, 0x1e, 0x1f, 0x01, 0x04}
		key = keys[int(key)%len(keys)]
		split := 0
		if len(in) > 0 {
			split = int(cut) % (len(in) + 1)
		}

		whole := newChord(key)
		fed := whole.feed(in, chordT0)
		if len(whole.held) > chordMax {
			t.Fatalf("holds %d bytes", len(whole.held))
		}
		ticked := whole.tick(chordT0.Add(holdFor))
		if len(whole.held) != 0 {
			t.Fatal("bytes held after holdFor")
		}
		for _, evs := range [][]chordEvent{fed, ticked} {
			for i, e := range evs {
				if e.act == actType && len(e.bytes) == 0 {
					t.Fatalf("an empty actType at %d", i)
				}
				if e.act != actType && e.bytes != nil {
					t.Fatalf("action %d carries bytes", e.act)
				}
				if e.act == actType && i > 0 && evs[i-1].act == actType {
					t.Fatalf("adjacent actType events at %d: %s", i, chordShow(evs))
				}
			}
		}
		all := chordJoin(append(fed, ticked...))

		// Typed bytes are a subsequence of the input. Send-through types the
		// bytes the second press came as, so it holds for that too.
		var typed []byte
		for _, e := range all {
			if e.act == actType {
				typed = append(typed, e.bytes...)
			}
		}
		j := 0
		for _, b := range in {
			if j < len(typed) && typed[j] == b {
				j++
			}
		}
		if j != len(typed) {
			t.Fatalf("typed %q is not a subsequence of %q", typed, in)
		}

		// Input with no escape and no key is typed whole.
		if !bytes.ContainsAny(in, string([]byte{0x1b, key})) {
			if len(in) > 0 && !chordSame(all, []chordEvent{{act: actType, bytes: in}}) {
				t.Fatalf("plain input %q gave %s", in, chordShow(all))
			}
		}

		// A reset at the split never panics, never holds past holdFor, and
		// types only input.
		reset := newChord(key)
		r := reset.feed(in[:split], chordT0)
		reset.reset()
		r = append(r, reset.feed(in[split:], chordT0)...)
		r = append(r, reset.tick(chordT0.Add(holdFor))...)
		if len(reset.held) != 0 || reset.state != chordGround {
			t.Fatal("bytes held after a reset and holdFor")
		}
		var rt []byte
		for _, e := range r {
			if e.act == actType {
				rt = append(rt, e.bytes...)
			}
		}
		j = 0
		for _, b := range in {
			if j < len(rt) && rt[j] == b {
				j++
			}
		}
		if j != len(rt) {
			t.Fatalf("after a reset, typed %q is not a subsequence of %q", rt, in)
		}

		// A split read gives the same events.
		parts := newChord(key)
		got := parts.feed(in[:split], chordT0)
		got = append(got, parts.feed(in[split:], chordT0)...)
		got = append(got, parts.tick(chordT0.Add(holdFor))...)
		if !chordSame(chordJoin(got), all) {
			t.Fatalf("split %q | %q\n got %s\nwant %s", in[:split], in[split:], chordShow(chordJoin(got)), chordShow(all))
		}
	})
}
