//go:build !unix

package owner

import "os"

// OpenFlags is nothing where there are no such flags; the caller's check of
// what it opened is all there is.
const OpenFlags = 0

// Blocking has nothing to clear where OpenFlags set nothing.
func Blocking(*os.File) error { return nil }
