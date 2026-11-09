package api

import (
	"net/http/httptest"
	"testing"

	"github.com/professor93/rota/store"
)

// A choice that is refused does not keep the store's file lock. (The panic
// path shares the same deferred close; no seam in the choice can be made to
// panic without a production hook.)
func TestARefusedChoiceLetsGoOfTheStore(t *testing.T) {
	h := newHarness(t, Options{})
	r := httptest.NewRequest("POST", "/", nil)
	for _, id := range []int{99, 4} { // not there; dead
		if st, _, err := h.api.chooseTerminal(r, termKindAccount, id); err == nil {
			st.Close()
			t.Fatalf("account %d was chosen", id)
		}
		st, err := store.Open(h.dir)
		if err != nil {
			t.Fatalf("account %d: the store is still locked: %v", id, err)
		}
		st.Close()
	}
}
