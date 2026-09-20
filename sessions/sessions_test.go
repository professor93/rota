package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// writeLines makes a transcript out of the lines given, creating its
// directory. The shapes here are the real ones, taken off disk.
func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An account told where it belongs keeps everything there. One that was not
// depends on its provider: codex, grok and kimi are always pointed at a
// private home, so their sessions belong to exactly one account, while Claude
// Code is left reading the person's own — which every other such account
// reads too, so no account owns what is found there.
func TestConfigHomeSaysWhoOwnsWhatIsFound(t *testing.T) {
	staged := "/rota/homes/claude-1"
	own := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", own)

	dir, shared := ConfigHome(&rota.Account{ID: 1, Provider: "claude"}, staged)
	if dir != own || !shared {
		t.Fatalf("a claude account with no project reads the person's own home, shared: %q %v", dir, shared)
	}
	dir, shared = ConfigHome(&rota.Account{ID: 1, Provider: "claude", ConfigDir: "/srv/api"}, staged)
	if dir != "/srv/api" || shared {
		t.Fatalf("one told where it belongs owns what is there: %q %v", dir, shared)
	}
	// codex always gets CODEX_HOME, so the staged home is its own.
	dir, shared = ConfigHome(&rota.Account{ID: 3, Provider: "codex"}, "/rota/homes/codex-3")
	if dir != "/rota/homes/codex-3" || shared {
		t.Fatalf("codex is always given a private home: %q %v", dir, shared)
	}

	// A claude account keeping its conversations to itself has them in its
	// own home, among the mirror's links, and owns them.
	dir, shared = ConfigHome(&rota.Account{ID: 1, Provider: "claude", Sessions: rota.SessionsOwn}, staged)
	if dir != staged || shared {
		t.Fatalf("its own home, and nobody else's: %q %v", dir, shared)
	}
	// One pointed at a folder has them there, wherever its configuration is.
	for _, a := range []*rota.Account{
		{ID: 1, Provider: "claude", Sessions: "/srv/threads/"},
		{ID: 2, Provider: "claude", Sessions: "/srv/threads", ConfigDir: "/srv/api"},
	} {
		// Cleaned, so two accounts naming one folder name one folder and a
		// scan does not count it twice.
		if dir, shared = ConfigHome(a, staged); dir != "/srv/threads" || shared {
			t.Fatalf("#%d: the folder it was pointed at: %q %v", a.ID, dir, shared)
		}
	}
}

