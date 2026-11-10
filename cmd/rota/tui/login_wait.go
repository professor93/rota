package tui

// Task 16 fills this file in: the device login, which polls by itself, and
// the delegated one, which hands the terminal to the provider's CLI. Until
// then each step says "not yet".

// newDeviceStep waits for l, a device login, to be approved as who.
func newDeviceStep(e *env, l *backendLogin, who string) step { return overlayNotYet() }

// newDelegatedStep signs account in through its own CLI.
func newDelegatedStep(e *env, account int) step { return overlayNotYet() }
