//go:build linux || darwin

package hold

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/pty"
)

// TestCaptureVT records what vim, less and the fake CLI write to an 80×24
// terminal, into testdata/vt/<name>.bin, for the screen tests to replay. It
// is a recording session, not a test: it runs real programs, its output
// depends on their versions, and the files it writes are committed, so it
// runs only when asked.
//
//	ROTA_VT_CAPTURE=1 go test -count=1 -run TestCaptureVT ./cmd/rota/hold/
//
// The goldens are made from the new files afterwards; scripts/vt/README says
// how.
func TestCaptureVT(t *testing.T) {
	if os.Getenv("ROTA_VT_CAPTURE") != "1" {
		t.Skip("ROTA_VT_CAPTURE=1 records the captures")
	}
	home := t.TempDir()
	// Nothing of the caller's environment is passed on, so a capture is the
	// same on any machine with the same programs. The locale is the one
	// addition: without UTF-8, vim and less show a wide rune as its bytes.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"TERM=xterm-256color",
		"HOME=" + home,
		"LESS=",
		"LESSHISTFILE=-",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
	}

	t.Run("vim", func(t *testing.T) {
		c := record(t, env, "vim", "-u", "NONE", "-N", "-i", "NONE", "-n",
			"-c", "syntax on", "testdata/vt/sample.go")
		c.settle(t)
		c.typed(t, "/func\r")
		c.typed(t, ":set number\r")
		save(t, "vim", c.bytes())
		c.typed(t, ":q!\r")
		c.exit(t)
		save(t, "vim-quit", c.bytes())
	})

	t.Run("less", func(t *testing.T) {
		c := record(t, env, "less", "-R", "testdata/vt/sample.ansi")
		c.settle(t)
		c.typed(t, "G")
		c.typed(t, "g")
		save(t, "less", c.bytes())
	})

	t.Run("fake", func(t *testing.T) {
		fake := fakecli.Install(t, t.TempDir(), "claude", fakecli.Spec{Tty: true})
		c := record(t, env, fake)
		// A typed line is echoed by the terminal before the fake reads it,
		// and an echoed "print 1b5b..." would be drawn on the screen under
		// test. The fake's output is what is being recorded, so the echo is
		// turned off.
		noEcho(t, c.master)
		c.wait(t, "ready 80x24")
		esc := "\x1b"
		for _, p := range []string{
			esc + "]0;rota spike\a",
			esc + "[?1049h" + esc + "[H" + esc + "[2J",
			esc + "[?1h" + esc + "[?2004h" + esc + "[?1002h" + esc + "[?1006h" + esc + "[?1004h" + esc + "[=1;1u",
			"\a",
			esc + "[2;3H" + esc + "[38;5;196m256-colour" + esc + "[48;5;21m on 21" + esc + "[0m " + esc + "[38;5;244mgrey" + esc + "[0m",
			esc + "[3;3H" + esc + "[38;2;255;128;0mtruecolour" + esc + "[48;2;0;64;128m on navy" + esc + "[0m",
			esc + "[4;3H" + esc + "[1mbold" + esc + "[22m " + esc + "[3mitalic" + esc + "[23m " +
				esc + "[4munderline" + esc + "[24m " + esc + "[7minverse" + esc + "[27m " + esc + "[2mdim" + esc + "[0m",
			esc + "[5;3Hwide: 世界!",
			esc + "[6;3H" + esc + "[31mred" + esc + "[0m " + esc + "[42mon green" + esc + "[0m " + esc + "[95mbright magenta" + esc + "[0m",
			esc + "[44m" + esc + "[8;1H" + esc + "[K" + esc + "[0m" + esc + "[8;5Hon a blue line",
			esc + "[12;40H" + esc + "[?25l",
		} {
			c.typed(t, "print "+hex.EncodeToString([]byte(p))+"\n")
		}
		save(t, "fake", c.bytes())
	})

	// Emoji and CJK are two columns wide and a combining mark none, which
	// is where emulators part ways first. Its goldens come from xterm with
	// Unicode 11 widths, as real terminals draw them; see scripts/vt/README.
	t.Run("emoji", func(t *testing.T) {
		fake := fakecli.Install(t, t.TempDir(), "claude", fakecli.Spec{Tty: true})
		c := record(t, env, fake)
		noEcho(t, c.master)
		c.wait(t, "ready 80x24")
		esc := "\x1b"
		for _, p := range []string{
			esc + "[H" + esc + "[2J",
			esc + "[2;3Ha✅b🤖c",
			esc + "[3;3H" + esc + "[32m✅ done" + esc + "[0m " + esc + "[44m🤖 on blue" + esc + "[0m",
			esc + "[4;3H日本語のテキスト: CJK",
			esc + "[5;3Hcafe\u0301 man\u0303ana, a\u0301\u0302 two marks",
			esc + "[6;3H✅日🤖e\u0301|",
			esc + "[8;1H🤖✅",
		} {
			c.typed(t, "print "+hex.EncodeToString([]byte(p))+"\n")
		}
		save(t, "emoji", c.bytes())
	})
}

