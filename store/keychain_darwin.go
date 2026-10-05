package store

// keychainKept is whether Claude Code keeps a home's login in the macOS
// keychain before the file beside it. On macOS it does: the item wins when
// both exist, and the file is only read when the item is absent. A variable
// rather than a constant so a test can see either platform's behaviour.
var keychainKept = true
