package rota

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestEnvironLeavesExactlyOneValuePerVariable(t *testing.T) {
	inherited := []string{"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=shell", "ANTHROPIC_API_KEY=k", "HOME=/h", "ANTHROPIC_API_KEYS=keep"}
	before := slices.Clone(inherited)
	got := Environ(inherited, &Command{Env: []string{"CLAUDE_CODE_OAUTH_TOKEN=rota"}, Drop: []string{"ANTHROPIC_API_KEY"}})
	want := []string{"PATH=/bin", "HOME=/h", "ANTHROPIC_API_KEYS=keep", "CLAUDE_CODE_OAUTH_TOKEN=rota"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
	if !slices.Equal(inherited, before) {
		t.Fatal("caller's slice was mutated")
	}
}

func TestCliRotatedTellsRotationFromRotaOwnWrites(t *testing.T) {
	a := &Account{Token: Token{Refresh: "store"}}
	cases := []struct {
		staged, file string
		want         bool
	}{
		{"", "", false},
		{"", "store", false},
		{stagedNone, "older-login", false},
		{"", "unknown-provenance", true},
		{fingerprint("file"), "file", false}, // rota staged "file", then refreshed itself
		{fingerprint("store"), "cli-new", true},
	}
	for i, c := range cases {
		a.Staged = c.staged
		if got := a.cliRotated(c.file); got != c.want {
			t.Fatalf("case %d: got %v", i, got)
		}
	}
}

func TestStageWritesPrivateFileAndRecordsWhatWasStaged(t *testing.T) {
	a := &Account{Token: Token{Refresh: "r1"}}
	path := filepath.Join(t.TempDir(), "deep", "auth.json")
	if err := stageRaw(a, path, StagedFile{Path: "auth.json", Mode: 0o600, Content: []byte(`{"k": "v"}`)}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) || a.Staged != fingerprint("r1") {
		t.Fatalf("err=%v mode=%v staged=%q", err, fi.Mode(), a.Staged)
	}
	var out map[string]string
	if !readJSON(path, &out) || out["k"] != "v" {
		t.Fatal("readJSON")
	}
	os.WriteFile(path, []byte("{"), 0o600)
	if readJSON(path, &out) || readJSON(path+".missing", &out) {
		t.Fatal("corrupt or missing files must read as false")
	}
}

// A child is told which account it runs as. The credential stays first —
// callers pin Env[0] to it — and the three identity variables come last, in
// their fixed order, so anything reading them reads the same shape every time.
func TestStageTellsTheChildWhichAccountItRunsAs(t *testing.T) {
	a := &Account{ID: 7, Provider: "claude", Email: "work@example.com", Token: Token{Access: "tok"}}
	cmd, err := Stage(a, "")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Env[0] != "CLAUDE_CODE_OAUTH_TOKEN=tok" {
		t.Fatalf("the credential comes first: %v", cmd.Env)
	}
	want := []string{"ROTA_PROVIDER=claude", "ROTA_ACCOUNT_ID=7", "ROTA_ACCOUNT=work@example.com"}
	if got := cmd.Env[len(cmd.Env)-3:]; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// ROTA_ACCOUNT carries the same label a person sees in rota's own listing:
// the e-mail when there is one, else the shortened UUID, else account-N.
func TestTheAccountVariableCarriesTheLabelAPersonSees(t *testing.T) {
	cases := []struct {
		a    Account
		want string
	}{
		{Account{ID: 1, Provider: "claude", Email: "a@b.c", UUID: "0123456789abcdef"}, "a@b.c"},
		{Account{ID: 2, Provider: "claude", UUID: "0123456789abcdef"}, "0123456789ab..."},
		{Account{ID: 3, Provider: "claude"}, "account-3"},
	}
	for _, c := range cases {
		cmd, err := Stage(&c.a, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := cmd.Env[len(cmd.Env)-1]; got != "ROTA_ACCOUNT="+c.want {
			t.Fatalf("account %d: got %q, want ROTA_ACCOUNT=%s", c.a.ID, got, c.want)
		}
	}
}

// Nesting is the reason the replacement rule matters: a rota run started
// from inside another one inherits the outer ROTA_ACCOUNT, and the child must
// see the account it was actually launched as — once, not twice.
func TestAnInheritedAccountVariableIsReplacedNotDuplicated(t *testing.T) {
	a := &Account{ID: 4, Provider: "claude", Email: "inner@example.com", Token: Token{Access: "tok"}}
	cmd, err := Stage(a, "")
	if err != nil {
		t.Fatal(err)
	}
	inherited := []string{"PATH=/bin", "ROTA_PROVIDER=codex", "ROTA_ACCOUNT_ID=99", "ROTA_ACCOUNT=outer@example.com"}
	got := Environ(inherited, cmd)
	want := []string{"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=tok",
		"ROTA_PROVIDER=claude", "ROTA_ACCOUNT_ID=4", "ROTA_ACCOUNT=inner@example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
}

// The identity travels with a plan as well, so an application that stores
// the staged files itself still gets a command that names the account. codex
// is the provider that plans rather than launches.
func TestAPlanCarriesTheAccountIdentityAsWell(t *testing.T) {
	a := &Account{ID: 12, Provider: "codex", Staged: stagedNone, Email: "planner@example.com",
		Token: Token{Access: "A", Refresh: "R", ExpiresAt: NowMS() + 3_600_000},
		Extra: map[string]string{"id_token": "ID", "account_id": "acct"}}
	cmd, _, err := StagePlan(context.Background(), a, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ROTA_PROVIDER=codex", "ROTA_ACCOUNT_ID=12", "ROTA_ACCOUNT=planner@example.com"}
	if got := cmd.Env[len(cmd.Env)-3:]; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A codex auth.json that already holds the very login rota would stage is
// left as it is, and is known to be rota's. Writing it again would change
// nothing rota keeps, and it would be a write over a file a daemon Codex left
// running in the home may be rotating at that moment: the refresh token it
// had just written would be lost, and the one rota put back is spent. A file
// that differs in anything rota writes is written as before.
func TestCodexLeavesAnAuthFileThatAlreadyHoldsTheLogin(t *testing.T) {
	p, _ := Lookup("codex")
	held := `{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"last_refresh":"2026-01-02T03:04:05Z",` +
		`"tokens":{"id_token":"i","access_token":"a1","refresh_token":"r1","account_id":"acct","login_owner_id":"o"}}`
	for _, c := range []struct {
		what, file, staged string
		kept               bool
	}{
		{"the same login", held, fingerprint("r1"), true},
		// Nothing known of the file, or a file from before this login: the
		// login it holds is still this one, and it is rota's from now on.
		{"the same login, provenance unknown", held, "", true},
		{"the same login, staged before this login", held, stagedNone, true},
		{"another access token", strings.Replace(held, `"a1"`, `"a0"`, 1), fingerprint("r1"), false},
		{"another id token", strings.Replace(held, `"id_token":"i"`, `"id_token":"j"`, 1), fingerprint("r1"), false},
		{"another ChatGPT account", strings.Replace(held, `"account_id":"acct"`, `"account_id":"other"`, 1), fingerprint("r1"), false},
		{"an API key beside it", strings.Replace(held, `"OPENAI_API_KEY":null`, `"OPENAI_API_KEY":"sk-x"`, 1), fingerprint("r1"), false},
		{"no auth mode", strings.Replace(held, `"auth_mode":"chatgpt",`, ``, 1), fingerprint("r1"), false},
	} {
		home := t.TempDir()
		path := filepath.Join(home, "auth.json")
		if err := os.WriteFile(path, []byte(c.file), 0o600); err != nil {
			t.Fatal(err)
		}
		a := &Account{ID: 1, Provider: "codex", Staged: c.staged,
			Token: Token{Access: "a1", Refresh: "r1"}, Extra: map[string]string{"id_token": "i", "account_id": "acct"}}
		if _, err := p.Launch(a, home); err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		raw, _ := os.ReadFile(path)
		if kept := string(raw) == c.file; kept != c.kept {
			t.Fatalf("%s: kept=%v, file now %s", c.what, kept, raw)
		}
		if !a.StagedCurrent() || a.Token.Refresh != "r1" {
			t.Fatalf("%s: the file is rota's staging either way: %+v %q", c.what, a.Token, a.Staged)
		}
	}

	// Fields beside the tokens of a shape nobody expected still leave a
	// rotated token to be adopted.
	home := t.TempDir()
	odd := `{"auth_mode":{"x":1},"OPENAI_API_KEY":7,"tokens":{"id_token":"i","access_token":"a2","refresh_token":"r2","account_id":"acct"}}`
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(odd), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Account{ID: 1, Provider: "codex", Staged: fingerprint("r1"),
		Token: Token{Access: "a1", Refresh: "r1"}, Extra: map[string]string{"id_token": "i", "account_id": "acct"}}
	if _, err := p.Launch(a, home); err != nil || a.Token.Refresh != "r2" {
		t.Fatalf("a rotation beside odd fields is adopted: %v %+v", err, a.Token)
	}
}

// A rotation read back from auth.json is read back once. Adopting a token
// records that the home holds it, so once rota refreshes past that token —
// in the store alone, as maintenance does — the file is known to lag behind
// rota rather than taken for a newer rotation. Taking it for one would put a
// spent refresh token back over the good one, and the next refresh would
// present it a second time, which codex refuses for good.
func TestACodexRotationIsReadBackOnce(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(
		`{"auth_mode":"chatgpt","tokens":{"id_token":"i","access_token":"a2","refresh_token":"r2","account_id":"acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Account{ID: 1, Provider: "codex", Staged: fingerprint("r1"),
		Token: Token{Access: "a1", Refresh: "r1"}, Extra: map[string]string{"id_token": "i", "account_id": "acct"}}
	if err := Adopt(a, home); err != nil || a.Token.Refresh != "r2" || !a.StagedCurrent() {
		t.Fatalf("the CLI's rotation is read back, and the home is known to hold it: %v %+v %q", err, a.Token, a.Staged)
	}
	a.Token = Token{Access: "a3", Refresh: "r3"} // rota refreshed r2 in the store alone
	if err := Adopt(a, home); err != nil || a.Token.Refresh != "r3" {
		t.Fatalf("a file holding a token rota has refreshed past is not read back again: %v %+v", err, a.Token)
	}
}
