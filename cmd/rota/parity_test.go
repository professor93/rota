package main

import (
	jsonv2 "encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/professor93/rota/api"
)

// The CLI and the HTTP API stream the same run, and the README says they send
// the same events. That claim is worth holding: they are two transports, and
// a claim nothing checks is one that stops being true quietly.
//
// Since message.Stream they share the reading and the numbering, so this
// pushes one CLI's output through both and compares what comes out. Only the
// framing differs — newline-delimited JSON in both cases here — and the
// framing is all either surface still owns.
func TestTheCLIAndTheAPIStreamTheSameEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}}]}`)
	streamingCLI(t)

	fromCLI := eventsOf(t, mustRun(t, "--json", "run", "1", "hi", "--stream"))
	fromAPI := eventsOf(t, mustPost(t, home, `{"prompt":"hi","stream":true}`))

	if len(fromCLI) == 0 {
		t.Fatal("the CLI sent nothing")
	}
	if len(fromCLI) != len(fromAPI) {
		t.Fatalf("different number of events:\ncli: %v\napi: %v", kinds(fromCLI), kinds(fromAPI))
	}
	for i := range fromCLI {
		cli, srv := fromCLI[i], fromAPI[i]
		for _, field := range []string{"type", "seq", "account", "provider", "text", "session_id"} {
			if !same(cli[field], srv[field]) {
				t.Fatalf("event %d differs on %q: cli %v, api %v\ncli: %+v\napi: %+v",
					i+1, field, cli[field], srv[field], cli, srv)
			}
		}
	}
	// And they really did carry something, so an empty stream cannot pass.
	if kinds(fromCLI)[0] != "init" || !strings.Contains(strings.Join(kinds(fromCLI), ","), "text") {
		t.Fatalf("a run with an answer in it: %v", kinds(fromCLI))
	}
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, code := call(t, args...)
	if code != 0 {
		t.Fatalf("%v: %d %q", args, code, errOut)
	}
	return out
}

// mustPost streams one run through the HTTP surface, asking for the same
// newline-delimited framing the CLI writes.
func mustPost(t *testing.T, dir, body string) string {
	t.Helper()
	srv, err := api.New(api.Options{Dir: dir, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest("POST", ts.URL+"/v1/run", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	blob, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(blob)
}

func eventsOf(t *testing.T, out string) []map[string]any {
	t.Helper()
	var got []map[string]any
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if err := jsonv2.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("not one whole JSON object per line: %q\n%s", line, out)
		}
		got = append(got, ev)
	}
	return got
}

func kinds(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		s, _ := ev["type"].(string)
		out = append(out, s)
	}
	return out
}

// same compares two decoded JSON values, treating absent and empty alike:
// a field neither surface sets is not a difference between them.
func same(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	return a == b
}
