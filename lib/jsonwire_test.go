package rota

import (
	"encoding/json"
	"testing"
)

// The JSON these types produce is a contract: it is the on-disk account
// store, the HTTP API's replies, and what `rota --json` prints. These tests
// pin the exact bytes so that changing how rota encodes JSON — the move from
// encoding/json to encoding/json/v2, say — has to prove it changed nothing.

func TestAccountEncodesWithoutItsZeroFields(t *testing.T) {
	a := &Account{ID: 2, Provider: "claude", Email: "a@b.c", Order: 1,
		Token: Token{Access: "tok", ExpiresAt: 1700000000000}}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":2,"provider":"claude","email":"a@b.c",` +
		`"token":{"accessToken":"tok","expiresAt":1700000000000},"order":1}`
	if string(raw) != want {
		t.Fatalf("\n got %s\nwant %s", raw, want)
	}
	// And what was omitted must come back as the zero it stood for.
	var back Account
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Dead || back.Delegated || back.Threshold != 0 || back.QuotaAt != 0 {
		t.Fatalf("%+v", back)
	}
}

func TestResultAlwaysReportsHowItEnded(t *testing.T) {
	// A clean run is exactly the case a caller must be able to read, so the
	// zero exit code and the false error flag are never omitted. Nor are the
	// model and effort: an empty effort is the honest answer for a provider
	// that has no such setting, and a missing field would make a client
	// guess whether the run simply did not say.
	raw, err := json.Marshal(&Result{Account: 1, Provider: "claude", Result: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"account":1,"provider":"claude","model":"","effort":"","result":"hi","is_error":false,"exit_code":0}`
	if string(raw) != want {
		t.Fatalf("\n got %s\nwant %s", raw, want)
	}
}

// TestProviderRepliesParseHoweverTheyAreCased holds the line under rota's
// feet: encoding/json matches object names case-insensitively, and every
// provider reply rota parses has relied on that. encoding/json/v2 is
// case-sensitive by default, so a migration that forgets to say otherwise
// silently reads an empty struct instead of failing.
func TestProviderRepliesParseHoweverTheyAreCased(t *testing.T) {
	var tok struct {
		Access  string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	if err := decodeLenient([]byte(`{"Access_Token":"a","EXPIRES_IN":60}`), &tok); err != nil {
		t.Fatal(err)
	}
	if tok.Access != "a" || tok.Expires != 60 {
		t.Fatalf("%+v", tok)
	}
}
