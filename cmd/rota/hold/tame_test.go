package hold

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/vtscan"
)

// guarded is a Screen fed in through Write.
func guarded(t *testing.T, cols, rows int, in ...string) *Screen {
	t.Helper()
	s := NewScreen(cols, rows)
	t.Cleanup(s.Close)
	for _, p := range in {
		s.Write([]byte(p))
	}
	return s
}

// unguarded is a Screen whose emulator was fed in directly, past tame: what
// x/vt makes of the bytes on its own.
func unguarded(t *testing.T, cols, rows int, in string) *Screen {
	t.Helper()
	s := NewScreen(cols, rows)
	t.Cleanup(s.Close)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.em.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	return s
}

// numbered is 24 rows that say which they are, so a scroll shows.
func numbered() string {
	var b strings.Builder
	for y := 1; y <= 24; y++ {
		fmt.Fprintf(&b, "\x1b[%d;1Hrow %d", y, y)
	}
	return b.String()
}

// TestMarginsPastTheScreenAreTheScreens: x/vt v0.1.0 keeps a bottom or right
// margin past the edge of the screen, and the next scroll, insert or delete
// panics, which would retire the screen for the rest of the session. The
// screen hands x/vt the margin xterm takes instead, the edge of the screen,
// and stays.
func TestMarginsPastTheScreenAreTheScreens(t *testing.T) {
	for _, c := range []struct{ name, in, same string }{
		{"DECSTBM then SD", "\x1b[1;30r\x1b[Tx", "\x1b[1;24r\x1b[Tx"},
		{"DECSTBM then SU", "\x1b[1;30r\x1b[Sx", "\x1b[1;24r\x1b[Sx"},
		{"DECSTBM then IL", "\x1b[3;30r\x1b[5;1H\x1b[Lx", "\x1b[3;24r\x1b[5;1H\x1b[Lx"},
		{"DECSTBM then DL", "\x1b[3;30r\x1b[5;1H\x1b[Mx", "\x1b[3;24r\x1b[5;1H\x1b[Mx"},
		{"DECSTBM then RI", "\x1b[1;30r\x1bMx", "\x1b[1;24r\x1bMx"},
		{"DECSTBM whose bottom overflows", "\x1b[1;99999999999999999999r\x1b[Tx", "\x1b[1;24r\x1b[Tx"},
		// xterm ignores a top margin at or below the bottom, once the
		// bottom is the screen's.
		{"DECSTBM below the screen", "\x1b[30;40r\x1b[Tx", "\x1b[Tx"},
		{"DECSLRM then ICH", "\x1b[?69h\x1b[1;100s\x1b[5@x", "\x1b[?69h\x1b[1;80s\x1b[5@x"},
		{"DECSLRM then IL", "\x1b[?69h\x1b[1;100s\x1b[Lx", "\x1b[?69h\x1b[1;80s\x1b[Lx"},
		{"DECSLRM then SU", "\x1b[?69h\x1b[1;100s\x1b[Sx", "\x1b[?69h\x1b[1;80s\x1b[Sx"},
		{"DECSLRM then SD", "\x1b[?69h\x1b[1;100s\x1b[Tx", "\x1b[?69h\x1b[1;80s\x1b[Tx"},
		// Without mode 69, CSI s saves the cursor whatever its numbers.
		{"SCOSC with numbers", "\x1b[3;5H\x1b[1;100s\x1b[Hab\x1b[uy", "\x1b[3;5H\x1b[s\x1b[Hab\x1b[uy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(numbered() + c.in)
			kept := bytes.Clone(in)
			s := NewScreen(80, 24)
			defer s.Close()
			s.Write(in)
			if !s.OK() {
				t.Fatalf("the screen retired on %q", c.in)
			}
			if !bytes.Equal(in, kept) {
				t.Fatalf("Write changed the bytes it was given:\n  %q\nnot\n  %q", in, kept)
			}
			want := unguarded(t, 80, 24, numbered()+c.same)
			if d := screenDiff("same", "screen", doc(t, want), doc(t, s)); d != "" {
				t.Errorf("%q draws another screen than %q:\n%s", c.in, c.same, d)
			}
		})
	}
}

