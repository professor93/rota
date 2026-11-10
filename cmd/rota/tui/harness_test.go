package tui

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/professor93/rota/internal/claudecode"
	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// TestMain keeps every test here away from the person running them: no
// keychain, no Claude Code daemon, no provider and no home of theirs.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	// No keychain and no Claude Code daemon of anybody's is reached from
	// here, nor from anything run from here.
	claudecode.StandIn()
	// No provider call a test did not stand in for reaches the real one.
	harnessOffline()
	// Nor the person's own home, Claude Code directory or profiles.
	person, err := os.MkdirTemp("", "rota-person")
	if err != nil {
		panic(err)
	}
	os.Unsetenv("ROTA_CLAUDE_HOME")
	os.Unsetenv("ROTA_ACCOUNT_ID")
	os.Setenv("HOME", person)
	os.Setenv("USERPROFILE", person)
	os.Unsetenv("ANTHROPIC_CONFIG_DIR")
	os.Unsetenv("XDG_CONFIG_HOME")
	dir, err := os.MkdirTemp("", "rota-claude")
	if err != nil {
		panic(err)
	}
	os.Setenv("CLAUDE_CONFIG_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.RemoveAll(person)
	os.Exit(code)
}

// harnessOffline points every endpoint of every provider at an address
// nothing answers on, so a made-up token never leaves the machine.
func harnessOffline() {
	const u = claudecode.Unreachable
	rota.ClaudeEndpoints.Authorize, rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile, rota.ClaudeEndpoints.Usage = u, u, u, u
	rota.ClaudeModelsEndpoint = u
	rota.CodexEndpoints.Authorize, rota.CodexEndpoints.Token = u, u
}

// The driver. Bubble Tea runs every command in a goroutine of its own and
// hands the model each message as it comes back; run does the same without
// a terminal, in rounds, so a test reads like the program it drives.
const (
	// harnessRound is the longest a round waits for the commands started
	// since the last delivery.
	harnessRound = 200 * time.Millisecond
	// harnessAwait is the longest await waits for a long-running command.
	harnessAwait = 5 * time.Second
	// harnessCap is how many messages from commands one run may deliver
	// before it gives up on a model that never goes quiet.
	harnessCap = 1000
)

// harnessTB is what the driver needs of a test; *testing.T has it.
type harnessTB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
}

type harnessResult struct {
	seq   uint64
	msg   tea.Msg
	panic any
	stack []byte
}

// harnessCommand is a command still running: what it is, and the message
// whose Update returned it.
type harnessCommand struct {
	name, from string
}

// harnessDriver is one test's commands. Each test has its own, made by its
// first run, so nothing one test leaves running reaches another.
type harnessDriver struct {
	mu      sync.Mutex
	seq     uint64
	pending map[uint64]harnessCommand
	fresh   map[uint64]bool // started since the last delivery
	results chan harnessResult
	gone    chan struct{} // closed when the pending commands are forgotten
}

var harnessDrivers struct {
	sync.Mutex
	of map[harnessTB]*harnessDriver
}

// harnessDriverOf is t's driver. Its cleanup fails the test for every
// command still running when the test ends, naming each.
func harnessDriverOf(t harnessTB) *harnessDriver {
	harnessDrivers.Lock()
	defer harnessDrivers.Unlock()
	if d := harnessDrivers.of[t]; d != nil {
		return d
	}
	if harnessDrivers.of == nil {
		harnessDrivers.of = map[harnessTB]*harnessDriver{}
	}
	d := &harnessDriver{}
	d.forget()
	harnessDrivers.of[t] = d
	t.Cleanup(func() {
		if left := d.leftover(); len(left) > 0 {
			t.Errorf("the test ended with %d command(s) still running:\n  %s", len(left), strings.Join(left, "\n  "))
		}
		d.forget()
		harnessDrivers.Lock()
		delete(harnessDrivers.of, t)
		harnessDrivers.Unlock()
	})
	return d
}

