//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/holdwire"
)

// A daemon a supervisor starts while another already serves the store has
// not failed: the store has its one daemon. It says so in its log and exits
// 0, so that neither systemd's Restart=on-failure nor launchd's KeepAlive
// starts it again every few seconds for as long as the other runs.
func TestAForegroundDaemonThatFindsAnotherExitsZero(t *testing.T) {
	r := newDaemonRig(t)
	release, err := holdwire.ClaimDaemon(r.terminals, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	out, errb, code := r.rota("serve", "start", "--foreground")
	if code != 0 || out != "" || errb != "" {
		t.Fatalf("serve start --foreground beside another daemon: exit %d %q %q\nlog:\n%s", code, out, errb, r.log())
	}
	said := regexp.MustCompile(`level=INFO msg="another rota daemon serves this store, so this one exits 0 rather than fail" pid=\d+ daemon=` +
		fmt.Sprint(os.Getpid()) + `\n`)
	if !said.MatchString(r.log()) || strings.Contains(r.log(), "did not start") {
		t.Fatalf("the log does not say why it exited:\n%s", r.log())
	}
	if d, ok := holdwire.RunningDaemon(r.terminals); !ok || d.PID != os.Getpid() {
		t.Fatalf("the daemon serving is %+v, not the one that was", d)
	}
}

// A daemon's control socket serves as the principal local, so no user or
// token in its file may have that name: serve start refuses the file at this
// terminal, naming the entry, and nothing is started.
func TestADaemonRefusesAPrincipalCalledLocal(t *testing.T) {
	r := newDaemonRig(t)
	if err := os.WriteFile(filepath.Join(r.home, "server.toml"), []byte(localToken), 0o600); err != nil {
		t.Fatal(err)
	}
	want := `[[tokens]] "local": the name "local" is reserved for the control socket's own principal; choose another`
	if out, errb, code := r.rota("serve", "start"); code != 1 || out != "" || errb != "error: "+want+"\n" {
		t.Fatalf("serve start with a token called local: exit %d %q %q, want %q", code, out, errb, want)
	}
	if _, ok := holdwire.RunningDaemon(r.terminals); ok {
		t.Fatal("a daemon serves with a token called local")
	}
}
