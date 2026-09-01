package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/rotation"
)

// The SDK states conditions; this command prescribes its own remedies. When
// lib says "needs re-auth" it no longer says `rota login` — lib has no
// business knowing this program's name or verbs — so the command appends the
// hint itself, in the one place every error passes through.
func TestTheCommandAppendsItsOwnRemedies(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("account 6: %w", rota.ErrReauth), "rota login"},
		{fmt.Errorf("%w: none ordered", rotation.ErrNone), "rota set"},
		{fmt.Errorf("grok: %w", rota.ErrUnsupported), "rota run"},
	}
	for _, c := range cases {
		var out, errBuf bytes.Buffer
		cli := &cli{out: &out, err: &errBuf}
		if code := cli.report(c.err); code != 1 {
			t.Fatalf("exit %d for %v", code, c.err)
		}
		if !strings.Contains(errBuf.String(), c.want) {
			t.Fatalf("the remedy %q must follow %v, got: %s", c.want, c.err, errBuf.String())
		}
	}

	// A machine reading --json output gets the condition, not terminal prose.
	var out, errBuf bytes.Buffer
	cli := &cli{json: true, out: &out, err: &errBuf}
	_ = cli.report(fmt.Errorf("account 6: %w", rota.ErrReauth))
	if strings.Contains(out.String(), "rota login") || errBuf.Len() != 0 {
		t.Fatalf("hints are for people, not JSON: out=%s err=%s", out.String(), errBuf.String())
	}
}
