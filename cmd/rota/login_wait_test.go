package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// typing makes the command's standard input a terminal this test types into:
// a pipe, with the seam that asks whether it is a terminal saying yes. What is
// written before the command reads is already there when it does, and closing
// the writer is the person pressing ^D.
func typing(t *testing.T, lines string) (w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, lines); err != nil {
		t.Fatal(err)
	}
	wasIn, wasTerm := stdin, loginTerminal
	stdin, loginTerminal = r, func(io.Reader) bool { return true }
	t.Cleanup(func() {
		stdin, loginTerminal = wasIn, wasTerm
		w.Close()
		r.Close()
	})
	return w
}

// approvalsByCode is a fake Claude token endpoint whose approver is whoever
// the code typed names; a code it does not know is rejected, as a mistyped
// one is.
func approvalsByCode(t *testing.T, who map[string]approver) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/profile") {
			w.WriteHeader(500)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		code, _ := body["code"].(string)
		a, ok := who[code]
		if !ok {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Invalid authorization code"})
			return
		}
		doc := map[string]any{"access_token": "NEW-" + a.uuid, "refresh_token": "R-" + a.uuid, "expires_in": 3600}
		if a.uuid != "" {
			doc["account"] = map[string]string{"uuid": a.uuid, "email_address": a.email}
		}
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	for _, e := range []*string{&rota.ClaudeEndpoints.Token, &rota.ClaudeEndpoints.Profile} {
		old := *e
		t.Cleanup(func() { *e = old })
	}
	rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile = srv.URL+"/token", srv.URL+"/profile"
}

// duringWait runs f once the command is waiting, in the command's own
// goroutine, before anything is read.
func duringWait(t *testing.T, f func(ctx context.Context)) {
	t.Helper()
	awaitBegun = f
	t.Cleanup(func() { awaitBegun = nil })
}

var known = map[string]approver{
	"AS12": {"u12", "a@b.c"}, "AS13": {"u13", "d@e.f"}, "STRANGER": {"u-new", "new@x.y"},
	"NOBODY": {}, "RECREATED": {"u-recreated", "a@b.c"},
}

func TestLoginOnATerminalWaitsForTheCodeAndTakesAnotherAfterAWrongOne(t *testing.T) {
	home := seedTwoClaude(t)
	approvalsByCode(t, known)
	typing(t, "\nWRONG\nSTRANGER\n")
	out, errOut, code := call(t, "login")
	if code != 0 || !strings.Contains(out, "Added claude account 14") {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "Open this and approve") || !strings.Contains(out, "Then: rota login ") {
		t.Fatalf("what a login prints is printed first: %q", out)
	}
	if strings.Count(errOut, "m left") < 3 || !strings.Contains(errOut, "authorization code was rejected") {
		t.Fatalf("a prompt for each line, the time left in it, and the rejection: %q", errOut)
	}
	if p := parked(t, home); p != "" {
		t.Fatalf("finished, and still parked: %s", p)
	}
}

// Approving as the wrong account is the likely mistake, and the same login
// goes on to finish once the right one approves.
func TestLoginByIDOnATerminalTakesTheRightApprovalAfterRefusals(t *testing.T) {
	home := seedTwoClaude(t)
	approvalsByCode(t, known)
	typing(t, "AS13\nSTRANGER\nNOBODY\nAS12\n")
	out, errOut, code := call(t, "login", "12")
	if code != 0 || !strings.Contains(out, "Refreshed claude account 12") {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"which is account 13", "new@x.y", "named no account"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal %q was not shown: %q", want, errOut)
		}
	}
	d := readAccounts(t, home)
	if len(d.Accounts) != 2 || d.NextID != 14 || d.Accounts[0].Dead || d.Accounts[1].Token.Access != "OLD-13" {
		t.Fatalf("%+v", d)
	}
	if p := parked(t, home); p != "" {
		t.Fatalf("finished, and still parked: %s", p)
	}
}

