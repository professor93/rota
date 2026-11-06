package holdwire

import (
	"fmt"
	"path/filepath"
)

// Where everything is. A store's terminals directory (store.TerminalDir)
// holds one directory per held session, named by its id, and the daemon's
// three files beside them:
//
//	terminals/
//	  ctl.sock        the daemon's API for this machine's own user (0600)
//	  daemon.lock     flocked by the one daemon serving this store
//	  daemon.pid      who that daemon is
//	  <id>/           one held session (0700)
//	    sock          the holder's link to the daemon (0600)
//	    lock          flocked by the holder for its whole life
//	    meta.json     what the session is (0600, never a secret)
//	    holder.log    what the holder had to say
//
// Each name is said once, here, so the holder and the daemon cannot look in
// two places.

// Dir is one session's directory.
func Dir(terminals, id string) string { return filepath.Join(terminals, id) }

// SockPath is the socket a holder listens on.
func SockPath(dir string) string { return filepath.Join(dir, "sock") }

// LockPath is the file a holder flocks for its whole life: locked means
// alive, and nothing decides otherwise by a failed connect.
func LockPath(dir string) string { return filepath.Join(dir, "lock") }

// MetaPath is the session's record.
func MetaPath(dir string) string { return filepath.Join(dir, "meta.json") }

// LogPath is the holder's own log.
func LogPath(dir string) string { return filepath.Join(dir, "holder.log") }

// CtlSock is the daemon's socket for the machine's own user.
func CtlSock(terminals string) string { return filepath.Join(terminals, "ctl.sock") }

// DaemonLock is the file the one daemon serving a store flocks.
func DaemonLock(terminals string) string { return filepath.Join(terminals, "daemon.lock") }

// DaemonPIDFile names the daemon holding DaemonLock.
func DaemonPIDFile(terminals string) string { return filepath.Join(terminals, "daemon.pid") }

// MaxSockPath is the longest socket path rota will bind. The kernel's limit
// is a little over a hundred bytes — 104 on macOS, 108 on Linux, counting
// the terminating zero — and a path past it is cut short or refused with an
// error that does not say which path; a round hundred is under both.
const MaxSockPath = 100

// SockFits refuses a socket path too long to bind, by name, so the person
// sees which directory is too deep rather than a bare "invalid argument".
func SockFits(path string) error {
	if len(path) > MaxSockPath {
		return fmt.Errorf("the socket path %s is %d bytes, longer than the %d a unix socket can have; a shorter ROTA_HOME fixes it",
			path, len(path), MaxSockPath)
	}
	return nil
}
