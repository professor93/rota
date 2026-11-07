//go:build !linux && !darwin

package proc

import "net"

// PeerPID is not known here.
func PeerPID(c *net.UnixConn) (int, error) { return 0, ErrPlatform }

// PeerUID is not known here either.
func PeerUID(c *net.UnixConn) (int, error) { return -1, ErrPlatform }