// Claude Code keeps one directory per project and one file per conversation.
// The directory name is the project path with the separators replaced, which
// cannot be undone — "a-b" and "a/b" encode alike — so the cwd is read out of
// the transcript, where it is written exactly.
func TestClaudeSessionsComeBackWithTheirRealDirectory(t *testing.T) {
	home := t.TempDir()
	id := "aaaaaaaa-1111-4111-8111-111111111111"
	writeLines(t, filepath.Join(home, "projects", "-Users-me-src-a-b", id+".jsonl"),
		`{"type":"queue-operation","sessionId":"`+id+`","timestamp":"2026-08-27T22:59:00.251Z"}`,
		`{"type":"summary"}`,
		`{"type":"user","cwd":"/Users/me/src/a-b","sessionId":"`+id+`"}`)

	got, total, err := In(&rota.Account{ID: 1, Provider: "claude"}, home, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || total != 1 {
		t.Fatalf("one conversation, got %d of %d: %+v", len(got), total, got)
	}
	if got[0].ID != id {
		t.Fatalf("the id is what --resume takes: %q", got[0].ID)
	}
	if got[0].Dir != "/Users/me/src/a-b" {
		t.Fatalf("the directory must come from the transcript, not the folder name: %q", got[0].Dir)
	}
	if got[0].At.IsZero() {
		t.Fatal("a session must say when it was last touched")
	}
}

// named lists one claude conversation written from these records.
func named(t *testing.T, lines ...string) Session {
	t.Helper()
	home := t.TempDir()
	id := "aaaaaaaa-1111-4111-8111-111111111111"
	writeLines(t, filepath.Join(home, "projects", "-tmp-x", id+".jsonl"), lines...)
	got, _, err := In(&rota.Account{ID: 1, Provider: "claude"}, home, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("one conversation, got %+v", got)
	}
	return got[0]
}

// A conversation is shown by the name Claude Code gave it. The title is
// rewritten on every turn as it learns what the conversation is about, so
// the last one is the current one — and a conversation named only as an
// agent is shown by that name.
func TestAConversationIsShownByTheNameItsCliGaveIt(t *testing.T) {
	const id = `"sessionId":"aaaaaaaa-1111-4111-8111-111111111111"`
	got := named(t,
		`{"type":"user","cwd":"/tmp/x","message":{"role":"user","content":"look at the store"},`+id+`}`,
		`{"type":"ai-title","aiTitle":"first guess",`+id+`}`,
		`{"type":"agent-name","agentName":"the agent's name",`+id+`}`,
		`{"type":"ai-title","aiTitle":"store mirror conversation names",`+id+`}`)
	if got.Name != "store mirror conversation names" {
		t.Fatalf("the last title is the current one: %q", got.Name)
	}
	if got = named(t,
		`{"type":"user","cwd":"/tmp/x","message":{"role":"user","content":"hi"},`+id+`}`,
		`{"type":"agent-name","agentName":"the agent's name",`+id+`}`,
	); got.Name != "the agent's name" {
		t.Fatalf("with no title of its own, the agent names it: %q", got.Name)
	}
}

// A conversation nobody named is shown by what it was asked first, which is
// how Claude Code's own picker names one. Plenty of records are shaped like
// a person speaking without being one — the caveat before a local command,
// a tool's result fed back, a sidechain a subagent held — and the one thing
// among them a person would recognise is the slash command they typed.
func TestAnUnnamedConversationIsShownByWhatItWasAskedFirst(t *testing.T) {
	const id = `"sessionId":"aaaaaaaa-1111-4111-8111-111111111111"`
	got := named(t,
		`{"type":"user","isMeta":true,"cwd":"/tmp/x","message":{"role":"user","content":"<local-command-caveat>Caveat: ...</local-command-caveat>"},`+id+`}`,
		`{"type":"user","isSidechain":true,"message":{"role":"user","content":"a subagent's own prompt"},`+id+`}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]},`+id+`}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"  read the mirror  \nand the tests"}]},`+id+`}`)
	if got.Name != "read the mirror" {
		t.Fatalf("the first line of the first thing said: %q", got.Name)
	}
	if got.Dir != "/tmp/x" {
		t.Fatalf("and the directory is still read: %q", got.Dir)
	}

	if got = named(t,
		`{"type":"user","cwd":"/tmp/x","message":{"role":"user","content":"<command-message>statusline</command-message>\n<command-name>/statusline</command-name>"},`+id+`}`,
	); got.Name != "/statusline" {
		t.Fatalf("a slash command is what the person typed: %q", got.Name)
	}

	// Nothing said and nothing named is no name, rather than a guess made
	// from the file name or the folder.
	if got = named(t,
		`{"type":"queue-operation",`+id+`}`,
		`{"type":"assistant","message":{"role":"assistant","content":"hello"},`+id+`}`,
	); got.Name != "" {
		t.Fatalf("nothing to go on means nothing: %q", got.Name)
	}
}

// A name is one line of a column and not a paragraph: whitespace collapses
// and a long one is cut with an ellipsis, so a row stays a row.
func TestALongNameIsCutToFitARow(t *testing.T) {
	long := strings.Repeat("dispatch ", 20)
	got := named(t, `{"type":"ai-title","aiTitle":"`+long+`","sessionId":"aaaaaaaa-1111-4111-8111-111111111111"}`)
	if r := []rune(got.Name); len(r) != maxName+1 || !strings.HasSuffix(got.Name, "…") {
		t.Fatalf("%d runes: %q", len(r), got.Name)
	}
}

// A transcript is read at its two ends and never in the middle. Conversations
// reach tens of megabytes and a listing reads one file per row, so the title
// is looked for in the last stretch of the file and the opening prompt in the
// first — a title that is in neither is a name not found rather than a
// listing that reads a gigabyte.
func TestOnlyTheEndsOfATranscriptAreRead(t *testing.T) {
	const id = `"sessionId":"aaaaaaaa-1111-4111-8111-111111111111"`
	padding := func(n int) string {
		return `{"type":"assistant","message":{"role":"assistant","content":"` + strings.Repeat("x", n) + `"},` + id + `}`
	}
	got := named(t,
		`{"type":"user","cwd":"/tmp/x","message":{"role":"user","content":"the opening prompt"},`+id+`}`,
		padding(promptWindow),
		`{"type":"ai-title","aiTitle":"a title nobody will see",`+id+`}`,
		padding(2*titleWindow),
		`{"type":"assistant","message":{"role":"assistant","content":"done"},`+id+`}`)
	if got.Name != "the opening prompt" {
		t.Fatalf("a title outside both windows is not looked for: %q", got.Name)
	}

	// The tail window opens in the middle of whatever record spans it, and
	// half a record is not a record: a fragment that reads like a title is
	// not one, and the real title after it is.
	got = named(t,
		`{"type":"user","cwd":"/tmp/x","message":{"role":"user","content":"the opening prompt"},`+id+`}`,
		`{"type":"assistant","message":{"role":"assistant","content":"`+strings.Repeat("x", titleWindow)+
			`{\"type\":\"ai-title\",\"aiTitle\":\"quoted, not written\"}"},`+id+`}`,
		`{"type":"ai-title","aiTitle":"the real one",`+id+`}`)
	if got.Name != "the real one" {
		t.Fatalf("the record that spans the window edge must not be half-read: %q", got.Name)
	}
}

// codex files them by date rather than by project, and writes a session_meta
// record first with the exact directory in it.
func TestCodexSessionsComeBackWithTheirRealDirectory(t *testing.T) {
	home := t.TempDir()
	id := "01a00000-0000-7000-8000-000000000002"
	writeLines(t, filepath.Join(home, "sessions", "2026", "08", "27", "rollout-2026-08-27T21-09-13-"+id+".jsonl"),
		`{"timestamp":"2026-08-27T16:09:13.518Z","type":"session_meta","payload":{"session_id":"`+id+`","cwd":"/Users/me/work","originator":"codex_exec"}}`,
		`{"type":"message"}`)

	got, total, err := In(&rota.Account{ID: 3, Provider: "codex"}, home, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || total != 1 || got[0].ID != id || got[0].Dir != "/Users/me/work" {
		t.Fatalf("%d: %+v", total, got)
	}
}

// Newest first, because the one worth resuming is almost always the last one.
func TestSessionsComeBackNewestFirst(t *testing.T) {
	home := t.TempDir()
	for i, id := range []string{"aaaaaaaa-0000-0000-0000-000000000001", "bbbbbbbb-0000-0000-0000-000000000002"} {
		p := filepath.Join(home, "projects", "-tmp-x", id+".jsonl")
		writeLines(t, p, `{"type":"user","cwd":"/tmp/x","sessionId":"`+id+`"}`)
		when := time.Now().Add(time.Duration(-i) * time.Hour)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := In(&rota.Account{ID: 1, Provider: "claude"}, home, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].At.After(got[1].At) {
		t.Fatalf("newest first: %+v", got)
	}
}

// A provider whose sessions rota cannot find says nothing rather than
// guessing at a layout, and an empty or missing home is not an error: an
// account that has never run has no conversations, which is not a fault.
func TestUnknownProvidersAndEmptyHomesAreQuiet(t *testing.T) {
	got, _, err := In(&rota.Account{ID: 1, Provider: "kimi"}, t.TempDir(), 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("kimi: %v %+v", err, got)
	}
	for _, provider := range []string{"claude", "codex", "grok"} {
		got, _, err := In(&rota.Account{ID: 1, Provider: provider}, filepath.Join(t.TempDir(), "never-run"), 0)
		if err != nil || len(got) != 0 {
			t.Fatalf("a missing home is not an error: %s: %v %+v", provider, err, got)
		}
	}
}

// A transcript that says nothing about where it ran still counts: the
// conversation is resumable, and a missing directory is reported as missing
// rather than guessed at from the folder name.
func TestASessionWithNoRecordedDirectoryIsStillListed(t *testing.T) {
	home := t.TempDir()
	id := "cccccccc-0000-0000-0000-000000000003"
	writeLines(t, filepath.Join(home, "projects", "-tmp-y", id+".jsonl"), `{"type":"queue-operation"}`)
	got, _, err := In(&rota.Account{ID: 1, Provider: "claude"}, home, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Dir != "" {
		t.Fatalf("%+v", got)
	}
}

// A limit returns the newest few and still says how many there are, and only
// those few are opened.
//
// A person's own Claude Code home holds thousands of transcripts. Reading the
// head of every one to show five would cost a visible pause for nothing, so
// the count of files actually opened is pinned rather than assumed.
func TestALimitReturnsTheNewestFewAndOpensOnlyThose(t *testing.T) {
	home := t.TempDir()
	for i := range 6 {
		id := fmt.Sprintf("aaaaaaaa-0000-0000-0000-00000000000%d", i)
		p := filepath.Join(home, "projects", "-tmp-x", id+".jsonl")
		writeLines(t, p, `{"type":"user","cwd":"/tmp/x","sessionId":"`+id+`"}`)
		when := time.Now().Add(time.Duration(-i) * time.Hour)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}

	opened := 0
	orig := openTranscript
	t.Cleanup(func() { openTranscript = orig })
	openTranscript = func(path string) (*os.File, error) {
		opened++
		return orig(path)
	}

	got, total, err := In(&rota.Account{ID: 1, Provider: "claude"}, home, 5)
	if err != nil {
		t.Fatal(err)
	}
	if total != 6 {
		t.Fatalf("the total counts them all, not just the ones shown: %d", total)
	}
	if len(got) != 5 {
		t.Fatalf("the limit is what comes back: %d", len(got))
	}
	if opened != 5 {
		t.Fatalf("only what is shown may be opened, got %d opens for 5 sessions", opened)
	}
	for _, s := range got {
		if s.Dir != "/tmp/x" {
			t.Fatalf("every session shown must have been read: %+v", s)
		}
	}

	// And with no limit, every one of them is read.
	opened = 0
	if _, _, err := In(&rota.Account{ID: 1, Provider: "claude"}, home, 0); err != nil {
		t.Fatal(err)
	}
	if opened != 6 {
		t.Fatalf("without a limit all six are read, got %d", opened)
	}
}

// grok files them by project like Claude Code, but two levels deep and with
// the path percent-encoded rather than mangled: one directory per project,
// one directory per conversation inside it, and a summary.json in each.
//
// The summary is pretty-printed, so a reader that scans line by line — as the
// two jsonl formats do — finds nothing in it. That shape is taken off disk,
// not tidied for the test.
//
// The project directory also holds a prompt_history.jsonl, and the sessions
// directory a session_search.sqlite. Neither is a conversation.
func TestGrokSessionsComeBackWithTheirRealDirectory(t *testing.T) {
	home := t.TempDir()
	id := "01a00000-0000-7000-8000-000000000003"
	project := filepath.Join(home, "sessions", "%2FUsers%2Fme%2Fsrc%2Fa-b")
	writeLines(t, filepath.Join(project, id, "summary.json"),
		`{`,
		`  "info": {`,
		`    "id": "`+id+`",`,
		`    "cwd": "/Users/me/src/a-b"`,
		`  },`,
		`  "session_summary": "hi",`,
		`  "created_at": "2026-08-28T14:30:32.762108Z",`,
		`  "updated_at": "2026-08-28T14:30:48.935066Z"`,
		`}`)
	writeLines(t, filepath.Join(project, "prompt_history.jsonl"), `{"prompt":"hi"}`)
	writeLines(t, filepath.Join(home, "sessions", "session_search.sqlite"), `not a conversation`)

	got, total, err := In(&rota.Account{ID: 8, Provider: "grok"}, home, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || total != 1 {
		t.Fatalf("one conversation, got %d of %d: %+v", len(got), total, got)
	}
	if got[0].ID != id {
		t.Fatalf("the id is what --resume takes: %q", got[0].ID)
	}
	if got[0].Dir != "/Users/me/src/a-b" {
		t.Fatalf("the directory must come from the summary, not the folder name: %q", got[0].Dir)
	}
	if got[0].At.IsZero() {
		t.Fatal("a session must say when it was last touched")
	}
}

// The same economy the jsonl formats get: the order comes from the directory
// entries, and only the summaries of the few that survive the limit are read.
// A grok home that has been used for a while holds hundreds of these.
func TestGrokListsWithoutOpeningEveryConversation(t *testing.T) {
	home := t.TempDir()
	for i := range 4 {
		id := fmt.Sprintf("01a048c7-0000-0000-0000-00000000000%d", i)
		dir := filepath.Join(home, "sessions", "%2Ftmp%2Fx", id)
		writeLines(t, filepath.Join(dir, "summary.json"),
			`{"info":{"id":"`+id+`","cwd":"/tmp/x"}}`)
		when := time.Now().Add(time.Duration(-i) * time.Hour)
		if err := os.Chtimes(dir, when, when); err != nil {
			t.Fatal(err)
		}
	}

	opened := 0
	orig := openTranscript
	t.Cleanup(func() { openTranscript = orig })
	openTranscript = func(path string) (*os.File, error) {
		opened++
		return orig(path)
	}

	got, total, err := In(&rota.Account{ID: 8, Provider: "grok"}, home, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || len(got) != 2 {
		t.Fatalf("the newest two of four: %d of %d", len(got), total)
	}
	if opened != 2 {
		t.Fatalf("only what is shown may be opened, got %d opens for 2 sessions", opened)
	}
	if !got[0].At.After(got[1].At) {
		t.Fatalf("newest first: %+v", got)
	}
	for _, s := range got {
		if s.Dir != "/tmp/x" {
			t.Fatalf("every session shown must have been read: %+v", s)
		}
	}
}
