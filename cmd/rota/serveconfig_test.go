package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/professor93/rota/api"
)

// writeConfig puts a server file where a server would read one, readable by
// nobody else, and points ROTA_HOME at the directory holding it.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	path := filepath.Join(home, api.ConfigName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// lineFor is one key's line of --print-config, without its padding.
func lineFor(t *testing.T, out, key string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		k, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Join(strings.Fields(line), " ")
		}
	}
	t.Fatalf("--print-config printed no %q:\n%s", key, out)
	return ""
}

// TestPrintConfigShowsWhereEachValueCameFrom walks the whole precedence: a
// flag beats the environment, the environment beats the file, and the file
// beats the default.
func TestPrintConfigShowsWhereEachValueCameFrom(t *testing.T) {
	writeConfig(t, `
[server]
listen = "127.0.0.1:9999"

[auth]
token = "from-the-file"

[runs]
timeout = "30s"
max_concurrent = 3
`)
	t.Setenv("ROTA_TOKEN", "from-the-environment")

	out, errb, code := call(t, "serve", "--print-config", "--timeout", "45s")
	if code != 0 || errb != "" {
		t.Fatalf("--print-config must exit 0 quietly: %d %q", code, errb)
	}
	for _, c := range []struct{ key, want string }{
		{"timeout", `timeout = "45s" # flag --timeout`},
		{"max_concurrent", "max_concurrent = 3 # file"},
		{"replay", "replay = 1000 # default"},
		{"listen", `listen = "127.0.0.1:9999" # file`},
		{"token", `token = "(set)" # env ROTA_TOKEN`},
	} {
		if got := lineFor(t, out, c.key); got != c.want {
			t.Fatalf("%s: %q, want %q", c.key, got, c.want)
		}
	}
	if strings.Contains(out, "from-the-file") || strings.Contains(out, "from-the-environment") {
		t.Fatalf("a token was printed:\n%s", out)
	}

	// The address is positional, and it beats the file like any other
	// argument.
	out, _, _ = call(t, "serve", "7777", "--print-config")
	if got := lineFor(t, out, "listen"); got != `listen = "7777" # argument` {
		t.Fatalf("the address given: %q", got)
	}
}

// TestPrintConfigIsAFileRotaWouldRead closes the loop: what it prints is
// something the parser reads back, masked token and comments included.
func TestPrintConfigIsAFileRotaWouldRead(t *testing.T) {
	writeConfig(t, "[routes]\nwebsocket = false\n")
	t.Setenv("ROTA_TOKEN", "t")
	out, _, code := call(t, "serve", "--print-config")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	round := filepath.Join(t.TempDir(), api.ConfigName)
	if err := os.WriteFile(round, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := api.LoadConfig(round)
	if err != nil {
		t.Fatalf("what --print-config printed is not a file rota reads: %v\n%s", err, out)
	}
	if c.Routes.WebSocket || !c.Routes.API || c.Auth.Token != "(set)" {
		t.Fatalf("read back as %#v / %#v", c.Routes, c.Auth)
	}
}

func TestServeRefusesAConfigThatIsNotThere(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	t.Setenv("ROTA_TOKEN", "t")
	missing := filepath.Join(t.TempDir(), "nowhere.toml")
	_, errb, code := call(t, "serve", "--config", missing, "--print-config")
	if code == 0 || !strings.Contains(errb, "nowhere.toml") {
		t.Fatalf("a named file that is not there must be refused: %d %q", code, errb)
	}
}

// TestServeWithoutAFileIsTheServerItAlwaysWas: the default file is allowed
// not to exist, and then nothing has changed.
func TestServeWithoutAFileIsTheServerItAlwaysWas(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	t.Setenv("ROTA_TOKEN", "t")
	out, errb, code := call(t, "serve", "--print-config")
	if code != 0 {
		t.Fatalf("%d %q", code, errb)
	}
	for _, want := range []string{
		`listen = "127.0.0.1:8787" # default`,
		"api = true # default",
		`timeout = "10m" # default`,
	} {
		key, _, _ := strings.Cut(want, " =")
		if got := lineFor(t, out, key); got != want {
			t.Fatalf("%q, want %q", got, want)
		}
	}
}

// TestServeRefusesAFileOthersCanRead is the permission rule seen from the
// command: the file may hold the token, so it is the operator's alone.
func TestServeRefusesAFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has neither the bits nor the convention")
	}
	path := writeConfig(t, "[server]\nquiet = true\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Skipf("this filesystem will not say: %v", err)
	}
	t.Setenv("ROTA_TOKEN", "t")
	_, errb, code := call(t, "serve", "--print-config")
	if code == 0 || !strings.Contains(errb, "chmod 600 "+path) {
		t.Fatalf("a world-readable file must be refused with the fix: %d %q", code, errb)
	}
}

func TestServeRefusesAFileItCannotUnderstand(t *testing.T) {
	writeConfig(t, "[runs]\ntimeout = 10\n")
	t.Setenv("ROTA_TOKEN", "t")
	_, errb, code := call(t, "serve", "--print-config")
	if code == 0 || !strings.Contains(errb, "runs.timeout wants a length of time") {
		t.Fatalf("%d %q", code, errb)
	}
	writeConfig(t, "[routes]\napi = false\n")
	if _, errb, code := call(t, "serve", "--print-config"); code == 0 ||
		!strings.Contains(errb, "routes.playground needs routes.api") {
		t.Fatalf("%d %q", code, errb)
	}
}

// TestServeTakesItsTokenFromTheFile: a file is a place to keep the token
// that the process table never sees.
func TestServeTakesItsTokenFromTheFile(t *testing.T) {
	writeConfig(t, "[auth]\ntoken = \"only-in-the-file\"\n")
	t.Setenv("ROTA_TOKEN", "")
	out, _, code := call(t, "serve", "--print-config")
	if code != 0 {
		t.Fatal(code)
	}
	if got := lineFor(t, out, "token"); got != `token = "(set)" # file` {
		t.Fatalf("%q", got)
	}
	// And a flag still beats it.
	out, _, _ = call(t, "serve", "--print-config", "--token", "typed")
	if got := lineFor(t, out, "token"); got != `token = "(set)" # flag --token` {
		t.Fatalf("%q", got)
	}
}
