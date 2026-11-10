//go:build linux || darwin

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// A terminal whose claude account's arguments ask for Remote Control, in any
// spelling, is held to remote_control's rule whether this server runs it or
// a holder does. From a bearer token it is refused in one sentence naming
// the control socket, and the account is left as it was; so is --settings
// rota cannot read. Another provider's arguments are not read, and neither
// are a shell's. Over the socket each spelling readies the account, its
// setting turned on; arguments that only come close leave it off.
func TestArgsCarryingRemoteControlAreReadied(t *testing.T) {
	for name, open := range map[string]func(*testing.T) *terms{
		"this server": func(t *testing.T) *terms { return withTerminals(t) },
		"a holder":    func(t *testing.T) *terms { return withHeld(t, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			p := open(t)
			start := func(h *harness, body map[string]any) (int, string) {
				resp, raw := h.do("POST", "/v1/terminals", body)
				return resp.StatusCode, string(raw)
			}
			for _, args := range remoteSpellings {
				if code, raw := start(p.harness, map[string]any{"account": 1, "args": args}); code != http.StatusForbidden || errorOf(raw) != wantArgsLocal {
					t.Errorf("a token with args %q answers %d %s, want 403 %q", args, code, raw, wantArgsLocal)
				}
			}
			missing := filepath.Join(t.TempDir(), "missing.json")
			if code, raw := start(p.harness, map[string]any{"account": 1, "args": []string{"--settings", missing}}); code != http.StatusForbidden || errorOf(raw) != wantSettingsLocal {
				t.Errorf("a token with unreadable --settings answers %d %s, want 403 %q", code, raw, wantSettingsLocal)
			}
			if p.storedRemoteControl(1) {
				t.Fatal("a token's terminal turned remote control on")
			}
			if code, raw := start(p.harness, map[string]any{"account": 2, "args": []string{"--rc"}}); code != http.StatusCreated {
				t.Errorf("codex with --rc from a token answers %d %s, want 201: codex has no Remote Control", code, raw)
			}
			if _, raw := start(p.harness, map[string]any{"kind": "shell", "args": []string{"--rc"}}); errorOf(raw) == wantArgsLocal {
				t.Errorf("a shell's own arguments are refused as Claude Code's: %s", raw)
			}

			local := p.door(&localControl)
			for _, args := range remoteSpellings {
				start(local.harness, map[string]any{"account": 1, "args": args})
				if !p.storedRemoteControl(1) {
					t.Errorf("a terminal over the socket with args %q did not ready the account", args)
				}
				p.remoteOff(1)
			}
			for _, args := range plainArgs {
				start(local.harness, map[string]any{"account": 3, "args": args})
				if p.storedRemoteControl(3) {
					t.Fatalf("a terminal over the socket with args %q turned the setting on", args)
				}
			}
			// A file the CLI is pointed at, relative to where it starts.
			cwd := filepath.Join(p.root, "rc")
			if err := os.MkdirAll(cwd, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "rc.json"), []byte(`{"remoteControlAtStartup":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if code, raw := start(p.harness, map[string]any{"account": 1, "cwd": cwd, "args": []string{"--settings", "rc.json"}}); code != http.StatusForbidden || errorOf(raw) != wantArgsLocal {
				t.Errorf("a token with --settings naming a file that turns it on answers %d %s, want 403", code, raw)
			}
		})
	}
}
