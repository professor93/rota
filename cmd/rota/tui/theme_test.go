package tui

import (
	"bytes"
	"image/color"
	"regexp"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
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
	if colours < 8 {
		t.Errorf("only %d tokens set a foreground", colours)
	}
	if !NewTheme(colorprofile.TrueColor, true).Dark || NewTheme(colorprofile.TrueColor, false).Dark {
		t.Error("Theme.Dark is not the background it was made for")
	}
	if p := NewTheme(colorprofile.ANSI256, true).Profile; p != colorprofile.ANSI256 {
		t.Errorf("Theme.Profile = %v", p)
	}
}

func TestTheGlobalKeysAreTheDesignedOnes(t *testing.T) {
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
// going quiet, nor reach the host's clipboard on ctrl+v or ctrl+shift+c.
func TestInputsNeitherBlinkNorReadTheClipboard(t *testing.T) {
	th := NewTheme(colorprofile.TrueColor, true)
	in := newInput(th)
	if cmd := in.Focus(); cmd != nil {
		t.Errorf("textinput Focus returns a command (a blinking cursor): %T", cmd())
	}
	if in.KeyMap.Paste.Enabled() {
		t.Error("textinput's ctrl+v is bound")
	}
	if _, cmd := in.Update(press("ctrl+v")[0]); cmd != nil {
		t.Errorf("textinput ctrl+v returns a command: %T", cmd())
	}
	in, _ = in.Update(tea.PasteMsg{Content: "abc"})
	if in.Value() != "abc" {
		t.Errorf("textinput takes a paste as %q", in.Value())
	}

	area := newArea(th)
	if cmd := area.Focus(); cmd != nil {
		t.Errorf("textarea Focus returns a command (a blinking cursor): %T", cmd())
	}
	if area.KeyMap.Paste.Enabled() || area.KeyMap.CopySelection.Enabled() {
		t.Error("textarea's ctrl+v or ctrl+shift+c is bound")
	}
	for _, k := range []string{"ctrl+v", "ctrl+shift+c"} {
		if _, cmd := area.Update(press(k)[0]); cmd != nil {
			t.Errorf("textarea %s returns a command: %T", k, cmd())
		}
	}
	area, _ = area.Update(tea.PasteMsg{Content: "abc"})
	if area.Value() != "abc" {
		t.Errorf("textarea takes a paste as %q", area.Value())
	}
}
