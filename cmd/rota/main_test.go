package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/wire"
)

// TestMain makes the real process handover unreachable.
//
// handOver ends in syscall.Exec, which replaces the running binary. A test
// that reaches it becomes the vendor CLI: `go test` then reports that CLI's
// exit status as the package's, so every failure already printed is
// discarded and every test after it never runs — while the package still
// says ok. That had been happening, silently, to everything below
// main_test.go. A test that means to exercise the handover calls handover(t).
func TestMain(m *testing.M) {
	execProcess = func(path string, _, _ []string) error {
		panic("this test reached the real process handover (" + path + "); call handover(t) to watch it instead")
	}
	os.Exit(m.Run())
}

// handover watches the handover instead of being replaced by it, and returns
// the argv rota would have given the CLI.
func handover(t *testing.T) *[]string {
	t.Helper()
	orig := execProcess
	t.Cleanup(func() { execProcess = orig })
	var argv []string
	execProcess = func(_ string, a, _ []string) error { argv = a; return nil }
	return &argv
}

func call(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return out.String(), errb.String(), code
}

// cliFakeProvider is an API-key provider used to exercise the whole login,
// list and remove path without a network.
type cliFakeProvider struct{}

func (cliFakeProvider) Name() string { return "t-cli-fake" }
func (cliFakeProvider) Begin(_ context.Context) (string, map[string]string, error) {
	return "https://example.test/keys", map[string]string{"kind": "apikey"}, nil
}
func (cliFakeProvider) Complete(_ context.Context, key string, _ map[string]string) (*rota.Token, error) {
	return &rota.Token{Access: key, Identity: &rota.Identity{UUID: "key-" + key}}, nil
}
func (cliFakeProvider) Launch(a *rota.Account, _ string) (*rota.Command, error) {
	return &rota.Command{Bin: "claude", Env: []string{"FAKE=" + a.Token.Access}}, nil
}

func init() { rota.Register(cliFakeProvider{}) }

func TestUsageAndUnknownInput(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	if out, _, code := call(t); code != 0 || !strings.Contains(out, "Usage") {
		t.Fatalf("bare: %d %q", code, out)
	}
	if out, _, code := call(t, "help"); code != 0 || !strings.Contains(out, "Usage") {
		t.Fatalf("help: %d %q", code, out)
	}
	if out, _, code := call(t, "--version"); code != 0 || !strings.Contains(out, wire.Version) {
		t.Fatalf("version: %d %q", code, out)
	}
	for _, args := range [][]string{{"bogus"}, {"list", "-x"}, {"remove", "abc"}, {"remove"}, {"list", "--refresh=1"}} {
		if _, err, code := call(t, args...); code != 2 || !strings.Contains(err, "usage") && !strings.Contains(err, "unknown") {
			t.Fatalf("%v: code=%d err=%q", args, code, err)
		}
	}
}

func TestErrorsGoToTheRightStream(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	if out, err, code := call(t, "remove", "9"); code != 1 || out != "" || !strings.Contains(err, "error: no such account") {
		t.Fatalf("text: %d %q %q", code, out, err)
	}
	if out, err, code := call(t, "--json", "remove", "9"); code != 1 || err != "" || !strings.Contains(out, `"error": "no such account: 9"`) {
		t.Fatalf("json: %d %q %q", code, out, err)
	}
	if _, err, code := call(t, "run", "9"); code != 1 || !strings.Contains(err, "no such account") {
		t.Fatalf("run: %d %q", code, err)
	}
	if _, err, code := call(t, "login", "gemni"); code != 1 || !strings.Contains(err, "not a known provider, account id or login id") {
		t.Fatalf("typo: %d %q", code, err)
	}
	if _, err, code := call(t, "login", "abcdef"); code != 1 || !strings.Contains(err, "no pending login") {
		t.Fatalf("stale id: %d %q", code, err)
	}
}

