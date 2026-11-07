//go:build !linux && !darwin

package main

import (
	"log/slog"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/holdwire"
)

// listenCtl has nothing to listen for here: the daemon and its held sessions
// run on linux and macOS, and so does the socket that reaches them.
func listenCtl(*api.Server, string, *slog.Logger) (func(), error) {
	return nil, holdwire.ErrPlatform
}
