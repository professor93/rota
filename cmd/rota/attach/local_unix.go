//go:build linux || darwin

package attach

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/muesli/cancelreader"

	"github.com/professor93/rota/internal/pty"
)

// The person's own terminal, for `rota run --attach`.
//
// Raw mode is two ioctls, as in package main's rawMode, copied here because
// this package cannot import main and rota takes no module for it: read the
// settings, set them with line editing, echo, signals and output processing
// off, and set the ones read back at the end. The ioctl names differ by
// kernel, so they are in local_linux.go and local_darwin.go.

// localNotATerminal is what Local says of standard input that is not a
// terminal: a pipe, a file, or /dev/null.
var localNotATerminal = errors.New("attach: standard input is not a terminal")

// Local is the person's own terminal as an attach's Terminal: what they type
// is read from in, the screen is out, and the window is in's. It puts in in
// raw mode, so every key reaches the session as typed, Ctrl-C and Ctrl-\
// among them, and the CLI's own terminal does the echoing and editing. The
// window's size is asked of in each time, and Resized fires on SIGWINCH.
//
// One goroutine reads in and sends each chunk on Input. Its read can be
// cancelled without taking a byte: it waits for in to be readable, and
// restore wakes it.
//
// restore must be called on every way out, and at once when the attach
// returns: a key typed between the attach's last receive and restore is
// dropped, taken by the reader or flushed, as everything typed before the
// terminal is handed back is. restore stops that reader, so a key typed
// after it is read by whatever reads in next and by nothing else; then it sets the settings Local found, stops following the window,
// and drops whatever was typed and not yet read, such as the key release a
// kitty terminal reports after the last key, so it never reaches the shell.
// It is safe to call twice, and the second call does nothing. An in that is
// not a terminal is refused, and then t and restore are nil.
func Local(in, out *os.File) (t Terminal, restore func(), err error) {
	before, err := localTermios(in)
	if err != nil {
		if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.ENODEV) {
			return nil, nil, localNotATerminal
		}
		return nil, nil, fmt.Errorf("attach: reading the terminal's settings: %w", err)
	}
	if err := localSetTermios(in, localRaw(before)); err != nil {
		return nil, nil, fmt.Errorf("attach: making the terminal raw: %w", err)
	}
	reader, err := localNewReader(in)
	if err != nil {
		_ = localSetTermios(in, before)
		return nil, nil, fmt.Errorf("attach: reading the terminal: %w", err)
	}
	l := &localTerm{
		in:      in,
		out:     out,
		reader:  reader,
		input:   make(chan []byte),
		pumped:  make(chan struct{}),
		winch:   make(chan os.Signal, 1),
		resized: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	l.last.Store(localSize{80, 24})
	l.Size()
	signal.Notify(l.winch, syscall.SIGWINCH)
	go l.follow()
	go l.pump()
	var once sync.Once
	return l, func() {
		once.Do(func() {
			// The reader first, before anything is put back: once it has
			// stopped, no key is taken from in on nobody's behalf.
			close(l.done)
			if l.reader.Cancel() {
				// A reader that cannot be woken, cancelreader's fallback,
				// is not waited for: it ends at the next key, and drops it.
				<-l.pumped
			}
			_ = l.reader.Close()
			signal.Stop(l.winch)
			_ = localSetTermios(in, before)
			_ = localFlushInput(in)
		})
	}, nil
}

// localNewReader is cancelreader.NewReader, and a variable only so that a
// test can give the pump a read that panics.
var localNewReader = cancelreader.NewReader

// pump reads in and hands each chunk to Input, until the read fails or
// restore cancels it. Input is closed as it stops, a panic in the read
// included, which inputErr then says: the attach ends Lost with it and
// puts the terminal back, where the panic would have ended the process
// with the terminal raw.
func (l *localTerm) pump() {
	defer close(l.pumped)
	defer close(l.input)
	defer func() {
		if p := recover(); p != nil {
			l.inErr.Store(fmt.Errorf("input: panic: %v", p))
		}
	}()
	buf := make([]byte, 4096)
	for {
		n, err := l.reader.Read(buf)
		if n > 0 {
			select {
			case l.input <- bytes.Clone(buf[:n]):
			case <-l.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// localRaw is before with everything the terminal does on its own turned
// off, as main's rawMode sets it: no echo, no line editing, no signals from
// ^C or ^\, no CR and NL translation either way, eight bits, and a read that
// returns as soon as one byte is there.
func localRaw(before syscall.Termios) syscall.Termios {
	raw := before
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	return raw
}

// localSize is a window in cells.
type localSize struct{ cols, rows int }

// localTerm is Local's Terminal.
type localTerm struct {
	in, out *os.File
	reader  cancelreader.CancelReader
	input   chan []byte
	pumped  chan struct{} // closed once pump has stopped
	winch   chan os.Signal
	resized chan struct{}
	done    chan struct{}
	last    atomic.Value // localSize: the last size the terminal gave
	inErr   atomic.Value // error: why the pump stopped, when it panicked
}

// inputErr is why Input was closed, when it was not the terminal's own end:
// the pump's panic. Nil otherwise.
func (l *localTerm) inputErr() error {
	err, _ := l.inErr.Load().(error)
	return err
}

func (l *localTerm) Input() <-chan []byte        { return l.input }
func (l *localTerm) Write(p []byte) (int, error) { return l.out.Write(p) }
func (l *localTerm) Resized() <-chan struct{}    { return l.resized }

// Size asks the terminal, and keeps the last answer for when it cannot say
// or says 0, which a serial line or a half-made window may: 80x24 until the
// terminal has given a size once.
func (l *localTerm) Size() (cols, rows int) {
	c, r, err := pty.Size(l.in)
	if err == nil && c > 0 && r > 0 {
		z := localSize{int(c), int(r)}
		l.last.Store(z)
		return z.cols, z.rows
	}
	z := l.last.Load().(localSize)
	return z.cols, z.rows
}

// follow turns SIGWINCH into Resized until restore. Several signals before
// anyone looks are one Resized, since Size reads the window as it is then.
func (l *localTerm) follow() {
	for {
		select {
		case <-l.winch:
			select {
			case l.resized <- struct{}{}:
			default:
			}
		case <-l.done:
			return
		}
	}
}

// localTermios reads f's terminal settings.
func localTermios(f *os.File) (syscall.Termios, error) {
	var t syscall.Termios
	err := localIoctl(f, localGetTermios, unsafe.Pointer(&t))
	return t, err
}

// localSetTermios sets them, at once.
func localSetTermios(f *os.File, t syscall.Termios) error {
	return localIoctl(f, localSetTermiosNow, unsafe.Pointer(&t))
}

// localIoctl runs one ioctl on f through its SyscallConn, as internal/pty
// does. cancelreader calls Fd on the same file, which leaves it in blocking
// mode; that costs nothing here, since Local's reads are woken by
// cancelreader's own pipe, never by closing the file or by a deadline.
func localIoctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioerr error
	if err := conn.Control(func(fd uintptr) {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
			ioerr = errno
		}
	}); err != nil {
		return err
	}
	return ioerr
}
