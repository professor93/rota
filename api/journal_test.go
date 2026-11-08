package api

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// apiRefresher is a metered provider that refreshes, for the journal test.
type apiRefresher struct{ apiKeyProvider }

func (apiRefresher) Name() string { return "t-api-journal" }
func (apiRefresher) Refresh(context.Context, *rota.Account) (*rota.Token, error) {
	return &rota.Token{Access: "new", Refresh: "r-new", ExpiresAt: rota.NowMS() + 3_600_000}, nil
}
func (apiRefresher) Quota(context.Context, string) (*rota.Quota, error) {
	return &rota.Quota{Windows: []rota.Window{{Name: "w", Percent: 3, Primary: true}}}, nil
}

// A token the server refreshes for ?refresh=1 is journalled as the API's.
func TestARefreshForTheAccountListIsJournalledAsTheAPIs(t *testing.T) {
	rota.Register(apiRefresher{})
	h := newHarness(t, Options{})
	st, err := store.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Accounts = append(st.Accounts, &rota.Account{ID: 9, Provider: "t-api-journal", Order: 9,
		Token: rota.Token{Access: "old", Refresh: "r-old", ExpiresAt: rota.NowMS() - 1}})
	err = st.Save()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp, raw := h.do("GET", "/v1/accounts?refresh=1", nil); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	raw, err := os.ReadFile(filepath.Join(h.dir, "refresh.log"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range bytes.Lines(raw) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["path"] == "api.renew" && m["outcome"] == "rotated" {
			return
		}
	}
	t.Fatalf("no api.renew line:\n%s", raw)
}
