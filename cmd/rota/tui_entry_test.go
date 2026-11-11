package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/tui"
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/store"
)

// tuiAt says whether run takes its two ends for a terminal the TUI can
// draw on, for this test alone.
func tuiAt(t *testing.T, on bool) {
	t.Helper()
	was := tuiTerminal
	tuiTerminal = func(io.Reader, io.Writer) bool { return on }
	t.Cleanup(func() { tuiTerminal = was })
}

// tuiStarts puts f where the TUI would start, for this test alone, and
// counts the starts.
func tuiStarts(t *testing.T, f func(ctx context.Context, o tui.Options) (tui.Result, error)) *int {
	t.Helper()
	was := startTUI
	n := new(int)
	startTUI = func(ctx context.Context, o tui.Options) (tui.Result, error) {
		*n++
		return f(ctx, o)
	}
	t.Cleanup(func() { startTUI = was })
	return n
}

// tuiNever is a TUI that must not start.
func tuiNever(t *testing.T) *int {
	t.Helper()
	return tuiStarts(t, func(context.Context, tui.Options) (tui.Result, error) {
		t.Error("the TUI started")
		return tui.Result{}, nil
	})
}

// Off a terminal — a script, a pipe, CI, rota-sdk-test running the binary —
// bare rota prints the short usage and exits 0, exactly as it always did.
func TestBareRotaOffATerminalPrintsUsage(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	started := tuiNever(t)
	out, errs, code := call(t)
	if code != 0 || out != shortUsage+"\n" || errs != "" {
		t.Fatalf("bare rota off a terminal: %d %q %q", code, out, errs)
	}
	if *started != 0 {
		t.Fatalf("the TUI started %d times", *started)
	}
	// A buffer at either end is no terminal, whatever the other end is.
	if tuiTerminal(os.Stdin, &bytes.Buffer{}) || tuiTerminal(strings.NewReader(""), os.Stdout) {
		t.Fatal("a buffer was taken for a terminal")
	}
}

// On a terminal it opens the TUI instead, with everything a local TUI
// needs: the Backend, the daemon's state, this rota to hand the terminal
// to, the machine's name and the two ends it was run with.
func TestBareRotaOnATerminalOpensTheTUI(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	tuiAt(t, true)
	var got tui.Options
	started := tuiStarts(t, func(_ context.Context, o tui.Options) (tui.Result, error) {
		got = o
		return tui.Result{}, nil
	})
	var out, errs bytes.Buffer
	if code := run(nil, &out, &errs); code != 0 || out.Len() != 0 || errs.Len() != 0 {
		t.Fatalf("bare rota on a terminal: %d %q %q", code, out.String(), errs.String())
	}
	if *started != 1 {
		t.Fatalf("the TUI started %d times; want once", *started)
	}
	if got.Backend == nil || got.Exec == nil || got.Daemon == nil {
		t.Fatalf("the TUI was started without its Backend, Exec or Daemon: %+v", got)
	}
	if got.Remote || got.Resize != nil || got.Cols != 0 || got.Rows != 0 {
		t.Fatalf("a local TUI was started as one over SSH: %+v", got)
	}
	if got.In != stdin || got.Out != io.Writer(&out) {
		t.Fatal("the TUI was not started on the ends run was given")
	}
	if host, _ := os.Hostname(); got.Host != host {
		t.Fatalf("Host is %q; want %q", got.Host, host)
	}
}

// --json with no command is usage, terminal or not: a program asked for a
// document, and a screen is not one.
func TestJSONAloneStillPrintsUsage(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	tuiAt(t, true)
	started := tuiNever(t)
	for _, args := range [][]string{{"--json"}, {"--json", "--json"}} {
		out, errs, code := call(t, args...)
		if code != 0 || out != shortUsage+"\n" || errs != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, errs)
		}
	}
	if *started != 0 {
		t.Fatalf("the TUI started %d times", *started)
	}
}