// capture is one program on a terminal, and everything it has written.
type capture struct {
	master *os.File
	cmd    *exec.Cmd

	mu   sync.Mutex
	out  bytes.Buffer
	last time.Time
	done chan struct{}
}

func record(t *testing.T, env []string, argv ...string) *capture {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	master, err := pty.Start(cmd, 80, 24)
	if err != nil {
		t.Fatalf("starting %s: %v", argv[0], err)
	}
	c := &capture{master: master, cmd: cmd, last: time.Now(), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			c.mu.Lock()
			c.out.Write(buf[:n])
			if n > 0 {
				c.last = time.Now()
			}
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		master.Close()
		<-c.done
	})
	return c
}

func (c *capture) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.out.Bytes())
}

// settle waits until the program has said something and then nothing for
// half a second: a full-screen program draws in bursts, and the recording
// must not stop in the middle of one.
func (c *capture) settle(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		c.mu.Lock()
		quiet := c.out.Len() > 0 && time.Since(c.last) > 500*time.Millisecond
		c.mu.Unlock()
		if quiet {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the program never went quiet; it wrote %q", c.bytes())
}

// typed sends keys and waits for whatever they draw.
func (c *capture) typed(t *testing.T, keys string) {
	t.Helper()
	before := len(c.bytes())
	if _, err := c.master.Write([]byte(keys)); err != nil {
		t.Fatalf("typing %q: %v", keys, err)
	}
	for deadline := time.Now().Add(5 * time.Second); len(c.bytes()) == before && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	c.settle(t)
}

func (c *capture) wait(t *testing.T, text string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if strings.Contains(string(c.bytes()), text) {
			c.settle(t)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%q never arrived; the program wrote %q", text, c.bytes())
}

// exit waits for the program to end and for the last of its output.
func (c *capture) exit(t *testing.T) {
	t.Helper()
	exited := make(chan error, 1)
	go func() { exited <- c.cmd.Wait() }()
	select {
	case err := <-exited:
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the program did not exit")
	}
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the terminal never said end of file")
	}
}

func save(t *testing.T, name string, b []byte) {
	t.Helper()
	path := filepath.Join("testdata", "vt", name+".bin")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s, %d bytes", path, len(b))
}

// noEcho turns the terminal's echo off through its master, which shares the
// slave's settings.
func noEcho(t *testing.T, master *os.File) {
	t.Helper()
	conn, err := master.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var errno syscall.Errno
	if err := conn.Control(func(fd uintptr) {
		var tio syscall.Termios
		if _, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, getTermios, uintptr(unsafe.Pointer(&tio))); errno != 0 {
			return
		}
		tio.Lflag &^= syscall.ECHO | syscall.ECHONL
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, setTermios, uintptr(unsafe.Pointer(&tio)))
	}); err != nil {
		t.Fatal(err)
	}
	if errno != 0 {
		t.Fatalf("turning the echo off: %v", errno)
	}
}
