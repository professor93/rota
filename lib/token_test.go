package rota

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func jwtWith(t *testing.T, payload string, pad bool) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	if pad {
		enc = base64.URLEncoding.EncodeToString([]byte(payload))
	}
	return "eyJhbGciOiJub25lIn0." + enc + ".sig"
}

func TestWhenDecodesRFC3339WithFractionAndOffset(t *testing.T) {
	var w When
	if err := json.Unmarshal([]byte(`"2026-08-28T10:11:12.345678+02:00"`), &w); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 28, 8, 11, 12, 345678000, time.UTC)
	if !w.Equal(want) {
		t.Fatalf("got %v want %v", w.Time, want)
	}
}

func TestWhenToleratesEmptyAndGarbage(t *testing.T) {
	for _, in := range []string{`""`, `"yesterday"`, `null`, `42`} {
		var w When
		if err := json.Unmarshal([]byte(in), &w); err != nil {
			t.Fatalf("%s: unexpected error %v", in, err)
		}
		if !w.IsZero() {
			t.Fatalf("%s: expected zero, got %v", in, w.Time)
		}
	}
}

func TestWhenOmittedWhenZero(t *testing.T) {
	raw, err := json.Marshal(Window{Name: "5h"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"name":"5h","percent":0}` {
		t.Fatalf("got %s", raw)
	}
}

func TestIdentityFromJWTAcceptsPaddedAndUnpadded(t *testing.T) {
	for _, pad := range []bool{false, true} {
		id := identityFromJWT(jwtWith(t, `{"email":"a@b.c","sub":"u-1","organization_id":"o-1"}`, pad))
		if id == nil || id.Email != "a@b.c" || id.UUID != "u-1" || id.Org != "o-1" {
			t.Fatalf("pad=%v: got %+v", pad, id)
		}
	}
}

func TestIdentityFromJWTFallsBackToUserIDAndRejectsJunk(t *testing.T) {
	if id := identityFromJWT(jwtWith(t, `{"user_id":"k-9"}`, false)); id == nil || id.UUID != "k-9" {
		t.Fatalf("got %+v", id)
	}
	for _, junk := range []string{"", "abc", "a.b", "a.!!!.c", jwtWith(t, `{"foo":1}`, false), jwtWith(t, `not json`, false)} {
		if id := identityFromJWT(junk); id != nil {
			t.Fatalf("%q: expected nil, got %+v", junk, id)
		}
	}
}

func TestJWTExpiryAndChatGPTAccount(t *testing.T) {
	tok := jwtWith(t, `{"exp":1700000000,"https://api.openai.com/auth":{"chatgpt_account_id":"acct-7"}}`, false)
	if got := jwtExpiryMS(tok); got != 1700000000000 {
		t.Fatalf("exp: got %d", got)
	}
	if got := chatgptAccountID(tok); got != "acct-7" {
		t.Fatalf("account: got %q", got)
	}
	if jwtExpiryMS("junk") != 0 || chatgptAccountID("junk") != "" {
		t.Fatal("junk must yield zero values")
	}
}

func TestFingerprintIsShortStableAndDistinct(t *testing.T) {
	a, b := fingerprint("refresh-token-A"), fingerprint("refresh-token-B")
	if len(a) != 16 || a == b || a != fingerprint("refresh-token-A") {
		t.Fatalf("a=%q b=%q", a, b)
	}
	if fingerprint("") != "" {
		t.Fatal("empty input must fingerprint to empty")
	}
}
