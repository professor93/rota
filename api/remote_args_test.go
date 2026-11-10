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

// These are about Remote Control asked for by a CLI's arguments rather than
// by remote_control: Claude Code's flag, bare or with a name, and its
// remote-control subcommand. Whatever carries Remote Control is held to the
// option's rule — taken only over the control socket, and there readied by
// the store's one rule for it — and so is an account's remote_control
// setting.

// remoteArgs is every way a CLI's arguments ask for Remote Control: the
// flag, the flag with a name, the subcommand, either after other flags, and
// the subcommand after "--", which is still the first operand.
var remoteArgs = [][]string{
	{"--remote-control"},
	{"--remote-control=laptop"},
	{"remote-control"},
	{"--model", "opus", "remote-control"},
	{"--resume", "abc", "--remote-control"},
	{"--", "remote-control"},
}

// plainArgs carry no Remote Control, however close they come.
var plainArgs = [][]string{
	{"--model", "opus"},
	{"--remote-controlled"},
	{"remote"},
	{"--name", "remote control"},
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

// A terminal whose arguments ask for Remote Control is refused from a
// bearer token, and from a user even through LocalHandler, before anything
// is chosen or started, in one sentence that names the control socket.
// Arguments that do not ask for it, and a shell's, which are not Claude
// Code's, are not refused for it.
func TestRemoteControlInArgsIsTakenOnlyOverTheControlSocket(t *testing.T) {
	s, err := New(Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1, Routes: &Routes{API: true, WebSocket: true, Terminal: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	user := s.LocalHandler(&Principal{Name: "alice", Role: RoleControl, Via: "user"})
	const want = "--remote-control and the remote-control subcommand are taken only over the control socket, as remote_control is"
	body := func(kind string, args []string) string {
		raw, _ := json.Marshal(map[string]any{"account": 1, "kind": kind, "args": args})
		return string(raw)
	}
	for name, h := range map[string]http.Handler{"a token": s.Handler(), "a user": user} {
		for _, args := range remoteArgs {
			if code, answer := ask(h, "POST", "/v1/terminals", body("", args), "Authorization", "Bearer x"); code != http.StatusForbidden || errorOf(answer) != want {
				t.Errorf("%s sending args %q answers %d %s, want 403 %q", name, args, code, answer, want)
			}
		}
		for _, args := range plainArgs {
			if code, answer := ask(h, "POST", "/v1/terminals", body("", args), "Authorization", "Bearer x"); code == http.StatusForbidden {
				t.Errorf("%s sending args %q is refused: %d %s", name, args, code, answer)
			}
		}
		raw, _ := json.Marshal(map[string]any{"kind": "shell", "args": []string{"--remote-control"}})
		if _, answer := ask(h, "POST", "/v1/terminals", string(raw), "Authorization", "Bearer x"); errorOf(answer) == want {
			t.Errorf("%s: a shell's own arguments are refused as Claude Code's: %s", name, answer)
		}
	}
}

// An account's remote_control setting is the control socket's to change,
// like a terminal's remote_control: a bearer token is refused it, on or off,
// and changes nothing; the socket turns it on and off; and the token still
// changes everything else.
func TestPatchRemoteControlIsTakenOnlyOverTheControlSocket(t *testing.T) {
	h := newHarness(t, Options{})
	const want = "remote_control is taken only over the control socket"
	for _, on := range []bool{true, false} {
		resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"remote_control": on})
		if resp.StatusCode != http.StatusForbidden || errorOf(string(raw)) != want {
			t.Errorf("a token sending remote_control %v answers %d %s, want 403 %q", on, resp.StatusCode, raw, want)
		}
	}
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"remote_control": true, "threshold": 70}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("remote_control beside another field from a token answers %d %s, want 403", resp.StatusCode, raw)
	}
	if h.storedRemoteControl(1) {
		t.Fatal("a refused PATCH turned remote control on")
	}
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{"threshold": 70}); resp.StatusCode != http.StatusOK {
		t.Fatalf("a token changing the threshold answers %d %s", resp.StatusCode, raw)
	}
	local := h.socket()
	if resp, raw := local.do("PATCH", "/v1/accounts/1", map[string]any{"remote_control": true}); resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(raw), `"remote_control":true`) || !h.storedRemoteControl(1) {
		t.Fatalf("on over the socket: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := local.do("PATCH", "/v1/accounts/1", map[string]any{"remote_control": false}); resp.StatusCode != http.StatusOK || h.storedRemoteControl(1) {
		t.Fatalf("off over the socket: %d %s", resp.StatusCode, raw)
	}
}

// A run's raw arguments, where the server allows them at all, are held to
// the same rule: Remote Control in them is refused from a bearer token in
// the terminals' sentence, and over the socket the account is readied by
// the store's rule — refused when it is not Claude Code's, and its setting
// turned on when it is.
func TestARunsArgsCarryingRemoteControl(t *testing.T) {
	h := newHarness(t, Options{AllowRawFlags: true})
	const want = "--remote-control and the remote-control subcommand are taken only over the control socket, as remote_control is"
	for _, args := range remoteArgs {
		resp, raw := h.do("POST", "/v1/accounts/2/run", map[string]any{"prompt": "hi", "args": args})
		if resp.StatusCode != http.StatusForbidden || errorOf(string(raw)) != want {
			t.Errorf("a token running with args %q answers %d %s, want 403 %q", args, resp.StatusCode, raw, want)
		}
	}
	if resp, raw := h.do("POST", "/v1/accounts/2/run", map[string]any{"prompt": "hi", "args": []string{"--remote-controlled"}}); resp.StatusCode == http.StatusForbidden {
		t.Errorf("args without Remote Control are refused: %d %s", resp.StatusCode, raw)
	}
	local := h.socket()
	for _, args := range remoteArgs {
		resp, raw := local.do("POST", "/v1/accounts/2/run", map[string]any{"prompt": "hi", "args": args})
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "Remote Control is Claude Code's") {
			t.Errorf("local running codex with args %q answers %d %s, want 400 saying Remote Control is Claude Code's", args, resp.StatusCode, raw)
		}
	}
	local.do("POST", "/v1/accounts/1/run", map[string]any{"prompt": "hi", "args": []string{"remote-control"}})
	if !h.storedRemoteControl(1) {
		t.Fatal("a run asking for Remote Control over the socket did not turn the account's setting on")
	}
}