func TestAccountLifecycleThroughTheCLI(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no vendor CLI anywhere
	if out, _, code := call(t, "list"); code != 0 || !strings.Contains(out, "No accounts yet") {
		t.Fatalf("empty list: %d %q", code, out)
	}
	if out, _, code := call(t, "list", "--json"); code != 0 || !strings.Contains(out, `"accounts": []`) {
		t.Fatalf("empty json list: %d %q", code, out)
	}
	out, _, code := call(t, "login", "t-cli-fake")
	m := regexp.MustCompile(`rota login ([0-9a-f]{6}) <key>`).FindStringSubmatch(out)
	if code != 0 || m == nil || !strings.Contains(out, "https://example.test/keys") {
		t.Fatalf("begin: %d %q", code, out)
	}
	if out, _, code := call(t, "login", m[1], "sk-test-key"); code != 0 || !strings.Contains(out, "Added t-cli-fake account 1") {
		t.Fatalf("finish: %d %q", code, out)
	}
	out, _, code = call(t, "--json", "login", "t-cli-fake")
	if code != 0 || !strings.Contains(out, `"kind": "apikey"`) {
		t.Fatalf("json begin: %d %q", code, out)
	}
	id := regexp.MustCompile(`"id": "([0-9a-f]{6})"`).FindStringSubmatch(out)[1]
	if out, _, code := call(t, "login", id, "sk-test-key", "--json"); code != 0 || !strings.Contains(out, `"status": "refreshed"`) || !strings.Contains(out, `"id": 1`) {
		t.Fatalf("same key lands on the same account: %d %q", code, out)
	}
	out, _, code = call(t, "list")
	if code != 0 || !strings.Contains(out, "t-cli-fake") || !strings.Contains(out, "  ok") {
		t.Fatalf("list: %d %q", code, out)
	}
	if !strings.Contains(out, "CHECKED") || !strings.Contains(out, "n/a") {
		t.Fatalf("an unmetered provider must be shown as having no limits: %q", out)
	}
	out, _, code = call(t, "list", "t-cli-fake", "--json", "-r")
	if code != 0 || !strings.Contains(out, `"provider": "t-cli-fake"`) || !strings.Contains(out, `"status": "ok"`) {
		t.Fatalf("json list: %d %q", code, out)
	}
	if !strings.Contains(out, `"metered": false`) || strings.Contains(out, `"checkedAt"`) {
		t.Fatalf("json list must say a provider is unmetered rather than unchecked: %q", out)
	}
	if out, _, code := call(t, "list", "claude"); code != 0 || !strings.Contains(out, "No claude accounts") {
		t.Fatalf("filtered list: %d %q", code, out)
	}
	// rota builds a command line for the CLIs it knows; a provider it can
	// only launch says so, and points at the ways to run it anyway. The
	// binary has to be findable for that to be the honest complaint —
	// otherwise the missing CLI is the more useful thing to say.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err, code := call(t, "run", "1", "-p", "hi"); code != 1 || !strings.Contains(err, "rota run <id> --") {
		t.Fatalf("run without a modelled CLI: %d %q", code, err)
	}
	t.Setenv("PATH", t.TempDir())
	if out, _, code := call(t, "remove", "1", "--json"); code != 0 || !strings.Contains(out, `"removed"`) || !strings.Contains(out, `"provider": "t-cli-fake"`) {
		t.Fatalf("remove: %d %q", code, out)
	}
	if out, _, code := call(t, "list"); code != 0 || !strings.Contains(out, "No accounts yet") {
		t.Fatalf("list after remove: %d %q", code, out)
	}
}

func TestRendering(t *testing.T) {
	q := &rota.Quota{Windows: []rota.Window{
		{Name: "5h", Percent: 2.4, ResetsAt: rota.When{Time: time.Now().Add(2*time.Hour + 40*time.Minute + 30*time.Second)}, Primary: true},
		{Name: "Fable", Percent: 91, ResetsAt: rota.When{Time: time.Now().Add(-time.Minute)}, Scoped: true},
		{Name: "7d", Percent: 40},
	}}
	if got := summarize(q); got != "5h 2% (2h 40m)  Fable 91% (0m)  7d 40%" {
		t.Fatalf("%q", got)
	}
	if summarize(nil) != "-" || wire.Countdown(rota.When{}) != "" {
		t.Fatal("empty cases")
	}
	if got := wire.Countdown(rota.When{Time: time.Now().Add(49*time.Hour + 5*time.Minute)}); got != "2d 1h" {
		t.Fatalf("%q", got)
	}
}

