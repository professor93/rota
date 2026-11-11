package tui

// Editing an account, removing one, and the confirm both use (R35). The
// form holds every `rota set` flag and saves what was changed in one write;
// removing asks first and names the sessions running on the account; and
// either says so when the account is removed elsewhere while it is open
// (Review Focus 4). The form is here, its drawing in edit_view.go, and the
// remove and the confirm in edit_ask.go.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/cmd/rota/client"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/wire"
)

// editFormBindings are the form's keys: tab and shift+tab move between the
// rows, enter saves, or clears on the clear row, space switches a switch,
// and esc cancels. Close is enter or esc once the account is gone.
type editFormBindings struct {
	Next, Prev, Save, Clear, Toggle, Cancel, Close key.Binding
}

func editFormKeys() editFormBindings {
	return editFormBindings{
		Next:   key.NewBinding(key.WithKeys("tab", "down"), key.WithHelp("tab", "next")),
		Prev:   key.NewBinding(key.WithKeys("shift+tab", "up"), key.WithHelp("⇧tab", "back")),
		Save:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "save")),
		Clear:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "clear")),
		Toggle: key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "switch")),
		Cancel: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		Close:  key.NewBinding(key.WithKeys("enter", "esc"), key.WithHelp("⏎", "close")),
	}
}

// editField is a row of the form, in the order drawn.
type editField int

const (
	editOrder editField = iota
	editThreshold
	editFolder
	editConfig
	editSessions
	editRemote
	editForget
	editClear
)

var editLabels = [...]string{"order", "threshold", "folder", "config", "sessions", "remote control", "forget long token", "clear"}

// editNames are the words a refusal names each row by: the API's field
// names, and the store's and the rotation's words for them. The forget row
// has none: the API refuses long only for a value other than forget, which
// this form never sends, and "long" is a word of ordinary sentences.
var editNames = [...][]string{
	editOrder:     {"order", "place", "placed"},
	editThreshold: {"threshold"},
	editFolder:    {"cwd", "working directory"},
	editConfig:    {"config_dir", "configuration directory"},
	editSessions:  {"sessions"},
	editRemote:    {"remote_control", "remote control"},
}

// editHints say under the row with focus what it takes.
var editHints = [...]string{
	editOrder:     "a number, first, last, up, down, before:<id>, after:<id>, or out",
	editThreshold: "the usage at which the rotation moves on: 1 to 100",
	editFolder:    "where its runs start: an absolute path, or empty for none",
	editConfig:    "its own CLI configuration and credentials: an absolute path, or empty for none",
	editSessions:  "where its conversations live: shared, own, or a directory",
	editRemote:    "its own .claude.json, which Claude Code's Remote Control needs",
	editForget:    "the token is thrown away when the form is saved",
	editClear:     "",
}

// editEmpty is what an empty field reads while it has no focus.
var editEmpty = [...]string{editFolder: "not set", editConfig: "not set", editSessions: "shared", editClear: ""}

// The form's columns: the label after an indent and the focus mark, and
// the value after it. editLimit bounds what a field holds.
const (
	editIndent  = 2
	editLabelW  = 18
	editValueAt = editIndent + 2 + editLabelW
	editLimit   = 4096
	editPercent = "threshold must be a percentage from 1 to 100"
	editKeyLike = "that value looks like a key, so it is not taken as a directory or sent"
)

// editRow is one row: a field's text, as it was when the form opened.
type editRow struct {
	field editField
	in    textinput.Model
	orig  string
	err   string // a refusal or a check, said under the row
}

// editForm is the form that edits one account.
type editForm struct {
	e    *env
	keys editFormBindings
	acct wire.Account
	// local says the principal is this machine's own, the one the server
	// takes a change of Remote Control from.
	local bool
	rows  []editRow
	at    int
	// remote and forget are the switches as they stand.
	remote, forget bool
	// note is a failure that names no row, said at the top.
	note                 string
	saving, gone, closed bool
	why                  string // the server's words when a save found it gone
}

// editSavedMsg is how a save of form f ended. sent is which rows it sent.
type editSavedMsg struct {
	form    *editForm
	err     error
	cleared bool
	sent    []editField
}

