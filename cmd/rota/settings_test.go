package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A settings file that sets back what a launch drops is said on stderr, as a
// warning naming the account, the file and the key, by a handover and by a
// run alike — once each, and never with the value. The answer on stdout is
// untouched.
func TestALaunchWarnsAboutSettingsThatSendTheLoginElsewhere(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@b.c","order":1,"token":{"accessToken":"t"}}]}`)
	answering(t, "claude")
	handover(t)
	person := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", person)
	settings := filepath.Join(person, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://SECRET.example"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	file, err := filepath.EvalSymlinks(settings)
	if err != nil {
		t.Fatal(err)
	}
	want := "warning: claude/a@b.c: settings Claude Code reads put back what rota keeps out of this account's environment, " +
		"so its login may go to another host or another credential be used in its place: " + file + " sets env.ANTHROPIC_BASE_URL\n"

	_, errOut, code := call(t, "run", "1")
	if code != 0 || strings.Count(errOut, want) != 1 || strings.Contains(errOut, "SECRET") {
		t.Fatalf("the handover warns once, without the value: %d %q", code, errOut)
	}
	out, errOut, code := call(t, "run", "1", "hello")
	if code != 0 || out != "ANSWERED\n" || strings.Count(errOut, want) != 1 || strings.Contains(errOut, "SECRET") {
		t.Fatalf("a run warns once, without the value: %d %q %q", code, out, errOut)
	}
}