// forget drops every command still running, as a program that has ended
// does. What they return later goes nowhere.
func (d *harnessDriver) forget() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gone != nil {
		close(d.gone)
	}
	d.pending, d.fresh = map[uint64]harnessCommand{}, map[uint64]bool{}
	d.results, d.gone = make(chan harnessResult, 16), make(chan struct{})
}

// leftover names the commands still running, in the order they started.
func (d *harnessDriver) leftover() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	seqs := make([]uint64, 0, len(d.pending))
	for s := range d.pending {
		seqs = append(seqs, s)
	}
	slices.Sort(seqs)
	names := make([]string, 0, len(seqs))
	for _, s := range seqs {
		c := d.pending[s]
		names = append(names, c.name+", returned by Update("+c.from+")")
	}
	return names
}

func (d *harnessDriver) start(cmd tea.Cmd, from tea.Msg) {
	if cmd == nil {
		return
	}
	name := "a command"
	if f := runtime.FuncForPC(reflect.ValueOf(cmd).Pointer()); f != nil {
		name = f.Name()
	}
	d.mu.Lock()
	d.seq++
	seq, results, gone := d.seq, d.results, d.gone
	d.pending[seq] = harnessCommand{name: name, from: fmt.Sprintf("%T", from)}
	d.fresh[seq] = true
	d.mu.Unlock()
	go func() {
		r := harnessResult{seq: seq}
		defer func() {
			if p := recover(); p != nil {
				r.panic, r.stack = p, debug.Stack()
			}
			select {
			case results <- r:
			case <-gone:
			}
		}()
		r.msg = cmd()
	}()
}

// collect ends a round. It waits, at most harnessRound, for the commands
// started since the last delivery, takes whatever else has returned by
// then, and gives back what came, in the order the commands started. A
// command still running at the end of its round is long-running: no round
// waits for it again, and the first round that finds it returned delivers
// its message. With longWait above zero it waits that long for any one
// command to return, long-running ones included, and reports whether one
// did.
func (d *harnessDriver) collect(t harnessTB, longWait time.Duration) ([]harnessResult, bool) {
	t.Helper()
	d.mu.Lock()
	results := d.results
	d.mu.Unlock()
	var got []harnessResult
	quit := false // the program ends: no round waits past it
	take := func(r harnessResult) {
		if _, ok := r.msg.(tea.QuitMsg); ok {
			quit = true
		}
		d.mu.Lock()
		delete(d.pending, r.seq)
		delete(d.fresh, r.seq)
		d.mu.Unlock()
		if r.panic != nil {
			d.forget()
			t.Fatalf("a command panicked: %v\n%s", r.panic, r.stack)
		}
		got = append(got, r)
	}
	waiting := func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		if quit {
			return false
		}
		if longWait > 0 {
			return len(got) == 0 && len(d.pending) > 0
		}
		return len(d.fresh) > 0
	}
	limit := harnessRound
	if longWait > 0 {
		limit = longWait
	}
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
wait:
	for waiting() {
		select {
		case r := <-results:
			take(r)
		case <-deadline.C:
			break wait
		}
	}
	for {
		select {
		case r := <-results:
			take(r)
			continue
		default:
		}
		break
	}
	d.mu.Lock()
	clear(d.fresh)
	d.mu.Unlock()
	slices.SortFunc(got, func(a, b harnessResult) int { return cmp.Compare(a.seq, b.seq) })
	return got, len(got) > 0
}

