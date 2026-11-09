package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// approver is a fake Claude token endpoint; approving is whoever it is set to
// next, and a blank uuid is an approval that names nobody.
type approver struct{ uuid, email string }

func claudeApproves(t *testing.T) *approver {
	t.Helper()
	who := &approver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/profile") {
			w.WriteHeader(500)
			return
		}
		doc := map[string]any{"access_token": "NEW-" + who.uuid, "refresh_token": "R-" + who.uuid, "expires_in": 3600}
		if who.uuid != "" {
			doc["account"] = map[string]string{"uuid": who.uuid, "email_address": who.email}
		}
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	for _, e := range []*string{&rota.ClaudeEndpoints.Token, &rota.ClaudeEndpoints.Profile} {
		old := *e
		t.Cleanup(func() { *e = old })
	}
	rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile = srv.URL+"/token", srv.URL+"/profile"
	return who
}

// seedTwoClaude is two claude accounts, 12 of them dead, and returns the store.
func seedTwoClaude(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":14,"accounts":[
		{"id":12,"provider":"claude","email":"a@b.c","uuid":"u12","order":1,"dead":true,"deadReason":"gone",
		 "token":{"accessToken":"OLD-12","refreshToken":"R12"}},
		{"id":13,"provider":"claude","email":"d@e.f","uuid":"u13","order":2,
		 "token":{"accessToken":"OLD-13","refreshToken":"R13"}}]}`)
	return home
}

type accountsDoc struct {
	NextID   int `json:"nextId"`
	Accounts []struct {
		ID         int    `json:"id"`
		Dead       bool   `json:"dead"`
		DeadReason string `json:"deadReason"`
		Token      struct {
			Access string `json:"accessToken"`
		} `json:"token"`
	} `json:"accounts"`
}

func readAccounts(t *testing.T, home string) accountsDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d accountsDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func loginIDFrom(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`Then: rota login ([0-9a-f]{6}) <code>`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no way to finish it in: %q", out)
	}
	return m[1]
}

func parked(t *testing.T, home string) string {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(home, "homes", "pending.json"))
	return string(raw)
}

func TestLoginByAccountIDStartsALoginBoundToIt(t *testing.T) {
	home := seedTwoClaude(t)
	out, errOut, code := call(t, "login", "12")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if !strings.Contains(out, "a@b.c") || !strings.Contains(out, "account 12") {
		t.Fatalf("which account to approve as: %q", out)
	}
	if !regexp.MustCompile(`(?m)^  \S+\?\S+$`).MatchString(out) {
		t.Fatalf("no URL: %q", out)
	}
	id := loginIDFrom(t, out)
	if p := parked(t, home); !strings.Contains(p, `"account": 12`) || !strings.Contains(p, id) {
		t.Fatalf("not bound: %s", p)
	}

	// --json is the login, as a login that starts anywhere else prints it.
	out, _, code = call(t, "--json", "login", "13")
	var l rota.Login
	if err := json.Unmarshal([]byte(out), &l); code != 0 || err != nil || l.Account != 13 || l.URL == "" || l.Kind != "code" {
		t.Fatalf("%d %v %q", code, err, out)
	}
}

func TestLoginByAccountIDFinishedAsThatAccountRevivesIt(t *testing.T) {
	home := seedTwoClaude(t)
	who := claudeApproves(t)
	out, _, _ := call(t, "login", "12")
	*who = approver{"u12", "a@b.c"}
	out, errOut, code := call(t, "login", loginIDFrom(t, out), "CODE")
	if code != 0 || !strings.Contains(out, "Refreshed claude account 12") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	d := readAccounts(t, home)
	if len(d.Accounts) != 2 || d.NextID != 14 {
		t.Fatalf("an account was added: %+v", d)
	}
	if a := d.Accounts[0]; a.ID != 12 || a.Dead || a.DeadReason != "" || a.Token.Access != "NEW-u12" {
		t.Fatalf("not revived: %+v", a)
	}
	if b := d.Accounts[1]; b.Token.Access != "OLD-13" {
		t.Fatalf("the other moved: %+v", b)
	}
}

func TestLoginByAccountIDRefusesApprovalAsAnotherAccount(t *testing.T) {
	home := seedTwoClaude(t)
	who := claudeApproves(t)
	out, _, _ := call(t, "login", "12")
	id := loginIDFrom(t, out)
	before := readAccounts(t, home)

	*who = approver{"u13", "d@e.f"}
	_, errOut, code := call(t, "login", id, "CODE")
	if code == 0 || !strings.Contains(errOut, "d@e.f") || !strings.Contains(errOut, "account 13") ||
		!strings.Contains(errOut, "account 12") || !strings.Contains(errOut, "a@b.c") {
		t.Fatalf("%d %q", code, errOut)
	}
	if strings.Contains(errOut, "NEW-u13") || strings.Contains(errOut, "R-u13") {
		t.Fatalf("a token in the refusal: %q", errOut)
	}
	after := readAccounts(t, home)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("changed:\n%+v\n%+v", before, after)
	}
	if !strings.Contains(parked(t, home), id) {
		t.Fatal("the parked login must stay")
	}

	// Approving again as the right account finishes the same login.
	*who = approver{"u12", "a@b.c"}
	out, errOut, code = call(t, "login", id, "CODE2")
	if code != 0 || !strings.Contains(out, "Refreshed claude account 12") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if d := readAccounts(t, home); d.Accounts[0].Dead || d.Accounts[1].Token.Access != "OLD-13" {
		t.Fatalf("%+v", d)
	}
}

func TestLoginByAccountIDRefusesAnUnknownOrNamelessApproval(t *testing.T) {
	for _, c := range []struct {
		name string
		who  approver
		word string
	}{
		{"an account rota does not hold", approver{"u-new", "new@x.y"}, "new@x.y"},
		{"nobody", approver{}, "named no account"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := seedTwoClaude(t)
			who := claudeApproves(t)
			out, _, _ := call(t, "login", "12")
			*who = c.who
			_, errOut, code := call(t, "login", loginIDFrom(t, out), "CODE")
			if code == 0 || !strings.Contains(errOut, c.word) {
				t.Fatalf("%d %q", code, errOut)
			}
			if d := readAccounts(t, home); len(d.Accounts) != 2 || d.NextID != 14 || !d.Accounts[0].Dead {
				t.Fatalf("%+v", d)
			}
		})
	}
}

// With no vendor CLI to pass them to, what follows the id is a mistake, and is
// not repeated: it may be a credential typed in the wrong place.
func TestLoginByAccountIDTakesNothingElse(t *testing.T) {
	home := seedTwoClaude(t)
	_, errOut, code := call(t, "login", "12", "hunter2", "--region", "global")
	if code != 2 || !strings.Contains(errOut, "usage") || strings.Contains(errOut, "hunter2") {
		t.Fatalf("%d %q", code, errOut)
	}
	if p := parked(t, home); p != "" {
		t.Fatalf("parked: %s", p)
	}
}

func TestLoginByAccountIDOnAKeySaysHowAKeyIsGiven(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","uuid":"key-1","credential":"apikey","token":{"accessToken":"sk-ant-api-xxxxWXYZ"}},
		{"id":2,"provider":"grok","uuid":"key-2","token":{"accessToken":"xai-abc"}}]}`)
	_, errOut, code := call(t, "login", "1")
	if code != 1 || !strings.Contains(errOut, "rota login --key") || strings.Contains(errOut, "signed in already") {
		t.Fatalf("%d %q", code, errOut)
	}
	_, errOut, code = call(t, "login", "2")
	if code != 1 || !strings.Contains(errOut, "rota login grok") || strings.Contains(errOut, "xai-abc") {
		t.Fatalf("%d %q", code, errOut)
	}
	if p := parked(t, home); p != "" {
		t.Fatalf("parked: %s", p)
	}
}

