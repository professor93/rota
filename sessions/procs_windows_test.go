package sessions

import "testing"

// The Windows process table comes as "pid|path" lines, paths with spaces
// and backslashes included, and a vendor CLI is recognised by its name
// whichever separator the path uses.
func TestWindowsProcessListNamesTheCLIs(t *testing.T) {
	out := "12|C:\\Program Files\\Git\\usr\\bin\\bash.exe\r\n" +
		"340|C:\\Users\\me\\AppData\\Roaming\\npm\\claude.exe\r\n" +
		"341|\r\n" +
		"x|C:\\bad\r\n" +
		"350|D:\\tools\\codex.exe\r\n"
	list := parseWindowsList(out)
	if len(list) != 3 || list[0].pid != 12 || list[1].pid != 340 || list[2].pid != 350 {
		t.Fatalf("%+v", list)
	}
	if cliFor(list[0].command) != "" || cliFor(list[1].command) != "claude" || cliFor(list[2].command) != "codex" {
		t.Fatalf("%q %q %q", cliFor(list[0].command), cliFor(list[1].command), cliFor(list[2].command))
	}
	if cliFor("claude") != "" {
		t.Fatal("a bare name is not a path")
	}
}
