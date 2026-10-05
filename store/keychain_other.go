//go:build !darwin

package store

// keychainKept is false everywhere but macOS: Claude Code keeps a home's
// login in .credentials.json alone, and rota never runs anything to look for
// a keychain.
var keychainKept = false
