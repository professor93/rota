package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/events"
)

const (
	// cmdStderr is how much of a program's stderr is kept: its first line
	// is all that is said, and a program that writes on is not stored.
	cmdStderr = 4 << 10
	// cmdLine bounds that first line as the error says it.
	cmdLine = 200
	// cmdWaitDelay is how long a run waits, once its program has exited or
	// been killed, for whatever still holds its stderr: a helper it left
	// behind cannot hold the attempt.
	cmdWaitDelay = 2 * time.Second
	// cmdTempFail is EX_TEMPFAIL, the exit that asks for another try.
	cmdTempFail = 75
)

// cmdKill is killGroup, a variable so a test can play a program that ended
// by itself just as its time ran out.
var cmdKill = killGroup

// command is a command sink: a program run once for each event, handed the
// event on its stdin.
type command struct {
	path string   // argv[0], looked up when the sink was made
	args []string // the rest of argv
	hide []string // the variables the program must not see
	keys []string // their values when the sink was made
}

// newCommand makes a command sink. Its program is argv[0] as it is when it
// is an absolute path, else a name looked up on PATH now, once; a name
// found only in a directory relative to wherever the daemon runs is refused
// (exec.ErrDot), as is a relative path. Its error may name the program but
// never quotes a file it read (newSender's contract).
func newCommand(s api.EventSink, e env) (sender, error) {
	if len(s.Command) == 0 || s.Command[0] == "" {
		return nil, errors.New("command is empty; give the program and its arguments")
	}
	name := s.Command[0]
	if !filepath.IsAbs(name) && name != filepath.Base(name) {
		return nil, fmt.Errorf("command %q is a relative path; give a name on PATH or an absolute path", name)
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	c := &command{path: path, args: slices.Clone(s.Command[1:]), hide: slices.Concat([]string{"ROTA_TOKEN"}, e.hide)}
	for _, h := range c.hide {
		if v := os.Getenv(h); h != "" && v != "" && !slices.Contains(c.keys, v) {
			c.keys = append(c.keys, v)
		}
	}
	return c, nil
}

// secrets is the values the hidden variables had when the sink was made: a
// program that prints one has it taken out of what is said.
func (c *command) secrets() []string { return c.keys }

// send runs the program once for e. Its stdin is the event's JSON and a
// newline, its environment the daemon's without the hidden variables, its
// stdout is thrown away and the first line of its stderr goes into the
// error. Exit 0 is delivered, 75 is tried again, and any other end is
// dropped. When ctx ends first, the program and its whole process group are
// killed, and the event dropped. A program that exits by itself has its
// group left alone: a script may mean to leave a job running in the
// background (sound &), and one still holding stderr costs the attempt no
// more than cmdWaitDelay.
func (c *command) send(ctx context.Context, e events.Event) error {
	cmd := exec.CommandContext(ctx, c.path, c.args...)
	cmd.Env = c.environ()
	cmd.Stdin = bytes.NewReader(cmdJSON(e))
	stderr := &cmdHead{}
	cmd.Stderr = stderr
	cmd.WaitDelay = cmdWaitDelay
	ownGroup(cmd)
	// killed is set only when the kill landed: a program that ended by
	// itself as ctx ended is said by how it ended.
	var killed atomic.Bool
	cmd.Cancel = func() error {
		err := cmdKill(cmd)
		killed.Store(err == nil)
		return err
	}
	err := cmd.Run()
	st := cmd.ProcessState
	if err == nil || st != nil && st.Exited() && st.ExitCode() == 0 {
		// What held on to its stderr afterwards is not the program's failure.
		return nil
	}
	said := c.said(stderr.firstLine())
	switch {
	case killed.Load() || st == nil && ctx.Err() != nil:
		// Asked first, since a program killed on Windows exits 1.
		return dropped(errors.New("command did not exit in time and was killed" + said))
	case st != nil && st.Exited():
		if code := st.ExitCode(); code != cmdTempFail {
			return dropped(fmt.Errorf("command exited %d%s", code, said))
		}
		return again(errors.New("command exited 75"+said), 0)
	case st != nil:
		return dropped(errors.New("command ended: " + st.String() + said))
	}
	return cmdStartFailed(err, c.keys)
}

// cmdStartFailed is a program that did not start, as send says it, cleaned
// with keys. A failure that passes, such as a fork limit or a binary being
// written, is tried again; any other, such as a program gone, not allowed
// to run or not a program at all (a script without its #! line), is
// dropped at once. Windows says its own codes, never these, so there every
// start failure is dropped.
func cmdStartFailed(err error, keys []string) error {
	msg := errors.New(cleanErr(fmt.Errorf("command could not start: %w", err), keys))
	for _, temp := range []error{syscall.EAGAIN, syscall.ENOMEM, syscall.ETXTBSY, syscall.EMFILE, syscall.ENFILE, syscall.EINTR} {
		if errors.Is(err, temp) {
			return msg
		}
	}
	return dropped(msg)
}

// said is line as an error ends with it: after ": ", without a hidden value
// in it, and at most cmdLine bytes, cut on a rune's edge; "" for no line.
func (c *command) said(line string) string {
	if line == "" {
		return ""
	}
	return ": " + events.Clean(redact(line, c.keys), cmdLine)
}

// environ is the daemon's environment without the hidden variables. A
// Windows name is the same name in any case.
func (c *command) environ() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.ContainsFunc(c.hide, func(h string) bool {
			return h == name || runtime.GOOS == "windows" && strings.EqualFold(h, name)
		})
	})
}

// cmdJSON is e as one line of JSON and a newline: the line the event
// stream carries as its data.
func cmdJSON(e events.Event) []byte {
	return append(events.JSON(e), '\n')
}

// cmdHead keeps the first cmdStderr bytes written to it and takes no notice
// of the rest.
type cmdHead struct {
	mu sync.Mutex
	b  []byte
}

func (h *cmdHead) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room := cmdStderr - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

// firstLine is the first line kept that is not blank, trimmed.
func (h *cmdHead) firstLine() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for l := range strings.Lines(string(h.b)) {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
