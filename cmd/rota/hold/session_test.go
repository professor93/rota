//go:build linux || darwin

package hold

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/share"
)

// A holder's session, driven through its link the way the daemon drives it,
// with a fake CLI on the other side (rig_test.go).

func TestALinkGetsEverythingSinceItsOffset(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	p := r.link(t)
	p.since(0)
	for _, s := range []string{"one", "two", "three"} {
		r.c.print(t, s)
	}
	if got := p.output(0, 11); got != "onetwothree" {
		t.Fatalf("since 0: %q", got)
	}
	q := r.link(t)
	q.since(5)
	if got := q.output(5, 6); got != "othree" {
		t.Fatalf("since 5: %q", got)
	}
}

func TestASlowLinkCostsAGapNotTheChild(t *testing.T) {
	r := newRig(t, 1024, newFakeChild())
	claim(t, r.terminals)
	p := r.link(t)
	p.since(0)
	// The link reads nothing while the CLI prints far more than the ring
	// keeps, and the CLI finishes anyway.
	r.c.print(t, strings.Repeat("x", 64<<10))
	r.c.print(t, "END")
	total := int64(64<<10 + 3)
	next, jumped := int64(0), false
	var tail []byte
	for next < total {
		off, b, err := holdwire.OutputOf(p.until(holdwire.Output))
		if err != nil {
			t.Fatal(err)
		}
		if off < next {
			t.Fatalf("output at %d after output up to %d", off, next)
		}
		if off > next {
			jumped = true
		}
		next = off + int64(len(b))
		tail = append(tail, b...)
	}
	if !jumped {
		t.Fatal("a 1 KiB ring sent 64 KiB without a gap")
	}
	if !bytes.HasSuffix(tail, []byte("xEND")) || next != total {
		t.Fatalf("the output ends at %d with %q", next, tail[max(0, len(tail)-8):])
	}
}

// TestHelperHoldsTheStore is another process holding the store's daemon
// lock, for TestOnlyTheDaemonMayLink. It is not a test on its own.
func TestHelperHoldsTheStore(t *testing.T) {
	terminals := os.Getenv("ROTA_HOLD_TEST_TERMINALS")
	if terminals == "" {
		t.Skip("a helper process")
	}
	release, err := holdwire.ClaimDaemon(terminals, "helper")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fmt.Println("claimed")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestOnlyTheDaemonMayLink(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	refused := func(why string) {
		t.Helper()
		p := r.dial(t)
		var m holdwire.RefusedMsg
		p.decode(holdwire.Refused, &m, holdwire.Welcome)
		if m.Message != "only the rota daemon serving this store may link to a session" {
			t.Fatalf("%s: refused with %q", why, m.Message)
		}
		p.eof()
	}
	refused("with no daemon")

	// Another process serves the store: this one is not it.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsTheStore$")
	cmd.Env = append(os.Environ(), "ROTA_HOLD_TEST_TERMINALS="+r.terminals)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "claimed\n" {
		t.Fatalf("the helper said %q", line)
	}
	refused("with another process as the daemon")
	in.Close()
	_ = cmd.Wait()

	claim(t, r.terminals)
	p := r.link(t)
	q := r.link(t)
	p.eof()
	q.send(holdwire.Ping, nil)
	q.until(holdwire.Pong)
}

func TestQueriesAreAnsweredWithNobodyAttached(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	r.c.reads()
	r.c.print(t, "\x1b[c")
	r.c.waitTyped(t, "\x1b[?62;22c")
}

func TestCPRAnswersWhereTheCursorIs(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	r.c.reads()
	r.c.print(t, "abc\x1b[6n")
	r.c.waitTyped(t, "\x1b[1;4R")
}

// A CLI that asks where the cursor is and then for the terminal's
// attributes, as a sentinel, reads the two answers in that order, although
// the second is known at once and the first only once the screen has caught
// up.
func TestAnswersKeepTheOrderOfTheQuestions(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	r.c.reads()
	r.c.print(t, "ab\x1b[6n\x1b[c")
	r.c.waitTyped(t, "\x1b[1;3R\x1b[?62;22c")
}

// The same across two reads, with the screen still working out the cursor
// when the second question arrives.
func TestAnAnswerWaitsForTheCursorAskedBeforeIt(t *testing.T) {
	orig := vtWrite
	t.Cleanup(func() { vtWrite = orig }) // after the session is done
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	vtWrite = func(em *vt.Emulator, p []byte) (int, error) {
		if bytes.Contains(p, []byte("cd")) {
			once.Do(func() { close(entered); <-gate })
		}
		return orig(em, p)
	}
	r := newRig(t, 0, newFakeChild())
	r.c.reads()
	r.c.print(t, "cd\x1b[6n")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the screen never took the output")
	}
	r.c.print(t, "\x1b[c")
	close(gate)
	r.c.waitTyped(t, "\x1b[1;3R\x1b[?62;22c")
}

