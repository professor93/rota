package tui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/wire"
)

// editModel is the Accounts tab over rows, started at w×h, with every
// Change answered with the account it names and every Remove taken.
func editModel(t *testing.T, rows []wire.Account, w, h int, with ...func(*Options)) (*Model, *fakeBackend) {
	t.Helper()
	m, b := accountsModel(t, rows, with...)
	b.ChangeFn = func(_ context.Context, id int, _ Change) (wire.Account, error) {
		for _, r := range rows {
			if r.ID == id {
				return r, nil
			}
		}
		return wire.Account{}, &client.Refusal{Status: 404, Message: fmt.Sprintf("no such account: %d", id)}
	}
	b.RemoveFn = func(context.Context, int) error { return nil }
	run(t, m, tea.BatchMsg{m.Init()}, size(w, h))
	return m, b
}

// editOverSSH makes Who answer a control principal that is not this
// machine's own.
func editOverSSH(b *fakeBackend) {
	b.WhoFn = func(context.Context) (Principal, error) {
		return Principal{Name: "alice", Role: "control", Via: "ssh"}, nil
	}
}

// editSaid is a Change as the fields it sends, by the API's names.
func editSaid(c Change) string {
	var parts []string
	if c.Order != nil {
		parts = append(parts, "order="+*c.Order)
	}
	if c.Threshold != nil {
		parts = append(parts, "threshold="+strconv.Itoa(*c.Threshold))
	}
	if c.Cwd != nil {
		parts = append(parts, "cwd="+*c.Cwd)
	}
	if c.ConfigDir != nil {
		parts = append(parts, "config_dir="+*c.ConfigDir)
	}
	if c.Sessions != nil {
		parts = append(parts, "sessions="+*c.Sessions)
	}
	if c.RemoteControl != nil {
		parts = append(parts, "remote_control="+strconv.FormatBool(*c.RemoteControl))
	}
	if c.ForgetLong {
		parts = append(parts, "long=forget")
	}
	return strings.Join(parts, " ")
}

// editCalls is every Change b took, as "id: fields".
func editCalls(b *fakeBackend) []string {
	var out []string
	for _, c := range b.named("Change") {
		out = append(out, fmt.Sprintf("%v: %s", c.Args[0], editSaid(c.Args[1].(Change))))
	}
	return out
}

// editScreen is the overlay on top as plain lines, drawn in the body; nil
// when none is open.
func editScreen(m *Model) []string {
	if len(m.stack) == 0 {
		return nil
	}
	_, h := m.body()
	view := accountsPlain(m.stack[len(m.stack)-1].View(m.e.width, h))
	return strings.Split(strings.TrimSuffix(view, "\n"), "\n")
}

