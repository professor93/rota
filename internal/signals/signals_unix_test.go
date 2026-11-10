//go:build unix

package signals

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

var taken = []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// A signal during the call cancels its context, and is said to have come.
func TestASignalDuringTheCallCancelsIt(t *testing.T) {
	if During([]os.Signal{syscall.SIGTERM}, func(context.Context) {}) {
		t.Fatal("a call nothing signalled reads as signalled")
	}
	for _, sig := range taken {
		if signal.Ignored(sig) {
			continue // left as this process was started
		}
		came := During([]os.Signal{sig}, func(ctx context.Context) {
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
				t.Errorf("%v did not cancel the call", sig)
			}
		})
		if !came {
			t.Errorf("%v: the call does not read as signalled", sig)
		}
	}
}

// One that reaches the process as the call ends, before its context has been
// cancelled, is counted all the same.
func TestASignalAsTheCallEndsIsCounted(t *testing.T) {
	for _, sig := range taken {
		t.Run(sig.String(), func(t *testing.T) {
			if signal.Ignored(sig) {
				t.Skip("left as this process was started")
			}
			// Taken here too, to know when it has reached this process,
			// which on Linux may be after kill returns.
			kept := make(chan os.Signal, 1)
			signal.Notify(kept, sig)
			t.Cleanup(func() { signal.Stop(kept) })
			for i := range 50 {
				came := During([]os.Signal{sig}, func(context.Context) {
					if err := syscall.Kill(os.Getpid(), sig); err != nil {
						t.Fatal(err)
					}
					select {
					case <-kept:
					case <-time.After(10 * time.Second):
						t.Fatal("the signal never reached the process")
					}
				})
				if !came {
					t.Fatalf("try %d: the signal was taken and not counted", i)
				}
			}
		})
	}
}

// One sent as the call returns, which may reach the process only after the
// signals have gone back, is either counted or ends the process, as it would
// have without the call: never taken and then lost. Measured in a process of
// its own, where nothing else takes it.
func TestASignalSentAsTheCallReturnsIsNeverLost(t *testing.T) {
	if name := os.Getenv("ROTA_SIGNALS_LATE"); name != "" {
		var sig syscall.Signal
		for _, s := range taken {
			if s.String() == name {
				sig = s
			}
		}
		for range 50 {
			if !During([]os.Signal{sig}, func(context.Context) { _ = syscall.Kill(os.Getpid(), sig) }) {
				time.Sleep(10 * time.Second) // for it to end this process, as it should
				os.Exit(5)
			}
		}
		os.Exit(0)
	}
	for _, sig := range taken {
		t.Run(sig.String(), func(t *testing.T) {
			if signal.Ignored(sig) {
				t.Skip("left as this process was started")
			}
			for range 3 {
				cmd := exec.Command(os.Args[0], "-test.run=^TestASignalSentAsTheCallReturnsIsNeverLost$")
				cmd.Env = append(os.Environ(), "ROTA_SIGNALS_LATE="+sig.String())
				err := cmd.Run()
				if err == nil {
					continue // every one was counted
				}
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != sig {
					t.Fatalf("the signal was taken and not counted: %v", ee)
				}
			}
		})
	}
}
