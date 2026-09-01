package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// What a request actually costs, so the router's share of it is visible.
// Routing and token checking are nanoseconds; opening the account store is
// microseconds; running an agent is seconds.

func benchRequest(b *testing.B, h http.Handler, method, path string) {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			b.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
}

func BenchmarkHello(b *testing.B) {
	h := newHarness(b, Options{})
	benchRequest(b, h.handler, "GET", "/")
}

func BenchmarkListAccounts(b *testing.B) {
	h := newHarness(b, Options{})
	benchRequest(b, h.handler, "GET", "/v1/accounts")
}

func BenchmarkSchema(b *testing.B) {
	h := newHarness(b, Options{})
	benchRequest(b, h.handler, "GET", "/v1/schema")
}
