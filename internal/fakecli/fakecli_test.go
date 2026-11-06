package fakecli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	Maybe()
	os.Exit(m.Run())
}

// A fake asked to warm up exits at once and does nothing it was installed
// to do: Link runs it that way, and nothing may come of that run.
func TestAFakeAskedToWarmUpDoesNothing(t *testing.T) {
	dir := t.TempDir()
	touched := filepath.Join(dir, "touched")
	path := Install(t, dir, "claude", Spec{Stdout: []string{"played"}, Touch: touched, Exit: 3})
	out, err := exec.Command(path, warmArg).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("a warm-up exits 0 and says nothing: %v %q", err, out)
	}
	if _, err := os.Stat(touched); err == nil {
		t.Fatal("a warm-up played the fake")
	}
	// Run for real, it plays its part.
	out, _ = exec.Command(path).Output()
	if string(out) != "played\n" {
		t.Fatalf("the fake said %q", out)
	}
}

// WaitReady returns once the fake is up, with the pid of the fake itself.
func TestWaitReadyGivesTheFakesPid(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	path := Install(t, dir, "claude", Spec{Ready: ready, Sleep: "30s", KeepStdin: true})
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if pid := WaitReady(t, ready); pid != cmd.Process.Pid {
		t.Fatalf("WaitReady says %d, the fake is %d", pid, cmd.Process.Pid)
	}
}

// TestTheTtyFakePrintsAnyBytesAndShowsControlBytesItReads drives the tty
// fake the way a capture does: a print line writes exactly its bytes, with no
// newline, and a line with control bytes in it comes back with each one
// spelled out, so a test can see what arrived without the terminal acting on
// it.
func TestTheTtyFakePrintsAnyBytesAndShowsControlBytesItReads(t *testing.T) {
	in := strings.Join([]string{
		"print 1b5b3f3130343968",
		"print e4b896",
		"print zz",
		"a\x1bb\x7fc\x01",
		"plain words",
		"bye",
	}, "\n") + "\n"
	var out bytes.Buffer
	if code := atty(Spec{Tty: true}, strings.NewReader(in), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	_, got, ok := strings.Cut(out.String(), "\r\n")
	if !ok {
		t.Fatalf("no ready line: %q", out.String())
	}
	want := "\x1b[?1049h" + "世" +
		"bad print: zz\r\n" +
		`got:a\x1bb\x7fc\x01` + "\r\n" +
		"got:plain words\r\n" +
		"bye\r\n"
	if got != want {
		t.Fatalf("the fake wrote\n%q\nwant\n%q", got, want)
	}
}
