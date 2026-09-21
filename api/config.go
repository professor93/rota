package api

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	// Users and Tokens are the principals besides the one bearer token: the
	// people who sign in on the page, and the tokens a script carries. They
	// are arrays of tables rather than more keys under [auth] because each
	// one is a thing with a name and a role, and [auth] is about one
	// credential this server has always had.
	Users  []UserEntry  `toml:"users"`
	Tokens []TokenEntry `toml:"tokens"`

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
	Token     string `toml:"token,secret"`
	TokenFile string `toml:"token_file"`
	TokenEnv  string `toml:"token_env"`
	// SessionTTL is how long a sign-in on the page lasts. It belongs here
	// rather than beside the users because it is the same for all of them:
	// a per-user lifetime would be one more thing to get wrong in a file
	// somebody edits by hand.
	SessionTTL time.Duration `toml:"session_ttl"`
}

// UserEntry is one person who may sign in on the page: a name, a role, and
// a password nobody can read back out of the file.
//
// The password is stored derived — see passwd.go for the shape — and rota
// never writes this file: `rota serve passwd <name>` prints the block to
// paste, which keeps the only copy of the plain password in the head of
// whoever typed it.
type UserEntry struct {
	Name     string `toml:"name"`
	Role     string `toml:"role"`
	Password string `toml:"password,secret"`
}

// TokenEntry is one more bearer token, with a role of its own: a CI job that
// may look and not touch is this, rather than a second server.
//
// Only the SHA-256 of the token is written down, so a file somebody reads
// gives them nothing to send. `rota serve token <name>` prints the token
// once and the block that holds its hash.
type TokenEntry struct {
	Name   string `toml:"name"`
	Role   string `toml:"role"`
	SHA256 string `toml:"sha256,secret"`
}

// RoutesSection switches whole groups of routes on and off. A group that is
// off is not registered at all, so its paths answer 404 like any other path
// this server has never heard of.
type RoutesSection struct {
	API        bool `toml:"api"`
	Playground bool `toml:"playground"`
	WebSocket  bool `toml:"websocket"`
	Health     bool `toml:"health"`
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
		Auth:   AuthSection{TokenEnv: "ROTA_TOKEN", SessionTTL: defaultSessionTTL},
		Routes: RoutesSection{API: true, Playground: true, WebSocket: true, Health: true},
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
	// What each group of routes needs from another, as a table rather than a
	// chain of ifs: a group added later is a row, and the reason it depends
	// on what it depends on is written beside it. health needs nothing,
	// which is the whole point of it — a probe that goes off with the page
	// is not a probe.
	for _, dep := range []struct {
		group, needs, why string
		on, has           bool
	}{
		{"playground", "api", "the page has nothing to call without it", c.Routes.Playground, c.Routes.API},
		{"websocket", "api", "a socket is the same run by another door", c.Routes.WebSocket, c.Routes.API},
	} {
		if dep.on && !dep.has {
			return fmt.Errorf("routes.%s needs routes.%s: %s", dep.group, dep.needs, dep.why)
		}
	}
	if err := c.checkPrincipals(); err != nil {
		return err
	}
	if c.Auth.SessionTTL <= 0 {
		return errors.New("auth.session_ttl must be longer than nothing")
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

// checkPrincipals says what is wrong with the people and the tokens this
// file names. Every message names the entry it is about — by its name where
// it has one, by its position where it does not — and the field, because
// the person reading it is looking at a file with several of these in it.
func (c *Config) checkPrincipals() error {
	seen := map[string]bool{}
	for i, u := range c.Users {
		at := entryName("users", i, u.Name)
		if u.Name == "" {
			return fmt.Errorf("%s: name is empty; every user needs one to be logged as", at)
		}
		if seen[u.Name] {
			return fmt.Errorf("%s: there is already a user called %q", at, u.Name)
		}
		seen[u.Name] = true
		if _, err := parseRole(u.Role); err != nil {
			return fmt.Errorf("%s: role: %w", at, err)
		}
		if u.Password == "" {
			return fmt.Errorf("%s: password is empty; make one with `rota serve passwd %s`", at, u.Name)
		}
		if _, err := parsePassword(u.Password); err != nil {
			return fmt.Errorf("%s: password: %w", at, err)
		}
	}
	seen = map[string]bool{}
	for i, t := range c.Tokens {
		at := entryName("tokens", i, t.Name)
		if t.Name == "" {
			return fmt.Errorf("%s: name is empty; every token needs one to be logged as", at)
		}
		if seen[t.Name] {
			return fmt.Errorf("%s: there is already a token called %q", at, t.Name)
		}
		seen[t.Name] = true
		if _, err := parseRole(t.Role); err != nil {
			return fmt.Errorf("%s: role: %w", at, err)
		}
		if _, err := parseSum(t.SHA256); err != nil {
			return fmt.Errorf("%s: sha256: %w", at, err)
		}
	}
	return nil
}

// entryName is how one entry of an array of tables is pointed at in a
// message: by its name if it has one, by where it is if it does not.
func entryName(table string, i int, name string) string {
	if name == "" {
		return fmt.Sprintf("[[%s]] #%d", table, i+1)
	}
	return fmt.Sprintf("[[%s]] %q", table, name)
}

// parseRole reads the one word a role is written as.
func parseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleWatch, RoleControl:
		return Role(s), nil
	case "":
		return "", fmt.Errorf("is empty; write %q or %q", RoleControl, RoleWatch)
	}
	return "", fmt.Errorf("%q is not a role; write %q or %q", s, RoleControl, RoleWatch)
}

