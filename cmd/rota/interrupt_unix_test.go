//go:build unix

package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// An interrupt, a SIGTERM or a closed terminal during a command's look at its
// accounts ends the look, which finishes what is under way, rather than the
// process, which would not; and once the look is over the signal is this
// process's default again.
func TestASignalDuringALookEndsTheLookNotTheProcess(t *testing.T) {
	if interruptedDuring(func(context.Context) {}) {
		t.Fatal("a look nothing interrupted reads as interrupted")
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if signal.Ignored(sig) {
			continue // left as this process was started, by design
		}
		cut := interruptedDuring(func(ctx context.Context) {
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Errorf("%v did not end the look", sig)
			}
		})
		if !cut {
			t.Errorf("%v: the look does not read as interrupted", sig)
		}
	}
}

// One that reaches the process as the look ends is not lost on the way to the
// signals going back: the look reads as interrupted, so nothing is started
// after it, rather than the signal being taken and the command going on.
func TestASignalAsALookEndsIsNotLost(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			if signal.Ignored(sig) {
				t.Skip("left as this process was started, by design")
			}
			// Taken here too, to know when it has reached this process,
			// which on Linux may be after kill returns: a signal sent to a
			// process may be taken by another of its threads than the sender.
			kept := make(chan os.Signal, 1)
			signal.Notify(kept, sig)
			t.Cleanup(func() { signal.Stop(kept) })
			for i := range 50 {
				cut := interruptedDuring(func(context.Context) {
					if err := syscall.Kill(os.Getpid(), sig); err != nil {
						t.Fatal(err)
					}
					select {
					case <-kept:
					case <-time.After(10 * time.Second):
						t.Fatal("the signal never reached the process")
					}
				})
				if !cut {
					t.Fatalf("try %d: the signal was taken and the command went on", i)
				}
			}
		})
	}
}

// watchLooks remembers the context of the latest look a command began.
func watchLooks(t *testing.T) (latest func() context.Context) {
	t.Helper()
	var (
		mu  sync.Mutex
		ctx context.Context
	)
	lookBegun = func(c context.Context) {
		mu.Lock()
		defer mu.Unlock()
		ctx = c
	}
	t.Cleanup(func() { lookBegun = nil })
	return func() context.Context {
		mu.Lock()
		defer mu.Unlock()
		return ctx
	}
}

// interruptOnArrival sends this process SIGINT once the provider has been
// presented want and is holding its answer, waits for the look that is waiting
// on that answer to be cancelled by it, runs meanwhile, and lets the answer go.
func interruptOnArrival(t *testing.T, look func() context.Context, p *heldProvider, want string, meanwhile func()) {
	t.Helper()
	if rt := p.arrival(t); rt != want {
		t.Fatalf("presented %s, want %s", rt, want)
	}
	waiting := look()
	if waiting == nil {
		t.Fatal("the refresh was made outside any look")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupt did not cancel the look")
	}
	if meanwhile != nil {
		meanwhile()
	}
	p.letGo()
}

type called struct {
	out, err string
	code     int
}

func callAsync(t *testing.T, args ...string) <-chan called {
	got := make(chan called, 1)
	go func() {
		out, errOut, code := call(t, args...)
		got <- called{out, errOut, code}
	}()
	return got
}

func result(t *testing.T, got <-chan called) called {
	t.Helper()
	select {
	case r := <-got:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("the command did not return")
		return called{}
	}
}

// rota list interrupted while a refresh is being answered: the refresh goes on
// to its answer, the listing is not printed, the command exits with 130 — and
// what went wrong beside the interrupt is said, not swallowed. Here the store
// cannot be saved after the answer, so the new login is in the home alone, and
// the person is told; that the rest was left for the next look is not news.
func TestAnInterruptedListFinishesTheRefreshAndSaysWhatWentWrong(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	p := newHeldProvider(t)
	writeAccounts(t, home, expiredClaude(t, home, 1, "R0"))
	look := watchLooks(t)
	got := callAsync(t, "list")
	accounts := filepath.Join(home, "accounts.json")
	kept := filepath.Join(t.TempDir(), "accounts.json")
	interruptOnArrival(t, look, p, "R0", func() {
		// The save after the answer fails, whoever runs the test: a file
		// cannot be renamed over a directory.
		if err := os.Rename(accounts, kept); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(accounts, 0o700); err != nil {
			t.Fatal(err)
		}
	})
	r := result(t, got)
	_ = os.Remove(accounts)
	_ = os.Rename(kept, accounts)
	if r.code != 130 || r.out != "" {
		t.Fatalf("an interrupted listing exits 130 and lists nothing: %d %q %q", r.code, r.out, r.err)
	}
	if !strings.Contains(r.err, "warning:") || !strings.Contains(r.err, "accounts.json") {
		t.Errorf("the failed save was not said: %q", r.err)
	}
	if strings.Contains(r.err, "whoever asked went away") {
		t.Errorf("what was left for the next look is not news to whoever interrupted: %q", r.err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "homes", "claude-1", ".credentials.json"))
	if !strings.Contains(string(raw), `"R0+"`) {
		t.Errorf("the new login is not in the home, the one copy left: %s", raw)
	}
	if got := p.tokens(); !slices.Equal(got, []string{"R0"}) {
		t.Errorf("presented %v", got)
	}
}

// A run on a named account, headless or kept open with --input, refreshes its
// login as its launch is made. An interrupt then lets that refresh finish and
// be saved, starts no CLI, and exits with 130; an interrupt once the CLI runs
// is the CLI's, as it always was.
func TestAnInterruptedLaunchKeepsTheRefreshAndStartsNothing(t *testing.T) {
	for _, way := range []struct {
		name string
		args []string
	}{
		{"headless", []string{"run", "1", "hello"}},
		{"--input", []string{"run", "1", "hello", "--input"}},
		{"handover", []string{"run", "1"}},
	} {
		t.Run(way.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("ROTA_HOME", home)
			p := newHeldProvider(t)
			writeAccounts(t, home, expiredClaude(t, home, 1, "R0"))
			bin, ran := t.TempDir(), filepath.Join(t.TempDir(), "ran")
			fakecli.Install(t, bin, "claude", fakecli.Spec{Stdout: []string{`[{"type":"result","result":"ANSWERED"}]`}, Touch: ran})
			t.Setenv("PATH", bin)
			argv := handover(t)
			look := watchLooks(t)
			got := callAsync(t, way.args...)
			interruptOnArrival(t, look, p, "R0", nil)
			r := result(t, got)
			if r.code != 130 {
				t.Fatalf("an interrupted launch exits 130: %d %q %q", r.code, r.out, r.err)
			}
			if _, err := os.Stat(ran); err == nil || *argv != nil {
				t.Fatalf("a CLI was started after the interrupt: ran=%v handover=%v", err == nil, *argv)
			}
			holdsLogin(t, home, 1, "R0+")
			if got := p.tokens(); !slices.Equal(got, []string{"R0"}) {
				t.Errorf("presented %v", got)
			}
		})
	}
}
