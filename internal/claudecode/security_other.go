//go:build !darwin

package claudecode

import (
	"context"
	"errors"
)

// runSecurity has nothing to run outside macOS: Claude Code keeps its login
// in a file there, and rota never asks for a keychain.
func runSecurity(context.Context, ...string) ([]byte, int, error) {
	return nil, 0, errors.New("there is no macOS keychain on this platform")
}