// The TUI's own failure is said as any command's is.
func TestATUIThatFailsSaysSo(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	tuiAt(t, true)
	tuiStarts(t, func(context.Context, tui.Options) (tui.Result, error) {
		return tui.Result{}, errors.New("the terminal went away")
	})
	out, errs, code := call(t)
	if code != 1 || out != "" || !strings.Contains(errs, "error: the terminal went away") {
		t.Fatalf("a failed TUI: %d %q %q", code, out, errs)
	}
}

// The local TUI is answered by an API served inside this process: nothing
// listens for it, no daemon is started or claimed, and what it reads is the
// store's.
func TestTheLocalTUIOpensNoPortAndNoSocket(t *testing.T) {
	shortHome(t) // one claude account, a@x
	home := os.Getenv("ROTA_HOME")
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	terminals, err := st.TerminalDir()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	none := func(when string) {
		t.Helper()
		for _, name := range []string{"daemon.lock", "ctl.sock", "share.sock"} {
			if _, err := os.Lstat(filepath.Join(terminals, name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: %s is there (%v)", when, name, err)
			}
		}
	}
	before := ownSockets(t)
	started := tuiStarts(t, func(ctx context.Context, o tui.Options) (tui.Result, error) {
		who, err := o.Backend.Who(ctx)
		if err != nil || who.Role != "control" || who.Name != "local" {
			t.Errorf("Who: %+v %v", who, err)
		}
		l, err := o.Backend.Accounts(ctx, tui.RefreshNone)
		if err != nil || len(l.Accounts) != 1 || l.Accounts[0].Email != "a@x" {
			t.Errorf("Accounts: %+v %v", l, err)
		}
		if d := o.Daemon(ctx); d.Running || d.Err != "" || len(d.Live) != 0 {
			t.Errorf("Daemon: %+v; want none running and nothing held", d)
		}
		none("while the TUI is open")
		if runtime.GOOS == "linux" {
			if opened := newListeners(t, before); len(opened) != 0 {
				t.Errorf("the TUI opened listening sockets: %v", opened)
			}
		}
		return tui.Result{}, nil
	})
	var out, errs bytes.Buffer
	c := &cli{out: &out, err: &errs, in: strings.NewReader("")}
	if err := c.openTUI(); err != nil {
		t.Fatal(err)
	}
	if *started != 1 {
		t.Fatalf("the TUI started %d times; want once", *started)
	}
	none("after the TUI")
	// Quick to quit: nothing to wait for, so nothing said.
	if out.Len() != 0 || errs.Len() != 0 {
		t.Fatalf("a TUI with nothing left said %q %q", out.String(), errs.String())
	}
	if runtime.GOOS != "linux" {
		t.Log("listening sockets are read from /proc on Linux alone")
	}
}

// The API in the TUI is R1's: the local principal's, refreshing nothing on
// its own (the daemon or the next command does), with no terminals, no page
// and no daemon claim. A run it starts may go on for a day, open or not,
// where the API's defaults would cut an Ask at ten minutes (F26).
func TestTheTUIsServerIsMadeAsR1Says(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	var got api.Options
	was := tuiServer
	tuiServer = func(o api.Options) (*api.Server, error) {
		got = o
		return was(o)
	}
	t.Cleanup(func() { tuiServer = was })
	tuiStarts(t, func(context.Context, tui.Options) (tui.Result, error) { return tui.Result{}, nil })
	if err := (&cli{out: io.Discard, err: io.Discard, in: strings.NewReader("")}).openTUI(); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.EvalSymlinks(home)
	dir, _ := filepath.EvalSymlinks(got.Dir)
	if dir != abs || len(got.Token) < 20 || got.RefreshEvery >= 0 || got.HoldCommand != nil || got.Log == nil {
		t.Fatalf("the TUI's server: %+v", got)
	}
	// Its log would land on the TUI's screen: it says nothing at all.
	if got.Log.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("the TUI's server logs")
	}
	if got.Routes == nil || *got.Routes != (api.Routes{API: true, WebSocket: true, Health: true}) {
		t.Fatalf("the TUI's server answers %+v", got.Routes)
	}
	if got.Timeout != 24*time.Hour || got.InputTimeout != 24*time.Hour {
		t.Fatalf("a run from the TUI is cut at %v, or %v open; want a day", got.Timeout, got.InputTimeout)
	}
	first := got.Token
	if err := (&cli{out: io.Discard, err: io.Discard, in: strings.NewReader("")}).openTUI(); err != nil {
		t.Fatal(err)
	}
	if got.Token == first {
		t.Fatal("two TUIs were served with the same token")
	}
}

