package message

import (
	"testing"
)

// An answer is markdown with code in the middle of it. A client that wants
// to show the code differently from the prose should not have to write a
// markdown parser to find out where one stops.

func TestProseWithNoCodeIsOneBlock(t *testing.T) {
	got := Blocks("Two files:\n\n- a\n- b")
	if len(got) != 1 || got[0].Kind != "text" || got[0].Text != "Two files:\n\n- a\n- b" {
		t.Fatalf("%+v", got)
	}
}

func TestCodeIsItsOwnBlockAndKeepsItsLanguage(t *testing.T) {
	got := Blocks("before\n\n```go\nfmt.Println(1)\n```\n\nafter")
	if len(got) != 3 {
		t.Fatalf("want three blocks, got %d: %+v", len(got), got)
	}
	if got[0].Kind != "text" || got[0].Text != "before" {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].Kind != "code" || got[1].Lang != "go" || got[1].Text != "fmt.Println(1)" {
		t.Errorf("second: %+v", got[1])
	}
	if got[2].Kind != "text" || got[2].Text != "after" {
		t.Errorf("third: %+v", got[2])
	}
}

func TestCodeWithNoLanguageStillSeparates(t *testing.T) {
	got := Blocks("```\nls -1\n```")
	if len(got) != 1 || got[0].Kind != "code" || got[0].Lang != "" || got[0].Text != "ls -1" {
		t.Fatalf("%+v", got)
	}
}

// A longer fence is how markdown quotes a fence, so the inner one is content.
func TestALongerFenceHoldsABackquotedFence(t *testing.T) {
	got := Blocks("````md\n```go\nx\n```\n````")
	if len(got) != 1 || got[0].Kind != "code" || got[0].Lang != "md" {
		t.Fatalf("%+v", got)
	}
	if got[0].Text != "```go\nx\n```" {
		t.Fatalf("inner fence must survive: %q", got[0].Text)
	}
}

// A CLI can be killed mid-answer. Whatever the fence opened is still code.
func TestAnUnclosedFenceIsStillCode(t *testing.T) {
	got := Blocks("here:\n\n```sh\nrm -rf /")
	if len(got) != 2 || got[1].Kind != "code" || got[1].Lang != "sh" || got[1].Text != "rm -rf /" {
		t.Fatalf("%+v", got)
	}
}

func TestAnEmptyAnswerHasNoBlocks(t *testing.T) {
	if got := Blocks("   \n\n "); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}
