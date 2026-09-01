package main

import (
	"slices"
	"testing"
)

// `--resume` on its own means the conversation you were just having. With a
// session id it means that one. Go's flag package cannot express an optional
// value, so a bare one is rewritten before parsing rather than turning
// --resume into a flag that no longer takes an id.
func TestABareResumeMeansTheLastSession(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []string
		want []string
	}{
		{"on its own", []string{"--resume"}, []string{"--resume=last"}},
		{"before another flag", []string{"--resume", "--json"}, []string{"--resume=last", "--json"}},
		{"one dash", []string{"-resume"}, []string{"--resume=last"}},
		{"after the prompt", []string{"1", "carry on", "--resume"}, []string{"1", "carry on", "--resume=last"}},
		{"with a session id", []string{"--resume", "s-1"}, []string{"--resume", "s-1"}},
		{"already spelled out", []string{"--resume=s-1"}, []string{"--resume=s-1"}},
		{"before the end of flags", []string{"--resume", "--", "--raw"}, []string{"--resume=last", "--", "--raw"}},
		{"past the separator it is not ours", []string{"--", "--resume"}, []string{"--", "--resume"}},
	} {
		if got := bareResume(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