// Where approving again cannot help, the wait ends there with the refusal,
// and what was typed after it is not read.
func TestLoginOnATerminalStopsWhereApprovingAgainCannotHelp(t *testing.T) {
	t.Run("the same e-mail under another id", func(t *testing.T) {
		home := seedTwoClaude(t)
		approvalsByCode(t, known)
		typing(t, "RECREATED\nAS12\n")
		out, errOut, code := call(t, "login", "12")
		if code != 1 || !strings.Contains(errOut, "account id differs") || strings.Contains(out, "Refreshed") {
			t.Fatalf("%d\n%s\n%s", code, out, errOut)
		}
		if !readAccounts(t, home).Accounts[0].Dead {
			t.Fatal("the second line was read")
		}
	})
	t.Run("the account removed meanwhile", func(t *testing.T) {
		home := seedTwoClaude(t)
		approvalsByCode(t, known)
		typing(t, "AS12\n")
		duringWait(t, func(context.Context) {
			writeStore(t, home, `{"ordered":true,"nextId":14,"accounts":[
				{"id":13,"provider":"claude","email":"d@e.f","uuid":"u13","order":2,"token":{"accessToken":"OLD-13"}}]}`)
		})
		out, errOut, code := call(t, "login", "12")
		if code != 1 || !strings.Contains(errOut, "no longer here") {
			t.Fatalf("%d\n%s\n%s", code, out, errOut)
		}
		if d := readAccounts(t, home); len(d.Accounts) != 1 {
			t.Fatalf("%+v", d)
		}
	})
}

// ^D ends the wait and not the login: it stays parked, and the line that
// finishes it later is said.
func TestLoginOnATerminalLeavesTheLoginOpenAtTheEndOfInput(t *testing.T) {
	home := seedTwoClaude(t)
	typing(t, "\n").Close()
	out, errOut, code := call(t, "login", "12")
	if code != 1 {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	id := loginIDFrom(t, out)
	if !strings.Contains(errOut, "rota login "+id+" <code>") || !strings.Contains(parked(t, home), id) {
		t.Fatalf("not left open: %q %s", errOut, parked(t, home))
	}
}

// A login that runs out of time is not finished later, and does not say it
// could be: it says how to start again.
func TestLoginOnATerminalSaysWhenTheLoginRanOut(t *testing.T) {
	seedTwoClaude(t)
	typing(t, "")
	was := loginTTL
	loginTTL = 200 * time.Millisecond
	t.Cleanup(func() { loginTTL = was })
	out, errOut, code := call(t, "login", "12")
	if code != 1 || !strings.Contains(errOut, "expired") || !strings.Contains(errOut, "`rota login 12`") {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	id := loginIDFrom(t, out)
	if strings.Contains(errOut, "rota login "+id) {
		t.Fatalf("offers to finish what has run out: %q", errOut)
	}
}

func TestLongLoginOnATerminalWaitsForTheCode(t *testing.T) {
	seedLong(t, time.Time{}, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "LONG-SECRET", "expires_in": 31536000,
			"account": map[string]string{"uuid": "u1", "email_address": "a@b.c"}})
	}))
	defer srv.Close()
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL
	defer func() { rota.ClaudeEndpoints.Token = old }()
	typing(t, "CODE\n")
	out, errOut, code := call(t, "login", "--long")
	if code != 0 || !strings.Contains(out, "long-lived token stored for #1") || strings.Contains(out+errOut, "LONG-SECRET") {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
}

// A device login is approved in the browser and nothing is typed: the wait
// asks the provider by itself until it says yes.
type waitDeviceProvider struct{ asked *atomic.Int32 }

func (waitDeviceProvider) Name() string { return "t-wait-device" }
func (waitDeviceProvider) Begin(context.Context) (string, map[string]string, error) {
	return "https://example.test/device?user_code=ABCD", map[string]string{"kind": "device"}, nil
}
func (p waitDeviceProvider) Complete(context.Context, string, map[string]string) (*rota.Token, error) {
	if p.asked.Add(1) < 3 {
		return nil, rota.ErrAuthPending
	}
	return &rota.Token{Access: "DEV", Identity: &rota.Identity{UUID: "dev-1"}}, nil
}
func (waitDeviceProvider) Launch(*rota.Account, string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}

