package main

import "github.com/professor93/rota/cmd/rota/hold"

// serveHold is `rota serve hold`: one held session's process, started by the
// daemon with the session's request on standard input. It takes no argument,
// since an argument can hold a prompt and argv is anybody's to read, and its
// exit status is its own: the daemon reads why from its one line of answer.
func (c *cli) serveHold(args []string) error {
	if len(args) > 0 {
		return usageErr("serve hold takes no arguments; the daemon writes its request to standard input")
	}
	return exitCode(hold.Main(c.in, c.out, c.openStore))
}
