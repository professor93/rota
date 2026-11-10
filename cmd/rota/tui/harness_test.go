package tui

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
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
	// harnessRound is how long a round waits for the commands under way.
	// A round ends early once all of them have returned.
	harnessRound = 200 * time.Millisecond
	// harnessCap is how many messages one run may deliver before it gives
	// up on a model that never goes quiet.
	harnessCap = 1000
)

type harnessResult struct {
	seq   uint64
	msg   tea.Msg
	panic any
	stack []byte
}

// harnessDriver holds the commands still running when a run went quiet. The
// next run takes their messages when they return, as Bubble Tea would hand
// them to the same program. A test that leaves one running lets it return
// before it ends, or the next test's run receives its message.
var harnessDriver struct {
	sync.Mutex
	seq     uint64
	pending map[uint64]bool
	results chan harnessResult
}

func harnessStart(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	harnessDriver.Lock()
	if harnessDriver.results == nil {
		harnessDriver.results = make(chan harnessResult, 64)
		harnessDriver.pending = map[uint64]bool{}
	}
	harnessDriver.seq++
	seq, results := harnessDriver.seq, harnessDriver.results
	harnessDriver.pending[seq] = true
	harnessDriver.Unlock()
	go func() {
		r := harnessResult{seq: seq}
		defer func() {
			if p := recover(); p != nil {
				r.panic, r.stack = p, debug.Stack()
			}
			results <- r
		}()
		r.msg = cmd()
	}()
}

func harnessPending() int {
	harnessDriver.Lock()
	defer harnessDriver.Unlock()
	return len(harnessDriver.pending)
}

// harnessCollect waits one round for the commands under way and returns what
// came back, in the order the commands were started.
func harnessCollect() []harnessResult {
	harnessDriver.Lock()
	results := harnessDriver.results
	harnessDriver.Unlock()
	if results == nil {
		return nil
	}
	var got []harnessResult
	deadline := time.NewTimer(harnessRound)
	defer deadline.Stop()
	for harnessPending() > 0 {
		select {
		case r := <-results:
			harnessDriver.Lock()
			delete(harnessDriver.pending, r.seq)
			harnessDriver.Unlock()
			if r.panic != nil {
				harnessForget()
				panic(fmt.Sprintf("a command panicked: %v\n%s", r.panic, r.stack))
			}
			got = append(got, r)
		case <-deadline.C:
			return harnessInOrder(got)
		}
	}
	return harnessInOrder(got)
}

func harnessInOrder(got []harnessResult) []harnessResult {
	slices.SortFunc(got, func(a, b harnessResult) int { return cmp.Compare(a.seq, b.seq) })
	return got
}

// harnessForget drops the commands still running, so a run that panicked
// leaves nothing behind for the next test.
func harnessForget() {
	harnessDriver.Lock()
	defer harnessDriver.Unlock()
	harnessDriver.results, harnessDriver.pending = nil, nil
}