// run sends each message to m's Update and runs every command that comes
// back, each in its own goroutine, until the model is quiet: nothing left to
// deliver, and every command started since the last delivery either
// returned or past its round. A tea.BatchMsg, given or returned, is
// expanded into its commands, so run(t, m, tea.BatchMsg{m.Init()}) starts a
// model. A command slower than its round is long-running: run does not wait
// for it again, and await does (see collect). tea.QuitMsg ends the run, as
// it ends the program: it is not delivered, and every command still running
// is forgotten.
//
// It returns every message the commands produced, in the order delivered:
// the effects (handOffMsg, clipboardMsg, bellMsg), a quit, and anything the
// model ignores; the test's own messages are not in it. The test fails at
// once past harnessCap messages from commands, with the last ten, on a
// command's panic, and on tea.Sequence, whose message cannot be expanded;
// and when it ends with a command still running.
func run(t *testing.T, m tea.Model, msgs ...tea.Msg) (tea.Model, []tea.Msg) {
	t.Helper()
	return harnessRun(t, m, 0, msgs)
}

// await is run, after first waiting up to five seconds for a command that
// is still running (a stream, a page's end) to return. The test fails if
// none does.
func await(t *testing.T, m tea.Model, msgs ...tea.Msg) (tea.Model, []tea.Msg) {
	t.Helper()
	return harnessRun(t, m, harnessAwait, msgs)
}

func harnessRun(t harnessTB, m tea.Model, longWait time.Duration, msgs []tea.Msg) (tea.Model, []tea.Msg) {
	t.Helper()
	d := harnessDriverOf(t)
	var seen, last []tea.Msg
	produced := 0
	type item struct {
		msg     tea.Msg
		command bool
	}
	var queue []item
	if longWait > 0 {
		got, ok := d.collect(t, longWait)
		if !ok {
			t.Fatalf("await: no command returned within %v; still running:\n  %s", longWait, strings.Join(d.leftover(), "\n  "))
		}
		for _, r := range got {
			queue = append(queue, item{r.msg, true})
		}
	}
	for _, msg := range msgs {
		queue = append(queue, item{msg, false})
	}
	for {
		for len(queue) > 0 {
			it := queue[0]
			queue = queue[1:]
			switch msg := it.msg.(type) {
			case nil:
				continue
			case tea.BatchMsg:
				for _, cmd := range msg {
					d.start(cmd, msg)
				}
				continue
			case tea.QuitMsg:
				d.forget()
				if it.command {
					seen = append(seen, msg)
				}
				return m, seen
			}
			if v := reflect.ValueOf(it.msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeFor[tea.Cmd]() {
				d.forget()
				t.Fatalf("a %T reached the driver: tea.Sequence is not used here, tea.Batch is", it.msg)
			}
			if it.command {
				seen = append(seen, it.msg)
				produced++
				last = append(last, it.msg)
				if len(last) > 10 {
					last = last[1:]
				}
				if produced > harnessCap {
					var b strings.Builder
					for _, l := range last {
						fmt.Fprintf(&b, "\n  %T %v", l, l)
					}
					d.forget()
					t.Fatalf("the model never went quiet: %d messages from commands, the last ten:%s", produced, b.String())
				}
			}
			var cmd tea.Cmd
			m, cmd = m.Update(it.msg)
			d.start(cmd, it.msg)
		}
		got, _ := d.collect(t, 0)
		if len(got) == 0 {
			return m, seen
		}
		for _, r := range got {
			queue = append(queue, item{r.msg, true})
		}
	}
}

// plain is the model's view as text, with every escape sequence stripped.
func plain(m tea.Model) string {
	return ansi.Strip(m.View().Content)
}

// golden compares got with testdata/golden/<name>.txt, and writes it there
// instead when ROTA_UPDATE_GOLDEN=1.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".txt")
	if os.Getenv("ROTA_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (ROTA_UPDATE_GOLDEN=1 writes it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from %s (ROTA_UPDATE_GOLDEN=1 rewrites it)\n--- got\n%s\n--- want\n%s", name, path, got, want)
	}
}

