package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

// This file is the two logins that are a paste rather than a browser: an API
// key that makes an account of its own (`rota login --key`), and a token from
// `claude setup-token` that becomes an existing account's long-lived one
// (`rota login <account-id> --token`).
//
// Neither credential is ever taken from the command line. Arguments sit in the
// process table, where every process on the machine can read them — the agents
// rota starts among them — and in a shell's history for good. Both are read
// from the terminal with its echo off, or as one line of standard input when
// that is not a terminal, and nothing printed afterwards carries them: an API
// key is shown, where it is shown at all, by its last four characters.

// pasted is what takePasted lifted out of the arguments.
type pasted struct {
	key, token bool
	label      string
	labelled   bool
}

// keyInArgv is why a credential written on the command line is refused, in
// words that never repeat it.
const keyInArgv = "%s takes nothing on the command line: it is read from the terminal, not shown as it is typed, " +
	"or as one line of standard input — never from an argument, which every process can read and a shell keeps"

// takePasted lifts --key, --token and --label out of login's arguments,
// before anything else reads them: --label's value could be a provider's name.
func takePasted(args []string) (rest []string, p pasted, err error) {
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		switch name {
		case "--key", "--token":
			if hasValue {
				return nil, p, usageErr(keyInArgv, name)
			}
			p.key, p.token = p.key || name == "--key", p.token || name == "--token"
		case "--label":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, p, usageErr("--label needs a name after it")
				}
				i++
				value = args[i]
			}
			p.label, p.labelled = value, true
		default:
			rest = append(rest, args[i])
		}
	}
	if p.key && p.token {
		return nil, p, usageErr("--key and --token are two different credentials; one at a time")
	}
	return rest, p, nil
}

// loginKey is `rota login [provider] --key [--label name]`.
func (c *cli) loginKey(provider string, long bool, rest []string, label string) error {
	switch {
	case long:
		return usageErr("--key and --long are two different logins: a key is an account of its own")
	case len(rest) > 0:
		return usageErr(keyInArgv, "--key")
	}
	if provider == "" {
		provider = rota.DefaultProvider
	}
	key, err := c.askSecret("API key")
	if err != nil {
		return err
	}
	s, err := c.openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	a, added, unverified, err := s.LoginKey(context.Background(), provider, key, label)
	if err != nil {
		return err
	}
	status, verb := "updated", "Updated"
	if added {
		status, verb = "added", "Added"
	}
	if c.json {
		doc := map[string]any{"id": a.ID, "provider": a.Provider, "uuid": a.UUID, "label": a.Label(),
			"status": status, "credential": a.Credential, "order": a.Order}
		if unverified != nil {
			doc["warning"] = unverified.Error()
		}
		return c.emit(doc)
	}
	if unverified != nil {
		fmt.Fprintf(c.err, "warning: %v\n", unverified)
	}
	fmt.Fprintf(c.out, "%s %s account %d (%s): it runs on an API key, and every run on it is paid for from that key's credit.\n",
		verb, a.Provider, a.ID, a.Label())
	if a.Order == 0 {
		fmt.Fprintf(c.out, "It is out of the rotation, so it runs only by id (`rota %d \"...\"`); `rota set %d --order last` puts it in.\n",
			a.ID, a.ID)
	}
	if added {
		fmt.Fprintln(c.out, "Claude Code asks once, the first time it opens on this account, whether to use the key: say yes.")
	}
	return nil
}

// loginToken is `rota login <account-id> --token`.
func (c *cli) loginToken(provider string, long bool, rest []string) error {
	// The one argument is an account id. Anything else is refused without
	// being repeated: it may be the token itself, typed where it should not be.
	if provider != "" || long || len(rest) != 1 || !isNumber(rest[0]) {
		return usageErr("usage: rota login <account-id> --token — the token from `claude setup-token` is read " +
			"from the terminal or standard input, and becomes that account's long-lived token")
	}
	id, err := parseID(rest[0])
	if err != nil {
		return err
	}
	tok, err := c.askSecret("token from `claude setup-token`")
	if err != nil {
		return err
	}
	s, err := c.openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	a, err := s.PasteLong(context.Background(), id, tok)
	if err != nil {
		if errors.Is(err, rota.ErrNoAccount) {
			return fmt.Errorf("%w; see `rota list`", err)
		}
		return err
	}
	if c.json {
		return c.emit(map[string]any{"id": a.ID, "provider": a.Provider, "email": a.Email,
			"uuid": a.UUID, "status": "long", "long_until": a.LongUntil().UTC().Format(time.RFC3339),
			"note": store.PastedLongCaveat})
	}
	fmt.Fprintf(c.out, "long-lived token stored for #%d %s, good until %s; a run without the account's home, or on a dead login, uses it from now on.\n",
		a.ID, a, a.LongUntil().Format(time.DateOnly))
	fmt.Fprintln(c.out, store.PastedLongCaveat)
	return nil
}

