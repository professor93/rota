//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/store"
)

// rota send into a held session, and held sessions in rota list --sessions.
// The sends run on the daemon rig: each command is this test binary acting
// as rota in a process of its own, so is the daemon and each holder, and
// the CLI in a held session is the tty fake. The listing reads records
// alone, so its tests plant them and need no daemon.

// A held session is typed into by its name: the text as a paste where the
// CLI turned bracketed paste on, then Enter, and nothing printed — or
// {"ok":true} with --json. --interrupt, --steer and --close are a run's.
func TestSendTypesIntoAHeldSessionByName(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	id, _, _ := r.detach(r.cmd("run", "1", "--detach", "--label", "api"))
	p := r.attach(id)
	p.waitDoc("hello")
	p.waitOut("ready")
	// ESC[?2004h, and a word to know it has been drawn.
	p.send(0x2, []byte("print 1b5b3f32303034687061737465206f6e0d0a\r"))
	p.waitOut("paste on")

	if out, errb, code := r.rota("send", "api", "hello"); code != 0 || out != "" || errb != "" {
		t.Fatalf("send api: exit %d %q %q\nlog:\n%s", code, out, errb, r.log())
	}
	p.waitOut(`got:\x1b[200~hello\x1b[201~`)

	out, errb, code := r.rota("--json", "send", "api", "again")
	var doc map[string]any
	if code != 0 || errb != "" || json.Unmarshal([]byte(out), &doc) != nil || len(doc) != 1 || doc["ok"] != true {
		t.Fatalf("send --json: exit %d %q %q; want {\"ok\":true}", code, out, errb)
	}
	p.waitOut(`got:\x1b[200~again\x1b[201~`)

	for _, flag := range []string{"--interrupt", "--steer", "--close"} {
		_, errb, code := r.rota("send", "api", "x", flag)
		if code != 2 || !strings.Contains(errb, "those are for a run started with --input") {
			t.Fatalf("send api %s: exit %d %q; want a usage error", flag, code, errb)
		}
	}
}

// A session nobody labelled is called after its folder, and the word rota
// run --detach says to type goes on finding it: after its CLI sets a title,
// which never renames it, and after its CLI names its conversation, which
// it is called from then on, by the daemon and by the listing alike.
func TestSendFindsAnUnlabelledSessionByTheWordDetachSaid(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	dir := filepath.Join(t.TempDir(), "api1755")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := r.cmd("run", "1", "--detach")
	cmd.Dir = dir
	id, m, _ := r.detach(cmd)
	if m[3] != "api1755" || m[5] != "api1755" {
		t.Fatalf("an unlabelled session is held as %q and sent to as %q; want its folder, api1755", m[3], m[5])
	}
	word := m[5]
	p := r.attach(id)
	p.waitDoc("hello")
	p.waitOut("ready")

	// ESC ] 0 ; ✳ Fix the tests BEL, as Claude Code sets its title.
	p.send(0x2, []byte("print 1b5d303be29cb3204669782074686520746573747307\r"))
	within(t, "the daemon reads the title", 20*time.Second, func() bool { return r.session(id).Title == "Fix the tests" })
	if s := r.session(id); s.Name != "api1755" {
		t.Fatalf("a title renamed the session %q", s.Name)
	}
	if out, errb, code := r.rota("send", word, "hello"); code != 0 || out != "" || errb != "" {
		t.Fatalf("send %s once the CLI set a title: exit %d %q %q\nlog:\n%s", word, code, out, errb, r.log())
	}
	p.waitOut("got:hello")

	// The CLI names its conversation fake.
	p.send(0x2, []byte("registry idle\r"))
	p.waitOut("registry idle")
	within(t, "the daemon reads the conversation's name", 20*time.Second, func() bool { return r.session(id).Name == "fake" })
	for _, w := range []string{word, "fake"} {
		if out, errb, code := r.rota("send", w, "by "+w); code != 0 || out != "" || errb != "" {
			t.Fatalf("send %s once the CLI named its conversation: exit %d %q %q\nlog:\n%s", w, code, out, errb, r.log())
		}
		p.waitOut("got:by " + w)
	}
	out, errb, code := r.rota("list", "--short", "--sessions")
	if code != 0 || !regexp.MustCompile(`(?m)^  ● fake +#1 a@x +claude +running +\S*api1755 +`+id[:8]+`$`).MatchString(out) {
		t.Fatalf("list --sessions: exit %d %q\n%s", code, errb, out)
	}
}

