//go:build !linux && !darwin

package main

import "github.com/professor93/rota/internal/holdwire"

// There is no daemon here to supervise: install and uninstall refuse in the
// sentence every held-session command uses.

func (c *cli) installDaemon() error { return holdwire.ErrPlatform }

func (c *cli) uninstallDaemon() error { return holdwire.ErrPlatform }