func TestServeParsesItsAddressAndDemandsAToken(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	t.Setenv("ROTA_TOKEN", "")
	if _, err, code := call(t, "serve"); code != 2 || !strings.Contains(err, "token") {
		t.Fatalf("a server with no token must refuse: %d %q", code, err)
	}
	cases := []struct{ in, want string }{
		{"", "127.0.0.1:8787"},
		{"9000", "0.0.0.0:9000"},
		{":9000", ":9000"},
		{"127.0.0.1:9000", "127.0.0.1:9000"},
		{"localhost:9000", "localhost:9000"},
	}
	for _, c := range cases {
		got, err := listenAddr(c.in)
		if err != nil || got != c.want {
			t.Fatalf("%q: got %q (%v) want %q", c.in, got, err, c.want)
		}
	}
	if _, err := listenAddr("not:a:port"); err == nil {
		t.Fatal("a nonsense address must be refused")
	}
	if _, err, code := call(t, "serve", "9000", "--token=t", "--root", "/definitely/not/here"); code != 2 || !strings.Contains(err, "root") {
		t.Fatalf("a missing root must be refused: %d %q", code, err)
	}
}

func TestDelegatedLoginTellsYouWhatToRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	out, _, code := call(t, "login", "grok")
	m := regexp.MustCompile(`rota login ([0-9a-f]{6})`).FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("begin: %d %q", code, out)
	}
	if !strings.Contains(out, "console.x.ai") || !strings.Contains(out, "without a key") {
		t.Fatalf("both routes must be offered: %q", out)
	}
	// Finishing with no key at all delegates the login to grok itself.
	out, _, code = call(t, "login", m[1])
	if code != 0 || !strings.Contains(out, "Added grok account 1") {
		t.Fatalf("finish: %d %q", code, out)
	}
	// It points at rota's own command rather than making the person retype
	// an environment variable they must not get wrong.
	if !strings.Contains(out, "rota login 1") {
		t.Fatalf("it must say how to sign the account in: %q", out)
	}
	if out, _, code := call(t, "list"); code != 0 || !strings.Contains(out, "grok") {
		t.Fatalf("list: %d %q", code, out)
	}
	// The same in JSON, for anything driving rota rather than reading it.
	out, _, _ = call(t, "--json", "login", "grok")
	id := regexp.MustCompile(`"id": "([0-9a-f]{6})"`).FindStringSubmatch(out)[1]
	out, _, code = call(t, "--json", "login", id)
	if code != 0 || !strings.Contains(out, `"delegated": true`) || !strings.Contains(out, `"loginCommand"`) {
		t.Fatalf("json finish: %d %q", code, out)
	}
}

func TestServeDocumentsItself(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	out, _, code := call(t, "serve", "--help")
	if code != 0 {
		t.Fatalf("asking for help is not an error: %d", code)
	}
	for _, want := range []string{"rota serve [address]", "-token", "-root", "-allow-dangerous", "-tls-cert", "ROTA_TOKEN"} {
		if !strings.Contains(out, want) {
			t.Fatalf("serve --help must document %q:\n%s", want, out)
		}
	}
}

func TestLoginRunsTheDelegatedFlowItself(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	out, _, _ := call(t, "--json", "login", "grok")
	id := regexp.MustCompile(`"id": "([0-9a-f]{6})"`).FindStringSubmatch(out)[1]
	if _, _, code := call(t, "login", id); code != 0 {
		t.Fatalf("finish: %d", code)
	}

	// Without the vendor CLI installed it says so, rather than printing a
	// command and leaving the person to discover that.
	t.Setenv("PATH", t.TempDir())
	_, errOut, code := call(t, "login", "1")
	if code != 1 || !strings.Contains(errOut, "PATH") {
		t.Fatalf("missing CLI: %d %q", code, errOut)
	}

	// An account rota holds a credential for has nothing to log into.
	if _, errOut, code := call(t, "login", "99"); code != 1 || !strings.Contains(errOut, "no such account") {
		t.Fatalf("unknown account: %d %q", code, errOut)
	}
}

