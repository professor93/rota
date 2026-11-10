//go:build linux || darwin

package api

import (
	"net/http"
	"strings"
	"testing"
)

// Over the control socket, a terminal whose arguments ask for Remote
// Control is readied exactly as one with remote_control is, whether this
// server runs it or a holder does: refused for an account that is not
// Claude Code's, every form alike, and for a claude account its setting
// turned on. Arguments without it start the same account as before.
func TestArgsCarryingRemoteControlAreReadied(t *testing.T) {
	for name, open := range map[string]func(*testing.T) *terms{
		"this server": func(t *testing.T) *terms { return withTerminals(t) },
		"a holder":    func(t *testing.T) *terms { return withHeld(t, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			p := open(t)
			local := p.door(&localControl)
			for _, args := range remoteArgs {
				resp, raw := local.do("POST", "/v1/terminals", map[string]any{"account": 2, "args": args})
				if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "Remote Control is Claude Code's") {
					t.Errorf("codex with args %q answers %d %s, want 400 saying Remote Control is Claude Code's", args, resp.StatusCode, raw)
				}
			}
			if resp, raw := local.do("POST", "/v1/terminals", map[string]any{"account": 2, "args": []string{"--remote-controlled"}}); resp.StatusCode != http.StatusCreated {
				t.Fatalf("codex with args that ask for nothing answers %d %s, want 201", resp.StatusCode, raw)
			}
			local.do("POST", "/v1/terminals", map[string]any{"account": 1, "args": []string{"--remote-control=laptop"}})
			if !p.storedRemoteControl(1) {
				t.Fatal("a terminal asking for Remote Control in its args did not turn the account's setting on")
			}
		})
	}
}
