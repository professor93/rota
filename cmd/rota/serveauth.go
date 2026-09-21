package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	nethttp "net/http"
	"os"
	"strings"
	"time"

	"github.com/professor93/rota/api"
)

// rota never writes server.toml. These three commands are the reason that is
// bearable: two of them print the block to paste, and the third asks a
// running server for a link. A program that edits a file somebody else also
// edits has to merge, and a merge of a file that decides who may reach a
// server is a class of bug nobody needs.

const servePasswdUsage = `rota serve passwd <name> [--role control|watch]

Prints the [[users]] block for one person, with their password derived
rather than stored. Paste it into server.toml.

The password is asked for twice and never printed, logged or kept: what
goes into the file is a PBKDF2-SHA256 hash and the random salt it was
made with. With standard input redirected, one line is read instead, so
this works in a script — but a password on a pipe is a password in
whatever wrote the pipe.

Flags:
`

const serveTokenUsage = `rota serve token <name> [--role control|watch]

Prints one new bearer token and the [[tokens]] block that admits it.

The token is shown once and nowhere else: what the file holds is its
SHA-256, so a server.toml somebody reads gives them nothing to send.
Copy it now — a lost token is replaced, not recovered.

Flags:
`

const serveInviteUsage = `rota serve invite [--ttl 10m] [--config PATH]

Asks a running server for a single-use link that signs somebody in as a
watcher: they see everything and can change nothing.

This is a client, not a server: it reads the same configuration the
server does to find the address, the TLS settings and a control token,
and calls POST /v1/invites on the server already listening there.

Flags:
`

// role is the --role flag, which both printing commands take.
func roleFlag(fs *flag.FlagSet) *string {
	return fs.String("role", string(api.RoleWatch),
		"what this principal may do: control (everything, including running agents) or watch (read only)")
}

func checkRole(s string) error {
	if api.Role(s) != api.RoleControl && api.Role(s) != api.RoleWatch {
		return usageErr("--role is %q or %q, not %q", api.RoleControl, api.RoleWatch, s)
	}
	return nil
}

// servePasswd prints a [[users]] block for one name.
func (c *cli) servePasswd(args []string) error {
	fs := flag.NewFlagSet("serve passwd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	role := roleFlag(fs)
	rest, err := parseFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(c.out, servePasswdUsage)
			fs.SetOutput(c.out)
			fs.PrintDefaults()
			return nil
		}
		return usageErr("%v", err)
	}
	if len(rest) != 1 {
		return usageErr("serve passwd takes one name")
	}
	if err := checkRole(*role); err != nil {
		return err
	}
	name := rest[0]
	pw, err := c.askPassword()
	if err != nil {
		return err
	}
	hash, err := api.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "\n[[users]]\nname     = %q\nrole     = %q\npassword = %q\n", name, *role, hash)
	fmt.Fprintln(c.err, "\nrota: paste that into server.toml, which nobody but its owner may read (chmod 600).")
	return nil
}

// askPassword reads a password from the terminal twice without echoing it,
// or — when standard input is not a terminal — one line, having said that
// what is typed will be seen.
//
// The prompts go to stderr, so that redirecting stdout to a file collects
// the block and nothing else.
func (c *cli) askPassword() (string, error) {
	in := bufio.NewReader(c.in)
	line := func() (string, error) {
		s, err := in.ReadString('\n')
		if err != nil && (err != io.EOF || s == "") {
			return "", err
		}
		return strings.TrimRight(s, "\r\n"), nil
	}
	// The echo can only be turned off on the real terminal this process was
	// given. A test, and a script, hand it something else, and that is read
	// as a line — which is what a redirected standard input is anyway.
	tty, _ := c.in.(*os.File)
	if tty == nil || !isTerminal(tty) {
		fmt.Fprintln(c.err, "rota: standard input is not a terminal, so the password is read from it as typed and is echoed.")
		pw, err := line()
		if err != nil {
			return "", err
		}
		if pw == "" {
			return "", errors.New("a password with nothing in it is not a password")
		}
		return pw, nil
	}
	fmt.Fprint(c.err, "Password: ")
	first, err := noEcho(tty, line)
	fmt.Fprintln(c.err)
	if err != nil {
		return "", err
	}
	if first == "" {
		return "", errors.New("a password with nothing in it is not a password")
	}
	fmt.Fprint(c.err, "Again: ")
	again, err := noEcho(tty, line)
	fmt.Fprintln(c.err)
	if err != nil {
		return "", err
	}
	if first != again {
		return "", errors.New("those two passwords are not the same; nothing was printed")
	}
	return first, nil
}