// A login put aside and left when the TUI ends is said on the way out, in
// login-by-id's own words, and the command it names finishes it.
func TestAPendingLoginIsSaidOnTheWayOut(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no vendor CLI anywhere
	for _, c := range []struct {
		name string
		ago  time.Duration
		left string
	}{
		{"just begun", 0, strconv.Itoa(int(loginTTL/time.Minute)) + "m"},
		{"begun four minutes ago", 4 * time.Minute, strconv.Itoa(int(loginTTL/time.Minute)-4) + "m"},
		{"almost out", loginTTL - 30*time.Second, "(?:30|[12]?[0-9])s"},
		{"run out", loginTTL + time.Minute, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			tuiStarts(t, func(ctx context.Context, o tui.Options) (tui.Result, error) {
				l, err := o.Backend.BeginLogin(ctx, tui.LoginBegin{Provider: "t-cli-fake"})
				if err != nil {
					t.Fatal(err)
				}
				l.CreatedAt -= c.ago.Milliseconds()
				return tui.Result{Pending: &l}, nil
			})
			var out, errs bytes.Buffer
			if err := (&cli{out: &out, err: &errs, in: strings.NewReader("")}).openTUI(); err != nil {
				t.Fatal(err)
			}
			if c.left == "" {
				// Open for nobody: not offered.
				if out.Len() != 0 || errs.Len() != 0 {
					t.Fatalf("a login that has run out: %q %q", out.String(), errs.String())
				}
				return
			}
			m := regexp.MustCompile(`^The login stays open for ` + c.left + `: rota login ([0-9a-f]{6}) <key>\n$`).FindStringSubmatch(errs.String())
			if m == nil || out.Len() != 0 {
				t.Fatalf("on the way out: %q %q", out.String(), errs.String())
			}
			if c.ago != 0 {
				return
			}
			// The server in the TUI is gone, and the login it began is
			// still parked in the store for the command it named.
			if got, _, code := call(t, "login", m[1], "sk-test-key"); code != 0 || !strings.Contains(got, "Added t-cli-fake account") {
				t.Fatalf("finishing it: %d %q", code, got)
			}
		})
	}
}

// tuiSignalsFake stands in for signal.Notify: a test sends the signals
// itself, and sees what was asked for and when it was let go of.
type tuiSignalsFake struct {
	mu       sync.Mutex
	c        chan<- os.Signal
	sigs     []os.Signal
	released atomic.Int32
}

func fakeSignals(t *testing.T) *tuiSignalsFake {
	t.Helper()
	f := &tuiSignalsFake{}
	was := tuiSignals
	tuiSignals = func(c chan<- os.Signal, sigs ...os.Signal) func() {
		f.mu.Lock()
		f.c, f.sigs = c, sigs
		f.mu.Unlock()
		return func() { f.released.Add(1) }
	}
	t.Cleanup(func() { tuiSignals = was })
	return f
}

func (f *tuiSignalsFake) send(t *testing.T, sig os.Signal) {
	t.Helper()
	f.mu.Lock()
	c := f.c
	f.mu.Unlock()
	if c == nil {
		t.Fatal("no signal was asked for")
	}
	select {
	case c <- sig:
	case <-time.After(5 * time.Second):
		t.Fatalf("nobody took %v", sig)
	}
}

