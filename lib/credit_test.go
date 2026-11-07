package rota

import (
	"context"
	"testing"
	"time"
)

// What Claude Code prints, in print mode, for a run the API refused because
// the Console organisation behind the key has no credit left: the answer is
// its own sentence for the API's refusal, and the result is an error.
const creditRefusal = `{"type":"result","subtype":"success","is_error":true,"api_error_status":400,` +
	`"duration_ms":312,"num_turns":1,"result":"Credit balance is too low","stop_reason":"stop_sequence",` +
	`"session_id":"s-credit","total_cost_usd":0}`

// A run that ended for want of credit is told apart by what Claude Code says
// and by nothing that merely mentions it: the answer has to be the error, and
// it has to be Claude Code's.
func TestARunOutOfCreditIsToldApart(t *testing.T) {
	var buffered Result
	absorb([]byte(creditRefusal), false, caps{}, &buffered)
	buffered.Provider = "claude"
	if !buffered.OutOfCredit() {
		t.Fatalf("Claude Code's own verdict: %+v", buffered)
	}
	for name, c := range map[string]struct {
		res  *Result
		want bool
	}{
		"the API's own words": {&Result{Provider: "claude", IsError: true,
			Result: "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}, true},
		"an answer about credit": {&Result{Provider: "claude", Result: "Your credit balance is too low, it says."}, false},
		"another refusal":        {&Result{Provider: "claude", IsError: true, Result: "Prompt is too long"}, false},
		"another CLI":            {&Result{Provider: "codex", IsError: true, Result: "Credit balance is too low"}, false},
		"no result":              {nil, false},
	} {
		if got := c.res.OutOfCredit(); got != c.want {
			t.Fatalf("%s: got %v", name, got)
		}
	}
}

// An account a run found out of credit says so until the time the
// application chose, and then is an account like any other again; a key
// pasted again is a fresh start.
func TestAnAccountOutOfCreditIsLimitedUntilItsPauseEnds(t *testing.T) {
	a := keyAccount(t)
	if a.OutOfCredit() || a.Status() != StatusOK {
		t.Fatalf("a new key: %v %s", a.OutOfCredit(), a.Status())
	}
	a.NoCreditUntil = Now().Add(time.Hour).UnixMilli()
	if !a.OutOfCredit() || a.Status() != StatusLimited {
		t.Fatalf("while it lasts: %v %s", a.OutOfCredit(), a.Status())
	}
	a.NoCreditUntil = Now().Add(-time.Minute).UnixMilli()
	if a.OutOfCredit() || a.Status() != StatusOK {
		t.Fatalf("once it is over: %v %s", a.OutOfCredit(), a.Status())
	}

	a.NoCreditUntil = Now().Add(time.Hour).UnixMilli()
	tok, _, err := Paste(context.Background(), "claude", testKey)
	if err != nil {
		t.Fatal(err)
	}
	a.Apply(tok)
	if a.OutOfCredit() || a.NoCreditUntil != 0 {
		t.Fatalf("the key pasted again forgets the verdict: %d", a.NoCreditUntil)
	}
}
