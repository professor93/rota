// Package fakecli makes a test binary stand in for a vendor CLI.
//
// Tests need a claude, codex or grok on PATH that answers the way the real
// one would, without a network or a real CLI. A shell script did that on
// unix and nowhere else. Here the test binary itself is installed under the
// vendor's name, next to a small spec saying what to print and how to exit,
// and TestMain hands the process to Maybe, which plays the part when it sees
// the spec. One mechanism, every platform, no shell.
package fakecli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Spec is what the fake does when run. Lines may carry {{stdin}} (what was
// read, trailing newlines trimmed, the way $(cat) trims), {{args}} (the
// arguments joined by spaces, like $*), {{env:NAME}} and {{cwd}} (the
// working directory with symlinks resolved, like pwd -P).
type Spec struct {
	Stdout []string `json:"stdout,omitempty"`
	Stderr []string `json:"stderr,omitempty"`
	Exit   int      `json:"exit,omitempty"`
	// Sleep is a duration to wait before writing anything, for tests that
	// kill a run or bound its concurrency.
	Sleep string `json:"sleep,omitempty"`
	// Touch names a file to create before exiting, so a test can prove the
	// fake ran — or did not.
	Touch string `json:"touch,omitempty"`
	// KeepStdin leaves stdin unread. The default reads it to the end, as
	// every real CLI does with a piped prompt.
	KeepStdin bool `json:"keep_stdin,omitempty"`
}

// Result is a Spec that answers as the claude CLI does in print mode: one
// terminal result event carrying the prompt and the argv, "fake-stderr" on
// stderr, and the exit code given.
func Result(exit int) Spec {
	return Spec{
		Stdout: []string{`{"type":"result","subtype":"success","is_error":false,"session_id":"s-fake","result":"STDIN={{stdin}} ARGS={{args}}","num_turns":1,"total_cost_usd":0.5}`},
		Stderr: []string{"fake-stderr"},
		Exit:   exit,
	}
}

// Lines is a Spec that prints exactly these lines and exits 0.
func Lines(lines ...string) Spec { return Spec{Stdout: lines} }

// Exe is name as an executable file is called on this platform.
func Exe(name string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") {
		return name + ".exe"
	}
	return name
}

// Install puts a fake called name into dir and returns its path. PATH is
// the caller's to set: a test that hands rota a BaseEnv needs dir there too.
func Install(t testing.TB, dir, name string, spec Spec) string {
	t.Helper()
	path := filepath.Join(dir, Exe(name))
	if err := Link(path); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specPath(path), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Link makes path another name for this test binary: a hard link where the
// filesystem allows, a copy otherwise. A symlink would need a privilege on
// Windows that a test runner does not have.
func Link(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Remove(path)
	if err := os.Link(self, path); err == nil {
		return nil
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

func specPath(exe string) string { return exe + ".spec.json" }

// Maybe plays the fake when this process is one — a spec sits beside the
// executable — and never returns in that case. TestMain calls it first;
// a real test binary has no spec and carries on.
func Maybe() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	raw, err := os.ReadFile(specPath(exe))
	if err != nil {
		return
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "fake cli:", err)
		os.Exit(70)
	}
	os.Exit(run(spec, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(spec Spec, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	in := ""
	if !spec.KeepStdin {
		raw, _ := io.ReadAll(stdin)
		in = strings.TrimRight(string(raw), "\n")
	}
	if spec.Sleep != "" {
		if d, err := time.ParseDuration(spec.Sleep); err == nil {
			time.Sleep(d)
		}
	}
	cwd, _ := os.Getwd()
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	expand := func(line string) string {
		line = strings.ReplaceAll(line, "{{stdin}}", in)
		line = strings.ReplaceAll(line, "{{args}}", strings.Join(args, " "))
		line = strings.ReplaceAll(line, "{{cwd}}", cwd)
		for {
			start := strings.Index(line, "{{env:")
			if start < 0 {
				return line
			}
			end := strings.Index(line[start:], "}}")
			if end < 0 {
				return line
			}
			name := line[start+len("{{env:") : start+end]
			// {{env:NAME|fallback}} reads like ${NAME:-fallback}.
			fallback := ""
			if n, f, ok := strings.Cut(name, "|"); ok {
				name, fallback = n, f
			}
			value, set := os.LookupEnv(name)
			if !set || value == "" {
				value = fallback
			}
			line = line[:start] + value + line[start+end+2:]
		}
	}
	for _, l := range spec.Stdout {
		fmt.Fprintln(stdout, expand(l))
	}
	for _, l := range spec.Stderr {
		fmt.Fprintln(stderr, expand(l))
	}
	if spec.Touch != "" {
		_ = os.WriteFile(spec.Touch, nil, 0o600)
	}
	return spec.Exit
}
