package hold

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/professor93/rota/internal/claudecode"
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/vtscan"
)

// TestMain lets this test binary stand in for the fake CLI the capture
// harness records.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	claudecode.StandIn() // no test here may reach a real keychain
	os.Exit(m.Run())
}

// The captures TestCaptureVT recorded, each with xterm's view of it beside it
// as <name>.xterm.json, made by scripts/vt/golden.mjs.
var captures = []string{"vim", "vim-quit", "less", "fake", "emoji"}

// vtDoc is golden.mjs's document: what a terminal shows, cell by cell, and
// the sha256 of what it was shown.
type vtDoc struct {
	Sha256 string   `json:"sha256"`
	Rows   []string `json:"rows"`
	Cells  []vtCell `json:"cells"`
	Cursor vtCursor `json:"cursor"`
	Modes  vtModes  `json:"modes"`
}

type vtCell struct {
	X         int    `json:"x"`
	Y         int    `json:"y"`
	Ch        string `json:"ch"`
	Fg        string `json:"fg"`
	Bg        string `json:"bg"`
	Bold      bool   `json:"bold"`
	Italic    bool   `json:"italic"`
	Underline bool   `json:"underline"`
	Inverse   bool   `json:"inverse"`
	Dim       bool   `json:"dim"`
}

type vtCursor struct {
	X      int  `json:"x"`
	Y      int  `json:"y"`
	Hidden bool `json:"hidden"`
}

type vtModes struct {
	Alt            bool `json:"alt"`
	CursorKeys     bool `json:"cursorKeys"`
	BracketedPaste bool `json:"bracketedPaste"`
	Mouse          int  `json:"mouse"`
	Focus          bool `json:"focus"`
}

// golden is xterm's view of the bytes from, which <name>.xterm.json must
// have been made from: a golden left over from another capture or another
// repaint would compare against a screen nobody drew.
func golden(t *testing.T, name string, from []byte) vtDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vt", name+".xterm.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d vtDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("%s.xterm.json: %v", name, err)
	}
	if sum := sha256.Sum256(from); d.Sha256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("%s.xterm.json was made from other bytes than %s.bin (sha256 %s, not %x); make it again, as scripts/vt/README says",
			name, name, d.Sha256, sum)
	}
	return d
}

