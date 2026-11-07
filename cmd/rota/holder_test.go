package main

import "os"

// A holder is this test binary answering to rota's command line, which it
// does only with ROTA_AS_RUN=1 in its environment: without it, the binary
// started as `rota serve claim` would run every test again. So a handover in
// a test lends its claim where the test asked for that, and otherwise keeps
// it across its exec, as where no holder can be started.
func init() {
	startHolder = func(files []*os.File) (claimHolder, error) {
		if os.Getenv("ROTA_AS_RUN") != "1" {
			return nil, errNoHolder
		}
		return holdInBackground(files)
	}
}
