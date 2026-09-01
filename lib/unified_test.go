package rota

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// A request field names a thing the caller wants, not the flag some vendor
// happens to spell it with. Where two CLIs mean the same thing, rota offers
// one field and each argv builder does its own translating — the way effort
// is one field behind --effort and --reasoning-effort.

func TestOneSchemaFieldServesEveryProvider(t *testing.T) {
	schema := `{"type":"object"}`
	spec := Spec{Prompt: "p", JSONSchema: json.RawMessage(schema)}

	for _, flavor := range []string{"claude", "grok"} {
		if err := spec.Check(flavor, nil); err != nil {
			t.Fatalf("%s: %v", flavor, err)
		}
		argv, err := specArgv(spec, flavor, nil)
		if err != nil {
			t.Fatalf("%s: %v", flavor, err)
		}
		if want := "--json-schema " + schema; !strings.Contains(strings.Join(argv, " "), want) {
			t.Fatalf("%s: missing %q in %v", flavor, want, argv)
		}
	}

	// codex takes the same schema from a file, which is its business, not
	// the caller's.
	if err := spec.Check("codex", nil); err != nil {
		t.Fatalf("codex: %v", err)
	}
	argv, err := specArgv(spec, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(argv, "--output-schema")
	if i < 0 || i == len(argv)-1 {
		t.Fatalf("codex: no --output-schema in %v", argv)
	}
	body, err := os.ReadFile(argv[i+1])
	if err != nil {
		t.Fatalf("codex: schema file: %v", err)
	}
	defer os.Remove(argv[i+1])
	if string(body) != schema {
		t.Fatalf("codex: schema file holds %q, want %q", body, schema)
	}
}

func TestOneForkFieldServesEveryProvider(t *testing.T) {
	spec := Spec{Prompt: "p", Resume: "sid", ForkSession: true}

	for _, flavor := range []string{"claude", "grok"} {
		argv, err := specArgv(spec, flavor, nil)
		if err != nil {
			t.Fatalf("%s: %v", flavor, err)
		}
		if !slices.Contains(argv, "--fork-session") {
			t.Fatalf("%s: no --fork-session in %v", flavor, argv)
		}
	}

	argv, err := specArgv(spec, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "fork") {
		t.Fatalf("codex: no fork subcommand in %v", argv)
	}
}

func TestOneEphemeralFieldServesEveryProvider(t *testing.T) {
	spec := Spec{Prompt: "p", Ephemeral: true}

	argv, err := specArgv(spec, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "--no-session-persistence") {
		t.Fatalf("claude: no --no-session-persistence in %v", argv)
	}

	argv, err = specArgv(spec, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "--ephemeral") {
		t.Fatalf("codex: no --ephemeral in %v", argv)
	}
}

// "Carry on from wherever I left off" is a thing every one of these CLIs
// can do, under four spellings: -c, --continue, and for codex a resume
// subcommand with --last. One field asks for it.
func TestCarryingOnWorksOnEveryProvider(t *testing.T) {
	spec := Spec{Prompt: "p", Continue: true}

	for _, want := range []struct{ flavor, flag string }{
		{"claude", "-c"},
		{"grok", "--continue"},
		{"kimi", "-c"},
	} {
		argv, err := specArgv(spec, want.flavor, nil)
		if err != nil {
			t.Fatalf("%s: %v", want.flavor, err)
		}
		if !slices.Contains(argv, want.flag) {
			t.Errorf("%s: no %s in %v", want.flavor, want.flag, argv)
		}
	}

	// codex has no flag for it: the most recent session is a subcommand.
	argv, err := specArgv(spec, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "resume") || !slices.Contains(argv, "--last") {
		t.Fatalf("codex: %v", argv)
	}
}

// Forking the most recent one is the same question with a different answer.
func TestCarryingOnCanForkOnCodexToo(t *testing.T) {
	argv, err := specArgv(Spec{Prompt: "p", Continue: true, ForkSession: true}, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "fork") || !slices.Contains(argv, "--last") {
		t.Fatalf("%v", argv)
	}
}

// "last" was codex's word for the most recent session and a literal session
// id everywhere else, so the same request meant two different things
// depending on who answered it. It now means the same thing to all of them.
func TestResumingTheLastSessionMeansTheSameEverywhere(t *testing.T) {
	spec := Spec{Prompt: "p", Resume: "last"}

	for _, want := range []struct{ flavor, flag string }{
		{"claude", "-c"},
		{"grok", "--continue"},
		{"kimi", "-c"},
	} {
		argv, err := specArgv(spec, want.flavor, nil)
		if err != nil {
			t.Fatalf("%s: %v", want.flavor, err)
		}
		if !slices.Contains(argv, want.flag) {
			t.Errorf("%s: no %s in %v", want.flavor, want.flag, argv)
		}
		if slices.Contains(argv, "last") {
			t.Errorf("%s: sent \"last\" as a session id: %v", want.flavor, argv)
		}
	}

	argv, err := specArgv(spec, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "resume") || !slices.Contains(argv, "--last") {
		t.Fatalf("codex: %v", argv)
	}
}
