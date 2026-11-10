package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These are about Remote Control asked for by a claude account's CLI
// arguments rather than by remote_control: every spelling Claude Code takes
// (holdwire.AsksRemoteControl). Whatever carries Remote Control is held to
// the option's rule — taken only over the control socket, and there readied
// by the store's one rule for it — and so is a change to an account's
// remote_control setting.

// The sentences the two refusals are made in.
const (
	wantArgsLocal     = "--remote-control and the remote-control subcommand are taken only over the control socket, as remote_control is"
	wantSettingsLocal = "--settings that rota cannot read is taken only over the control socket, since it could turn Remote Control on"
)

// remoteSpellings is a sample of the spellings that ask for Remote Control, the
// security review's probe of 2035e75 first: each of its rows was started
// from a token without the account being readied. The matcher's own test
// has the whole table.
var remoteSpellings = [][]string{
	{"--remote-control"},
	{"--rc"},
	{"--rc=laptop"},
	{"--rc", "laptop"},
	{"rc"},
	{"remote"},
	{"sync"},
	{"bridge"},
	{"--settings", `{"remoteControlAtStartup":true}`},
	{`--settings={"remoteControlAtStartup":true}`},
	{"--remote-control=laptop"},
	{"remote-control"},
	{"--model", "opus", "rc"},
	{"--", "remote-control"},
	// The re-review's probes of a46fbc9, each a "--" taken as a flag's
	// value, after which the flag is still a flag.
	{"--model", "--", "--rc"},
	{"-n", "--", "--remote-control"},
	{"--name", "--", "--settings", `{"remoteControlAtStartup":true}`},
	{"--append-system-prompt", "--", "--rc=desk"},
	{"-cn", "--", "--rc"},
	{"--add-dir", "--", "--rc"},
	// A --settings that cannot be read does not hide a later one.
	{"--settings", "missing.json", "--settings", `{"remoteControlAtStartup":true}`},
}

// plainArgs carry no Remote Control, however close they come: a flag's
// value is a value.
var plainArgs = [][]string{
	{"--model", "opus"},
	{"--remote-controlled"},
	{"--name", "sync"},
	{"-w", "remote-control"},
	{"--remote-control-session-name-prefix=x"},
	{"--settings", `{"model":"x"}`},
}

// socket is the same harness reached through LocalHandler as local, on a
// listener of its own: what the control socket is, without the socket.
func (h *harness) socket() *harness {
	h.t.Helper()
	c := *h
	c.srv = httptest.NewServer(h.api.LocalHandler(&localControl))
	h.t.Cleanup(c.srv.Close)
	return &c
}