// One command signs an account in, whichever of the three things that means:
// starting a login, finishing one, or handing a delegated account to its own
// CLI. Which of them it is comes from the argument, not from a second verb.
func TestLoginIsOneCommandForEveryWayIn(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())

	// Nothing after it starts a login on the default provider.
	out, _, code := call(t, "login")
	if code != 0 || !strings.Contains(out, "claude") {
		t.Fatalf("bare login must start a claude login: %d %q", code, out)
	}
	// A provider name starts one there instead.
	out, _, code = call(t, "login", "t-cli-fake")
	if code != 0 {
		t.Fatalf("provider: %d %q", code, out)
	}
	m := regexp.MustCompile(`rota login ([0-9a-f]{6})`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the login must say how to finish itself, in its own name: %q", out)
	}
	// A login id finishes that login.
	if out, _, code := call(t, "login", m[1], "sk-test-key"); code != 0 || !strings.Contains(out, "account 1") {
		t.Fatalf("finish: %d %q", code, out)
	}
	// And an account id is the account, not a login id.
	if _, errOut, code := call(t, "login", "1"); code == 0 || !strings.Contains(errOut, "rota holds its credential") {
		t.Fatalf("account 1 signs in through rota, so it has nothing to log into: %d %q", code, errOut)
	}
	// The old spellings are gone.
	if _, errOut, code := call(t, "auth", "claude"); code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("`rota auth` must be gone: %d %q", code, errOut)
	}
}