// harnessKeyNames maps a key's name, as tea.Key.String writes it, to its code:
// "enter", "up", "f1", "space" and the rest.
var harnessKeyNames = func() map[string]rune {
	names := map[string]rune{}
	add := func(r rune) {
		if s := (tea.Key{Code: r}).Keystroke(); utf8.RuneCountInString(s) > 1 {
			if _, ok := names[s]; !ok {
				names[s] = r
			}
		}
	}
	for r := rune(0); r < 0x80; r++ {
		add(r)
	}
	for r := tea.KeyExtended; r < tea.KeyExtended+1000; r++ {
		add(r)
	}
	return names
}()

// press turns key names into key presses, as a terminal would report them:
// "q", "M", "?", "enter", "esc", "shift+up", "alt+enter", "ctrl+s". The
// pressed key's String() is the name given.
func press(keys ...string) []tea.Msg {
	msgs := make([]tea.Msg, 0, len(keys))
	for _, name := range keys {
		msgs = append(msgs, tea.KeyPressMsg(harnessKey(name)))
	}
	return msgs
}

func harnessKey(name string) tea.Key {
	var k tea.Key
	rest := name
	for {
		mod, after, ok := strings.Cut(rest, "+")
		if !ok || after == "" {
			break
		}
		switch mod {
		case "ctrl":
			k.Mod |= tea.ModCtrl
		case "alt":
			k.Mod |= tea.ModAlt
		case "shift":
			k.Mod |= tea.ModShift
		case "meta":
			k.Mod |= tea.ModMeta
		case "super":
			k.Mod |= tea.ModSuper
		case "hyper":
			k.Mod |= tea.ModHyper
		default:
			panic("press: unknown modifier in " + name)
		}
		rest = after
	}
	if code, ok := harnessKeyNames[rest]; ok {
		k.Code = code
		if code == tea.KeySpace && k.Mod == 0 {
			k.Text = " "
		}
		return k
	}
	r, size := utf8.DecodeRuneInString(rest)
	if size != len(rest) || r == utf8.RuneError {
		panic("press: unknown key " + name)
	}
	k.Code = r
	switch {
	case k.Mod&^tea.ModShift != 0:
		// A key with ctrl or alt carries no text.
	case unicode.IsUpper(r):
		// As the terminal reports it: the lower case key with shift.
		k.Code, k.ShiftedCode, k.Mod, k.Text = unicode.ToLower(r), r, k.Mod|tea.ModShift, string(r)
	default:
		k.Text = string(r)
	}
	return k
}

// size is the message a terminal of w columns and h rows sends.
func size(w, h int) tea.WindowSizeMsg {
	return tea.WindowSizeMsg{Width: w, Height: h}
}

// fakeTicks stands in for tea.Tick: a timer it makes fires only when the test
// advances the clock past it, so nothing waits on real time. A tick the
// model sets again on its message counts from the clock when that message is
// run, so a test advances one period at a time to see every period fire.
type fakeTicks struct {
	mu     sync.Mutex
	clock  time.Time
	seq    uint64
	timers []harnessTimer
}

type harnessTimer struct {
	at  time.Time
	seq uint64
	fn  func(time.Time) tea.Msg
}

// now is the fake clock, for a model that reads the time as well as ticking.
func (f *fakeTicks) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started()
}

func (f *fakeTicks) started() time.Time {
	if f.clock.IsZero() {
		f.clock = time.Unix(1_800_000_000, 0)
	}
	return f.clock
}

// tick is tea.Tick's signature. Its command sets the timer and returns no
// message; advance returns the message once the time has come. Ticks due at
// the same moment fire in the order tick was called.
func (f *fakeTicks) tick(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	f.mu.Lock()
	f.seq++
	seq := f.seq
	f.mu.Unlock()
	return func() tea.Msg {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.timers = append(f.timers, harnessTimer{at: f.started().Add(d), seq: seq, fn: fn})
		return nil
	}
}

