package main

import (
	"github.com/professor93/rota/internal/fakecli"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `rota run 2 --resume <id>` where the session lives in account 1's home:
// the transcript is copied over before the CLI launches, so the rotation's
// promise — continue where the quota ran out — holds across accounts.
func TestResumeFollowsTheRotationAcrossAccounts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROTA_HOME", dir)
	h1, h2 := filepath.Join(dir, "own1"), filepath.Join(dir, "own2")
	seedAccounts(t, dir, `{"accounts":[
		{"id":1,"provider":"claude","uuid":"c1","order":1,"token":{"accessToken":"t1"},"configDir":"`+filepath.ToSlash(h1)+`"},
		{"id":2,"provider":"claude","uuid":"c2","order":2,"token":{"accessToken":"t2"},"configDir":"`+filepath.ToSlash(h2)+`"}],"nextId":3,"ordered":true}`)

	id := "01a00000-0000-7000-8000-00000000d00d"
	rel := filepath.Join("projects", "-tmp-x", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(h1, rel)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h1, rel),
		[]byte(`{"type":"user","cwd":"/tmp/x","sessionId":"`+id+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Lines(`{"type":"result","subtype":"success","is_error":false,"session_id":"s1","result":"ok","num_turns":1}`))
	t.Setenv("PATH", bin)

	_, errb, code := call(t, "run", "2", "--resume", id, "go on")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(h2, rel)); err != nil {
		t.Fatalf("the transcript must be in account 2's home before the launch: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(h1, rel)); !strings.Contains(string(raw), id) {
		t.Fatal("the source must survive")
	}
}
