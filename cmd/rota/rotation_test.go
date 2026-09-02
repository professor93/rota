package main

import (
	"encoding/json"
	"fmt"
	"github.com/professor93/rota/internal/fakecli"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// otherCLI is a provider whose vendor binary has a name of its own, so a
// test can tell which of two accounts rota chose by which command it went
// looking for.
type otherCLI struct{ cliFakeProvider }

func (otherCLI) Name() string { return "t-other-cli" }
func (otherCLI) Launch(*rota.Account, string) (*rota.Command, error) {
	return &rota.Command{Bin: "t-other-cli-bin"}, nil
}

func init() { rota.Register(otherCLI{}) }

// writeStore lays down an account file for a test.
func writeStore(t *testing.T, home, blob string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
}

// answering installs a stand-in vendor CLI under each of these names.
func answering(t *testing.T, names ...string) string {
	t.Helper()
	bin := t.TempDir()
	for _, n := range names {
		fakecli.Install(t, bin, n, fakecli.Lines(`[{"type":"result","result":"ANSWERED","session_id":"s-1"}]`))
	}
	t.Setenv("PATH", bin)
	return bin
}

func TestRunWithoutAnIDAsksTheFirstAccountInTheRotation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"second@x","order":2,"token":{"accessToken":"t"}},
		{"id":2,"provider":"claude","email":"first@x","order":1,"token":{"accessToken":"t"}}]}`)
	answering(t, "claude")

	out, errOut, code := call(t, "run", "-v", "a question")
	if code != 0 || out != "ANSWERED\n" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "first@x") {
		t.Fatalf("the rotation runs order 1 first: %q", errOut)
	}
}

func TestRunSkipsAnAccountThatHasReachedItsThreshold(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	// A reading taken just now, so nothing goes to the network for it.
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"busy@x","order":1,"threshold":50,"quotaAt":%d,
		 "quota":{"windows":[{"name":"5h","percent":62}]},"token":{"accessToken":"t"}},
		{"id":2,"provider":"claude","email":"spare@x","order":2,"quotaAt":%d,
		 "quota":{"windows":[{"name":"5h","percent":4}]},"token":{"accessToken":"t"}}]}`,
		time.Now().UnixMilli(), time.Now().UnixMilli()))
	answering(t, "claude")

	_, errOut, code := call(t, "run", "-v", "a question")
	if code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if !strings.Contains(errOut, "spare@x") {
		t.Fatalf("62%% is past a threshold of 50, so the rotation moves on: %q", errOut)
	}
}

func TestRunWithoutArgumentsOpensTheDefaultAccountsOwnCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	t.Setenv("PATH", t.TempDir()) // no vendor CLI anywhere
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":2,"token":{"accessToken":"t"}},
		{"id":2,"provider":"t-other-cli","email":"b@x","order":1,"token":{"accessToken":"t"}}]}`)

	// Getting as far as looking for the other account's binary proves both
	// that no id meant the rotation's choice and that no prompt meant the
	// interactive form.
	_, errOut, code := call(t, "run")
	if code != 1 || !strings.Contains(errOut, "t-other-cli-bin") || !strings.Contains(errOut, "PATH") {
		t.Fatalf("%d %q", code, errOut)
	}
	// An id with no prompt opens that account instead.
	if _, errOut, code := call(t, "run", "1"); code != 1 || !strings.Contains(errOut, "claude") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestRunSaysWhenNothingIsInTheRotation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	answering(t, "claude")
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":0,"token":{"accessToken":"t"}}]}`)

	_, errOut, code := call(t, "run", "hello")
	if code != 1 || !strings.Contains(errOut, "rotation") {
		t.Fatalf("%d %q", code, errOut)
	}
	// Naming the account by id still works: order 0 means "not automatic",
	// not "disabled".
	if out, _, code := call(t, "run", "1", "hello"); code != 0 || out != "ANSWERED\n" {
		t.Fatalf("%d %q", code, out)
	}
}

