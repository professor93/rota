package holdwire

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// What Resume and Move carry over is how the CLI was asked to run, never
// which conversation it was in: that is decided again each time.
func TestStoredArgsDropResumeAndSessionFlags(t *testing.T) {
	cases := []struct {
		provider string
		in, want []string
	}{
		{"claude",
			[]string{"--model", "opus", "--resume", "abc", "-c", "--session-id=x", "--fork-session", "--permission-mode", "plan", "-r", "d"},
			[]string{"--model", "opus", "--permission-mode", "plan"}},
		// A bare --resume opens the picker, and the flag after it is not
		// its value.
		{"claude", []string{"--resume", "--model", "opus"}, []string{"--model", "opus"}},
		{"claude", []string{"--continue", "--session-id", "u-1", "--resume=abc", "-r"}, []string{}},
		// After --, everything is the prompt's.
		{"claude", []string{"--model", "opus", "--", "-c", "--resume"}, []string{"--model", "opus", "--", "-c", "--resume"}},
		{"codex", []string{"resume", "abc", "--model", "o3"}, []string{"--model", "o3"}},
		{"codex", []string{"resume", "--last"}, []string{}},
		{"codex", []string{"--model", "o3"}, []string{"--model", "o3"}},
		// A provider with no known session flags keeps what it was given.
		{"kimi", []string{"--resume", "x"}, []string{"--resume", "x"}},
	}
	for _, c := range cases {
		in := slices.Clone(c.in)
		got := StoredArgs(c.provider, in)
		if got == nil || !slices.Equal(got, c.want) {
			t.Errorf("%s %q = %q, want %q", c.provider, c.in, got, c.want)
		}
		if !slices.Equal(in, c.in) {
			t.Errorf("%s %q: the caller's slice was changed to %q", c.provider, c.in, in)
		}
	}
}

// meta.json is readable by its owner alone and is never half-written: a
// write that fails on its way into place leaves the old record as it was,
// and no temporary file behind.
func TestMetaIsPrivateAndAtomic(t *testing.T) {
	dir := t.TempDir()
	exit := 3
	m := &Meta{
		ID: "0123456789abcdef", Name: "audit", Kind: KindAccount, Account: 3, Provider: "codex",
		HolderPID: 10, HolderStart: time.Date(2026, 11, 6, 9, 0, 0, 123456789, time.UTC), BootID: "b",
		ChildPID: 11, ChildStart: time.Date(2026, 11, 6, 9, 0, 1, 0, time.UTC),
		Protocol: Major, Version: "1.4.0-beta.4", Cwd: "/w", Args: []string{"--model", "o3"},
		Started: time.Date(2026, 11, 6, 9, 0, 0, 5, time.UTC),
	}
	if err := WriteMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(MetaPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("meta.json is %v", fi.Mode().Perm())
		}
	}
	raw, err := os.ReadFile(MetaPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"id"`, `"name"`, `"kind"`, `"account"`, `"provider"`, `"holder_pid"`, `"holder_start"`,
		`"boot_id"`, `"child_pid"`, `"child_start"`, `"protocol"`, `"version"`, `"cwd"`, `"args"`, `"conversation"`,
		`"conversation_name"`, `"conversation_cwd"`, `"token_until"`, `"started"`, `"2026-11-06T09:00:00.123456789Z"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("meta.json has no %s:\n%s", key, raw)
		}
	}
	for _, key := range []string{`"ended"`, `"exit"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("meta.json has %s before it is known:\n%s", key, raw)
		}
	}
	back, err := ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !back.HolderStart.Equal(m.HolderStart) || back.ID != m.ID || !slices.Equal(back.Args, m.Args) || back.Exit != nil {
		t.Fatalf("read back as %+v", back)
	}

	was := renameFile
	renameFile = func(string, string) error { return errors.New("the disk said no") }
	m.Exit, m.Ended = &exit, time.Now()
	err = WriteMeta(dir, m)
	renameFile = was
	if err == nil {
		t.Fatal("a failed rename was not reported")
	}
	after, rerr := os.ReadFile(MetaPath(dir))
	if rerr != nil || string(after) != string(raw) {
		t.Fatalf("the old record changed:\n%s", after)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != filepath.Base(MetaPath(dir)) {
			t.Errorf("left behind: %s", e.Name())
		}
	}

	if err := WriteMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	if back, err := ReadMeta(dir); err != nil || back.Exit == nil || *back.Exit != 3 || back.Ended.IsZero() {
		t.Fatalf("the ending was not kept: %+v %v", back, err)
	}
}