// maxSecret bounds what is read for one pasted credential: a key is about a
// hundred characters, and nothing worth reading is a hundred times that.
const maxSecret = 8 << 10

// askSecret reads one pasted credential: from the terminal with its echo off,
// or one line of standard input when that is not a terminal. The prompt goes
// to stderr, so that what a command prints on stdout stays its answer.
//
// A terminal whose echo rota cannot turn off — a Windows console — is not read
// from at all: the credential would be on the screen. Standard input from a
// file or a pipe is read on every platform.
func (c *cli) askSecret(what string) (string, error) {
	in := bufio.NewReader(io.LimitReader(c.in, maxSecret))
	line := func() (string, error) {
		s, err := in.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return s, nil
	}
	tty, _ := c.in.(*os.File)
	var (
		s   string
		err error
	)
	switch {
	case tty != nil && isTerminal(tty):
		fmt.Fprintf(c.err, "Paste the %s (it is not shown): ", what)
		s, err = noEcho(tty, line)
		fmt.Fprintln(c.err)
	case tty != nil && !canHideInput && isCharDevice(tty):
		return "", fmt.Errorf("this terminal cannot hide what is typed, so rota does not read the %s from it; "+
			"put it in a file only you can read and pass that as standard input", what)
	default:
		s, err = line()
	}
	if err != nil {
		return "", fmt.Errorf("reading the %s: %w", what, err)
	}
	return strings.TrimSpace(s), nil
}

// looksLikeKey reports whether an argument may be a credential typed where an
// argument was expected — `rota login sk-ant-…` for `rota login --key`, say. A
// message about such an argument must not repeat it: whatever it printed would
// be in a terminal's scrollback and in any log that keeps stderr.
//
// An argument a message is about to quote is refused this way when it is
// longer than any word a command expects, as well as when it has a
// credential's shape.
func looksLikeKey(s string) bool { return wire.HoldsSecret(s) || len(s) > 40 }

// keyInArgs is the refusal of an argument that looks like a key, in words that
// do not repeat it, or nil when none of args does.
func keyInArgs(args ...string) error {
	for _, a := range args {
		if looksLikeKey(a) {
			return errKeyArg
		}
	}
	return nil
}

// keyInValues is keyInArgs for values a command keeps rather than quotes — a
// directory, which may well be longer than forty characters — and so only for
// those that have a credential's shape.
func keyInValues(values ...string) error {
	for _, v := range values {
		if wire.HoldsSecret(v) {
			return errKeyArg
		}
	}
	return nil
}

// errKeyArg is why an argument that looks like a key is refused.
var errKeyArg = usageErr("that argument looks like a key, so it is not repeated here; an API key is pasted with " +
	"`rota login --key`, which reads it hidden — one typed on a command line is in the shell's history now")

// keyInFlagErr is a flag parser's error with any argument that looks like a
// key taken out: the flag package quotes the value it could not use.
func keyInFlagErr(err error, args []string) error {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	for _, a := range args {
		if looksLikeKey(a) && strings.Contains(err.Error(), a) {
			return keyInArgs(a)
		}
	}
	return err
}

// unsecret is a message with anything shaped like an Anthropic credential
// taken out. Every error this command prints passes through it, as the last
// line of defence for a message somebody wrote without thinking of keys.
func unsecret(msg string) string { return wire.Unsecret(msg) }

// isCharDevice reports whether f is a terminal or some other character
// device rather than a file or a pipe.
func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
