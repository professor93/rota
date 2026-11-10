package proc

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// linuxDomain is a Linux pid space as Claude Code names it.
const linuxDomain = "linux:5f0c2a1e7b3d4e8a9c612d4f8a0b3e17:pid:[4026531836]"

// This machine's pid space is named as Claude Code's build for this system
// names it.
func TestThisMachinesPidSpaceIsNamedAsClaudeCodeNamesIt(t *testing.T) {
	switch got := PidDomain(); runtime.GOOS {
	case "darwin":
		if got != "darwin" {
			t.Fatalf("macOS: %q", got)
		}
	case "linux":
		id, err := os.ReadFile("/etc/machine-id")
		if err != nil {
			id = nil
		}
		ns, err := os.Readlink("/proc/self/ns/pid")
		if err != nil {
			ns = ""
		}
		if want := "linux:" + strings.TrimSpace(string(id)) + ":" + ns; got != want {
			t.Fatalf("linux: %q, want %q", got, want)
		}
	default:
		if got != "" {
			t.Fatalf("%s: %q", runtime.GOOS, got)
		}
	}
}

// On Linux the pid space is "linux:", the machine id with its newline
// trimmed, ":", and the link /proc/self/ns/pid makes, each part empty when
// it cannot be read, exactly as Claude Code's Linux build (2.1.319) makes
// it. On macOS it is "darwin" whatever the files say; elsewhere it is
// unknown.
func TestThePidSpaceIsMadeAsClaudeCodeMakesIt(t *testing.T) {
	files := map[string]string{"/etc/machine-id": "5f0c2a1e7b3d4e8a9c612d4f8a0b3e17\n"}
	links := map[string]string{"/proc/self/ns/pid": "pid:[4026531836]"}
	read := func(name string) ([]byte, error) {
		if body, ok := files[name]; ok {
			return []byte(body), nil
		}
		return nil, os.ErrNotExist
	}
	link := func(name string) (string, error) {
		if to, ok := links[name]; ok {
			return to, nil
		}
		return "", os.ErrPermission
	}
	if got := pidDomainOn("linux", read, link); got != linuxDomain {
		t.Fatalf("linux: %q, want %q", got, linuxDomain)
	}
	delete(files, "/etc/machine-id")
	if got := pidDomainOn("linux", read, link); got != "linux::pid:[4026531836]" {
		t.Fatalf("linux with no machine id: %q", got)
	}
	delete(links, "/proc/self/ns/pid")
	if got := pidDomainOn("linux", read, link); got != "linux::" {
		t.Fatalf("linux with nothing to read: %q", got)
	}
	if got := pidDomainOn("darwin", read, link); got != "darwin" {
		t.Fatalf("macOS: %q", got)
	}
	if got := pidDomainOn("windows", read, link); got != "" {
		t.Fatalf("windows: %q", got)
	}
}