func TestRunIsNonInteractiveAndProviderNeutral(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	t.Setenv("PATH", t.TempDir()) // no vendor CLI anywhere
	// A claude account, so ask has a real CLI vocabulary to build.
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(
		`{"accounts":[{"id":1,"provider":"claude","email":"a@b.c","token":{"accessToken":"t"}}],"nextId":2}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// No prompt means the CLI's own session, with or without an id; there
	// is no vendor CLI here, which is as far as that can get.
	if _, errOut, code := call(t, "run"); code != 1 || !strings.Contains(errOut, "PATH") {
		t.Fatalf("interactive, rotation's choice: %d %q", code, errOut)
	}
	if _, errOut, code := call(t, "run", "1"); code != 1 || !strings.Contains(errOut, "PATH") {
		t.Fatalf("interactive, named account: %d %q", code, errOut)
	}
	// Flags but no prompt is a mistake rather than either of those.
	if _, errOut, code := call(t, "run", "1", "--model", "opus"); code != 2 || !strings.Contains(errOut, "usage") {
		t.Fatalf("a prompt is required once flags are given: %d %q", code, errOut)
	}
	if _, errOut, code := call(t, "run", "99", "hi"); code != 1 || !strings.Contains(errOut, "no such account") {
		t.Fatalf("unknown account: %d %q", code, errOut)
	}
	// A model belonging to another provider is refused before anything runs.
	if _, errOut, code := call(t, "run", "1", "hi", "--model", "gpt-5.6-sol"); code != 1 || !strings.Contains(errOut, "no model") {
		t.Fatalf("model check: %d %q", code, errOut)
	}
	// The vendor CLI is missing here, which is as far as this can get.
	_, errOut, code := call(t, "run", "1", "hello")
	if code != 1 || !strings.Contains(errOut, "PATH") {
		t.Fatalf("missing CLI: %d %q", code, errOut)
	}
	if _, _, code := call(t, "run", "1", "hi", "--effort", "nonsense"); code != 1 {
		t.Fatalf("effort check: %d", code)
	}
}

func TestRunDocumentsItself(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	out, _, code := call(t, "run", "--help")
	if code != 0 {
		t.Fatalf("%d", code)
	}
	for _, want := range []string{"rota run [id]", "-model", "-effort", "-stream", "--json",
		"never interactive", "-resume", "-i", "rotation"} {
		if !strings.Contains(out, want) {
			t.Fatalf("run --help must document %q:\n%s", want, out)
		}
	}
}

func TestRunTakesThePromptHoweverItIsWritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(
		`{"accounts":[{"id":1,"provider":"claude","email":"a@b.c","token":{"accessToken":"t"}}],"nextId":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\n" +
		`printf '[{"type":"result","result":"ANSWERED","session_id":"s-1"}]\n'` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	// The prompt is positional, but -p and --print are what people's hands
	// already type.
	for _, argv := range [][]string{
		{"run", "1", "hello"},
		{"run", "1", "-p", "hello"},
		{"run", "1", "--print", "hello"},
		{"run", "1", "hello", "--model", "sonnet"},
		{"run", "1", "--model", "sonnet", "hello"},
	} {
		out, errOut, code := call(t, argv...)
		if code != 0 || out != "ANSWERED\n" {
			t.Fatalf("%v: %d %q %q", argv, code, out, errOut)
		}
	}
	// Several words are one prompt.
	if out, _, code := call(t, "run", "1", "tell", "me", "about", "this"); code != 0 || out != "ANSWERED\n" {
		t.Fatalf("%d %q", code, out)
	}
	// No prompt at all hands the terminal to the CLI, which is what the help
	// says it does. The handover is stubbed because the real one is
	// syscall.Exec: reaching it here would replace this test binary with the
	// fake CLI, and `go test` would report that CLI's exit status as the
	// package's — discarding every failure so far and skipping every test
	// after this one.
	handedTo := handover(t)
	if _, errOut, code := call(t, "run", "1"); code != 0 {
		t.Fatalf("bare run must open the CLI: %d %q", code, errOut)
	}
	if len(*handedTo) == 0 || (*handedTo)[0] != "claude" {
		t.Fatalf("the CLI must be handed the terminal under its own name: %v", *handedTo)
	}
}
func TestRunPrintsOnlyTheAnswerUnlessAsked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(
		`{"accounts":[{"id":1,"provider":"claude","email":"a@b.c","token":{"accessToken":"t"}}],"nextId":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A stand-in for the vendor CLI that answers and says a few other
	// things, the way a real one does.
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\n" +
		`printf '[{"type":"system","subtype":"init","session_id":"s-1"},` +
		`{"type":"result","result":"THE ANSWER","session_id":"s-1","total_cost_usd":0.01}]\n'` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	out, errOut, code := call(t, "run", "1", "hello")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if out != "THE ANSWER\n" {
		t.Fatalf("stdout must carry the answer and nothing else: %q", out)
	}
	if errOut != "" {
		t.Fatalf("text mode must say nothing else at all: %q", errOut)
	}

	// Everything about the run is still available, on request.
	out, errOut, code = call(t, "run", "1", "hello", "-v")
	if code != 0 || out != "THE ANSWER\n" {
		t.Fatalf("%d %q", code, out)
	}
	for _, want := range []string{"claude/a@b.c", "s-1", "--resume"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("-v must report %q: %q", want, errOut)
		}
	}

	// And --json carries it whether or not anyone asked for -v.
	out, _, code = call(t, "--json", "run", "1", "hello")
	if code != 0 || !strings.Contains(out, `"session_id": "s-1"`) || !strings.Contains(out, `"result": "THE ANSWER"`) {
		t.Fatalf("json: %d %q", code, out)
	}
}

func TestRunTakesJSONWhereverItIsWritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(
		`{"accounts":[{"id":1,"provider":"claude","email":"a@b.c","token":{"accessToken":"t"}}],"nextId":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\n" +
		`printf '[{"type":"result","result":"ANSWERED","session_id":"s-1"}]\n'` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	for _, argv := range [][]string{
		{"--json", "run", "1", "hi"},
		{"run", "1", "hi", "--json"},
		{"run", "1", "--json", "hi"},
	} {
		out, _, code := call(t, argv...)
		if code != 0 || !strings.Contains(out, `"result": "ANSWERED"`) {
			t.Fatalf("%v: %d %q", argv, code, out)
		}
	}
	// But a vendor's own --json still reaches it untouched, which means the
	// CLI is handed the terminal rather than asked a question.
	passed := handover(t)
	if _, errOut, code := call(t, "run", "1", "--", "--json"); code != 0 {
		t.Fatalf("verbatim --json: %d %q", code, errOut)
	}
	if !slices.Contains(*passed, "--json") {
		t.Fatalf("--json after -- belongs to the CLI: %v", *passed)
	}
}

func TestRunSaysWhenTheCLIIsSimplyNotInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	t.Setenv("PATH", t.TempDir())
	// A provider whose headless flags rota does not model, so the run fails
	// with ErrUnsupported and the message has to choose what to blame.
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(
		`{"accounts":[{"id":4,"provider":"t-other-cli","uuid":"u","token":{"accessToken":"t"}}],"nextId":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := call(t, "run", "4", "hello")
	if code != 1 {
		t.Fatalf("code: %d %q", code, errOut)
	}
	// The useful fact first: there is nothing to run.
	if !strings.Contains(errOut, "PATH") || !strings.Contains(errOut, "t-other-cli-bin") {
		t.Fatalf("it must say the CLI is missing: %q", errOut)
	}
	// And not mislead about rota's own vocabulary being the obstacle.
	if strings.Contains(errOut, "not modelled") {
		t.Fatalf("a missing CLI is not a modelling problem: %q", errOut)
	}
}

func TestLoginPassesExtraArgumentsToTheVendorLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	t.Setenv("PATH", t.TempDir())
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(
		`{"accounts":[{"id":9,"provider":"kimi","uuid":"u","delegated":true}],"nextId":10}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The vendor's login takes options of its own — a region, say — and
	// rota has no business knowing which one is right.
	_, errOut, code := call(t, "login", "9", "--region", "mainland-cn")
	if code != 1 || !strings.Contains(errOut, "PATH") {
		t.Fatalf("%d %q", code, errOut)
	}
}

// The thing people do most often needs no verb: `rota "..."` is `rota run
// "..."`, and a leading number still names the account.
//
// What makes that safe is that a mistyped command must never turn into a
// question someone pays for. So only an argument that could not be a command
// counts: one with whitespace in it, which no command has, or one -p
// introduces. `rota lst` stays an error.
func TestAPromptNeedsNoVerb(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(
		`{"accounts":[{"id":1,"provider":"claude","email":"a@b.c","token":{"accessToken":"t"}},`+
			`{"id":2,"provider":"claude","email":"b@b.c","token":{"accessToken":"t"}}],"nextId":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\n" +
		`printf '[{"type":"result","result":"ANSWERED","session_id":"s-1"}]\n'` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	for _, argv := range [][]string{
		{"summarize this repo"},
		{"2", "summarize this repo"},
		{"-p", "hello"},
		{"2", "-p", "hello"},
	} {
		out, errOut, code := call(t, argv...)
		if code != 0 || out != "ANSWERED\n" {
			t.Fatalf("%v must be a question: %d %q %q", argv, code, out, errOut)
		}
	}

	// A mistyped command must stay a mistyped command. Reading it as a prompt
	// would send it to a provider and charge for the answer.
	for _, argv := range [][]string{{"lst"}, {"bogus"}, {"sett", "1"}, {"remov", "1"}} {
		if _, errOut, code := call(t, argv...); code != 2 || !strings.Contains(errOut, "unknown command") {
			t.Fatalf("%v must stay an unknown command: %d %q", argv, code, errOut)
		}
	}

	// And a real command still wins, whitespace in its arguments or not.
	if out, _, code := call(t, "list", "--short"); code != 0 || !strings.Contains(out, "claude") {
		t.Fatalf("list: %d %q", code, out)
	}
}
