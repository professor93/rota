//go:build linux || darwin

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

// These are about ctl.sock, the daemon's control socket: who reaches it,
// that it is the daemon's alone, and that a process inside a held session
// may only watch there.

// The probe is this test binary run as a process of its own, in a session of
// its own: for every line it is given it asks ctl.sock to start a terminal,
// over a new connection each time, and prints the status it was answered.
// The body is not JSON, so a caller allowed to change things is answered by
// the route itself, 400, and a watcher by the guard, 403; nothing is started.
func init() {
	if os.Getenv("ROTA_CTL_PROBE") == "1" {
		os.Exit(ctlProbe(os.Stdin, os.Stdout, os.Getenv("ROTA_CTL_SOCK")))
	}
}

func ctlProbe(in io.Reader, out io.Writer, sock string) int {
	lines := bufio.NewScanner(in)
	for lines.Scan() {
		code := 0
		resp, err := ctlClient(sock).Post("http://rota/v1/terminals", "application/json", strings.NewReader("{"))
		if err == nil {
			code = resp.StatusCode
			resp.Body.Close()
		}
		fmt.Fprintln(out, code)
	}
	return 0
}

// ctlClient talks HTTP over the socket, one connection per request: the role
// is the connection's, so a test about it must not be answered on one opened
// before.
func ctlClient(sock string) *nethttp.Client {
	return &nethttp.Client{Timeout: 20 * time.Second, Transport: &nethttp.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

// ctlGet is one GET over the socket.
func ctlGet(t *testing.T, sock, path string) (int, map[string]any) {
	t.Helper()
	resp, err := ctlClient(sock).Get("http://rota" + path)
	if err != nil {
		t.Fatalf("GET %s over %s: %v", path, sock, err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	return resp.StatusCode, doc
}

// ctlStore is a short store and its terminals directory.
func ctlStore(t *testing.T) (home, terminals string) {
	t.Helper()
	shortHome(t)
	home = os.Getenv("ROTA_HOME")
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	terminals, err = st.TerminalDir()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := holdwire.SockFits(holdwire.CtlSock(terminals)); err != nil {
		t.Skipf("no control socket fits here: %v", err)
	}
	return home, terminals
}

// ctlDaemon is a server holding the store's daemon claim, which api.New takes
// for a server with HoldCommand. Nothing here starts a terminal, so the
// command is never run.
func ctlDaemon(t *testing.T) (*api.Server, string) {
	t.Helper()
	home, terminals := ctlStore(t)
	srv, err := api.New(api.Options{Token: "x", Dir: home, RefreshEvery: -1,
		HoldCommand: func() *exec.Cmd { return exec.Command(os.DevNull) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return srv, terminals
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The socket is its owner's alone — 0600, in a directory that is 0700 — and
// that is all its authorization: a caller outside every held session is this
// machine's user, and may control. It answers health like any server, and
// stopping it takes the file with it.
func TestCtlSockAnswersOnlyItsOwner(t *testing.T) {
	srv, terminals := ctlDaemon(t)
	stop, err := listenCtl(srv, terminals, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	sock := holdwire.CtlSock(terminals)
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != fs.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Fatalf("ctl.sock is %v, want a socket with 0600", fi.Mode())
	}
	if code, doc := ctlGet(t, sock, "/v1/health"); code != 200 || doc["ok"] != true {
		t.Fatalf("GET /v1/health over ctl.sock answered %d %v", code, doc)
	}
	if code, doc := ctlGet(t, sock, "/v1/session"); code != 200 ||
		doc["name"] != "local" || doc["role"] != "control" || doc["via"] != "local" {
		t.Fatalf("this test, inside no held session, is %d %v; want local with control", code, doc)
	}
	stop()
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("ctl.sock is still there after stop: %v", err)
	}
}

// A process inside a held session — one whose session is a live record's
// holder or child — is local with the watch role, so the agent in a held
// session cannot start, type into or end one. The same process outside
// every held session controls, and so does it once the record is nobody's.
func TestACallerInsideAHeldSessionOnlyWatches(t *testing.T) {
	srv, terminals := ctlDaemon(t)
	stop, err := listenCtl(srv, terminals, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), "ROTA_CTL_PROBE=1", "ROTA_CTL_SOCK="+holdwire.CtlSock(terminals))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
	t.Cleanup(func() { in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := cmd.Process.Pid
	answers := bufio.NewScanner(out)
	probe := func() string {
		t.Helper()
		fmt.Fprintln(in, "go")
		if !answers.Scan() {
			t.Fatalf("the probe said nothing: %v", answers.Err())
		}
		return answers.Text()
	}

	if got := probe(); got != "400" {
		t.Fatalf("the probe outside every held session was answered %s, want the route's own 400", got)
	}

	dir := holdwire.Dir(terminals, "00000000000000c7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := func(holder, child int) {
		t.Helper()
		m := &holdwire.Meta{ID: "00000000000000c7", Kind: holdwire.KindAccount, Account: 1, Provider: "claude",
			HolderPID: holder, ChildPID: child, Protocol: holdwire.Major, Version: wire.Version, Started: time.Now()}
		if err := holdwire.WriteMeta(dir, m); err != nil {
			t.Fatal(err)
		}
	}
	record(0, pid)
	release, err := holdwire.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if got := probe(); got != "403" {
		t.Fatalf("the probe as a held session's child was answered %s, want 403", got)
	}
	// The holder's own session counts the same.
	record(pid, 0)
	if got := probe(); got != "403" {
		t.Fatalf("the probe as a held session's holder was answered %s, want 403", got)
	}
	// A record nobody holds is a session that has stopped, and nothing
	// running is inside it.
	release()
	if got := probe(); got != "400" {
		t.Fatalf("the probe named by a stopped record was answered %s, want 400", got)
	}
}

// The handler picks the principal by what the connection says and nothing
// else, and a connection that says nothing is the watcher's: a mistake in
// placing a caller can only ever take a change away.
func TestCtlHandlerChoosesByTheConnection(t *testing.T) {
	srv, err := api.New(api.Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	h := ctlHandler(srv)
	post := func(ctx context.Context) int {
		r := httptest.NewRequest("POST", "/v1/terminals", strings.NewReader("{")).WithContext(ctx)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	may := func(v bool) context.Context {
		return context.WithValue(context.Background(), ctlMayKey{}, func() bool { return v })
	}
	for _, c := range []struct {
		what string
		ctx  context.Context
		want int
	}{
		{"a connection that says nothing", context.Background(), 403},
		{"a caller that may not", may(false), 403},
		{"a caller that may", may(true), 400},
	} {
		if got := post(c.ctx); got != c.want {
			t.Errorf("%s is answered %d, want %d", c.what, got, c.want)
		}
	}
}

// A file left at ctl.sock by a daemon that crashed — a plain file, or a
// socket with nobody behind it — does not keep the next daemon from
// listening there.
func TestAStaleCtlSockIsReplaced(t *testing.T) {
	srv, terminals := ctlDaemon(t)
	sock := holdwire.CtlSock(terminals)
	if err := os.WriteFile(sock, []byte("left by a crash"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop, err := listenCtl(srv, terminals, quietLog())
	if err != nil {
		t.Fatalf("a file left at ctl.sock stopped the daemon listening: %v", err)
	}
	if code, doc := ctlGet(t, sock, "/v1/health"); code != 200 || doc["ok"] != true {
		t.Fatalf("GET /v1/health over the new ctl.sock answered %d %v", code, doc)
	}
	stop()

	dead, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	dead.SetUnlinkOnClose(false)
	dead.Close()
	stop, err = listenCtl(srv, terminals, quietLog())
	if err != nil {
		t.Fatalf("a dead socket left at ctl.sock stopped the daemon listening: %v", err)
	}
	t.Cleanup(stop)
	if code, _ := ctlGet(t, sock, "/v1/health"); code != 200 {
		t.Fatalf("GET /v1/health over the new ctl.sock answered %d", code)
	}
}

// Only the daemon listens at ctl.sock, and only the daemon removes what is
// there: a server without the store's daemon claim is refused, and the file
// is left as it was — it may be a live daemon's.
func TestCtlSockIsTheDaemonsAlone(t *testing.T) {
	home, terminals := ctlStore(t)
	srv, err := api.New(api.Options{Token: "x", Dir: home, RefreshEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	sock := holdwire.CtlSock(terminals)
	if err := os.WriteFile(sock, []byte("a live daemon's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stop, err := listenCtl(srv, terminals, quietLog()); err == nil {
		stop()
		t.Fatal("a server without the daemon claim listened at ctl.sock")
	} else if want := "does not hold " + filepath.Join(terminals, "daemon.lock"); !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal is %q; want it to say it %s", err, want)
	}
	if raw, _ := os.ReadFile(sock); string(raw) != "a live daemon's" {
		t.Fatalf("the refused listen touched ctl.sock: %q", raw)
	}
}