// editJoined is lines as one text, every run of spaces one space, so a
// sentence reads the same however it was wrapped.
func editJoined(lines []string) string {
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// editRowAt matches a row of the form: two spaces, the focus mark or two
// more, and the label.
var editRowAt = regexp.MustCompile(`^  (> |  )(order|threshold|folder|config|sessions|remote control|forget long token|clear)( |$)`)

// editBeside is what is drawn under the row labelled label, up to the next
// row: its value's further lines, a refusal, a hint.
func editBeside(lines []string, label string) string {
	var out []string
	in := false
	for _, l := range lines {
		if m := editRowAt.FindStringSubmatch(l); m != nil {
			in = m[2] == label
			continue
		}
		if in && strings.HasPrefix(l, strings.Repeat(" ", 10)) {
			out = append(out, l)
		}
	}
	return editJoined(out)
}

// editAbove is what is drawn above the first row.
func editAbove(lines []string) string {
	var out []string
	for _, l := range lines {
		if editRowAt.MatchString(l) {
			break
		}
		out = append(out, l)
	}
	return editJoined(out)
}

// editFocused is the label of the row with focus, or "".
func editFocused(lines []string) string {
	for _, l := range lines {
		if m := editRowAt.FindStringSubmatch(l); m != nil && m[1] == "> " {
			return m[2]
		}
	}
	return ""
}

// editAfterChange is what was asked of b after its first Change.
func editAfterChange(b *fakeBackend) []string {
	calls := b.calls()
	for i, c := range calls {
		if strings.HasPrefix(c, "Change(") {
			return calls[i+1:]
		}
	}
	return nil
}

func TestEditSavesOnlyWhatChanged(t *testing.T) {
	for _, c := range []struct {
		name  string
		msgs  []tea.Msg
		want  []string
		error string // beside which row, with what, when the form stays
	}{
		{name: "threshold", msgs: press("tab", "ctrl+u", "8", "0", "enter"), want: []string{"1: threshold=80"}},
		{name: "a percent sign", msgs: press("tab", "ctrl+u", "8", "0", "%", "enter"), want: []string{"1: threshold=80"}},
		{name: "order", msgs: press("ctrl+u", "u", "p", "enter"), want: []string{"1: order=up"}},
		{name: "folder", msgs: append(press("tab", "tab"), tea.PasteMsg{Content: "/srv/work"}, press("enter")[0]),
			want: []string{"1: cwd=/srv/work"}},
		{name: "config", msgs: append(press("tab", "tab", "tab"), tea.PasteMsg{Content: "/srv/conf\n"}, press("enter")[0]),
			want: []string{"1: config_dir=/srv/conf"}},
		{name: "sessions", msgs: press("tab", "tab", "tab", "tab", "ctrl+u", "o", "w", "n", "enter"),
			want: []string{"1: sessions=own"}},
		{name: "remote control", msgs: press("shift+tab", "shift+tab", "space", "enter"),
			want: []string{"1: remote_control=true"}},
		{name: "keys the shell would take", msgs: press("tab", "tab", "/", "q", "?", "M", "1", "]", "enter"),
			want: []string{"1: cwd=/q?M1]"}},
		{name: "sessions emptied", msgs: press("tab", "tab", "tab", "tab", "ctrl+u", "enter")},
		{name: "two at once", msgs: append(press("tab", "ctrl+u", "8", "0", "tab"), tea.PasteMsg{Content: "/srv/work"}, press("enter")[0]),
			want: []string{"1: threshold=80 cwd=/srv/work"}},
		{name: "typed back as it was", msgs: press("tab", "ctrl+u", "7", "5", "enter")},
		{name: "the same number spelled otherwise", msgs: press("tab", "ctrl+u", "0", "7", "5", "enter")},
		{name: "switched back as it was", msgs: press("shift+tab", "shift+tab", "space", "space", "enter")},
		{name: "nothing", msgs: press("enter")},
		{name: "cancelled", msgs: press("tab", "ctrl+u", "8", "0", "esc")},
		{name: "not a number", msgs: press("tab", "ctrl+u", "x", "enter"),
			error: "threshold: threshold must be a percentage from 1 to 100"},
		{name: "out of range", msgs: press("tab", "ctrl+u", "0", "enter"),
			error: "threshold: threshold must be a percentage from 1 to 100"},
		{name: "a key", msgs: append(press("tab", "tab"), tea.PasteMsg{Content: backendHTTPKey}, press("enter")[0]),
			error: "folder: that value looks like a key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, b := editModel(t, accountsThree(), 80, 24)
			run(t, m, press("e")...)
			if len(m.stack) != 1 || editFocused(editScreen(m)) != "order" {
				t.Fatalf("e opened %d overlays, focused on %q:\n%s", len(m.stack), editFocused(editScreen(m)), strings.Join(editScreen(m), "\n"))
			}
			run(t, m, c.msgs...)
			if got := editCalls(b); !slices.Equal(got, c.want) {
				t.Errorf("sent %q, not %q", got, c.want)
			}
			if c.error != "" {
				label, say, _ := strings.Cut(c.error, ": ")
				if len(m.stack) != 1 {
					t.Fatalf("the form closed on a value it cannot send")
				}
				if got := editBeside(editScreen(m), label); !strings.Contains(got, say) {
					t.Errorf("beside %s: %q, not %q", label, got, say)
				}
				_, h := m.body()
				if strings.Contains(m.stack[0].View(80, h), "TESTONLY") {
					t.Errorf("a key is drawn")
				}
				return
			}
			if len(m.stack) != 0 {
				t.Errorf("the form is still open:\n%s", strings.Join(editScreen(m), "\n"))
			}
			if c.want == nil {
				return
			}
			// What is saved is said, and the listing read again at once.
			if after := editAfterChange(b); !slices.Contains(after, "Accounts(none)") {
				t.Errorf("after the change the backend was asked %q, with no listing", after)
			}
			if got := plain(m); !strings.Contains(got, "saved claude account 1 (work@example.com)") {
				t.Errorf("no toast says it was saved:\n%s", got)
			}
		})
	}
}

// An account out of the rotation reads out, and 0 is the same place.
func TestEditOfAnAccountOutOfTheRotation(t *testing.T) {
	rows := accountsThree()
	rows[1].Order = 0
	m, b := editModel(t, rows, 80, 24)
	run(t, m, press("down", "e")...)
	if got := editScreen(m); !strings.Contains(got[editRowIndex(got, "order")], "out") {
		t.Errorf("the order reads %q", got[editRowIndex(got, "order")])
	}
	run(t, m, press("ctrl+u", "0", "enter")...)
	if got := editCalls(b); len(got) != 0 || len(m.stack) != 0 {
		t.Errorf("0 for out sent %q", got)
	}
	run(t, m, press("e")...)
	run(t, m, press("ctrl+u", "1", "enter")...)
	if got := editCalls(b); !slices.Equal(got, []string{"2: order=1"}) {
		t.Errorf("putting it back sent %q", got)
	}
}

// editRowIndex is the line of the row labelled label, or -1.
func editRowIndex(lines []string, label string) int {
	return slices.IndexFunc(lines, func(l string) bool {
		m := editRowAt.FindStringSubmatch(l)
		return m != nil && m[2] == label
	})
}

// A field is edited anywhere in it, and its cursor is drawn where it
// stands.
func TestEditFieldEditsAnywhere(t *testing.T) {
	m, b := editModel(t, accountsThree(), 80, 24)
	run(t, m, press("e", "e")...)
	if len(m.stack) != 1 {
		t.Fatalf("e e opened %d forms", len(m.stack))
	}
	run(t, m, press("tab", "tab", "tab", "tab")...)
	if lines := editScreen(m); !strings.Contains(lines[editRowIndex(lines, "sessions")], "shared") {
		t.Errorf("default sessions read %q", lines[editRowIndex(lines, "sessions")])
	}
	run(t, m, press("shift+tab", "shift+tab", "/", "s", "r", "v", "/", "w", "r", "k", "left", "left", "o")...)
	_, h := m.body()
	cursor := m.e.th.Selected.Render("r")
	if view := m.stack[0].View(80, h); !strings.Contains(view, "/srv/wo"+cursor+"k") {
		t.Errorf("the cursor is not on the r:\n%q", view)
	}
	run(t, m, press("enter")...)
	if got := editCalls(b); !slices.Equal(got, []string{"1: cwd=/srv/work"}) {
		t.Errorf("sent %q", got)
	}
}

