package fakecli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// notAFake writes an executable called name into dir that is neither this
// test binary nor any program at all: it stands for a vendor's real CLI,
// which is only ever looked for here, never run.
func notAFake(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("not a program; never run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// list is dirs as PATH lists them.
func list(dirs ...string) string { return strings.Join(dirs, string(os.PathListSeparator)) }

// TestThePathLeavesOutEveryDirectoryHoldingARealVendorCLI is what keeps a
// test that forgot its fake from starting the real CLI somebody has
// installed: every directory holding one is left out of PATH,
// and every other directory stays, in its place, so git and sh are still
// found.
func TestThePathLeavesOutEveryDirectoryHoldingARealVendorCLI(t *testing.T) {
	var names []string
	for _, name := range vendors {
		names = append(names, Exe(name))
	}
	if runtime.GOOS == "windows" {
		// What npm installs there, besides an exe of the vendor's own.
		names = append(names, "claude.cmd", "codex.bat")
	}
	var path, kept []string
	for _, name := range names {
		vendor, tools := t.TempDir(), t.TempDir()
		notAFake(t, vendor, name)
		notAFake(t, tools, Exe("git"))
		path, kept = append(path, vendor, tools), append(kept, tools)
	}
	// A directory that does not exist holds nothing, and stays too.
	gone := filepath.Join(t.TempDir(), "gone")
	path, kept = append(path, gone), append(kept, gone)
	t.Setenv("PATH", list(path...))

	scrubPath()

	for _, name := range vendors {
		if p, err := exec.LookPath(name); err == nil {
			t.Errorf("%s is still found, at %s", name, p)
		}
	}
	if got, want := os.Getenv("PATH"), list(kept...); got != want {
		t.Fatalf("PATH is\n%s\nwant\n%s", got, want)
	}
	if p, err := exec.LookPath("git"); err != nil || p != filepath.Join(kept[0], Exe("git")) {
		t.Fatalf("git is found at %q (%v), want the first directory kept", p, err)
	}
}

// TestThePathKeepsTheFakesOfTheTestThatStartedThisProcess: a process a test
// starts from this binary — a holder, a daemon, rota run as a child — runs
// TestMain too, with the test's fakes already on PATH, and they stay. A spec
// beside a CLI is not what makes it a fake: being this binary is.
func TestThePathKeepsTheFakesOfTheTestThatStartedThisProcess(t *testing.T) {
	fakes := t.TempDir()
	Install(t, fakes, "claude", Lines("fake"))
	Install(t, fakes, "codex", Lines("fake"))
	impostor := t.TempDir()
	real := notAFake(t, impostor, Exe("grok"))
	if err := os.WriteFile(specPath(real), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", list(impostor, fakes))

	scrubPath()

	if got := os.Getenv("PATH"); got != fakes {
		t.Fatalf("PATH is %q, want only the fakes' directory %q", got, fakes)
	}
	if p, err := exec.LookPath("claude"); err != nil || p != filepath.Join(fakes, Exe("claude")) {
		t.Fatalf("the fake claude is found at %q (%v)", p, err)
	}
}

// tally is a testing.TB that keeps every failure it is told of and carries
// on, so a test can watch NoneOnPath fail without failing itself.
type tally struct {
	testing.TB
	said []string
}

func (r *tally) Helper()                   {}
func (r *tally) Fatal(a ...any)            { r.said = append(r.said, fmt.Sprint(a...)) }
func (r *tally) Errorf(f string, a ...any) { r.said = append(r.said, fmt.Sprintf(f, a...)) }

// NoneOnPath, which each package's guard test calls, fails for a vendor CLI
// left on PATH, and for a TestMain that never called Maybe even where no CLI
// is installed to give that away.
func TestNoneOnPathFailsForARealCLIAndForATestMainWithoutMaybe(t *testing.T) {
	dir := t.TempDir()
	real := notAFake(t, dir, Exe("codex"))
	t.Setenv("PATH", dir)
	r := &tally{TB: t}
	NoneOnPath(r)
	if len(r.said) != 1 || !strings.Contains(r.said[0], real) {
		t.Fatalf("a real codex on PATH fails, by path: %q", r.said)
	}

	t.Setenv("PATH", t.TempDir())
	was := scrubbed
	t.Cleanup(func() { scrubbed = was })
	scrubbed = false
	r = &tally{TB: t}
	NoneOnPath(r)
	if len(r.said) != 1 || !strings.Contains(r.said[0], "fakecli.Maybe") {
		t.Fatalf("a TestMain that never called Maybe fails, naming it: %q", r.said)
	}
}

// This package's own guard: its TestMain calls Maybe like every other.
func TestNoVendorCLIIsOnPath(t *testing.T) { NoneOnPath(t) }
