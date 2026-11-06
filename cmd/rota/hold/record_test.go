//go:build linux || darwin

package hold

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTheHolderLogMovesAsideAtItsCap: every goroutine of a holder logs at
// once, and the log never grows past its cap: it moves to holder.log.1 and
// starts again, both readable by their owner alone, no line cut in two.
func TestTheHolderLogMovesAsideAtItsCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "holder.log")
	l := &logFile{path: path}
	log := l.logger()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				log("a line", "goroutine", g, "i", i, "pad", strings.Repeat("x", 40))
			}
		}()
	}
	wg.Wait()
	l.close()
	for _, p := range []string{path, path + ".1"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > logCap {
			t.Fatalf("%s is %d bytes, past the cap", p, fi.Size())
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %o", p, fi.Mode().Perm())
		}
		raw, _ := os.ReadFile(p)
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			if !strings.HasPrefix(line, "time=") || !strings.HasSuffix(line, strings.Repeat("x", 40)) {
				t.Fatalf("%s has a line cut short: %q", p, line)
			}
		}
	}
}

// TestTheRegistryIsLookedForWhereTheCLILooks: Claude Code keeps its registry
// under CLAUDE_CONFIG_DIR, or ~/.claude without it, and writes its times in
// TZ's zone, the last TZ given winning as getenv reads it.
func TestTheRegistryIsLookedForWhereTheCLILooks(t *testing.T) {
	path, _ := registryOf([]string{"HOME=/h", "CLAUDE_CONFIG_DIR=/c"}, 7)
	if path != filepath.Join("/c", "sessions", "7.json") {
		t.Fatalf("with CLAUDE_CONFIG_DIR: %s", path)
	}
	if path, _ = registryOf([]string{"HOME=/h"}, 7); path != filepath.Join("/h", ".claude", "sessions", "7.json") {
		t.Fatalf("without it: %s", path)
	}
	if path, _ = registryOf([]string{"PATH=/bin"}, 7); path != "" {
		t.Fatalf("with neither there is nowhere to look: %s", path)
	}

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no zone database here: %v", err)
	}
	type zoneCase struct {
		env  []string
		want *time.Location
	}
	cases := []zoneCase{
		{nil, time.Local},
		{[]string{"TZ="}, time.UTC},
		{[]string{"TZ=:UTC"}, time.UTC},
		{[]string{"TZ=Europe/Paris", "TZ=America/New_York"}, ny},
		{[]string{"TZ=:America/New_York"}, ny},
		{[]string{"TZ=Not/A_Zone"}, time.Local},
	}
	if file := "/usr/share/zoneinfo/America/New_York"; fileExists(file) {
		cases = append(cases, zoneCase{[]string{"TZ=:" + file}, ny})
	}
	for _, c := range cases {
		got := zoneOf(c.env)
		probe := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
		if got.String() != c.want.String() && probe.In(got).Format(time.RFC3339) != probe.In(c.want).Format(time.RFC3339) {
			t.Errorf("%v: zone %s, want %s", c.env, got, c.want)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
