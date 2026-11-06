//go:build !linux && !darwin

package proc

import "net"

// PeerPID is not known here.
func PeerPID(c *net.UnixConn) (int, error) { return 0, errNoHeld }
