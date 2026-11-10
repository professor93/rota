package fakecli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/professor93/rota/internal/proc"
)

// ask answers the tty fake's lines about where it runs, and says whether the
// line was one of them:
//
//   - "env NAME" prints "env NAME=<value>", or "env NAME unset";
//   - "envhas <text>" prints "envhas yes" when any variable's name or value
//     contains the text, and "envhas no" otherwise;
//   - "registry <status> [waiting]" writes the file Claude Code keeps for a
//     running session, $CLAUDE_CONFIG_DIR/sessions/<pid>.json, and prints
//     "registry <status>".
//
// A test reads what a program at a terminal was really given this way, from
// the program itself, rather than trusting what its parent meant to give it.
func ask(line string) (string, bool) {
	if name, ok := strings.CutPrefix(line, "env "); ok {
		if v, set := os.LookupEnv(name); set {
			return "env " + name + "=" + v, true
		}
		return "env " + name + " unset", true
	}
	if text, ok := strings.CutPrefix(line, "envhas "); ok {
		for _, e := range os.Environ() {
			if strings.Contains(e, text) {
				return "envhas yes", true
			}
		}
		return "envhas no", true
	}
	if rest, ok := strings.CutPrefix(line, "registry "); ok {
		words := strings.Fields(rest)
		if len(words) == 0 {
			return "bad registry: " + rest, true
		}
		if err := writeRegistry(words[0], words[1:]); err != nil {
			return "bad registry: " + err.Error(), true
		}
		return "registry " + words[0], true
	}
	return "", false
}

// writeRegistry writes a session file shaped like one Claude Code writes,
// with the keys of a 2.1.318 file and pidDomain, about this process: its
// own pid, its start and the pid space it runs in. procStart and pidDomain
// are what Claude Code's build for this system writes (see procStart and
// proc.PidDomain); where rota knows no pid space, none is written.
// startedAt is when the fake first made the file. waitingFor is null unless
// a word says what is waited for. The file is written beside its place and
// renamed in, so a reader never sees half of one.
func writeRegistry(status string, waiting []string) error {
	pid := os.Getpid()
	start, err := procStart(pid)
	if err != nil {
		return err
	}
	var waitingFor any
	if len(waiting) > 0 {
		waitingFor = waiting[0]
	}
	cwd, _ := os.Getwd()
	rec := map[string]any{
		"pid":        pid,
		"procStart":  start,
		"sessionId":  fmt.Sprintf("fake-%d", pid),
		"cwd":        cwd,
		"name":       "fake",
		"status":     status,
		"waitingFor": waitingFor,
		"kind":       "interactive",
		"startedAt":  madeAt(),
		"updatedAt":  time.Now().UnixMilli(),
		"version":    "2.1.318",
	}
	if domain := proc.PidDomain(); domain != "" {
		rec["pidDomain"] = domain
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	if home == "" {
		return errors.New("CLAUDE_CONFIG_DIR is not set")
	}
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// madeAt is when the fake first made its session file, in milliseconds
// since the epoch: Claude Code's startedAt, written when the session starts
// and kept as the file is rewritten.
var madeAt = sync.OnceValue(func() int64 { return time.Now().UnixMilli() })

// procStart is this process's start as Claude Code's build for this system
// writes it into procStart. On Linux it is field 22 of /proc/<pid>/stat,
// the start in clock ticks since boot, as digits (2.1.319). Elsewhere it is
// the output of `ps -o lstart= -p <pid>` under LC_ALL=C TZ=UTC, as the
// macOS build writes it (2.1.317 to 2.1.322): the start in UTC as ANSIC
// lays it out, whatever zone the fake runs in, and now where no start can
// be read.
func procStart(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		ticks, err := proc.StartTicks(pid)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(ticks, 10), nil
	}
	start, err := proc.StartTime(pid)
	if err != nil {
		start = time.Now()
	}
	return start.UTC().Format(time.ANSIC), nil
}