// newEdit is the form that edits acct: every row as it stands, the order
// first with focus.
func newEdit(e *env, acct wire.Account) overlay {
	f := &editForm{e: e, keys: editFormKeys(), acct: acct, local: e.who.Via == "local", remote: acct.RemoteControl}
	for fd := editOrder; fd <= editSessions; fd++ {
		// Sessions and Remote Control are Claude Code's: the server refuses
		// either for another provider's account.
		if fd == editSessions && !f.claude() {
			continue
		}
		in := newInput(e.th)
		in.CharLimit = editLimit
		in.SetValue(editValue(fd, acct))
		f.rows = append(f.rows, editRow{field: fd, in: in, orig: editNorm(fd, in.Value())})
	}
	if f.claude() {
		f.rows = append(f.rows, editRow{field: editRemote})
	}
	if acct.LongUntil != "" {
		f.rows = append(f.rows, editRow{field: editForget})
	}
	f.rows = append(f.rows, editRow{field: editClear})
	f.focus(0)
	return f
}

// editValue is a field's text for a: the order as `rota set` takes it, the
// threshold judged by, and shared for the default sessions.
func editValue(fd editField, a wire.Account) string {
	switch fd {
	case editOrder:
		if a.Order < 1 {
			return "out"
		}
		return strconv.Itoa(a.Order)
	case editThreshold:
		return strconv.Itoa(barsThreshold(a.Threshold))
	case editFolder:
		return a.Cwd
	case editConfig:
		return a.ConfigDir
	case editSessions:
		return editNorm(editSessions, a.Sessions)
	}
	return ""
}

// editNorm is a field's text as compared and sent: without the spaces
// around it, 0 as out, a percent without its sign, empty sessions shared.
func editNorm(fd editField, v string) string {
	v = strings.TrimSpace(v)
	switch fd {
	case editOrder:
		if v == "0" {
			return "out"
		}
	case editThreshold:
		return strings.TrimSpace(strings.TrimSuffix(v, "%"))
	case editSessions:
		if v == "" {
			return "shared"
		}
	}
	return v
}

func (f *editForm) About() (string, int) { return "edit", f.acct.ID }
func (f *editForm) claude() bool         { return rota.Flavor(f.acct.Provider) == "claude" }
func (f *editForm) Closed() bool         { return f.closed }
func (f *editForm) live() bool           { return !f.gone && !f.saving && !f.closed }
func (f *editForm) Typing() bool         { return f.live() && f.text(f.at) }

func (f *editForm) text(i int) bool { return f.rows[i].field <= editSessions }

// can says row i takes focus: every row but Remote Control for a principal
// whose change of it the server would refuse.
func (f *editForm) can(i int) bool { return f.rows[i].field != editRemote || f.local }

func (f *editForm) focus(i int) {
	for j := range f.rows {
		f.rows[j].in.Blur()
	}
	f.at = i
	if f.text(i) {
		_ = f.rows[i].in.Focus()
	}
}

// move gives focus to the next row that takes it, d rows on, round.
func (f *editForm) move(d int) {
	n := len(f.rows)
	for i, s := 0, f.at; i < n; i++ {
		if s = (s + d + n) % n; f.can(s) {
			f.focus(s)
			return
		}
	}
}

// arm sets which keys work as the form stands.
func (f *editForm) arm() {
	k, live, fd := &f.keys, f.live(), f.rows[f.at].field
	k.Close.SetEnabled(f.gone && !f.closed)
	for _, b := range []*key.Binding{&k.Next, &k.Prev, &k.Cancel} {
		b.SetEnabled(live)
	}
	k.Toggle.SetEnabled(live && (fd == editRemote || fd == editForget))
	k.Clear.SetEnabled(live && fd == editClear)
	k.Save.SetEnabled(live && fd != editClear)
}

func (f *editForm) Keys() []key.Binding {
	f.arm()
	k := f.keys
	return editOn(k.Save, k.Clear, k.Toggle, k.Next, k.Prev, k.Cancel, k.Close)
}

func editOn(bs ...key.Binding) []key.Binding {
	return slices.DeleteFunc(bs, func(b key.Binding) bool { return !b.Enabled() })
}

func (f *editForm) Update(msg tea.Msg) (overlay, tea.Cmd) {
	switch msg := msg.(type) {
	case listingMsg:
		if !f.gone {
			gone, again := editMissing(msg, f.acct.ID)
			f.gone = gone
			return f, again
		}
	case editSavedMsg:
		if msg.form == f {
			return f, f.saved(msg)
		}
	case tea.KeyPressMsg:
		return f, f.key(msg)
	case tea.PasteMsg:
		if f.Typing() {
			f.rows[f.at].in, _ = f.rows[f.at].in.Update(msg)
		}
	}
	return f, nil
}

