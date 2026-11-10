package api

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/pty"
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
session_ttl = "2h"
local_user = "inoyat"

[routes]
api = true
playground = false
websocket = false
health = false
terminal = false

[terminal]
shell = true
max_sessions = 2
scrollback_bytes = 4096
idle_timeout = "90m"
keep_stopped = "36h"
record = true
record_max_bytes = 1024
share = false

[ssh]
enabled = true
listen = "0.0.0.0:2200"
host_key = "/etc/rota/host"

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

[[users]]
name = "inoyat"
role = "control"
password = "pbkdf2-sha256$600000$c2FsdHNhbHRzYWx0c2FsdA==$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
ssh_keys = ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f inoyat@laptop"]

[[tokens]]
name = "ci-watch"
role = "watch"
sha256 = "0000000000000000000000000000000000000000000000000000000000000000"
`))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Server: ServerSection{Listen: "0.0.0.0:1234", Quiet: true},
		TLS:    TLSSection{Cert: "/c.pem", Key: "/k.pem"},
		Auth:   AuthSection{Token: "t", TokenEnv: "OTHER", SessionTTL: 2 * time.Hour, LocalUser: "inoyat"},
		Routes: RoutesSection{API: true},
		Runs: RunsSection{
			Timeout: 30 * time.Second, MaxConcurrent: 2, InputTimeout: 5 * time.Minute,
			InputGrace: -time.Second, Replay: 10, RefreshEvery: 0,
			Roots: []string{"/tmp"}, AllowDangerous: true, AllowRawFlags: true,
		},
		Terminal: TerminalSection{
			Shell: true, MaxSessions: 2, ScrollbackBytes: 4096,
			IdleTimeout: 90 * time.Minute, KeepStopped: 36 * time.Hour,
			Record: true, RecordMaxBytes: 1024, Share: false,
		},
		SSH:   SSHSection{Enabled: true, Listen: "0.0.0.0:2200", HostKey: "/etc/rota/host"},
		Store: StoreSection{Dir: "/srv/rota"},
		Users: []UserEntry{{Name: "inoyat", Role: "control",
			Password: "pbkdf2-sha256$600000$c2FsdHNhbHRzYWx0c2FsdA==$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			SSHKeys:  []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f inoyat@laptop"}}},
		Tokens: []TokenEntry{{Name: "ci-watch", Role: "watch",
			SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}},
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
		{"a relative log file", "[server]\nlog_file = \"logs/rota.log\"\n", "server.log_file must be an absolute path"},
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
	for _, want := range []string{"readable or writable by other users", "chmod 600 " + path} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%v\nmust say %q", err, want)
		}
	}
}

// TestLogFileFromTheFile: server.log_file is read like every other key, a
// relative one is refused wherever it came from, and the default is stderr,
// which is no file at all.
func TestLogFileFromTheFile(t *testing.T) {
	inFile := filepath.Join(t.TempDir(), "file.log")
	c, err := LoadConfig(write(t, "[server]\nlog_file = "+quote(inFile)+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.LogFile != inFile || c.From["server.log_file"] != "file" {
		t.Fatalf("from the file: %q %q", c.Server.LogFile, c.From["server.log_file"])
	}
	if DefaultConfig().Server.LogFile != "" {
		t.Fatal("the default must be stderr, which is no file at all")
	}
	// The command's flag lands in the same field, and Check catches it there.
	c.Server.LogFile = "rota.log"
	if err := c.Check(); err == nil || !strings.Contains(err.Error(), "server.log_file must be an absolute path") {
		t.Fatalf("a relative path from the command line: %v", err)
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

// A configuration asking for a terminal where there are none is refused at
// start-up, by name, rather than at the first terminal nobody can open.
func TestATerminalIsRefusedWherePseudoTerminalsAreNot(t *testing.T) {
	if pty.Supported {
		t.Skip("this platform has pseudo-terminals")
	}
	_, err := LoadConfig(write(t, "[routes]\nterminal = true\n"))
	if err == nil || !strings.Contains(err.Error(), "has no pseudo-terminal") ||
		!strings.Contains(err.Error(), runtime.GOOS) {
		t.Fatalf("got %v, want a refusal naming %s", err, runtime.GOOS)
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

// goodHash is one real derived password at the real cost, made on first use
// and reused: every test below that needs a valid entry needs the same valid
// entry. On first use rather than at start-up because this test binary is
// also the fake vendor CLI, re-executed for every run — six hundred thousand
// rounds in package initialization would be paid by each of those children.
var goodHash = sync.OnceValue(func() string {
	h, err := HashPassword("open sesame")
	if err != nil {
		panic(err)
	}
	return h
})

// shortSalt, cheap and stub are entries wrong in one way each, written out
// rather than built so that what the file says is what the test reads.
const (
	realSalt = "c2FsdHNhbHRzYWx0c2FsdA=="                     // 16 bytes
	realKey  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // 32 bytes
)

// TestUsersAndTokensAreCheckedByName: every refusal names the entry it is
// about and the field, because the person reading it is holding a file with
// several of these in it.
func TestUsersAndTokensAreCheckedByName(t *testing.T) {
	user := func(body string) string { return "[[users]]\n" + body }
	tok := func(body string) string { return "[[tokens]]\n" + body }
	sum := strings.Repeat("ab", 32)
	for _, c := range []struct{ name, body, want string }{
		{"a user with no name", user("role = \"watch\"\npassword = \"x\"\n"),
			`[[users]] #1: name is empty`},
		{"a user with no role", user("name = \"a\"\npassword = \"x\"\n"),
			`[[users]] "a": role: is empty; write "control" or "watch"`},
		{"a user with an invented role", user("name = \"a\"\nrole = \"admin\"\npassword = \"x\"\n"),
			`[[users]] "a": role: "admin" is not a role`},
		{"a user with no password", user("name = \"a\"\nrole = \"watch\"\npassword = \"\"\n"),
			"[[users]] \"a\": password is empty; make one with `rota serve passwd a`"},
		{"a password in the wrong shape", user("name = \"a\"\nrole = \"watch\"\npassword = \"hunter2\"\n"),
			`[[users]] "a": password: must be pbkdf2-sha256`},
		{"a password from another algorithm", user("name = \"a\"\nrole = \"watch\"\npassword = \"bcrypt$1$" + realSalt + "$" + realKey + "\"\n"),
			`"bcrypt" is not an algorithm rota knows`},
		{"a password derived too cheaply", user("name = \"a\"\nrole = \"watch\"\npassword = \"pbkdf2-sha256$1000$" + realSalt + "$" + realKey + "\"\n"),
			`1000 iterations is fewer than the 100000`},
		{"a password with a short salt", user("name = \"a\"\nrole = \"watch\"\npassword = \"pbkdf2-sha256$600000$c2FsdA==$" + realKey + "\"\n"),
			`the salt is 4 bytes; rota insists on at least 16`},
		{"a password with a short hash", user("name = \"a\"\nrole = \"watch\"\npassword = \"pbkdf2-sha256$600000$" + realSalt + "$aGFzaA==\"\n"),
			`the hash is 4 bytes; a pbkdf2-sha256 hash is 32`},
		{"two users with one name", user("name = \"a\"\nrole = \"watch\"\npassword = \""+goodHash()+"\"\n") +
			user("name = \"a\"\nrole = \"control\"\npassword = \""+goodHash()+"\"\n"),
			`[[users]] "a": there is already a user called "a"`},
		{"a token with no name", tok("role = \"watch\"\nsha256 = \"" + sum + "\"\n"),
			`[[tokens]] #1: name is empty`},
		{"a token with an invented role", tok("name = \"ci\"\nrole = \"root\"\nsha256 = \"" + sum + "\"\n"),
			`[[tokens]] "ci": role: "root" is not a role`},
		{"a token that is not a digest", tok("name = \"ci\"\nrole = \"watch\"\nsha256 = \"nope\"\n"),
			`[[tokens]] "ci": sha256: "nope" is not 64 hexadecimal characters`},
		{"a digest of the wrong length", tok("name = \"ci\"\nrole = \"watch\"\nsha256 = \"abcd\"\n"),
			`is not 64 hexadecimal characters`},
		{"two tokens with one name", tok("name = \"ci\"\nrole = \"watch\"\nsha256 = \""+sum+"\"\n") +
			tok("name = \"ci\"\nrole = \"control\"\nsha256 = \""+sum+"\"\n"),
			`[[tokens]] "ci": there is already a token called "ci"`},
		{"a session that lasts no time", "[auth]\nsession_ttl = \"0s\"\n",
			"auth.session_ttl must be longer than nothing"},
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

