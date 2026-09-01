package message

import "testing"

func TestAnAnswerThatAsksNothingIsNotAQuestion(t *testing.T) {
	for _, md := range []string{
		"Done. Two files changed.",
		"",
		"The config lives in ~/.claude/settings.json",
		"I looked at whether this was possible? It is. Here is the patch.",
	} {
		if got := Asked(md); got != nil {
			t.Errorf("%q: %+v", md, got)
		}
	}
}

func TestAQuestionAtTheEndIsOne(t *testing.T) {
	got := Asked("I removed the old file.\n\nShould I also drop the migration?")
	if got == nil {
		t.Fatal("nil")
	}
	if got.Kind != "text" {
		t.Errorf("kind: %q", got.Kind)
	}
	if got.Question != "Should I also drop the migration?" {
		t.Errorf("question: %q", got.Question)
	}
	if len(got.Options) != 0 {
		t.Errorf("options: %+v", got.Options)
	}
}

func TestAListUnderTheQuestionIsTheChoices(t *testing.T) {
	got := Asked("Which should I use?\n\n- Postgres\n- SQLite\n- MySQL")
	if got == nil {
		t.Fatal("nil")
	}
	if got.Kind != "choice" {
		t.Errorf("kind: %q", got.Kind)
	}
	if got.Question != "Which should I use?" {
		t.Errorf("question: %q", got.Question)
	}
	if len(got.Options) != 3 || got.Options[0] != "Postgres" || got.Options[2] != "MySQL" {
		t.Errorf("options: %+v", got.Options)
	}
	if got.Multiple {
		t.Error("a plain list is one choice, not several")
	}
}

// A task list is markdown's way of saying more than one may be picked.
func TestATaskListMeansMoreThanOneMayBePicked(t *testing.T) {
	got := Asked("Which of these should I turn on?\n\n- [ ] tracing\n- [ ] metrics\n- [ ] profiling")
	if got == nil {
		t.Fatal("nil")
	}
	if !got.Multiple || got.Kind != "choice" {
		t.Fatalf("%+v", got)
	}
	if len(got.Options) != 3 || got.Options[0] != "tracing" {
		t.Fatalf("options: %+v", got.Options)
	}
}

func TestNumberedChoicesCountToo(t *testing.T) {
	got := Asked("Pick one:\n\n1. keep it\n2. rewrite it")
	if got == nil || got.Kind != "choice" || len(got.Options) != 2 || got.Options[1] != "rewrite it" {
		t.Fatalf("%+v", got)
	}
}

// Code is not a list of choices, whatever it contains.
func TestAListInsideCodeIsNotAChoice(t *testing.T) {
	got := Asked("Shall I run this?\n\n```sh\n- not an option\n- nor this\n```")
	if got == nil {
		t.Fatal("nil")
	}
	if got.Kind != "text" || len(got.Options) != 0 {
		t.Fatalf("%+v", got)
	}
}

// The real thing: this is how a headless run actually ended, and the choice
// is written into the sentence rather than listed. Splitting prose on "or"
// turns "use foo or bar" into two options that were never offered, so rota
// reports the question and leaves the reading to a person.
func TestAnInlineChoiceIsReportedAsAQuestionNotAsOptions(t *testing.T) {
	got := Asked("Blocked. `rm` outside working directory not allowed.\n\n" +
		"Want run inside working dir instead, or add `/tmp` to allowed dirs?")
	if got == nil {
		t.Fatal("nil")
	}
	if got.Kind != "text" {
		t.Errorf("kind: %q", got.Kind)
	}
	if len(got.Options) != 0 {
		t.Errorf("rota must not invent options: %+v", got.Options)
	}
}
