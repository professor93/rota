package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/professor93/rota/api"
	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/cmd/rota/tui"
	"github.com/professor93/rota/internal/holdwire"
	rota "github.com/professor93/rota/lib"
)

// tuiTerminal says whether bare rota opens the TUI on these two ends: only
// when both are a terminal, the terminal is not a dumb one, and this is
// Linux or macOS (R4). Anywhere else bare rota prints the short usage, as
// it always has. A variable so a test can say yes for a buffer.
var tuiTerminal = func(in io.Reader, out io.Writer) bool {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, ok := in.(*os.File)
	fo, ok2 := out.(*os.File)
	return ok && ok2 && isTerminal(fi) && isTerminal(fo)
}

// startTUI runs the TUI, and tuiServer makes the API it is answered by.
// Variables so a test can stand in for the one and see what the other is
// made with.
var (
	startTUI  = tui.Run
	tuiServer = api.New
)

// tuiRunCap is how long a run the local TUI starts may go on, one that
// stays open as well: a day, where the API's own default would cut an Ask
// at ten minutes and a foreground `rota run` is never cut at all.
const tuiRunCap = 24 * time.Hour

// quitSayAfter is how long quitting may wait silently for what the server
// in the TUI still has under way before it says so.
var quitSayAfter = time.Second

// openTUI is bare rota on a terminal: the TUI, answered by rota's own API
// served inside this process to the local principal (R1). No port and no
// socket is opened for it, and no daemon is started or claimed; the daemon,
// when one runs, is asked only for the sessions it holds.
func (c *cli) openTUI() error {
	home, terminals, err := daemonStore()
	if err != nil {
		return err
	}
	srv, err := tuiServer(api.Options{
		Dir:          home,
		Token:        rand.Text(),
		RefreshEvery: -1,
		Routes:       &api.Routes{API: true, WebSocket: true, Health: true},
		Timeout:      tuiRunCap,
		InputTimeout: tuiRunCap,
		Log:          slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return err
	}
	cl, stop := rotaclient.InProcess(srv.LocalHandler(&ctlControl))
	host, _ := os.Hostname()
	res, err := startTUI(context.Background(), tui.Options{
		Backend: tui.NewHTTP(cl.HTTP(), "http://rota"),
		In:      c.in,
		Out:     c.out,
		Daemon:  localDaemon(terminals),
		Exec:    selfCmd,
		OpenURL: localBrowser(),
		Host:    host,
	})
	quitWait(c.err, func() {
		stop()
		srv.Stop()
	})
	if l := res.Pending; l != nil {
		// The TUI's reading of the login carries no state, and finishLater
		// needs none: the id and the kind say the command (R18).
		ends := time.UnixMilli(l.CreatedAt).Add(loginTTL)
		fmt.Fprintf(c.err, "The login stays open for %s: %s\n", timeLeft(ends), finishLater(&rota.Login{ID: l.ID, Kind: l.Kind}))
	}
	return err
}

// quitWait runs stop, which waits for a refresh under way to be answered
// and saved, up to store.RefreshTimeout, and for a run kept open to be
// closed. Once that has taken quitSayAfter it says so on w, once, so that
// nobody takes a TUI that has gone for one that hangs.
func quitWait(w io.Writer, stop func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		stop()
	}()
	t := time.NewTimer(quitSayAfter)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
		fmt.Fprintln(w, "rota: waiting for a refresh to be saved")
	}
	<-done
}

// localDaemon is what the header says about this store's daemon. When one
// holds the lock it is asked over its socket for the sessions it holds, as
// `rota list --sessions` asks it. When none does, the holders still running
// are read from their records: a session outlives the daemon that started
// it, and is still there to attach to. Each call ends when ctx does and
// leaves nothing running behind it.
func localDaemon(terminals string) func(ctx context.Context) tui.Daemon {
	return func(ctx context.Context) tui.Daemon {
		d, running := holdwire.RunningDaemon(terminals)
		if !running {
			return heldLeft(terminals)
		}
		out := tui.Daemon{Running: true, PID: d.PID}
		if err := ctx.Err(); err != nil {
			out.Err = err.Error()
			return out
		}
		cl, err := rotaclient.DialNow(terminals)
		if errors.Is(err, rotaclient.ErrNoDaemon) {
			return heldLeft(terminals) // it stopped meanwhile
		}
		if err != nil {
			out.Err = err.Error()
			return out
		}
		sessions, err := daemonSessions(ctx, cl)
		if err != nil {
			out.Err = err.Error()
			return out
		}
		out.Live = liveSessions(sessions)
		return out
	}
}

// liveSessions is the sessions the daemon listed that are still running.
func liveSessions(sessions []rotaclient.Session) []tui.Live {
	var live []tui.Live
	for _, s := range sessions {
		if s.Ended == nil {
			live = append(live, tui.Live{ID: s.ID, Name: s.Name, Account: s.Account})
		}
	}
	return live
}

// daemonSessions is the client's List under ctx: GET /v1/terminals, cut
// short when ctx ends, where List keeps going for its own thirty seconds.
func daemonSessions(ctx context.Context, cl *rotaclient.Client) ([]rotaclient.Session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://rota/v1/terminals", nil)
	if err != nil {
		return nil, err
	}
	resp, err := cl.HTTP().Do(req)
	if err != nil {
		// The URL is the client's own, and says nothing.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("the rota daemon did not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the rota daemon answered %s", resp.Status)
	}
	var doc struct {
		Terminals []rotaclient.Session `json:"terminals"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("the rota daemon's sessions could not be read: %w", err)
	}
	return doc.Terminals, nil
}

// heldLeft is no daemon, and the sessions whose holders still run.
func heldLeft(terminals string) tui.Daemon {
	var out tui.Daemon
	for _, r := range holdwire.Scan(terminals) {
		if r.State == holdwire.StateLive && r.Meta != nil {
			out.Live = append(out.Live, tui.Live{ID: r.ID, Name: r.Name(), Account: r.Meta.Account})
		}
	}
	return out
}

// selfCmd is this rota with args, on the environment it has and nothing
// more. Its three ends are left for Bubble Tea to fill in when it hands the
// terminal over.
func selfCmd(args ...string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = os.Environ()
	return cmd
}

// browserOS and browserCommands are where a browser is opened from, and
// with what: variables so a test can point them at a fake.
var (
	browserOS       = runtime.GOOS
	browserCommands = map[string]string{"darwin": "open", "linux": "xdg-open"}
)

// browserWait is how long an opener is given to say it failed. One still
// running after it, as xdg-open waiting on a browser it started, is taken
// as having opened the page.
var browserWait = 5 * time.Second

// localBrowser opens a page in a browser on this machine (R19): open on macOS,
// and xdg-open on Linux when a display is there to show one. Elsewhere it
// is nil, and the TUI offers no o.
func localBrowser() func(url string) error {
	switch browserOS {
	case "darwin":
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return nil
		}
	default:
		return nil
	}
	name := browserCommands[browserOS]
	if name == "" {
		return nil
	}
	return func(url string) error {
		cmd := browserCmd(name, url)
		if err := cmd.Start(); err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }() // reaps it, however long it runs
		t := time.NewTimer(browserWait)
		defer t.Stop()
		select {
		case err := <-done:
			return err
		case <-t.C:
			return nil
		}
	}
}

// browserCmd is the opener with the URL as its one argument, no shell
// between: nothing in a URL is ever read as a command. Its ends are
// /dev/null, so nothing it says lands on the TUI's screen.
func browserCmd(name, url string) *exec.Cmd {
	return exec.Command(name, url)
}
