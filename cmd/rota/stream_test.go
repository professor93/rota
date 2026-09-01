package main

import (
	jsonv2 "encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// streamingCLI is a fake that prints events the way the real ones do: one
// JSON object per line, as they happen.
func streamingCLI(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\n" +
		`printf '{"type":"system","subtype":"init","session_id":"s-1"}\n'` + "\n" +
		`printf '{"type":"assistant","message":{"content":[{"type":"text","text":"hello "}]}}\n'` + "\n" +
		`printf '{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]}}\n'` + "\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"session_id":"s-1","result":"hello world","num_turns":1,"total_cost_usd":0.01}\n'` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func oneAccount(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}}]}`)
}

// Streaming in text mode prints what the agent said, and nothing else.
//
// It used to hand the vendor's own event lines straight to stdout, so asking
// for text got a screenful of somebody else's JSON.
func TestStreamingTextPrintsTextAndNothingElse(t *testing.T) {
	oneAccount(t)
	streamingCLI(t)

	out, _, code := call(t, "run", "1", "hi", "--stream")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	if !strings.Contains(out, "hello world") {
		t.Fatalf("the answer must arrive: %q", out)
	}
	if strings.Contains(out, "{") || strings.Contains(out, "session_id") {
		t.Fatalf("text mode must not print the provider's own events: %q", out)
	}
}

// Streaming in JSON mode is newline-delimited: exactly one complete object
// per line, the same events the HTTP surface sends, so a caller can move
// between the two without changing what reads them.
func TestStreamingJSONIsOneObjectPerLine(t *testing.T) {
	oneAccount(t)
	streamingCLI(t)

	out, _, code := call(t, "--json", "run", "1", "hi", "--stream")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("a stream is several events: %q", out)
	}
	var events []map[string]any
	for i, line := range lines {
		var ev map[string]any
		if err := jsonv2.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d is not one whole JSON object: %q\n%s", i+1, line, out)
		}
		events = append(events, ev)
	}

	// rota speaks first, with what the run is about to do.
	first := events[0]
	if first["type"] != "init" {
		t.Fatalf("the first event is rota's own: %+v", first)
	}
	if first["model"] == nil || first["effort"] == nil {
		t.Fatalf("the opening event says which model and effort were resolved: %+v", first)
	}
	if first["account"] == nil {
		t.Fatalf("and which account is paying: %+v", first)
	}

	// Every event is numbered, so a reader can notice a gap.
	if events[1]["seq"] == nil {
		t.Fatalf("events carry their place in the stream: %+v", events[1])
	}

	// The last one says how it ended.
	last := events[len(events)-1]
	if last["type"] != "done" && last["type"] != "error" {
		t.Fatalf("a stream ends by saying how: %+v", last)
	}

	// And nothing pretty-printed is mixed in: the whole answer document at
	// the end would break every one of those lines.
	if strings.Contains(out, "\n  \"") {
		t.Fatalf("json streaming is newline-delimited, not a document:\n%s", out)
	}
}

// Without --stream, JSON is still one document, indented, as it always was.
func TestWithoutStreamingJSONIsStillOneDocument(t *testing.T) {
	oneAccount(t)
	streamingCLI(t)

	out, _, code := call(t, "--json", "run", "1", "hi")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	var doc map[string]any
	if err := jsonv2.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("one document: %v\n%s", err, out)
	}
	if doc["result"] != "hello world" {
		t.Fatalf("%+v", doc)
	}
}

// A run learns which conversation it is in from the CLI, part way through,
// and that has to reach the registry — otherwise a listing says which account
// is spending without saying what it is spending on.
//
// It is learned whether or not the events are being printed: a run that was
// not asked to stream is still read, because the reading is happening anyway.
func TestARunLearnsItsSessionWhetherOrNotItPrints(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		var learned []string
		e := newEventStream(io.Discard, true, 1, "claude")
		e.quiet = quiet
		e.learn = func(id string) { learned = append(learned, id) }

		if _, err := e.Write([]byte(`{"type":"system","subtype":"init","session_id":"s-77"}` + "\n")); err != nil {
			t.Fatal(err)
		}
		if len(learned) == 0 || learned[0] != "s-77" {
			t.Fatalf("quiet=%v: the conversation must be learned: %v", quiet, learned)
		}
	}
}

// Printing is what --stream asks for; reading is not optional. A quiet stream
// writes nothing at all, which is what a run without --stream must look like.
func TestAQuietStreamPrintsNothing(t *testing.T) {
	var out strings.Builder
	e := newEventStream(&out, true, 1, "claude")
	e.quiet = true
	if _, err := e.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Fatalf("a run that did not ask to stream prints nothing as it goes: %q", out.String())
	}
}