var deviceAsked = &atomic.Int32{}

// waitOwnProvider's CLI signs itself in, so there is nothing to wait for.
type waitOwnProvider struct{}

func (waitOwnProvider) Name() string { return "t-wait-own" }
func (waitOwnProvider) Begin(context.Context) (string, map[string]string, error) {
	return "https://example.test/own", map[string]string{"kind": "delegated"}, nil
}
func (waitOwnProvider) Complete(context.Context, string, map[string]string) (*rota.Token, error) {
	return &rota.Token{Delegated: true, Identity: &rota.Identity{UUID: "own-1"}}, nil
}
func (waitOwnProvider) Launch(*rota.Account, string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}

func init() {
	rota.Register(waitDeviceProvider{deviceAsked})
	rota.Register(waitOwnProvider{})
}

func TestDeviceLoginOnATerminalAsksUntilApproved(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	typing(t, "")
	was := devicePoll
	devicePoll = 10 * time.Millisecond
	t.Cleanup(func() { devicePoll = was })
	deviceAsked.Store(0)
	out, errOut, code := call(t, "login", "t-wait-device")
	if code != 0 || !strings.Contains(out, "Added t-wait-device account 1") || deviceAsked.Load() != 3 {
		t.Fatalf("%d asked %d\n%s\n%s", code, deviceAsked.Load(), out, errOut)
	}
}

// A key is read the way every pasted key is, and is on the screen nowhere.
func TestKeyLoginOnATerminalReadsTheKeyUnseen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	typing(t, "\nsk-test-key\n")
	out, errOut, code := call(t, "login", "t-cli-fake")
	if code != 0 || !strings.Contains(out, "Added t-cli-fake account 1") {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(out+errOut, "sk-test-key") || !strings.Contains(errOut, "not shown") {
		t.Fatalf("the key was shown, or the prompt does not say it is not: %q %q", out, errOut)
	}
	if p := parked(t, home); p != "" {
		t.Fatalf("finished, and still parked: %s", p)
	}
}

// What has no wait in it is as it was: a delegated login, --json, a login id
// with its code, and an input that is not a terminal.
func TestLoginWithoutATerminalOrWithJSONIsTwoSteps(t *testing.T) {
	t.Run("delegated", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("ROTA_HOME", home)
		typing(t, "")
		out, errOut, code := call(t, "login", "t-wait-own")
		if code != 0 || !strings.Contains(out, "registers the account") || strings.Contains(errOut, "left") {
			t.Fatalf("%d\n%s\n%s", code, out, errOut)
		}
	})
	t.Run("json", func(t *testing.T) {
		home := seedTwoClaude(t)
		typing(t, "")
		out, errOut, code := call(t, "--json", "login", "12")
		var l rota.Login
		if err := json.Unmarshal([]byte(out), &l); code != 0 || err != nil || l.Account != 12 || errOut != "" {
			t.Fatalf("%d %v\n%s\n%s", code, err, out, errOut)
		}
		if !strings.Contains(parked(t, home), l.ID) {
			t.Fatal("not parked")
		}
		// And the second step is still a command of its own.
		approvalsByCode(t, known)
		out, errOut, code = call(t, "login", l.ID, "AS12")
		if code != 0 || !strings.Contains(out, "Refreshed claude account 12") || strings.Contains(errOut, "left") {
			t.Fatalf("%d\n%s\n%s", code, out, errOut)
		}
	})
	t.Run("not a terminal", func(t *testing.T) {
		home := seedTwoClaude(t)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		wasIn, wasTerm := stdin, loginTerminal
		stdin, loginTerminal = r, isLoginTerminal
		t.Cleanup(func() { stdin, loginTerminal = wasIn, wasTerm })
		out, errOut, code := call(t, "login", "12")
		if code != 0 || strings.Contains(errOut, "left") {
			t.Fatalf("%d\n%s\n%s", code, out, errOut)
		}
		if !strings.Contains(parked(t, home), loginIDFrom(t, out)) {
			t.Fatal("not parked")
		}
	})
}
