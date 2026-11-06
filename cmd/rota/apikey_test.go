package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// Made-up credentials in the shapes Claude Code tells apart. None of them is
// anybody's.
const (
	testKey   = "sk-ant-api03-TESTONLY-not-a-real-key-0000000000-WXYZ"
	testSetup = "sk-ant-oat01-TESTONLY-not-a-real-setup-token-2222"
)

// keyAPI stands in for the API's model list, answering status and counting
// the questions it is asked.
func keyAPI(t *testing.T, status int) *atomic.Int64 {
	t.Helper()
	var asked atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeModelsEndpoint
	rota.ClaudeModelsEndpoint = srv.URL + "/v1/models"
	t.Cleanup(func() { rota.ClaudeModelsEndpoint = old })
	return &asked
}

// secretless fails when anything printed carries a credential, whole or the
// tail end of one.
func secretless(t *testing.T, what string, outs ...string) {
	t.Helper()
	for _, o := range outs {
		if strings.Contains(o, "TESTONLY") {
			t.Fatalf("%s printed a credential: %q", what, o)
		}
	}
}

// The key is read from standard input, never from the command line, and
// nothing printed afterwards carries it — only its last four characters.
func TestLoginKeyReadsTheKeyAndShowsOnlyItsEnd(t *testing.T) {
	keyAPI(t, 200)
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)

	out, errOut, code := runCLIStdin(t, testKey+"\n", "login", "claude", "--key")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	secretless(t, "login", out, errOut)
	for _, want := range []string{"Added claude account 1 (api key ...WXYZ)", "out of the rotation", "rota set 1 --order"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(home, "accounts.json"))
	if !strings.Contains(string(raw), testKey) || !strings.Contains(string(raw), `"credential": "apikey"`) {
		t.Fatalf("the store holds the key: %s", raw[:min(len(raw), 80)])
	}

	// The same key again, named this time: the same account, renamed.
	out, errOut, code = runCLIStdin(t, testKey+"\n", "login", "--key", "--label", "work credit")
	if code != 0 || !strings.Contains(out, "work credit") || !strings.Contains(out, "account 1") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	secretless(t, "login again", out, errOut)

	for _, args := range [][]string{{"list"}, {"list", "--short"}, {"list", "--json"}, {"set", "1"}, {"set", "1", "--json"}} {
		out, errOut, code := call(t, args...)
		if code != 0 {
			t.Fatalf("%v: %d %q", args, code, errOut)
		}
		secretless(t, strings.Join(args, " "), out, errOut)
		if args[0] == "list" && !strings.Contains(args[len(args)-1], "json") && !strings.Contains(out, "api key") {
			t.Fatalf("%v: USAGE says it is an API key: %q", args, out)
		}
		if strings.Contains(strings.Join(args, " "), "json") && !strings.Contains(out, `"credential": "apikey"`) {
			t.Fatalf("%v: JSON names the kind of credential: %q", args, out)
		}
	}
}

