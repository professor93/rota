//go:build linux || darwin

package hold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// One read of the registry is one poll of it, so what a poll does with a
// file is tested here a read at a time, with nothing to wait for: a file an
// hour off is some other claude's and changes nothing, the child's own is
// recorded, and a file that has gone keeps what was recorded.
func TestEachRegistryReadKeepsTheConversationItKnows(t *testing.T) {
	const pid = 48213
	start := time.Now().Truncate(time.Second)
	dir := t.TempDir()
	home := t.TempDir()
	rec := &metaFile{dir: dir, m: holdwire.Meta{ID: "0123456789abcdef"}}
	if err := holdwire.WriteMeta(dir, &rec.m); err != nil {
		t.Fatal(err)
	}
	h := &holder{
		registry: registryPath(home, pid), zone: time.Local, childStart: start,
		child: &ptyChild{cmd: &exec.Cmd{Process: &os.Process{Pid: pid}}},
		rec:   rec, sess: &Session{},
		log: func(msg string, args ...any) { t.Errorf("the holder logged %q %v", msg, args) },
	}
	write := func(started time.Time, session string) {
		t.Helper()
		raw := `{"pid":` + strconv.Itoa(pid) + `,"procStart":"` + started.Local().Format(time.ANSIC) +
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

	write(start.Add(-time.Hour), "somebody-else")
	if last := h.readRegistry(nil); last != nil || recorded().Conversation != "" {
		t.Fatalf("a registry an hour off was believed: %+v, recorded %q", last, recorded().Conversation)
	}

	write(start, "mine")
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
	write(start.Add(time.Hour), "somebody-else")
	if again := h.readRegistry(last); again != last || recorded().Conversation != "mine" {
		t.Fatalf("another claude's file replaced the conversation: %+v, recorded %q", again, recorded().Conversation)
	}
}
