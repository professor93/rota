//go:build linux || darwin

package fakecli

import (
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

// termSize asks the kernel how big this program's window is. It is the
// device that is asked, not the environment: a size read out of $COLUMNS
// would prove only that somebody set $COLUMNS.
func termSize() (cols, rows uint16) {
	var ws struct {
		rows, cols     uint16
		xpixel, ypixel uint16
	}
	conn, err := os.Stdout.SyscallConn()
	if err != nil {
		return 0, 0
	}
	_ = conn.Control(func(fd uintptr) {
		syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	})
	return ws.cols, ws.rows
}

// watchResize calls back whenever the window changes, which is what the
// kernel's SIGWINCH is for.
func watchResize(changed func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			changed()
		}
	}()
}