// The window closed under the TUI, or the process told to end: a hang-up
// or a terminate ends it as quitting does, with the status interrupted
// gives (130) and no error line. An interrupt while it runs is Bubble
// Tea's, or a CLI's the TUI has handed the terminal to, and ends nothing
// here.
func TestAHangUpEndsTheTUIAsQuittingDoes(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Setenv("ROTA_HOME", t.TempDir())
			tuiAt(t, true)
			f := fakeSignals(t)
			tuiStarts(t, func(ctx context.Context, o tui.Options) (tui.Result, error) {
				f.send(t, os.Interrupt)
				time.Sleep(50 * time.Millisecond)
				if ctx.Err() != nil {
					t.Error("an interrupt ended the TUI's context")
				}
				f.send(t, sig)
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
					t.Errorf("%v did not end the TUI", sig)
				}
				return tui.Result{}, fmt.Errorf("%w: %w", tea.ErrProgramKilled, ctx.Err())
			})
			out, errs, code := call(t)
			if code != 130 || out != "" || errs != "" {
				t.Fatalf("%v: %d %q %q", sig, code, out, errs)
			}
			if !slices.Equal(f.sigs, interruptSignals()) || f.released.Load() != 1 {
				t.Fatalf("asked for %v, let go %d times; want %v once", f.sigs, f.released.Load(), interruptSignals())
			}
		})
	}
	if runtime.GOOS != "windows" {
		for _, sig := range []os.Signal{syscall.SIGHUP, syscall.SIGTERM, os.Interrupt} {
			if !slices.Contains(interruptSignals(), sig) && !signal.Ignored(sig) {
				t.Fatalf("%v is not taken", sig)
			}
		}
	}
}

// An interrupt Bubble Tea took ends the TUI as an interrupt ends any
// command: status 130, and no error line.
func TestAnInterruptedTUIIsAnInterrupt(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	tuiAt(t, true)
	tuiStarts(t, func(context.Context, tui.Options) (tui.Result, error) {
		return tui.Result{}, tea.ErrInterrupted
	})
	if out, errs, code := call(t); code != 130 || out != "" || errs != "" {
		t.Fatalf("an interrupted TUI: %d %q %q", code, out, errs)
	}
}