// TestPrintMasksEverySecret: the token, a user's password and a token's
// digest all come out as "(set)", and the output says at the top that it is
// a report rather than a file.
func TestPrintMasksEverySecret(t *testing.T) {
	c := DefaultConfig()
	c.Users = []UserEntry{{Name: "inoyat", Role: "control", Password: goodHash()}}
	c.Tokens = []TokenEntry{{Name: "ci-watch", Role: "watch", SHA256: strings.Repeat("ab", 32)}}
	var b bytes.Buffer
	if err := c.Print(&b, "a-real-secret"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, gone := range []string{"a-real-secret", goodHash(), strings.Repeat("ab", 32)} {
		if strings.Contains(out, gone) {
			t.Fatalf("a secret was printed:\n%s", out)
		}
	}
	for _, want := range []string{
		`# A secret shows as "(set)": this is a report of what rota would`,
		`token           = "(set)"`,
		`session_ttl     = "12h"`,
		`health          = true`,
		"[[users]]",
		`name            = "inoyat"`,
		`password        = "(set)"`,
		"[[tokens]]",
		`sha256          = "(set)"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output must have %q:\n%s", want, out)
		}
	}
}

// TestPrintIsStillAFileWhenNothingIsSecret pins the other half: a server
// with no secrets prints something it reads back, and says nothing about
// reports.
func TestPrintIsStillAFileWhenNothingIsSecret(t *testing.T) {
	var b bytes.Buffer
	if err := DefaultConfig().Print(&b, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "report") {
		t.Fatalf("nothing is set, so there is nothing to warn about:\n%s", b.String())
	}
	back := DefaultConfig()
	if err := toml.Unmarshal([]byte(b.String()), back); err != nil {
		t.Fatalf("what it printed is not a file it reads: %v\n%s", err, b.String())
	}
	if err := back.Check(); err != nil {
		t.Fatalf("what it printed is not a file it would serve with: %v", err)
	}
}

// TestKeepStoppedIsADuration: how long a stopped held session stays listed
// is a length of time like every other one here, written the way the parser
// reads it back, refused when it is nothing, and handed to the server as
// it was written.
func TestKeepStoppedIsADuration(t *testing.T) {
	var b bytes.Buffer
	if err := DefaultConfig().Print(&b, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "\nkeep_stopped    = \"168h\"") {
		t.Fatalf("the default is not printed as 168h:\n%s", b.String())
	}
	if _, err := LoadConfig(write(t, "[terminal]\nkeep_stopped = \"0s\"\n")); err == nil ||
		!strings.Contains(err.Error(), "terminal.keep_stopped must be longer than nothing") {
		t.Fatalf("0s answers %v", err)
	}
	c, err := LoadConfig(write(t, "[terminal]\nkeep_stopped = \"72h\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	opts, _, err := c.Serve()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Terminal.KeepStopped != 72*time.Hour {
		t.Fatalf("72h came through as %v", opts.Terminal.KeepStopped)
	}
	if c.From["terminal.keep_stopped"] != "file" {
		t.Fatalf("the file's own key is %q", c.From["terminal.keep_stopped"])
	}
}

// TestLocalUserMustBeAControlUser: auth.local_user says which person the
// local principal is, so it names somebody who may sign in and may control,
// or the file is refused. Named rightly, it reaches the server and is
// printed like every other key.
func TestLocalUserMustBeAControlUser(t *testing.T) {
	user := func(name, role string) string {
		return "[[users]]\nname = \"" + name + "\"\nrole = \"" + role + "\"\npassword = \"" + goodHash() + "\"\n"
	}
	for _, c := range []struct{ name, body, who string }{
		{"nobody by that name", "[auth]\nlocal_user = \"ghost\"\n" + user("me", "control"), "ghost"},
		{"a watcher", "[auth]\nlocal_user = \"viewer\"\n" + user("me", "control") + user("viewer", "watch"), "viewer"},
		{"a token's name", "[auth]\nlocal_user = \"ci\"\n[[tokens]]\nname = \"ci\"\nrole = \"control\"\nsha256 = \"" +
			strings.Repeat("ab", 32) + "\"\n", "ci"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(write(t, c.body))
			want := `auth.local_user names "` + c.who + `", who is not a [[users]] entry that may control`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%q answers %v, want %q", c.body, err, want)
			}
		})
	}
	c, err := LoadConfig(write(t, "[auth]\nlocal_user = \"you\"\n"+user("me", "control")+user("you", "control")))
	if err != nil {
		t.Fatal(err)
	}
	opts, _, err := c.Serve()
	if err != nil {
		t.Fatal(err)
	}
	if opts.LocalUser != "you" {
		t.Fatalf("local_user came through as %q", opts.LocalUser)
	}
	var b bytes.Buffer
	if err := DefaultConfig().Print(&b, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "\nlocal_user      = \"\"") {
		t.Fatalf("local_user is not printed:\n%s", b.String())
	}
}

// TestLocalIsTheSoleControlUser: the principal on the control socket is a
// person for the keyboard's sake — the one user who may control when there
// is exactly one, the one local_user names when there are more, and itself
// when neither says. A user is who signed in; a token or an invite is
// nobody.
func TestLocalIsTheSoleControlUser(t *testing.T) {
	server := func(local string, users ...User) *Server {
		t.Helper()
		s, err := New(Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1, Users: users, LocalUser: local})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Stop)
		return s
	}
	person := func(name string, role Role) User { return User{Name: name, Role: role, Password: cheapHash()} }
	local := &Principal{Name: "local", Role: RoleControl, Via: "local"}

	if got := server("", person("me", RoleControl), person("viewer", RoleWatch)).personOf(local); got != "me" {
		t.Errorf("with one control user, local is %q, want me", got)
	}
	two := server("", person("me", RoleControl), person("you", RoleControl))
	if got := two.personOf(local); got != "local" {
		t.Errorf("with two control users and no local_user, local is %q, want local", got)
	}
	if got := server("you", person("me", RoleControl), person("you", RoleControl)).personOf(local); got != "you" {
		t.Errorf("with local_user = you, local is %q, want you", got)
	}
	if got := server("").personOf(local); got != "local" {
		t.Errorf("with nobody to sign in, local is %q, want local", got)
	}
	for _, c := range []struct {
		p    *Principal
		want string
	}{
		{&Principal{Name: "me", Role: RoleControl, Via: "user"}, "me"},
		{&Principal{Name: "viewer", Role: RoleWatch, Via: "user"}, "viewer"},
		{&Principal{Name: "token", Role: RoleControl, Via: "token"}, ""},
		{&Principal{Name: "invite-abcdef", Role: RoleWatch, Via: "invite"}, ""},
		{nil, ""},
	} {
		if got := two.personOf(c.p); got != c.want {
			t.Errorf("%+v is the person %q, want %q", c.p, got, c.want)
		}
	}
}