func TestEditShowsTheRefusalBesideItsField(t *testing.T) {
	t.Run("the API's own", func(t *testing.T) {
		dir := seedStore(t, "")
		rig := backendHTTPServe(t, api.RoleControl, dir)
		shellRegistry(t)
		register(tabsAccounts, newAccounts)
		m := shellModel(t, rig.b, &fakeTicks{clock: accountsNow})
		run(t, m, tea.BatchMsg{m.Init()}, size(120, 40))
		run(t, m, press("e")...)
		run(t, m, press("tab", "tab", "tab")...)
		// The store's own directory is no account's configuration.
		run(t, m, tea.PasteMsg{Content: dir}, press("enter")[0])
		lines := editScreen(m)
		if len(m.stack) != 1 {
			t.Fatalf("the form closed on a refusal")
		}
		got := editBeside(lines, "config")
		if !strings.Contains(got, "config_dir") || !strings.Contains(got, "that directory is rota's own") {
			t.Errorf("beside config: %q\n%s", got, strings.Join(lines, "\n"))
		}
		if strings.Contains(editAbove(lines), "rota's own") || strings.Contains(editBeside(lines, "folder"), "rota's own") {
			t.Errorf("the refusal is said elsewhere too:\n%s", strings.Join(lines, "\n"))
		}
		if f := editFocused(lines); f != "config" {
			t.Errorf("the focus is on %q, not on the field refused", f)
		}
		l, err := rig.b.Accounts(context.Background(), RefreshNone)
		if err != nil || l.Accounts[0].ConfigDir != "" {
			t.Errorf("the account was changed anyway: %+v %v", l.Accounts, err)
		}
		if body := rig.rec.last(t, "PATCH", "/v1/accounts/1").Body; body != `{"config_dir":"`+dir+`"}` {
			t.Errorf("the PATCH sent %s", body)
		}
	})
	for _, c := range []struct {
		name, refusal string
		status        int
		msgs          []tea.Msg
		where         string
	}{
		{name: "named among two", status: 400, refusal: `cwd must be an absolute path, got "srv"`,
			msgs: append(press("tab", "ctrl+u", "8", "0", "tab"), tea.PasteMsg{Content: "srv"}, press("enter")[0]), where: "folder"},
		{name: "a place", status: 400, refusal: `invalid request: "sideways" is not a place; use a number, 0 or out, first, last, up, down, before:<id> or after:<id>`,
			msgs: append(press("ctrl+u", "s", "i", "d", "e", "w", "a", "y", "s", "tab", "ctrl+u", "8", "0"), press("enter")[0]), where: "order"},
		{name: "remote control", status: 400, refusal: `claude account 1 (orders@example.com) cannot have Remote Control: it runs on a token because its configuration directory is one`,
			msgs: append(append(press("tab", "tab", "tab"), tea.PasteMsg{Content: "/srv/conf"}), press("tab", "tab", "space", "enter")...), where: "remote control"},
		{name: "the first named", status: 400, refusal: `cwd "/srv" is the same directory as config_dir`,
			msgs: append(append(press("tab", "tab"), tea.PasteMsg{Content: "/srv"}, press("tab")[0], tea.PasteMsg{Content: "/srv"}), press("enter")...), where: "folder"},
		{name: "the only one sent", status: 409, refusal: "something is in the way",
			msgs: press("tab", "tab", "tab", "tab", "ctrl+u", "o", "w", "n", "enter"), where: "sessions"},
		{name: "none named among two", status: 409, refusal: "account is already running: Claude Code runs there",
			msgs: append(press("tab", "ctrl+u", "8", "0", "tab"), tea.PasteMsg{Content: "/srv"}, press("enter")[0]), where: "top"},
		{name: "no answer", refusal: "the rota API did not answer PATCH /v1/accounts/1: connection refused",
			msgs: append(press("tab", "ctrl+u", "8", "0", "tab"), tea.PasteMsg{Content: "/srv"}, press("enter")[0]), where: "top"},
		{name: "no answer, one field", refusal: "the rota API did not answer PATCH /v1/accounts/1: connection refused",
			msgs: press("tab", "tab", "tab", "tab", "ctrl+u", "o", "w", "n", "enter"), where: "top"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, b := editModel(t, accountsThree(), 120, 40)
			b.ChangeFn = func(context.Context, int, Change) (wire.Account, error) {
				if c.status == 0 {
					return wire.Account{}, errors.New(c.refusal)
				}
				return wire.Account{}, &client.Refusal{Status: c.status, Message: c.refusal}
			}
			run(t, m, press("e")...)
			run(t, m, c.msgs...)
			lines := editScreen(m)
			if len(m.stack) != 1 || len(b.named("Change")) != 1 {
				t.Fatalf("%d overlays open after %d changes", len(m.stack), len(b.named("Change")))
			}
			at := map[string]string{"top": editAbove(lines)}
			for _, l := range []string{"order", "threshold", "folder", "config", "sessions", "remote control", "clear"} {
				at[l] = editBeside(lines, l)
			}
			for where, got := range at {
				if says := strings.Contains(got, editJoined([]string{c.refusal})); says != (where == c.where) {
					t.Errorf("%s says %q\n%s", where, got, strings.Join(lines, "\n"))
				}
			}
			if f := editFocused(lines); c.where != "top" && f != c.where {
				t.Errorf("the focus is on %q, not on the row refused", f)
			}
		})
	}
	t.Run("cleaned", func(t *testing.T) {
		m, b := editModel(t, accountsThree(), 120, 40)
		b.ChangeFn = func(context.Context, int, Change) (wire.Account, error) {
			return wire.Account{}, &client.Refusal{Status: 400, Message: "cwd \x1b[2J\x1b]52;c;aGk=\a was " + backendHTTPKey}
		}
		run(t, m, press("e")...)
		run(t, m, press("tab", "tab", "x", "enter")...)
		_, h := m.body()
		view := m.stack[0].View(120, h)
		if strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\x1b]") || strings.Contains(view, "TESTONLY") {
			t.Errorf("the refusal was drawn as it came:\n%q", view)
		}
		if got := editBeside(editScreen(m), "folder"); !strings.Contains(got, "was") {
			t.Errorf("beside folder: %q", got)
		}
	})
}