// A CLI waiting on its person for a permission is not typed into, since
// Enter there may approve a tool; --force types all the same. A session
// that has ended says so.
func TestSendRefusesAWaitingClaude(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	id, _, _ := r.detach(r.cmd("run", "1", "--detach", "--label", "asks"))
	p := r.attach(id)
	p.waitDoc("hello")
	p.waitOut("ready")
	p.send(0x2, []byte("registry waiting permission\r"))
	p.waitOut("registry waiting")
	within(t, "the daemon says the session needs its person", 20*time.Second, func() bool {
		return r.session(id).Status == "needs_you"
	})

	want := "session asks is waiting for a permission or a question, and Enter there may approve a tool; --force sends anyway"
	if out, errb, code := r.rota("send", "asks", "no"); code != 1 || out != "" || !strings.Contains(errb, want) {
		t.Fatalf("send to a waiting claude: exit %d %q %q; want %q", code, out, errb, want)
	}
	out, errb, code := r.rota("--json", "send", "asks", "no")
	var doc map[string]any
	if code != 1 || json.Unmarshal([]byte(out), &doc) != nil || doc["error"] != want || doc["reason"] != "waiting" || len(doc) != 2 {
		t.Fatalf("send --json to a waiting claude: exit %d %q %q; want the sentence and reason waiting", code, out, errb)
	}
	if out, errb, code := r.rota("send", "asks", "yes", "--force"); code != 0 || out != "" || errb != "" {
		t.Fatalf("send --force: exit %d %q %q\nlog:\n%s", code, out, errb, r.log())
	}
	p.waitOut("got:yes")
	if strings.Contains(p.printed(), "got:no") {
		t.Fatalf("the refused send was typed:\n%s", p.printed())
	}

	p.send(0x2, []byte("bye\r"))
	within(t, "the session ends", 20*time.Second, func() bool { return r.session(id).State != "live" })
	if out, errb, code := r.rota("send", id, "late"); code != 1 || out != "" || !strings.Contains(errb, "session asks has ended") {
		t.Fatalf("send to an ended session: exit %d %q %q", code, out, errb)
	}
}

// The run socket is tried first, as it always was: a run listening under a
// held session's name is the one sent into, and the session is typed into
// only when it is named by what names it alone.
func TestSendStillReachesARun(t *testing.T) {
	r := newDaemonRig(t)
	r.start()
	id, _, _ := r.detach(r.cmd("run", "1", "--detach", "--label", "nightly"))

	st, err := store.Open(r.home)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := st.RunDir()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(runs, "nightly.sock"))
	if err != nil {
		t.Skipf("a run cannot be sent into here: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	asked := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		asked <- line
		_, _ = conn.Write([]byte(`{"id":"m1"}` + "\n"))
	}()
	if out, errb, code := r.rota("send", "nightly", "first"); code != 0 || out != "accepted m1\n" {
		t.Fatalf("send to a run: exit %d %q %q", code, out, errb)
	}
	if got := <-asked; !strings.Contains(got, `"first"`) {
		t.Fatalf("the run was asked %q", got)
	}

	p := r.attach(id)
	p.waitDoc("hello")
	p.waitOut("ready")
	if out, errb, code := r.rota("send", id, "second"); code != 0 || out != "" || errb != "" {
		t.Fatalf("send by the session's id: exit %d %q %q", code, out, errb)
	}
	p.waitOut("got:second")
	if strings.Contains(p.printed(), "first") {
		t.Fatalf("the run's message reached the held session too:\n%s", p.printed())
	}

	// Once that run has gone, its name is the session's again.
	ln.Close()
	if out, errb, code := r.rota("send", "nightly", "third"); code != 0 || out != "" || errb != "" {
		t.Fatalf("send by name with the run gone: exit %d %q %q", code, out, errb)
	}
	p.waitOut("got:third")
}

