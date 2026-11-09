package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What --detach refuses, it refuses before anything is started: a prompt,
// which a session held for later has nobody to answer, -p among the CLI's
// arguments, which would have it answer one and exit, and --share, which is
// this terminal rather than one the daemon holds. Each is a usage error, and
// the store is left without a daemon.
func TestDetachRefusesAPromptAndPrint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	const (
		prompt = "--detach hands the CLI its terminal, so it takes no prompt or -p: give the CLI's arguments after --"
		share  = "--share and --detach are two different terminals; choose one"
	)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "1", "--detach", "fix it"}, prompt},
		{[]string{"run", "1", "--detach", "-p", "hi"}, prompt},
		{[]string{"run", "1", "--detach", "--", "-p", "hi"}, prompt},
		{[]string{"run", "1", "--detach", "-i", "--print", "hi"}, prompt},
		{[]string{"run", "1", "--detach", "fix", "--", "-i"}, prompt},
		{[]string{"run", "--detach", "--remote-control", "fix it"}, prompt},
		{[]string{"run", "1", "--detach", "--share"}, share},
		{[]string{"run", "--share=watch", "--detach", "-i"}, share},
		{[]string{"run", "1", "--detach=yes"}, "--detach takes no value"},
	} {
		out, errb, code := call(t, c.args...)
		if code != 2 || out != "" || !strings.Contains(errb, "error: "+c.want+"\n") {
			t.Errorf("rota %s: exit %d %q %q, want 2 and %q", strings.Join(c.args, " "), code, out, errb, c.want)
		}
	}
	_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "daemon.pid" {
			t.Errorf("a refused --detach left %s", path)
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(home, "daemon.log")); err == nil {
		t.Error("a refused --detach started a daemon, which wrote its log")
	}
}