// run sends each message to m's Update and runs every command that comes
// back, each in its own goroutine, until the model is quiet: nothing left to
// deliver and nothing returning within a round. A tea.BatchMsg, given or
// returned, is expanded into its commands, so run(m, tea.BatchMsg{m.Init()})
// starts a model. A command slower than a round stays pending and its
// message goes to the next run. tea.QuitMsg ends the run, as it ends the
// program; it is not delivered.
//
// It returns every message the commands produced, in the order delivered:
// the effects (handOffMsg, clipboardMsg, bellMsg) and anything the model
// ignores. More than harnessCap messages in one run panic with the last ten.
func run(m tea.Model, msgs ...tea.Msg) (tea.Model, []tea.Msg) {
	var seen, last []tea.Msg
	delivered := 0
	queue := slices.Clone(msgs)
	fromCommands := make([]bool, len(queue))
	for {
		for len(queue) > 0 {
			msg, produced := queue[0], fromCommands[0]
			queue, fromCommands = queue[1:], fromCommands[1:]
			switch msg := msg.(type) {
			case nil:
				continue
			case tea.BatchMsg:
				for _, cmd := range msg {
					harnessStart(cmd)
				}
				continue
			case tea.QuitMsg:
				return m, append(seen, msg)
			}
			if produced {
				seen = append(seen, msg)
			}
			delivered++
			last = append(last, msg)
			if len(last) > 10 {
				last = last[1:]
			}
			if delivered > harnessCap {
				var b strings.Builder
				for _, l := range last {
					fmt.Fprintf(&b, "\n  %T %v", l, l)
				}
				harnessForget()
				panic(fmt.Sprintf("the model never went quiet: %d messages, the last ten:%s", delivered, b.String()))
			}
			var cmd tea.Cmd
			m, cmd = m.Update(msg)
			harnessStart(cmd)
		}
		if harnessPending() == 0 {
			return m, seen
		}
		got := harnessCollect()
		if len(got) == 0 {
			return m, seen
		}
		for _, r := range got {
			queue = append(queue, r.msg)
			fromCommands = append(fromCommands, true)
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
// advances the clock past it, so nothing waits on real time.
type fakeTicks struct {
	mu     sync.Mutex
	now    time.Time
	timers []harnessTimer
}

type harnessTimer struct {
	at time.Time
	fn func(time.Time) tea.Msg
}

// tick is tea.Tick's signature. Its command sets the timer and returns no
// message; advance returns the message once the time has come.
func (f *fakeTicks) tick(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.now.IsZero() {
			f.now = time.Unix(1_800_000_000, 0)
		}
		f.timers = append(f.timers, harnessTimer{at: f.now.Add(d), fn: fn})
		return nil
	}
}

// advance moves the clock by d and returns the messages of every timer that
// came due, earliest first.
func (f *fakeTicks) advance(d time.Duration) []tea.Msg {
	f.mu.Lock()
	if f.now.IsZero() {
		f.now = time.Unix(1_800_000_000, 0)
	}
	f.now = f.now.Add(d)
	var due, left []harnessTimer
	for _, t := range f.timers {
		if !t.at.After(f.now) {
			due = append(due, t)
		} else {
			left = append(left, t)
		}
	}
	f.timers = left
	f.mu.Unlock()
	slices.SortStableFunc(due, func(a, b harnessTimer) int { return a.at.Compare(b.at) })
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
	m, seen := run(m, tea.BatchMsg{m.Init()})
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
	m, _ = run(m, ticks.advance(time.Millisecond)...)
	if got := m.(harnessToy).got; got[len(got)-1] != "tick" {
		t.Fatalf("after a second: %q", got)
	}
	m, _ = run(m, size(80, 24))
	if got := m.(harnessToy).got; got[len(got)-1] != "size 80x24" {
		t.Fatalf("after a resize: %q", got)
	}
	m, seen = run(m, press("x", "q")...)
	if len(seen) != 1 || seen[0] != (tea.QuitMsg{}) {
		t.Errorf("q saw %v, want the quit", seen)
	}
	if got := m.(harnessToy).got; got[len(got)-1] != "key q" {
		t.Errorf("after q: %q", got)
	}
	// The program ends at the quit: nothing after it is delivered.
	m, _ = run(m, append([]tea.Msg{tea.QuitMsg{}}, press("y")...)...)
	if got := m.(harnessToy).got; got[len(got)-1] != "key q" {
		t.Errorf("a key after the quit was delivered: %q", got)
	}
	if strings.Contains(plain(m), "\x1b") || !strings.HasPrefix(plain(m), "got ") {
		t.Errorf("plain: %q", plain(m))
	}
}

// harnessSlow returns its message only once released.
type harnessSlow struct {
	release chan struct{}
	got     []string
}

func (m harnessSlow) Init() tea.Cmd { return nil }

func (m harnessSlow) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case harnessToyMsg:
		m.got = append(slices.Clone(m.got), string(msg))
	case tea.KeyPressMsg:
		return m, func() tea.Msg { <-m.release; return harnessToyMsg("late") }
	}
	return m, nil
}

func (m harnessSlow) View() tea.View { return tea.NewView(strings.Join(m.got, ",")) }

func TestTheDriverKeepsASlowCommandForTheNextRun(t *testing.T) {
	release := make(chan struct{})
	var m tea.Model = harnessSlow{release: release}
	start := time.Now()
	m, _ = run(m, press("x")...)
	if len(m.(harnessSlow).got) != 0 {
		t.Fatalf("delivered before it returned: %q", m.(harnessSlow).got)
	}
	if d := time.Since(start); d < harnessRound {
		t.Errorf("went quiet after %v, before a round", d)
	}
	close(release)
	m, seen := run(m)
	if got := m.(harnessSlow).got; !slices.Equal(got, []string{"late"}) || len(seen) != 1 {
		t.Fatalf("the next run delivered %q and saw %v", got, seen)
	}
}

// harnessEcho answers every message with another, for ever.
type harnessEcho struct{}

func (harnessEcho) Init() tea.Cmd { return nil }
func (m harnessEcho) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return m, func() tea.Msg { return harnessToyMsg("again") }
}
func (harnessEcho) View() tea.View { return tea.NewView("") }

func TestTheDriverGivesUpOnAModelThatNeverGoesQuiet(t *testing.T) {
	defer func() {
		p := recover()
		s, _ := p.(string)
		if !strings.Contains(s, "never went quiet") || strings.Count(s, "harnessToyMsg") != 10 {
			t.Errorf("panic: %v", p)
		}
	}()
	run(harnessEcho{}, harnessToyMsg("go"))
	t.Error("run returned")
}

func TestPressNamesTheKeyItMakes(t *testing.T) {
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

func TestGoldenComparesAndRewrites(t *testing.T) {
	m, _ := run(harnessToy{ticks: &fakeTicks{}}, size(60, 20))
	golden(t, "harness-toy", plain(m)+"\n")
}
