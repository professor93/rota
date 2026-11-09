package holdwire

import (
	"testing"

	"github.com/professor93/rota/internal/proc"
)

// The refusal is one error with one sentence, whichever package gives it, so
// a caller can test for it and a person reads the same words everywhere.
func TestOneSentenceRefusesEveryPlatformWithoutHeldSessions(t *testing.T) {
	if ErrPlatform != proc.ErrPlatform {
		t.Fatal("holdwire and proc refuse with different errors")
	}
	if got, want := ErrPlatform.Error(), "held sessions and the rota daemon run on linux and macOS"; got != want {
		t.Fatalf("the refusal says %q, want %q", got, want)
	}
}
