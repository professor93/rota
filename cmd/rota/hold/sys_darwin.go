//go:build darwin

package hold

import "syscall"

// The BSDs, macOS among them, name the ioctl that reads a terminal's
// settings TIOCGETA.
const readTermios = syscall.TIOCGETA

// dupOnto makes newfd another name for oldfd.
func dupOnto(oldfd, newfd int) error { return syscall.Dup2(oldfd, newfd) }

// sessionOf is the session this process belongs to.
func sessionOf() (int, error) { return syscall.Getsid(0) }
