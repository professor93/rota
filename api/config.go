package api

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/professor93/rota/internal/toml"
)

// ConfigName is what the file is called when nobody names another: it sits
// in the store directory, beside the accounts it serves.
const ConfigName = "server.toml"

// Config is the whole of server.toml — one file for running the server and
// for the routes it answers, so that starting one is reading a file rather
// than remembering a command line.
//
// Every section is its own struct because the file grows by sections: users
// with roles, a login page and a terminal each arrive as one more table here
// and one more field below, and nothing already written has to move.
//
// rota has no database. The store directory is what persists, and [store]
// is where it is said.
type Config struct {
	Server ServerSection `toml:"server"`
	TLS    TLSSection    `toml:"tls"`
	Auth   AuthSection   `toml:"auth"`
	Routes RoutesSection `toml:"routes"`
	Runs   RunsSection   `toml:"runs"`
	Store  StoreSection  `toml:"store"`

	// From says where each value came from, by dotted key: "runs.timeout" ->
	// "file", "auth.token" -> "env ROTA_TOKEN". Nothing reads it to decide
	// anything — it is what --print-config prints beside each line — and a
	// key it has no entry for still has its default. LoadConfig fills in
	// what the file won; the command fills in what the command line and the
	// environment won, because only the command knows what was typed.
	From map[string]string `toml:"-"`
}

// ServerSection is the address side of the server.
type ServerSection struct {
	Listen string `toml:"listen"`
	Quiet  bool   `toml:"quiet"`
}

// TLSSection is the certificate pair, or neither.
type TLSSection struct {
	Cert string `toml:"cert"`
	Key  string `toml:"key"`
}

// AuthSection is where the one bearer token comes from. The three are
// alternatives, not layers: a token written here, a file holding one, or the
// name of an environment variable that does.
type AuthSection struct {
	Token     string `toml:"token"`
	TokenFile string `toml:"token_file"`
	TokenEnv  string `toml:"token_env"`
}

// RoutesSection switches whole groups of routes on and off. A group that is
// off is not registered at all, so its paths answer 404 like any other path
// this server has never heard of.
type RoutesSection struct {
	API        bool `toml:"api"`
	Playground bool `toml:"playground"`
	WebSocket  bool `toml:"websocket"`
}

// RunsSection is everything about the CLIs this server starts.
type RunsSection struct {
	Timeout        time.Duration `toml:"timeout"`
	MaxConcurrent  int           `toml:"max_concurrent"`
	InputTimeout   time.Duration `toml:"input_timeout"`
	InputGrace     time.Duration `toml:"input_grace"`
	Replay         int           `toml:"replay"`
	RefreshEvery   time.Duration `toml:"refresh_every"`
	Roots          []string      `toml:"roots"`
	AllowDangerous bool          `toml:"allow_dangerous"`
	AllowRawFlags  bool          `toml:"allow_raw_flags"`
}

// StoreSection says where rota keeps everything it has. Empty is $ROTA_HOME
// or ~/.rota, which is where every other rota command looks.
type StoreSection struct {
	Dir string `toml:"dir"`
}

// DefaultConfig is what a server is without a file: exactly what rota serve
// did before this file existed. Every default here is the one the code
// already applies, so a file that says nothing changes nothing.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerSection{Listen: "127.0.0.1:8787"},
		Auth:   AuthSection{TokenEnv: "ROTA_TOKEN"},
		Routes: RoutesSection{API: true, Playground: true, WebSocket: true},
		Runs: RunsSection{
			Timeout: 10 * time.Minute, MaxConcurrent: 8,
			InputTimeout: time.Hour, InputGrace: time.Minute,
			Replay: 1000, RefreshEvery: defaultRefreshEvery,
			Roots: []string{},
		},
		From: map[string]string{},
	}
}

// LoadConfig reads one server.toml. Whatever the file leaves out keeps its
// default, and every key it sets is remembered as the file's, so
// --print-config can say so.
//
// The file is refused if anyone but its owner can read it: it may hold the
// bearer token, and a token that authorizes running every account on the
// machine is not a thing to leave world-readable. Windows has neither those
// bits nor that convention, so the rule is unix's.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := ownerOnly(path, fi.Mode()); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	c := DefaultConfig()
	keys, err := toml.UnmarshalKeys(data, c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, k := range keys {
		c.From[k] = "file"
	}
	if err := c.Check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// ownerOnly refuses a file group or others can read, and says the one
// command that fixes it.
func ownerOnly(path string, mode fs.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := mode.Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is readable by other users (mode %04o); it may hold a token, so fix it with: chmod 600 %s",
			path, perm, path)
	}
	return nil
}

