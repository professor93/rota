package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// modelWindows is a store of two claude accounts read just now: the first
// in the rotation has spent its weekly Fable window, the second has room in
// it.
func modelWindows(t *testing.T, firstFable, secondFable int) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	now := time.Now().UnixMilli()
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"first@x","order":1,"threshold":80,"quotaAt":%[1]d,"token":{"accessToken":"t"},
		 "quota":{"windows":[{"name":"5h","percent":10,"primary":true},{"name":"7d","percent":20},{"name":"Fable","percent":%[2]d,"scoped":true}]}},
		{"id":2,"provider":"claude","email":"second@x","order":2,"quotaAt":%[1]d,"token":{"accessToken":"t"},
		 "quota":{"windows":[{"name":"5h","percent":50,"primary":true},{"name":"Fable","percent":%[3]d,"scoped":true}]}}]}`,
		now, firstFable, secondFable))
	answering(t, "claude")
}

// A request that names a model is judged by that model's own window too: a
// weekly Fable window at its account's threshold passes the account by for a
// Fable request, and for nothing else. A request that names no model is
// chosen for exactly as before.
func TestARequestForAModelPassesByAnAccountThatSpentItsWindow(t *testing.T) {
	modelWindows(t, 85, 3)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "-v", "--model", "fable", "a question"}, "second@x"},
		{[]string{"run", "-v", "--model", "claude-fable-5", "a question"}, "second@x"},
		{[]string{"run", "-v", "--model", "FABLE", "a question"}, "second@x"},
		{[]string{"run", "-v", "--model", "opus", "a question"}, "first@x"},
		{[]string{"run", "-v", "a question"}, "first@x"},
	} {
		out, errOut, code := call(t, c.args...)
		if code != 0 || out != "ANSWERED\n" {
			t.Fatalf("%v: %d %q %q", c.args, code, out, errOut)
		}
		if !strings.Contains(errOut, c.want) {
			t.Fatalf("%v runs on %s: %q", c.args, c.want, errOut)
		}
	}
	// By id it still runs: naming an account is a decision.
	if out, _, code := call(t, "run", "1", "--model", "fable", "a question"); code != 0 || out != "ANSWERED\n" {
		t.Fatalf("by id: %d %q", code, out)
	}
}

// When every account has spent the window of the model asked for, the
// refusal says so, and the same request for another model still runs.
func TestARefusalSaysWhichModelsWindowIsSpent(t *testing.T) {
	modelWindows(t, 100, 100)
	_, errOut, code := call(t, "run", "--model", "fable", "a question")
	if code == 0 || !strings.Contains(errOut, "Fable") || !strings.Contains(errOut, "rotation") {
		t.Fatalf("%d %q", code, errOut)
	}
	if out, _, code := call(t, "run", "--model", "sonnet", "a question"); code != 0 || out != "ANSWERED\n" {
		t.Fatalf("another model: %d %q", code, out)
	}
}

// A model's window is in the readings: a line of its own under USAGE in
// `rota list`, and marked scoped in --json.
func TestAModelsWindowIsShownInTheListing(t *testing.T) {
	modelWindows(t, 85, 3)
	out, errOut, code := call(t, "list")
	if code != 0 || !strings.Contains(out, "Fable 85%") || !strings.Contains(out, "Fable 3%") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	out, _, code = call(t, "--json", "list")
	var doc struct {
		Accounts []struct {
			Windows []struct {
				Name    string  `json:"name"`
				Percent float64 `json:"percent"`
				Scoped  bool    `json:"scoped"`
			} `json:"windows"`
		} `json:"accounts"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Accounts) != 2 {
		t.Fatalf("%d %q", code, out)
	}
	w := doc.Accounts[1].Windows
	if len(w) != 2 || w[1].Name != "Fable" || !w[1].Scoped || w[1].Percent != 3 {
		t.Fatalf("the second account's windows: %+v", w)
	}
}
