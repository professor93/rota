package wire

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

func TestUploadsLandInAPrivateDirectory(t *testing.T) {
	files := []Upload{
		{Path: "notes/a.txt", Content: base64.StdEncoding.EncodeToString([]byte("hi"))},
		{Path: "b.bin", Content: base64.StdEncoding.EncodeToString([]byte{1, 2})},
	}
	dir, err := StageUploads(files)
	if dir != "" {
		defer os.RemoveAll(dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the directory must be private: %v %v", err, fi.Mode())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "notes", "a.txt"))
	if err != nil || string(raw) != "hi" {
		t.Fatalf("%q %v", raw, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "b.bin")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("uploaded files must be private: %v", fi.Mode())
	}
}

func TestUploadPathsAreRefusedNotRewritten(t *testing.T) {
	for _, bad := range []string{"../escape.txt", "/abs.txt", "", "~/x", "a/../../b", "./x"} {
		dir, err := StageUploads([]Upload{{Path: bad, Content: "aGk="}})
		if dir != "" {
			os.RemoveAll(dir)
		}
		if !errors.Is(err, rota.ErrInvalidRequest) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	dir, err := StageUploads([]Upload{{Path: "a.txt", Content: "not base64!!"}})
	if dir != "" {
		os.RemoveAll(dir)
	}
	if !errors.Is(err, rota.ErrInvalidRequest) || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("%v", err)
	}
	if d, err := StageUploads(nil); d != "" || err != nil {
		t.Fatalf("no files means no directory: %q %v", d, err)
	}
}

func TestTerminalEventDescribesHowARunEnded(t *testing.T) {
	res := &rota.Result{ExitCode: 3, SessionID: "s1", IsError: true, DurationMS: 42}
	raw, _ := json.Marshal(Ended(res, nil))
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	if doc["type"] != "done" || doc["exit_code"].(float64) != 3 || doc["session_id"] != "s1" ||
		doc["is_error"] != true || doc["duration_ms"].(float64) != 42 {
		t.Fatalf("%s", raw)
	}
	raw, _ = json.Marshal(Ended(nil, errors.New("boom")))
	json.Unmarshal(raw, &doc)
	if doc["type"] != "error" || doc["error"] != "boom" {
		t.Fatalf("%s", raw)
	}
	// Success must still say so out loud.
	raw, _ = json.Marshal(Ended(&rota.Result{}, nil))
	if !strings.Contains(string(raw), `"exit_code":0`) || !strings.Contains(string(raw), `"is_error":false`) {
		t.Fatalf("a clean run must report its zero exit code: %s", raw)
	}
	if Ended(res, nil).Type != "done" || Ended(nil, nil).Type != "done" {
		t.Fatal("a run with no result still ends")
	}
	// A streaming caller that let the rotation choose learns which account
	// answered only from this event.
	if got := Ended(&rota.Result{Account: 7}, nil).Account; got != 7 {
		t.Fatalf("the terminal event must name the account that ran, got %d", got)
	}
}

/* ------------------------------------------------- an account rendered -- */

// meter is a provider that publishes a usage endpoint; plain is one that
// does not. Nothing here talks to a network.
type plain struct{ name string }

func (p plain) Name() string { return p.name }
func (p plain) Begin(_ context.Context) (string, map[string]string, error) {
	return "https://x/auth", map[string]string{"verifier": "v"}, nil
}
func (p plain) Complete(_ context.Context, code string, _ map[string]string) (*rota.Token, error) {
	return &rota.Token{Access: code}, nil
}
func (p plain) Launch(a *rota.Account, _ string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}

type metered struct{ plain }

func (m metered) Quota(context.Context, string) (*rota.Quota, error) { return &rota.Quota{}, nil }

func msAgo(d time.Duration) int64 { return time.Now().Add(-d).UnixMilli() }

func TestDescribeReportsWhenLimitsWereChecked(t *testing.T) {
	rota.Register(metered{plain{name: "t-wire-meter"}})
	rota.Register(plain{name: "t-wire-plain"})

	if v := Describe(&rota.Account{ID: 9, Provider: "t-wire-plain"}); v.Metered {
		t.Fatal("a provider with no usage endpoint is not metered")
	}
	a := &rota.Account{ID: 1, Provider: "t-wire-meter"}
	if !Describe(a).Metered {
		t.Fatal("a provider with a usage endpoint is metered")
	}
	if v := Describe(a); v.CheckedAt != "" || v.CheckedAgo != "" {
		t.Fatalf("never checked: %+v", v)
	}

	a.QuotaAt = msAgo(90 * time.Second)
	a.Quota = &rota.Quota{}
	v := Describe(a)
	// The instant itself, not the hour it happens to fall in: comparing a
	// formatted prefix fails for ninety seconds after every hour turns.
	when, err := time.Parse(time.RFC3339, v.CheckedAt)
	if err != nil {
		t.Fatalf("checkedAt must be RFC 3339: %q (%v)", v.CheckedAt, err)
	}
	if d := time.Since(when); d < time.Minute || d > 2*time.Minute {
		t.Fatalf("checkedAt should be about ninety seconds ago, got %v", d)
	}
	if v.CheckedAgo != "1m ago" {
		t.Fatalf("%+v", v)
	}
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{{5 * time.Second, "just now"}, {3 * time.Hour, "3h ago"}, {50 * time.Hour, "2d ago"}} {
		a.QuotaAt = msAgo(c.ago)
		if got := Describe(a).CheckedAgo; got != c.want {
			t.Errorf("%v ago: got %q, want %q", c.ago, got, c.want)
		}
	}
}

// A view carries the numbers a store holds, present even when they are zero:
// what an order or threshold of 0 means is decided by whoever runs the
// rotation, and a missing field would leave that decision with nothing to
// read. Percent is a reading rather than a rule, so lib does fill it in.
func TestDescribeAlwaysCarriesTheStoredNumbers(t *testing.T) {
	raw, err := json.Marshal(Describe(&rota.Account{ID: 1, Provider: "claude"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"order":0`, `"threshold":0`, `"percent":0`, `"metered":true`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("%s is missing from %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "checkedAt") {
		t.Fatalf("an unread account has no reading to timestamp: %s", raw)
	}
}

// A delegated account is signed in by running the vendor's own CLI. rota
// prints that command for a person to paste, so it has to read as one line
// with the environment in front of it.
func TestLoginCommandReadsAsOneLine(t *testing.T) {
	home := "/tmp/rota-home"
	a := &rota.Account{ID: 1, Provider: "grok", Delegated: true}
	line := LoginCommand(a, home)
	for _, want := range []string{"GROK_HOME=" + home, "grok", "login", "--device-code"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	if !strings.HasPrefix(line, "GROK_HOME=") {
		t.Fatalf("the environment comes first, or the line cannot be pasted: %q", line)
	}
	// An account rota holds a credential for has nothing to run.
	if got := LoginCommand(&rota.Account{ID: 2, Provider: "grok"}, home); got != "" {
		t.Fatalf("only a delegated account has a login to run, got %q", got)
	}
	if got := LoginCommand(&rota.Account{ID: 3, Provider: "claude", Delegated: true}, home); got != "" {
		t.Fatalf("claude signs in through rota, got %q", got)
	}
}
