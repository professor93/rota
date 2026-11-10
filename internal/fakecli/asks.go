package fakecli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// writeRegistry writes a session file shaped like one Claude Code 2.1.318
// writes: every key it has, with this process's own pid and start. The start
// is what Claude Code writes, the output of `ps -o lstart= -p <pid>` under
// LC_ALL=C TZ=UTC: the start in UTC, as ANSIC lays it out, whatever zone the
// fake runs in. waitingFor is null unless a word says what is waited for. The
// file is written beside its place and renamed in, so a reader never sees half
// of one.
func writeRegistry(status string, waiting []string) error {
	pid := os.Getpid()
	start, err := proc.StartTime(pid)
	if err != nil {
		start = time.Now()
	}
	var waitingFor any
	if len(waiting) > 0 {
		waitingFor = waiting[0]
	}
	cwd, _ := os.Getwd()
	raw, err := json.Marshal(map[string]any{
		"pid":        pid,
		"procStart":  start.UTC().Format(time.ANSIC),
		"sessionId":  fmt.Sprintf("fake-%d", pid),
		"cwd":        cwd,
		"name":       "fake",
		"status":     status,
		"waitingFor": waitingFor,
		"kind":       "interactive",
		"startedAt":  start.UnixMilli(),
		"updatedAt":  time.Now().UnixMilli(),
		"version":    "2.1.318",
	})
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
