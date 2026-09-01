// Package sessions reports what the vendor CLIs are doing: which of them are
// running right now, and which conversations an account could resume.
//
// None of this belongs in the SDK. Where a CLI files its transcripts, how an
// editor registers itself, how to read a process list — these are things an
// application does around the SDK's accounts, not part of signing one in or
// running an agent. lib knows none of it.
//
// Everything here is best-effort by nature: it reads files another program
// owns and may change. Nothing is invented to fill a gap — a directory that
// was not recorded comes back empty rather than guessed at, and a provider
// whose layout rota does not know reports nothing rather than nothing found.
package sessions

import (
	"bufio"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
)

// Session is one conversation an account could resume.
type Session struct {
	Account  int       `json:"account,omitempty"`
	Label    string    `json:"label,omitempty"`
	Provider string    `json:"provider"`
	ID       string    `json:"id"`
	Dir      string    `json:"dir,omitempty"`
	At       time.Time `json:"at"`
	Shared   bool      `json:"shared,omitempty"`

	// path is the transcript this came from, kept so the directory can be
	// read later and only for what is actually shown.
	path string
}

// maxHeader is how far into a transcript to look for the directory it ran in.
// Both CLIs write it within the first few records; a file that has not said
// by here is one that never will, and reading a whole transcript to find out
// would mean reading every megabyte of every conversation.
const maxHeader = 64

// ConfigHome is where an account's CLI keeps its own files, and whether that
// place is shared with anyone else.
//
// An account told where it belongs keeps everything there. One that was not
// depends on its provider: codex, grok and kimi are always launched with a
// private CODEX_HOME, GROK_HOME or KIMI_CODE_HOME, so the staged directory is
// theirs alone. Claude Code is only pointed at one when the account names it,
// and otherwise reads the person's own — which every other such account reads
// too, so nothing found there belongs to any single account.
func ConfigHome(a *rota.Account, staged string) (dir string, shared bool) {
	if a.ConfigDir != "" {
		return a.ConfigDir, false
	}
	if rota.Flavor(a.Provider) != "claude" {
		return staged, false
	}
	if own := os.Getenv("CLAUDE_CONFIG_DIR"); own != "" {
		return own, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", true
	}
	return filepath.Join(home, ".claude"), true
}

// readers is where each CLI rota knows to look for conversations, and how to
// read one once found. list gives the id and when it was last touched from
// the directory alone; read opens the file for the few that are shown.
//
// It is one list because two would drift. The dispatch here and the note in
// Scan are the same statement — which providers rota can read — and while
// they were written separately a provider could be read and warned about at
// the same time, or listed by neither.
var readers = map[string]struct {
	list func(home string) ([]Session, error)
	read func(path string, s *Session)
}{
	"claude": {claudeSessions, readClaudeHeader},
	"codex":  {codexSessions, readCodexHeader},
	"grok":   {grokSessions, readGrokSummary},
}

// readable reports whether rota knows where a provider's CLI files its
// conversations.
func readable(provider string) bool {
	_, ok := readers[rota.Flavor(provider)]
	return ok
}

// In lists the conversations an account could resume from home, newest first,
// and how many there are in total. A limit of 0 or less returns all of them.
//
// A home that does not exist is not an error: an account that has never run
// has no conversations, which is a fact rather than a fault.
//
// Only the sessions actually returned are opened. A person's own Claude Code
// home can hold thousands of transcripts, and reading the head of every one
// to show five would cost a second of someone's time for nothing: the order
// comes from the directory entries, and the directory each conversation ran
// in is read afterwards, for the few that survived the limit.
func In(a *rota.Account, home string, limit int) (found []Session, total int, err error) {
	if home == "" {
		return nil, 0, nil
	}
	r, ok := readers[rota.Flavor(a.Provider)]
	if !ok {
		// kimi keeps its conversations somewhere rota has not been able to
		// confirm. Reporting none is honest; inventing a layout and reporting
		// the wrong ones is not.
		return nil, 0, nil
	}
	found, err = r.list(home)
	if err != nil {
		return nil, 0, err
	}
	slices.SortFunc(found, func(x, y Session) int { return y.At.Compare(x.At) })
	total = len(found)
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}
	for i := range found {
		found[i].Account, found[i].Provider = a.ID, a.Provider
		found[i].Label = a.Label()
		r.read(found[i].path, &found[i])
	}
	return found, total, nil
}

