//go:build !linux && !darwin

package hold

import (
	"fmt"
	"io"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/store"
)

// Main refuses where there is no pseudo-terminal to hold a session on.
func Main(_ io.Reader, out io.Writer, _ func() (*store.Store, error)) int {
	fmt.Fprintln(out, holdwire.ErrPlatform)
	return 1
}