// Check says what is wrong with a configuration, in the file's own words. It
// is what LoadConfig applies to a file, and what Serve applies again once
// the command line has had its say, so a contradiction made by mixing the
// two is caught as well.
func (c *Config) Check() error {
	if c.Auth.Token != "" && c.Auth.TokenFile != "" {
		return errors.New("auth.token and auth.token_file are two ways to say the same thing; keep one")
	}
	if (c.TLS.Cert == "") != (c.TLS.Key == "") {
		return errors.New("tls.cert and tls.key go together")
	}
	if !c.Routes.API {
		// The page and the sockets are the API in another shape: the one
		// calls it, the other is it. Neither can stand while it is off.
		if c.Routes.Playground {
			return errors.New("routes.playground needs routes.api: the page has nothing to call without it")
		}
		if c.Routes.WebSocket {
			return errors.New("routes.websocket needs routes.api: a socket is the same run by another door")
		}
	}
	for _, d := range []struct {
		key string
		val time.Duration
	}{{"runs.timeout", c.Runs.Timeout}, {"runs.input_timeout", c.Runs.InputTimeout}} {
		if d.val <= 0 {
			return fmt.Errorf("%s must be longer than nothing", d.key)
		}
	}
	for _, n := range []struct {
		key string
		val int
	}{{"runs.max_concurrent", c.Runs.MaxConcurrent}, {"runs.replay", c.Runs.Replay}} {
		if n.val <= 0 {
			return fmt.Errorf("%s must be more than nothing", n.key)
		}
	}
	if _, err := ListenAddr(c.Server.Listen); err != nil {
		return fmt.Errorf("server.listen: %w", err)
	}
	return nil
}

// Token is the bearer token this configuration describes, and where it came
// from. The environment beats the file, as everywhere else here: the
// variable auth.token_env names, then auth.token, then the file
// auth.token_file points at. The command line beats all three, and the
// command applies that itself.
//
// An empty token is not an error here. Refusing to serve without one is the
// command's to say, in the words it has always said it in.
func (c *Config) Token() (token, from string, err error) {
	if name := c.Auth.TokenEnv; name != "" {
		if v := os.Getenv(name); v != "" {
			return v, "env " + name, nil
		}
	}
	if c.Auth.Token != "" {
		return c.Auth.Token, "file", nil
	}
	if path := c.Auth.TokenFile; path != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", "", fmt.Errorf("auth.token_file: %w", err)
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return "", "", fmt.Errorf("auth.token_file: %w", err)
		}
		if err := ownerOnly(path, fi.Mode()); err != nil {
			return "", "", fmt.Errorf("auth.token_file: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(f, 4096))
		if err != nil {
			return "", "", fmt.Errorf("auth.token_file: %w", err)
		}
		tok := strings.TrimSpace(string(raw))
		if tok == "" {
			return "", "", fmt.Errorf("auth.token_file: %s holds no token", path)
		}
		return tok, "file", nil
	}
	return "", "", nil
}

// Listener is the address side of a running server: where it listens, and
// the certificate pair it answers with, if it has one.
type Listener struct {
	Addr string
	Cert string
	Key  string
}

// Serve turns a configuration into the two things needed to start a server:
// the options New takes, and the listener around it.
//
// It fills everything but the token and the log. Those two belong to the
// command: only it knows what was typed on the command line, and only it
// knows where a server's own output goes.
func (c *Config) Serve() (Options, Listener, error) {
	if err := c.Check(); err != nil {
		return Options{}, Listener{}, err
	}
	addr, err := ListenAddr(c.Server.Listen)
	if err != nil {
		return Options{}, Listener{}, fmt.Errorf("server.listen: %w", err)
	}
	roots := make([]string, 0, len(c.Runs.Roots))
	for _, r := range c.Runs.Roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return Options{}, Listener{}, fmt.Errorf("root %q: %v", r, err)
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return Options{}, Listener{}, fmt.Errorf("root %q is not an existing directory", r)
		}
		roots = append(roots, abs)
	}
	// A refresh somebody turned off is zero to them and "the default" to the
	// option, so say off explicitly.
	refresh := c.Runs.RefreshEvery
	if refresh <= 0 {
		refresh = -1
	}
	routes := Routes{API: c.Routes.API, Playground: c.Routes.Playground, WebSocket: c.Routes.WebSocket}
	return Options{
		Dir:            c.Store.Dir,
		Roots:          roots,
		AllowDangerous: c.Runs.AllowDangerous,
		AllowRawFlags:  c.Runs.AllowRawFlags,
		Timeout:        c.Runs.Timeout,
		MaxConcurrent:  c.Runs.MaxConcurrent,
		InputTimeout:   c.Runs.InputTimeout,
		InputGrace:     c.Runs.InputGrace,
		Replay:         c.Runs.Replay,
		RefreshEvery:   refresh,
		Routes:         &routes,
	}, Listener{Addr: addr, Cert: c.TLS.Cert, Key: c.TLS.Key}, nil
}