// editSet is account 1 with every setting rota set clears given.
func editSet() []wire.Account {
	rows := accountsThree()
	rows[0].Cwd, rows[0].ConfigDir, rows[0].Sessions, rows[0].RemoteControl = "/srv/work", "/srv/conf", "own", true
	return rows
}

func TestClearAsksFirstAndSendsTheFour(t *testing.T) {
	m, b := editModel(t, editSet(), 80, 24)
	run(t, m, press("e")...)
	run(t, m, press("shift+tab")...)
	if f := editFocused(editScreen(m)); f != "clear" {
		t.Fatalf("shift+tab from the first row reached %q", f)
	}
	run(t, m, press("enter")...)
	if len(m.stack) != 2 || len(b.named("Change")) != 0 {
		t.Fatalf("clear opened %d overlays and sent %q", len(m.stack), editCalls(b))
	}
	ask := editJoined(editScreen(m))
	for _, want := range []string{"Clear the settings of claude account 1 (work@example.com)?", "rota set 1 --clear"} {
		if !strings.Contains(ask, want) {
			t.Errorf("the confirm does not say %q: %s", want, ask)
		}
	}
	// n keeps everything, and the form is there again.
	run(t, m, press("n")...)
	if len(m.stack) != 1 || len(b.named("Change")) != 0 || editFocused(editScreen(m)) != "clear" {
		t.Fatalf("n left %d overlays, sent %q", len(m.stack), editCalls(b))
	}
	run(t, m, press("enter")...)
	run(t, m, press("y")...)
	if got := editCalls(b); !slices.Equal(got, []string{"1: cwd= config_dir= sessions=shared remote_control=false"}) {
		t.Errorf("clear sent %q", got)
	}
	if len(m.stack) != 0 {
		t.Errorf("%d overlays are left open", len(m.stack))
	}
	if after := editAfterChange(b); !slices.Contains(after, "Accounts(none)") {
		t.Errorf("after clearing the backend was asked %q", after)
	}

	t.Run("over SSH", func(t *testing.T) {
		m, b := editModel(t, editSet(), 80, 24)
		editOverSSH(b)
		run(t, m, modelWhoMsg{Who: Principal{Name: "alice", Role: "control", Via: "ssh"}})
		run(t, m, press("e")...)
		run(t, m, press("shift+tab", "enter")...)
		if ask := editJoined(editScreen(m)); !strings.Contains(ask, "rota set 1 --remote-control off") {
			t.Errorf("the confirm does not say where Remote Control changes: %s", ask)
		}
		run(t, m, press("y")...)
		if got := editCalls(b); !slices.Equal(got, []string{"1: cwd= config_dir= sessions=shared"}) {
			t.Errorf("clear over SSH sent %q", got)
		}
	})
	t.Run("refused", func(t *testing.T) {
		m, b := editModel(t, editSet(), 80, 24)
		b.ChangeFn = func(context.Context, int, Change) (wire.Account, error) {
			return wire.Account{}, &client.Refusal{Status: 409, Message: "account is already running: Claude Code runs in its home"}
		}
		run(t, m, press("e")...)
		run(t, m, press("shift+tab", "enter")...)
		run(t, m, press("y")...)
		if len(m.stack) != 1 || !strings.Contains(editAbove(editScreen(m)), "Claude Code runs in its home") {
			t.Errorf("a refused clear left %d overlays:\n%s", len(m.stack), strings.Join(editScreen(m), "\n"))
		}
	})
}

