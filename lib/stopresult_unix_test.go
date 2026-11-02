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
		})
	}
}