func (f *editForm) key(msg tea.KeyPressMsg) tea.Cmd {
	f.arm()
	k := &f.keys
	switch {
	case key.Matches(msg, k.Close):
		f.closed = true
	case !f.live():
	case key.Matches(msg, k.Cancel):
		f.closed = true
	case key.Matches(msg, k.Next):
		f.move(1)
	case key.Matches(msg, k.Prev):
		f.move(-1)
	case key.Matches(msg, k.Toggle):
		if f.rows[f.at].field == editRemote {
			f.remote = !f.remote
		} else {
			f.forget = !f.forget
		}
	case key.Matches(msg, k.Clear):
		return f.askClear()
	case key.Matches(msg, k.Save):
		return f.save()
	case f.text(f.at):
		f.rows[f.at].in, _ = f.rows[f.at].in.Update(msg)
	}
	return nil
}

// change is what the rows changed, as one Change: a row left as it was is
// not in it, and Remote Control only from a principal the server takes it
// from. A value that cannot be sent is bad, the row it is in, with why.
func (f *editForm) change() (c Change, bad int, why string) {
	for i, r := range f.rows {
		v := editNorm(r.field, r.in.Value())
		if f.text(i) && v != r.orig && wire.HoldsSecret(v) {
			return Change{}, i, editKeyLike
		}
		switch r.field {
		case editOrder:
			if v != r.orig {
				c.Order = &v
			}
		case editThreshold:
			if v == r.orig {
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return Change{}, i, editPercent
			}
			if strconv.Itoa(n) != r.orig {
				c.Threshold = &n
			}
		case editFolder:
			if v != r.orig {
				c.Cwd = &v
			}
		case editConfig:
			if v != r.orig {
				c.ConfigDir = &v
			}
		case editSessions:
			if v != r.orig {
				c.Sessions = &v
			}
		case editRemote:
			if on := f.remote; f.local && on != f.acct.RemoteControl {
				c.RemoteControl = &on
			}
		case editForget:
			c.ForgetLong = f.forget
		}
	}
	return c, -1, ""
}

// save sends what was changed, or closes the form when nothing was.
func (f *editForm) save() tea.Cmd {
	f.forgetErrs()
	c, bad, why := f.change()
	switch {
	case bad >= 0:
		f.rows[bad].err = why
		f.focus(bad)
		return nil
	case c == Change{}:
		f.closed = true
		return nil
	}
	return f.send(c, false)
}

func (f *editForm) forgetErrs() {
	f.note = ""
	for i := range f.rows {
		f.rows[i].err = ""
	}
}

// send makes c the one PATCH of the account; the form waits for it.
func (f *editForm) send(c Change, cleared bool) tea.Cmd {
	f.forgetErrs()
	f.saving = true
	var sent []editField
	for _, s := range []struct {
		set bool
		fd  editField
	}{{c.Order != nil, editOrder}, {c.Threshold != nil, editThreshold}, {c.Cwd != nil, editFolder},
		{c.ConfigDir != nil, editConfig}, {c.Sessions != nil, editSessions}, {c.RemoteControl != nil, editRemote},
		{c.ForgetLong, editForget}} {
		if s.set {
			sent = append(sent, s.fd)
		}
	}
	b, id := f.e.b, f.acct.ID
	ended := func(err error) tea.Msg { return editSavedMsg{form: f, err: err, cleared: cleared, sent: sent} }
	return f.e.callOr(envCallTimeout, func(ctx context.Context) tea.Msg {
		_, err := b.Change(ctx, id, c)
		return ended(err)
	}, ended, nil)
}

// saved is a save's end: the form closes on a success, which is said and
// shown at once; it says the account is gone on a 404, and otherwise puts
// the refusal under the row it names, or at the top.
func (f *editForm) saved(msg editSavedMsg) tea.Cmd {
	f.saving = false
	var no *client.Refusal
	switch {
	case msg.err == nil:
		f.closed = true
		say := "saved "
		if msg.cleared {
			say = "cleared the settings of "
		}
		return tea.Batch(toast(levelInfo, "%s%s", say, editName(f.acct)), editRefresh)
	case errors.As(msg.err, &no) && no.Status == http.StatusNotFound:
		f.gone, f.why = true, no.Error()
		return editRefresh
	case f.gone:
	default:
		f.refused(msg.err, msg.sent)
	}
	return nil
}

// refused puts the server's refusal under a row, as place picks it, and
// anything else, a failure that is not a refusal among them, at the top.
func (f *editForm) refused(err error, sent []editField) {
	text, at := err.Error(), editField(-1)
	if no := (*client.Refusal)(nil); errors.As(err, &no) {
		at = f.place(text, sent)
	}
	for i := range f.rows {
		if f.rows[i].field == at {
			f.rows[i].err = text
			if f.can(i) {
				f.focus(i)
			}
			return
		}
	}
	f.note = text
}

