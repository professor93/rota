package askmodel

import (
	"context"
	"testing"
)

// A request that names no model, made inside one that named one, carries no
// model: the outer request's does not leak into it.
func TestARequestThatNamesNoModelCarriesNone(t *testing.T) {
	outer := With(context.Background(), "fable")
	if From(outer) != "fable" {
		t.Fatalf("the model named: %q", From(outer))
	}
	if got := From(With(outer, "")); got != "" {
		t.Fatalf("a nested request that names none carries %q", got)
	}
	if got := From(context.Background()); got != "" {
		t.Fatalf("a context nobody gave one: %q", got)
	}
}
