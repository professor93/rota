package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// claudeSays puts a claude on PATH that prints this line, and exits 1 when it
// is an error, as Claude Code does.
func claudeSays(t *testing.T, line string) {
	t.Helper()
	bin := t.TempDir()
	spec := fakecli.Lines(line)
	if strings.Contains(line, `"is_error":true`) {
		spec.Exit = 1
	}
	fakecli.Install(t, bin, "claude", spec)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A key the rotation picked and the API refused for want of credit is passed
// by afterwards, and the listing says why; a run on it by id that is answered
// puts it back. The run itself prints what Claude Code printed, and nothing of
// rota's.
func TestAKeyOutOfCreditIsPassedByUntilItIsAnswered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","uuid":"apikey-test","credential":"apikey","order":1,"token":{"accessToken":"%s"}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"},"quotaAt":%d,
		 "quota":{"windows":[{"name":"5h","percent":3,"primary":true}]}}]}`, testKey, time.Now().UnixMilli()))

	claudeSays(t, `{"type":"result","subtype":"success","is_error":true,"api_error_status":400,"num_turns":1,`+
		`"result":"Credit balance is too low","session_id":"s-credit","total_cost_usd":0}`)
	out, errOut, code := call(t, "run", "hello there")
	if code != 1 || strings.TrimSpace(out) != "Credit balance is too low" || errOut != "" {
		t.Fatalf("the run ends as Claude Code ended it: %d %q %q", code, out, errOut)
	}
	secretless(t, "run", out, errOut)

	out, _, code = call(t, "list")
	if code != 0 {
		t.Fatalf("list: %d %q", code, out)
	}
	row := lineWith(t, out, "api key ...WXYZ")
	if !strings.Contains(row, "out of credit (59m)") && !strings.Contains(row, "out of credit (1h 0m)") {
		t.Fatalf("STATUS says it, with how long it is passed by: %q", row)
	}
	if !strings.Contains(out, "uses claude/b@x") || !strings.Contains(out, "rota login --key") {
		t.Fatalf("the rotation has moved on, and the note says what ends it:\n%s", out)
	}
	out, _, _ = call(t, "list", "--json")
	if !strings.Contains(out, `"status": "limited"`) || !strings.Contains(out, `"no_credit_until": "`) ||
		!strings.Contains(out, `"default": 2`) {
		t.Fatalf("JSON says it in fields: %s", out)
	}

	claudeSays(t, `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"hi","session_id":"s-ok"}`)
	if out, errOut, code := call(t, "run", "1", "hello there"); code != 0 || strings.TrimSpace(out) != "hi" {
		t.Fatalf("by id it runs: %d %q %q", code, out, errOut)
	}
	out, _, _ = call(t, "list", "--json")
	if strings.Contains(out, "no_credit_until") || !strings.Contains(out, `"default": 1`) {
		t.Fatalf("an answer puts it back: %s", out)
	}
}

// lineWith is the line of out that carries s.
func lineWith(t *testing.T, out, s string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, s) {
			return line
		}
	}
	t.Fatalf("no line with %q in:\n%s", s, out)
	return ""
}
