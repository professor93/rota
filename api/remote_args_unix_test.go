//go:build linux || darwin

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
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
				if code, raw := start(p.harness, map[string]any{"account": 1, "args": args}); code != http.StatusForbidden || errorOf(raw) != tcpRefusal(args) {
					t.Errorf("a token with args %q answers %d %s, want 403 %q", args, code, raw, tcpRefusal(args))
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

// A held session is handed its guard through the holder's request when the
// account's remote control setting is off, and nothing when it is on,
// however it was asked for: from a token, or over the control socket as
// rota run --detach asks.
func TestAHeldSessionIsHandedTheGuardOnlyWithRemoteControlOff(t *testing.T) {
	log := filepath.Join(t.TempDir(), "request.json")
	p := withHeld(t, []string{"FAKE_HOLDER_LOG=" + log})
	handed := func() []string { return heldRequest(t, log).Args }
	local := p.door(&localControl)
	for name, h := range map[string]*harness{"a token": p.harness, "the socket": local.harness} {
		if resp, raw := h.do("POST", "/v1/terminals", map[string]any{"account": 1, "args": []string{"--model", "opus"}}); resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, raw)
		}
		r := heldRequest(t, log)
		if heldGuard(t, p, r) == nil || !slices.Equal(guardRest(r.Args), []string{"--model", "opus"}) {
			t.Errorf("%s, remote control off: the holder was handed %q, want the guard's file at both ends", name, r.Args)
		}
	}
	raw, err := os.ReadFile(filepath.Join(p.dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	accounts, _ := doc["accounts"].([]any)
	for _, a := range accounts {
		if a := a.(map[string]any); a["id"] == 3.0 {
			a["remoteControl"] = true
		}
	}
	on, _ := json.Marshal(doc)
	if !strings.Contains(string(on), `"remoteControl":true`) {
		t.Fatal("account 3 is not in the store as this test expects")
	}
	if err := os.WriteFile(filepath.Join(p.dir, "accounts.json"), on, 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, raw := local.do("POST", "/v1/terminals", map[string]any{"account": 3, "args": []string{"--model", "opus"}}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("remote control on: %d %s", resp.StatusCode, raw)
	}
	if got := handed(); !slices.Equal(got, []string{"--model", "opus"}) {
		t.Errorf("remote control on: the holder was handed %q, want the arguments as they were", got)
	}
}

// The holder is told, beside the guarded arguments its CLI runs with, the
// arguments the session's record keeps: the ones it was asked for, before
// the guard, a --settings file still a path. A session started again from
// that record (as Resume will) on an account whose Remote Control is on by
// then carries no guard, since each launch decides the guard afresh.
func TestAHeldSessionsRecordIsTheArgumentsBeforeTheGuard(t *testing.T) {
	log := filepath.Join(t.TempDir(), "request.json")
	p := withHeld(t, []string{"FAKE_HOLDER_LOG=" + log})
	if err := os.WriteFile(filepath.Join(p.root, "mine.json"), []byte(`{"model":"y"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	request := func() holdwire.Request { return heldRequest(t, log) }
	local := p.door(&localControl)
	// No arguments at all: the record keeps none, said as none, rather
	// than the holder falling back to the guarded ones.
	if resp, raw := local.do("POST", "/v1/terminals", map[string]any{"account": 1, "cwd": p.root}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if r := request(); r.Keep == nil || len(r.Keep) != 0 || heldGuard(t, p, r) == nil {
		t.Fatalf("no arguments: kept %#v, handed %q; want an empty Keep and the guard", r.Keep, r.Args)
	}
	given := []string{"--settings", "mine.json", "--model", "opus"}
	if resp, raw := local.do("POST", "/v1/terminals", map[string]any{"account": 1, "cwd": p.root, "args": given}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	r := request()
	if !slices.Equal(r.Keep, given) {
		t.Fatalf("the record is to keep %q, want %q as given", r.Keep, given)
	}
	if doc := heldGuard(t, p, r); doc == nil || doc["model"] != "y" || slices.Contains(r.Args, "mine.json") {
		t.Fatalf("the CLI is handed %q with %s, want the guard with the file folded in", r.Args, r.Guard)
	}
	p.turnOn(1)
	if resp, raw := local.do("POST", "/v1/terminals", map[string]any{"account": 1, "cwd": p.root, "args": r.Keep}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("started again: %d %s", resp.StatusCode, raw)
	}
	again := request()
	if !slices.Equal(again.Args, given) || !slices.Equal(again.Keep, given) || again.Guard != "" {
		t.Fatalf("started again with remote control on: handed %q, kept %q, guard %q; want %q and no guard", again.Args, again.Keep, again.Guard, given)
	}
}

// heldRequest is the request the fake holder logged last.
func heldRequest(t *testing.T, log string) holdwire.Request {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var seen struct {
		Request holdwire.Request `json:"request"`
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatal(err)
	}
	return seen.Request
}

// heldGuard is a held session's guard document, or nil unless the request
// carries one and its arguments name the session's guard file at both ends:
// the document goes to the holder on its input, never on a command line.
func heldGuard(t *testing.T, p *terms, r holdwire.Request) map[string]any {
	t.Helper()
	path := holdwire.GuardFile(holdwire.Dir(p.terminals(), r.ID))
	a := r.Args
	if len(a) < 2 || a[0] != "--settings" || a[1] != path || a[len(a)-2] != "--settings" || a[len(a)-1] != path ||
		strings.Contains(strings.Join(a, " "), "disableRemoteControl") {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.Guard), &doc); err != nil || doc["disableRemoteControl"] != true {
		return nil
	}
	return doc
}

// A terminal this server runs itself has its guard written to a file of its
// own, readable by this user alone in a directory nobody else may enter,
// for as long as its CLI runs, and taken away when the terminal ends.
func TestAServerRunTerminalsGuardIsAPrivateFileForItsLife(t *testing.T) {
	p := withTerminals(t)
	id, _ := p.start(nil)["id"].(string)
	files, _ := filepath.Glob(filepath.Join(p.dir, "terminals", "guards", "*", "guard.json"))
	if len(files) != 1 {
		t.Fatalf("guard files: %q, want one", files)
	}
	if fi, err := os.Stat(files[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the guard's file: %v %v, want 0600", fi, err)
	}
	if di, err := os.Stat(filepath.Dir(files[0])); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("its directory: %v %v, want 0700", di, err)
	}
	if resp, raw := p.do("DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Dir(files[0])); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the guard outlives its terminal")
		}
	}
}

// guardDirs is the guard directories under a store's terminals.
func guardDirs(t *testing.T, dir string) []string {
	t.Helper()
	found, _ := filepath.Glob(filepath.Join(dir, "terminals", "guards", "*"))
	return found
}

// The sweep takes away only what a process that has ended left: the guard
// directory of another process that is alive, here a CLI fake left
// sleeping, is left exactly as it is.
func TestTheGuardSweepLeavesALiveProcesssGuard(t *testing.T) {
	h := newHarness(t, Options{})
	bin := t.TempDir()
	fakecli.Install(t, bin, "sleeper", fakecli.Spec{Sleep: "60s"})
	other := exec.Command(filepath.Join(bin, fakecli.Exe("sleeper")))
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	if other.Process.Pid == os.Getpid() || !proc.Alive(other.Process.Pid) {
		t.Fatal("the other process is not one")
	}
	theirs := filepath.Join(h.dir, "terminals", "guards", strconv.Itoa(other.Process.Pid)+"-theirs")
	if err := os.MkdirAll(theirs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(theirs, "guard.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := h.api.openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, remove, err := guardPlace(st)
	if err != nil {
		t.Fatal(err)
	}
	remove()
	if _, err := os.Stat(filepath.Join(theirs, "guard.json")); err != nil {
		t.Fatalf("a live process's guard was swept: %v", err)
	}
}

// The daemon's sweep takes away a guard a holder that is gone left behind,
// and never one a live session's holder keeps for its CLI.
func TestTheRecordSweepLeavesALiveSessionsGuard(t *testing.T) {
	p := withHeld(t, nil)
	id, _ := p.start(nil)["id"].(string)
	live := holdwire.GuardFile(holdwire.Dir(p.terminals(), id))
	if err := os.WriteFile(live, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	gone := holdwire.Dir(p.terminals(), "00000000000000aa")
	if err := os.MkdirAll(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(holdwire.GuardFile(gone), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !holdwire.Locked(holdwire.LockPath(holdwire.Dir(p.terminals(), id))) {
		t.Fatal("the session is not locked, so this test would prove nothing")
	}
	p.api.sweepRecords()
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a live session's guard was swept: %v", err)
	}
	if _, err := os.Stat(holdwire.GuardFile(gone)); !os.IsNotExist(err) {
		t.Fatalf("a gone holder's guard was left: %v", err)
	}
}

// A server-run start that is refused, or fails, after its guard's place
// was made leaves no guard directory behind; and an account whose Remote
// Control is on already has no place made for a guard at all.
func TestAServerRunStartLeavesNoGuardBehind(t *testing.T) {
	p := withTerminals(t)
	local := p.door(&localControl)
	for _, args := range [][]string{
		{"--settings", "missing.json"},
		{"--managed-settings", `{"disableRemoteControl":false}`},
	} {
		if resp, raw := local.do("POST", "/v1/terminals", map[string]any{"account": 1, "args": args}); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%q: %d %s, want 400", args, resp.StatusCode, raw)
		}
		if left := guardDirs(t, p.dir); len(left) != 0 {
			t.Fatalf("%q left %q", args, left)
		}
	}
	// A failure past the guard's writing: the account's CLI is not there.
	t.Setenv("PATH", t.TempDir())
	if resp, raw := local.do("POST", "/v1/terminals", map[string]any{"account": 1}); resp.StatusCode < 400 {
		t.Fatalf("a start with no CLI to run answers %d %s", resp.StatusCode, raw)
	}
	if left := guardDirs(t, p.dir); len(left) != 0 {
		t.Fatalf("a failed start left %q", left)
	}
	// Remote Control on: no place made.
	ttyClaude(t)
	p.turnOn(3)
	id, _ := p.start(map[string]any{"account": 3})["id"].(string)
	if left := guardDirs(t, p.dir); len(left) != 0 {
		t.Fatalf("an account with Remote Control on had %q made for it", left)
	}
	p.do("DELETE", "/v1/terminals/"+id, nil)
}