func recording(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vt", name+".bin"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// doc reads a Screen the way golden.mjs reads xterm, after everything it was
// given. Modes are not the screen's to say and stay zero.
func doc(t *testing.T, s *Screen) vtDoc {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok {
		t.Fatal("the screen has no emulator")
	}
	s.settle()
	em := s.em
	var d vtDoc
	for y := range em.Height() {
		var row strings.Builder
		for x := range em.Width() {
			c := em.CellAt(x, y)
			if c == nil || c.Width == 0 {
				continue
			}
			ch := c.Content
			if ch == "" {
				ch = " "
			}
			row.WriteString(ch)
			cell := vtCell{
				X: x, Y: y, Ch: ch,
				Fg:        colourName(c.Style.Fg),
				Bg:        colourName(c.Style.Bg),
				Bold:      c.Style.Attrs&uv.AttrBold != 0,
				Italic:    c.Style.Attrs&uv.AttrItalic != 0,
				Underline: c.Style.Underline != uv.UnderlineStyleNone,
				Inverse:   c.Style.Attrs&uv.AttrReverse != 0,
				Dim:       c.Style.Attrs&uv.AttrFaint != 0,
			}
			if ch != " " || cell.Bg != "default" || cell.Inverse || cell.Underline {
				d.Cells = append(d.Cells, cell)
			}
		}
		d.Rows = append(d.Rows, strings.TrimRight(row.String(), " "))
	}
	p := em.CursorPosition()
	d.Cursor = vtCursor{X: p.X, Y: p.Y, Hidden: em.CursorHidden()}
	return d
}

func colourName(c color.Color) string {
	switch c := c.(type) {
	case nil:
		return "default"
	case ansi.BasicColor:
		return fmt.Sprintf("p%d", c)
	case ansi.IndexedColor:
		return fmt.Sprintf("p%d", c)
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// screenDiff says how two documents, labelled a and b, differ in rows, cells
// and cursor, one difference at a time, or nothing when they agree.
func screenDiff(a, b string, want, got vtDoc) string {
	la, lb := fmt.Sprintf("%-8s", a), fmt.Sprintf("%-8s", b)
	var out []string
	for y := range max(len(want.Rows), len(got.Rows)) {
		w, g := at(want.Rows, y), at(got.Rows, y)
		if w != g {
			out = append(out, fmt.Sprintf("row %d:\n  %s %q\n  %s %q", y, la, w, lb, g))
		}
	}
	key := func(c vtCell) [2]int { return [2]int{c.Y, c.X} }
	wc, gc := map[[2]int]vtCell{}, map[[2]int]vtCell{}
	var keys [][2]int
	for _, c := range want.Cells {
		wc[key(c)] = c
		keys = append(keys, key(c))
	}
	for _, c := range got.Cells {
		gc[key(c)] = c
		if _, ok := wc[key(c)]; !ok {
			keys = append(keys, key(c))
		}
	}
	slices.SortFunc(keys, func(a, b [2]int) int {
		if a[0] != b[0] {
			return a[0] - b[0]
		}
		return a[1] - b[1]
	})
	shown := 0
	for _, k := range keys {
		w, wok := wc[k]
		g, gok := gc[k]
		if wok == gok && w == g {
			continue
		}
		if shown++; shown > 40 {
			out = append(out, "... more cells differ")
			break
		}
		out = append(out, fmt.Sprintf("cell %d,%d:\n  %s %s\n  %s %s", k[1], k[0], la, cellText(w, wok), lb, cellText(g, gok)))
	}
	if want.Cursor != got.Cursor {
		out = append(out, fmt.Sprintf("cursor:\n  %s %+v\n  %s %+v", la, want.Cursor, lb, got.Cursor))
	}
	return strings.Join(out, "\n")
}

func at(rows []string, y int) string {
	if y < len(rows) {
		return rows[y]
	}
	return ""
}

func cellText(c vtCell, ok bool) string {
	if !ok {
		return "(blank)"
	}
	raw, _ := json.Marshal(c)
	return string(raw)
}

// feed writes a capture into a Screen the way the holder does: in reads of
// a few bytes, so sequences arrive in pieces, each cut at vtscan's last
// ground point and the rest held for the next. It returns the scanner, which
// has seen every byte.
func feed(t *testing.T, s *Screen, raw []byte) *vtscan.Scanner {
	t.Helper()
	return feedBy(t, s, raw, 13)
}

// feedBy is feed in reads of size bytes.
func feedBy(t *testing.T, s *Screen, raw []byte, size int) *vtscan.Scanner {
	t.Helper()
	sc := vtscan.NewScanner()
	var held []byte
	for len(raw) > 0 {
		chunk := raw[:min(len(raw), size)]
		raw = raw[len(chunk):]
		r := sc.Feed(chunk)
		held = append(held, chunk...)
		if r.Ground < 0 {
			continue
		}
		cut := len(held) - len(chunk) + r.Ground
		if cut > 0 {
			s.Write(held[:cut])
		}
		held = append([]byte(nil), held[cut:]...)
	}
	if len(held) > 0 {
		t.Fatalf("the capture ends inside a sequence: %q", held)
	}
	return sc
}

// TestXVTMatchesXterm is the spike's bar: x/vt, fed what vim, less and the
// fake CLI wrote, ends on the screen xterm ends on, cell for cell. It does so
// in reads of 13 bytes, and in reads of one, which cut apart every grapheme
// cluster that can be cut.
func TestXVTMatchesXterm(t *testing.T) {
	for _, name := range captures {
		for _, size := range []int{13, 1} {
			t.Run(fmt.Sprintf("%s by %d", name, size), func(t *testing.T) {
				rec := recording(t, name)
				want := golden(t, name, rec)
				s := NewScreen(80, 24)
				defer s.Close()
				feedBy(t, s, rec, size)
				if d := screenDiff("xterm", "x/vt", want, doc(t, s)); d != "" {
					t.Errorf("x/vt and xterm disagree on %s:\n%s", name, d)
				}
			})
		}
	}
}

// TestModesMatchXterm: the modes vtscan tracks, which a repaint restores and
// a viewer's keys depend on, are xterm's after every capture.
func TestModesMatchXterm(t *testing.T) {
	for _, name := range captures {
		t.Run(name, func(t *testing.T) {
			rec := recording(t, name)
			want := golden(t, name, rec)
			s := NewScreen(80, 24)
			defer s.Close()
			m := feed(t, s, rec).Modes()
			got := vtModes{
				Alt: m.AltScreen, CursorKeys: m.CursorKeys, BracketedPaste: m.BracketedPaste,
				Mouse: m.Mouse, Focus: m.Focus,
			}
			if got != want.Modes {
				t.Errorf("vtscan's modes after %s are\n  %+v\nxterm's are\n  %+v", name, got, want.Modes)
			}
			if m.CursorHidden != want.Cursor.Hidden {
				t.Errorf("vtscan says the cursor is hidden: %v; xterm says %v", m.CursorHidden, want.Cursor.Hidden)
			}
			// xterm 5.5 keeps neither the mouse encoding nor the kitty flags
			// where a test can read them, so fake.bin's are checked against
			// what its script set.
			if name == "fake" && (m.MouseEncoding != 1006 || m.Kitty != 1) {
				t.Errorf("after fake.bin the mouse encoding is %d and the kitty flags %d; the script set 1006 and 1", m.MouseEncoding, m.Kitty)
			}
		})
	}
}

// TestRepaintReproducesTheScreen: what Repaint draws is the screen, in x/vt
// and in xterm alike, modes included. ROTA_VT_GOLDEN=1 writes the repaints
// instead of checking them, for scripts/vt/golden.mjs to read; see
// scripts/vt/README.
func TestRepaintReproducesTheScreen(t *testing.T) {
	for _, name := range captures {
		t.Run(name, func(t *testing.T) {
			rec := recording(t, name)
			s := NewScreen(80, 24)
			defer s.Close()
			m := feed(t, s, rec).Modes()
			repaint, ok := s.Repaint(m)
			if !ok {
				t.Fatal("Repaint refused")
			}
			path := filepath.Join("testdata", "vt", name+".repaint.bin")
			if os.Getenv("ROTA_VT_GOLDEN") == "1" {
				if err := os.WriteFile(path, repaint, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			committed, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(repaint, committed) {
				t.Errorf("the repaint of %s is not %s:\n  got  %q\n  want %q", name, path, repaint, committed)
			}

			fresh := NewScreen(80, 24)
			defer fresh.Close()
			if again := feed(t, fresh, repaint).Modes(); again != m {
				t.Errorf("the repaint sets the modes\n  %+v\nnot\n  %+v", again, m)
			}
			if d := screenDiff("x/vt", "repaint", doc(t, s), doc(t, fresh)); d != "" {
				t.Errorf("x/vt draws the repaint of %s as another screen:\n%s", name, d)
			}

			before, after := golden(t, name, rec), golden(t, name+".repaint", committed)
			if d := screenDiff("xterm", "repaint", before, after); d != "" {
				t.Errorf("xterm draws the repaint of %s as another screen:\n%s", name, d)
			}
			if before.Modes != after.Modes {
				t.Errorf("xterm's modes after the repaint of %s are\n  %+v\nnot\n  %+v", name, after.Modes, before.Modes)
			}
		})
	}
}

// TestAPanickingEmulatorLeavesReplay: whatever x/vt does with a byte it did
// not expect, the holder carries on without a screen.
func TestAPanickingEmulatorLeavesReplay(t *testing.T) {
	calls := 0
	vtWrite = func(*vt.Emulator, []byte) (int, error) {
		calls++
		panic("x/vt fell over")
	}
	t.Cleanup(func() { vtWrite = (*vt.Emulator).Write })

	s := NewScreen(80, 24)
	defer s.Close()
	if !s.OK() {
		t.Fatal("a new screen is not OK")
	}
	if s.Lost() != "" {
		t.Fatalf("a new screen lost its emulator: %s", s.Lost())
	}
	s.Write([]byte("hello"))
	if s.OK() {
		t.Fatal("the screen is OK after its emulator panicked")
	}
	if !strings.HasPrefix(s.Lost(), "x/vt fell over\n") {
		t.Fatalf("the screen says it lost its emulator to %q", s.Lost())
	}
	if b, ok := s.Repaint(vtscan.Modes{}); b != nil || ok {
		t.Fatalf("a retired screen repaints %q, %v", b, ok)
	}
	if row, col := s.Cursor(); row != 1 || col != 1 {
		t.Fatalf("the cursor of a retired screen is %d;%d, want 1;1", row, col)
	}
	s.Resize(100, 30)
	for range 1000 {
		s.Write(bytes.Repeat([]byte("x"), 1024))
	}
	if calls != 1 {
		t.Fatalf("the emulator was called %d times; a retired screen calls it no more", calls)
	}
}

// TestQueryRepliesNeverBlockTheEmulator: x/vt answers a query by writing into
// a pipe, and a pipe nobody reads stops it dead. The screen reads it.
func TestQueryRepliesNeverBlockTheEmulator(t *testing.T) {
	s := NewScreen(80, 24)
	done := make(chan struct{})
	go func() {
		s.Write(bytes.Repeat([]byte("\x1b[c"), 10000))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Close would wait for the stuck Write, so the screen is left to
		// the end of the test binary.
		t.Fatal("10,000 device attribute queries did not go through the emulator in 2s")
	}
	defer s.Close()
	if !s.OK() {
		t.Fatal("the screen is not OK")
	}
}
