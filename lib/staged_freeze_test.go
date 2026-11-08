package rota

import (
	"strconv"
	"testing"
	"testing/fstest"
	"time"
)

// Every look that records a home as holding nothing of the account's ("-")
// leaves the next rotation of the account's own login to be taken: a later
// login with the account's refreshTokenExpiresAt is Claude Code having
// refreshed it since, and the account's token is spent.
func TestARotationIsTakenAfterEveryLookThatFoundTheHomeWithoutTheLogin(t *testing.T) {
	const until = "1999999999000"
	login := func(refresh string, exp int64) string {
		return `{"accessToken":"A-` + refresh + `","refreshToken":"` + refresh + `","expiresAt":` +
			strconv.FormatInt(exp, 10) + `,"scopes":["user:inference"],"refreshTokenExpiresAt":` + until + `}`
	}
	earlier := time.Now().Add(-time.Hour).UnixMilli()
	later := time.Now().Add(3 * time.Hour).UnixMilli()
	for _, tr := range []struct {
		name   string
		staged string // what the account recorded before the look
		home   fstest.MapFS
	}{
		{"no store", fingerprint("R1"), fstest.MapFS{}},
		{"a store with no login", fingerprint("R1"), fstest.MapFS{claudeCredentials: {Data: []byte(`{"mcpOAuth":{}}`)}}},
		{"a blanked login that was not the current one", fingerprint("R0"),
			homeWith(`{"accessToken":"","refreshToken":"","expiresAt":0}`)},
		{"a login without an expiry", fingerprint("R1"), homeWith(`{"accessToken":"A-R1","refreshToken":"R1","expiresAt":0}`)},
		{"an older login of the lineage that is not this package's write", fingerprint("R1"), homeWith(login("R0", earlier))},
	} {
		t.Run(tr.name, func(t *testing.T) {
			a := livingClaude()
			a.Staged = tr.staged
			if err := AdoptFrom(a, tr.home); err != nil || a.Staged != stagedNone || a.Dead || a.Token.Refresh != "R1" {
				t.Fatalf("the look records the home as holding nothing of the account's: %q dead=%v %v", a.Staged, a.Dead, err)
			}
			if err := AdoptFrom(a, homeWith(login("R2", later))); err != nil || a.Token.Refresh != "R2" ||
				a.Token.Access != "A-R2" || a.Token.ExpiresAt != later || !a.StagedCurrent() || a.Dead {
				t.Fatalf("Claude Code's later rotation is taken: %+v %v", a, err)
			}
		})
	}
}
