package rota

import (
	"os"
	"path/filepath"
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
	if err != nil || fi.Mode().Perm() != 0o600 || a.Staged != fingerprint("r1") {
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
