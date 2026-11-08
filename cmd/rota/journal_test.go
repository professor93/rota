package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// cliRefresher is a metered provider that refreshes, for the journal test.
type cliRefresher struct{ cliFakeProvider }

func (cliRefresher) Name() string { return "t-cli-journal" }
func (cliRefresher) Refresh(context.Context, *rota.Account) (*rota.Token, error) {
	return &rota.Token{Access: "new", Refresh: "r-new", ExpiresAt: rota.NowMS() + 3_600_000}, nil
}
func (cliRefresher) Quota(context.Context, string) (*rota.Quota, error) {
	return &rota.Quota{Windows: []rota.Window{{Name: "w", Percent: 3, Primary: true}}}, nil
}

func init() { rota.Register(cliRefresher{}) }

// A token `rota list` refreshes to read its usage is journalled as the
// listing's.
func TestARefreshForAListingIsJournalledAsTheListings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"accounts":[{"id":1,"provider":"t-cli-journal","order":1,`+
		`"token":{"accessToken":"old","refreshToken":"r-old","expiresAt":1}}]}`)
	if _, stderr, code := call(t, "list"); code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(home, "refresh.log"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range bytes.Lines(raw) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["path"] == "list.renew" && m["outcome"] == "rotated" {
			return
		}
	}
	t.Fatalf("no list.renew line:\n%s", raw)
}