func TestAFullInputQueueIsRefusedOutLoud(t *testing.T) {
	defer func(n int) { InputQueue = n }(InputQueue)
	InputQueue = 8
	r := newRig(t, 0, newFakeChild()) // a CLI that never reads
	claim(t, r.terminals)
	p := r.link(t)
	p.send(holdwire.Input, holdwire.PutInput(1, []byte("abcd")))
	p.send(holdwire.Input, holdwire.PutInput(2, []byte("efghij")))
	p.send(holdwire.Input, holdwire.PutInput(3, []byte("0123456789abcdef")))
	for _, seq := range []uint64{2, 3} {
		var m holdwire.InputRefusedMsg
		p.decode(holdwire.InputRefused, &m)
		if m.Seq != seq || m.Message != "the session is not taking keystrokes this fast; that input was not typed" {
			t.Fatalf("refused %+v, want sequence %d", m, seq)
		}
	}
}

func TestScreenStandsForAnOffsetBetweenSequences(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	p := r.link(t)
	p.since(0)
	first := "\x1b[?1049h\x1b[2;3Hhi\x1b[3"
	r.c.print(t, first)
	p.output(0, len(first))
	h, repaint := p.screen(1)
	if !h.OK || h.Offset != int64(len(first)-3) || !h.Modes.AltScreen || h.Cols != 80 || h.Rows != 24 {
		t.Fatalf("Screen %+v for %d bytes ending mid-sequence", h, len(first))
	}
	if !strings.Contains(repaint, "\x1b[?1049h") || !strings.Contains(repaint, "\x1b[2;1H  hi") {
		t.Fatalf("repaint %q", repaint)
	}
	r.c.print(t, "J")
	p.output(int64(len(first)), 1)
	if h2, _ := p.screen(2); !h2.OK || h2.Offset <= h.Offset {
		t.Fatalf("after the sequence ended: %+v, before it %+v", h2, h)
	}
}

func TestResizeIsBoundedAndSafeAfterTheEnd(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	p := r.link(t)
	for _, c := range []struct{ ask, want holdwire.SizeMsg }{
		{holdwire.SizeMsg{Cols: 5000, Rows: 5000}, holdwire.SizeMsg{Cols: 1000, Rows: 1000}},
		{holdwire.SizeMsg{}, holdwire.SizeMsg{Cols: 80, Rows: 24}},
		{holdwire.SizeMsg{Cols: 5000, Rows: 5000}, holdwire.SizeMsg{Cols: 1000, Rows: 1000}},
	} {
		p.json(holdwire.Resize, c.ask)
		var got holdwire.SizeMsg
		p.decode(holdwire.Resized, &got)
		if got != c.want {
			t.Fatalf("asked %+v, resized to %+v", c.ask, got)
		}
	}
	r.c.mu.Lock()
	sizes := slices.Clone(r.c.sizes)
	r.c.mu.Unlock()
	if want := []string{"1000x1000", "80x24", "1000x1000"}; !slices.Equal(sizes, want) {
		t.Fatalf("the CLI was resized to %v", sizes)
	}
	if h, _ := p.screen(1); h.Cols != 1000 || h.Rows != 1000 {
		t.Fatalf("the screen is %dx%d", h.Cols, h.Rows)
	}

	// The CLI is gone and its terminal not yet: a resize now has nothing to
	// resize and answers nothing.
	r.c.exitKeep(0)
	select {
	case <-r.s.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the session never saw its CLI exit")
	}
	p.json(holdwire.Resize, holdwire.SizeMsg{Cols: 100, Rows: 30})
	p.until(holdwire.Exit, holdwire.Resized)
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if len(r.c.sizes) != 3 {
		t.Fatalf("resized after the end: %v", r.c.sizes)
	}
}

