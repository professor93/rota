//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// A daemon asked to stop stops its sweeps at once, as a foreground serve does,
// while it still waits for what its listeners are serving. Here the sweeps run
// every 100 ms over an account with nothing to refresh, a run is held in flight
// on the control socket, and the daemon is asked to stop. Once the socket has
// closed, which comes after the sweeps are told to stop, the account's token is
// made to have run out on disk: a sweep still running would present it at once,
// and none does for as long as the daemon waits for the run.
func TestADaemonStartsNoSweepOnceAskedToStop(t *testing.T) {
	shortHome(t)
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
	sock := holdwire.CtlSock(terminals)
	if err := holdwire.SockFits(sock); err != nil {
		t.Skipf("no daemon fits here: %v", err)
	}
	p := newHeldProvider(t)
	runnable := &rota.Account{ID: 2, Provider: "claude", Email: "run@x", Order: 2, Token: rota.Token{Access: "t"}}
	writeAccounts(t, home, claudeExpiring(t, home, 1, "Ra", time.Now().Add(time.Hour).UnixMilli()), runnable)
	if err := os.WriteFile(filepath.Join(home, "server.toml"), []byte("[runs]\nrefresh_every = \"100ms\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, up := t.TempDir(), filepath.Join(t.TempDir(), "up")
	fakecli.Install(t, bin, "claude", fakecli.Spec{Stdout: []string{`[{"type":"result","result":"ANSWERED"}]`}, Sleep: "3s", Ready: up})
	t.Setenv("PATH", bin)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	was := serveUntil
	serveUntil = func() (context.Context, context.CancelFunc) { return ctx, stop }
	defer func() { serveUntil = was }()
	var out, errb lockedBuffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"serve", "start", "--foreground"}, &out, &errb) }()

	local := &http.Client{Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	ask := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, "http://rota"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := local.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if code, _ := ask("GET", "/v1/health", ""); code == http.StatusOK {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("the daemon ended at once: %d %q", code, errb.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never answered: %q", errb.String())
		}
	}
	// A run in flight, which the daemon's stop waits for.
	go ask("POST", "/v1/accounts/2/run", `{"prompt":"p"}`)
	fakecli.WaitReady(t, up)
	stop() // what SIGTERM does
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if code, _ := ask("GET", "/v1/health", ""); code == 0 {
			break // the socket is closed: the daemon's stop has begun, after StopRefreshing
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon kept answering after it was asked to stop: %q", errb.String())
		}
	}
	writeAccounts(t, home, expiredClaude(t, home, 1, "Ra"), runnable)
	select {
	case rt := <-p.arrived:
		t.Fatalf("a sweep presented %s while the daemon waited for a run to stop", rt)
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon did not stop")
	}
	if got := p.tokens(); len(got) != 0 {
		t.Fatalf("a sweep presented %v while the daemon waited for a run to stop", got)
	}
	holdsLogin(t, home, 1, "Ra")
}
