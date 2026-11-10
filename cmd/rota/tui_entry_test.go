package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
		{"almost out", loginTTL - 30*time.Second, "(30|29|28)s"},
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

// Quitting waits for what the in-process server still has under way, a
// refresh being saved among it. A quick quit says nothing; one that waits
// past quitSayAfter says so once, while it is still waiting.
func TestQuittingSaysOnceWhatItWaitsFor(t *testing.T) {
	var quick bytes.Buffer
	quitWait(&quick, func() {})
	if quick.Len() != 0 {
		t.Fatalf("a quick quit said %q", quick.String())
	}

	was := quitSayAfter
	quitSayAfter = 20 * time.Millisecond
	t.Cleanup(func() { quitSayAfter = was })
	said := &quitWriter{wrote: make(chan struct{}, 4)}
	stopped := false
	quitWait(said, func() {
		select {
		case <-said.wrote:
		case <-time.After(10 * time.Second):
			t.Error("nothing was said while the wait went on")
		}
		time.Sleep(50 * time.Millisecond) // past a second quitSayAfter
		stopped = true
	})
	if !stopped {
		t.Fatal("quitWait returned before the stop did")
	}
	if got := said.String(); got != "rota: waiting for a refresh to be saved\n" {
		t.Fatalf("a slow quit said %q", got)
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
