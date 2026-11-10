package tui

// How the edit form is drawn (edit.go): a row per setting, cleaned and fit
// to the width, the cursor where it stands, and a refusal under the row it
// names.

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	rota "github.com/professor93/rota/lib"
)

func (f *editForm) View(w, h int) string {
	th := f.e.th
	out := []string{th.Title.Render(accountsViewLine("Edit "+editName(f.acct), w-1))}
	if f.gone {
		out = append(append(out, ""), editWrap(th.Over, editGone(f.acct.ID), w-1)...)
		if f.why != "" {
			out = append(out, editWrap(th.Muted, f.why, w-1)...)
		}
		return editCut(out, h, 0, 0)
	}
	switch {
	case f.saving:
		out = append(out, th.Muted.Render(accountsViewLine("saving…", w-1)))
	case f.note != "":
		out = append(out, editWrap(th.Over, f.note, w-1)...)
	}
	out = append(out, "")
	hi, lo := 0, 0
	for i := range f.rows {
		if i == f.at {
			hi = len(out)
		}
		out = append(out, f.row(i, w)...)
		if i == f.at {
			lo = len(out) - 1
		}
	}
	return editCut(out, h, hi, lo)
}

// row draws row i: its label, its value, and under it a refusal, or the
// hint while it has focus.
func (f *editForm) row(i, w int) []string {
	th, r := f.e.th, &f.rows[i]
	on := i == f.at && f.live()
	mark, style := "  ", th.Muted
	if on {
		mark, style = "> ", th.Accent
	}
	vw := max(w-editValueAt-1, 1)
	var value []string
	switch r.field {
	case editRemote:
		now := editOnOff(f.remote)
		if f.local {
			value = editWrap(th.Text, now, vw)
		} else {
			value = editWrap(th.Muted, now+" · changed on this machine: rota set "+strconv.Itoa(f.acct.ID)+
				" --remote-control "+editOnOff(!f.acct.RemoteControl), vw)
		}
	case editForget:
		say := "[ ] keep it, until " + accountsViewUntil(barsTime(f.acct.LongUntil), f.e.now(), f.e.o.Loc)
		if f.forget {
			say = "[x] thrown away on save"
		}
		value = editWrap(th.Text, say, vw)
	case editClear:
		what := "folder, config and sessions back to the defaults"
		if f.local && rota.Flavor(f.acct.Provider) == "claude" {
			what = "folder, config, sessions and remote control back to the defaults"
		}
		value = editWrap(th.Muted, what, vw)
	default:
		value = []string{f.input(r, on, vw)}
	}
	pad := strings.Repeat(" ", editValueAt)
	out := []string{strings.Repeat(" ", editIndent) + mark + style.Render(fit(editLabels[r.field], editLabelW)) + value[0]}
	for _, l := range value[1:] {
		out = append(out, pad+l)
	}
	under, ustyle := r.err, th.Over
	if under == "" && on {
		under, ustyle = editHints[r.field], th.Muted
	}
	for _, l := range editWrap(ustyle, under, vw) {
		if l != "" {
			out = append(out, pad+l)
		}
	}
	return out
}

// input draws a field's text in w columns, cleaned as everything drawn is,
// with the cursor where it stands while the field has focus: the text
// scrolls so the cursor shows.
func (f *editForm) input(r *editRow, on bool, w int) string {
	th := f.e.th
	v := clean(r.in.Value())
	if !on {
		if v == "" {
			return th.Faint.Render(accountsViewLine(editEmpty[r.field], w))
		}
		return th.Text.Render(accountsViewLine(v, w))
	}
	runes := []rune(r.in.Value())
	at := min(textWidth(clean(string(runes[:min(r.in.Position(), len(runes))]))), textWidth(v))
	head, rest := editSplit(v, at)
	cur, tail := " ", ""
	if rest != "" {
		g, _ := ansi.FirstGraphemeCluster(rest, ansi.WcWidth)
		cur, tail = g, rest[len(g):]
	}
	if textWidth(cur) > w {
		cur, tail = " ", ""
	}
	for head != "" && textWidth(head)+textWidth(cur) > w {
		g, _ := ansi.FirstGraphemeCluster(head, ansi.WcWidth)
		head = head[len(g):]
	}
	if left := w - textWidth(head) - textWidth(cur); left > 0 && tail != "" {
		tail = strings.TrimRight(fit(tail, left), " ")
	} else {
		tail = ""
	}
	return th.Text.Render(head) + th.Selected.Render(cur) + th.Text.Render(tail)
}

// editSplit cuts s after the whole characters that fit in c columns.
func editSplit(s string, c int) (head, rest string) {
	end, used := 0, 0
	for rest := s; rest != ""; {
		g, gw := ansi.FirstGraphemeCluster(rest, ansi.WcWidth)
		if used+gw > c {
			break
		}
		end, used, rest = end+len(g), used+gw, rest[len(g):]
	}
	return s[:end], s[end:]
}

func editOnOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// editWrap is s cleaned and wrapped to w columns, each line in style; one
// empty line when there is nothing to say.
func editWrap(style lipgloss.Style, s string, w int) []string {
	lines := wrap(clean(s), w)
	if len(lines) == 0 {
		return []string{""}
	}
	for i, l := range lines {
		lines[i] = style.Render(l)
	}
	return lines
}

// editCut is lines cut to h, scrolled so lines hi to lo show.
func editCut(lines []string, h, hi, lo int) string {
	if h <= 0 {
		return ""
	}
	top := 0
	if len(lines) > h {
		top = max(min(max(lo-h+1, 0), hi), 0)
		lines = lines[top:min(top+h, len(lines))]
	}
	return strings.Join(lines, "\n")
}
