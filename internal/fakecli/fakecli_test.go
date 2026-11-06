package fakecli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTheTtyFakePrintsAnyBytesAndShowsControlBytesItReads drives the tty
// fake the way a capture does: a print line writes exactly its bytes, with no
// newline, and a line with control bytes in it comes back with each one
// spelled out, so a test can see what arrived without the terminal acting on
// it.
func TestTheTtyFakePrintsAnyBytesAndShowsControlBytesItReads(t *testing.T) {
	in := strings.Join([]string{
		"print 1b5b3f3130343968",
		"print e4b896",
		"print zz",
		"a\x1bb\x7fc\x01",
		"plain words",
		"bye",
	}, "\n") + "\n"
	var out bytes.Buffer
	if code := atty(Spec{Tty: true}, strings.NewReader(in), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	_, got, ok := strings.Cut(out.String(), "\r\n")
	if !ok {
		t.Fatalf("no ready line: %q", out.String())
	}
	want := "\x1b[?1049h" + "世" +
		"bad print: zz\r\n" +
		`got:a\x1bb\x7fc\x01` + "\r\n" +
		"got:plain words\r\n" +
		"bye\r\n"
	if got != want {
		t.Fatalf("the fake wrote\n%q\nwant\n%q", got, want)
	}
}

// TestTheTtyFakeSaysWhatItsEnvironmentHolds: env names one variable, envhas
// looks for some text anywhere in the environment, and registry writes the
// file Claude Code keeps for a running session where Claude Code keeps it.
func TestTheTtyFakeSaysWhatItsEnvironmentHolds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("FAKECLI_SET", "a value")
	in := strings.Join([]string{
		"env FAKECLI_SET",
		"env FAKECLI_NEVER_SET",
		"envhas a valu",
		"envhas FAKECLI_SE",
		"envhas nothing-has-this-0f3b",
		"registry idle",
		"registry waiting permission",
		"bye",
	}, "\n") + "\n"
	var out bytes.Buffer
	if code := atty(Spec{Tty: true}, strings.NewReader(in), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	_, got, _ := strings.Cut(out.String(), "\r\n")
	want := "env FAKECLI_SET=a value\r\n" +
		"env FAKECLI_NEVER_SET unset\r\n" +
		"envhas yes\r\n" + "envhas yes\r\n" + "envhas no\r\n" +
		"registry idle\r\n" + "registry waiting\r\n" +
		"bye\r\n"
	if got != want {
		t.Fatalf("the fake wrote\n%q\nwant\n%q", got, want)
	}

	raw, err := os.ReadFile(filepath.Join(home, "sessions", strconv.Itoa(os.Getpid())+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var reg map[string]any
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(reg))
	for k := range reg {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"cwd", "kind", "name", "pid", "procStart", "sessionId", "startedAt", "status", "updatedAt", "version", "waitingFor"}; !slices.Equal(keys, want) {
		t.Fatalf("a registry file has the keys %v, want %v", keys, want)
	}
	if reg["sessionId"] != "fake-"+strconv.Itoa(os.Getpid()) || reg["status"] != "waiting" ||
		reg["waitingFor"] != "permission" || reg["kind"] != "interactive" || reg["version"] != "2.1.318" ||
		reg["name"] != "fake" || reg["pid"] != float64(os.Getpid()) {
		t.Fatalf("the last line wrote %s", raw)
	}
	start, err := time.ParseInLocation(time.ANSIC, reg["procStart"].(string), time.Local)
	if err != nil {
		t.Fatalf("procStart is not ANSIC: %v", err)
	}
	if d := time.Since(start); d < -time.Second || d > time.Hour {
		t.Fatalf("procStart %v is not this process's own start", start)
	}
}

// TestTheTtyFakeWritesANullWaitingForWhenNotWaiting: a registry line with no
// second word is a session waiting for nothing, which Claude Code writes as
// null.
func TestTheTtyFakeWritesANullWaitingForWhenNotWaiting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	var out bytes.Buffer
	atty(Spec{Tty: true}, strings.NewReader("registry idle\nbye\n"), &out)
	raw, err := os.ReadFile(filepath.Join(home, "sessions", strconv.Itoa(os.Getpid())+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"waitingFor":null`) {
		t.Fatalf("waitingFor is null when nothing is waited for: %s", raw)
	}
}
