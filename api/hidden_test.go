package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The HTTP login surface hides the same provider the command line does,
// and the schema says so, so a generated form can leave it out while an
// account already on it still renders.
func TestHiddenProviderIsRefusedByLoginAndMarkedInTheSchema(t *testing.T) {
	h := newHarness(t, Options{})
	resp, raw := h.do("POST", "/v1/login", map[string]any{"provider": "kimi"})
	if resp.StatusCode != 400 || !strings.Contains(string(raw), "not offered") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	resp, raw = h.do("GET", "/v1/schema", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var doc struct {
		Providers map[string]struct {
			Hidden bool `json:"hidden"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Providers["kimi"].Hidden || doc.Providers["claude"].Hidden {
		t.Fatalf("%+v", doc.Providers)
	}
}
