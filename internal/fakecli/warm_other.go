//go:build !darwin

package fakecli

// warm has nothing to do where no policy daemon looks at a new executable
// before it may run.
func warm(string) error { return nil }