func TestForgetLongShowsOnlyWithALongToken(t *testing.T) {
	m, b := editModel(t, accountsThree(), 80, 24)
	run(t, m, press("e")...)
	if got := strings.Join(editScreen(m), "\n"); strings.Contains(got, "forget") {
		t.Errorf("an account with no long-lived token is offered to forget one:\n%s", got)
	}
	// Seven rows, and tab goes round them all.
	var seen []string
	for range 7 {
		seen = append(seen, editFocused(editScreen(m)))
		run(t, m, press("tab")...)
	}
	if want := []string{"order", "threshold", "folder", "config", "sessions", "remote control", "clear"}; !slices.Equal(seen, want) {
		t.Errorf("tab went %q", seen)
	}
	if f := editFocused(editScreen(m)); f != "order" {
		t.Errorf("tab after the last row reached %q", f)
	}
	run(t, m, press("esc")...)

	rows := accountsThree()
	rows[0].LongUntil = accountsAt(300 * 24 * time.Hour)
	m, b = editModel(t, rows, 80, 24)
	run(t, m, press("e")...)
	run(t, m, press("shift+tab", "shift+tab")...)
	lines := editScreen(m)
	if f := editFocused(lines); f != "forget long token" {
		t.Fatalf("the row before clear is %q:\n%s", f, strings.Join(lines, "\n"))
	}
	if got := editJoined(lines); !strings.Contains(got, "2027-09-06 20:17") {
		t.Errorf("the form does not say until when the token lasts:\n%s", strings.Join(lines, "\n"))
	}
	// Marked and unmarked sends nothing; marked sends long=forget alone.
	run(t, m, press("space", "space", "enter")...)
	if got := editCalls(b); len(got) != 0 || len(m.stack) != 0 {
		t.Errorf("unmarked, the form sent %q and left %d open", got, len(m.stack))
	}
	run(t, m, press("e")...)
	run(t, m, press("shift+tab", "shift+tab", "space")...)
	if got := editBeside(editScreen(m), "forget long token") + strings.Join(editScreen(m), "\n"); !strings.Contains(got, "[x]") {
		t.Errorf("the mark does not show:\n%s", strings.Join(editScreen(m), "\n"))
	}
	run(t, m, press("enter")...)
	if got := editCalls(b); !slices.Equal(got, []string{"1: long=forget"}) {
		t.Errorf("forgetting sent %q", got)
	}
}

// A control principal that is not this machine's own never sends
// remote_control: the server takes a change of it only over the control
// socket. Its row says where it is changed instead.
func TestEditOverSSHNeverSendsRemoteControl(t *testing.T) {
	m, b := editModel(t, accountsThree(), 120, 40)
	editOverSSH(b)
	run(t, m, modelWhoMsg{Who: Principal{Name: "alice", Role: "control", Via: "ssh"}})
	run(t, m, press("e")...)
	lines := editScreen(m)
	for _, l := range lines {
		if m := editRowAt.FindStringSubmatch(l); m != nil && m[2] == "remote control" {
			if !strings.Contains(l, "changed on this machine: rota set 1 --remote-control on") {
				t.Errorf("the row reads %q", l)
			}
		}
	}
	var seen []string
	for range 6 {
		seen = append(seen, editFocused(editScreen(m)))
		run(t, m, press("tab", "space")...)
	}
	if want := []string{"order", "threshold", "folder", "config", "sessions", "clear"}; !slices.Equal(seen, want) {
		t.Errorf("tab went %q", seen)
	}
	run(t, m, press("tab", "ctrl+u", "8", "0", "enter")...)
	if got := editCalls(b); !slices.Equal(got, []string{"1: threshold=80"}) {
		t.Errorf("over SSH the form sent %q", got)
	}

	rows := accountsThree()
	rows[0].RemoteControl = true
	m, b = editModel(t, rows, 120, 40)
	editOverSSH(b)
	run(t, m, modelWhoMsg{Who: Principal{Name: "alice", Role: "control", Via: "ssh"}})
	run(t, m, press("e")...)
	if got := editJoined(editScreen(m)); !strings.Contains(got, "on · changed on this machine: rota set 1 --remote-control off") {
		t.Errorf("an account with it on: %s", got)
	}
	// Locally it is a switch, and a codex account has no row for it.
	m, _ = editModel(t, accountsThree(), 120, 40)
	run(t, m, press("e")...)
	if got := editJoined(editScreen(m)); strings.Contains(got, "changed on this machine") || !strings.Contains(got, "remote control off") {
		t.Errorf("locally: %s", got)
	}
	run(t, m, press("esc", "down", "down", "e")...)
	if got := editJoined(editScreen(m)); !strings.Contains(got, "codex account 3") || strings.Contains(got, "remote control") {
		t.Errorf("a codex account: %s", got)
	}
}

