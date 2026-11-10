package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// guardDoc is the --settings a guarded launch is given last, as a map, or
// nil when the arguments carry no guard.
func guardDoc(t *testing.T, args []string) map[string]any {
	t.Helper()
	if len(args) < 2 || args[len(args)-2] != "--settings" {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(args[len(args)-1]), &doc); err != nil || doc["disableRemoteControl"] != true {
		return nil
	}
	return doc
}

// A claude session somebody other than the person at this machine's own
// terminal can type into is launched with Claude Code's own
// disableRemoteControl when the account's remote control setting is off,
// from a token and over the control socket alike: given last as
// --settings, with a --settings of its own folded in, inline or from its
// file. With the setting on, and for another provider, nothing is added.
// Over the socket a --settings rota cannot read is refused, never launched
// without the guard. This is argsFor, which a server-run terminal and a held
// one are both handed their arguments by.
func TestASessionWithRemoteControlOffIsLaunchedWithItsGuard(t *testing.T) {
	h := newHarness(t, Options{})
	st, err := h.api.openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := os.WriteFile(filepath.Join(h.root, "mine.json"), []byte(`{"model":"y"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	token := withPrincipal(httptest.NewRequest("POST", "/v1/terminals", nil), &Principal{Name: "alice", Role: RoleControl, Via: "token"})
	local := withPrincipal(httptest.NewRequest("POST", "/v1/terminals", nil), &localControl)
	args := func(r *http.Request, id int, in []string) ([]string, error) {
		t.Helper()
		a := st.Find(id)
		return h.api.argsFor(r, st, a, &termRequest{Args: in}, h.root)
	}
	for name, r := range map[string]*http.Request{"a token": token, "the socket": local} {
		got, err := args(r, 1, []string{"--model", "opus"})
		if err != nil || guardDoc(t, got) == nil || !slices.Equal(got[:len(got)-2], []string{"--model", "opus"}) {
			t.Errorf("%s, remote control off: %q %v, want the rest as it was and the guard last", name, got, err)
		}
		got, err = args(r, 1, []string{"--settings", `{"model":"x"}`, "--verbose"})
		if doc := guardDoc(t, got); err != nil || doc == nil || doc["model"] != "x" || !slices.Equal(got[:len(got)-2], []string{"--verbose"}) {
			t.Errorf("%s, an inline --settings: %q %v, want it folded into the guard", name, got, err)
		}
		got, err = args(r, 1, []string{"--settings", "mine.json"})
		if doc := guardDoc(t, got); err != nil || doc == nil || doc["model"] != "y" || len(got) != 2 {
			t.Errorf("%s, a --settings file: %q %v, want it folded into the guard", name, got, err)
		}
		if got, err := args(r, 2, []string{"--model", "o3"}); err != nil || !slices.Equal(got, []string{"--model", "o3"}) {
			t.Errorf("%s, codex: %q %v, want its arguments as they were", name, got, err)
		}
	}
	if _, err := args(local, 1, []string{"--settings", "missing.json"}); err == nil || err.Error() != guardUnread || statusFor(err) != http.StatusBadRequest {
		t.Errorf("the socket with a --settings rota cannot read: %v, want 400 %q", err, guardUnread)
	}
	// The policy tier's document from a token, whatever it says; over the
	// socket one that is not a document, or one that would turn
	// disableRemoteControl off above the guard, refuses the launch.
	for _, in := range [][]string{
		{"--managed-settings", `{"model":"x"}`},
		{"--managed-settings", "x"},
		{"--", `--managed-settings={}`},
	} {
		if _, err := args(token, 1, in); err == nil || err.Error() != managedLocal || statusFor(err) != http.StatusForbidden {
			t.Errorf("a token with %q: %v, want 403 %q", in, err, managedLocal)
		}
	}
	if _, err := args(local, 1, []string{"--managed-settings", "x"}); err == nil || err.Error() != managedUnread || statusFor(err) != http.StatusBadRequest {
		t.Errorf("the socket with a --managed-settings that is no document: %v, want 400 %q", err, managedUnread)
	}
	if _, err := args(local, 1, []string{"--managed-settings", `{"disableRemoteControl":false}`}); err == nil ||
		err.Error() != managedReenables || statusFor(err) != http.StatusBadRequest {
		t.Errorf("the socket with a --managed-settings that lifts the guard: %v, want 400 %q", err, managedReenables)
	}
	if got, err := args(local, 1, []string{"--managed-settings", `{"model":"x"}`}); err != nil || guardDoc(t, got) == nil ||
		!slices.Equal(got[:len(got)-2], []string{"--managed-settings", `{"model":"x"}`}) {
		t.Errorf("the socket with a --managed-settings that says nothing of Remote Control: %q %v, want it behind the guard", got, err)
	}
	st.Find(1).RemoteControl = true
	if got, err := args(local, 1, []string{"--model", "opus"}); err != nil || !slices.Equal(got, []string{"--model", "opus"}) {
		t.Errorf("remote control on: %q %v, want nothing added", got, err)
	}
	if got, err := args(local, 1, []string{"--managed-settings", `{"disableRemoteControl":false}`}); err != nil || len(got) != 2 {
		t.Errorf("remote control on, with nothing to guard: %q %v, want it handed over", got, err)
	}
}

// turnOn turns account id's remote control setting on in the store's file,
// as a person would have with rota set, without a launch that readies it.
func (h *harness) turnOn(id int) {
	h.t.Helper()
	path := filepath.Join(h.dir, "accounts.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		h.t.Fatal(err)
	}
	accounts, _ := doc["accounts"].([]any)
	for _, a := range accounts {
		if a := a.(map[string]any); a["id"] == float64(id) {
			a["remoteControl"] = true
		}
	}
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// A run is a claude process the server starts too, and one that stays open
// takes messages from whoever sent it, so a claude account whose remote
// control setting is off runs behind the same guard: its settings, or the
// --settings among its raw args, carry disableRemoteControl, the rest
// exactly as asked. With the setting on, and for another provider, nothing
// is added.
func TestARunWithRemoteControlOffIsGuarded(t *testing.T) {
	h := newHarness(t, Options{AllowRawFlags: true})
	result := func(id int, body map[string]any) string {
		t.Helper()
		body["prompt"] = "hi"
		code, out, raw := h.run(id, body)
		if code != http.StatusOK {
			t.Fatalf("%v: %d %s", body, code, raw)
		}
		return out.Result
	}
	for _, c := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{}, `--settings {"disableRemoteControl":true}`},
		{map[string]any{"settings": map[string]any{"a": 1}}, `--settings {"a":1,"disableRemoteControl":true}`},
		{map[string]any{"args": []string{"--settings", `{"model":"x"}`}}, `--settings {"disableRemoteControl":true,"model":"x"}`},
		{map[string]any{"args": []string{"--append-system-prompt", "--settings"}},
			`--append-system-prompt --settings --settings {"disableRemoteControl":true}`},
		// Raw args that carry no --settings of their own still come after
		// the one the settings become, so the guard is theirs, last, with
		// the settings folded in.
		{map[string]any{"settings": map[string]any{"a": 1}, "args": []string{"--verbose"}},
			`--verbose --settings {"a":1,"disableRemoteControl":true}`},
	} {
		if got := result(1, c.body); !strings.Contains(got, c.want) || strings.Count(got, "disableRemoteControl") != 1 {
			t.Errorf("%v: %q, want %q once", c.body, got, c.want)
		}
	}
	if got := result(2, map[string]any{}); strings.Contains(got, "disableRemoteControl") {
		t.Errorf("codex is given Claude Code's guard: %q", got)
	}
	h.turnOn(3)
	if got := result(3, map[string]any{}); strings.Contains(got, "disableRemoteControl") {
		t.Errorf("an account with remote control on is given the guard: %q", got)
	}
	// One that stays open is guarded the same way. Its CLI leaves its input
	// open and answers with its arguments.
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Spec{KeepStdin: true, Stdout: []string{
		`{"type":"assistant","session_id":"s-open","message":{"role":"assistant","content":[{"type":"text","text":"ARGS={{args}}"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s-open","result":"ok","num_turns":1}`}})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := openStream(t, h, "POST", "/v1/accounts/1/run",
		map[string]any{"prompt": "hi", "stream": true, "input": true}, "Accept", "application/x-ndjson")
	s.waitFor(t, "done")
	if all := s.all(); !strings.Contains(all, `disableRemoteControl`) {
		t.Errorf("an open run is not guarded: %s", all)
	}
}
