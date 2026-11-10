//go:build !linux && !darwin

package attach

import (
	"os"

	"github.com/professor93/rota/internal/holdwire"
)

// Local refuses where rota holds no sessions: an attach needs the daemon,
// which runs on linux and macOS, so there is no terminal here to make raw.
func Local(in, out *os.File) (t Terminal, restore func(), err error) {
	return nil, nil, holdwire.ErrPlatform
}
