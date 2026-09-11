package message

import (
	"strings"
	"testing"
)

// A caller names the readings it wants, as a comma list or one at a time or
// both, and a name nobody knows is refused by name, with the known ones.
func TestParseWithTakesListsAndRepeats(t *testing.T) {
	w, err := ParseWith("blocks,ask")
	if err != nil || !w.Blocks || !w.Ask {
		t.Fatalf("%+v %v", w, err)
	}
	w, err = ParseWith("ask", "blocks")
	if err != nil || !w.Blocks || !w.Ask {
		t.Fatalf("%+v %v", w, err)
	}
	w, err = ParseWith(" blocks , ", "")
	if err != nil || !w.Blocks || w.Ask {
		t.Fatalf("spaces and empties are nothing: %+v %v", w, err)
	}
	if w, err = ParseWith(); err != nil || w != (With{}) {
		t.Fatalf("nothing asked is nothing read: %+v %v", w, err)
	}
	_, err = ParseWith("blocks,foo")
	if err == nil || !strings.Contains(err.Error(), `"foo"`) || !strings.Contains(err.Error(), "blocks") || !strings.Contains(err.Error(), "ask") {
		t.Fatalf("refused by name, naming the known: %v", err)
	}
}

// By default a text event is the text. Blocks are read out of it only when
// the stream was asked for them, and never out of a fragment.
func TestBlocksOnTextEventsOnlyWhenAsked(t *testing.T) {
	line := "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"Run:\\n```sh\\nls\\n```\"}]}}\n"
	delta := "{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Run:\\n```sh\\nls\\n```\"}}}\n"

	plain := &Stream{}
	got := collect(plain)
	plain.Write([]byte(line))
	if len(*got) != 1 || (*got)[0].Blocks != nil {
		t.Fatalf("unasked, the text is the text: %+v", *got)
	}

	asked := &Stream{With: With{Blocks: true}}
	got = collect(asked)
	asked.Write([]byte(line))
	asked.Write([]byte(delta))
	if len(*got) != 2 {
		t.Fatalf("%+v", *got)
	}
	if b := (*got)[0].Blocks; len(b) != 2 || b[0].Kind != "text" || b[1].Kind != "code" || b[1].Lang != "sh" {
		t.Fatalf("asked, the whole piece is split: %+v", b)
	}
	if (*got)[1].Blocks != nil {
		t.Fatalf("a fragment is never split: %+v", (*got)[1])
	}
}
