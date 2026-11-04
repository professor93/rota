package wire

import "regexp"

// anthropicSecret is the shape of an Anthropic credential long enough to be
// a real one, standing as a word of its own: the prefixes alone, as messages
// name them, are too short to match, and a word that merely contains the
// letters — task-ant-colony — is not one.
var anthropicSecret = regexp.MustCompile(`(^|[^A-Za-z0-9])sk-ant-[A-Za-z0-9_-]{16,}`)

// HoldsSecret reports whether s carries something shaped like an Anthropic
// API key or token: a value a transport is about to store as a path, or to
// repeat in a message, that is really a credential typed in the wrong place.
func HoldsSecret(s string) bool { return anthropicSecret.MatchString(s) }

// Unsecret is s with everything shaped like an Anthropic credential taken out,
// for a message somebody wrote without thinking of keys.
func Unsecret(s string) string {
	return anthropicSecret.ReplaceAllString(s, "${1}sk-ant-[not repeated]")
}
