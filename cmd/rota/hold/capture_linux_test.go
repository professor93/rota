package hold

import "syscall"

// The terminal settings ioctls, as linux spells them.
const getTermios, setTermios = syscall.TCGETS, syscall.TCSETS
