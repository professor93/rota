//go:build unix

package notify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// A program killed at the end of its attempt takes its whole process group
// with it: a helper it started is gone too, and nothing of the group is
// left behind the daemon.
func TestACommandsWholeGroupIsKilled(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "cmd.pid")
	helperReady := filepath.Join(dir, "child.pid")
	helper := fakecli.Install(t, dir, "rota-cmd-child", fakecli.Spec{Sleep: "20s", Ready: helperReady})
	cmdFake(t, "rota-cmd-group", fakecli.Spec{Spawn: helper, Ready: ready, Sleep: "20s"})
	snd := cmdSink(t, nil, "rota-cmd-group")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type up struct{ leader, child, group int }
	pids := make(chan up, 1)
	go func() {
		// Up, and its helper too, before its time ends, so the kill has a
		// group to reach.
		defer cancel()
		var u up
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if u.leader == 0 {
				u.leader = cmdPid(ready)
			}
			if u.child == 0 {
				u.child = cmdPid(helperReady)
			}
			if u.leader != 0 && u.child != 0 {
				u.group, _ = syscall.Getpgid(u.child)
				break
			}
		}
		pids <- u
		time.Sleep(200 * time.Millisecond)
	}()
	err := snd.send(ctx, cmdEvent())
	var d *drop
	if !errors.As(err, &d) {
		t.Fatalf("got %#v", err)
	}
	u := <-pids
	if u.leader == 0 || u.child == 0 {
		t.Fatalf("the program and its helper never said they were up: %+v", u)
	}
	if u.group != u.leader {
		t.Fatalf("the helper's group is %d, not the program's own %d", u.group, u.leader)
	}
	// The helper is reaped by whoever inherited it, a moment later; under a
	// container's first process nothing reaps it, and a zombie is gone too.
	deadline := time.Now().Add(3 * time.Second)
	for {
		gerr, cerr := syscall.Kill(-u.leader, 0), syscall.Kill(u.child, 0)
		if cmdGroupGone(u.leader) && cmdGone(u.child) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("kill(-%d, 0) = %v and kill(%d, 0) = %v: something of the group is left", u.leader, gerr, u.child, cerr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cmdPid is the pid a fake wrote to ready, or 0 before it has.
func cmdPid(ready string) int {
	raw, err := os.ReadFile(ready)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return p
}

// cmdGone says pid has died: it is not there, or it is a zombie nobody has
// reaped, as an orphan stays under a container's first process.
func cmdGone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	state, _, ok := cmdStat(pid)
	return ok && state == 'Z'
}

// cmdGroupGone says nothing that can act is left in group: no member at
// all, or (where /proc tells) only zombies.
func cmdGroupGone(group int) bool {
	if errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
		return true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	zombies := false
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		state, g, ok := cmdStat(pid)
		switch {
		case !ok || g != group:
		case state != 'Z':
			return false
		default:
			zombies = true
		}
	}
	return zombies
}

// cmdStat is pid's state and process group from /proc/<pid>/stat, counted
// from the last parenthesis.
func cmdStat(pid int) (state byte, group int, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := bytes.Fields(raw[i+1:])
	if len(f) < 3 || len(f[0]) != 1 {
		return 0, 0, false
	}
	g, err := strconv.Atoi(string(f[2]))
	return f[0][0], g, err == nil
}

// killGroup on a program that has ended by itself says so
// (os.ErrProcessDone) rather than claiming a kill: once reaped (ESRCH),
// and on macOS while still a zombie (EPERM). Linux signals a zombie and
// says nil, so that half is macOS's.
func TestKillingAnEndedGroupSaysItHadEnded(t *testing.T) {
	path := fakecli.Install(t, t.TempDir(), "rota-cmd-ended", fakecli.Spec{})
	reaped := exec.Command(path)
	ownGroup(reaped)
	if err := reaped.Run(); err != nil {
		t.Fatal(err)
	}
	if err := killGroup(reaped); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("reaped: %v", err)
	}
	if runtime.GOOS != "darwin" {
		return
	}
	zombie := exec.Command(path)
	ownGroup(zombie)
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	defer zombie.Wait()
	for end := time.Now().Add(10 * time.Second); syscall.Kill(-zombie.Process.Pid, 0) == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the program never ended")
		}
	}
	if err := killGroup(zombie); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("zombie: %v", err)
	}
}

// A program that can never start, such as a script without its #! line
// (exec format error), is dropped at once rather than tried again.
func TestAProgramThatCannotRunIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sh")
	if err := os.WriteFile(path, []byte("echo hi\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := cmdSend(t, cmdSink(t, nil, path), cmdEvent())
	var d *drop
	if !errors.As(err, &d) || !strings.HasPrefix(err.Error(), "command could not start") || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("got %#v", err)
	}
}
