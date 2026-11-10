//go:build !linux && !darwin

package main

import "github.com/professor93/rota/internal/holdwire"

// The daemon runs on linux and macOS, as the sessions it holds do. Here each
// of its verbs refuses in the sentence every held-session command uses, and
// foreground `rota serve` is the server it always was.

func (c *cli) startDaemon(bool, string) error { return holdwire.ErrPlatform }

func (c *cli) spawnDaemon(bool, string) (*daemonStart, error) { return nil, holdwire.ErrPlatform }

func (c *cli) runDaemon(bool, string) error { return holdwire.ErrPlatform }

func (c *cli) stopDaemon(bool, bool) error { return holdwire.ErrPlatform }

// refusedInsideHeldSession refuses nothing here: there are no held sessions
// to be inside.
func refusedInsideHeldSession(string, string) error { return nil }

func (c *cli) daemonStatus() error { return holdwire.ErrPlatform }
