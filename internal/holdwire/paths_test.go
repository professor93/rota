package holdwire

import (
	"path/filepath"
	"strings"
	"testing"
)

// A socket path the kernel would cut short is refused before anything binds
// it, by a message that says which path.
func TestSockFits(t *testing.T) {
	fits := "/" + strings.Repeat("a", MaxSockPath-1)
	if err := SockFits(fits); err != nil {
		t.Fatalf("%d bytes: %v", len(fits), err)
	}
	long := "/" + strings.Repeat("b", MaxSockPath)
	if len(long) != 101 {
		t.Fatalf("the test's own path is %d bytes", len(long))
	}
	err := SockFits(long)
	if err == nil || !strings.Contains(err.Error(), long) {
		t.Fatalf("101 bytes: %v", err)
	}
}

// Every file has one name, inside its session's directory or beside them.
func TestPathsAreWhereTheyAreSaidToBe(t *testing.T) {
	terminals := filepath.Join("store", "terminals")
	dir := Dir(terminals, "0123456789abcdef")
	for got, want := range map[string]string{
		dir:                      filepath.Join(terminals, "0123456789abcdef"),
		SockPath(dir):            filepath.Join(dir, "sock"),
		LockPath(dir):            filepath.Join(dir, "lock"),
		MetaPath(dir):            filepath.Join(dir, "meta.json"),
		LogPath(dir):             filepath.Join(dir, "holder.log"),
		CtlSock(terminals):       filepath.Join(terminals, "ctl.sock"),
		DaemonLock(terminals):    filepath.Join(terminals, "daemon.lock"),
		DaemonPIDFile(terminals): filepath.Join(terminals, "daemon.pid"),
	} {
		if got != want {
			t.Errorf("%s, want %s", got, want)
		}
	}
}

// A session id is what runID makes, and nothing that could leave the
// terminals directory or be mistaken for what lives beside the sessions.
func TestIsIDIsWhatRunIDMakes(t *testing.T) {
	for id, want := range map[string]bool{
		"0123456789abcdef":  true,
		"0123456789ABCDEF":  false,
		"0123456789abcde":   false,
		"0123456789abcdef0": false,
		"0123456789abcdeg":  false,
		"../456789abcdef01": false,
		"ctl.sock":          false,
		"":                  false,
	} {
		if got := IsID(id); got != want {
			t.Errorf("IsID(%q) = %v", id, got)
		}
	}
}
