package store

import (
	"os"
	"slices"
	"strings"
	"sync"
)

// hidden are the variables no vendor CLI may inherit. The child of a run is
// a coding agent with a shell: an environment it can print is an environment
// it can leak. This registry lives in store rather than in the SDK because
// which variables are secret is the application's fact — the SDK never reads
// the environment at all.
//
// ROTA_HOME is seeded because this package defines it (DefaultDir): it names
// the file the refresh tokens live in. The command adds ROTA_TOKEN, which
// authorizes running every account over HTTP. The lock is for the shape of
// the thing rather than for contention: names are registered while a program
// starts, but a server reaching its first run mid-registration would
// otherwise be a race, and a race here loses a secret.
var (
	hidden   = []string{"ROTA_HOME"}
	hiddenMu sync.RWMutex
)

// HideFromAgents records variables no child process may inherit. The package
// that defines a secret variable should register it, so a program cannot
// gain a secret without also declaring it. Repeats are harmless.
func HideFromAgents(names ...string) {
	hiddenMu.Lock()
	defer hiddenMu.Unlock()
	for _, n := range names {
		if n != "" && !slices.Contains(hidden, n) {
			hidden = append(hidden, n)
		}
	}
}

// HostEnv is this process's environment with every registered secret
// removed: the right base environment for anything that launches a vendor
// CLI. It is computed fresh on each call, so a variable set after startup is
// still seen — and still scrubbed.
func HostEnv() []string {
	hiddenMu.RLock()
	defer hiddenMu.RUnlock()
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if !slices.Contains(hidden, k) {
			out = append(out, e)
		}
	}
	return out
}
