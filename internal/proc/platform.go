package proc

import "errors"

// ErrPlatform is the answer wherever held sessions do not run. It lives here
// because every package that refuses for that reason can reach this one, so
// the sentence is written once: holdwire gives this same error.
var ErrPlatform = errors.New("held sessions and the rota daemon run on linux and macOS")