func TestSendWaitsPastesAndPressesEnter(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	r.c.reads()
	p := r.link(t)
	r.c.print(t, "\x1b[?2004h")
	printed := time.Now()
	p.json(holdwire.Send, holdwire.SendMsg{Seq: 4, Text: "hi\x1b[201~x"})
	var m holdwire.SentMsg
	p.decode(holdwire.Sent, &m)
	if m.Seq != 4 || m.Code != holdwire.SentOK {
		t.Fatalf("sent %+v", m)
	}
	pasted := r.c.waitTyped(t, "\x1b[200~hix\x1b[201~")
	entered := r.c.waitTyped(t, "\x1b[201~\r")
	if got := r.c.read(); got != "\x1b[200~hix\x1b[201~\r" {
		t.Fatalf("the CLI read %q", got)
	}
	if d := pasted.Sub(printed); d < 1400*time.Millisecond {
		t.Fatalf("typed %v after the last output, before the session was quiet", d)
	}
	if d := entered.Sub(pasted); d < 100*time.Millisecond {
		t.Fatalf("Enter came %v after the text", d)
	}
}

func TestSendRefusesAWaitingSession(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	r.c.reads()
	p := r.link(t)
	r.s.SetRegistry(&holdwire.Registry{Status: "waiting", WaitingFor: "permission"})
	p.json(holdwire.Send, holdwire.SendMsg{Seq: 1, Text: "yes"})
	var m holdwire.SentMsg
	p.decode(holdwire.Sent, &m)
	if m.Seq != 1 || m.Code != holdwire.SentWaiting || m.Message == "" {
		t.Fatalf("a send into a session waiting on a permission: %+v", m)
	}
	p.json(holdwire.Send, holdwire.SendMsg{Seq: 2, Text: "yes", Force: true})
	var forced holdwire.SentMsg
	p.decode(holdwire.Sent, &forced)
	if forced.Seq != 2 || forced.Code != holdwire.SentOK {
		t.Fatalf("a forced send: %+v", forced)
	}
	r.c.waitTyped(t, "yes\r")
	if got := r.c.read(); got != "yes\r" {
		t.Fatalf("the CLI read %q", got)
	}
}

func TestAPendingSendEndsWithTheChild(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	r.c.reads()
	p := r.link(t)
	r.s.SetRegistry(&holdwire.Registry{Status: "busy"})
	p.json(holdwire.Send, holdwire.SendMsg{Seq: 3, Text: "later", WaitMS: 60000})
	// Frames are handled in order: once the Pong is back, the send waits.
	p.send(holdwire.Ping, nil)
	p.until(holdwire.Pong)
	r.c.exit(5)
	var m holdwire.SentMsg
	p.decode(holdwire.Sent, &m, holdwire.Exit)
	if m.Seq != 3 || m.Code != holdwire.SentEnded {
		t.Fatalf("a send pending when the CLI ended: %+v", m)
	}
	var e holdwire.ExitMsg
	p.decode(holdwire.Exit, &e)
	if e.Code != 5 {
		t.Fatalf("exit %d", e.Code)
	}
	if got := r.c.read(); got != "" {
		t.Fatalf("the CLI read %q", got)
	}
}