// advance moves the clock by d and returns the messages of every timer that
// came due, earliest first.
func (f *fakeTicks) advance(d time.Duration) []tea.Msg {
	f.mu.Lock()
	f.clock = f.started().Add(d)
	var due, left []harnessTimer
	for _, t := range f.timers {
		if !t.at.After(f.clock) {
			due = append(due, t)
		} else {
			left = append(left, t)
		}
	}
	f.timers = left
	f.mu.Unlock()
	slices.SortFunc(due, func(a, b harnessTimer) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.seq, b.seq))
	})
	msgs := make([]tea.Msg, 0, len(due))
	for _, t := range due {
		msgs = append(msgs, t.fn(t.at))
	}
	return msgs
}

// harnessToy is a model whose Init batches two commands and a one-second
// tick, and which records every message it is given.
type harnessToy struct {
	ticks *fakeTicks
	got   []string
}

type harnessToyMsg string

func (m harnessToy) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg { return harnessToyMsg("one") },
		func() tea.Msg { time.Sleep(20 * time.Millisecond); return harnessToyMsg("two") },
		m.ticks.tick(time.Second, func(time.Time) tea.Msg { return harnessToyMsg("tick") }),
	)
}

func (m harnessToy) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case harnessToyMsg:
		m.got = append(slices.Clone(m.got), string(msg))
		if msg == "one" {
			// A command that returns a batch of its own.
			return m, tea.Batch(func() tea.Msg { return harnessToyMsg("three") }, nil)
		}
	case tea.KeyPressMsg:
		m.got = append(slices.Clone(m.got), "key "+msg.String())
		if msg.String() == "q" {
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.got = append(slices.Clone(m.got), fmt.Sprintf("size %dx%d", msg.Width, msg.Height))
	}
	return m, nil
}

func (m harnessToy) View() tea.View {
	return tea.NewView("\x1b[1mgot\x1b[m " + strings.Join(m.got, ", "))
}

func TestTheDriverRunsCommandsInline(t *testing.T) {
	ticks := &fakeTicks{}
	var m tea.Model = harnessToy{ticks: ticks}
	m, seen := run(t, m, tea.BatchMsg{m.Init()})
	got := m.(harnessToy).got
	slices.Sort(got)
	if want := []string{"one", "three", "two"}; !slices.Equal(got, want) {
		t.Fatalf("delivered %q, want %q", got, want)
	}
	if len(seen) != 3 {
		t.Errorf("run saw %v, want the three messages", seen)
	}
	if msgs := ticks.advance(999 * time.Millisecond); len(msgs) != 0 {
		t.Fatalf("the tick fired early: %v", msgs)
	}
	m, _ = run(t, m, ticks.advance(time.Millisecond)...)
	if got := m.(harnessToy).got; got[len(got)-1] != "tick" {
		t.Fatalf("after a second: %q", got)
	}
	m, seen = run(t, m, size(80, 24))
	if got := m.(harnessToy).got; got[len(got)-1] != "size 80x24" || len(seen) != 0 {
		t.Fatalf("after a resize: %q, saw %v", got, seen)
	}
	m, seen = run(t, m, press("x", "q")...)
	if len(seen) != 1 || seen[0] != (tea.QuitMsg{}) {
		t.Errorf("q saw %v, want the quit", seen)
	}
	if got := m.(harnessToy).got; got[len(got)-1] != "key q" {
		t.Errorf("after q: %q", got)
	}
	// The program ends at the quit: nothing after it is delivered, and a
	// quit the test sends is not among what the commands produced.
	m, seen = run(t, m, append([]tea.Msg{tea.QuitMsg{}}, press("y")...)...)
	if got := m.(harnessToy).got; got[len(got)-1] != "key q" || len(seen) != 0 {
		t.Errorf("after a quit sent in: %q, saw %v", got, seen)
	}
	if strings.Contains(plain(m), "\x1b") || !strings.HasPrefix(plain(m), "got ") {
		t.Errorf("plain: %q", plain(m))
	}
}