func TestLoginKeyJSON(t *testing.T) {
	keyAPI(t, 200)
	t.Setenv("ROTA_HOME", t.TempDir())
	out, errOut, code := runCLIStdin(t, testKey, "--json", "login", "--key")
	if code != 0 || !strings.Contains(out, `"status": "added"`) || !strings.Contains(out, `"credential": "apikey"`) ||
		!strings.Contains(out, `"order": 0`) {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	secretless(t, "json login", out, errOut)
}

// A key on the command line is refused before anything reads it, and never
// repeated: argv is in the process table and in a shell's history.
func TestLoginKeyNeverTakesTheKeyFromArgv(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	for _, args := range [][]string{
		{"login", "--key", testKey},
		{"login", "claude", "--key", testKey},
		{"login", "--key=" + testKey},
	} {
		out, errOut, code := runCLIStdin(t, "", args...)
		if code != 2 {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
		secretless(t, "a refused argv", out, errOut)
	}
}

func TestLoginKeyRefusesTheWrongThingWithoutRepeatingIt(t *testing.T) {
	keyAPI(t, 401)
	t.Setenv("ROTA_HOME", t.TempDir())
	for in, says := range map[string]string{
		testSetup + "\n": "setup-token",
		testKey + "\n":   "refused",
		"":               "nothing",
		"\n":             "nothing",
	} {
		out, errOut, code := runCLIStdin(t, in, "login", "--key")
		if code != 1 || !strings.Contains(errOut, says) {
			t.Fatalf("%q: %d %q %q", says, code, out, errOut)
		}
		secretless(t, "a refusal", out, errOut)
	}
	if out, _, _ := call(t, "list", "--short"); !strings.Contains(out, "No accounts") {
		t.Fatalf("nothing is stored: %q", out)
	}
}

func TestLoginLabelBelongsToAKey(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	for _, args := range [][]string{{"login", "--label", "x"}, {"login", "--key", "--long"}, {"login", "--key", "--label"}} {
		if _, errOut, code := runCLIStdin(t, testKey, args...); code != 2 {
			t.Fatalf("%v: %d %q", args, code, errOut)
		}
	}
}

// A setup token goes on the account named, as its long-lived token.
func TestLoginTokenPutsASetupTokenOnTheAccountNamed(t *testing.T) {
	home := seedLong(t, time.Time{}, "")
	out, errOut, code := runCLIStdin(t, testSetup+"\n", "login", "1", "--token")
	day := time.Now().AddDate(1, 0, 0).Format(time.DateOnly)
	if code != 0 || !strings.Contains(out, "long-lived token stored for #1 claude/a@b.c, good until "+day) {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	secretless(t, "--token", out, errOut)
	// Nothing about a pasted token can be checked, and that is said.
	if !strings.Contains(out, "cannot tell which claude.ai account this token belongs to or when it was made") ||
		!strings.Contains(out, "counted from now") {
		t.Fatalf("the caveat must be said: %q", out)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "accounts.json"))
	if !strings.Contains(string(raw), testSetup) {
		t.Fatal("not stored")
	}

	for _, c := range []struct {
		args []string
		in   string
		code int
	}{
		{[]string{"login", "--token"}, testSetup, 2},           // no account named
		{[]string{"login", "claude", "--token"}, testSetup, 2}, // a provider is not an account
		{[]string{"login", "1", "--token", testSetup}, "", 2},  // never from argv
		{[]string{"login", "9", "--token"}, testSetup, 1},      // no such account
		{[]string{"login", "1", "--token"}, testKey, 1},        // a key is not a setup token
	} {
		out, errOut, code := runCLIStdin(t, c.in, c.args...)
		if code != c.code {
			t.Fatalf("%v: %d %q %q", c.args[:2], code, out, errOut)
		}
		secretless(t, "a refused --token", out, errOut)
	}
}

// A Console API key given where a setup token belongs is refused on this
// machine, for what it is — when it is piped in on standard input, the only
// way it reaches this check — and never sent to the API to be refused there as a bad key.
func TestLoginTokenRefusesAKeyBeforeItLeavesTheMachine(t *testing.T) {
	asked := keyAPI(t, 401)
	home := seedLong(t, time.Time{}, "")
	out, errOut, code := runCLIStdin(t, testKey+"\n", "login", "1", "--token")
	if code != 1 || !strings.Contains(errOut, "Console API key") || !strings.Contains(errOut, "rota login --key") ||
		strings.Contains(errOut, "refused this key") {
		t.Fatalf("standard input: %d %q %q", code, out, errOut)
	}
	secretless(t, "a key as --token", out, errOut)
	for _, args := range [][]string{
		{"login", "1", "--token=" + testKey},
		{"login", "1", "--token", testKey},
	} {
		out, errOut, code := runCLIStdin(t, "", args...)
		if code != 2 {
			t.Fatalf("on the command line: %d %q %q", code, out, errOut)
		}
		secretless(t, "a key as --token on the command line", out, errOut)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("a key given as a setup token must go nowhere: %d requests reached the API", n)
	}
	if raw, _ := os.ReadFile(filepath.Join(home, "accounts.json")); strings.Contains(string(raw), "TESTONLY") {
		t.Fatal("nothing is stored")
	}
}

// A key typed where an argument goes — `rota login sk-ant-…` for `rota login
// --key`, above all — is never repeated, by any command, in any output; the
// refusal says it looks like a key and how one is pasted.
func TestAKeyTypedAsAnArgumentIsNeverRepeated(t *testing.T) {
	seedLong(t, time.Time{}, "")
	long := strings.Repeat("Q", 48)
	for _, args := range [][]string{
		{"login", testKey},
		{"--json", "login", testKey},
		{"login", "claude", testKey},
		{"login", long},
		{testKey},
		{"run", testKey},
		{"run", "1", testKey},
		{"run", "-p", testKey},
		{"run", "1", "--model=" + testKey, "-x"},
		{"set", testKey},
		{"set", "1", "--threshold", testKey},
		{"set", "1", "--remote-control", testKey},
		{"set", "1", "--cwd", testKey},
		{"set", "1", "--config", "/tmp/" + testKey},
		{"set", "1", "--sessions", testKey},
		{"remove", testKey},
		{"list", "--" + testKey},
		{"send", testKey, "hello"},
		{"serve", "8787", testKey},
		{"serve", "invite", testKey},
		{"serve", "passwd", "alice", "--role", testKey},
	} {
		out, errOut, code := runCLIStdin(t, "", args...)
		if code == 0 {
			t.Fatalf("%d: must be refused", len(args))
		}
		for _, secret := range []string{testKey, "TESTONLY", long} {
			if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
				t.Fatalf("a command repeated what looks like a key (args %d long): %q %q",
					len(args), strings.ReplaceAll(out, testKey, "<key>"), strings.ReplaceAll(errOut, testKey, "<key>"))
			}
		}
	}
	// The one that matters most says what to do instead.
	_, errOut, _ := call(t, "login", testKey)
	if !strings.Contains(errOut, "looks like a key") || !strings.Contains(errOut, "rota login --key") {
		t.Fatalf("the refusal must say how a key is pasted: %q", errOut)
	}
	// Nothing of it was kept as a directory, to be shown by a listing later.
	out, errOut, _ := call(t, "list", "--short", "--json")
	if strings.Contains(out+errOut, "TESTONLY") {
		t.Fatal("a key was saved as a path and listed")
	}
	// A key piped in as --input's first message is no more a prompt than one
	// typed as an argument.
	out, errOut, code := runCLIStdin(t, testKey+"\nmore\n", "run", "1", "--input")
	if code == 0 || strings.Contains(out+errOut, "TESTONLY") || !strings.Contains(errOut, "looks like a key") {
		t.Fatalf("--input: %d %q", code, strings.ReplaceAll(out+errOut, testKey, "<key>"))
	}
	// A directory with the letters inside a word of its own is a directory.
	dir := filepath.Join(t.TempDir(), "task-ant-colony-with-a-long-enough-tail")
	if _, errOut, code := call(t, "set", "1", "--cwd", dir); code != 0 {
		t.Fatalf("an ordinary directory: %d %q", code, errOut)
	}
}

// Remote Control refuses an API key, and rota says so by name rather than
// starting a session that cannot have it.
func TestAKeyAccountRefusesRemoteControl(t *testing.T) {
	keyAPI(t, 200)
	t.Setenv("ROTA_HOME", t.TempDir())
	if _, errOut, code := runCLIStdin(t, testKey, "login", "--key"); code != 0 {
		t.Fatal(errOut)
	}
	for _, args := range [][]string{{"set", "1", "--remote-control", "on"}, {"run", "1", "--remote-control"}} {
		out, errOut, code := call(t, args...)
		if code == 0 || !strings.Contains(errOut, "API key") {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
	}
}