func TestStopHangsUpThenKills(t *testing.T) {
	defer func(d time.Duration) { StopAfter = d }(StopAfter)
	StopAfter = 300 * time.Millisecond
	c := newFakeChild()
	c.ignoreHUP = true
	r := newRig(t, 0, c)
	claim(t, r.terminals)
	p := r.link(t)
	p.send(holdwire.Stop, nil)
	var e holdwire.ExitMsg
	p.decode(holdwire.Exit, &e)
	if e.Code != 137 {
		t.Fatalf("exit %d, want the KILL's 137", e.Code)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Equal(c.sigs, []os.Signal{syscall.SIGHUP, syscall.SIGKILL}) {
		t.Fatalf("signals %v", c.sigs)
	}
	if d := c.sigAt[1].Sub(c.sigAt[0]); d < StopAfter {
		t.Fatalf("KILL came %v after HUP", d)
	}
}

// The holder's own hang-up and termination stop its CLI the same way.
func TestASignalStopsTheChild(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	cancel := r.s.stopOnSignals()
	defer cancel()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-r.exits:
		if code != 129 {
			t.Fatalf("exit %d, want the HUP's 129", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not stop the CLI")
	}
}

func TestTheExitReachesTheLinkLast(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	p := r.link(t)
	p.since(0)
	r.c.print(t, "bye")
	r.c.exit(3)
	if got := p.output(0, 3); got != "bye" {
		t.Fatalf("output %q", got)
	}
	var e holdwire.ExitMsg
	p.decode(holdwire.Exit, &e, holdwire.Output)
	if e.Code != 3 {
		t.Fatalf("exit %d", e.Code)
	}
	select {
	case <-r.s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not done")
	}
	if code := <-r.exits; code != 3 || r.s.Code() != 3 {
		t.Fatalf("OnExit %d, Code %d", code, r.s.Code())
	}
	if r.s.screen.OK() || r.s.screen.Lost() != "" {
		t.Fatalf("the screen outlived its session: OK %v, lost %q", r.s.screen.OK(), r.s.screen.Lost())
	}
	p.eof()
}

func TestUnknownFramesAreIgnored(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	p := r.link(t)
	p.send(99, []byte("from a newer rota"))
	p.send(holdwire.Ping, nil)
	p.until(holdwire.Pong)
}

// A screen that falls a whole ring behind is retired, said once, and the
// session carries on: replays instead of repaints, and the cursor answered
// from the corner.
func TestAScreenARingBehindIsRetired(t *testing.T) {
	r := newRig(t, 1024, newFakeChild())
	claim(t, r.terminals)
	r.c.reads()
	r.s.em.mu.Lock()
	r.c.print(t, strings.Repeat("y", 4096)+"\x1b[6n")
	r.s.em.mu.Unlock()
	r.c.waitTyped(t, "\x1b[1;1R")
	r.c.print(t, strings.Repeat("z", 4096))
	p := r.link(t)
	if h, repaint := p.screen(1); h.OK || repaint != "" {
		t.Fatalf("a retired screen answered %+v %q", h, repaint)
	}
	n := 0
	for _, l := range r.logLines() {
		if strings.Contains(l, "retired") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d lines about retiring the screen: %q", n, r.logLines())
	}
}

// A repaint too big for a frame is none: the Screen says OK=false with no
// repaint, the daemon replays the ring instead, and the link stays.
func TestAScreenTooBigToRepaintIsReplayed(t *testing.T) {
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	p := r.link(t)
	p.since(0)
	p.json(holdwire.Resize, holdwire.SizeMsg{Cols: 1000, Rows: 1000})
	p.decode(holdwire.Resized, &holdwire.SizeMsg{})
	// Every cell styled apart from the one before it, which a repaint draws
	// in about 40 bytes: past a megabyte in all.
	out := "\x1b[1;2;3;4;9;38;2;1;2;3;48;2;4;5;6m" + strings.Repeat("\x1b[7mx\x1b[27mx", 16000)
	for b := []byte(out); len(b) > 0; {
		n := min(len(b), 32<<10)
		r.c.print(t, string(b[:n]))
		b = b[n:]
	}
	p.output(0, len(out))
	h, repaint := p.screen(1)
	if h.OK || repaint != "" || h.Offset != int64(len(out)) || h.Cols != 1000 || h.Rows != 1000 {
		t.Fatalf("a screen too big to repaint answered %+v with %d bytes", h, len(repaint))
	}
	p.send(holdwire.Ping, nil)
	p.until(holdwire.Pong)
}

// A Screen the daemon could not take, made with an error or too big for a
// frame, is answered all the same: OK=false and no repaint.
func TestAScreenThatCannotBeSentIsStillAnswered(t *testing.T) {
	orig := putScreen
	t.Cleanup(func() { putScreen = orig }) // after the session is done
	putScreen = func(h holdwire.ScreenHead, repaint []byte) ([]byte, error) {
		switch {
		case !h.OK:
		case h.Seq == 1:
			return nil, errors.New("no screen for you")
		case h.Seq == 2:
			return make([]byte, share.MaxFrame), nil
		}
		return orig(h, repaint)
	}
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	p := r.link(t)
	p.since(0)
	r.c.print(t, "hello")
	p.output(0, 5)
	for seq := uint64(1); seq <= 3; seq++ {
		h, repaint := p.screen(seq)
		if ok := seq == 3; h.OK != ok || (repaint == "") == ok || h.Offset != 5 {
			t.Fatalf("Screen %d: %+v with %q", seq, h, repaint)
		}
	}
}

// An emulator that panics takes the screen with it and nothing else: one
// line says what it panicked with, the cursor is the corner, and a Screen
// says OK=false.
func TestAPanickingScreenIsRetiredOutLoud(t *testing.T) {
	orig := vtWrite
	t.Cleanup(func() { vtWrite = orig }) // after the session is done
	vtWrite = func(em *vt.Emulator, p []byte) (int, error) {
		if bytes.Contains(p, []byte("boom")) {
			panic("x/vt fell over")
		}
		return orig(em, p)
	}
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	r.c.reads()
	p := r.link(t)
	r.c.print(t, "boom\x1b[6n")
	r.c.waitTyped(t, "\x1b[1;1R")
	if h, repaint := p.screen(1); h.OK || repaint != "" {
		t.Fatalf("a screen whose emulator panicked answered %+v %q", h, repaint)
	}
	r.c.print(t, "more\x1b[6n")
	r.c.waitTyped(t, "\x1b[1;1R\x1b[1;1R")
	var lines []string
	for _, l := range r.logLines() {
		if strings.Contains(l, "panicked") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "x/vt fell over") {
		t.Fatalf("the log says %q about the emulator", lines)
	}
}

// waitPanics is a CLI whose Wait panics when told to, as a broken Child
// might.
type waitPanics struct {
	*fakeChild
	now chan struct{}
}

func (w waitPanics) Wait() int {
	<-w.now
	panic("Wait fell over")
}

// A session whose end panics still ends: the panic is logged, Done closes,
// and the link closes.
func TestDoneClosesWhenTheEndPanics(t *testing.T) {
	c := newFakeChild()
	w := waitPanics{c, make(chan struct{})}
	r := newRigOf(t, 0, c, w)
	r.panics = "Wait fell over"
	claim(t, r.terminals)
	p := r.link(t)
	close(w.now)
	select {
	case <-r.s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed after the session's end panicked")
	}
	p.eof()
	logged := false
	for _, l := range r.logLines() {
		logged = logged || strings.Contains(l, "a panic in the session's end") && strings.Contains(l, "Wait fell over")
	}
	if !logged {
		t.Fatalf("the log says %q", r.logLines())
	}
}
