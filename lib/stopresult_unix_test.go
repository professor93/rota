//go:build unix

package rota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// errSink is what a caller's events writer says once it can take no more.
var errSink = errors.New("the sink has gone")

type failingSink struct{}

func (failingSink) Write([]byte) (int, error) { return 0, errSink }

// stopCase is one way a run ends before its CLI does, and what the run returned
// for it in 1.3.0 (140f2fb), when the CLI was killed outright. A CLI that dies
// of the SIGTERM it is now asked with first must give a caller exactly that.
type stopCase struct {
	what      string
	fake      fakecli.Spec
	spec      Spec
	lim       *Limits
	events    io.Writer
	cancel    time.Duration // 0: never cancelled
	err       error         // nil: none
	exit      int
	isError   bool
	truncated bool
}

func stopCases() []stopCase {
	long := strings.Repeat("x", 4096)
	return []stopCase{
		{what: "cancelled", fake: fakecli.Spec{Sleep: "30s"}, cancel: 300 * time.Millisecond,
			err: context.Canceled, exit: -1},
		{what: "timed out", fake: fakecli.Spec{Sleep: "30s"}, spec: Spec{TimeoutSeconds: 1},
			err: context.DeadlineExceeded, exit: -1},
		{what: "truncated", fake: fakecli.Spec{Stdout: []string{long, "{{sleep:30s}}"}},
			lim: &Limits{MaxBufferedOutput: 1024}, exit: -1, isError: true, truncated: true},
		// Over the scanner's own 64KB start too, which is the least a cap can be.
		{what: "over the line cap", fake: fakecli.Spec{Stdout: []string{strings.Repeat("x", 100<<10), "{{sleep:30s}}"}},
			spec: Spec{Stream: true}, lim: &Limits{MaxEventLine: 1024}, exit: -1, isError: true, truncated: true},
		{what: "its events writer failed", fake: fakecli.Spec{Stdout: []string{`{"type":"system"}`, "{{sleep:30s}}"}},
			spec: Spec{Stream: true}, events: failingSink{}, exit: -1, isError: true},
		{what: "its events writer failed after it ended", fake: fakecli.Spec{Stdout: []string{`{"type":"result","result":"ok"}`}},
			events: failingSink{}, err: errSink, exit: 0},
	}
}

func checkStopCase(t *testing.T, res *Result, err error, c stopCase) {
	t.Helper()
	if res == nil {
		t.Fatalf("a result: %v", err)
	}
	if c.err == nil && err != nil || c.err != nil && !errors.Is(err, c.err) {
		t.Fatalf("error %v, want %v", err, c.err)
	}
	if res.ExitCode != c.exit || res.IsError != c.isError || res.Truncated != c.truncated {
		t.Fatalf("exit=%d is_error=%v truncated=%v, want %d %v %v",
			res.ExitCode, res.IsError, res.Truncated, c.exit, c.isError, c.truncated)
	}
}

func runStopCase(t *testing.T, dir string, i int, c stopCase) (*Result, error) {
	t.Helper()
	bin := fakecli.Install(t, dir, "stop-case-"+string(rune('a'+i)), c.fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if c.cancel > 0 {
		time.AfterFunc(c.cancel, cancel)
	}
	a := &Account{ID: 1, Provider: "claude"}
	a.Token.Access = "tok"
	spec := c.spec
	spec.Prompt, spec.flavorOverride = "p", "claude"
	events := c.events
	if events == nil {
		events = &bytes.Buffer{}
	}
	return Run(ctx, a, "", &Command{Bin: bin, BaseEnv: []string{"PATH=/usr/bin:/bin"}}, spec, c.lim, events)
}

// Every way a run ends before its CLI does gives what 1.3.0 gave, for a CLI that
// does what a CLI does with a SIGTERM it does not handle. The values are pinned
// from 140f2fb, where this same test passes.
func TestARunEndedBeforeItsCLIGivesWhatItAlwaysGave(t *testing.T) {
	dir := t.TempDir()
	for i, c := range stopCases() {
		t.Run(c.what, func(t *testing.T) {
			res, err := runStopCase(t, dir, i, c)
			checkStopCase(t, res, err, c)
		})
	}
}

// Claude Code handles SIGTERM itself, so for a real run this is the common case.
// A run rota stopped for a limit, the buffered output, the line cap or an events
// writer that failed while the CLI was running, gives exactly what 1.3.0 gave
// whatever the CLI exits with after the asking: rota knows it made that stop. A
// cancelled or timed-out run keeps the CLI's own exit, since its error already
// says what happened.
func TestARunStoppedForALimitGivesWhatItAlwaysGaveWhateverItsCLIExitsWith(t *testing.T) {
	dir := t.TempDir()
	for _, exit := range []int{0, 3} {
		for i, c := range stopCases() {
			if c.what == "its events writer failed after it ended" {
				continue // it ends by itself, and is pinned above
			}
			c.fake.Term, c.fake.Exit = "exit", exit
			if c.cancel > 0 || c.spec.TimeoutSeconds > 0 {
				c.exit, c.isError = exit, false
			}
			t.Run(c.what, func(t *testing.T) {
				res, err := runStopCase(t, dir, i+10*exit, c)
				checkStopCase(t, res, err, c)
			})
		}
	}
}
