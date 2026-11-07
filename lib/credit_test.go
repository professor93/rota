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
// of it and by nothing else: its own result has to call the run an error, and
// its answer has to be the refusal, not something that quotes it.
func TestARunOutOfCreditIsToldApart(t *testing.T) {
	read := func(provider, line string) *Result {
		res := &Result{Provider: provider}
		absorb([]byte(line), false, caps{}, res)
		return res
	}
	if res := read("claude", creditRefusal); !res.OutOfCredit() {
		t.Fatalf("Claude Code's own verdict: %+v", res)
	}
	apiWords := `{"type":"result","subtype":"success","is_error":true,"result":"API Error: 400 Your credit balance is too low ` +
		`to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}`
	if !read("claude", apiWords).OutOfCredit() {
		t.Fatal("the API's own words, passed on")
	}
	quoted := `{"type":"result","subtype":"success","is_error":false,"num_turns":1,` +
		`"result":"Your app shows \"Credit balance is too low\" because the org has no prepaid credit."}`
	for name, res := range map[string]*Result{
		"an answer about credit": read("claude", quoted),
		"another refusal":        read("claude", `{"type":"result","is_error":true,"result":"Prompt is too long"}`),
		"a refusal that quotes it": read("claude",
			`{"type":"result","is_error":true,"result":"Error: Credit balance is too low? retry later"}`),
		"another CLI": read("codex", creditRefusal),
		"no result":   nil,
	} {
		if res.OutOfCredit() {
			t.Fatalf("%s: counted as out of credit", name)
		}
	}
	// rota's own verdict over an answer the CLI called a success: a non-zero
	// exit, or a limit rota stopped the run for, as finishRun records them.
	for name, mark := range map[string]func(*Result){
		"exit 1":        func(r *Result) { r.IsError, r.ExitCode = true, 1 },
		"stopped early": func(r *Result) { r.IsError, r.ExitCode, r.Truncated = true, -1, true },
	} {
		res := read("claude", quoted)
		mark(res)
		if res.OutOfCredit() {
			t.Fatalf("%s: rota's own verdict is not the CLI's", name)
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
