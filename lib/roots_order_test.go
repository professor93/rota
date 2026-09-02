package rota

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A confined caller learns nothing about a file it may not name. The roots
// decide first; only a file inside them is opened and vetted. Otherwise a
// missing file outside the roots would say "no such file" and a settings
// file carrying env would say so too, and the verdicts together read the
// filesystem the caller was confined away from.
func TestSuppliedConfigOutsideRootsIsRefusedUnread(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	missing := filepath.Join(outside, "missing.json")
	denied := filepath.Join(outside, "settings.json")
	if err := os.WriteFile(denied, []byte(`{"env":{"X":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lim := &Limits{Roots: []string{root}}
	quote := func(p string) json.RawMessage { return json.RawMessage(strconv.Quote(p)) }

	for _, tc := range []struct {
		name string
		spec Spec
	}{
		{"missing settings file", Spec{Prompt: "p", Settings: quote(missing)}},
		{"settings file with a denied key", Spec{Prompt: "p", Settings: quote(denied)}},
		{"missing mcp config", Spec{Prompt: "p", MCPConfig: []json.RawMessage{quote(missing)}}},
		{"mcp config with a denied key", Spec{Prompt: "p", MCPConfig: []json.RawMessage{quote(denied)}}},
	} {
		_, err := specArgv(tc.spec, "claude", lim)
		if !errors.Is(err, ErrOutsideRoots) {
			t.Fatalf("%s: want the roots' refusal and nothing more, got %v", tc.name, err)
		}
	}
	// Inside the roots the file is still read and vetted.
	inside := filepath.Join(root, "settings.json")
	if err := os.WriteFile(inside, []byte(`{"env":{"X":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := specArgv(Spec{Prompt: "p", Settings: quote(inside)}, "claude", lim); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a denied key inside the roots is still refused as invalid: %v", err)
	}
}