// plantRecord writes a held session's record into terminals, as a holder
// would, holding its lock for the rest of the test when live is true.
func plantRecord(t *testing.T, terminals string, m holdwire.Meta, live bool) {
	t.Helper()
	dir := holdwire.Dir(terminals, m.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if m.Protocol == 0 {
		m.Protocol = holdwire.Major
	}
	if err := holdwire.WriteMeta(dir, &m); err != nil {
		t.Fatal(err)
	}
	if live {
		release, err := holdwire.Lock(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
}

func terminalsOf(t *testing.T) string {
	t.Helper()
	st, err := store.Open(os.Getenv("ROTA_HOME"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir, err := st.TerminalDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Held sessions are listed only when there are some, live ones with ● and
// stopped ones with ○, newest first, read from their records with no daemon
// running; --json carries them under "held" only then.
func TestListShowsHeldSessionsOnlyWhenThereAreSome(t *testing.T) {
	shortHome(t)
	out, errb, code := call(t, "list", "--short", "--sessions")
	if code != 0 || strings.Contains(out, "Held sessions") {
		t.Fatalf("with no records: exit %d %q %q", code, out, errb)
	}
	out, errb, code = call(t, "--json", "list", "--short", "--sessions")
	var doc map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil {
		t.Fatalf("--json with no records: exit %d %q %q", code, out, errb)
	}
	if _, ok := doc["held"]; ok {
		t.Fatalf("--json with no records carries held: %s", out)
	}

	terminals := terminalsOf(t)
	now := time.Now().UTC().Truncate(time.Second)
	const live, old = "a1b2c3d4e5f60718", "0f0e0d0c0b0a0908"
	plantRecord(t, terminals, holdwire.Meta{ID: live, Name: "api", Kind: holdwire.KindAccount, Account: 1,
		Provider: "claude", Cwd: "/src/api", Started: now, HolderPID: os.Getpid()}, true)
	plantRecord(t, terminals, holdwire.Meta{ID: old, Name: "old", Kind: holdwire.KindAccount, Account: 1,
		Provider: "claude", Cwd: "/src/old", Started: now.Add(-time.Hour), Conversation: "conv-1"}, false)

	out, errb, code = call(t, "list", "--short", "--sessions")
	rows := regexp.MustCompile(`(?m)^Held sessions:\n  ● api +#1 a@x +claude +running +/src/api +a1b2c3d4\n  ○ old +#1 a@x +claude +interrupted +/src/old +0f0e0d0c$`)
	if code != 0 || !rows.MatchString(out) {
		t.Fatalf("with a live and an interrupted record: exit %d %q\n%s", code, errb, out)
	}

	out, errb, code = call(t, "--json", "list", "--short", "--sessions")
	var got struct {
		Held []rotaclient.Session `json:"held"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || len(got.Held) != 2 {
		t.Fatalf("--json with two records: exit %d %q\n%s", code, errb, out)
	}
	// Each in the shape GET /v1/terminals lists a session in: the account
	// an object, ended a bool with ended_at beside it once it has stopped.
	var shapes struct {
		Held []map[string]any `json:"held"`
	}
	_ = json.Unmarshal([]byte(out), &shapes)
	for i, want := range []map[string]any{
		{"id": live, "held": true, "ended": false, "state": "live",
			"account": map[string]any{"id": 1.0, "label": "a@x", "provider": "claude"}},
		{"id": old, "held": true, "ended": true, "state": "interrupted", "exit": -1.0, "resumable": true,
			"account": map[string]any{"id": 1.0, "label": "a@x", "provider": "claude"}},
	} {
		for key, v := range want {
			if !reflect.DeepEqual(shapes.Held[i][key], v) {
				t.Errorf("held[%d].%s is %v, want %v", i, key, shapes.Held[i][key], v)
			}
		}
		for _, key := range []string{"account_label", "provider"} {
			if _, ok := shapes.Held[i][key]; ok {
				t.Errorf("held[%d] has %s, which GET /v1/terminals has inside account", i, key)
			}
		}
	}
	if _, ok := shapes.Held[1]["ended_at"].(string); !ok {
		t.Errorf("the stopped one has no ended_at: %v", shapes.Held[1])
	}
	h, s := got.Held[0], got.Held[1]
	if h.ID != live || h.Name != "api" || h.Label != "api" || h.State != "live" || h.Status != "running" ||
		h.Account != 1 || h.AccountLabel != "a@x" || h.Provider != "claude" || h.Cwd != "/src/api" ||
		h.PID != os.Getpid() || !h.Attachable || h.Ended != nil || h.Exit != nil || !h.Started.Equal(now) {
		t.Fatalf("the live one: %+v", h)
	}
	if s.ID != old || s.State != "interrupted" || s.Status != "stopped" || s.Ended == nil || s.Exit == nil || *s.Exit != -1 ||
		!s.Resumable || s.Conversation != "conv-1" || s.Attachable {
		t.Fatalf("the interrupted one: %+v", s)
	}
}

// A record that cannot be read is left out of the listing with one line
// saying so, as Sessions: says a directory it could not read, and --json
// carries the line among its notes. A directory with no record in it yet —
// a holder on its way — is no session, and is passed over without a word.
func TestListSaysWhenItLeavesABrokenRecordOut(t *testing.T) {
	shortHome(t)
	terminals := terminalsOf(t)
	const broken, other, empty = "badbadbadbadbad1", "badbadbadbadbad2", "0000000000000001"
	for _, id := range []string{broken, other, empty} {
		if err := os.MkdirAll(holdwire.Dir(terminals, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(holdwire.MetaPath(holdwire.Dir(terminals, broken)), []byte("not a record"), 0o600); err != nil {
		t.Fatal(err)
	}
	const line = "  note: 1 held session is not listed, since its record cannot be read: " + broken + "\n"
	out, errb, code := call(t, "list", "--short", "--sessions")
	if code != 0 || !strings.HasSuffix(out, "\nHeld sessions:\n"+line) {
		t.Fatalf("with one broken record: exit %d %q\n%s", code, errb, out)
	}

	if err := os.WriteFile(holdwire.MetaPath(holdwire.Dir(terminals, other)), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	plantRecord(t, terminals, holdwire.Meta{ID: "a1b2c3d4e5f60718", Name: "api", Kind: holdwire.KindAccount, Account: 1,
		Provider: "claude", Cwd: "/src/api", Started: time.Now(), HolderPID: os.Getpid()}, true)
	const two = "note: 2 held sessions are not listed, since their records cannot be read: " + broken + ", " + other
	out, errb, code = call(t, "list", "--short", "--sessions")
	if code != 0 || !regexp.MustCompile(`(?m)^Held sessions:\n  ● api .*a1b2c3d4\n  `+two+`\n\z`).MatchString(out) {
		t.Fatalf("with two broken records and a live one: exit %d %q\n%s", code, errb, out)
	}
	out, errb, code = call(t, "--json", "list", "--short", "--sessions")
	var doc struct {
		Notes []string         `json:"notes"`
		Held  []map[string]any `json:"held"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Held) != 1 ||
		len(doc.Notes) == 0 || doc.Notes[len(doc.Notes)-1] != strings.TrimPrefix(two, "note: ") {
		t.Fatalf("--json: exit %d %q\n%s", code, errb, out)
	}
}

// A held session's name, or its folder, with an escape, a bell or a newline
// in it is drawn with each a space wherever rota prints it: in the listing,
// and in what a send says. --json carries the name and the folder as they
// are, and its sentence as it is printed.
func TestAHeldSessionsDirtyNameIsDrawnCleaned(t *testing.T) {
	shortHome(t)
	terminals := terminalsOf(t)
	now := time.Now().UTC().Truncate(time.Second)
	const name, cwd = "evil\x1b]0;owned\x07\nname", "/src/\x1b[2Jdir"
	const live, old = "a1b2c3d4e5f60718", "0f0e0d0c0b0a0908"
	plantRecord(t, terminals, holdwire.Meta{ID: live, Name: name, Kind: holdwire.KindAccount, Account: 1,
		Provider: "claude", Cwd: cwd, Started: now, HolderPID: os.Getpid()}, true)
	exit := 0
	plantRecord(t, terminals, holdwire.Meta{ID: old, Name: name, Kind: holdwire.KindAccount, Account: 1,
		Provider: "claude", Cwd: cwd, Started: now.Add(-time.Hour), Exit: &exit, Ended: now}, false)
	const drawn = "evil ]0;owned  name"

	out, errb, code := call(t, "list", "--short", "--sessions")
	rows := regexp.MustCompile(`(?m)^Held sessions:\n  ● ` + regexp.QuoteMeta(drawn) + ` +#1 a@x +claude +running +/src/ \[2Jdir +a1b2c3d4\n  ○ ` +
		regexp.QuoteMeta(drawn) + ` +#1 a@x +claude +ended +/src/ \[2Jdir +0f0e0d0c$`)
	if code != 0 || strings.ContainsAny(out, "\x1b\x07") || !rows.MatchString(out) {
		t.Fatalf("the listing: exit %d %q\n%q", code, errb, out)
	}
	out, errb, code = call(t, "--json", "list", "--short", "--sessions")
	var got struct {
		Held []rotaclient.Session `json:"held"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || len(got.Held) != 2 ||
		got.Held[0].Name != name || got.Held[0].Label != name || got.Held[0].Cwd != cwd {
		t.Fatalf("--json: exit %d %q\n%s", code, errb, out)
	}

	for target, want := range map[string]string{
		live: "rota: session " + drawn + " is running, but no rota daemon serves ",
		old:  "rota: session " + drawn + " has ended\n",
	} {
		if out, errb, code := call(t, "send", target, "hi"); code != 1 || out != "" ||
			strings.ContainsAny(errb, "\x1b\x07") || !strings.HasPrefix(errb, want) {
			t.Errorf("send %s: exit %d %q %q; want it to begin %q", target, code, out, errb, want)
		}
	}
	out, errb, code = call(t, "--json", "send", old, "hi")
	var doc map[string]any
	if code != 1 || json.Unmarshal([]byte(out), &doc) != nil || doc["error"] != "session "+drawn+" has ended" || doc["reason"] != "ended" {
		t.Errorf("send --json to the stopped one: exit %d %q %q", code, out, errb)
	}
}

// rota list --sessions passes its terminal on to the held sessions too: at
// 60 columns a row with a long title and a long folder fits, and keeps the
// start of its id whole.
func TestListHeldSessionsFitTheTerminalItIsOn(t *testing.T) {
	shortHome(t)
	terminals := terminalsOf(t)
	const cwd = "/Users/someone/GolandProjects/a-rather-long-project-name/services/api"
	plantRecord(t, terminals, holdwire.Meta{ID: "a1b2c3d4e5f60718", Name: "Review of the account rotation when a refresh token is reused",
		Kind: holdwire.KindAccount, Account: 1, Provider: "claude", Cwd: cwd, Started: time.Now(), HolderPID: os.Getpid()}, true)

	piped, errb, code := call(t, "list", "--short", "--sessions")
	if code != 0 || !strings.Contains(piped, cwd+"  a1b2c3d4\n") {
		t.Fatalf("off a terminal the folder is whole: exit %d %q\n%s", code, errb, piped)
	}
	var out, errs bytes.Buffer
	c := &cli{out: &out, err: &errs, in: strings.NewReader(""), cols: 60}
	if err := c.list([]string{"--short", "--sessions"}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if n := screenWidth(line); n > 60 {
			t.Fatalf("a line %d wide at 60 columns: %q\n%s", n, line, out.String())
		}
	}
	if !regexp.MustCompile(`(?m)^Held sessions:\n  ● Review… +#1 a@x +claude +running +…\S+/api  a1b2c3d4$`).MatchString(out.String()) {
		t.Fatalf("the title and the folder cut, the id whole:\n%s", out.String())
	}
}

// With no daemon, a send to a held session says that is why, and a stopped
// one says it has ended; a name that is nothing's says it is neither a run
// nor a session. The records are matched as the daemon's listing is: an id
// it begins before a running session's name, and two of either is an error
// that names both. A target with a path in it is never a run's socket.
func TestSendWithNoDaemonSaysSo(t *testing.T) {
	shortHome(t)
	terminals := terminalsOf(t)
	now := time.Now()
	plantRecord(t, terminals, holdwire.Meta{ID: "a1b2c3d4e5f60718", Name: "api", Kind: holdwire.KindAccount,
		Account: 1, Provider: "claude", Started: now}, true)
	plantRecord(t, terminals, holdwire.Meta{ID: "a1b2ffffffffffff", Name: "cafe", Kind: holdwire.KindAccount,
		Account: 1, Provider: "claude", Started: now}, true)
	exit := 0
	plantRecord(t, terminals, holdwire.Meta{ID: "cafe0d0c0b0a0908", Name: "old", Kind: holdwire.KindAccount,
		Account: 1, Provider: "claude", Started: now.Add(-time.Hour), Exit: &exit, Ended: now}, false)

	home := os.Getenv("ROTA_HOME")
	for target, want := range map[string]string{
		"api":              "session api is running, but no rota daemon serves " + home + " to type into it; rota serve start starts one",
		"a1b2c3":           "session api is running, but no rota daemon serves " + home,
		"a1b2":             `"a1b2" begins the ids of 2 sessions, a1b2c3d4e5f60718 and a1b2ffffffffffff; give more of the id`,
		"cafe":             "session old has ended",
		"cafe0d0c0b0a0908": "session old has ended",
		"old":              "no run or session old; rota list --sessions shows what is running",
		"nobody":           "no run or session nobody; rota list --sessions shows what is running",
		"../terminals/ctl": "no run or session ../terminals/ctl; rota list --sessions shows what is running",
	} {
		if out, errb, code := call(t, "send", target, "hi"); code != 1 || out != "" || !strings.Contains(errb, want) {
			t.Errorf("send %s: exit %d %q %q; want %q", target, code, out, errb, want)
		}
	}
	out, errb, code := call(t, "--json", "send", "cafe0d0c0b0a0908", "hi")
	var doc map[string]any
	if code != 1 || json.Unmarshal([]byte(out), &doc) != nil || doc["error"] != "session old has ended" || doc["reason"] != "ended" {
		t.Errorf("send --json to a stopped session: exit %d %q %q; want the sentence and reason ended", code, out, errb)
	}
	if _, errb, code := call(t, "send", "api", "--close"); code != 2 || !strings.Contains(errb, "those are for a run started with --input") {
		t.Errorf("send api --close with no daemon: exit %d %q; want a usage error", code, errb)
	}

	// A run listening is reached by its id, and never by a path that leads
	// to its socket from somewhere else.
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := st.RunDir()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(runs, "zz.sock"))
	if err != nil {
		t.Skipf("a run cannot be sent into here: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadString('\n')
			_, _ = conn.Write([]byte(`{"id":"m1"}` + "\n"))
			conn.Close()
		}
	}()
	if out, errb, code := call(t, "send", "zz", "hi"); code != 0 || out != "accepted m1\n" {
		t.Fatalf("send zz: exit %d %q %q", code, out, errb)
	}
	if out, errb, code := call(t, "send", "../runs/zz", "hi"); code != 1 || out != "" ||
		!strings.Contains(errb, "no run or session ../runs/zz;") {
		t.Errorf("send ../runs/zz: exit %d %q %q; it is no run's id", code, out, errb)
	}
}

// Each refusal of a send is said in rota's own words, naming the session;
// one that is none of them is the daemon's own sentence.
func TestASendRefusalSaysWhy(t *testing.T) {
	daemon := "the daemon's own words"
	for _, c := range []struct {
		err  error
		want string
	}{
		{&rotaclient.Refusal{Status: 409, Reason: "waiting", Message: daemon},
			"session api is waiting for a permission or a question, and Enter there may approve a tool; --force sends anyway"},
		{&rotaclient.Refusal{Status: 409, Reason: "keyboard", Message: daemon, Holder: "alice"}, "alice holds the keyboard of api"},
		{&rotaclient.Refusal{Status: 409, Reason: "keyboard", Message: daemon, Holder: "al\x1b[2Jice"}, "al [2Jice holds the keyboard of api"},
		{&rotaclient.Refusal{Status: 409, Reason: "keyboard", Message: daemon}, daemon},
		{&rotaclient.Refusal{Status: 504, Reason: "timeout", Message: daemon}, "session api did not become ready within 30s; nothing was typed"},
		{&rotaclient.Refusal{Status: 409, Reason: "ended", Message: daemon}, "session api has ended"},
		{&rotaclient.Refusal{Status: 409, Reason: "not_held", Message: daemon}, daemon},
		{errors.New("the rota daemon on ctl.sock: EOF"), "the rota daemon on ctl.sock: EOF"},
	} {
		if got := sendRefusal("api", c.err); got != c.want {
			t.Errorf("%#v is said %q; want %q", c.err, got, c.want)
		}
	}
}

// A busy account names the held session that has it.
func TestABusyAccountNamesItsSession(t *testing.T) {
	r := newDaemonRig(t)
	writeStore(t, r.home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}},
		{"id":2,"provider":"codex","email":"b@x","order":2,"token":{"accessToken":"tok-codex","refreshToken":"rt-codex"},"extra":{"id_token":"h.p.s"}}]}`)
	bin := ""
	for _, e := range r.env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			bin = filepath.SplitList(v)[0]
		}
	}
	fakecli.Install(t, bin, "codex", fakecli.Spec{Tty: true})
	r.start()
	id, _, _ := r.detach(r.cmd("run", "2", "--detach", "--label", "cx"))

	want := " — held by session cx (" + id + ")"
	for _, args := range [][]string{{"run", "2", "--", "--version"}, {"run", "2", "what is 2+2?"}} {
		if out, errb, code := r.rota(args...); code != 1 || !strings.Contains(errb, want) {
			t.Fatalf("rota %s while it is held: exit %d %q %q; want %q", strings.Join(args, " "), code, out, errb, want)
		}
	}
}
