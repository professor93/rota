//go:build linux || darwin

package sshd

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/cmd/rota/attach"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/vtscan"
)

/* ------------------------------------------------------------ helpers --- */

// attachBuf is what an SSH session has written, read while it writes.
type attachBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *attachBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *attachBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// attachSSH is one command running on a session channel with a pty, as
// `ssh -t host <line>` runs it: what is typed goes to its stdin, and its
// stdout and stderr are kept apart.
type attachSSH struct {
	t        *testing.T
	sess     *ssh.Session
	in       io.WriteCloser
	out, err attachBuf
	done     chan error
}

// attachOpen starts line on a fresh session channel of c with a pty of
// cols×rows.
func attachOpen(t *testing.T, c *ssh.Client, line string, cols, rows int) *attachSSH {
	t.Helper()
	a := &attachSSH{t: t, done: make(chan error, 1)}
	sshdEventually(t, func() (err error) {
		a.sess, err = c.NewSession()
		return err
	})
	t.Cleanup(func() { a.sess.Close() })
	a.sess.Stdout, a.sess.Stderr = &a.out, &a.err
	var err error
	if a.in, err = a.sess.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := a.sess.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := a.sess.Start(line); err != nil {
		t.Fatal(err)
	}
	go func() { a.done <- a.sess.Wait() }()
	return a
}

// typ types s, as the person's terminal sends it.
func (a *attachSSH) typ(s string) {
	a.t.Helper()
	if _, err := io.WriteString(a.in, s); err != nil {
		a.t.Fatalf("typing %q: %v", s, err)
	}
}

// mark is where the screen has got to, for a wait after it.
func (a *attachSSH) mark() int { return len(a.out.String()) }

