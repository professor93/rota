//go:build !linux && !darwin

package main

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/professor93/rota/internal/holdwire"
)

// There is no daemon here, so there is no control socket either, and asking
// for one is refused in the sentence every held-session command uses.
func TestCtlSockIsRefusedHere(t *testing.T) {
	stop, err := listenCtl(nil, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if stop != nil || !errors.Is(err, holdwire.ErrPlatform) {
		t.Fatalf("listenCtl answered %v, want %v", err, holdwire.ErrPlatform)
	}
}
