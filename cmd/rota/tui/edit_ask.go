package tui

// The questions of edit.go: whether to remove an account, naming the
// sessions running on it, and the confirm that clear and others ask with.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/wire"
)

// editAskBindings are a question's keys: y, and n or esc, which the help
// calls yes and no; and Close once what it asked about is gone.
type editAskBindings struct{ Yes, No, Close key.Binding }

func editAskKeys(yes, no string) editAskBindings {
	return editAskBindings{
		Yes:   key.NewBinding(key.WithKeys("y", "Y"), key.WithHelp("y", yes)),
		No:    key.NewBinding(key.WithKeys("n", "N", "esc"), key.WithHelp("n/esc", no)),
		Close: key.NewBinding(key.WithKeys("enter", "esc"), key.WithHelp("⏎", "close")),
	}
}

// editAsk is a question: the confirm, or whether to remove an account.
type editAsk struct {
	e           *env
	keys        editAskBindings
	title, body string
	yes         func() tea.Cmd
	// acct is the account a remove asks about, nil for a confirm.
	acct         *wire.Account
	gone, closed bool
}

// newRemove asks whether to remove acct, naming the sessions the daemon
// holds on it as they stand; y removes it.
func newRemove(e *env, acct wire.Account) overlay {
	return &editAsk{e: e, keys: editAskKeys("remove", "keep it"), acct: &acct,
		title: "Remove " + editName(acct) + "? Its home and staged credentials go with it.",
		yes:   func() tea.Cmd { return editRemoving(e, acct) }}
}

// newConfirm asks title and body, and runs yes on a y. n or esc closes it
// with nothing done.
func newConfirm(e *env, title, body string, yes func() tea.Cmd) overlay {
	return &editAsk{e: e, keys: editAskKeys("yes", "no"), title: title, body: body, yes: yes}
}

func (o *editAsk) About() (string, int) {
	if o.acct != nil {
		return "remove", o.acct.ID
	}
	return "confirm: " + o.title, 0
}

func (o *editAsk) Closed() bool { return o.closed }
func (o *editAsk) Typing() bool { return false }

func (o *editAsk) arm() {
	k, asking := &o.keys, !o.gone && !o.closed
	k.Yes.SetEnabled(asking)
	k.No.SetEnabled(asking)
	k.Close.SetEnabled(o.gone && !o.closed)
}

func (o *editAsk) Keys() []key.Binding {
	o.arm()
	return editOn(o.keys.Yes, o.keys.No, o.keys.Close)
}

func (o *editAsk) Update(msg tea.Msg) (overlay, tea.Cmd) {
	switch msg := msg.(type) {
	case listingMsg:
		if o.acct != nil && !o.gone {
			gone, again := editMissing(msg, o.acct.ID)
			o.gone = gone
			return o, again
		}
	case tea.KeyPressMsg:
		o.arm()
		switch {
		case key.Matches(msg, o.keys.Close), key.Matches(msg, o.keys.No):
			o.closed = true
		case key.Matches(msg, o.keys.Yes):
			o.closed = true
			if o.yes != nil {
				return o, o.yes()
			}
		}
	}
	return o, nil
}

func (o *editAsk) View(w, h int) string {
	th := o.e.th
	if o.gone {
		return editCut(editWrap(th.Over, editGone(o.acct.ID), w-1), h, 0, 0)
	}
	out := editWrap(th.Title, o.title, w-1)
	if o.body != "" {
		out = append(append(out, ""), editWrap(th.Text, o.body, w-1)...)
	}
	if o.acct != nil {
		if live := editLive(o.e.daemon.Live, o.acct.ID); live != "" {
			out = append(append(out, ""), editWrap(th.Warn, live, w-1)...)
		}
	}
	return editCut(out, h, 0, 0)
}

// editLive names the sessions the daemon holds on account id, or is "".
func editLive(live []Live, id int) string {
	var names []string
	for _, l := range live {
		if l.Account != id {
			continue
		}
		short := []rune(clean(l.ID))
		short = short[:min(len(short), 8)]
		if name := strings.TrimSpace(clean(l.Name)); name != "" {
			names = append(names, name+" ("+string(short)+")")
		} else {
			names = append(names, string(short))
		}
	}
	if len(names) == 0 {
		return ""
	}
	n := "1 live session"
	if len(names) > 1 {
		n = strconv.Itoa(len(names)) + " live sessions"
	}
	return n + " on it: " + strings.Join(names, ", ") + "; rota refuses to remove an account a CLI runs on"
}

// editRemoving removes acct, and says how that went: removed, already gone,
// or refused in the server's words. The listing is read again at once.
func editRemoving(e *env, acct wire.Account) tea.Cmd {
	b, id, name := e.b, acct.ID, editName(acct)
	failed := func(err error) tea.Msg {
		var no *client.Refusal
		if errors.As(err, &no) && no.Status == http.StatusNotFound {
			return tea.BatchMsg{toast(levelWarn, "%s", editGone(id)), editRefresh}
		}
		return toastMsg{Text: name + " was not removed: " + err.Error(), Level: levelErr}
	}
	return e.callOr(envCallTimeout, func(ctx context.Context) tea.Msg {
		if err := b.Remove(ctx, id); err != nil {
			return failed(err)
		}
		return tea.BatchMsg{toast(levelInfo, "removed %s", name), editRefresh}
	}, failed, nil)
}