// TestASequenceCutAcrossWritesIsMadeSafeToo: the holder cuts at ground points,
// but the screen does not count on it.
func TestASequenceCutAcrossWritesIsMadeSafeToo(t *testing.T) {
	s := guarded(t, 80, 24, numbered(), "\x1b[1;3", "0r\x1b[T", "x")
	if !s.OK() {
		t.Fatal("the screen retired")
	}
	want := unguarded(t, 80, 24, numbered()+"\x1b[1;24r\x1b[Tx")
	if d := screenDiff("same", "screen", doc(t, want), doc(t, s)); d != "" {
		t.Errorf("a margin cut across writes draws another screen:\n%s", d)
	}
}

// TestHugeCountsTakeNoTime: REP, ECH, CHT and CBT run their count in x/vt one
// at a time, REP for about a minute at 99999999, all under the screen's lock.
// The screen hands x/vt a count that draws the same.
func TestHugeCountsTakeNoTime(t *testing.T) {
	for _, c := range []struct{ name, in, same string }{
		{"REP", "x\x1b[99999999b", ""},
		{"ECH", "\x1b[3;1Hxxxxxxxxxx\x1b[3;4H\x1b[99999999X", "\x1b[3;1Hxxxxxxxxxx\x1b[3;4H\x1b[80X"},
		{"CHT", "\x1b[3;4H\x1b[99999999Ix", "\x1b[3;4H\x1b[80Ix"},
		{"CBT", "\x1b[3;70H\x1b[99999999Zx", "\x1b[3;70H\x1b[80Zx"},
	} {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			s := guarded(t, 80, 24, c.in)
			if took := time.Since(start); took > time.Second {
				t.Fatalf("%q took %v", c.in, took)
			}
			if !s.OK() {
				t.Fatal("the screen retired")
			}
			if c.same != "" {
				want := unguarded(t, 80, 24, c.same)
				if d := screenDiff("same", "screen", doc(t, want), doc(t, s)); d != "" {
					t.Errorf("%q draws another screen than %q:\n%s", c.in, c.same, d)
				}
				return
			}
			// 1 + 99999999 is 1,250,000 rows of 80: every row is full, and
			// the cursor waits in the last column of the last.
			d := doc(t, s)
			for y, row := range d.Rows {
				if row != strings.Repeat("x", 80) {
					t.Fatalf("row %d is %q", y, row)
				}
			}
			if d.Cursor.X != 79 || d.Cursor.Y != 23 {
				t.Fatalf("the cursor is at %+v, want 79,23", d.Cursor)
			}
		})
	}
}

