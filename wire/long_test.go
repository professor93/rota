package wire

import (
	"encoding/json"
	"strconv"
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

// The STATUS cell says when a login ends only in its last three days, in the
// fewest words that will do, and says it is over once it is. A dead account has
// its own reason to give, and one with no known end has nothing to say.
func TestTheStatusCellSaysWhenALoginEnds(t *testing.T) {
	at := func(d time.Duration) map[string]string {
		return map[string]string{"refresh_token_expires_at": strconv.FormatInt(time.Now().Add(d).UnixMilli(), 10)}
	}
	for _, c := range []struct {
		left time.Duration
		says string
	}{
		{30 * 24 * time.Hour, ""},
		{4 * 24 * time.Hour, ""},
		{loginClose + time.Minute, ""},
		{loginClose - time.Minute, "login ends in 2d"},
		{36 * time.Hour, "login ends in 1d"},
		{5*time.Hour + time.Minute, "login ends in 5h"},
		{20*time.Minute + time.Second, "login ends in 20m"},
		{time.Second, "login ends in 1m"},
		{-time.Minute - time.Second, "login ended 1m ago"},
		{-3*time.Hour - time.Minute, "login ended 3h ago"},
		{-49 * time.Hour, "login ended 2d ago"},
	} {
		a := &rota.Account{ID: 1, Provider: "claude", Extra: at(c.left)}
		if got := LoginStatus(a); got != c.says {
			t.Fatalf("%v left: %q, want %q", c.left, got, c.says)
		}
	}
	if got := LoginStatus(&rota.Account{ID: 1, Provider: "claude"}); got != "" {
		t.Fatalf("unknown is unsaid: %q", got)
	}
	if got := LoginStatus(&rota.Account{ID: 1, Provider: "claude", Dead: true, Extra: at(time.Hour)}); got != "" {
		t.Fatalf("a dead account says why it is dead: %q", got)
	}
}

// When the login itself ends is shown beside the long token's date, and said
// in a listing only in its last few days.
func TestWhenTheLoginEndsIsShownAndSaidOnlyNearTheEnd(t *testing.T) {
	a := &rota.Account{ID: 1, Provider: "claude"}
	if Describe(a).LoginUntil != "" || LoginNote(a) != "" {
		t.Fatal("unknown is unsaid")
	}
	at := func(d time.Duration) map[string]string {
		return map[string]string{"refresh_token_expires_at": strconv.FormatInt(time.Now().Add(d).UnixMilli(), 10)}
	}
	a.Extra = at(30 * 24 * time.Hour)
	if Describe(a).LoginUntil == "" || LoginNote(a) != "" {
		t.Fatal("a month away is shown and not said")
	}
	// Halfway through a day, not on its edge: exactly three days left is
	// rightly "in 3 days", and on a clock that reads whole microseconds
	// against an expiry kept in milliseconds, no time passing is common.
	a.Extra = at(2*24*time.Hour + 12*time.Hour)
	if note := LoginNote(a); !strings.Contains(note, "in 2 days") {
		t.Fatal(note)
	}
	a.Extra = at(-time.Hour)
	if note := LoginNote(a); !strings.Contains(note, "ended") {
		t.Fatal(note)
	}
}