// harnessSlow answers a key with a command that returns only once released,
// and "q" with that command and a quit together.
type harnessSlow struct {
	release chan struct{}
	got     []string
}

func (m harnessSlow) Init() tea.Cmd { return nil }

func (m harnessSlow) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	blocked := func() tea.Msg { <-m.release; return harnessToyMsg("late") }
	switch msg := msg.(type) {
	case harnessToyMsg:
		m.got = append(slices.Clone(m.got), string(msg))
	case tea.KeyPressMsg:
		if msg.String() == "q" {
			return m, tea.Batch(blocked, tea.Quit)
		}
		return m, blocked
	}
	return m, nil
}

func (m harnessSlow) View() tea.View { return tea.NewView(strings.Join(m.got, ",")) }

func TestHarnessAwaitsALongRunningCommand(t *testing.T) {
	release := make(chan struct{})
	var m tea.Model = harnessSlow{release: release}
	start := time.Now()
	m, _ = run(t, m, press("x")...)
	if len(m.(harnessSlow).got) != 0 {
		t.Fatalf("delivered before it returned: %q", m.(harnessSlow).got)
	}
	if d := time.Since(start); d < harnessRound {
		t.Errorf("went quiet after %v, before its round", d)
	}
	left := harnessDriverOf(t).leftover()
	if len(left) != 1 || !strings.Contains(left[0], "harnessSlow") || !strings.Contains(left[0], "tea.KeyPressMsg") {
		t.Errorf("still running: %q", left)
	}
	// A run while it is long-running waits for it no longer.
	start = time.Now()
	m, _ = run(t, m, size(80, 24))
	if d := time.Since(start); d >= harnessRound {
		t.Errorf("a run beside a long-running command took %v", d)
	}
	close(release)
	m, seen := await(t, m)
	if got := m.(harnessSlow).got; !slices.Equal(got, []string{"late"}) || len(seen) != 1 {
		t.Fatalf("await delivered %q and saw %v", got, seen)
	}
}

// harnessChain answers message n with a command that returns n+1, up to 5.
type harnessChain struct{ got []int }

type harnessHop int

func (harnessChain) Init() tea.Cmd { return nil }

func (m harnessChain) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if n, ok := msg.(harnessHop); ok {
		m.got = append(slices.Clone(m.got), int(n))
		if n < 5 {
			return m, func() tea.Msg { return n + 1 }
		}
	}
	return m, nil
}

func (harnessChain) View() tea.View { return tea.NewView("") }

// A stream or a page's end is pending for a whole test by design; hops
// beside it do not wait for it.
func TestHarnessHopsBesideALongRunningCommandAreQuick(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	m, _ := run(t, harnessSlow{release: release}, press("x")...)
	start := time.Now()
	chain, seen := run(t, harnessChain{}, harnessHop(1))
	if d := time.Since(start); d >= harnessRound {
		t.Errorf("five hops beside a long-running command took %v", d)
	}
	if got := chain.(harnessChain).got; !slices.Equal(got, []int{1, 2, 3, 4, 5}) || len(seen) != 4 {
		t.Errorf("hops %v, saw %v", got, seen)
	}
	m, _ = run(t, m, press("q")...) // the quit forgets the blocked command
	_ = m
}

// A command one test leaves running never reaches another test, nor slows
// it down.
func TestHarnessAStuckCommandStaysInItsTest(t *testing.T) {
	release := make(chan struct{})
	t.Run("leaves", func(t *testing.T) {
		m, seen := run(t, harnessSlow{release: release}, press("q")...)
		if len(seen) != 1 || seen[0] != (tea.QuitMsg{}) || len(m.(harnessSlow).got) != 0 {
			t.Errorf("q: saw %v, got %q", seen, m.(harnessSlow).got)
		}
		if left := harnessDriverOf(t).leftover(); len(left) != 0 {
			t.Errorf("the quit left %q running", left)
		}
	})
	t.Run("next", func(t *testing.T) {
		close(release)
		time.Sleep(20 * time.Millisecond)
		start := time.Now()
		chain, seen := run(t, harnessChain{}, harnessHop(1))
		if d := time.Since(start); d >= harnessRound {
			t.Errorf("five hops took %v", d)
		}
		for _, msg := range seen {
			if _, ok := msg.(harnessHop); !ok {
				t.Errorf("another test's message arrived: %T %v", msg, msg)
			}
		}
		if got := chain.(harnessChain).got; !slices.Equal(got, []int{1, 2, 3, 4, 5}) {
			t.Errorf("hops %v", got)
		}
	})
}