// The login that names nobody goes on matching by who approved, and is still
// what makes an account.
func TestBareLoginStillMatchesByIdentityAndAdds(t *testing.T) {
	home := seedTwoClaude(t)
	who := claudeApproves(t)
	out, _, _ := call(t, "login")
	if p := parked(t, home); strings.Contains(p, `"account"`) {
		t.Fatalf("a bare login is bound: %s", p)
	}
	*who = approver{"u13", "d@e.f"}
	if out, errOut, code := call(t, "login", loginIDFrom(t, out), "CODE"); code != 0 || !strings.Contains(out, "Refreshed claude account 13") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	out, _, _ = call(t, "login")
	*who = approver{"u-new", "new@x.y"}
	if out, errOut, code := call(t, "login", loginIDFrom(t, out), "CODE"); code != 0 || !strings.Contains(out, "Added claude account 14") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if d := readAccounts(t, home); len(d.Accounts) != 3 {
		t.Fatalf("%+v", d)
	}
}

// remedyID is the account the sentence tells somebody to log in again.
func remedyID(t *testing.T, sentence string) string {
	t.Helper()
	m := regexp.MustCompile(`rota login (\d+) makes a new`).FindStringSubmatch(sentence)
	if m == nil {
		t.Fatalf("no command in %q", sentence)
	}
	if _, err := strconv.Atoi(m[1]); err != nil {
		t.Fatal(err)
	}
	return m[1]
}

// The words an account is left dead with are a command, and the command works.
func TestTheDeadAccountRemediesStartALoginWhenFollowed(t *testing.T) {
	t.Run("the login's end", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("ROTA_HOME", home)
		writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":8,"accounts":[
			{"id":7,"provider":"claude","email":"a@b.c","uuid":"u7","order":1,
			 "token":{"accessToken":"t","refreshToken":"R","expiresAt":1},
			 "extra":{"refresh_token_expires_at":"%d"}}]}`, time.Now().Add(-time.Hour).UnixMilli()))
		call(t, "list", "-r")
		d := readAccounts(t, home)
		if !d.Accounts[0].Dead {
			t.Fatalf("not dead: %+v", d)
		}
		id := remedyID(t, d.Accounts[0].DeadReason)
		if out, errOut, code := call(t, "login", id); code != 0 || !strings.Contains(out, "account 7") {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
	})
	t.Run("an older copy", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("ROTA_HOME", home)
		const why = "Claude Code's refresh was refused and rota's copy is older than the home's last look, " +
			"so rota did not present it; rota login 7 makes a new login"
		raw, _ := json.Marshal(why)
		writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":8,"accounts":[
			{"id":7,"provider":"claude","email":"a@b.c","uuid":"u7","order":1,"dead":true,"deadReason":%s,
			 "token":{"accessToken":"t","refreshToken":"R"}}]}`, raw))
		id := remedyID(t, readAccounts(t, home).Accounts[0].DeadReason)
		if out, errOut, code := call(t, "login", id); code != 0 || !strings.Contains(out, "account 7") {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
	})
}
