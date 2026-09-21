package api

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/toml"
)

// write puts a file where a server would read one, readable by nobody else.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ConfigName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigKeepsTheDefaultsItIsNotTold(t *testing.T) {
	c, err := LoadConfig(write(t, "[server]\nlisten = \"0.0.0.0:9\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != "0.0.0.0:9" {
		t.Fatalf("listen is %q", c.Server.Listen)
	}
	want := DefaultConfig()
	want.Server.Listen = "0.0.0.0:9"
	want.From, c.From = nil, nil
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("one key changed more than itself:\n%#v", c)
	}
}

func TestLoadConfigRemembersWhatTheFileSaid(t *testing.T) {
	c, err := LoadConfig(write(t, "[runs]\ntimeout = \"30s\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.From["runs.timeout"] != "file" {
		t.Fatalf("the file's own key is %q", c.From["runs.timeout"])
	}
	if c.From["runs.replay"] != "" {
		t.Fatalf("a key the file never mentioned is %q", c.From["runs.replay"])
	}
}

func TestLoadConfigReadsTheWholeSchema(t *testing.T) {
	c, err := LoadConfig(write(t, `
[server]
listen = "0.0.0.0:1234"
quiet = true

[tls]
cert = "/c.pem"
key = "/k.pem"

[auth]
token = "t"
token_env = "OTHER"

[routes]
api = true
playground = false
websocket = false

[runs]
timeout = "30s"
max_concurrent = 2
input_timeout = "5m"
input_grace = "-1s"
replay = 10
refresh_every = "0s"
roots = ["/tmp"]
allow_dangerous = true
allow_raw_flags = true

[store]
dir = "/srv/rota"
`))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Server: ServerSection{Listen: "0.0.0.0:1234", Quiet: true},
		TLS:    TLSSection{Cert: "/c.pem", Key: "/k.pem"},
		Auth:   AuthSection{Token: "t", TokenEnv: "OTHER"},
		Routes: RoutesSection{API: true},
		Runs: RunsSection{
			Timeout: 30 * time.Second, MaxConcurrent: 2, InputTimeout: 5 * time.Minute,
			InputGrace: -time.Second, Replay: 10, RefreshEvery: 0,
			Roots: []string{"/tmp"}, AllowDangerous: true, AllowRawFlags: true,
		},
		Store: StoreSection{Dir: "/srv/rota"},
	}
	c.From = nil
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("decoded\n%#v\nwant\n%#v", c, want)
	}
}

func TestLoadConfigRefusals(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"two tokens", "[auth]\ntoken = \"a\"\ntoken_file = \"/t\"\n", "two ways to say the same thing"},
		{"half a certificate", "[tls]\ncert = \"/c\"\n", "tls.cert and tls.key go together"},
		{"a page with no API", "[routes]\napi = false\nwebsocket = false\n", "routes.playground needs routes.api"},
		{"a socket with no API", "[routes]\napi = false\nplayground = false\n", "routes.websocket needs routes.api"},
		{"no time to run", "[runs]\ntimeout = \"0s\"\n", "runs.timeout must be longer than nothing"},
		{"no time to talk", "[runs]\ninput_timeout = \"-1m\"\n", "runs.input_timeout must be longer than nothing"},
		{"nothing at once", "[runs]\nmax_concurrent = 0\n", "runs.max_concurrent must be more than nothing"},
		{"nothing kept", "[runs]\nreplay = -1\n", "runs.replay must be more than nothing"},
		{"a nonsense address", "[server]\nlisten = \"not:a:port\"\n", "server.listen"},
		{"a key nobody knows", "[runs]\ntiemout = \"1m\"\n", `"tiemout" is not something [runs] has`},
		{"a section nobody knows", "[servers]\nlisten = \"\"\n", `"servers" is not something the top level has`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(write(t, c.body))
			if err == nil {
				t.Fatalf("%q was accepted", c.body)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v\nmust say %q", err, c.want)
			}
		})
	}
}

// TestLoadConfigRefusesAFileOthersCanRead is the reason this file has a
// permission rule at all: it may hold the token that runs every account on
// the machine.
func TestLoadConfigRefusesAFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has neither the bits nor the convention")
	}
	path := write(t, "[server]\nquiet = true\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("a world-readable file was accepted")
	}
	for _, want := range []string{"readable by other users", "chmod 600 " + path} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%v\nmust say %q", err, want)
		}
	}
}

func TestLoadConfigNeedsTheFileToBeThere(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nowhere.toml")); err == nil {
		t.Fatal("a file that is not there was accepted")
	}
}

func TestTokenFromEachPlaceItCanCome(t *testing.T) {
	t.Setenv("ROTA_TOKEN", "")
	c := DefaultConfig()
	if tok, _, err := c.Token(); err != nil || tok != "" {
		t.Fatalf("with nothing set: %q %v", tok, err)
	}

	c.Auth.Token = "in-the-file"
	tok, from, err := c.Token()
	if err != nil || tok != "in-the-file" || from != "file" {
		t.Fatalf("from the file: %q %q %v", tok, from, err)
	}

	t.Setenv("ROTA_TOKEN", "in-the-environment")
	tok, from, err = c.Token()
	if err != nil || tok != "in-the-environment" || from != "env ROTA_TOKEN" {
		t.Fatalf("the environment must beat the file: %q %q %v", tok, from, err)
	}

	// A file may name another variable, and then ROTA_TOKEN is not it.
	c.Auth.TokenEnv = "OTHER_TOKEN"
	t.Setenv("OTHER_TOKEN", "in-the-other-one")
	tok, from, err = c.Token()
	if err != nil || tok != "in-the-other-one" || from != "env OTHER_TOKEN" {
		t.Fatalf("from a named variable: %q %q %v", tok, from, err)
	}
}