// wait waits for want on the screen after from.
func (a *attachSSH) wait(from int, want string) {
	a.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(a.out.String()[from:], want) {
		if time.Now().After(deadline) {
			a.t.Fatalf("%q never showed; the screen after %d:\n%q\nstderr %q", want, from, a.out.String()[from:], a.err.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// end waits for the command to end and gives its exit-status.
func (a *attachSSH) end() int {
	a.t.Helper()
	select {
	case err := <-a.done:
		var exit *ssh.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &exit):
			return exit.ExitStatus()
		}
		a.t.Fatalf("the command ended with no exit-status: %v; stderr %q", err, a.err.String())
	case <-time.After(10 * time.Second):
		a.t.Fatalf("the command did not end; stderr %q, the screen ends %q", a.err.String(), attachTail(a.out.String()))
	}
	return -1
}

// attachTail is the end of s, for a failure to quote.
func attachTail(s string) string {
	if len(s) > 400 {
		return s[len(s)-400:]
	}
	return s
}

// attachHold starts a session labelled label at cols×rows, as a desk would,
// in a folder of that name.
func attachHold(t *testing.T, r *commandsRig, label string, cols, rows uint16) client.Session {
	t.Helper()
	dir := filepath.Join(r.work, label)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := r.c.Create(client.Create{Account: 1, Label: label, Cwd: dir, Cols: cols, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// attachSize is the size the server says the session is.
func attachSize(t *testing.T, r *commandsRig, id string) (cols, rows int) {
	t.Helper()
	resp, err := r.c.HTTP().Get("http://rota/v1/terminals/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct{ Cols, Rows int }
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc.Cols, doc.Rows
}

// attachPlaces is how many attach places the server holds now.
func attachPlaces(r *commandsRig) int {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.s.attached
}

// attachGivenBack waits for every attach place to be given back.
func attachGivenBack(t *testing.T, r *commandsRig) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for attachPlaces(r) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d attach places were never given back", attachPlaces(r))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// attachReset is what the end of an attach leaves on a window rows high,
// and the newline the line on stderr then goes below: every mode off, the
// pen and the scroll region reset, and the cursor on the last row.
func attachReset(rows int) string {
	return string(vtscan.ResetAll()) + fmt.Sprintf("\x1b[0m\x1b[r\x1b[%d;1H\r\n", rows)
}

// attachCUP is a cursor put somewhere: what a row of the crop starts with.
var attachCUP = regexp.MustCompile(`\x1b\[[0-9;]*H`)

// attachWidest is the most cells written between two places the cursor was
// put, which is the widest row a window was given.
func attachWidest(s string) (int, string) {
	most, at := 0, ""
	for _, seg := range attachCUP.Split(s, -1) {
		if w := ansi.StringWidth(seg); w > most {
			most, at = w, seg
		}
	}
	return most, at
}

// attachScreen is a window of cols×rows as a terminal draws what it was
// sent, one string per row.
func attachScreen(t *testing.T, sent string, cols, rows int) []string {
	t.Helper()
	em := vt.NewEmulator(cols, rows)
	go func() { _, _ = io.Copy(io.Discard, em) }() // the replies it owes nobody
	// Ended by closing the replies' writer: Close writes a field Read looks
	// at without a lock.
	defer func() {
		if c, ok := em.InputPipe().(io.Closer); ok {
			_ = c.Close()
		}
	}()
	if _, err := em.Write([]byte(sent)); err != nil {
		t.Fatal(err)
	}
	return strings.Split(em.String(), "\n")
}

/* ---------------------------------------------------- attach and watch --- */

func TestAttachOverSSH(t *testing.T) {
	asked := make(chan string, 4)
	r := commandsStart(t, commandsTty, func(o *Options) {
		o.Taught = func(person string) bool {
			asked <- person
			return true
		}
	})
	s := r.open(t, "api")
	c := r.signIn(r.me)
	a := attachOpen(t, c, "attach api", 80, 24)
	a.wait(0, "ready 80x24")
	// A person not yet shown the chord is shown it, once.
	a.wait(0, "rota: to leave, press Ctrl-\\ then d; the session goes on")
	select {
	case who := <-asked:
		if who != "me" {
			t.Errorf("Taught was asked about %q, want me", who)
		}
	default:
		t.Error("Taught was never asked")
	}
	if n := attachPlaces(r); n != 1 {
		t.Errorf("an attach holds %d places, want 1", n)
	}
	m := a.mark()
	a.typ("hi\r")
	a.wait(m, "got:hi")
	a.typ("\x1cd")
	if code := a.end(); code != 0 {
		t.Fatalf("Ctrl-\\ d: exit %d, want 0; stderr %q", code, a.err.String())
	}
	if got, want := a.err.String(), "rota: detached from api; attach api returns to it\r\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
	// The window is given back, and the line goes below its last row.
	if out := a.out.String(); !strings.HasSuffix(out, attachReset(24)) {
		t.Errorf("the screen does not end with the reset and a newline: %q", attachTail(out))
	}
	if got, err := r.c.Get(s.ID); err != nil || got.Ended != nil {
		t.Errorf("the session did not go on: %v %+v", err, got)
	}
	attachGivenBack(t, r)
	if len(asked) != 0 {
		t.Errorf("Taught was asked again: %q", <-asked)
	}

	// By its id, and by the start of its id, the same session.
	for _, word := range []string{s.ID, s.ID[:6]} {
		a := attachOpen(t, c, "attach "+word, 80, 24)
		a.wait(0, "ready 80x24")
		a.typ("\x1cd")
		if code := a.end(); code != 0 || a.err.String() != "rota: detached from api; attach api returns to it\r\n" {
			t.Errorf("attach %s: %d %q", word, code, a.err.String())
		}
	}
}

func TestAttachNeedsATerminal(t *testing.T) {
	r := commandsStart(t, commandsTty)
	s := r.open(t, "api")
	c := r.signIn(r.me)
	for _, cmd := range []string{"attach", "watch"} {
		out, errOut, code := sshdRun(t, c, cmd+" api")
		want := cmd + " needs a terminal: ssh -t <host> " + cmd + " <session>\n"
		if code != 2 || out != "" || errOut != want {
			t.Errorf("%s with no pty: %d %q %q, want 2 and %q", cmd, code, out, errOut, want)
		}
		// One session, no more and no less, is a usage error.
		for _, line := range []string{cmd, cmd + " api web", cmd + " '' x"} {
			out, errOut, code := sshdRun(t, c, line)
			if code != 2 || out != "" || !strings.HasPrefix(errOut, cmd+" takes one session") || !strings.HasSuffix(errOut, usage) {
				t.Errorf("%q: %d %q %q", line, code, out, errOut)
			}
		}
	}
	if n := attachPlaces(r); n != 0 {
		t.Errorf("a refused attach holds %d places", n)
	}

	// A session there is none of, or one that has ended, is exit 1.
	for _, cmd := range []string{"attach", "watch"} {
		_, errOut, code := commandsPty(t, c, cmd+" nosuch", 80, 24)
		if code != 1 || !strings.HasPrefix(errOut, `no session is "nosuch"`) || !strings.HasSuffix(errOut, "\r\n") {
			t.Errorf("%s nosuch: %d %q", cmd, code, errOut)
		}
	}
	if err := r.c.Stop(s.ID); err != nil {
		t.Fatal(err)
	}
	sshdEventually(t, func() error {
		got, err := r.c.Get(s.ID)
		if err == nil && got.Ended == nil {
			err = errors.New("still running")
		}
		return err
	})
	for _, cmd := range []string{"attach", "watch"} {
		_, errOut, code := commandsPty(t, c, cmd+" "+s.ID, 80, 24)
		if code != 1 || errOut != "session api has ended\r\n" {
			t.Errorf("%s of an ended session: %d %q", cmd, code, errOut)
		}
		// By its label it is not found: a name finds a live session only.
		_, errOut, code = commandsPty(t, c, cmd+" api", 80, 24)
		if code != 1 || !strings.HasPrefix(errOut, `no session is "api"`) {
			t.Errorf("%s api once it has ended: %d %q", cmd, code, errOut)
		}
	}
	if n := attachPlaces(r); n != 0 {
		t.Errorf("a refused attach holds %d places", n)
	}
}

func TestWatchNeverTypes(t *testing.T) {
	r := commandsStart(t, commandsTty)
	s := r.open(t, "api")
	c := r.signIn(r.me)
	a := attachOpen(t, c, "watch api", 80, 24)
	a.wait(0, "ready 80x24")
	a.typ("x\r")
	// Something else typed after it, by a keyboard that may, shows on the
	// watcher's screen: had x been typed, it would have shown first.
	time.Sleep(200 * time.Millisecond)
	tm, err := r.c.Attach(s.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	if err := tm.Type([]byte("y\r")); err != nil {
		t.Fatal(err)
	}
	a.wait(0, "got:y")
	// A watcher's keys are dropped, never sent to be refused, and the
	// chord's k and z are answered here.
	a.typ("\x1ck")
	a.wait(0, "rota: watching; this window cannot take the keyboard")
	if out := a.out.String(); strings.Contains(out, "got:x") || strings.Contains(out, "attached to watch") {
		t.Errorf("the watcher's keys were sent: %q", out)
	}
	a.typ("\x1cd")
	if code := a.end(); code != 0 {
		t.Fatalf("Ctrl-\\ d: exit %d; stderr %q", code, a.err.String())
	}
	if got, want := a.err.String(), "rota: detached from api; watch api returns to it\r\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
	attachGivenBack(t, r)
}

// Review Focus 3: a phone attaches to the desk's session.
func TestAPhoneNeverResizesTheDesk(t *testing.T) {
	r := commandsStart(t, commandsTty)
	s := attachHold(t, r, "api", 100, 30)
	c := r.signIn(r.me)
	a := attachOpen(t, c, "attach api", 40, 12)
	a.wait(0, "z fits")

	// The crop is anchored at the bottom and moves up only to keep the
	// cursor in view, so a screen not yet full shows what is under the
	// cursor: it is filled first, by lines longer than the session is wide.
	a.typ("noise 30\r")
	a.wait(0, "noise done")
	// Then thirty lines of a hundred characters, one at a time, each seen in
	// the crop, just above the cursor, before the next.
	for i := range 30 {
		line := fmt.Sprintf("%02d", i) + strings.Repeat("y", 98)
		a.typ("print " + hex.EncodeToString([]byte(line+"\r\n")) + "\r")
		a.wait(0, fmt.Sprintf("%02d", i)+strings.Repeat("y", 38))
	}
	if cols, rows := attachSize(t, r, s.ID); cols != 100 || rows != 30 {
		t.Fatalf("the desk's session is %dx%d once the phone attached, want 100x30", cols, rows)
	}
	sent := a.out.String()
	if w, row := attachWidest(sent); w > 40 {
		t.Fatalf("the phone was sent a row of %d cells: %q", w, row)
	}
	if strings.Contains(sent, strings.Repeat("y", 41)) {
		t.Fatal("the session's own lines reached the phone")
	}
	if rows := attachScreen(t, sent, 40, 12); !strings.Contains(rows[11], "100x30") {
		t.Fatalf("the last row is %q, want the sizes", rows[11])
	}

	// The phone turned sideways: a wider crop, the desk untouched.
	m := a.mark()
	if err := a.sess.WindowChange(15, 60); err != nil {
		t.Fatal(err)
	}
	a.wait(m, "this window 60x15")
	if w, row := attachWidest(a.out.String()[m:]); w > 60 {
		t.Fatalf("a 60-cell window was sent a row of %d cells: %q", w, row)
	}
	m = a.mark()
	if err := a.sess.WindowChange(12, 40); err != nil {
		t.Fatal(err)
	}
	a.wait(m, "\x1b[12;1H\x1b[7mrota: Ctrl-\\ z fits; api is 100x30, thi")
	if cols, rows := attachSize(t, r, s.ID); cols != 100 || rows != 30 {
		t.Fatalf("the desk's session is %dx%d once the phone turned, want 100x30", cols, rows)
	}

	// z fits the session to the phone, and its output then comes as it is.
	fitted := a.mark()
	a.typ("\x1cz")
	sshdEventually(t, func() error {
		if cols, rows := attachSize(t, r, s.ID); cols != 40 || rows != 12 {
			return fmt.Errorf("%dx%d", cols, rows)
		}
		return nil
	})
	if w, row := attachWidest(a.out.String()[m:fitted]); w > 40 {
		t.Fatalf("the phone was sent a row of %d cells: %q", w, row)
	}
	m = a.mark()
	a.typ("after\r")
	a.wait(m, "after\r\ngot:after\r")

	// A window bigger than the session sends no size.
	if err := a.sess.WindowChange(40, 120); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	m = a.mark()
	a.typ("size\r")
	a.wait(m, "size\r\nsize 40x12\r")
	if cols, rows := attachSize(t, r, s.ID); cols != 40 || rows != 12 {
		t.Errorf("a window of 120x40 made the session %dx%d", cols, rows)
	}
	if strings.Contains(a.out.String(), "120x40") {
		t.Errorf("the session was told of the 120x40 window: %q", attachTail(a.out.String()))
	}
	a.typ("\x1cd")
	if code := a.end(); code != 0 {
		t.Fatalf("Ctrl-\\ d: exit %d; stderr %q", code, a.err.String())
	}
}

func TestNewAttachesWhenThereIsATerminal(t *testing.T) {
	r := commandsStart(t, commandsTty)
	c := r.signIn(r.me)
	a := attachOpen(t, c, "new --label x", 70, 20)
	a.wait(0, "ready 70x20")
	a.typ("\x1cd")
	if code := a.end(); code != 0 {
		t.Fatalf("Ctrl-\\ d: exit %d; stderr %q", code, a.err.String())
	}
	if got, want := a.err.String(), "rota: detached from x; attach x returns to it\r\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
	if strings.Contains(a.out.String(), "held as session") {
		t.Errorf("new on a terminal said it held the session: %q", a.out.String())
	}
	all, err := r.c.List()
	if err != nil || len(all) != 1 || all[0].Label != "x" || all[0].Ended != nil {
		t.Fatalf("after new: %v %+v", err, all)
	}
	if got := r.created(); !slices.Equal(got, []string{"map[cols:70 label:x rows:20]"}) {
		t.Errorf("new asked for %q", got)
	}
	attachGivenBack(t, r)
}

// eye, whose sign-in may only watch, is shown the session me types into,
// and nothing eye types reaches it: an attach of eye's is a watch.
func TestTwoPeopleKeepTheirKeyboards(t *testing.T) {
	r := commandsStart(t, commandsTty)
	r.open(t, "api")
	me := attachOpen(t, r.signIn(r.me), "attach api", 80, 24)
	me.wait(0, "ready 80x24")
	eye := attachOpen(t, r.signIn(r.eye), "attach api", 80, 24)
	eye.wait(0, "ready 80x24")

	eye.typ("eye\r")
	eye.typ("\x1cz")
	eye.wait(0, "rota: watching; this window cannot fit the session")
	time.Sleep(200 * time.Millisecond)
	m := me.mark()
	me.typ("me\r")
	me.wait(m, "got:me")
	eye.wait(0, "got:me")
	for who, a := range map[string]*attachSSH{"me": me, "eye": eye} {
		if out := a.out.String(); strings.Contains(out, "eye") || strings.Contains(out, "may only watch") {
			t.Errorf("eye's keys were sent; %s's screen: %q", who, out)
		}
	}
	for _, a := range []*attachSSH{eye, me} {
		a.typ("\x1cd")
		if code := a.end(); code != 0 {
			t.Fatalf("Ctrl-\\ d: exit %d; stderr %q", code, a.err.String())
		}
	}
	if got, want := eye.err.String(), "rota: detached from api; attach api returns to it\r\n"; got != want {
		t.Errorf("eye's stderr %q, want %q", got, want)
	}
	attachGivenBack(t, r)
}

// The window an attach is given follows the channel: the pty's size, then
// each window-change, each said once it comes.
func TestAChanTermFollowsTheWindow(t *testing.T) {
	resize := make(chan [2]int, channelResizes)
	ch := &Channel{In: strings.NewReader(""), Out: io.Discard, PTY: true, Cols: 80, Rows: 24, Resize: resize}
	in := inputPump(ch.In)
	defer in.stop()
	term := attachChan(ch, in)
	defer term.stop()
	if cols, rows := term.Size(); cols != 80 || rows != 24 {
		t.Fatalf("Size %dx%d, want 80x24", cols, rows)
	}
	for _, z := range [][2]int{{100, 30}, {40, 12}} {
		resize <- z
		select {
		case <-term.Resized():
		case <-time.After(5 * time.Second):
			t.Fatalf("no Resized for %v", z)
		}
		if cols, rows := term.Size(); cols != z[0] || rows != z[1] {
			t.Errorf("Size %dx%d after %v", cols, rows, z)
		}
	}
	var _ attach.Terminal = term
}
