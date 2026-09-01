package rota

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// StagePlan is Stage without the disk: the command plus the credential
// files as values, for an application that stores files its own way. The
// owner's rule, verbatim: "It is even correct to return files as functions.
// The app using ./lib should decide where to store it if it is a file."
func TestStagePlanReturnsTheFilesInsteadOfWritingThem(t *testing.T) {
	home := t.TempDir()
	a := &Account{ID: 3, Provider: "codex", Staged: stagedNone,
		Token: Token{Access: "A", Refresh: "R", ExpiresAt: NowMS() + 3_600_000},
		Extra: map[string]string{"id_token": "ID", "account_id": "acct"}}

	cmd, files, err := StagePlan(context.Background(), a, home)
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil || cmd.Bin != "codex" {
		t.Fatalf("the command still comes back: %+v", cmd)
	}
	if len(files) != 1 || files[0].Path != "auth.json" || files[0].Mode != 0o600 {
		t.Fatalf("codex stages one private file: %+v", files)
	}
	body := string(files[0].Content)
	if !strings.Contains(body, `"refresh_token"`) || !strings.Contains(body, "R") {
		t.Fatalf("the file carries the credential: %s", body)
	}
	// The whole point: nothing touched the caller's disk.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("StagePlan must write nothing: %v", entries)
	}
}

// A provider with no file to stage plans an empty list, and the plan still
// carries the command — claude, whose credential travels in the
// environment, is the proof.
func TestAPlanWithNoFilesIsStillAPlan(t *testing.T) {
	a := &Account{ID: 1, Provider: "claude", Token: Token{Access: "tok"}}
	cmd, files, err := StagePlan(context.Background(), a, "")
	if err != nil || cmd == nil || len(files) != 0 {
		t.Fatalf("%+v %+v %v", cmd, files, err)
	}
}

// Adoption reads through fs.FS, so an application that keeps homes
// somewhere other than the local disk hands lib the contents instead of a
// path. Adopt(a, home) stays as the local-disk convenience over it.
func TestAdoptReadsFromAnyFilesystem(t *testing.T) {
	a := &Account{ID: 8, Provider: "grok", Delegated: true}
	fsys := fstest.MapFS{
		"auth.json": &fstest.MapFile{Data: []byte(
			`{"xai|prod": {"user_id": "u-77", "email": "grok@x"}}`)},
	}
	if err := AdoptFrom(a, fsys); err != nil {
		t.Fatal(err)
	}
	if a.Email != "grok@x" || a.UUID != "u-77" {
		t.Fatalf("identity must arrive through the filesystem value: %+v", a)
	}
}
