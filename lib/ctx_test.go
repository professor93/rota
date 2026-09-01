package rota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Every network verb honors the caller's context. Before this, no exported
// flow accepted one: the hidden client timeout was the only bound, a server
// could not cancel a login when its client hung up, and a test could not
// bound a hang. The proof: a call against a stalled endpoint returns with
// the context's own error, long before the transport would have given up.
func TestACancelledContextStopsANetworkCall(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stall
	}))
	t.Cleanup(func() { close(stall); srv.Close() })
	old := ClaudeEndpoints.Token
	ClaudeEndpoints.Token = srv.URL
	t.Cleanup(func() { ClaudeEndpoints.Token = old })

	a := &Account{ID: 1, Provider: "claude",
		Token: Token{Access: "t", Refresh: "r", ExpiresAt: 1}} // long expired

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	start := time.Now()
	_, err := Refresh(ctx, a)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the caller's deadline must be the one that fires: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation must not wait for the transport")
	}
	if a.Dead {
		t.Fatal("a cancelled call is transient, not a dead credential")
	}
}