// place is the row a refusal is about: of the rows it names, the first
// that this write sent; else the one row sent, when it sent one; else the
// first it names that the form has; else none, -1. The account, as the
// server writes it into a sentence, names nothing: its label is the
// person's own words, and may hold a row's.
func (f *editForm) place(text string, sent []editField) editField {
	named := editNamed(editUnlabelled(text, f.acct))
	for _, fd := range named {
		if slices.Contains(sent, fd) {
			return fd
		}
	}
	if len(sent) == 1 {
		return sent[0]
	}
	for _, fd := range named {
		if slices.ContainsFunc(f.rows, func(r editRow) bool { return r.field == fd }) {
			return fd
		}
	}
	return -1
}

// editUnlabelled is text with account a taken out where the server writes
// it, provider/label (rota.Account's String): the label is the name, else
// the email, else the uuid's start, else account-N.
func editUnlabelled(text string, a wire.Account) string {
	uuid := a.UUID
	if len(uuid) > 12 {
		uuid = uuid[:12] + "..."
	}
	for _, label := range []string{a.Name, a.Email, uuid, "account-" + strconv.Itoa(a.ID)} {
		if label != "" {
			text = strings.ReplaceAll(text, a.Provider+"/"+label, " ")
		}
	}
	return text
}

// editNamed is every row text names, in the order it first names them.
func editNamed(text string) []editField {
	type hit struct {
		at int
		fd editField
	}
	var hits []hit
	low := strings.ToLower(text)
	for fd, names := range editNames {
		first := -1
		for _, n := range names {
			if i := editWord(low, n); i >= 0 && (first < 0 || i < first) {
				first = i
			}
		}
		if first >= 0 {
			hits = append(hits, hit{first, editField(fd)})
		}
	}
	slices.SortFunc(hits, func(x, y hit) int { return x.at - y.at })
	out := make([]editField, len(hits))
	for i, h := range hits {
		out[i] = h.fd
	}
	return out
}

// editWord is where w first stands in s as words of their own, or -1.
func editWord(s, w string) int {
	for from := 0; from < len(s); {
		i := strings.Index(s[from:], w)
		if i < 0 {
			return -1
		}
		i += from
		end := i + len(w)
		if (i == 0 || strings.IndexByte(` "'`, s[i-1]) >= 0) && (end == len(s) || strings.IndexByte(` :,;."'`, s[end]) >= 0) {
			return i
		}
		from = i + 1
	}
	return -1
}

// askClear asks before clearing what `rota set --clear` clears. From a
// principal the server takes no change of Remote Control from, Remote
// Control is left, and the confirm says where it is changed.
func (f *editForm) askClear() tea.Cmd {
	empty, shared, id := "", "shared", strconv.Itoa(f.acct.ID)
	c := Change{Cwd: &empty, ConfigDir: &empty, Sessions: &shared}
	body, claude := "Its folder, config and sessions go back to the defaults", f.claude()
	if !claude {
		body = "Its folder and config go back to the defaults"
	}
	switch {
	case f.local:
		off := false
		c.RemoteControl = &off
		if claude {
			body += ", and Remote Control goes off"
		}
		body += ", as rota set " + id + " --clear does."
	case claude && f.acct.RemoteControl:
		body += ". Remote Control is changed only on this machine: rota set " + id + " --remote-control off."
	default:
		body += "."
	}
	body += " Its threshold, its place and its long-lived token stay."
	return accountsOpen(newConfirm(f.e, "Clear the settings of "+editName(f.acct)+"?", body, func() tea.Cmd {
		if !f.live() {
			return nil
		}
		return f.send(c, true)
	}))
}

// editRefresh asks for the listing now, so what was changed shows.
func editRefresh() tea.Msg { return refreshMsg{How: RefreshNone} }

// editName names an account as the form and the asks do: claude account 1
// (work@example.com).
func editName(a wire.Account) string {
	return clean(a.Provider) + " account " + strconv.Itoa(a.ID) + " (" + accountsLabel(a) + ")"
}

func editGone(id int) string { return "account " + strconv.Itoa(id) + " is gone (removed elsewhere)" }

// editMissing is what a listing says of account id: gone when a read of
// what is known lacks it. A read that refreshed usage may have begun before
// the account was made, so one that lacks it asks for a read of what is
// known now, which settles it.
func editMissing(msg listingMsg, id int) (gone bool, again tea.Cmd) {
	switch {
	case msg.Err != nil || editListed(msg.Listing.Accounts, id):
		return false, nil
	case msg.How == RefreshNone:
		return true, nil
	}
	return false, editRefresh
}

func editListed(rows []wire.Account, id int) bool {
	return slices.ContainsFunc(rows, func(r wire.Account) bool { return r.ID == id })
}
