package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// A long-lived token is the most valuable thing an account holds, and every
// rendering of that account carries its date and nothing else of it.
func TestDescribeCarriesTheLongTokensDateAndNeverTheToken(t *testing.T) {
	a := &rota.Account{ID: 1, Provider: "claude"}
	if v := Describe(a); v.LongUntil != "" || LongNote(a) != "" {
		t.Fatalf("an account with no long token says nothing about one: %+v", v)
	}
	until := time.Now().Add(200 * 24 * time.Hour)
	a.Long = &rota.LongToken{Access: "LONG-SECRET", ExpiresAt: until.UnixMilli()}
	raw, err := json.Marshal(Describe(a))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "LONG-SECRET") {
		t.Fatalf("the token itself must never be rendered: %s", raw)
	}
	when, err := time.Parse(time.RFC3339, Describe(a).LongUntil)
	if err != nil || when.Unix() != until.Unix() {
		t.Fatalf("long_until must be the expiry in RFC 3339: %q (%v)", Describe(a).LongUntil, err)
	}
	if LongNote(a) != "" {
		t.Fatalf("most of a year left is nothing worth mentioning: %q", LongNote(a))
	}

	for _, c := range []struct {
		what string
		left time.Duration
		says string
	}{
		{"a month out", LongSoon - time.Hour, "expires on"},
		{"already gone", -time.Hour, "expired on"},
	} {
		a.Long.ExpiresAt = time.Now().Add(c.left).UnixMilli()
		note := LongNote(a)
		if !strings.Contains(note, c.says) || !strings.Contains(note, "rota login --long") {
			t.Fatalf("%s: %q", c.what, note)
		}
		if strings.Contains(note, "LONG-SECRET") {
			t.Fatalf("%s: the note carried the token: %q", c.what, note)
		}
	}
}
