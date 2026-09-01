package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// seedStore writes an accounts file by hand and opens it, which is quicker
// than driving a login for accounts whose only interesting property is which
// provider they belong to.
func seedStore(t *testing.T, body string) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// notes about session stores, which are the ones this is about; a scan also
// says what it could not read of the process list, and that is not the point
// here.
func storeNotes(rep Report) []string {
	var out []string
	for _, n := range rep.Notes {
		if strings.Contains(n, "no session store") {
			out = append(out, n)
		}
	}
	return out
}

// A provider whose conversations rota can read must not be warned about, and
// one it cannot must be.
//
// These were two statements of one list — the dispatch in In and the note
// here — and a list written twice drifts. grok's conversations sat readable
// on disk while the note still said they could not be listed.
func TestScanWarnsOnlyAboutProvidersItCannotRead(t *testing.T) {
	s, dir := seedStore(t, `{"accounts":[
		{"id":1,"provider":"grok","uuid":"g1","order":1},
		{"id":2,"provider":"kimi","uuid":"k1","order":2}],"nextId":3,"ordered":true}`)

	id := "01a00000-0000-7000-8000-000000000003"
	writeLines(t, filepath.Join(dir, "homes", "grok-1", "sessions", "%2Ftmp%2Fx", id, "summary.json"),
		`{"info":{"id":"`+id+`","cwd":"/tmp/x"}}`)

	rep := Scan(s, 0)

	if len(rep.Sessions) != 1 || rep.Sessions[0].ID != id || rep.Sessions[0].Account != 1 {
		t.Fatalf("the grok conversation belongs to the account whose home it is in: %+v", rep.Sessions)
	}
	notes := storeNotes(rep)
	if len(notes) != 1 {
		t.Fatalf("one provider cannot be read, so one note: %q", notes)
	}
	if !strings.Contains(notes[0], "kimi") {
		t.Fatalf("the note must name what could not be read: %q", notes[0])
	}
	if strings.Contains(notes[0], "grok") {
		t.Fatalf("a provider whose conversations are on the screen must not be warned about: %q", notes[0])
	}
}

// Nothing is warned about when everything can be read, so a scan of readable
// accounts is quiet.
func TestScanSaysNothingWhenEveryProviderCanBeRead(t *testing.T) {
	s, _ := seedStore(t, `{"accounts":[
		{"id":1,"provider":"grok","uuid":"g1","order":1},
		{"id":3,"provider":"codex","uuid":"c1","order":2}],"nextId":4,"ordered":true}`)

	if notes := storeNotes(Scan(s, 0)); len(notes) != 0 {
		t.Fatalf("nothing to warn about: %q", notes)
	}
}

// Every provider rota supports is either read or warned about. A new one
// added to the reader without the note, or the other way round, is the drift
// this catches.
func TestEveryProviderIsEitherReadOrWarnedAbout(t *testing.T) {
	for _, p := range rota.Providers() {
		s, _ := seedStore(t, `{"accounts":[{"id":1,"provider":"`+p+`","uuid":"u1","order":1}],"nextId":2,"ordered":true}`)
		warned := len(storeNotes(Scan(s, 0))) > 0
		if warned == readable(p) {
			t.Fatalf("%s: readable is %v but the scan %s warn about it",
				p, readable(p), map[bool]string{true: "does", false: "does not"}[warned])
		}
	}
}