// harnessFakeT records what the driver reports, and stops as Fatalf does.
type harnessFakeT struct {
	errors, fatals []string
	cleanups       []func()
}

type harnessStop struct{}

func (*harnessFakeT) Helper() {}
func (f *harnessFakeT) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}
func (f *harnessFakeT) Fatalf(format string, args ...any) {
	f.fatals = append(f.fatals, fmt.Sprintf(format, args...))
	panic(harnessStop{})
}
func (f *harnessFakeT) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }

// end runs body as a test would, then the cleanups.
func (f *harnessFakeT) end(body func()) {
	func() {
		defer func() {
			if p := recover(); p != nil {
				if _, ok := p.(harnessStop); !ok {
					panic(p)
				}
			}
		}()
		body()
	}()
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

// harnessEcho answers every message with another, for ever.
type harnessEcho struct{}

func (harnessEcho) Init() tea.Cmd { return nil }
func (m harnessEcho) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return m, func() tea.Msg { return harnessToyMsg("again") }
}
func (harnessEcho) View() tea.View { return tea.NewView("") }

// harnessKeys takes keys and starts nothing.
type harnessKeys struct{ n int }

func (harnessKeys) Init() tea.Cmd { return nil }
func (m harnessKeys) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.KeyPressMsg); ok {
		m.n++
	}
	return m, nil
}
func (harnessKeys) View() tea.View { return tea.NewView("") }

// harnessCrash answers a key with a command that panics.
type harnessCrash struct{}

func (harnessCrash) Init() tea.Cmd { return nil }
func (m harnessCrash) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.KeyPressMsg); ok {
		return m, func() tea.Msg { panic("boom") }
	}
	return m, nil
}
func (harnessCrash) View() tea.View { return tea.NewView("") }

func TestHarnessFailsTheTestItDrives(t *testing.T) {
	t.Run("a model that never goes quiet", func(t *testing.T) {
		f := &harnessFakeT{}
		f.end(func() { harnessRun(f, harnessEcho{}, 0, []tea.Msg{harnessToyMsg("go")}) })
		if len(f.fatals) != 1 || !strings.Contains(f.fatals[0], "never went quiet") || strings.Count(f.fatals[0], "harnessToyMsg") != 10 {
			t.Errorf("fatals %q", f.fatals)
		}
		if len(f.errors) != 0 {
			t.Errorf("errors %q", f.errors)
		}
	})
	t.Run("many keys are not commands", func(t *testing.T) {
		f := &harnessFakeT{}
		var m tea.Model
		f.end(func() { m, _ = harnessRun(f, harnessKeys{}, 0, press(slices.Repeat([]string{"a"}, 2*harnessCap)...)) })
		if len(f.fatals) != 0 || m.(harnessKeys).n != 2*harnessCap {
			t.Errorf("fatals %q, keys %v", f.fatals, m)
		}
	})
	t.Run("a command that panics", func(t *testing.T) {
		f := &harnessFakeT{}
		f.end(func() { harnessRun(f, harnessCrash{}, 0, press("x")) })
		if len(f.fatals) != 1 || !strings.Contains(f.fatals[0], "a command panicked: boom") {
			t.Errorf("fatals %q", f.fatals)
		}
	})
	t.Run("a command left running", func(t *testing.T) {
		f := &harnessFakeT{}
		release := make(chan struct{})
		defer close(release)
		f.end(func() { harnessRun(f, harnessSlow{release: release}, 0, press("x")) })
		if len(f.errors) != 1 || !strings.Contains(f.errors[0], "1 command(s) still running") || !strings.Contains(f.errors[0], "harnessSlow") {
			t.Errorf("errors %q", f.errors)
		}
	})
	t.Run("await with nothing to wait for", func(t *testing.T) {
		f := &harnessFakeT{}
		f.end(func() { harnessRun(f, harnessKeys{}, time.Millisecond, nil) })
		if len(f.fatals) != 1 || !strings.Contains(f.fatals[0], "no command returned") {
			t.Errorf("fatals %q", f.fatals)
		}
	})
	t.Run("tea.Sequence", func(t *testing.T) {
		f := &harnessFakeT{}
		seq := tea.Sequence(func() tea.Msg { return nil }, func() tea.Msg { return nil })
		f.end(func() { harnessRun(f, harnessKeys{}, 0, []tea.Msg{seq()}) })
		if len(f.fatals) != 1 || !strings.Contains(f.fatals[0], "tea.Sequence") {
			t.Errorf("fatals %q", f.fatals)
		}
	})
}

