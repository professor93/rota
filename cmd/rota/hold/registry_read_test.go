//go:build linux || darwin

package hold

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
)

// One read of the registry is one poll of it, so what a poll does with a
// file is tested here a read at a time, with nothing to wait for: a file an
// hour off is some other claude's and changes nothing, the child's own is
// recorded, and a file that has gone keeps what was recorded. Each file is
// written as Claude Code's macOS build writes it, its procStart in UTC
// whatever the zone the CLI and the holder run in.
func TestEachRegistryReadKeepsTheConversationItKnows(t *testing.T) {
	if !inZones(t) {
		return
	}
	onMac(t)
	start := time.Now().Truncate(time.Second)
	readsKeepTheConversation(t, start, 0, macDomain, linuxDomain, func(off time.Duration) string {
		return start.Add(off).UTC().Format(time.ANSIC)
	})
}

// On Linux the same, each file giving its writer's start as Claude Code's
// Linux build does: the tick since boot it started at.
func TestEachRegistryReadOnLinuxGoesByTheTick(t *testing.T) {
	onLinux(t)
	const ticks = 48273615
	other := strings.Replace(linuxDomain, "4026531836", "4026532001", 1)
	readsKeepTheConversation(t, time.Now(), ticks, linuxDomain, other, func(off time.Duration) string {
		return strconv.FormatInt(ticks+int64(off/(10*time.Millisecond)), 10)
	})
}

// readsKeepTheConversation is what each read does, for a child that started
// at start, at tick ticks, in pid space here; foreign is another pid space,
// and procStart is a file's procStart for a writer that started off from
// the child.
func readsKeepTheConversation(t *testing.T, start time.Time, ticks int64, here, foreign string, procStart func(off time.Duration) string) {
	t.Helper()
	const pid = 48213
	dir := t.TempDir()
	home := t.TempDir()
	rec := &metaFile{dir: dir, m: holdwire.Meta{ID: "0123456789abcdef"}}
	if err := holdwire.WriteMeta(dir, &rec.m); err != nil {
		t.Fatal(err)
	}
	h := &holder{
		childStart: start,
		childTicks: ticks,
		child:      &ptyChild{cmd: &exec.Cmd{Process: &os.Process{Pid: pid}}},
		rec:        rec, sess: &Session{},
		log: func(msg string, args ...any) { t.Errorf("the holder logged %q %v", msg, args) },
	}
	h.registry = registryOf(append(os.Environ(), "CLAUDE_CONFIG_DIR="+home), pid)
	write := func(off time.Duration, domain, session string) {
		t.Helper()
		raw := `{"pid":` + strconv.Itoa(pid) + `,"procStart":"` + procStart(off) + `","pidDomain":"` + domain +
			`","sessionId":"` + session + `","cwd":"/work","name":"` + session + `-name","status":"idle","waitingFor":null}`
		if err := os.MkdirAll(filepath.Dir(h.registry), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.registry, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recorded := func() *holdwire.Meta {
		t.Helper()
		m, err := holdwire.ReadMeta(dir)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	write(-time.Hour, here, "somebody-else")
	if last := h.readRegistry(nil); last != nil || recorded().Conversation != "" {
		t.Fatalf("a registry an hour off was believed: %+v, recorded %q", last, recorded().Conversation)
	}
	write(0, foreign, "somebody-else")
	if last := h.readRegistry(nil); last != nil || recorded().Conversation != "" {
		t.Fatalf("a registry from another pid space was believed: %+v, recorded %q", last, recorded().Conversation)
	}

	write(0, here, "mine")
	last := h.readRegistry(nil)
	if m := recorded(); last == nil || last.SessionID != "mine" || m.Conversation != "mine" ||
		m.ConversationName != "mine-name" || m.ConversationCwd != "/work" {
		t.Fatalf("the child's own registry: %+v, recorded %+v", last, m)
	}

	if err := os.Remove(h.registry); err != nil {
		t.Fatal(err)
	}
	if again := h.readRegistry(last); again != last || recorded().Conversation != "mine" {
		t.Fatalf("a registry that went away took the conversation with it: %+v, recorded %q", again, recorded().Conversation)
	}
	write(time.Hour, here, "somebody-else")
	if again := h.readRegistry(last); again != last || recorded().Conversation != "mine" {
		t.Fatalf("another claude's file replaced the conversation: %+v, recorded %q", again, recorded().Conversation)
	}
}

// The file the fake claude writes is its own to the holder on the system
// the test runs on, by nothing but what the system says: no seam, so on
// Linux by the tick it started at and on macOS by its start in UTC, in this
// machine's pid space. A start an hour or a minute off is not.
func TestTheFakesRegistryIsItsOwnHere(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	home := t.TempDir()
	path := fakecli.Install(t, bin, "claude", fakecli.Spec{Tty: true})
	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+home)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := time.AfterFunc(time.Minute, func() { _ = cmd.Process.Kill() })
	t.Cleanup(func() {
		stop.Stop()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if _, err := io.WriteString(in, "registry idle\n"); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(out)
	for {
		if !lines.Scan() {
			t.Fatalf("the fake ended before it wrote its registry: %v", lines.Err())
		}
		if line := strings.TrimSpace(lines.Text()); strings.Contains(line, "registry") {
			if line != "registry idle" {
				t.Fatalf("the fake could not write its registry: %q", line)
			}
			break
		}
	}

	pid := cmd.Process.Pid
	start, err := proc.StartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	ticks, err := proc.StartTicks(pid)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(registryPath(home, pid))
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseRegistry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.SessionID != "fake-"+strconv.Itoa(pid) || f.PidDomain == "" || f.PidDomain != proc.PidDomain() {
		t.Fatalf("the fake's file, in this machine's pid space %q: %s", proc.PidDomain(), raw)
	}
	if startsInTicks {
		if f.ProcTicks != ticks || !f.ProcStart.IsZero() {
			t.Fatalf("on Linux the fake's procStart is the tick it started at, %d: %s", ticks, raw)
		}
	} else if f.ProcTicks != 0 || f.ProcStart.IsZero() {
		t.Fatalf("on macOS the fake's procStart is a time: %s", raw)
	}
	if !f.belongsTo(pid, start, ticks) {
		t.Fatalf("the fake's own file, started at %v and tick %d, is not its own: %s", start, ticks, raw)
	}
	if f.belongsTo(pid, start.Add(-time.Hour), ticks-360000) || f.belongsTo(pid, start.Add(time.Minute), ticks+6000) {
		t.Fatalf("the fake's file is the file of a child started an hour or a minute off: %s", raw)
	}
	if f.belongsTo(pid+1, start, ticks) {
		t.Fatalf("the fake's file is another pid's: %s", raw)
	}
	if _, err := io.WriteString(in, "bye\n"); err != nil {
		t.Fatal(err)
	}
}
