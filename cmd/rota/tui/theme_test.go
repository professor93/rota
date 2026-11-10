package tui

import (
	"bytes"
	"image/color"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// themeTokens names every token of th, so a test covers a token added later.
func themeTokens(th Theme) map[string]lipgloss.Style {
	return map[string]lipgloss.Style{
		"Text": th.Text, "Muted": th.Muted, "Faint": th.Faint, "Accent": th.Accent,
		"Good": th.Good, "Warn": th.Warn, "Over": th.Over, "Dead": th.Dead,
		"Key": th.Key, "Selected": th.Selected, "Rule": th.Rule, "Title": th.Title,
	}
}

// themeWritten is s.Render("x") as a terminal at p receives it.
func themeWritten(p colorprofile.Profile, s lipgloss.Style) string {
	var buf bytes.Buffer
	w := &colorprofile.Writer{Forward: &buf, Profile: p}
	w.Write([]byte(s.Render("x")))
	return buf.String()
}

func themeHasForeground(s lipgloss.Style) bool {
	_, none := s.GetForeground().(lipgloss.NoColor)
	return !none
}

func TestEveryTierRendersItsOwnColours(t *testing.T) {
	basic := regexp.MustCompile(`\x1b\[[39][0-7]m`)
	for _, dark := range []bool{true, false} {
		if got := themeWritten(colorprofile.TrueColor, NewTheme(colorprofile.TrueColor, dark).Good); !strings.Contains(got, "38;2;") {
			t.Errorf("dark=%v TrueColor: %q has no 24-bit foreground", dark, got)
		}
		if got := themeWritten(colorprofile.ANSI256, NewTheme(colorprofile.ANSI256, dark).Good); !strings.Contains(got, "38;5;") || strings.Contains(got, "38;2;") {
			t.Errorf("dark=%v ANSI256: %q is not a 256-colour foreground", dark, got)
		}
		if got := themeWritten(colorprofile.ANSI, NewTheme(colorprofile.ANSI, dark).Good); !basic.MatchString(got) || strings.Contains(got, "38;") {
			t.Errorf("dark=%v ANSI: %q is not one of the 16 colours", dark, got)
		}
		// The Writer converts colours itself, so a theme that ignored the
		// depth would pass the checks above. Rendered without one, each
		// colour token is already the depth's own colour.
		for _, tier := range []struct {
			p    colorprofile.Profile
			kind color.Color
			seq  *regexp.Regexp
		}{
			{colorprofile.TrueColor, color.RGBA{}, regexp.MustCompile(`38;2;`)},
			{colorprofile.ANSI256, ansi.IndexedColor(0), regexp.MustCompile(`38;5;`)},
			{colorprofile.ANSI, ansi.BasicColor(0), regexp.MustCompile(`\x1b\[(?:[0-9]+;)*[39][0-7]m`)},
		} {
			for name, s := range themeTokens(NewTheme(tier.p, dark)) {
				if !themeHasForeground(s) {
					continue
				}
				if got := s.GetForeground(); reflect.TypeOf(got) != reflect.TypeOf(tier.kind) {
					t.Errorf("dark=%v %v: %s is a %T", dark, tier.p, name, got)
				}
				if got := s.Render("x"); !tier.seq.MatchString(got) {
					t.Errorf("dark=%v %v: %s renders %q", dark, tier.p, name, got)
				}
			}
		}
		th := NewTheme(colorprofile.Ascii, dark)
		for name, s := range themeTokens(th) {
			if got := themeWritten(colorprofile.Ascii, s); strings.Contains(got, "38;") || strings.Contains(got, "48;") || basic.MatchString(got) {
				t.Errorf("dark=%v Ascii: %s renders a colour: %q", dark, name, got)
			}
			if themeHasForeground(s) {
				t.Errorf("dark=%v Ascii: %s sets a foreground", dark, name)
			}
		}
		if got := themeWritten(colorprofile.Ascii, th.Selected); !strings.Contains(got, "\x1b[7m") {
			t.Errorf("dark=%v Ascii: Selected %q is not reverse", dark, got)
		}
		if got := themeWritten(colorprofile.Ascii, th.Over); !strings.Contains(got, "\x1b[1m") {
			t.Errorf("dark=%v Ascii: Over %q is not bold", dark, got)
		}
		if got := themeWritten(colorprofile.Ascii, th.Faint); !strings.Contains(got, "\x1b[2m") {
			t.Errorf("dark=%v Ascii: Faint %q is not faint", dark, got)
		}
	}
}

func TestNoColorIsHonoured(t *testing.T) {
	var buf bytes.Buffer
	env := []string{"TERM=xterm-256color", "COLORTERM=truecolor"}
	if p := Profile(&buf, append(env, "NO_COLOR=1"), true); p != colorprofile.Ascii {
		t.Errorf("remote with NO_COLOR: %v, want Ascii", p)
	}
	if p := Profile(&buf, env, true); p != colorprofile.TrueColor {
		t.Errorf("remote: %v, want TrueColor", p)
	}
	if p := Profile(&buf, []string{"TERM=xterm-256color"}, true); p != colorprofile.ANSI256 {
		t.Errorf("remote at xterm-256color: %v, want ANSI256", p)
	}
	// Locally the output decides, and a buffer is no terminal; NO_COLOR is
	// still Ascii, whatever the output.
	if p := Profile(&buf, append(env, "NO_COLOR=1"), false); p != colorprofile.Ascii {
		t.Errorf("local with NO_COLOR: %v, want Ascii", p)
	}
	if p := Profile(&buf, []string{"TERM=xterm-256color"}, false); p != colorprofile.NoTTY {
		t.Errorf("local to a buffer: %v, want NoTTY", p)
	}
	// no-color.org: any value that is not empty.
	if p := Profile(&buf, append(env, "NO_COLOR=yes"), true); p != colorprofile.Ascii {
		t.Errorf("NO_COLOR=yes: %v, want Ascii", p)
	}
	if p := Profile(&buf, append(env, "NO_COLOR="), true); p != colorprofile.TrueColor {
		t.Errorf("NO_COLOR empty: %v, want TrueColor", p)
	}
}

func TestLightAndDarkDiffer(t *testing.T) {
	dark, light := themeTokens(NewTheme(colorprofile.TrueColor, true)), themeTokens(NewTheme(colorprofile.TrueColor, false))
	colours := 0
	for name, d := range dark {
		l := light[name]
		if themeHasForeground(d) != themeHasForeground(l) {
			t.Errorf("%s sets a foreground on one background only", name)
			continue
		}
		if !themeHasForeground(d) {
			continue
		}
		colours++
		if themeSame(d.GetForeground(), l.GetForeground()) {
			t.Errorf("%s has the same foreground on dark and light", name)
		}
	}
	// Every token but Text, Faint and Selected.
	if colours != 9 {
		t.Errorf("%d tokens set a foreground, want 9", colours)
	}
	if !NewTheme(colorprofile.TrueColor, true).Dark || NewTheme(colorprofile.TrueColor, false).Dark {
		t.Error("Theme.Dark is not the background it was made for")
	}
	if p := NewTheme(colorprofile.ANSI256, true).Profile; p != colorprofile.ANSI256 {
		t.Errorf("Theme.Profile = %v", p)
	}
}

func TestThemeGlobalKeysAreTheDesignedOnes(t *testing.T) {
	k := newGlobalKeys()
	for b, names := range map[*key.Binding][]string{
		&k.Quit: {"q", "ctrl+c"}, &k.Help: {"?"}, &k.Mouse: {"M"},
		&k.Next: {"]"}, &k.Prev: {"["}, &k.Jump: {"1", "2", "3", "4"},
	} {
		for _, n := range names {
			if !key.Matches(press(n)[0].(tea.KeyPressMsg), *b) {
				t.Errorf("%q does not match %v", n, b.Keys())
			}
		}
		if b.Help().Key == "" || b.Help().Desc == "" {
			t.Errorf("%v has no help", b.Keys())
		}
	}
	all := []key.Binding{k.Quit, k.Help, k.Mouse, k.Next, k.Prev, k.Jump}
	for _, n := range []string{"m", "Q", "5", "0", "tab"} {
		if key.Matches(press(n)[0].(tea.KeyPressMsg), all...) {
			t.Errorf("%q matches a global key", n)
		}
	}
}

func themeSame(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

// The inputs every task uses neither blink, which would keep the driver from
// going quiet, nor reach the host's clipboard on ctrl+v or ctrl+shift+c. No
// command an input returns is ever run here: one that reached the clipboard
// would read or write the real one on this machine.
func TestThemeInputsNeitherBlinkNorReachTheClipboard(t *testing.T) {
	th := NewTheme(colorprofile.TrueColor, true)
	in := newInput(th)
	if in.Focus() != nil {
		t.Error("textinput Focus returns a command: a blinking cursor")
	}
	if in.KeyMap.Paste.Enabled() {
		t.Error("textinput's ctrl+v is bound")
	}
	// The same key on Bubbles' own bindings returns the clipboard read, so
	// the check below can fail.
	ctl := in
	ctl.KeyMap = textinput.DefaultKeyMap()
	if _, cmd := ctl.Update(press("ctrl+v")[0]); cmd == nil {
		t.Fatal("the control: Bubbles' ctrl+v returns no command")
	}
	if _, cmd := in.Update(press("ctrl+v")[0]); cmd != nil {
		t.Error("textinput ctrl+v returns a command")
	}
	in, _ = in.Update(tea.PasteMsg{Content: "abc"})
	if in.Value() != "abc" {
		t.Errorf("textinput takes a paste as %q", in.Value())
	}

	area := newArea(th)
	if area.Focus() != nil {
		t.Error("textarea Focus returns a command: a blinking cursor")
	}
	if area.KeyMap.Paste.Enabled() || area.KeyMap.CopySelection.Enabled() {
		t.Error("textarea's ctrl+v or ctrl+shift+c is bound")
	}
	// A copy needs a selection: without one Bubbles' own ctrl+shift+c
	// returns nothing either.
	area, _ = area.Update(tea.PasteMsg{Content: "abc"})
	area, _ = area.Update(press("ctrl+g")[0])
	if area.Value() != "abc" || !area.HasSelection() {
		t.Fatalf("textarea %q, selected %v", area.Value(), area.HasSelection())
	}
	for _, k := range []string{"ctrl+v", "ctrl+shift+c"} {
		ctl := area
		ctl.KeyMap = textarea.DefaultKeyMap()
		if _, cmd := ctl.Update(press(k)[0]); cmd == nil {
			t.Fatalf("the control: Bubbles' %s returns no command", k)
		}
		if _, cmd := area.Update(press(k)[0]); cmd != nil {
			t.Errorf("textarea %s returns a command", k)
		}
	}
}
