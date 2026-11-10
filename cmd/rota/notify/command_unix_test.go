//go:build unix

package notify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	// The helper is reaped by whoever inherited it, a moment later.
	deadline := time.Now().Add(3 * time.Second)
	for {
		gerr, cerr := syscall.Kill(-u.leader, 0), syscall.Kill(u.child, 0)
		if errors.Is(gerr, syscall.ESRCH) && errors.Is(cerr, syscall.ESRCH) {
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
