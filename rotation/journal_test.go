package rotation

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

// refreshingMeter is a metered provider that refreshes, issuing tok.
type refreshingMeter struct {
	meter
	tok *rota.Token
}

func (m refreshingMeter) Refresh(context.Context, *rota.Account) (*rota.Token, error) {
	return m.tok, nil
}

// A token the rotation refreshes while it chooses an account is journalled as
// the choosing's.
func TestARefreshWhileChoosingIsJournalledAsChoose(t *testing.T) {
	rota.Register(refreshingMeter{meter{provider{name: "t-rot-journal"},
		&rota.Quota{Windows: []rota.Window{{Name: "w", Percent: 3, Primary: true}}}},
		&rota.Token{Access: "new", Refresh: "r-new", ExpiresAt: rota.NowMS() + 3_600_000}})
	s := openTemp(t)
	s.Accounts = []*rota.Account{{ID: 1, Provider: "t-rot-journal", Order: 1,
		Token: rota.Token{Access: "old", Refresh: "r-old", ExpiresAt: rota.NowMS() - 1}}}
	if _, err := Choose(context.Background(), s, 0); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.Backend().(*store.FileBackend).Dir, "refresh.log"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range bytes.Lines(raw) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["path"] == "choose.renew" && m["outcome"] == "rotated" {
			return
		}
	}
	t.Fatalf("no choose.renew line:\n%s", raw)
}
