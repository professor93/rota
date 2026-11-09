//go:build linux || darwin

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
	"github.com/professor93/rota/wire"
)

// A process an agent's command starts is inside its held session, though
// it is in a session of its own: Claude Code runs each command its Bash tool
// is given with setsid, so the session never says, and the process's
// parents do. These tests make that shape: a process a record names as its
// CLI, and under it a probe in a session of its own.

// The CLI stand-in is this test binary run with ROTA_SETSID_CHILD set to
// one NAME=value: it starts this binary again, in a session of its own, with
// that variable added and its own taken out, hands it its standard input and
// output, and waits for it. The variable it adds is in the child's
// environment alone, so the stand-in itself is no probe.
func init() {
	child, ok := os.LookupEnv("ROTA_SETSID_CHILD")
	if !ok {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, "ROTA_SETSID_CHILD=")
	}), child)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// underSetsid starts the CLI stand-in with env, its probe made by child,
// and returns the stand-in's pid and a way to give the probe one line and
// read the one it answers. Both are stopped when the test ends.
func underSetsid(t *testing.T, env []string, child string) (cli int, ask func(line string) string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(slices.Clone(env), "ROTA_SETSID_CHILD="+child)
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
	// Closing its input ends the probe, and with it the stand-in.
	t.Cleanup(func() {
		in.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	answers := bufio.NewScanner(out)
	return cmd.Process.Pid, func(line string) string {
		t.Helper()
		fmt.Fprintln(in, line)
		if !answers.Scan() {
			t.Fatalf("the probe said nothing to %q: %v", line, answers.Err())
		}
		return answers.Text()
	}
}

// heldRecord writes a running session's record naming cli as its CLI, and
// holds its lock until release or the end of the test.
func heldRecord(t *testing.T, terminals, id string, cli int) (release func()) {
	t.Helper()
	dir := holdwire.Dir(terminals, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &holdwire.Meta{ID: id, Kind: holdwire.KindAccount, Account: 1, Provider: "claude",
		ChildPID: cli, Protocol: holdwire.Major, Version: wire.Version, Started: time.Now()}
	if err := holdwire.WriteMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	release, err := holdwire.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return release
}

// On ctl.sock the probe a held CLI started in a session of its own may only
// watch: 403 where it was 400 before the CLI was recorded, and 400 again once
// the session is over.
func TestASetsidChildOfAHeldCLIOnlyWatches(t *testing.T) {
	srv, terminals := ctlDaemon(t)
	stop, err := listenCtl(srv, terminals, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	cli, ask := underSetsid(t, append(os.Environ(), "ROTA_CTL_SOCK="+holdwire.CtlSock(terminals)), "ROTA_CTL_PROBE=1")
	if got := ask("go"); got != "400" {
		t.Fatalf("the probe outside every held session was answered %s, want the route's own 400", got)
	}
	release := heldRecord(t, terminals, "00000000000000c8", cli)
	if got := ask("go"); got != "403" {
		t.Fatalf("the probe in a session of its own under a held CLI was answered %s, want 403", got)
	}
	release()
	if got := ask("go"); got != "400" {
		t.Fatalf("the probe once the session was over was answered %s, want 400", got)
	}
}

// rota serve stop, install and uninstall run by a held CLI's command in a
// session of its own are each refused as inside that held session, and the
// supervisor is asked nothing; once the session is over, the same process
// may.
func TestASetsidChildOfAHeldCLIIsRefusedTheDaemon(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	record := filepath.Join(t.TempDir(), "calls")
	cli, line := underSetsid(t, append(os.Environ(), "ROTA_UNIT_RECORD="+record), "ROTA_SESSION_PROBE=1")
	ask := func(cmd string) (int, string) {
		t.Helper()
		got := line(cmd)
		code, said, _ := strings.Cut(got, " ")
		n, err := strconv.Atoi(code)
		if err != nil {
			t.Fatalf("the probe answered %q", got)
		}
		s, err := strconv.Unquote(said)
		if err != nil {
			t.Fatalf("the probe answered %q", got)
		}
		return n, s
	}

	const id = "00000000000000e2"
	release := heldRecord(t, r.terminals, id, cli)
	for _, verb := range []string{"stop", "stop --all --yes", "install", "uninstall"} {
		code, said := ask("serve " + verb)
		command, _, _ := strings.Cut(verb, " ")
		if want := "rota serve " + command + " is refused inside held session " + id + "; run it from outside the session"; code != 1 || !strings.Contains(said, want) {
			t.Fatalf("serve %s under a held CLI: exit %d %q, want %q", verb, code, said, want)
		}
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the supervisor was asked from inside a held session: %v", err)
	}
	if _, err := os.Lstat(r.want.path); err != nil {
		t.Fatalf("the unit was taken away from inside a held session: %v", err)
	}

	release()
	if code, said := ask("serve uninstall"); code != 0 || said != fmt.Sprintf("rota daemon uninstalled for %s; 0 sessions keep running\n", r.store) {
		t.Fatalf("uninstall once the session was over: exit %d %q", code, said)
	}
}

// The walk up a caller's parents ends at pid 1, which is outside every held
// session; it goes maxAncestry parents up at most, and a caller further
// down than that, or one whose own parent cannot be read, cannot be placed,
// which every caller of heldSessionOf takes as inside. Some session is held
// throughout, elsewhere, or the walk would not be taken at all.
func TestTheAncestryWalkIsBounded(t *testing.T) {
	terminals := t.TempDir()
	was := parentOf
	t.Cleanup(func() { parentOf = was })
	self := os.Getpid()
	asked := 0
	chain := func(next func(int) (int, error)) {
		asked = 0
		parentOf = func(pid int) (int, error) { asked++; return next(pid) }
	}
	heldRecord(t, terminals, "00000000000000e4", 2)

	chain(func(int) (int, error) { return 1, nil })
	if id, err := heldSessionOf(self, terminals); id != "" || err != nil || asked != 1 {
		t.Fatalf("a parent that is pid 1 gave %q, %v after %d parents", id, err, asked)
	}
	chain(func(pid int) (int, error) { return pid + 1, nil })
	if id, err := heldSessionOf(self, terminals); id != "" || err == nil || asked != maxAncestry {
		t.Fatalf("an endless chain gave %q, %v after %d parents, want an error after %d", id, err, asked, maxAncestry)
	}
	chain(func(int) (int, error) { return 0, os.ErrNotExist })
	if _, err := heldSessionOf(self, terminals); err == nil {
		t.Fatal("a caller whose own parent cannot be read was placed")
	}
	// And a held CLI that many parents up is still found.
	chain(func(pid int) (int, error) { return pid + 1, nil })
	heldRecord(t, terminals, "00000000000000e3", self+maxAncestry)
	if id, err := heldSessionOf(self, terminals); id != "00000000000000e3" || err != nil {
		t.Fatalf("a held CLI %d parents up gave %q, %v", maxAncestry, id, err)
	}
}

// hiddenProc is a Linux /proc mounted with hidepid: the caller, this test,
// can read its own stat, whose parent is ppid, and every process given in
// own; any other's stat is refused with hidden, as root's sshd above an ssh
// login is.
func hiddenProc(t *testing.T, ppid int, own map[int]int, hidden error) {
	t.Helper()
	was := parentOf
	t.Cleanup(func() { parentOf = was })
	stat := func(pid, ppid int) []byte {
		return fmt.Appendf(nil, "%d (sh) S %d %d %d 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 1234 1 2\n", pid, ppid, pid, pid)
	}
	kernel := proc.Procfs{Read: func(name string) ([]byte, error) {
		var pid int
		if _, err := fmt.Sscanf(name, "/proc/%d/stat", &pid); err != nil {
			return nil, os.ErrNotExist
		}
		if pid == os.Getpid() {
			return stat(pid, ppid), nil
		}
		if parent, ok := own[pid]; ok {
			return stat(pid, parent), nil
		}
		return nil, hidden
	}}
	parentOf = kernel.Parent
}

// On a /proc mounted with hidepid an ssh caller's walk meets root's sshd,
// whose stat it cannot read. That ancestor is another user's, and no
// holder or CLI, which are this user's own, is above it, so the caller is
// outside by ancestry rather than unplaceable: it may control and is not
// refused serve stop. A held CLI below that ancestor is still found, and a
// caller whose own stat cannot be read still cannot be placed. With no
// session held at all, nothing is walked.
func TestAnAncestorAnotherUserHidesEndsTheWalk(t *testing.T) {
	self := os.Getpid()
	const sshd, cli, elsewhere = 900001, 900003, 900002
	for name, hidden := range map[string]error{"hidepid=1": os.ErrPermission, "hidepid=2": os.ErrNotExist} {
		t.Run(name, func(t *testing.T) {
			terminals := t.TempDir()
			was := parentOf
			t.Cleanup(func() { parentOf = was })
			walked := false
			parentOf = func(int) (int, error) { walked = true; return 0, hidden }
			if id, err := heldSessionOf(self, terminals); id != "" || err != nil || walked {
				t.Fatalf("with nothing held: %q, %v, and walked %v", id, err, walked)
			}
			hiddenProc(t, sshd, nil, hidden)
			heldRecord(t, terminals, "00000000000000e5", elsewhere)
			if id, err := heldSessionOf(self, terminals); id != "" || err != nil {
				t.Fatalf("under an ancestor another user's, with a session held elsewhere: %q, %v; want outside", id, err)
			}
			hiddenProc(t, cli, map[int]int{cli: sshd}, hidden)
			heldRecord(t, terminals, "00000000000000e6", cli)
			if id, err := heldSessionOf(self, terminals); id != "00000000000000e6" || err != nil {
				t.Fatalf("under a held CLI below that ancestor: %q, %v", id, err)
			}
			parentOf = func(int) (int, error) { return 0, hidden }
			if _, err := heldSessionOf(self, terminals); err == nil {
				t.Fatal("a caller whose own stat cannot be read was placed")
			}
		})
	}
}
