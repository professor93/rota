package tui

import (
	"image/color"
	"io"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// Theme is every style the TUI draws with. It is made once for the
// terminal's colour depth and background, and nothing else picks a colour.
type Theme struct {
	// Dark is the background the theme was made for.
	Dark bool
	// Profile is the colour depth it was made for.
	Profile colorprofile.Profile

	Text     lipgloss.Style // ordinary text, in the terminal's own colour
	Muted    lipgloss.Style // secondary text: labels, units, hints
	Faint    lipgloss.Style // what is stale or out of play
	Accent   lipgloss.Style // the active tab, a link, a focused field
	Good     lipgloss.Style // usage well under the threshold, a daemon running
	Warn     lipgloss.Style // usage nearing the threshold
	Over     lipgloss.Style // usage at or past the threshold
	Dead     lipgloss.Style // an account that needs a sign-in again
	Key      lipgloss.Style // a key in the footer and the help
	Selected lipgloss.Style // the selected row
	Rule     lipgloss.Style // lines between parts of the screen
	Title    lipgloss.Style // the program's name and a section's
}

// themeColor is one colour at each depth: the 16-colour value, the 256-colour
// one and the 24-bit one, as lipgloss.Complete takes them.
type themeColor struct{ ansi, ansi256, truecolor string }

// themePalette holds each colour token's colour on a light and a dark
// background. The 16-colour values are the terminal's own palette, so they
// follow whatever scheme the person chose.
var themePalette = struct {
	muted, accent, good, warn, over, dead, key, rule, title [2]themeColor // [light, dark]
}{
	muted:  [2]themeColor{{"8", "243", "#6B7280"}, {"7", "248", "#A9B1BD"}},
	accent: [2]themeColor{{"4", "26", "#1D4ED8"}, {"12", "111", "#7AA2F7"}},
	good:   [2]themeColor{{"2", "28", "#15803D"}, {"10", "114", "#86C77A"}},
	warn:   [2]themeColor{{"3", "136", "#A16207"}, {"11", "179", "#E0AF68"}},
	over:   [2]themeColor{{"1", "160", "#C81E1E"}, {"9", "203", "#F7768E"}},
	dead:   [2]themeColor{{"5", "125", "#9D174D"}, {"13", "175", "#D68AB0"}},
	key:    [2]themeColor{{"6", "30", "#0E7490"}, {"14", "117", "#7DCFFF"}},
	rule:   [2]themeColor{{"7", "251", "#C9CDD4"}, {"8", "239", "#4A5060"}},
	title:  [2]themeColor{{"5", "55", "#5B21B6"}, {"13", "141", "#BB9AF7"}},
}

// NewTheme makes the theme for colour depth p on a dark or a light
// background. Below 16 colours (Ascii, NoTTY) it sets no colour at all:
// Selected stays reverse, Over bold and Faint faint, so no state is told by
// colour alone.
func NewTheme(p colorprofile.Profile, dark bool) Theme {
	bg := 0
	if dark {
		bg = 1
	}
	complete := lipgloss.Complete(p)
	fg := func(s lipgloss.Style, c [2]themeColor) lipgloss.Style {
		if p < colorprofile.ANSI {
			return s
		}
		v := c[bg]
		return s.Foreground(complete(lipgloss.Color(v.ansi), lipgloss.Color(v.ansi256), lipgloss.Color(v.truecolor)))
	}
	pal := themePalette
	return Theme{
		Dark:     dark,
		Profile:  p,
		Text:     lipgloss.NewStyle(),
		Muted:    fg(lipgloss.NewStyle(), pal.muted),
		Faint:    lipgloss.NewStyle().Faint(true),
		Accent:   fg(lipgloss.NewStyle(), pal.accent),
		Good:     fg(lipgloss.NewStyle(), pal.good),
		Warn:     fg(lipgloss.NewStyle(), pal.warn),
		Over:     fg(lipgloss.NewStyle().Bold(true), pal.over),
		Dead:     fg(lipgloss.NewStyle(), pal.dead),
		Key:      fg(lipgloss.NewStyle().Bold(true), pal.key),
		Selected: lipgloss.NewStyle().Reverse(true),
		Rule:     fg(lipgloss.NewStyle(), pal.rule),
		Title:    fg(lipgloss.NewStyle().Bold(true), pal.title),
	}
}

// Profile is the colour depth to draw at. Locally it is what the output and
// the environment say (colorprofile.Detect). Over SSH the channel is no
// terminal, so it is what TERM and COLORTERM say (colorprofile.Env). A
// NO_COLOR that is set and not empty means no colour either way.
func Profile(out io.Writer, env []string, remote bool) colorprofile.Profile {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "NO_COLOR="); ok && v != "" {
			return colorprofile.Ascii
		}
	}
	if remote {
		return colorprofile.Env(env)
	}
	return colorprofile.Detect(out, env)
}

// themeCursor is the cursor's colour, the accent's at this depth.
func themeCursor(th Theme) color.Color {
	if c := th.Accent.GetForeground(); c != nil {
		if _, none := c.(lipgloss.NoColor); !none {
			return c
		}
	}
	return lipgloss.NoColor{}
}

// newInput is the one-line text field every task uses. Its cursor does not
// blink, and ctrl+v does not read the host's clipboard: over SSH that host
// is the daemon's, and a paste arrives as tea.PasteMsg anyway.
func newInput(th Theme) textinput.Model {
	in := textinput.New()
	s := textinput.DefaultStyles(th.Dark)
	s.Cursor.Blink = false
	s.Cursor.Color = themeCursor(th)
	s.Focused.Text, s.Blurred.Text = th.Text, th.Muted
	s.Focused.Placeholder, s.Blurred.Placeholder = th.Faint, th.Faint
	s.Focused.Prompt, s.Blurred.Prompt = th.Accent, th.Muted
	in.SetStyles(s)
	in.KeyMap.Paste.Unbind()
	return in
}

// newArea is the many-line text field every task uses, with newInput's
// cursor and paste, and no ctrl+shift+c writing to the host's clipboard.
func newArea(th Theme) textarea.Model {
	a := textarea.New()
	s := textarea.DefaultStyles(th.Dark)
	s.Cursor.Blink = false
	s.Cursor.Color = themeCursor(th)
	s.Focused.Text, s.Blurred.Text = th.Text, th.Muted
	s.Focused.Placeholder, s.Blurred.Placeholder = th.Faint, th.Faint
	s.Focused.Prompt, s.Blurred.Prompt = th.Accent, th.Muted
	a.SetStyles(s)
	a.KeyMap.Paste.Unbind()
	a.KeyMap.CopySelection.Unbind()
	return a
}
