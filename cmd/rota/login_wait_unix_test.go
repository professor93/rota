//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/pty"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// ^C while the command waits ends the wait with the status an interrupt
// gives, and leaves the login to be finished later.
func TestLoginOnATerminalLeavesTheLoginOpenOnAnInterrupt(t *testing.T) {
	home := seedTwoClaude(t)
	typing(t, "")
	duringWait(t, func(ctx context.Context) {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Error(err)
		}
	})
	out, errOut, code := call(t, "login", "12")
	if code != 130 {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	id := loginIDFrom(t, out)
	if !strings.Contains(errOut, "rota login "+id+" <code>") || !strings.Contains(parked(t, home), id) {
		t.Fatalf("not left open: %q %s", errOut, parked(t, home))
	}
}

// The look at whether the login is still parked takes the store's lock, which
// another rota may hold for half a minute. ^C is not held up behind it.
func TestAnInterruptIsNotHeldBehindTheStoreLock(t *testing.T) {
	home := seedTwoClaude(t)
	typing(t, "")
	was := pendingCheck
	pendingCheck = 20 * time.Millisecond
	t.Cleanup(func() { pendingCheck = was })
	var sent atomic.Int64
	duringWait(t, func(context.Context) {
		other, err := store.Open(home) // another rota, holding the lock
		if err != nil {
			t.Error(err)
			return
		}
		go func() {
			time.Sleep(200 * time.Millisecond)
			sent.Store(time.Now().UnixNano())
			syscall.Kill(os.Getpid(), syscall.SIGINT)
			time.Sleep(3 * time.Second)
			other.Close()
		}()
	})
	r := result(t, callAsync(t, "login", "12"))
	took := time.Duration(time.Now().UnixNano() - sent.Load())
	if r.code != 130 || took > time.Second {
		t.Fatalf("%d, %v after ^C\n%s\n%s", r.code, took, r.out, r.err)
	}
}

// failingDevice is a device login whose provider answers every question with
// err: a network that is down, or a refusal.
type failingDevice struct {
	name  string
	err   error
	asked *atomic.Int32
}

func (d failingDevice) Name() string { return d.name }
func (failingDevice) Begin(context.Context) (string, map[string]string, error) {
	return "https://example.test/device", map[string]string{"kind": "device"}, nil
}
func (d failingDevice) Complete(context.Context, string, map[string]string) (*rota.Token, error) {
	d.asked.Add(1)
	return nil, d.err
}
func (failingDevice) Launch(*rota.Account, string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}

var (
	downAsked   = &atomic.Int32{}
	deniedAsked = &atomic.Int32{}
)

func init() {
	rota.Register(failingDevice{"t-wait-device-down", errors.New("connection refused"), downAsked})
	rota.Register(failingDevice{"t-wait-device-denied", &rota.OAuthError{Code: "access_denied"}, deniedAsked})
}

// A device wait whose provider keeps failing still waits between questions,
// and ^C still ends it.
func TestADeviceWaitThatKeepsFailingStillStopsOnAnInterrupt(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	typing(t, "")
	was := devicePoll
	devicePoll = 50 * time.Millisecond
	t.Cleanup(func() { devicePoll = was })
	downAsked.Store(0)
	duringWait(t, func(context.Context) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			syscall.Kill(os.Getpid(), syscall.SIGINT)
		}()
	})
	r := result(t, callAsync(t, "login", "t-wait-device-down"))
	if r.code != 130 || downAsked.Load() > 10 {
		t.Fatalf("%d, asked %d times\n%s\n%s", r.code, downAsked.Load(), r.out, r.err)
	}
	if n := strings.Count(r.err, "connection refused"); n != 1 {
		t.Fatalf("the same failure said %d times: %q", n, r.err)
	}
}

// A provider that refuses the device login has spent it: the wait stops there.
func TestADeviceWaitStopsWhenTheProviderRefuses(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	typing(t, "")
	deniedAsked.Store(0)
	r := result(t, callAsync(t, "login", "t-wait-device-denied"))
	if r.code != 1 || deniedAsked.Load() != 1 {
		t.Fatalf("%d, asked %d times\n%s\n%s", r.code, deniedAsked.Load(), r.out, r.err)
	}
}

// At a real terminal, a pasted key is not shown as it is typed, and ^C at the
// prompt leaves the login open with the status an interrupt gives.
func TestAKeyTypedAtARealTerminalIsNotShown(t *testing.T) {
	for _, c := range []struct {
		name, typed, want string
	}{
		{"a key", "sk-test-key\r", "Added t-cli-fake account 1"},
		{"^C", "\x03", "The login stays open for "},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			cmd := exec.Command(os.Args[0], "login", "t-cli-fake")
			cmd.Env = append(os.Environ(), "ROTA_AS_RUN=1", "ROTA_HOME="+home)
			master, err := pty.Start(cmd, 120, 25)
			if err != nil {
				t.Fatalf("no pseudo-terminal: %v", err)
			}
			screen := watchScreen(master)
			t.Cleanup(func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_ = cmd.Wait()
				master.Close()
			})
			screen.wait(t, "it is not shown")
			// The prompt is printed with the echo already off, so the moment it
			// shows is a moment the terminal echoes nothing.
			tio, ok := termios(master.Fd())
			if !ok {
				t.Fatal("the pseudo-terminal's settings cannot be read")
			}
			if tio.Lflag&syscall.ECHO != 0 {
				t.Fatal("the terminal echoes what is typed at the key prompt")
			}
			if _, err := master.Write([]byte(c.typed)); err != nil {
				t.Fatal(err)
			}
			screen.wait(t, c.want)
			if strings.Contains(screen.text(), "sk-test-key") {
				t.Fatalf("the key is on the screen:\n%s", screen.text())
			}
			err = cmd.Wait()
			var ee *exec.ExitError
			if c.typed == "\x03" && (!errors.As(err, &ee) || ee.ExitCode() != 130) {
				t.Fatalf("^C: %v\n%s", err, screen.text())
			}
		})
	}
}