func TestRemoveNamesLiveSessions(t *testing.T) {
	live := daemonMsg{Daemon: Daemon{Running: true, Live: []Live{
		{ID: "a1b2c3d4e5f60718", Name: "build", Account: 3},
		{ID: "e5f6a7b8c9d0e1f2", Name: "chat\x1b[2J", Account: 3},
		{ID: "ffff0000aaaabbbb", Name: "other", Account: 1},
		{ID: "0123456789abcdef", Name: "\x1b]0;x\a", Account: 3},
	}}}
	m, b := editModel(t, accountsThree(), 120, 40)
	run(t, m, live)
	run(t, m, press("down", "down", "d")...)
	got := editJoined(editScreen(m))
	for _, want := range []string{
		"Remove codex account 3 (cdx@example.com)? Its home and staged credentials go with it.",
		"3 live sessions on it: build (a1b2c3d4), chat (e5f6a7b8), 01234567; rota refuses to remove an account a CLI runs on",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the ask does not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "other") {
		t.Errorf("a session on another account is named: %s", got)
	}
	_, h := m.body()
	if v := m.stack[0].View(120, h); strings.Contains(v, "\x1b[2J") {
		t.Errorf("a session's name was drawn as it came: %q", v)
	}
	// One session, and then none: said as it stands.
	run(t, m, daemonMsg{Daemon: Daemon{Running: true, Live: live.Daemon.Live[:1]}})
	if got := editJoined(editScreen(m)); !strings.Contains(got, "1 live session on it: build (a1b2c3d4); rota refuses") {
		t.Errorf("one session: %s", got)
	}
	run(t, m, daemonMsg{})
	if got := editJoined(editScreen(m)); strings.Contains(got, "live session") {
		t.Errorf("no session: %s", got)
	}
	// n and esc keep it.
	run(t, m, press("n")...)
	run(t, m, press("d")...)
	run(t, m, press("esc")...)
	if len(m.stack) != 0 || len(b.named("Remove")) != 0 {
		t.Fatalf("n and esc left %d open and removed %v", len(m.stack), b.named("Remove"))
	}
	// y removes it, says so and reads the listing again.
	run(t, m, press("d")...)
	run(t, m, press("x", "enter", "space")...)
	if len(m.stack) != 1 || len(b.named("Remove")) != 0 {
		t.Fatalf("a key that is not y or n did something")
	}
	run(t, m, press("y")...)
	if got := b.named("Remove"); len(got) != 1 || got[0].String() != "Remove(3)" || len(m.stack) != 0 {
		t.Fatalf("y removed %v and left %d open", got, len(m.stack))
	}
	calls := b.calls()
	if i := slices.Index(calls, "Remove(3)"); !slices.Contains(calls[i:], "Accounts(none)") {
		t.Errorf("after removing, the backend was asked %q", calls[i:])
	}
	if got := plain(m); !strings.Contains(got, "removed codex account 3 (cdx@example.com)") {
		t.Errorf("nothing says it was removed:\n%s", got)
	}
	// A refusal is a toast, in the server's words.
	b.RemoveFn = func(context.Context, int) error {
		return &client.Refusal{Status: 409, Message: "account is already running: a CLI runs on it"}
	}
	run(t, m, press("d")...)
	run(t, m, press("y")...)
	if got := plain(m); !strings.Contains(got, "codex account 3 (cdx@example.com) was not removed: account is already running") {
		t.Errorf("a refused remove:\n%s", got)
	}
	// Pressed twice before it opens, it opens once.
	run(t, m, press("d", "d")...)
	if len(m.stack) != 1 {
		t.Errorf("d d opened %d", len(m.stack))
	}
}

func TestAnEditOfAnAccountRemovedElsewhereSaysSo(t *testing.T) {
	rows := accountsThree()
	gone := "account 3 is gone (removed elsewhere)"
	t.Run("edit", func(t *testing.T) {
		m, b := editModel(t, rows, 80, 24)
		run(t, m, press("down", "down", "e")...)
		run(t, m, listingMsg{Err: errors.New("no answer")})
		if got := editJoined(editScreen(m)); strings.Contains(got, "gone") {
			t.Errorf("a failed read said it is gone: %s", got)
		}
		run(t, m, accountsListing(rows[:2]...))
		if got := editJoined(editScreen(m)); !strings.Contains(got, gone) {
			t.Errorf("the form says: %s", got)
		}
		run(t, m, press("tab", "8", "space", "y")...)
		if len(m.stack) != 1 || len(b.named("Change")) != 0 {
			t.Fatalf("a gone account was edited: %d open, %q", len(m.stack), editCalls(b))
		}
		run(t, m, press("enter")...)
		if len(m.stack) != 0 {
			t.Errorf("enter did not close it")
		}
	})
	t.Run("remove", func(t *testing.T) {
		m, b := editModel(t, rows, 80, 24)
		run(t, m, press("down", "down", "d")...)
		run(t, m, accountsListing(rows[:2]...))
		if got := editJoined(editScreen(m)); !strings.Contains(got, gone) {
			t.Errorf("the ask says: %s", got)
		}
		run(t, m, press("y")...)
		if len(m.stack) != 1 || len(b.named("Remove")) != 0 {
			t.Fatalf("y on a gone account: %d open, %v", len(m.stack), b.named("Remove"))
		}
		run(t, m, press("enter")...)
		if len(m.stack) != 0 {
			t.Errorf("enter did not close it")
		}
	})
	t.Run("a read that refreshed usage", func(t *testing.T) {
		for _, open := range []string{"e", "d"} {
			m, b := editModel(t, rows, 80, 24)
			run(t, m, press("down", "down", open)...)
			// Begun before account 3 was made, it lacks it; the cache has it.
			before := len(b.named("Accounts"))
			run(t, m, listingMsg{Listing: Listing{Accounts: rows[:2]}, How: RefreshStale})
			if got := editJoined(editScreen(m)); strings.Contains(got, "gone") {
				t.Errorf("%s: a slow read made it gone: %s", open, got)
			}
			if asked := b.named("Accounts")[before:]; len(asked) != 1 || asked[0].String() != "Accounts(none)" {
				t.Errorf("%s: then asked %v", open, asked)
			}
			// Now it is gone from the cache too.
			b.AccountsFn = func(context.Context, Refresh) (Listing, error) { return Listing{Accounts: rows[:2]}, nil }
			run(t, m, listingMsg{Listing: Listing{Accounts: rows[:2]}, How: RefreshForce})
			if got := editJoined(editScreen(m)); !strings.Contains(got, gone) {
				t.Errorf("%s: gone from the cache too, it says: %s", open, got)
			}
		}
	})
	t.Run("a save that races it", func(t *testing.T) {
		m, b := editModel(t, rows, 80, 24)
		b.ChangeFn = func(_ context.Context, id int, _ Change) (wire.Account, error) {
			return wire.Account{}, &client.Refusal{Status: 404, Message: "no such account: 3"}
		}
		run(t, m, press("down", "down", "e")...)
		run(t, m, press("tab", "ctrl+u", "9", "0", "enter")...)
		got := editJoined(editScreen(m))
		if !strings.Contains(got, gone) || !strings.Contains(got, "no such account: 3") {
			t.Errorf("after a 404 the form says: %s", got)
		}
		run(t, m, press("enter")...)
		if len(m.stack) != 0 {
			t.Errorf("enter did not close it")
		}
	})
	t.Run("under the confirm", func(t *testing.T) {
		m, b := editModel(t, rows, 80, 24)
		run(t, m, press("e")...)
		run(t, m, press("shift+tab", "enter")...)
		run(t, m, accountsListing(rows[1:]...))
		run(t, m, press("y")...)
		if got := editCalls(b); len(got) != 0 {
			t.Errorf("a clear of a gone account was sent: %q", got)
		}
		if got := editJoined(editScreen(m)); !strings.Contains(got, "account 1 is gone (removed elsewhere)") {
			t.Errorf("the form under the confirm says: %s", got)
		}
	})
}