func TestTokenFromAFile(t *testing.T) {
	t.Setenv("ROTA_TOKEN", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("  from-a-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	c.Auth.TokenFile = path
	tok, from, err := c.Token()
	if err != nil || tok != "from-a-file" || from != "file" {
		t.Fatalf("%q %q %v", tok, from, err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.Token(); err == nil || !strings.Contains(err.Error(), "chmod 600 "+path) {
			t.Fatalf("a token file others can read must be refused: %v", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Token(); err == nil || !strings.Contains(err.Error(), "holds no token") {
		t.Fatalf("an empty token file must be refused: %v", err)
	}
	c.Auth.TokenFile = filepath.Join(dir, "nowhere")
	if _, _, err := c.Token(); err == nil {
		t.Fatal("a token file that is not there must be refused")
	}
}

func TestServeTurnsAConfigIntoOptions(t *testing.T) {
	dir := t.TempDir()
	c := DefaultConfig()
	c.Server.Listen = "9000"
	c.Runs.Roots = []string{dir}
	c.Runs.RefreshEvery = 0
	c.Routes.WebSocket = false
	c.TLS = TLSSection{Cert: "/c.pem", Key: "/k.pem"}
	opts, l, err := c.Serve()
	if err != nil {
		t.Fatal(err)
	}
	if l.Addr != "0.0.0.0:9000" || l.Cert != "/c.pem" || l.Key != "/k.pem" {
		t.Fatalf("listener %#v", l)
	}
	if opts.RefreshEvery != -1 {
		t.Fatalf("a refresh turned off must say off, not %v", opts.RefreshEvery)
	}
	if len(opts.Roots) != 1 || opts.Roots[0] != dir {
		t.Fatalf("roots %q", opts.Roots)
	}
	if opts.Routes == nil || opts.Routes.WebSocket || !opts.Routes.API {
		t.Fatalf("routes %#v", opts.Routes)
	}
	if opts.Timeout != 10*time.Minute || opts.MaxConcurrent != 8 || opts.Replay != 1000 ||
		opts.InputTimeout != time.Hour || opts.InputGrace != time.Minute {
		t.Fatalf("the defaults did not survive: %#v", opts)
	}

	c.Runs.Roots = []string{filepath.Join(dir, "nowhere")}
	if _, _, err := c.Serve(); err == nil || !strings.Contains(err.Error(), "is not an existing directory") {
		t.Fatalf("a root that is not there: %v", err)
	}
}

// TestSampleFileIsTheDefaults keeps docs/server.toml from rotting: it is the
// whole schema written out with the value rota would have used anyway, so a
// default that moves without the sample moving is a failing test.
func TestSampleFileIsTheDefaults(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "docs", "server.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// The sample is a document in a repository, so it is readable by
	// everybody; a server's own copy is not, which is what LoadConfig
	// insists on.
	path := write(t, string(raw))
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultConfig()
	got.From, want.From = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("docs/server.toml no longer says what the defaults are:\n%#v\nwant\n%#v", got, want)
	}
}

// TestPrintIsAFileThisCanRead closes the loop: what --print-config writes is
// something this parser reads back, token and comments included.
func TestPrintIsAFileThisCanRead(t *testing.T) {
	c := DefaultConfig()
	c.From["runs.timeout"] = "flag --timeout"
	c.Runs.Timeout = 90 * time.Second
	c.Runs.Roots = []string{`/a path/with "quotes"`}
	c.From["auth.token"] = "env ROTA_TOKEN"
	var b bytes.Buffer
	if err := c.Print(&b, "a-real-secret"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "a-real-secret") {
		t.Fatalf("the token was printed:\n%s", out)
	}
	for _, want := range []string{
		`token           = "(set)"            # env ROTA_TOKEN`,
		`timeout         = "90s"              # flag --timeout`,
		`replay          = 1000               # default`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output must have the line %q:\n%s", want, out)
		}
	}
	back := DefaultConfig()
	if err := toml.Unmarshal([]byte(out), back); err != nil {
		t.Fatalf("what it printed is not a file it reads: %v\n%s", err, out)
	}
	if back.Runs.Timeout != 90*time.Second || back.Auth.Token != "(set)" ||
		!reflect.DeepEqual(back.Runs.Roots, c.Runs.Roots) {
		t.Fatalf("read back as %#v", back.Runs)
	}
}

func TestPrintSaysNothingIsSetWhenNothingIs(t *testing.T) {
	var b bytes.Buffer
	if err := DefaultConfig().Print(&b, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `token           = ""                 # default`) {
		t.Fatalf("a server with no token:\n%s", b.String())
	}
}
