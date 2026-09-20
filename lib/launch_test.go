package rota

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