// Quitting while a refresh is under way in the TUI's server waits for its
// answer and the save, says so once, and lets no interrupt or terminate cut
// the wait: the token it replaced is spent the moment the provider answers.
// Once it returns, the TUI's API is gone.
func TestQuittingWaitsForARefreshToBeSaved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@b.c","uuid":"u1","order":1,
		 "token":{"accessToken":"OLD-1","refreshToken":"R-OLD","expiresAt":1}}]}`)
	asked := make(chan struct{}, 1)
	answer := make(chan struct{})
	claudeAt(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
		select {
		case asked <- struct{}{}:
		default:
		}
		select {
		case <-answer:
		case <-time.After(30 * time.Second):
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "NEW-1", "refresh_token": "R-NEW", "expires_in": 3600})
	})
	f := fakeSignals(t)
	was := quitSayAfter
	quitSayAfter = 50 * time.Millisecond
	t.Cleanup(func() { quitSayAfter = was })
	var backend tui.Backend
	tuiStarts(t, func(ctx context.Context, o tui.Options) (tui.Result, error) {
		backend = o.Backend
		// The person quits while the listing's refresh is with the
		// provider: the TUI's own call ends, and the refresh goes on.
		call, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-asked:
			case <-time.After(20 * time.Second):
			}
			cancel()
		}()
		if _, err := o.Backend.Accounts(call, tui.RefreshForce); err == nil {
			t.Error("the listing was answered while the refresh was held")
		}
		return tui.Result{}, nil
	})
	said := &quitWriter{wrote: make(chan struct{}, 4)}
	done := make(chan error, 1)
	go func() { done <- (&cli{out: io.Discard, err: said, in: strings.NewReader("")}).openTUI() }()
	select {
	case <-said.wrote:
	case err := <-done:
		t.Fatalf("quitting returned (%v) without waiting for the refresh", err)
	case <-time.After(30 * time.Second):
		t.Fatal("quitting never said what it waits for")
	}
	f.send(t, os.Interrupt)
	f.send(t, syscall.SIGTERM)
	select {
	case err := <-done:
		t.Fatalf("quitting returned (%v) before the refresh was saved", err)
	case <-time.After(300 * time.Millisecond):
	}
	if n := f.released.Load(); n != 0 {
		t.Fatal("the signals were let go of while the save was still to come")
	}
	close(answer)
	select {
	case err := <-done:
		// The terminate sent meanwhile ends the command as interrupted
		// would, once the save is done.
		if err != errInterrupted {
			t.Fatalf("quitting ended with %v; want %v, for the terminate", err, errInterrupted)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("quitting never ended")
	}
	if got := said.String(); got != "rota: waiting for what the TUI started to finish, a refresh being saved among it\n" {
		t.Fatalf("quitting said %q", got)
	}
	if got := readAccounts(t, home).Accounts[0].Token.Access; got != "NEW-1" {
		t.Fatalf("the store holds %q; want the refreshed NEW-1", got)
	}
	if f.released.Load() != 1 {
		t.Fatalf("the signals were let go of %d times", f.released.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := backend.Who(ctx); err == nil {
		t.Fatal("the TUI's API still answers after quitting")
	}
}

// A panic out of the TUI still stops its server, waiting for a save as
// quitting does, on its way up.
func TestAPanicInTheTUIStillStopsItsServer(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	var backend tui.Backend
	tuiStarts(t, func(_ context.Context, o tui.Options) (tui.Result, error) {
		backend = o.Backend
		panic("the TUI broke")
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_ = (&cli{out: io.Discard, err: io.Discard, in: strings.NewReader("")}).openTUI()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := backend.Who(ctx); err == nil {
		t.Fatal("the TUI's API still answers after a panic")
	}
}

// Quitting waits for what the in-process server still has under way, a
// refresh being saved among it. A quick quit says nothing; one that waits
// past quitSayAfter says so once, while it is still waiting.
func TestQuittingSaysOnceWhatItWaitsFor(t *testing.T) {
	var quick bytes.Buffer
	q := &quitSaid{w: &quick}
	q.begin()
	q.done()
	if quick.Len() != 0 {
		t.Fatalf("a quick quit said %q", quick.String())
	}

	was := quitSayAfter
	quitSayAfter = 20 * time.Millisecond
	t.Cleanup(func() { quitSayAfter = was })
	said := &quitWriter{wrote: make(chan struct{}, 4)}
	q = &quitSaid{w: said}
	q.begin()
	q.begin() // a second begin counts nothing again
	select {
	case <-said.wrote:
	case <-time.After(10 * time.Second):
		t.Error("nothing was said while the wait went on")
	}
	time.Sleep(50 * time.Millisecond) // past a second quitSayAfter
	q.done()
	if got := said.String(); got != "rota: waiting for what the TUI started to finish, a refresh being saved among it\n" {
		t.Fatalf("a slow quit said %q", got)
	}
	// Once done, nothing more is said, nor is a count begun.
	q.begin()
	time.Sleep(50 * time.Millisecond)
	if got := said.String(); got != "rota: waiting for what the TUI started to finish, a refresh being saved among it\n" {
		t.Fatalf("after done it said %q", got)
	}
}

// The wait quitting says is counted from the program's end, when the
// screen is given back, so the TUI's own leaving (a page closed, a run
// stopped, up to ten seconds) is part of it: a TUI that ran long and left
// at once says nothing, and one still leaving past quitSayAfter says so
// once, while it is leaving.
func TestQuittingCountsTheWaitFromTheProgramsEnd(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	fakeSignals(t)
	was := quitSayAfter
	quitSayAfter = 300 * time.Millisecond
	t.Cleanup(func() { quitSayAfter = was })

	tuiStarts(t, func(_ context.Context, o tui.Options) (tui.Result, error) {
		time.Sleep(2 * quitSayAfter)
		o.Ended()
		return tui.Result{}, nil
	})
	var quick bytes.Buffer
	if err := (&cli{out: io.Discard, err: &quick, in: strings.NewReader("")}).openTUI(); err != nil || quick.Len() != 0 {
		t.Fatalf("a TUI that ran long and left at once ended with %v, saying %q", err, quick.String())
	}

	said := &quitWriter{wrote: make(chan struct{}, 4)}
	tuiStarts(t, func(_ context.Context, o tui.Options) (tui.Result, error) {
		o.Ended()
		select {
		case <-said.wrote:
		case <-time.After(10 * time.Second):
			t.Error("nothing was said while the TUI was still leaving")
		}
		time.Sleep(2 * quitSayAfter) // past a second quitSayAfter
		return tui.Result{}, nil
	})
	if err := (&cli{out: io.Discard, err: said, in: strings.NewReader("")}).openTUI(); err != nil {
		t.Fatalf("openTUI: %v", err)
	}
	if got := said.String(); got != "rota: waiting for what the TUI started to finish, a refresh being saved among it\n" {
		t.Fatalf("a slow leave said %q", got)
	}
}

// quitWriter is a buffer that says when it is written to.
type quitWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	wrote chan struct{}
}

func (w *quitWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	return n, err
}

func (w *quitWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// The TUI hands the terminal to this very rota, with the arguments it is
// given, the environment it has and nothing more: natural by default.
// Bubble Tea fills in the three ends.
func TestSelfCmdIsThisRotaAsItIs(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := selfCmd("run", "2")
	if cmd.Path != exe || !slices.Equal(cmd.Args, []string{exe, "run", "2"}) {
		t.Fatalf("selfCmd is %q %q; want %q with run 2", cmd.Path, cmd.Args, exe)
	}
	if !slices.Equal(cmd.Env, os.Environ()) {
		t.Fatal("selfCmd's environment is not this process's")
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil || cmd.Dir != "" {
		t.Fatalf("selfCmd set what Bubble Tea fills in: %+v", cmd)
	}
}

// A browser is opened where one can be (R19): open on macOS, xdg-open on a
// Linux with a display, and nowhere else. The URL is one argument, never a
// shell's to read.
func TestBrowserIsOnlyWhereOneCanOpen(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", t.TempDir()) // the opener is found by its path alone
	opened := filepath.Join(t.TempDir(), "opened")
	fake := fakecli.Install(t, bin, "opener", fakecli.Spec{Touch: opened})
	failing := fakecli.Install(t, bin, "failing", fakecli.Spec{Exit: 3})
	slow := fakecli.Install(t, bin, "slow", fakecli.Spec{Sleep: "3s"})
	browserAs := func(goos string, commands map[string]string) func(string) error {
		t.Helper()
		wasOS, wasCmds := browserOS, browserCommands
		browserOS, browserCommands = goos, commands
		defer func() { browserOS, browserCommands = wasOS, wasCmds }()
		return localBrowser()
	}
	everywhere := map[string]string{"darwin": fake, "linux": fake, "windows": fake, "freebsd": fake}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	for _, c := range []struct {
		goos, display, wayland string
		some                   bool
	}{
		{"darwin", "", "", true},
		{"linux", "", "", false},
		{"linux", ":0", "", true},
		{"linux", "", "wayland-0", true},
		{"windows", ":0", "", false},
		{"freebsd", ":0", "", false},
	} {
		t.Setenv("DISPLAY", c.display)
		t.Setenv("WAYLAND_DISPLAY", c.wayland)
		if got := browserAs(c.goos, everywhere); (got != nil) != c.some {
			t.Errorf("%s with DISPLAY=%q WAYLAND_DISPLAY=%q: a browser is %v; want %v", c.goos, c.display, c.wayland, got != nil, c.some)
		}
	}
	if browserCommands["darwin"] != "open" || browserCommands["linux"] != "xdg-open" || len(browserCommands) != 2 {
		t.Fatalf("the openers are %v", browserCommands)
	}

	t.Setenv("DISPLAY", "")
	open := browserAs("darwin", map[string]string{"darwin": fake})
	if err := open("https://claude.ai/oauth?a=1&b=2;x=$(y)"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(opened); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the opener never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := browserAs("darwin", map[string]string{"darwin": failing})("https://x.test/"); err == nil {
		t.Fatal("an opener that failed was not said")
	}
	// One that does not end soon, as xdg-open waiting on a browser it
	// started, is taken as opened; it is not waited on.
	was := browserWait
	browserWait = 100 * time.Millisecond
	t.Cleanup(func() { browserWait = was })
	began := time.Now()
	if err := browserAs("darwin", map[string]string{"darwin": slow})("https://x.test/"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("a slow opener held the TUI for %v", d)
	}
	if args := browserCmd("xdg-open", "https://x.test/a b?c=1&d=2;rm -rf ~").Args; !slices.Equal(args, []string{"xdg-open", "https://x.test/a b?c=1&d=2;rm -rf ~"}) {
		t.Fatalf("the URL was not one argument: %q", args)
	}
}

// ownSockets is the inode of every socket this process holds open, from
// /proc/self/fd: nil where there is no /proc.
func ownSockets(t *testing.T) map[string]bool {
	t.Helper()
	if runtime.GOOS != "linux" {
		return nil
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	own := map[string]bool{}
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err != nil {
			continue // the directory's own descriptor, closed by now
		}
		if inode, ok := strings.CutPrefix(link, "socket:["); ok {
			own[strings.TrimSuffix(inode, "]")] = true
		}
	}
	return own
}

// newListeners is every socket this process opened since before that
// listens, on TCP or a unix path: only this process's, so another test
// binary's server on the machine is nobody's concern here.
func newListeners(t *testing.T, before map[string]bool) []string {
	t.Helper()
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) { // tcp6 without IPv6
			t.Fatal(err)
		}
		return string(raw)
	}
	listening := procListening(read("/proc/net/tcp")+read("/proc/net/tcp6"), read("/proc/net/unix"))
	var opened []string
	for inode := range ownSockets(t) {
		if !before[inode] && listening[inode] {
			opened = append(opened, inode)
		}
	}
	return opened
}

// procListening is the inodes of the listening sockets in /proc/net/tcp
// (and tcp6) and /proc/net/unix, as they read: a TCP socket in state 0A, a
// unix one flagged __SO_ACCEPTCON.
func procListening(tcp, unix string) map[string]bool {
	out := map[string]bool{}
	for line := range strings.Lines(tcp) {
		f := strings.Fields(line)
		if len(f) >= 10 && f[3] == "0A" {
			out[f[9]] = true
		}
	}
	sc := bufio.NewScanner(strings.NewReader(unix))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 7 && f[3] == "00010000" {
			out[f[6]] = true
		}
	}
	return out
}

// The /proc reading is checked here on every platform, since the test that
// relies on it runs on Linux alone.
func TestTheProcListingIsReadAsItIs(t *testing.T) {
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:2254 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41001 1 0000000000000000 100 0 0 10 0
   1: 0100007F:A2B4 0100007F:2254 01 00000000:00000000 00:00000000 00000000  1000        0 41002 1 0000000000000000 20 4 30 10 -1
`
	unix := `Num       RefCount Protocol Flags    Type St Inode Path
0000000000000000: 00000002 00000000 00010000 0001 01 51001 /run/x/ctl.sock
0000000000000000: 00000003 00000000 00000000 0001 03 51002 /run/x/ctl.sock
0000000000000000: 00000002 00000000 00010000 0005 01 51003 @abstract
`
	got := procListening(tcp, unix)
	want := map[string]bool{"41001": true, "51001": true, "51003": true}
	if len(got) != len(want) {
		t.Fatalf("listening: %v; want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("listening: %v; want %v", got, want)
		}
	}
}