// TestCappedCountsDrawWhatTheFullCountDraws runs counts around and past each
// cap, on small screens and from different places, against x/vt running the
// full count.
func TestCappedCountsDrawWhatTheFullCountDraws(t *testing.T) {
	setups := []struct {
		name, in string
		margins  bool
	}{
		{"home", "x", false},
		{"mid-screen", "\x1b[2;4Hx", false},
		{"no autowrap", "\x1b[?7lx", false},
		{"coloured", "\x1b[41;33mx", false},
		{"line drawing", "\x1b(0q", false},
		{"tab stops", "\x1b[3g\x1b[1;3H\x1bH\x1b[1;6H\x1bH\x1b[2;5Hx", false},
		{"in a region", "\x1b[2;3r\x1b[2;1Hx", true},
		{"below a region", "\x1b[1;2r\x1b[3;1Hx", true},
		{"origin mode", "\x1b[2;3r\x1b[?6h\x1b[1;5Hx", true},
		{"left and right margins", "\x1b[?69h\x1b[3;6s\x1b[2;4Hx", true},
	}
	for _, size := range [][2]int{{10, 4}, {7, 3}, {1, 1}} {
		w, h := size[0], size[1]
		// Every count up to a little past where tame starts to cap it, then
		// some well past it.
		counts := func(floor int) []int {
			var ns []int
			for n := range floor + 3*w {
				ns = append(ns, n)
			}
			for k := range 4 {
				ns = append(ns, floor+(k+3)*w+k%w, floor*(k+5)+k)
			}
			return ns
		}
		for _, setup := range setups {
			// The margins these set must be on the screen: margins past it
			// are TestMarginsPastTheScreenAreTheScreens's.
			if setup.margins && (w < 6 || h < 3) {
				continue
			}
			t.Run(fmt.Sprintf("%dx%d %s", w, h, setup.name), func(t *testing.T) {
				for _, op := range []struct {
					final byte
					floor int
				}{{'b', 2 * w * h}, {'X', w}, {'I', w}, {'Z', w}} {
					for _, n := range counts(op.floor) {
						in := setup.in + fmt.Sprintf("\x1b[%d%c", n, op.final) + "\x1b[1my"
						full, capped := unguarded(t, w, h, in), guarded(t, w, h, in)
						d := screenDiff("x/vt", "screen", doc(t, full), doc(t, capped))
						full.Close()
						capped.Close()
						if d != "" {
							t.Fatalf("%q:\n%s", in, d)
						}
					}
				}
			})
		}
	}
}

// TestAClusterCutAcrossWritesStaysWhole: x/vt finishes a grapheme cluster at
// the end of each write, and drops a combining mark that arrives alone. The
// screen keeps the end of a write back until the next shows it is finished.
func TestAClusterCutAcrossWritesStaysWhole(t *testing.T) {
	for _, c := range []struct{ name, a, b string }{
		{"combining mark", "cafe", "\u0301!"},
		{"two combining marks", "a\u0301", "\u0302!"},
		{"ZWJ sequence, cut before the ZWJ", "👩", "\u200d💻!"},
		{"ZWJ sequence, cut after the ZWJ", "👩\u200d", "💻!"},
		{"flag", "🇺", "🇸!"},
		{"variation selector", "✔", "\ufe0f!"},
		{"mark after a colour change", "e\x1b[31m", "\u0301!"},
		{"mark after CJK", "日本e", "\u0301!"},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := doc(t, unguarded(t, 80, 24, c.a+c.b))
			if d := screenDiff("whole", "cut", want, doc(t, guarded(t, 80, 24, c.a, c.b))); d != "" {
				t.Errorf("%q then %q draws another screen than both at once:\n%s", c.a, c.b, d)
			}
		})
	}
}

// TestWhatIsHeldBackIsOnTheScreenForReaders: Cursor, Repaint and Resize see
// everything written, held back or not.
func TestWhatIsHeldBackIsOnTheScreenForReaders(t *testing.T) {
	s := guarded(t, 80, 24, "abc")
	if row, col := s.Cursor(); row != 1 || col != 4 {
		t.Errorf("after abc the cursor is %d;%d, want 1;4", row, col)
	}
	s.Write([]byte("de"))
	r, ok := s.Repaint(vtscan.Modes{})
	if !ok || !bytes.Contains(r, []byte("abcde")) {
		t.Errorf("the repaint after abcde is %q, %v", r, ok)
	}
	s.Write([]byte("fg"))
	s.Resize(40, 10)
	if d := doc(t, s); d.Rows[0] != "abcdefg" || d.Cursor.X != 7 {
		t.Errorf("after abcdefg and a resize the first row is %q and the cursor %+v", d.Rows[0], d.Cursor)
	}
}

// TestALongRunIsNotHeldForever: a run of printed characters with no ASCII and
// no control in it is held only up to maxHeld.
func TestALongRunIsNotHeldForever(t *testing.T) {
	s := guarded(t, 80, 24, strings.Repeat("日", maxHeld/3+1))
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.held) > maxHeld {
		t.Fatalf("%d bytes are held", len(s.held))
	}
}