// While a save is out the form takes no key, and its end reaches the form
// that sent it and no other.
func TestEditWaitsForItsSave(t *testing.T) {
	rows := accountsThree()
	m, b := editModel(t, rows, 80, 24)
	hold := make(chan error)
	end := func(err error) {
		t.Helper()
		select {
		case hold <- err:
		case <-time.After(5 * time.Second):
			t.Fatalf("no save is waiting for its end")
		}
		await(t, m)
	}
	b.ChangeFn = func(ctx context.Context, _ int, _ Change) (wire.Account, error) {
		select {
		case err := <-hold:
			return wire.Account{}, err
		case <-ctx.Done():
			return wire.Account{}, ctx.Err()
		}
	}
	run(t, m, press("e")...)
	run(t, m, press("tab", "ctrl+u", "8", "0", "enter")...)
	if got := editJoined(editScreen(m)); !strings.Contains(got, "saving…") {
		t.Errorf("a save under way is not said: %s", got)
	}
	run(t, m, press("9", "enter", "esc", "shift+tab")...)
	if len(m.stack) != 1 || len(b.named("Change")) != 1 || m.typing() {
		t.Fatalf("keys while saving: %d open, %q sent, typing %v", len(m.stack), editCalls(b), m.typing())
	}
	end(&client.Refusal{Status: 409, Message: "busy now"})
	lines := editScreen(m)
	if l := lines[editRowIndex(lines, "threshold")]; !strings.Contains(l, "80") || strings.Contains(l, "809") {
		t.Errorf("a key typed while saving reached the field: %q", l)
	}
	if got := editBeside(lines, "threshold"); !strings.Contains(got, "busy now") {
		t.Errorf("beside threshold: %q", got)
	}
	// Removed elsewhere while a save is out, then closed, and another
	// account's form opened: the old save's end is not the new form's.
	run(t, m, press("enter")...)
	run(t, m, accountsListing(rows[1:]...))
	run(t, m, press("enter")...)
	run(t, m, press("e")...)
	end(&client.Refusal{Status: 404, Message: "no such account: 1"})
	if got := editJoined(editScreen(m)); strings.Contains(got, "gone") || !strings.Contains(got, "Edit claude account 2") {
		t.Errorf("the new form took the old save's end: %s", got)
	}
}

func TestConfirmIsYesOrNo(t *testing.T) {
	m, _ := editModel(t, accountsThree(), 80, 24)
	said := 0
	yes := func() tea.Cmd {
		said++
		return toast(levelInfo, "yes ran")
	}
	ask := func() tea.Msg {
		return openMsg{O: newConfirm(m.e, "Stop the \x1b]0;evil\x07session?", "It ends \x1b[2Jnow.", yes)}
	}
	run(t, m, ask())
	lines := editScreen(m)
	if got := editJoined(lines); !strings.Contains(got, "session?") || !strings.Contains(got, "now.") ||
		strings.Contains(got, "evil") || strings.Contains(got, "[2J") {
		t.Errorf("the confirm reads %q", got)
	}
	if got := plain(m); !strings.Contains(got, "y yes") || !strings.Contains(got, "n/esc no") {
		t.Errorf("the footer does not offer y and n:\n%s", got)
	}
	// The same confirm twice opens once.
	run(t, m, ask())
	if len(m.stack) != 1 {
		t.Errorf("the same confirm opened %d times", len(m.stack))
	}
	run(t, m, press("x", "enter", "space", "tab", "1")...)
	if len(m.stack) != 1 || said != 0 {
		t.Fatalf("a key that is neither y nor n: %d open, yes ran %d times", len(m.stack), said)
	}
	for _, no := range []string{"n", "N", "esc"} {
		run(t, m, press(no)...)
		if len(m.stack) != 0 || said != 0 {
			t.Fatalf("%s: %d open, yes ran %d times", no, len(m.stack), said)
		}
		run(t, m, ask())
	}
	run(t, m, press("y")...)
	if len(m.stack) != 0 || said != 1 {
		t.Fatalf("y: %d open, yes ran %d times", len(m.stack), said)
	}
	if got := plain(m); !strings.Contains(got, "yes ran") {
		t.Errorf("what yes returned was not run:\n%s", got)
	}
	run(t, m, ask())
	run(t, m, press("Y")...)
	if said != 2 {
		t.Errorf("Y ran yes %d times in all", said)
	}
}