// parseSum reads the hex SHA-256 a token entry is written as.
func parseSum(s string) ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != len(out) {
		return out, fmt.Errorf("%q is not 64 hexadecimal characters; make one with `rota serve token <name>`", s)
	}
	copy(out[:], raw)
	return out, nil
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
	routes := Routes{API: c.Routes.API, Playground: c.Routes.Playground,
		WebSocket: c.Routes.WebSocket, Health: c.Routes.Health}
	users := make([]User, 0, len(c.Users))
	for _, u := range c.Users {
		users = append(users, User{Name: u.Name, Role: Role(u.Role), Password: u.Password})
	}
	tokens := make([]TokenPrincipal, 0, len(c.Tokens))
	for _, t := range c.Tokens {
		tokens = append(tokens, TokenPrincipal{Name: t.Name, Role: Role(t.Role), SHA256: t.SHA256})
	}
	return Options{
		Dir:            c.Store.Dir,
		Users:          users,
		Tokens:         tokens,
		SessionTTL:     c.Auth.SessionTTL,
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
// No secret is written down, only whether there is one — every field tagged
// `secret` in the schema above comes out as "(set)" — so the output can be
// pasted where the file itself could not. A server with no secrets set is a
// file this parser reads straight back; one with them is a report, and says
// so at the top, because "(set)" is not a password.
func (c *Config) Print(w io.Writer, token string) error {
	var b, body strings.Builder
	masked := false
	key := func(name string, rendered string) {
		short := name[strings.LastIndexByte(name, '.')+1:]
		fmt.Fprintf(&body, "%-15s = %-18s # %s\n", short, rendered, c.origin(name))
	}
	str := func(name, v string) {
		if v != "" && isSecret(name) {
			v, masked = "(set)", true
		}
		key(name, quote(v))
	}
	yes := func(name string, v bool) { key(name, strconv.FormatBool(v)) }
	num := func(name string, v int) { key(name, strconv.Itoa(v)) }
	span := func(name string, v time.Duration) { key(name, quote(duration(v))) }

	body.WriteString("[server]\n")
	str("server.listen", c.Server.Listen)
	yes("server.quiet", c.Server.Quiet)

	body.WriteString("\n[tls]\n")
	str("tls.cert", c.TLS.Cert)
	str("tls.key", c.TLS.Key)

	body.WriteString("\n[auth]\n")
	// The token printed is the one this server would actually use, whatever
	// it came from — and it comes out masked, like every other secret.
	str("auth.token", token)
	str("auth.token_file", c.Auth.TokenFile)
	str("auth.token_env", c.Auth.TokenEnv)
	span("auth.session_ttl", c.Auth.SessionTTL)

	body.WriteString("\n[routes]\n")
	yes("routes.api", c.Routes.API)
	yes("routes.playground", c.Routes.Playground)
	yes("routes.websocket", c.Routes.WebSocket)
	yes("routes.health", c.Routes.Health)

	body.WriteString("\n[runs]\n")
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

	body.WriteString("\n[store]\n")
	str("store.dir", c.Store.Dir)

	for i, u := range c.Users {
		body.WriteString("\n[[users]]\n")
		str(fmt.Sprintf("users.%d.name", i), u.Name)
		str(fmt.Sprintf("users.%d.role", i), u.Role)
		str(fmt.Sprintf("users.%d.password", i), u.Password)
	}
	for i, t := range c.Tokens {
		body.WriteString("\n[[tokens]]\n")
		str(fmt.Sprintf("tokens.%d.name", i), t.Name)
		str(fmt.Sprintf("tokens.%d.role", i), t.Role)
		str(fmt.Sprintf("tokens.%d.sha256", i), t.SHA256)
	}

	if masked {
		b.WriteString("# A secret shows as \"(set)\": this is a report of what rota would\n" +
			"# serve with, not a file to load back.\n\n")
	}
	b.WriteString(body.String())
	_, err := io.WriteString(w, b.String())
	return err
}

// isSecret reports whether the value at one dotted key is a secret, by
// looking the key up in the schema above and asking the tag. The keys of an
// array of tables carry their index — "users.0.password" — which is dropped
// here: which entry it is does not change what the field is.
func isSecret(key string) bool { return secretKeys()[strings.Join(unindex(key), ".")] }

func unindex(key string) []string {
	parts := strings.Split(key, ".")
	out := parts[:0]
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err == nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// secretKeys walks the schema once and collects the dotted key of every
// field tagged secret. It is derived rather than listed so that a field
// added to one of the structs above is masked by saying so where it is
// declared, and not by remembering to edit a second list down here.
var secretKeys = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		for i := range t.NumField() {
			f := t.Field(i)
			name := toml.Name(f)
			if name == "" {
				continue
			}
			at := name
			if path != "" {
				at = path + "." + name
			}
			if toml.Option(f, "secret") {
				out[at] = true
			}
			ft := f.Type
			if ft.Kind() == reflect.Slice {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && ft != durationStruct {
				walk(ft, at)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return out
})

// durationStruct is what the walk above must not descend into: a
// time.Duration is an integer, but a time.Time — if one ever appears here —
// is a struct with unexported fields and nothing to say about secrets.
var durationStruct = reflect.TypeOf(time.Time{})

// origin is where one key's value came from, with "default" for the keys
// nobody has spoken for.
func (c *Config) origin(key string) string {
	// An entry of an array of tables is recorded once for the table, not
	// once per row — the file said "users.name", whichever user it was — so
	// the index this printer carries is dropped before looking it up.
	if from := c.From[strings.Join(unindex(key), ".")]; from != "" {
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
