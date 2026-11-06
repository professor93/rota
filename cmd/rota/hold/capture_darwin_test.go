package hold

import "syscall"

// The terminal settings ioctls, as the BSDs spell them.
const getTermios, setTermios = syscall.TIOCGETA, syscall.TIOCSETA
