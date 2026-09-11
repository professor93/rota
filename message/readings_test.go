package message

import (
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// Every name is known, in a comma list or one at a time, and the requests
// among them become what the CLI is asked for.
func TestEveryReadingHasANameAndSomeAskTheCLI(t *testing.T) {
	w, err := ParseWith(strings.Join(Names, ","))
	if err != nil {
		t.Fatal(err)
	}
	if w != (With{true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true}) {
		t.Fatalf("every name sets its reading: %+v", w)
	}
	var s rota.Spec
	w.Apply(&s)
	if !s.IncludeEvents || !s.IncludePartialMessages || !s.IncludeHookEvents || !s.ForwardSubagentText || !s.PromptSuggestions || !s.IncludeArgv {
		t.Fatalf("raw, deltas, hooks, subagents, suggestions and argv are asked of the CLI: %+v", s)
	}
	if !w.NeedsJSON() || !w.NeedsStream() {
		t.Fatal("readings that only exist as JSON imply it; deltas imply a stream")
	}
	only, _ := ParseWith("deltas,hooks,subagents,suggestions")
	if only.NeedsJSON() {
		t.Fatal("requests that only change what the CLI prints leave text mode alone")
	}
	if plain, _ := ParseWith("blocks"); plain.NeedsStream() {
		t.Fatal("a reading of the answer needs no stream")
	}
}

// code is the fences alone; plain flattens markdown; links are the URLs.
func TestCodePlainAndLinksAreReadFromTheAnswer(t *testing.T) {
	res := &rota.Result{Result: "## Done\nEdited **two** files, see [the docs](https://example.dev/docs) and https://example.dev/api#auth.\n```bash\nmake test\n```\nAlso `go vet`.\n```\nls\n```"}
	r := Read(res, With{Code: true, Plain: true, Links: true}, Sources{})
	if len(r.Code) != 2 || r.Code[0].Lang != "bash" || r.Code[0].Text != "make test" || r.Code[1].Lang != "" || r.Code[1].Text != "ls" {
		t.Fatalf("code: %+v", r.Code)
	}
	if r.Plain != "Done\nEdited two files, see the docs (https://example.dev/docs) and https://example.dev/api#auth.\nmake test\nAlso go vet.\nls" {
		t.Fatalf("plain: %q", r.Plain)
	}
	if strings.Join(r.Links, " ") != "https://example.dev/docs https://example.dev/api#auth" {
		t.Fatalf("links: %v", r.Links)
	}
	if none := Read(&rota.Result{Result: "no code here"}, With{Code: true, Links: true}, Sources{}); none.Code != nil || none.Links != nil {
		t.Fatalf("nothing to read is nothing: %+v", none)
	}
}

// A stream told to keep a tally sees which tools ran, which were refused,
// which files they named, how much went by and when the first text came.
func TestATallyFollowsTheStream(t *testing.T) {
	tally := &Tally{}
	s := &Stream{With: With{Timing: true}, Tally: tally}
	// A clock that steps by a known amount, so what the tally times is exact
	// and no platform's tick can make a duration zero.
	tick := time.Unix(1_000, 0)
	s.now = func() time.Time { tick = tick.Add(10 * time.Millisecond); return tick }
	var got []Event
	s.Emit = func(ev Event) error { got = append(got, ev); return nil }
	s.Send(Event{Type: "init"})
	for _, line := range []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/srv/a.go"}}]}}`,
		`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"t2","message":"no"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Edit","input":{"file_path":"/srv/a.go","old_string":"x","new_string":"y"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t4","name":"Read","input":{"file_path":"/srv/a.go"}}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"do"}}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"parent_tool_use_id":"t9"}`,
	} {
		s.Write([]byte(line + "\n"))
	}
	if tally.Tools["Read"] != 2 || tally.Tools["Edit"] != 1 || tally.Blocked["Bash"] != 1 {
		t.Fatalf("tools: %+v blocked: %+v", tally.Tools, tally.Blocked)
	}
	if strings.Join(tally.Read, ",") != "/srv/a.go" || strings.Join(tally.Written, ",") != "/srv/a.go" {
		t.Fatalf("files once each, read and written apart: %v %v", tally.Read, tally.Written)
	}
	// Six the CLI said; rota's own init is the clock's zero, not an event.
	if tally.Events != 6 || tally.Fragments != 1 || tally.Bytes == 0 || tally.ByType["tool"] != 3 || tally.ByType["init"] != 0 {
		t.Fatalf("%+v", tally)
	}
	if !tally.sawText || !tally.sawTool {
		t.Fatalf("first text and first tool are noted: %+v", tally)
	}
	// The clock is known, so the firsts are known: a tool ran before the text.
	if tally.FirstTool <= 0 || tally.FirstText <= 0 || tally.FirstTool >= tally.FirstText {
		t.Fatalf("first tool comes before first text, both after the start: %+v", tally)
	}
	if tally.Total != 0 {
		t.Fatalf("the total is known only when the stream ends: %+v", tally)
	}
	s.Rest()
	if tally.Total <= 0 {
		t.Fatalf("Rest ends the stream and fixes the total: %+v", tally)
	}
	// At is measured from the first event: zero on it, rising after it.
	for i, ev := range got {
		if ev.At < 0 || (i > 0 && ev.At <= 0) || (i > 0 && ev.At < got[i-1].At) {
			t.Fatalf("at is measured from the first event and never goes back: %+v", got)
		}
	}
	if last := got[len(got)-1]; last.Subagent != "t9" || last.Text != "done" {
		t.Fatalf("a subagent's text names the call that delegated it: %+v", last)
	}
	r := Read(&rota.Result{DurationMS: 42, Truncated: true}, With{Files: true, Tools: true, Stats: true, Timing: true}, Sources{Tally: tally})
	if r.Files == nil || r.Tools["Read"] != 2 || r.Stats.Events != 6 || !r.Stats.Truncated || r.Timing.TotalMS <= 0 {
		t.Fatalf("%+v", r)
	}
}

// A tally whose stream never ended — a buffered run reads its events through
// a quiet stream that is never closed — reports the child's duration as the
// total, so timing is never silently zero.
func TestTimingFallsBackToTheChildsDuration(t *testing.T) {
	r := Read(&rota.Result{DurationMS: 42}, With{Timing: true}, Sources{Tally: &Tally{}})
	if r.Timing == nil || r.Timing.TotalMS != 42 {
		t.Fatalf("%+v", r.Timing)
	}
}

// The account reading copies the account; the stderr reading is the one
// that touches result, on request only, and only when there was no answer.
func TestAccountAndStderrReadings(t *testing.T) {
	a := &rota.Account{ID: 3, Provider: "claude", Email: "you@example.com", Order: 2}
	r := ReplyFor(&rota.Result{Result: "hi"}, With{Account: true, Stderr: true}, Sources{Account: a, Threshold: 80})
	if r.AccountLabel != "claude/you@example.com" || r.Order != 2 || r.Threshold != 80 || r.Result.Result != "hi" {
		t.Fatalf("%+v", r)
	}
	failed := &rota.Result{IsError: true, Stderr: "Not logged in"}
	if got := ReplyFor(failed, With{}, Sources{}); got.Result.Result != "" {
		t.Fatalf("unasked, result stays empty: %+v", got)
	}
	if got := ReplyFor(failed, With{Stderr: true}, Sources{}); got.Result.Result != "Not logged in" || failed.Result != "" {
		t.Fatalf("asked, a copy carries stderr as result and the original is untouched: %+v", got)
	}
}
