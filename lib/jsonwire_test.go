package rota

import (
	"bytes"
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

// TestTheSameValueIsAlwaysTheSameBytes holds what encoding/json always did and
// v2 does not by default: a map's keys are written sorted, so encoding a
// value twice gives the same bytes. Without it the order follows the map's
// own iteration, which Go changes from one run to the next. A struct is the
// opposite case and is pinned with it: its fields keep the order they are
// declared in, never sorted.
func TestTheSameValueIsAlwaysTheSameBytes(t *testing.T) {
	type entry struct {
		Zeta  string         `json:"zeta"`
		Alpha map[int]string `json:"alpha"`
	}
	v := map[string]any{
		"warnings": []string{"w"}, "accounts": []int{1}, "sessions": map[string]int{"s2": 2, "s1": 1, "s10": 10},
		"default": 1, "instances": nil, "notes": "n", "shared": true, "zz": map[string]any{"b": 1, "a": 2, "c": 3},
		"entry": entry{Zeta: "z", Alpha: map[int]string{2: "two", 10: "ten", 1: "one"}},
	}
	const want = `{"accounts":[1],"default":1,` +
		`"entry":{"zeta":"z","alpha":{"1":"one","10":"ten","2":"two"}},` +
		`"instances":null,"notes":"n","sessions":{"s1":1,"s10":10,"s2":2},` +
		`"shared":true,"warnings":["w"],"zz":{"a":2,"b":1,"c":3}}`

	var buf bytes.Buffer
	for i := range 50 {
		raw, err := Encode(v)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != want {
			t.Fatalf("Encode, try %d:\n got %s\nwant %s", i, raw, want)
		}
		indented, err := EncodeIndent(v)
		if err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, indented); err != nil || compact.String() != want {
			t.Fatalf("EncodeIndent, try %d, is not Encode indented: %v\n%s", i, err, indented)
		}
		buf.Reset()
		if err := EncodeTo(&buf, v); err != nil {
			t.Fatal(err)
		}
		if buf.String() != want+"\n" {
			t.Fatalf("EncodeTo, try %d:\n got %s", i, buf.Bytes())
		}
	}
}
