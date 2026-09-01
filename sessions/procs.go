package sessions

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// proc is one line of the process list: what is running, and under which id.
type proc struct {
	pid     int
	command string
}

// cliNames are the vendor executables rota knows how to recognise. They are
// the flavours lib speaks, which is the same list by construction.
var cliNames = []string{"claude", "codex", "grok", "kimi"}

// listProcs and cwdOf are variables so tests can stand in for the machine.
var (
	listProcs = psList
	cwdOf     = processCwd
)

// ProcessInstances reports vendor CLIs running that rota did not start,
// skipping the pids given because those are the registry's to describe.
//
// The second result explains a gap rather than leaving one: where a process
// is working takes /proc on Linux and lsof on macOS, and a machine without
// lsof can see that a CLI is running but not where. Saying so is better than
// dropping the row, and much better than guessing a directory.
func ProcessInstances(known []int) ([]Instance, string) {
	list, err := listProcs()
	if err != nil {
		return nil, "could not read the process list: " + err.Error()
	}
	var (
		out     []Instance
		blind   int
		blindBy string
	)
	for _, p := range list {
		provider := cliFor(p.command)
		if provider == "" || slices.Contains(known, p.pid) {
			continue
		}
		in := Instance{Kind: "cli", Provider: provider, PID: p.pid}
		dir, err := cwdOf(p.pid)
		switch {
		case err == nil:
			in.Dir = dir
		default:
			blind++
			blindBy = err.Error()
		}
		out = append(out, in)
	}
	if blind > 0 {
		where := "lsof"
		if runtime.GOOS == "linux" {
			where = "/proc"
		}
		return out, plural(blind) + " directory could not be read (" + where + ": " + blindBy + ")"
	}
	return out, ""
}

// cliFor names the provider a command belongs to, or "" for anything else.
//
// argv[0] has to be a path to the executable. A CLI runs helpers that rename
// their process, and Claude Code has several: "claude bg-pty-host --bg-pty-host
// …", "claude bg-spare …". Their argv[0] is the bare word "claude", so a name
// match alone reports one open project half a dozen times over. What a renamed
// process cannot fake is a path, so a separator is what is required.
//
// The cost is a CLI exec'd as a bare name with no path, which is missed. That
// is the right way round: a missing row is a smaller lie than six invented
// ones, and every ps this was checked against prints the resolved path.
func cliFor(argv0 string) string {
	argv0 = strings.TrimSpace(argv0)
	if !strings.ContainsRune(argv0, filepath.Separator) {
		return ""
	}
	// Windows executables carry .exe; the provider name does not.
	name := strings.TrimSuffix(filepath.Base(argv0), ".exe")
	if slices.Contains(cliNames, name) {
		return name
	}
	return ""
}

// psList reads the process table. There is no way to do this from the standard
// library, and ps is the one tool present on every unix rota runs on.
func psList() ([]proc, error) {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	return parsePS(string(out)), nil
}

// parsePS turns what ps prints into pids and their argv[0]. It is separate
// from the call so it can be tested against real output, which is where the
// surprises are.
func parsePS(out string) []proc {
	var list []proc
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(id)
		if err != nil {
			continue
		}
		// Only argv[0] names the executable; what follows are its arguments.
		argv0, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
		list = append(list, proc{pid: pid, command: argv0})
	}
	return list
}

// processCwd is where a process is working.
//
// Linux keeps it in /proc, which costs a readlink. macOS has no such file and
// no way to ask without cgo, so it takes lsof — which is present on a stock
// install but is still another program, and one rota can do without.
func processCwd(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		return os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	}
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", err
	}
	// -Fn prints one field per line, each tagged by its first character; the
	// name is the one beginning with "n".
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n"), nil
		}
	}
	return "", os.ErrNotExist
}

func plural(n int) string {
	if n == 1 {
		return "one"
	}
	return strconv.Itoa(n)
}
