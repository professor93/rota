package wire

import (
	"strings"
	"testing"
)

// A credential is found where it stands as a word, and only there: a word
// that happens to contain the letters, and the prefixes messages name, are
// left exactly as they were.
func TestSecretsAreFoundOnlyWhereTheyStand(t *testing.T) {
	const key = "sk-ant-api03-TESTONLY-not-a-real-key-0000000000-WXYZ"
	for _, s := range []string{key, "error: " + key + " is bad", `"` + key + `"`, "--cwd=" + key, "/tmp/" + key} {
		if !HoldsSecret(s) {
			t.Fatalf("missed: %q", strings.ReplaceAll(s, key, "<key>"))
		}
		if got := Unsecret(s); strings.Contains(got, "TESTONLY") || !strings.Contains(got, "sk-ant-[not repeated]") {
			t.Fatalf("not taken out: %q", strings.ReplaceAll(got, key, "<key>"))
		}
	}
	for _, s := range []string{
		"task-ant-colony-with-a-long-enough-tail",
		"/home/me/task-ant-colonies-and-more-of-them",
		"a key starts sk-ant-api... and a token sk-ant-oat...",
	} {
		if HoldsSecret(s) || Unsecret(s) != s {
			t.Fatalf("an ordinary string was taken for a key: %q -> %q", s, Unsecret(s))
		}
	}
}
