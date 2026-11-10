package proc

import (
	"os"
	"runtime"
	"strings"
)

// PidDomain is this machine's pid space as Claude Code names it in the
// pidDomain of the records it writes, so that a record's pid is known to
// name a process here: "darwin" on macOS, and on Linux
// "linux:<machine-id>:<pid namespace>", the namespace as /proc/self/ns/pid
// links it, such as "pid:[4026531836]", each part empty when it cannot be
// read, as Claude Code leaves it. Elsewhere rota does not know it, and it
// is empty.
func PidDomain() string { return pidDomainOn(runtime.GOOS, os.ReadFile, os.Readlink) }

// pidDomainOn is PidDomain for the system goos, reading files with read
// and links with link.
func pidDomainOn(goos string, read func(string) ([]byte, error), link func(string) (string, error)) string {
	switch goos {
	case "darwin":
		return "darwin"
	case "linux":
		id, err := read("/etc/machine-id")
		if err != nil {
			id = nil
		}
		ns, err := link("/proc/self/ns/pid")
		if err != nil {
			ns = ""
		}
		return "linux:" + strings.TrimSpace(string(id)) + ":" + ns
	}
	return ""
}
