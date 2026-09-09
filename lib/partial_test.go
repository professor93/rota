package rota

import (
	"errors"
	"strings"
	"testing"
)

// A fragment is a piece of a stream, and the CLI refuses to send fragments
// into a buffered run. The refusal is rota's, before anything is spent, and
// it names the field a caller has to add — rather than quietly switching
// the reply to a stream a caller did not ask for.
func TestPartialMessagesNeedAStream(t *testing.T) {
	err := Spec{Prompt: "hi", IncludePartialMessages: true}.Check("claude", nil)
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "include_partial_messages") || !strings.Contains(err.Error(), "stream") {
		t.Fatalf("refused by name, naming what is missing: %v", err)
	}
	if err := (Spec{Prompt: "hi", Stream: true, IncludePartialMessages: true}).Check("claude", nil); err != nil {
		t.Fatalf("with a stream it is allowed: %v", err)
	}
}