// claudeSessions reads <home>/projects/<encoded>/<id>.jsonl.
//
// The directory name is the project path with the separators swapped, which
// cannot be undone — "a-b" and "a/b" encode to the same thing — so the path
// is read out of the transcript, where it is written exactly.
func claudeSessions(home string) ([]Session, error) {
	dirs, err := os.ReadDir(filepath.Join(home, "projects"))
	if err != nil {
		return nil, skipMissing(err)
	}
	var out []Session
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		project := filepath.Join(home, "projects", d.Name())
		files, err := os.ReadDir(project)
		if err != nil {
			continue // a directory that went away between the two reads
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			s := Session{ID: strings.TrimSuffix(name, ".jsonl"), path: filepath.Join(project, name)}
			if info, err := f.Info(); err == nil {
				s.At = info.ModTime()
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// codexSessions reads <home>/sessions/YYYY/MM/DD/rollout-<stamp>-<id>.jsonl.
// The first record is a session_meta carrying both the id and the directory,
// so the file name is only a fallback for the id.
func codexSessions(home string) ([]Session, error) {
	root := filepath.Join(home, "sessions")
	var out []Session
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner is skipped, not fatal
		}
		name := d.Name()
		if d.IsDir() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		s := Session{ID: codexID(name), path: path}
		if info, err := d.Info(); err == nil {
			s.At = info.ModTime()
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, skipMissing(err)
	}
	return out, nil
}

// codexID takes the uuid off the end of a rollout name, which is
// rollout-<timestamp>-<uuid>.jsonl. A uuid is five dash-separated groups, so
// it is the last five.
func codexID(name string) string {
	base := strings.TrimSuffix(name, ".jsonl")
	parts := strings.Split(base, "-")
	if len(parts) < 5 {
		return base
	}
	return strings.Join(parts[len(parts)-5:], "-")
}

// grokSessions reads <home>/sessions/<encoded>/<id>/summary.json.
//
// Two levels: a directory per project, then a directory per conversation
// inside it. The project's name is its path percent-encoded, which could be
// undone — but the summary says where the conversation ran exactly, so it is
// read there as codex's is, and the encoding never has to be trusted.
//
// Only directories are conversations. A prompt_history.jsonl sits beside them
// in each project, and a session_search.sqlite beside the projects.
func grokSessions(home string) ([]Session, error) {
	projects, err := readDirNames(filepath.Join(home, "sessions"))
	if err != nil {
		return nil, skipMissing(err)
	}
	var out []Session
	for _, project := range projects {
		dir := filepath.Join(home, "sessions", project)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // a directory that went away between the two reads
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			s := Session{ID: e.Name(), path: filepath.Join(dir, e.Name(), "summary.json")}
			if info, err := e.Info(); err == nil {
				s.At = info.ModTime()
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// readClaudeHeader fills in where a conversation ran, which Claude Code
// writes on its records.
func readClaudeHeader(path string, s *Session) {
	s.Dir = headerField(path, func(rec map[string]jsontext.Value) string {
		return text(rec["cwd"])
	})
}

// readCodexHeader reads the session_meta record codex writes first, which
// carries both the id and the directory. The id there wins over the one in
// the file name, which is only a fallback.
func readCodexHeader(path string, s *Session) {
	s.Dir = headerField(path, func(rec map[string]jsontext.Value) string {
		var payload struct {
			SessionID string `json:"session_id"`
			Cwd       string `json:"cwd"`
		}
		p, ok := rec["payload"]
		if !ok || jsonv2.Unmarshal(p, &payload) != nil {
			return ""
		}
		if payload.SessionID != "" {
			s.ID = payload.SessionID
		}
		return payload.Cwd
	})
}

// readGrokSummary reads the summary grok keeps beside each conversation,
// which carries both the id and the directory. The id there wins over the one
// in the folder name, which is only a fallback.
//
// Unlike the two jsonl formats this is a single pretty-printed object rather
// than a record per line, so it is decoded whole. That is affordable because
// it is a handful of fields about the conversation and not the conversation
// itself — the transcript beside it is the file that grows.
func readGrokSummary(path string, s *Session) {
	f, err := openTranscript(path)
	if err != nil {
		return
	}
	defer f.Close()
	var summary struct {
		Info struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"info"`
	}
	if jsonv2.UnmarshalRead(f, &summary) != nil {
		return
	}
	if summary.Info.ID != "" {
		s.ID = summary.Info.ID
	}
	s.Dir = summary.Info.Cwd
}

// openTranscript is os.Open, as a variable so a test can count what was read.
// Whether a scan opens five files or five thousand is the difference between
// an instant answer and a visible pause, and that is worth pinning.
var openTranscript = os.Open

// headerField runs pick over the first records of a transcript and returns
// the first non-empty answer. A file that says nothing gives "", which is
// reported as unknown rather than filled in from the directory name.
func headerField(path string, pick func(map[string]jsontext.Value) string) string {
	f, err := openTranscript(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Transcripts hold whole conversations, so one record can be very large;
	// the default 64KB line limit would stop the scan on a long answer.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for n := 0; n < maxHeader && sc.Scan(); n++ {
		var rec map[string]jsontext.Value
		if err := jsonv2.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if got := pick(rec); got != "" {
			return got
		}
	}
	// sc.Err() is deliberately not consulted. Every way this scan can fail —
	// a record longer than the buffer, a truncated file, a read error on a
	// transcript another program is writing — means the same thing to the
	// caller: this file did not say. That is the documented answer above, and
	// turning it into an error would take a listing down over one unreadable
	// conversation among thousands.
	return ""
}

// text unwraps a JSON string, or "" for anything else.
func text(v jsontext.Value) string {
	var s string
	if len(v) == 0 || jsonv2.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

// readDirNames is the directories inside dir, or nothing when there are none.
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// skipMissing turns "there is nothing there" into no error, because an
// account that has never run is not a failure.
func skipMissing(err error) error {
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