func TestOrderAndThresholdAreSetFromTheCommandLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"}}]}`)

	if _, errOut, code := call(t, "set", "2", "--order", "1"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, errOut, code := call(t, "set", "1", "--threshold", "80"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	out, _, code := call(t, "--json", "list")
	if code != 0 {
		t.Fatalf("%d", code)
	}
	var doc struct {
		Accounts []struct {
			ID        int `json:"id"`
			Order     int `json:"order"`
			Threshold int `json:"threshold"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err, out)
	}
	got := map[int][2]int{}
	for _, a := range doc.Accounts {
		got[a.ID] = [2]int{a.Order, a.Threshold}
	}
	// Putting 2 first moved 1 down: the queue is a list, and no two accounts
	// share a number.
	if got[1] != [2]int{2, 80} || got[2] != [2]int{1, 100} {
		t.Fatalf("%v", got)
	}
	// Misuse is caught rather than stored.
	for _, argv := range [][]string{
		{"set", "1", "--order", "x"}, {"set", "9", "--order", "1"}, {"set", "1", "--order", "-2"},
		{"set", "1", "--threshold", "0"}, {"set", "1", "--threshold", "101"}, {"set"},
	} {
		if _, _, code := call(t, argv...); code == 0 {
			t.Fatalf("%v was accepted", argv)
		}
	}

	// One account, one write: the fields that used to need three commands
	// are set together, which is what PATCH /v1/accounts/{id} already did.
	if _, errOut, code := call(t, "set", "2", "--order", "2", "--threshold", "60"); code != 0 {
		t.Fatalf("together: %d %q", code, errOut)
	}
	out, _, code = call(t, "--json", "list")
	if code != 0 {
		t.Fatalf("%d", code)
	}
	doc.Accounts = nil
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err, out)
	}
	for _, a := range doc.Accounts {
		if a.ID == 2 && (a.Order != 2 || a.Threshold != 60) {
			t.Fatalf("both fields must land in one write: %+v", a)
		}
	}
}

// --order takes a place, not just a number: words for the ends and the
// neighbours, and a position relative to another account. Because a move
// changes the neighbours too, the answer says what moved and shows the
// whole queue rather than one account's number.
func TestSetOrderTakesWordsAndSaysWhatMoved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":5,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"}},
		{"id":3,"provider":"claude","email":"c@x","order":3,"token":{"accessToken":"t"}},
		{"id":4,"provider":"claude","email":"d@x","order":0,"token":{"accessToken":"t"}}]}`)

	say := func(args ...string) string {
		t.Helper()
		out, errOut, code := call(t, append([]string{"set"}, args...)...)
		if code != 0 {
			t.Fatalf("set %v: %d %q", args, code, errOut)
		}
		return out
	}
	want := func(out string, lines ...string) {
		t.Helper()
		for _, l := range lines {
			if !strings.Contains(out, l) {
				t.Fatalf("want %q in:\n%s", l, out)
			}
		}
	}
	want(say("2", "--order", "first"),
		"#2 b@x moved to 1st in the rotation; #1 a@x is now 2nd.",
		"Rotation: #2 b@x, #1 a@x, #3 c@x. Out of it: #4 d@x.")
	want(say("4", "--order", "before:3"),
		"#4 d@x joined the rotation at 3rd; #3 c@x is now 4th.",
		"Rotation: #2 b@x, #1 a@x, #4 d@x, #3 c@x.\n")
	want(say("2", "--order", "out"),
		"#2 b@x left the rotation; #1 a@x is now 1st, #4 d@x 2nd, #3 c@x 3rd.",
		"Rotation: #1 a@x, #4 d@x, #3 c@x. Out of it: #2 b@x.")
	want(say("1", "--order", "up"), "#1 a@x is already 1st in the rotation.\n")
	want(say("3", "--order", "up"), "#3 c@x moved to 2nd in the rotation; #4 d@x is now 3rd.")
	want(say("1", "--order", "99"), "#1 a@x moved to 3rd in the rotation; #3 c@x is now 1st, #4 d@x 2nd.")
	// Anything else set alongside the move is still reported.
	want(say("1", "--order", "last", "--threshold", "70"), "is already 3rd", "until 70% usage")

	// A move that cannot be made is refused with the reason, and nothing
	// moves.
	if _, errOut, code := call(t, "set", "2", "--order", "down"); code == 0 || !strings.Contains(errOut, "out of the rotation") {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, errOut, code := call(t, "set", "1", "--order", "after:2"); code == 0 || !strings.Contains(errOut, "no account 2 in the rotation") {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, _, code := call(t, "set", "1", "--order", "sideways"); code != 2 {
		t.Fatalf("a word that is not a place is misuse: %d", code)
	}
	want(say("1"), "number 3, until 70% usage")

	// --json keeps the account view: the queue is one `rota list` away.
	out, _, code := call(t, "--json", "set", "1", "--order", "first")
	if code != 0 || !strings.Contains(out, `"order": 1`) || strings.Contains(out, "Rotation:") {
		t.Fatalf("%d %s", code, out)
	}
}

// Order 0 is a real value, not an omission: it takes an account out of the
// rotation. A flag package default cannot tell the two apart, so `set` has
// to look at which flags were actually given.
func TestSetTellsOrderZeroFromAnUngivenOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":3,"threshold":70,"token":{"accessToken":"t"}}]}`)

	// A threshold on its own must leave the order alone.
	if _, errOut, code := call(t, "set", "1", "--threshold", "90"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if out, _, _ := call(t, "--json", "list"); !strings.Contains(out, `"order": 3`) {
		t.Fatalf("an ungiven order must be left alone: %s", out)
	}
	// And --order 0 must be stored, not read as "nothing given".
	if _, errOut, code := call(t, "set", "1", "--order", "0"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if out, _, _ := call(t, "--json", "list"); !strings.Contains(out, `"order": 0`) {
		t.Fatalf("--order 0 must take the account out of the rotation: %s", out)
	}
}

func TestListIsSortedByRotationOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":5,"accounts":[
		{"id":1,"provider":"claude","email":"third@x","order":0,"token":{"accessToken":"t"}},
		{"id":2,"provider":"claude","email":"second@x","order":9,"token":{"accessToken":"t"}},
		{"id":3,"provider":"claude","email":"first@x","order":1,"token":{"accessToken":"t"}}]}`)

	out, _, code := call(t, "list", "--short")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	first, second, third := strings.Index(out, "first@x"), strings.Index(out, "second@x"), strings.Index(out, "third@x")
	if first < 0 || first > second || second > third {
		t.Fatalf("the rotation's order is the listing's order, unordered accounts last:\n%s", out)
	}
	if !strings.Contains(out, "ORDER") {
		t.Fatalf("the short list still names the column it is sorted by:\n%s", out)
	}
	// --short does not go to the network, so it works with no provider
	// reachable at all; the full list has the same order.
	full, _, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d", code)
	}
	if strings.Index(full, "first@x") > strings.Index(full, "second@x") {
		t.Fatalf("full list out of order:\n%s", full)
	}
}