// serveToken prints one new bearer token and the block that admits it.
func (c *cli) serveToken(args []string) error {
	fs := flag.NewFlagSet("serve token", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	role := roleFlag(fs)
	rest, err := parseFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(c.out, serveTokenUsage)
			fs.SetOutput(c.out)
			fs.PrintDefaults()
			return nil
		}
		return usageErr("%v", err)
	}
	if len(rest) != 1 {
		return usageErr("serve token takes one name")
	}
	if err := checkRole(*role); err != nil {
		return err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(tok))
	fmt.Fprintf(c.out, "%s\n", tok)
	fmt.Fprintf(c.out, "\n[[tokens]]\nname   = %q\nrole   = %q\nsha256 = %q\n",
		rest[0], *role, hex.EncodeToString(sum[:]))
	fmt.Fprintln(c.err, "\nrota: that token is shown once. The file holds only its hash, so it cannot be read back out.")
	return nil
}

// serveInvite asks a server that is already running for a watcher's link.
func (c *cli) serveInvite(args []string) error {
	fs := flag.NewFlagSet("serve invite", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var (
		ttl  = fs.Duration("ttl", 10*time.Minute, "how long the link works for (a day at most)")
		conf = fs.String("config", "", "the file the server was configured by (default: <store dir>/server.toml)")
	)
	rest, err := parseFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(c.out, serveInviteUsage)
			fs.SetOutput(c.out)
			fs.PrintDefaults()
			return nil
		}
		return usageErr("%v", err)
	}
	if len(rest) > 0 {
		return usageErr("serve invite takes no arguments, got %q", rest[0])
	}
	cfg, err := serverConfig(*conf)
	if err != nil {
		return err
	}
	tok, _, err := cfg.Token()
	if err != nil {
		return usageErr("%v", err)
	}
	if tok == "" {
		return usageErr("an invite is made by asking the running server, which needs the control token: " +
			"put it in ROTA_TOKEN, or in auth.token / auth.token_file in the same file the server reads")
	}
	base, err := serverURL(cfg)
	if err != nil {
		return err
	}
	body := fmt.Sprintf(`{"ttl":%q}`, ttl.String())
	req, err := nethttp.NewRequest("POST", base+"/v1/invites", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	client := &nethttp.Client{
		Timeout:   10 * time.Second,
		Transport: &nethttp.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("no server answered on %s: %w", base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		URL     string `json:"url"`
		Expires string `json:"expires"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != nethttp.StatusOK {
		if out.Error != "" {
			return fmt.Errorf("the server refused: %s", out.Error)
		}
		return fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	fmt.Fprintln(c.out, out.URL)
	fmt.Fprintf(c.err, "rota: one use, until %s. Whoever opens it can watch and change nothing.\n", out.Expires)
	return nil
}

// serverURL is where this configuration's server can be reached from here.
// An address on every interface is not an address to dial, so the loopback
// is used instead: this command runs on the same machine as the server it
// is asking.
func serverURL(cfg *api.Config) (string, error) {
	addr, err := api.ListenAddr(cfg.Server.Listen)
	if err != nil {
		return "", usageErr("server.listen: %v", err)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", usageErr("server.listen: %v", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if cfg.TLS.Cert != "" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}