func TestHarnessTicksFireInOrder(t *testing.T) {
	f := &fakeTicks{}
	start := f.now()
	var cmds []tea.Cmd
	for _, s := range []string{"a", "b", "c"} {
		cmds = append(cmds, f.tick(time.Second, func(time.Time) tea.Msg { return harnessToyMsg(s) }))
	}
	// Set in the reverse order of the calls, as goroutines may.
	for i := len(cmds) - 1; i >= 0; i-- {
		cmds[i]()
	}
	if got := f.advance(time.Second); !slices.Equal(got, []tea.Msg{harnessToyMsg("a"), harnessToyMsg("b"), harnessToyMsg("c")}) {
		t.Errorf("fired %v", got)
	}
	if got := f.now().Sub(start); got != time.Second {
		t.Errorf("the clock moved %v", got)
	}
}

func TestHarnessPressNamesTheKeyItMakes(t *testing.T) {
	for _, name := range []string{"q", "M", "?", "1", "]", "[", "enter", "esc", "tab", "shift+tab", "space", "backspace", "up", "shift+up", "shift+down", "alt+enter", "shift+enter", "ctrl+s", "ctrl+c", "ctrl+x", "ctrl+v", "ctrl+shift+c", "pgdown", "+", "-", "%"} {
		msg := press(name)[0].(tea.KeyPressMsg)
		if msg.String() != name {
			t.Errorf("press(%q) is %q", name, msg.String())
		}
	}
	if k := press("M")[0].(tea.KeyPressMsg); k.Code != 'm' || k.Mod != tea.ModShift || k.Text != "M" {
		t.Errorf("M is %+v", k)
	}
	if k := press("a")[0].(tea.KeyPressMsg); k.Text != "a" {
		t.Errorf("a carries no text: %+v", k)
	}
	if k := press("ctrl+s")[0].(tea.KeyPressMsg); k.Text != "" {
		t.Errorf("ctrl+s carries text: %+v", k)
	}
}

func TestHarnessGoldenComparesAndRewrites(t *testing.T) {
	m, _ := run(t, harnessToy{ticks: &fakeTicks{}}, size(60, 20))
	golden(t, "harness-toy", plain(m)+"\n")

	t.Run("rewrites", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv("ROTA_UPDATE_GOLDEN", "1")
		golden(t, "new", "one\n")
		b, err := os.ReadFile(filepath.Join("testdata", "golden", "new.txt"))
		if err != nil || string(b) != "one\n" {
			t.Fatalf("wrote %q, %v", b, err)
		}
		t.Setenv("ROTA_UPDATE_GOLDEN", "")
		golden(t, "new", "one\n")
	})
}
