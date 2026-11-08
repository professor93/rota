package sessions

import (
	"errors"
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

// listProcs and cwdsOf are variables so tests can stand in for the machine.
var (
	listProcs = psList
	cwdsOf    = processCwds
)

// cwd is where one process is working, or why that could not be read.
type cwd struct {
	dir string
	err error
}

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
	var out []Instance
	for _, p := range list {
		provider := cliFor(p.command)
		if provider == "" || slices.Contains(known, p.pid) {
			continue
		}
		out = append(out, Instance{Kind: "cli", Provider: provider, PID: p.pid})
	}
	if len(out) == 0 {
		return nil, ""
	}
	// Every directory is asked for at once: on macOS each question is an lsof,
	// and a busy machine pays for every one of them.
	pids := make([]int, len(out))
	for i, in := range out {
		pids[i] = in.PID
	}
	var (
		blind   int
		blindBy string
	)
	for i, c := range cwdsOf(pids) {
		if c.err != nil {
			blind++
			blindBy = c.err.Error()
			continue
		}
		out[i].Dir = c.dir
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
	// A path, on either convention: ps on unix prints one, and so does
	// the Windows process table, and a test may feed either to either.
	if !strings.ContainsAny(argv0, `/\`) {
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
	if runtime.GOOS == "windows" {
		return windowsList()
	}
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	return parsePS(string(out)), nil
}

// windowsList reads the process table the way Windows exposes it: the
// executable's full path per process, which is what cliFor needs. There
// is no ps; PowerShell is on every supported Windows.
func windowsList() ([]proc, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-CimInstance Win32_Process | ForEach-Object { "$($_.ProcessId)|$($_.ExecutablePath)" }`).Output()
	if err != nil {
		return nil, err
	}
	return parseWindowsList(string(out)), nil
}

// parseWindowsList turns "pid|path" lines into procs. A path may hold
// spaces, so the separator is one that cannot appear in it.
func parseWindowsList(out string) []proc {
	var list []proc
	for line := range strings.SplitSeq(out, "\n") {
		id, path, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(id))
		if err != nil || strings.TrimSpace(path) == "" {
			continue
		}
		list = append(list, proc{pid: pid, command: strings.TrimSpace(path)})
	}
	return list
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

// processCwds is where each process is working, in the order asked.
//
// Linux keeps it in /proc, which costs a readlink. macOS has no such file and
// no way to ask without cgo, so it takes lsof — which is present on a stock
// install but is still another program, and one rota can do without. It is
// one lsof for all of them rather than one each, because starting lsof is
// what costs: tens of milliseconds a time on a loaded machine.
func processCwds(pids []int) []cwd {
	out := make([]cwd, len(pids))
	switch runtime.GOOS {
	case "windows":
		// Another process's working directory is not readable without
		// native calls rota does not make; the row is still listed, blind.
		for i := range out {
			out[i].err = errors.New("the working directory of another process is not readable on windows")
		}
	case "linux":
		for i, pid := range pids {
			out[i].dir, out[i].err = os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
		}
	default:
		lsofCwds(pids, out, runLsof)
	}
	return out
}

// lsofChunk bounds how many pids go into one lsof's argument, which stays a
// few kilobytes however many CLIs are open.
const lsofChunk = 256

// runLsof asks lsof for the working directory of every pid in the
// comma-separated list, one record per process it could read.
func runLsof(list string) ([]byte, error) {
	return exec.Command("lsof", "-a", "-p", list, "-d", "cwd", "-Fpn").Output()
}

// lsofCwds fills out with where each pid is working, asking lsof a chunk at a
// time.
//
// lsof leaves out a process that has gone or that belongs to someone it may
// not read, and then exits 1 while still printing every other one. So each
// pid is judged by its own record, and gets what one lsof for it alone would
// have said: the directory its record names; "not found" for a record that
// names none, as that lsof would have succeeded with no name; and for no
// record at all, the error lsof ended with, since that lsof would have failed.
func lsofCwds(pids []int, out []cwd, lsof func(list string) ([]byte, error)) {
	for start := 0; start < len(pids); start += lsofChunk {
		chunk := pids[start:min(start+lsofChunk, len(pids))]
		ids := make([]string, len(chunk))
		for i, pid := range chunk {
			ids[i] = strconv.Itoa(pid)
		}
		raw, err := lsof(strings.Join(ids, ","))
		if err == nil {
			err = os.ErrNotExist
		}
		dirs := parseLsofCwds(string(raw))
		for i, pid := range chunk {
			dir, listed := dirs[pid]
			switch {
			case dir != "":
				out[start+i] = cwd{dir: dir}
			case listed:
				out[start+i] = cwd{err: os.ErrNotExist}
			default:
				out[start+i] = cwd{err: err}
			}
		}
	}
}

// parseLsofCwds reads what lsof -Fpn prints: one field per line, tagged by its
// first character, where "p" opens a process and the "n" under it names its
// working directory. lsof escapes a newline or a tab in a name, so a line is
// never part of a path; the name is kept exactly as printed. Every process
// printed has an entry, "" when no absolute path was named for it.
func parseLsofCwds(out string) map[int]string {
	dirs := map[int]string{}
	pid := 0
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
			if pid > 0 {
				if _, seen := dirs[pid]; !seen {
					dirs[pid] = ""
				}
			}
		case strings.HasPrefix(line, "n/") && pid > 0 && dirs[pid] == "":
			dirs[pid] = line[1:]
		}
	}
	return dirs
}

func plural(n int) string {
	if n == 1 {
		return "one"
	}
	return strconv.Itoa(n)
}