// ListenAddr turns what a person typed into an address net.Listen accepts. A
// bare port means every interface, because someone who writes "8787" rather
// than "127.0.0.1:8787" is asking to be reachable from elsewhere.
//
// It is here rather than in the command because [server] listen takes the
// same spellings as the address on the command line, and one rule read two
// ways is two rules.
func ListenAddr(in string) (string, error) {
	in = strings.TrimSpace(in)
	switch {
	case in == "":
		return "127.0.0.1:8787", nil
	case !strings.Contains(in, ":"):
		port, err := strconv.Atoi(in)
		if err != nil || port < 1 || port > 65535 {
			return "", fmt.Errorf("%q is not a port or a host:port", in)
		}
		return "0.0.0.0:" + in, nil
	}
	if _, _, err := net.SplitHostPort(in); err != nil {
		return "", fmt.Errorf("%q is not a host:port: %w", in, err)
	}
	return in, nil
}

// Print writes the configuration as TOML: every key of the schema, in the
// order the file has them, each with a comment saying where its value came
// from.
//
// The token is never written down, only whether there is one, so the output
// can be pasted where the file itself could not. What comes out is a file
// this parser reads back.
func (c *Config) Print(w io.Writer, token string) error {
	var b strings.Builder
	key := func(name string, rendered string) {
		short := name[strings.IndexByte(name, '.')+1:]
		fmt.Fprintf(&b, "%-15s = %-18s # %s\n", short, rendered, c.origin(name))
	}
	str := func(name, v string) { key(name, quote(v)) }
	yes := func(name string, v bool) { key(name, strconv.FormatBool(v)) }
	num := func(name string, v int) { key(name, strconv.Itoa(v)) }
	span := func(name string, v time.Duration) { key(name, quote(duration(v))) }

	b.WriteString("[server]\n")
	str("server.listen", c.Server.Listen)
	yes("server.quiet", c.Server.Quiet)

	b.WriteString("\n[tls]\n")
	str("tls.cert", c.TLS.Cert)
	str("tls.key", c.TLS.Key)

	b.WriteString("\n[auth]\n")
	// The token is a secret whatever it came from, so only its existence is
	// printed. "(set)" is a string like any other, which keeps this file
	// readable by the parser that wrote it.
	masked := ""
	if token != "" {
		masked = "(set)"
	}
	str("auth.token", masked)
	str("auth.token_file", c.Auth.TokenFile)
	str("auth.token_env", c.Auth.TokenEnv)

	b.WriteString("\n[routes]\n")
	yes("routes.api", c.Routes.API)
	yes("routes.playground", c.Routes.Playground)
	yes("routes.websocket", c.Routes.WebSocket)

	b.WriteString("\n[runs]\n")
	span("runs.timeout", c.Runs.Timeout)
	num("runs.max_concurrent", c.Runs.MaxConcurrent)
	span("runs.input_timeout", c.Runs.InputTimeout)
	span("runs.input_grace", c.Runs.InputGrace)
	num("runs.replay", c.Runs.Replay)
	span("runs.refresh_every", c.Runs.RefreshEvery)
	quoted := make([]string, 0, len(c.Runs.Roots))
	for _, r := range c.Runs.Roots {
		quoted = append(quoted, quote(r))
	}
	key("runs.roots", "["+strings.Join(quoted, ", ")+"]")
	yes("runs.allow_dangerous", c.Runs.AllowDangerous)
	yes("runs.allow_raw_flags", c.Runs.AllowRawFlags)

	b.WriteString("\n[store]\n")
	str("store.dir", c.Store.Dir)

	_, err := io.WriteString(w, b.String())
	return err
}

// origin is where one key's value came from, with "default" for the keys
// nobody has spoken for.
func (c *Config) origin(key string) string {
	if from := c.From[key]; from != "" {
		return from
	}
	return "default"
}

// quote writes a string the way this parser reads one back, with the five
// escapes it knows and \u for anything else that cannot be written plain.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// duration writes a length of time the short way, so what was written "10m"
// comes back as "10m" rather than "10m0s".
func duration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	case d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}