// storedRemoteControl is whether account id has its remote control setting
// on, read from the store's file.
func (h *harness) storedRemoteControl(id int) bool {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.dir, "accounts.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var doc struct {
		Accounts []struct {
			ID            int  `json:"id"`
			RemoteControl bool `json:"remoteControl"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		h.t.Fatal(err)
	}
	for _, a := range doc.Accounts {
		if a.ID == id {
			return a.RemoteControl
		}
	}
	h.t.Fatalf("no account %d in the store", id)
	return false
}

// remoteOff turns account id's setting off again over the socket, so the
// next request starts from it off.
func (h *harness) remoteOff(id int) {
	h.t.Helper()
	if resp, raw := h.socket().do("PATCH", "/v1/accounts/"+itoa(id), map[string]any{"remote_control": false}); resp.StatusCode != http.StatusOK {
		h.t.Fatalf("turning remote control off: %d %s", resp.StatusCode, raw)
	}
}

func itoa(n int) string { raw, _ := json.Marshal(n); return string(raw) }

// A change to an account's remote_control setting is the control socket's
// to make, like a terminal's remote_control: a bearer token is refused one,
// whatever else it sends, and changes nothing. A remote_control that is what
// the setting already is changes nothing, so it is taken from anybody,
// beside whatever else the request changes. The socket turns it on and off.
func TestPatchRemoteControlIsTakenOnlyOverTheControlSocket(t *testing.T) {
	h := newHarness(t, Options{})
	const want = "remote_control is taken only over the control socket"
	for _, body := range []map[string]any{
		{"remote_control": true},
		{"remote_control": true, "threshold": 70},
	} {
		resp, raw := h.do("PATCH", "/v1/accounts/1", body)
		if resp.StatusCode != http.StatusForbidden || errorOf(string(raw)) != want {
			t.Errorf("a token sending %v answers %d %s, want 403 %q", body, resp.StatusCode, raw, want)
		}
	}
	if h.storedRemoteControl(1) {
		t.Fatal("a refused PATCH turned remote control on")
	}
	resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"remote_control": false, "threshold": 70})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"threshold":70`) {
		t.Fatalf("a token sending the setting as it is, beside a threshold: %d %s", resp.StatusCode, raw)
	}
	local := h.socket()
	if resp, raw := local.do("PATCH", "/v1/accounts/1", map[string]any{"remote_control": true}); resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(raw), `"remote_control":true`) || !h.storedRemoteControl(1) {
		t.Fatalf("on over the socket: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"remote_control": true, "threshold": 60}); resp.StatusCode != http.StatusOK {
		t.Fatalf("a token sending on, which it already is: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"remote_control": false}); resp.StatusCode != http.StatusForbidden || !h.storedRemoteControl(1) {
		t.Fatalf("a token turning it off answers %d %s, want 403 and the setting kept", resp.StatusCode, raw)
	}
	if resp, raw := local.do("PATCH", "/v1/accounts/1", map[string]any{"remote_control": false}); resp.StatusCode != http.StatusOK || h.storedRemoteControl(1) {
		t.Fatalf("off over the socket: %d %s", resp.StatusCode, raw)
	}
}

// A run's raw arguments, where the server allows them at all, are held to
// the same rule. From a bearer token, a claude account's are refused in the
// terminals' sentence, and so is --settings rota cannot read; another
// provider's are not read, having no Remote Control to start. Over the
// socket each spelling readies the account, turning its setting on, but
// only once the run is known to be allowed: a run refused for anything else
// leaves the setting as it was. --settings that cannot be read is Claude
// Code's to refuse there.
func TestARunsArgsCarryingRemoteControl(t *testing.T) {
	h := newHarness(t, Options{AllowRawFlags: true})
	run := func(h *harness, id int, body map[string]any) (int, string) {
		body["prompt"] = "hi"
		resp, raw := h.do("POST", "/v1/accounts/"+itoa(id)+"/run", body)
		return resp.StatusCode, string(raw)
	}
	for _, args := range remoteSpellings {
		if code, raw := run(h, 1, map[string]any{"args": args}); code != http.StatusForbidden || errorOf(raw) != wantArgsLocal {
			t.Errorf("a token running with args %q answers %d %s, want 403 %q", args, code, raw, wantArgsLocal)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if code, raw := run(h, 1, map[string]any{"args": []string{"--settings", missing}}); code != http.StatusForbidden || errorOf(raw) != wantSettingsLocal {
		t.Errorf("a token running with unreadable --settings answers %d %s, want 403 %q", code, raw, wantSettingsLocal)
	}
	for _, args := range plainArgs {
		if code, raw := run(h, 1, map[string]any{"args": args}); code == http.StatusForbidden {
			t.Errorf("a token running with args %q is refused: %d %s", args, code, raw)
		}
	}
	if code, raw := run(h, 2, map[string]any{"args": []string{"--rc"}}); code == http.StatusForbidden {
		t.Errorf("codex's args are read for Claude Code's Remote Control: %d %s", code, raw)
	}
	if h.storedRemoteControl(1) {
		t.Fatal("a token's run turned remote control on")
	}

	local := h.socket()
	for _, args := range remoteSpellings {
		run(local, 1, map[string]any{"args": args})
		if !h.storedRemoteControl(1) {
			t.Errorf("a run over the socket with args %q did not ready the account", args)
		}
		h.remoteOff(1)
	}
	if code, raw := run(local, 1, map[string]any{"args": []string{"--rc"}, "effort": "nope"}); code != http.StatusBadRequest || h.storedRemoteControl(1) {
		t.Fatalf("a refused run over the socket answers %d %s and left the setting %v, want 400 and off", code, raw, h.storedRemoteControl(1))
	}
	for _, args := range plainArgs {
		run(local, 1, map[string]any{"args": args})
		if h.storedRemoteControl(1) {
			t.Fatalf("a run over the socket with args %q turned the setting on", args)
		}
	}
	if code, raw := run(local, 1, map[string]any{"args": []string{"--settings", missing}}); code == http.StatusForbidden {
		t.Errorf("local running with unreadable --settings is refused for it: %d %s", code, raw)
	}
}

// The run WebSocket's first frame carries args as a run's body does, and is
// held to the same rule.
func TestARunSocketsArgsCarryingRemoteControl(t *testing.T) {
	h := newHarness(t, Options{AllowRawFlags: true})
	echoClaude(t)
	c := dialRun(t, h, "/v1/accounts/1/ws")
	c.say(map[string]any{"type": "start", "prompt": "one", "args": []string{"--rc"}})
	e := c.waitType("error")
	if msg, _ := e["error"].(string); msg != wantArgsLocal {
		t.Fatalf("a token's socket starting with --rc is told %v, want %q", e, wantArgsLocal)
	}
	c.expectClose(wsPolicy)
	if h.storedRemoteControl(1) {
		t.Fatal("a refused socket turned remote control on")
	}
}
