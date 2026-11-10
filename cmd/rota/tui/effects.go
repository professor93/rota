package tui

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// Every terminal effect is asked for by a message of this package (R31):
// a test sees it among what the commands produced, and the shell turns it
// into Bubble Tea's own command, whose message is unexported.

// handOffMsg hands the terminal to Cmd until it ends; Done is told how.
type handOffMsg struct {
	Cmd  tea.ExecCommand
	Done func(error) tea.Msg
}

// clipboardMsg puts Text on the clipboard of the person's terminal, by OSC
// 52, which works over SSH too (R20).
type clipboardMsg struct{ Text string }

// bellMsg rings the terminal's bell.
type bellMsg struct{}

// handOff is the command that asks the shell to hand the terminal to c.
func handOff(c tea.ExecCommand, done func(error) tea.Msg) tea.Cmd {
	return func() tea.Msg { return handOffMsg{Cmd: c, Done: done} }
}

// copyText is the command that asks for text to be put on the clipboard.
func copyText(text string) tea.Cmd {
	return func() tea.Msg { return clipboardMsg{Text: text} }
}

// openURL is the command that opens url in a browser on this machine, or
// says why it cannot: there is none (R19), or it failed.
func openURL(e *env, url string) tea.Cmd {
	open := e.o.OpenURL
	return func() tea.Msg {
		if open == nil {
			return toastMsg{Text: "no browser on this machine; copy the link instead", Level: levelWarn}
		}
		if err := open(url); err != nil {
			return toastMsg{Text: "the browser did not open: " + err.Error(), Level: levelErr}
		}
		return nil
	}
}

// effect turns a terminal effect into Bubble Tea's own command.
func (m *Model) effect(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case handOffMsg:
		done := msg.Done
		if done == nil {
			done = func(error) tea.Msg { return nil }
		}
		return tea.Exec(msg.Cmd, done), true
	case clipboardMsg:
		return tea.SetClipboard(msg.Text), true
	case bellMsg:
		return tea.Raw("\a"), true
	}
	return nil, false
}

// effectsTailSize is how much of a command's stderr tailed keeps.
const effectsTailSize = 4 << 10

// tailed is a command handed the terminal that keeps the last 4 KiB of
// what it writes to stderr, so a failure can be said in a toast after the
// alternate screen comes back and hides it (R28). Everything still reaches
// the writer the program set.
type tailed struct {
	c      *exec.Cmd
	stderr io.Writer
	tail   *effectsRing
}

// execTail wraps c to keep the tail of its stderr.
func execTail(c *exec.Cmd) *tailed {
	return &tailed{c: c, tail: &effectsRing{}}
}

// SetStdin, SetStdout and SetStderr are the terminal Bubble Tea hands over.
// A stream c already has is kept, as Bubble Tea's own does.
func (t *tailed) SetStdin(r io.Reader) {
	if t.c.Stdin == nil {
		t.c.Stdin = r
	}
}

func (t *tailed) SetStdout(w io.Writer) {
	if t.c.Stdout == nil {
		t.c.Stdout = w
	}
}

func (t *tailed) SetStderr(w io.Writer) { t.stderr = w }

func (t *tailed) Run() error {
	w := t.stderr
	if t.c.Stderr != nil {
		w = t.c.Stderr
	}
	t.c.Stderr = effectsTee{tail: t.tail, w: w}
	return t.c.Run()
}

// Last is the last line of the tail that is not blank, trimmed.
func (t *tailed) Last() string {
	lines := strings.Split(strings.ReplaceAll(t.tail.String(), "\r", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// effectsTee writes to the tail first, then to w, and answers as w does:
// what the command says reaches the terminal exactly as without the tail.
type effectsTee struct {
	tail *effectsRing
	w    io.Writer
}

func (t effectsTee) Write(p []byte) (int, error) {
	t.tail.Write(p)
	if t.w == nil {
		return len(p), nil
	}
	return t.w.Write(p)
}

// effectsRing keeps the last effectsTailSize bytes written to it.
type effectsRing struct {
	mu  sync.Mutex
	buf []byte
}

func (r *effectsRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= effectsTailSize {
		r.buf = append(r.buf[:0], p[len(p)-effectsTailSize:]...)
		return len(p), nil
	}
	if over := len(r.buf) + len(p) - effectsTailSize; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	r.buf = append(r.buf, p...)
	return len(p), nil
}

func (r *effectsRing) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(bytes.Clone(r.buf))
}

func (r *effectsRing) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buf)
}
