package owner

import (
	"io/fs"
	"runtime"
	"strings"
	"testing"
)

func TestOnlyRefusesWhatOthersCanReadOrWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		if err := Only("/f", 0o666); err != nil {
			t.Fatalf("windows has no such rule: %v", err)
		}
		return
	}
	for _, mode := range []fs.FileMode{0o600, 0o400, 0o700} {
		if err := Only("/f", mode); err != nil {
			t.Fatalf("%04o: %v", mode, err)
		}
	}
	// Writing counts as much as reading: a log others can write is a log
	// others can forge.
	for _, mode := range []fs.FileMode{0o644, 0o640, 0o620, 0o602} {
		err := Only("/f", mode)
		if err == nil || !strings.Contains(err.Error(), "readable or writable by other users") ||
			!strings.Contains(err.Error(), "chmod 600 /f") {
			t.Fatalf("%04o: %v", mode, err)
		}
	}
}