// editHostile is the design's accounts with the first made hostile: a
// label of control sequences and 140 columns, a folder holding an OSC 52
// and a key, and sessions in a CJK directory.
func editHostile() []wire.Account {
	rows := accountsThree()
	rows[0].Name = "\x1b[2J\x1b]0;title\a" + strings.Repeat("wide 字 ", 20)
	rows[0].Cwd = "/srv/\x1b]52;c;aGk=\a/" + backendHTTPKey
	rows[0].Sessions = "/srv/会話/" + strings.Repeat("長い", 30)
	rows[0].LongUntil = accountsAt(100 * 24 * time.Hour)
	return rows
}

func TestEditDrawsWithinItsWidth(t *testing.T) {
	for _, sz := range [][2]int{{60, 14}, {80, 24}, {120, 40}} {
		t.Run(fmt.Sprintf("%dx%d", sz[0], sz[1]), func(t *testing.T) {
			m, b := editModel(t, editHostile(), sz[0], sz[1])
			b.ChangeFn = func(context.Context, int, Change) (wire.Account, error) {
				return wire.Account{}, &client.Refusal{Status: 400, Message: "cwd \x1b[31mis\x1b[0m " + strings.Repeat("long ", 40) + backendHTTPKey}
			}
			editOverSSH(b)
			run(t, m, modelWhoMsg{Who: Principal{Name: "alice", Role: "control", Via: "ssh"}}, live3())
			check := func(what string) {
				t.Helper()
				_, h := m.body()
				view := m.stack[len(m.stack)-1].View(sz[0], h)
				if n := strings.Count(view, "\n") + 1; n > h {
					t.Errorf("%s: %d lines in %d rows", what, n, h)
				}
				for _, l := range strings.Split(view, "\n") {
					if w := ansi.StringWidthWc(l); w > sz[0] {
						t.Errorf("%s: a line of %d columns in %d: %q", what, w, sz[0], ansi.Strip(l))
					}
					if w := ansi.StringWidth(l); w > sz[0] {
						t.Errorf("%s: a line of %d graphemes in %d: %q", what, w, sz[0], ansi.Strip(l))
					}
				}
				for _, bad := range []string{"\x1b[2J", "\x1b]", "\x1b[31m", "TESTONLY", "\a"} {
					if strings.Contains(view, bad) {
						t.Errorf("%s: %q is drawn", what, bad)
					}
				}
			}
			run(t, m, press("e")...)
			for i := range 8 {
				check(fmt.Sprintf("the form, row %d", i))
				if editFocused(editScreen(m)) == "" {
					t.Errorf("row %d has focus and is not drawn:\n%s", i, strings.Join(editScreen(m), "\n"))
				}
				run(t, m, press("tab")...)
			}
			run(t, m, press("esc")...)
			run(t, m, press("e")...)
			// A cursor anywhere in a field of wide characters.
			run(t, m, press("tab", "tab", "tab", "tab")...)
			if f := editFocused(editScreen(m)); f != "sessions" {
				t.Fatalf("four tabs reached %q", f)
			}
			for range 30 {
				run(t, m, press("left")...)
				check("sessions, the cursor moved left")
			}
			run(t, m, press("shift+tab", "shift+tab", "x", "enter")...)
			check("refused")
			// Under a long refusal every row still shows when it has focus.
			for i := range 8 {
				if editFocused(editScreen(m)) == "" {
					t.Errorf("row %d after the refusal has focus and is not drawn:\n%s", i, strings.Join(editScreen(m), "\n"))
				}
				run(t, m, press("tab")...)
			}
			run(t, m, press("esc")...)
			run(t, m, press("d")...)
			check("the remove")
			run(t, m, accountsListing(accountsThree()[1:]...))
			check("gone")
		})
	}
}

// live3 is a daemon holding a session on account 1 whose name is hostile.
func live3() daemonMsg {
	return daemonMsg{Daemon: Daemon{Running: true, Live: []Live{
		{ID: "\x1b[2J" + strings.Repeat("9", 40), Name: strings.Repeat("会話 ", 30) + "\x1b]0;x\a", Account: 1},
	}}}
}

func TestEditUpdateDoesNoIO(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	m, b := accountsModel(t, accountsThree())
	rows := accountsThree()
	rows[0].LongUntil = accountsAt(24 * time.Hour)
	keys := press("tab", "shift+tab", "up", "down", "space", "ctrl+u", "8", "0", "left", "right", "x", "y", "n", "N",
		"Y", "enter", "esc", "q", "1", "]")
	assertNoIO(t, b, func() {
		m.Init()
		for _, msg := range []tea.Msg{size(80, 24), modelWhoMsg{Who: Principal{Role: "control", Via: "local"}},
			accountsListing(rows...), live3()} {
			m.Update(msg)
		}
		for _, o := range []overlay{
			newEdit(m.e, rows[0]), newEdit(m.e, rows[2]), newRemove(m.e, rows[2]),
			newConfirm(m.e, "Stop?", "Now.", func() tea.Cmd { return nil }),
		} {
			m.Update(openMsg{O: o})
			msgs := append([]tea.Msg{tea.PasteMsg{Content: "/srv"}, tea.PasteStartMsg{}, tea.PasteEndMsg{}, accountsListing(rows...),
				listingMsg{Err: errors.New("x")}, daemonMsg{}, size(60, 14), tea.MouseClickMsg{X: 3, Y: 4, Button: tea.MouseLeft}},
				keys...)
			msgs = append(msgs, accountsListing(rows[:1]...), press("tab")[0], press("enter")[0])
			for _, msg := range msgs {
				m.Update(msg)
				m.View()
			}
			m.stack = nil
		}
	})
}
