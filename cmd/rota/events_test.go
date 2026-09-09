package main

import (
	jsonv2 "encoding/json/v2"
	"strings"
	"testing"
)

// --events attaches the provider's own event to each of rota's, the way
// include_events does over HTTP. It is machine output by nature, so it
// implies --json rather than having nowhere to put what it asked for.
func TestEventsAttachTheProvidersOwnLineToEachStreamedEvent(t *testing.T) {
	oneAccount(t)
	streamingCLI(t)

	out, _, code := call(t, "run", "1", "hi", "--stream", "--events")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	events := eventsOf(t, out) // one JSON object per line, or this fails
	var withRaw int
	for _, ev := range events {
		raw, ok := ev["raw"].(map[string]any)
		if !ok {
			continue
		}
		withRaw++
		if raw["type"] == "assistant" && ev["type"] != "text" {
			t.Fatalf("raw rides on the event it was read from: %v", ev)
		}
	}
	// The CLI's init, two answers and result carry raw; rota's own init
	// and done have no provider line to carry.
	if withRaw != 4 {
		t.Fatalf("%d events carry the provider's line in:\n%s", withRaw, out)
	}
	if first, last := events[0], events[len(events)-1]; first["raw"] != nil || last["raw"] != nil {
		t.Fatalf("rota's own events have nothing to attach: %v %v", first, last)
	}
}

// Without --stream the same flag puts the whole event stream in the
// buffered reply, as include_events does.
func TestEventsKeepTheWholeStreamInABufferedReply(t *testing.T) {
	oneAccount(t)
	streamingCLI(t)

	out, _, code := call(t, "run", "1", "hi", "--events")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	var doc struct {
		Result string           `json:"result"`
		Events []map[string]any `json:"events"`
	}
	if err := jsonv2.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--events implies --json: %v\n%s", err, out)
	}
	if doc.Result != "hello world" || len(doc.Events) != 4 || doc.Events[1]["type"] != "assistant" {
		t.Fatalf("the answer, and every line the CLI printed: %s", out)
	}
	if strings.Contains(out, `"raw"`) {
		t.Fatalf("a buffered reply keeps the lines whole, not as raw on events: %s", out)
	}
}
