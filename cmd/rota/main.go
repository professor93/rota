// Command rota runs several AI coding CLIs across several accounts.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	nethttp "net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/professor93/rota/api"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/rotation"
	"github.com/professor93/rota/sessions"
	"github.com/professor93/rota/store"
	"github.com/professor93/rota/wire"
)

const usage = `rota — run several AI coding CLIs across several accounts

Usage:
  rota "<prompt>"               ask, with no verb at all — the usual thing
  rota <id> "<prompt>"          ask that account instead

  rota login [provider]         start a new account: prints a login id and a URL
  rota login <login-id> [code]  finish that login (no code for device flows)
  rota login <account-id>       sign an existing account in through its own CLI
  rota list [provider] [-r]     accounts with usage (-r forces a quota refresh)
  rota list --sessions          ...and what the CLIs are running or could resume
  rota run [id] <prompt>        ask an account and print the answer
  rota run [id]                 open that account's CLI instead
  rota run [id] -- <args...>    hand the CLI these arguments untouched
  rota set <id> [flags]         where an account sits and what it reads:
                                --order, --threshold, --cwd, --config
  rota remove <id>...           forget accounts and their staged credentials
  rota serve [addr] --token=T   serve the HTTP API and its playground

The id is optional: leave it out and rota takes the first account in the
rotation that is still under its threshold — order 1, then 2, and so on.
Order 0 keeps an account out of that queue without removing it; it can still
be run by naming its id.

serve takes a bare port as 0.0.0.0:port, or a full host:port; it listens on
127.0.0.1:8787 by default. The token may come from ROTA_TOKEN instead of the
command line, which keeps it out of the process table.

A first word that is not a command is taken as a prompt, but only when it
could not be a mistyped one: it has to contain a space, or follow -p. So
"rota lst" stays an error rather than becoming a question you pay for.

Add --json to any command for machine-readable output. After a bare -- it
belongs to the vendor CLI instead, along with everything else.
Default provider is claude. Accounts live in $ROTA_HOME or ~/.rota (0600);
no vendor CLI's own credential store is ever read or written.`

const shortUsage = "rota " + wire.Version + ` — several AI coding CLIs, several accounts, one rotation

Usage:
  rota "<prompt>"          ask; the rotation picks the account
  rota <id> "<prompt>"     ask that account
  rota login [provider]    add an account, or finish signing one in
  rota list                accounts, usage, health
  rota run [id] [flags]    ask with flags — or open the CLI itself
  rota set <id> [flags]    order, threshold, cwd, config
  rota remove <id>...      forget accounts
  rota serve [addr]        the HTTP API and its playground

` + "`rota --help`" + ` for the full story; ` + "`rota <command> -h`" + ` for its flags.`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type cli struct {
	json     bool
	out, err io.Writer
}

// usageError exits with status 2; exitCode carries a vendor CLI's own status.
type (
	usageError string
	exitCode   int
)

func (e usageError) Error() string { return string(e) }
func (e exitCode) Error() string   { return "exit " + strconv.Itoa(int(e)) }

func usageErr(format string, a ...any) error { return usageError(fmt.Sprintf(format, a...)) }

// run is the whole program: 0 on success, 1 on failure, 2 on misuse.
func run(argv []string, stdout, stderr io.Writer) int {
	rota.UserAgent = "rota/" + wire.Version // this program, not the SDK, is what providers see
	c := &cli{out: stdout, err: stderr}
	for len(argv) > 0 && argv[0] == "--json" {
		c.json, argv = true, argv[1:]
	}
	if len(argv) == 0 {
		fmt.Fprintln(stdout, shortUsage)
		return 0
	}
	cmd, args := argv[0], argv[1:]
	if cmd != "run" { // run forwards everything after the id verbatim
		args = c.stripJSON(args)
	}
	var err error
	switch cmd {
	case "list", "ls":
		err = c.list(args)
	case "run":
		err = c.run(args)
	case "set":
		err = c.set(args)
	case "remove", "rm":
		err = c.remove(args)
	case "serve":
		err = c.serve(args)
	case "login":
		err = c.login(args)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usage)
	case "version", "--version":
		fmt.Fprintln(stdout, "rota "+wire.Version)
	default:
		// The thing people do most often needs no verb. A first word that is
		// not a command may still be a question, and the whole argv is it.
		//
		// This lives in the default arm rather than in front of the switch so
		// there is no second list of command names to keep in step: whatever
		// the switch answers to is a command, and nothing else is.
		if looksLikePrompt(argv) {
			err = c.run(argv)
		} else {
			err = usageErr("unknown command %q", cmd)
		}
	}
	return c.report(err)
}

func (c *cli) report(err error) int {
	var code exitCode
	if errors.As(err, &code) {
		return int(code)
	}
	if err == nil {
		return 0
	}
	status := 1
	var ue usageError
	if errors.As(err, &ue) {
		status = 2
	}
	if c.json {
		_ = c.emit(map[string]string{"error": err.Error()})
	} else {
		fmt.Fprintln(c.err, "error:", err)
		if hint := remediation(err); hint != "" {
			fmt.Fprintln(c.err, "try:", hint)
		}
		if status == 2 {
			fmt.Fprintln(c.err, "see `rota help` for usage")
		}
	}
	return status
}

// remediation is this command's own advice for a condition the SDK stated.
// lib says what is wrong in its own vocabulary — it does not know this
// program's name or verbs — and the one place every error passes through is
// where the `rota ...` sentence belongs. JSON output never carries it: a
// machine wants the condition, not terminal prose.
func remediation(err error) string {
	switch {
	case errors.Is(err, rota.ErrReauth):
		return "`rota login <id>` to sign it in again, or `rota run <id> -i` to use the CLI's own session (ids: `rota list`)"
	case errors.Is(err, rotation.ErrNone):
		return "`rota set <id> --order 1` to give an account a place in the rotation"
	case errors.Is(err, rota.ErrUnsupported):
		return "`rota run <id> -- <flags>` to hand the CLI its own arguments untouched"
	}
	return ""
}

func (c *cli) stripJSON(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" {
			c.json = true
		} else {
			out = append(out, a)
		}
	}
	return out
}

// emit writes one JSON document, indented, with a trailing newline.
// encoding/json/v2 does not escape HTML, so an authorize URL stays readable
// instead of arriving as a run of \u0026.
func (c *cli) emit(v any) error {
	raw, err := rota.EncodeIndent(v)
	if err != nil {
		return err
	}
	_, err = c.out.Write(append(raw, '\n'))
	return err
}

// takeProvider pulls a provider name (bare, --provider X, --provider=X) out
// of arguments that belong to rota and are never forwarded. Anything else
// starting with "-" is returned for the caller to accept or reject.
func takeProvider(args []string) (name string, rest []string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--provider" && i+1 < len(args):
			name, i = args[i+1], i+1
		case strings.HasPrefix(a, "--provider="):
			name = strings.TrimPrefix(a, "--provider=")
		case slices.Contains(rota.Providers(), a):
			name = a
		default:
			rest = append(rest, a)
		}
	}
	return name, rest
}

// splitFlags separates positional arguments from the allowed flags; any
// other "-" argument is a usage error rather than something to ignore.
func splitFlags(rest []string, allowed ...string) (args []string, flags map[string]bool, err error) {
	flags = map[string]bool{}
	for _, a := range rest {
		switch {
		case !strings.HasPrefix(a, "-"):
			args = append(args, a)
		case slices.Contains(allowed, a):
			flags[a] = true
		default:
			return nil, nil, usageErr("unknown flag %q", a)
		}
	}
	return args, flags, nil
}

func parseID(s string) (int, error) {
	id, err := strconv.Atoi(s)
	if err != nil || id < 1 {
		return 0, usageErr("%q is not an account id; see `rota list`", s)
	}
	return id, nil
}

// looksLikeLoginID reports whether s has the shape of a login id — six hex
// characters — so a mistyped provider name gets a better message than "no
// pending login".
func looksLikeLoginID(s string) bool {
	return len(s) == 6 && strings.Trim(s, "0123456789abcdef") == ""
}

// login is every way an account is signed in, because from outside they are
// one thing:
//
//	rota login                   start a new account on the default provider
//	rota login codex             start one there instead
//	rota login 4f2a1b [code]     finish the login with that id
//	rota login 2 [options]       hand account 2 to its own CLI's login
//
// This used to be two commands, auth and login, divided by something that is
// rota's business rather than the person's: whether rota holds the account's
// credential or the vendor CLI keeps its own. rota already knows which, so
// it decides, and nobody has to remember which verb a provider wants.
func (c *cli) login(args []string) error {
	// A provider name is taken out first because it can also be written
	// --provider=x, and because naming one always means "start a new one".
	name, rest := takeProvider(args)
	if name != "" || len(rest) == 0 {
		if rest, _, err := splitFlags(rest); err != nil {
			return err
		} else if len(rest) > 0 {
			return usageError(loginUsage)
		}
		s, err := openStore()
		if err != nil {
			return err
		}
		defer s.Close()
		return c.begin(s, name)
	}

	// Otherwise the first argument names something that already exists. An
	// account id wins over a login id when both could read it — six random
	// hex characters are all digits often enough to matter — because an
	// account is a thing someone can see in `rota list`.
	id, numeric := 0, false
	if n, err := strconv.Atoi(rest[0]); err == nil && n >= 1 {
		id, numeric = n, true
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	if numeric && s.Find(id) != nil {
		// The delegated flow runs a vendor CLI a person will sit in front
		// of, and takes the lock again afterwards, so it owns its own store.
		s.Close()
		return c.signIn(id, rest[1:])
	}
	defer s.Close()
	if looksLikeLoginID(rest[0]) {
		if rest, _, err := splitFlags(rest[1:]); err != nil {
			return err
		} else if len(rest) > 1 {
			return usageError(loginUsage)
		}
		return c.finish(s, rest[0], strings.Join(rest[1:], ""))
	}
	if numeric {
		return fmt.Errorf("%w; see `rota list`", rota.WrapNoAccount(id))
	}
	return fmt.Errorf("%q is not a known provider, account id or login id; providers: %s",
		rest[0], strings.Join(rota.Providers(), ", "))
}

const loginUsage = `usage: rota login [provider]             start a new account there
       rota login <login-id> [code]      finish that login
       rota login <account-id> [options] sign it in through its own CLI

`

func (c *cli) begin(s *store.Store, provider string) error {
	l, err := s.BeginLogin(context.Background(), provider)
	if err != nil {
		return err
	}
	if c.json {
		return c.emit(l)
	}
	fmt.Fprintf(c.out, "Open this and approve access as your %s account:\n\n  %s\n\n", l.Provider, l.URL)
	switch l.Kind {
	case "device":
		fmt.Fprintf(c.out, "Then, once approved: rota login %s\n", l.ID)
	case "apikey":
		fmt.Fprintf(c.out, "Then: rota login %s <key>\n", l.ID)
	case "delegated":
		fmt.Fprintf(c.out, "Then: rota login %s\n  — registers the account; its CLI signs itself in afterwards.\n", l.ID)
	default:
		fmt.Fprintf(c.out, "Then: rota login %s <code>\n", l.ID)
	}
	if l.Delegated {
		fmt.Fprintf(c.out, "Or, without a key: rota login %s\n"+
			"  — registers the account and prints one command that signs %s in itself,\n"+
			"    keeping its own credentials in a directory rota reserves for it.\n", l.ID, l.Provider)
	}
	return nil
}

func (c *cli) finish(s *store.Store, id, code string) error {
	a, added, err := s.FinishLogin(context.Background(), id, code)
	if errors.Is(err, rota.ErrAuthPending) {
		if c.json {
			return c.emit(map[string]any{"id": id, "status": "pending"})
		}
		fmt.Fprintln(c.out, "Not approved yet. Approve in the browser, then run this again.")
		return nil
	}
	if err != nil {
		return err
	}
	status, verb := "refreshed", "Refreshed"
	if added {
		status, verb = "added", "Added"
	}
	login := wire.LoginCommand(a, s.Home(a))
	if c.json {
		doc := map[string]any{"id": a.ID, "provider": a.Provider, "email": a.Email, "uuid": a.UUID, "status": status}
		if a.Delegated {
			doc["delegated"] = true
			doc["loginCommand"] = login
		}
		return c.emit(doc)
	}
	fmt.Fprintf(c.out, "%s %s account %d (%s).\n", verb, a.Provider, a.ID, a.Label())
	if login != "" {
		fmt.Fprintf(c.out, "\nrota holds no credential for it. Sign it in once:\n\n  rota login %d\n", a.ID)
	}
	return nil
}

func (c *cli) list(args []string) error {
	filter, rest := takeProvider(args)
	rest, flags, err := splitFlags(rest, "-r", "--refresh", "-s", "--short", "--sessions", "--all")
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usageErr("usage: list [provider] [-r] [--short] [--sessions [--all]]")
	}
	// --sessions asks what the CLIs are doing: which are running, and which
	// conversations could be picked up again. It is off by default because
	// it reads directories another program owns, and nobody wants that on
	// every list.
	withSessions := flags["--sessions"]
	recent := 5
	if flags["--all"] {
		// --all lifts the per-account limit on a listing that is not being
		// asked for otherwise. Accepting it silently would look like it did
		// something.
		if !withSessions {
			return usageErr("--all belongs with --sessions: it is how many conversations to list")
		}
		recent = 0
	}
	force := flags["-r"] || flags["--refresh"]
	// --short is the listing that costs nothing: what is already known,
	// in rotation order, and no provider asked anything.
	short := flags["-s"] || flags["--short"]
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()

	var shown []*rota.Account
	for _, a := range s.Accounts {
		if filter == "" || a.Provider == filter {
			shown = append(shown, a)
		}
	}
	// Rotation order is the listing's order: the queue reads top to bottom
	// in the sequence rota will actually spend it, and whatever was left
	// out of the queue comes after.
	rotation.Sort(shown)
	if len(shown) == 0 {
		if c.json {
			return c.emit(map[string]any{"accounts": []wire.Account{}})
		}
		if filter != "" {
			fmt.Fprintf(c.out, "No %s accounts. Run `rota login %s`.\n", filter, filter)
		} else {
			fmt.Fprintf(c.out, "No accounts yet. Run `rota login [provider]`.\nProviders: %s\n", strings.Join(rota.Providers(), ", "))
		}
		return nil
	}
	var errs []error
	if !short {
		errs = s.Refresh(context.Background(), force, shown...)
	}

	if c.json {
		out := make([]wire.Account, 0, len(shown))
		for _, a := range shown {
			view := wire.Describe(a)
			view.Threshold = rotation.Cutoff(a)
			out = append(out, view)
		}
		doc := map[string]any{"accounts": out}
		if pick, err := rotation.Pick(shown); err == nil {
			// Which account a request with no id would run on, from the
			// readings just taken.
			doc["default"] = pick.ID
		}
		if len(errs) > 0 {
			warn := make([]string, 0, len(errs))
			for _, e := range errs {
				warn = append(warn, e.Error())
			}
			doc["warnings"] = warn
		}
		if withSessions {
			rep := sessions.Scan(s, recent)
			doc["instances"] = rep.Instances
			doc["sessions"] = rep.Sessions
			if rep.Shared != nil {
				doc["shared"] = rep.Shared
			}
			if len(rep.Notes) > 0 {
				doc["notes"] = rep.Notes
			}
		}
		return c.emit(doc)
	}

	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	if short {
		// The reading's age belongs beside it here more than anywhere else:
		// --short asks no provider anything, so what it shows may be hours
		// old and nothing else on the line would say so.
		fmt.Fprintln(w, "ORDER\tID\tPROVIDER\tACCOUNT\tUSAGE\tCHECKED")
		for _, a := range shown {
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n", place(a), a.ID, a.Provider, a.Label(), headline(a), checkedAgo(a))
		}
		_ = w.Flush()
		if withSessions {
			c.showSessions(sessions.Scan(s, recent))
		}
		return nil
	}
	fmt.Fprintln(w, "ORDER\tID\tPROVIDER\tACCOUNT\tUSAGE\tUNTIL\tCHECKED\tSTATUS")
	for _, a := range shown {
		status := string(a.Status())
		if a.Status() == rota.StatusReauth {
			status = "re-auth needed"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%d%%\t%s\t%s\n",
			place(a), a.ID, a.Provider, a.Label(), summarize(a.Quota), rotation.Cutoff(a), checkedAgo(a), status)
	}
	_ = w.Flush()
	// UNTIL is the threshold, so say what it decides rather than leaving a
	// column of percentages to be guessed at.
	if pick, perr := rotation.Pick(shown); perr == nil {
		fmt.Fprintf(c.out, "\n`rota run` without an id uses %s, and moves on at %d%% usage.\n", pick, rotation.Cutoff(pick))
	} else {
		fmt.Fprintf(c.out, "\n%v\n", perr)
	}
	for _, a := range shown {
		if a.Quota != nil && a.Quota.Note != "" {
			fmt.Fprintf(c.out, "\n%s: %s\n", a, a.Quota.Note)
		}
	}
	if withSessions {
		c.showSessions(sessions.Scan(s, recent))
	}
	for _, e := range errs {
		fmt.Fprintf(c.err, "warning: %v\n", e)
	}
	return nil
}

// showSessions prints what the CLIs are doing: what is running now, and what
// could be resumed. Both sections say when nothing was found, because an
// empty section and an absent one mean different things.
func (c *cli) showSessions(rep sessions.Report) {
	fmt.Fprintln(c.out, "\nRunning instances:")
	if len(rep.Instances) == 0 {
		fmt.Fprintln(c.out, "  none")
	}
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	for _, in := range rep.Instances {
		who := "-"
		if in.Account != 0 {
			who = fmt.Sprintf("#%d %s", in.Account, in.Label)
		}
		fmt.Fprintf(w, "  ●\t%s\t%s\t%s\t%s\n", what(in), who, orDash(in.Dir), instanceNote(in))
	}
	_ = w.Flush()

	fmt.Fprintln(c.out, "\nSessions:")
	if len(rep.Sessions) == 0 && rep.Shared == nil {
		fmt.Fprintln(c.out, "  none")
	}
	w = tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	for _, x := range rep.Sessions {
		who := "shared"
		if x.Account != 0 {
			who = fmt.Sprintf("#%d %s", x.Account, x.Label)
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", who, x.Provider, short(x.ID), orDash(x.Dir))
	}
	_ = w.Flush()
	if sh := rep.Shared; sh != nil {
		fmt.Fprintf(c.out, "  %s holds %d conversations across %d projects, shared by every account with no --config of its own.\n",
			sh.Dir, sh.Sessions, sh.Projects)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(c.out, "  note: %s\n", n)
	}
}

// what an instance is: the CLI's own name when it is one, and the editor's
// when an editor is holding it open. "cli" alone would be the one thing the
// row already implies.
func what(in sessions.Instance) string {
	if in.Kind == "cli" && in.Provider != "" {
		return in.Provider
	}
	return in.Kind
}

// instanceNote is the short right-hand column: how long a run rota started
// has been going, and the conversation it is in.
func instanceNote(in sessions.Instance) string {
	switch {
	case in.Session != "" && !in.Since.IsZero():
		return fmt.Sprintf("%s, session %s", wire.Since(in.Since), short(in.Session))
	case !in.Since.IsZero():
		return wire.Since(in.Since)
	case in.PID != 0:
		return fmt.Sprintf("pid %d", in.PID)
	}
	return ""
}

// short is the leading part of a session id, which is enough to recognise one
// and short enough to sit in a column. The whole id is in --json.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

const runUsage = `rota run [id] <prompt> [flags]

Asks one account for one answer and prints it. The id is optional: without
one, rota takes the first account in the rotation still under its threshold
(see ` + "`rota set`" + `). With no prompt either, it opens
that account's own CLI instead of asking it anything.

rota speaks its own vocabulary here — --model, --effort, --resume — and
builds the right headless command for whichever CLI the account belongs to,
so the same request works against every provider.

Asking a question is never interactive: a CLI that would otherwise stop over
the directory or a permission is given the flags that make it answer instead.

In text mode the answer is the only thing printed. -v adds which account ran
and how to resume; rota's own --json replaces it with the full result — cost,
usage, session id and exit status.

--stream prints the run as it happens rather than at the end, in whichever
form was asked for: prose in text mode, and with --json one complete JSON
object per line — the same events the HTTP API sends, opening with rota's own
saying which account, model and effort the run resolved to.

Conversations carry on: every run has a session id, and --resume <id>
continues from it. On its own, --resume picks up the most recent
conversation, which every provider can find without being told its id, and
--fork branches off one instead of adding to it.

  rota run "summarize this repo"     whichever account the rotation picks
  rota run 2 "summarize this repo"   that account
  rota run 2 "and the tests?" --resume 30040947-e103-4d58-8b0d-46417297cb1b
  rota run                           open the CLI itself, as it comes
  rota run 2 -i                      the same, for a named account
  rota run 2 -- --some-vendor-flag   hand it these arguments untouched

Flags:
`

// run asks one account a question, or opens its CLI.
//
// The account is named by its numeric id and carries its own provider, so a
// question needs nothing else: rota knows that Claude Code wants -p, Codex
// wants its exec subcommand and Grok Build wants a prompt file, and asking
// should not require knowing which. Two escape hatches remain for when
// rota's vocabulary is not what you want: -i opens the CLI as it comes, and
// -- hands it whatever follows, untouched.
func (c *cli) run(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		return c.answer(0, args)
	}
	// A leading number is the account; without one the rotation chooses.
	// Only a bare number counts, so a prompt is never mistaken for an id —
	// except a prompt that is nothing but digits, which -p still spells.
	id := 0
	rest := args
	if len(rest) > 0 && isNumber(rest[0]) {
		var err error
		if id, err = parseID(rest[0]); err != nil {
			return err
		}
		rest = rest[1:]
	}

	// Two ways to say "not rota's vocabulary": -- for the arguments that
	// follow, -i for the CLI's own session.
	if len(rest) > 0 && rest[0] == "--" {
		return c.handOver(id, rest[1:])
	}
	if i := slices.IndexFunc(rest, func(a string) bool { return a == "-i" || a == "--interactive" }); i >= 0 {
		return c.handOver(id, slices.Delete(slices.Clone(rest), i, i+1))
	}
	// Nothing left to say is a request for the CLI itself, not a mistake:
	// `rota run` and `rota run 2` open a session.
	if len(rest) == 0 {
		return c.handOver(id, nil)
	}
	return c.answer(id, rest)
}

// account resolves what the command line named — an id, or nothing at all,
// which means the rotation's choice — and adds the one hint that only a
// terminal can act on.
func account(s *store.Store, id int) (*rota.Account, error) {
	a, err := rotation.Choose(context.Background(), s, id)
	if errors.Is(err, rota.ErrNoAccount) {
		return nil, fmt.Errorf("%w; see `rota list`", err)
	}
	return a, err
}

// looksLikePrompt reports whether argv reads as a question rather than as a
// mistyped command.
//
// The cost of the two mistakes is not the same. Reading `rota lst` as a
// question sends it to a provider and charges for the answer; refusing a
// question costs one retry. So only an argument that could not be a command
// counts: one with whitespace in it, which no command has, or -p, which is
// how a one-word prompt is spelled. A bare account id may come first.
func looksLikePrompt(argv []string) bool {
	for i, a := range argv {
		if a == "--" {
			return false // everything after belongs to a CLI, not to rota
		}
		if a == "-p" || a == "--print" {
			return true
		}
		if strings.ContainsAny(a, " \t\n") {
			return i == 0 || (i == 1 && isNumber(argv[0]))
		}
	}
	return false
}

// isNumber reports whether s is a bare decimal number, which is how an
// account id is told apart from a prompt.
func isNumber(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// execProcess replaces rota with the vendor CLI, and is a variable only so a
// test can watch the handover instead of being replaced by it. syscall.Exec
// swaps the running binary, so a test that reached the real one would become
// the CLI: everything it had already reported would be discarded and
// everything after it would never run, while `go test` reported the CLI's own
// exit status as the package's.
var execProcess = execCLI

// handOver gives the account's CLI the terminal, arguments and all.
func (c *cli) handOver(id int, args []string) error {
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	a, err := account(s, id)
	if err != nil {
		return err
	}
	// The claim on the account is not released here: this process is about
	// to become the CLI, and the CLI is what must hold it. It goes only if
	// the handover does not happen.
	path, env, release, err := s.Prepare(context.Background(), a)
	if err != nil {
		return err
	}
	bin := filepath.Base(path)
	reg := sessions.RegistryFor(s)
	cwd, _ := os.Getwd() // where the CLI will start, which is where rota is
	_ = s.Close()        // everything is on disk; release the lock before handing over

	// Status goes to stderr so a redirected stdout carries only the CLI's
	// own output.
	fmt.Fprintf(c.err, "rota: %s via %s\n", a, bin)
	// This process is about to become the CLI: execve keeps the process id,
	// so the entry written here goes on describing what is running, and is
	// cleaned up by the liveness check rather than by anything rota runs
	// afterwards -- there is no afterwards.
	if _, rerr := reg.Add(sessions.Instance{
		Account: a.ID, Label: a.Label(), Provider: a.Provider, Dir: cwd,
	}); rerr != nil {
		fmt.Fprintf(c.err, "warning: could not record this run: %v\n", rerr)
	}
	err = execProcess(path, append([]string{bin}, args...), env)
	release() // only reached when the handover did not happen
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return exitCode(ee.ExitCode())
	}
	return err
}

// explain turns "rota cannot build a command line for this provider" into
// the fact that is actually actionable.
//
// There are two reasons a provider cannot be asked a question, and they need
// different answers: rota may not model that CLI's headless flags, or the
// CLI may simply not be installed. The second is far more common and the
// message for the first is misleading when it applies — it sends someone
// looking for an escape hatch that would fail the same way.
func (c *cli) explain(a *rota.Account, s *store.Store, err error) error {
	if !errors.Is(err, rota.ErrUnsupported) || rota.Flavor(a.Provider) != "" {
		return err
	}
	cmd, serr := rota.Stage(a, s.Home(a))
	if serr != nil {
		return err
	}
	if _, lerr := exec.LookPath(cmd.Bin); lerr != nil {
		return rota.WrapNoBinary(cmd.Bin, lerr)
	}
	return fmt.Errorf("%w; rota does not model %s's headless flags, so pass its own: rota run %d -- <flags>",
		err, a.Provider, a.ID)
}

// answer asks one question and prints the reply.
func (c *cli) answer(id int, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var (
		flagPrompt = fs.String("p", "", "the prompt, for anyone used to typing it as a flag")
		model      = fs.String("model", "", "model to use; the provider's default when empty")
		effort     = fs.String("effort", "", "reasoning effort, for providers that have one")
		stream     = fs.Bool("stream", false, "print events as they happen: text, or one JSON object per line with --json")
		cwd        = fs.String("cwd", "", "working directory for the run")
		timeout    = fs.Duration("timeout", 0, "give up after this long")
		mode       = fs.String("permission-mode", "", "how the agent asks before acting")
		sandbox    = fs.String("sandbox", "", "sandbox profile, for providers that have one")
		system     = fs.String("system-prompt", "", "instructions added before the prompt")
		resume     = fs.String("resume", "", "continue an earlier conversation by session id; on its own, the most recent one")
		cont       = fs.Bool("continue", false, "continue the most recent conversation in this directory")
		session    = fs.String("session", "", "name a new conversation with this id, so it can be resumed later")
		fork       = fs.Bool("fork", false, "with --resume: branch into a new conversation instead of continuing it")
		schema     = fs.String("json-schema", "", "constrain the answer to this JSON Schema")
		stateless  = fs.Bool("stateless", false, "answer from nothing: no session saved, no settings, memory or rules read")
		verbose    = fs.Bool("v", false, "also report which account ran and how to resume the conversation")
		asJSON     = fs.Bool("json", false, "print the whole result — cost, usage, session id, exit status")
		altPrompt  = fs.String("print", "", "an alias for -p")
	)
	// One-letter forms for the everyday flags.
	fs.StringVar(model, "m", "", "= --model")
	fs.StringVar(effort, "e", "", "= --effort")
	fs.BoolVar(stream, "s", false, "= --stream")
	fs.BoolVar(cont, "c", false, "= --continue")
	fs.StringVar(resume, "r", "", "= --resume")
	fs.DurationVar(timeout, "t", 0, "= --timeout")
	fs.BoolVar(stateless, "S", false, "= --stateless")
	words, err := parseFlags(fs, bareResume(args))
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(c.out, runUsage)
			fs.SetOutput(c.out)
			fs.PrintDefaults()
			return nil
		}
		return usageErr("%v", err)
	}
	text := strings.TrimSpace(strings.Join(words, " "))
	if text == "" {
		text = *flagPrompt
	}
	if text == "" {
		text = *altPrompt
	}

	spec := rota.Spec{
		Prompt: text, Model: *model, Effort: *effort, Stream: *stream, Cwd: *cwd,
		PermissionMode: *mode, Sandbox: *sandbox, SystemPrompt: *system,
		Resume: *resume, Continue: *cont, SessionID: *session,
		TimeoutSeconds: int(timeout.Seconds()), OneShot: true,
	}
	spec.ForkSession = *fork
	if *schema != "" {
		spec.JSONSchema = json.RawMessage(*schema)
	}

	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	a, err := account(s, id)
	if err != nil {
		return err
	}
	// What the account already knows and the request did not say — its own
	// project directory, most of all.
	spec = spec.For(a)
	if *stateless {
		// Per provider, because each CLI spells statelessness its own way —
		// and one that cannot spell it gets a refusal, not a half-promise.
		switch rota.Flavor(a.Provider) {
		case "claude":
			spec.Ephemeral = true
			spec.SettingSources = []string{}
			spec.Hermetic = true
		case "codex":
			spec.Ephemeral = true
			spec.IgnoreUserConfig = true
			spec.IgnoreRules = true
		default:
			return usageErr("--stateless is not available for %s: its CLI has no flags to skip its own state", a.Provider)
		}
	}
	if spec.Resume != "" && spec.Resume != "last" {
		// The conversation may live in a sibling account's home; copy it in
		// so the rotation's promise — continue where the quota ran out —
		// holds across accounts.
		if err := sessions.CopyForResume(s, a, spec.Resume); err != nil {
			return err
		}
	}
	// Asked after the account is known, so a wrong id says so rather than
	// complaining about a prompt for an account that does not exist. Only
	// flags with no prompt reach this: bare `rota run [id]` opens the CLI.
	if text == "" {
		return usageErr("usage: run [id] <prompt> [flags]   (or `run [id]` for the CLI itself)")
	}

	// Streaming means rota renders the events, not that the vendor's own
	// lines are handed through: text mode prints prose, JSON mode prints one
	// object per line. rota speaks first, so a reader knows which account,
	// model and effort the run resolved to before any of it has happened.
	// Write down whose run this is before it starts. Nothing else can say:
	// by default every Claude Code account reads the same ~/.claude, so a
	// process list shows the work without showing whose quota pays for it.
	started, rerr := sessions.RegistryFor(s).Add(sessions.Instance{
		Account: a.ID, Label: a.Label(), Provider: a.Provider,
		Dir: spec.Cwd, Session: spec.Resume,
	})
	if rerr != nil {
		// Not being able to record a run is no reason not to make it.
		fmt.Fprintf(c.err, "warning: could not record this run: %v\n", rerr)
	}

	// The events are read whether or not they are printed. Printing them is
	// what --stream asks for; reading them is how the conversation id reaches
	// the entry above — while the run is going for a streamed one, and only
	// at the end for a buffered one, whose CLI prints a single document when
	// it has finished.
	watch := newEventStream(c.out, c.json || *asJSON, a.ID, a.Provider)
	watch.quiet = !*stream
	watch.learn = started.Learned
	var live *eventStream
	if *stream {
		model, effort, _ := rota.Resolved(a, s.Home(a), spec)
		live = watch
		_ = live.stream.Send(message.Event{
			Type: "init", Model: model, Effort: effort, Cwd: spec.Cwd, SessionID: spec.Resume,
		})
	}
	if *verbose {
		fmt.Fprintf(c.err, "rota: %s\n", a)
	}
	res, err := s.Run(context.Background(), a, spec, nil, watch)
	_ = started.End()
	if live != nil {
		// A stream says how it ended in the stream, whichever way it ended,
		// so a reader never has to guess whether more is coming.
		live.end(wire.Ended(res, err))
		if err != nil {
			return c.explain(a, s, err)
		}
		if res.ExitCode != 0 {
			return exitCode(res.ExitCode)
		}
		return nil
	}
	if err != nil {
		return c.explain(a, s, err)
	}
	// --json is rota's own, and works before the command or among its
	// flags. It is not stripped globally for run, because a vendor CLI has
	// one too and `run <id> -- --json` must still reach it.
	if c.json || *asJSON {
		// The same shape the HTTP reply has: the result, and what rota read
		// out of it. One JSON document means one thing either way in.
		return c.emit(struct {
			*rota.Result
			Blocks []message.Block `json:"blocks,omitzero"`
			Ask    *message.Ask    `json:"ask,omitzero"`
		}{res, message.Blocks(res.Result), message.Asked(res.Result)})
	}
	if !*stream && res.Result != "" {
		fmt.Fprintln(c.out, res.Result)
	}
	// Nothing else is printed by default: an answer is what was asked for,
	// and a terminal full of rota's own commentary is noise whether or not
	// it is on stderr.
	if *verbose && res.SessionID != "" {
		fmt.Fprintf(c.err, "rota: session %s — continue it with `rota run %d <prompt> --resume %s`\n",
			res.SessionID, a.ID, res.SessionID)
	}
	if res.Stderr != "" && (res.IsError || res.ExitCode != 0) {
		fmt.Fprintln(c.err, res.Stderr)
	}
	if res.ExitCode != 0 {
		return exitCode(res.ExitCode)
	}
	return nil
}

// set is everything about an account that is a choice rather than a fact:
// where it sits in the rotation, when the rotation gives up on it, and which
// project it belongs to.
//
// One command because it is one write. These were three — order, threshold
// and project — while the HTTP side did all of it with a single PATCH, which
// left the command line more broken up than its own API. With no flags it
// asks rather than changes.
//
// --cwd and --config stay separate on purpose. A working directory is a
// repository someone will commit; a config directory is where this account's
// credential file is written. Pointing them at the same place puts a live
// token in a commit, which is why Account.CheckProject refuses it.
func (c *cli) set(args []string) error {
	if len(args) == 0 {
		return usageError(setUsage)
	}
	id, err := parseID(args[0])
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var (
		order     = fs.String("order", "", "place in the rotation: a number, first, last, up, down, before:<id>, after:<id>; 0 or out leaves it")
		threshold = fs.Int("threshold", 0, "usage percent at which the rotation moves on")
		cwd       = fs.String("cwd", "", "where runs on this account start")
		config    = fs.String("config", "", "this account's own CLI configuration and credentials")
		clear     = fs.Bool("clear", false, "forget cwd and config, so the account goes back to the defaults")
	)
	if _, err := parseFlags(fs, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(c.out, setUsage)
			fs.SetOutput(c.out)
			fs.PrintDefaults()
			return nil
		}
		return usageErr("%v", err)
	}
	// Which flags were actually written, not which have a non-zero value:
	// --order 0 is a real choice, and means the opposite of leaving it out.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var place rotation.Place
	if given["order"] {
		if place, err = rotation.ParsePlace(*order); err != nil {
			return usageErr("%v", err)
		}
	}
	if given["threshold"] && (*threshold < 1 || *threshold > 100) {
		return usageErr("threshold must be a number from 1 to 100, not %d", *threshold)
	}

	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	a := s.Find(id)
	if a == nil {
		return fmt.Errorf("%w; see `rota list`", rota.WrapNoAccount(id))
	}

	// Nothing to set is a question, not a change.
	if len(given) == 0 {
		return c.show(a, s.Home(a))
	}

	// The project half is checked on a copy, so a refusal leaves the stored
	// account exactly as it was rather than half-applied.
	want := *a
	if *clear {
		want.Cwd, want.ConfigDir = "", ""
	}
	if *cwd != "" {
		if want.Cwd, err = filepath.Abs(*cwd); err != nil {
			return err
		}
	}
	if *config != "" {
		if want.ConfigDir, err = filepath.Abs(*config); err != nil {
			return err
		}
	}
	if err := want.CheckProject(); err != nil {
		return err
	}
	// The move goes next, before anything is written into the account: a
	// refusal — up from outside the queue, before an id that is not in it —
	// must also leave the store as it was.
	var moved rotation.Moved
	if given["order"] {
		if moved, err = rotation.Move(s.Accounts, a, place); err != nil {
			return err
		}
	}
	a.Cwd, a.ConfigDir = want.Cwd, want.ConfigDir
	if given["threshold"] {
		a.Threshold = *threshold
	}
	if given["order"] || given["threshold"] {
		// Any deliberate choice about the rotation settles it for this store,
		// so a later load never renumbers what was chosen here.
		s.Ordered = true
	}
	if err := s.Save(); err != nil {
		return err
	}
	if given["order"] && !c.json {
		// A move changes the neighbours too, so the answer is the queue, not
		// one account's number. Anything else set at the same time still
		// gets the usual block.
		c.sayMoved(a, moved, s.Accounts)
		if len(given) == 1 {
			return nil
		}
	}
	return c.show(a, s.Home(a))
}

// sayMoved is what a move did, in two lines: what moved and who made room,
// then the rotation as it now stands.
func (c *cli) sayMoved(a *rota.Account, m rotation.Moved, all []*rota.Account) {
	name := func(x *rota.Account) string { return fmt.Sprintf("#%d %s", x.ID, x.Label()) }
	var did string
	switch {
	case m.Was == 0 && m.Now == 0:
		did = "is already out of the rotation"
	case m.Now == 0:
		did = "left the rotation"
	case m.Was == 0:
		did = "joined the rotation at " + ordinal(m.Now)
	case m.Was == m.Now:
		did = "is already " + ordinal(m.Now) + " in the rotation"
	default:
		did = "moved to " + ordinal(m.Now) + " in the rotation"
	}
	line := name(a) + " " + did
	if len(m.Shifted) > 0 {
		parts := make([]string, len(m.Shifted))
		for i, x := range m.Shifted {
			parts[i] = name(x) + " " + ordinal(x.Order)
		}
		parts[0] = name(m.Shifted[0]) + " is now " + ordinal(m.Shifted[0].Order)
		line += "; " + strings.Join(parts, ", ")
	}
	fmt.Fprintln(c.out, line+".")

	sorted := append([]*rota.Account(nil), all...)
	rotation.Sort(sorted)
	var in, out []string
	for _, x := range sorted {
		if rotation.InQueue(x) {
			in = append(in, name(x))
		} else {
			out = append(out, name(x))
		}
	}
	line = "Rotation: empty"
	if len(in) > 0 {
		line = "Rotation: " + strings.Join(in, ", ")
	}
	line += "."
	if len(out) > 0 {
		line += " Out of it: " + strings.Join(out, ", ") + "."
	}
	fmt.Fprintln(c.out, line)
}

// ordinal is 1st, 2nd, 3rd, 4th ... 11th, 12th, 13th ... 21st.
func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}

// show is what an account is set to: one block for the two questions people
// ask about it, where it sits and what it reads.
func (c *cli) show(a *rota.Account, home string) error {
	if c.json {
		view := wire.Describe(a)
		view.Threshold = rotation.Cutoff(a)
		return c.emit(view)
	}
	place := fmt.Sprintf("number %d, until %d%% usage", a.Order, rotation.Cutoff(a))
	if a.Order == 0 {
		place = fmt.Sprintf("out of the rotation; run it by id, or put it back with `rota set %d --order last`", a.ID)
	}
	where := a.Cwd
	if where == "" {
		where = "wherever rota is run from"
	}
	fmt.Fprintf(c.out, "#%d %s\n  rotation    %s\n  runs in     %s\n  configured  %s\n",
		a.ID, a.Label(), place, where, home)
	if a.ConfigDir == "" && rota.Flavor(a.Provider) == "claude" {
		fmt.Fprintln(c.out, "  memory and skills come from your own ~/.claude until --config names somewhere else")
	}
	return nil
}

const setUsage = `usage: rota set <id> [--order place] [--threshold pct] [--cwd dir] [--config dir] [--clear]

Sets what is a choice about an account rather than a fact about it, in one
write. With no flags it prints what the account is set to.

--order is its place in the rotation. The rotation is a list: putting an
account somewhere moves the ones after it down, and the numbers always read
1, 2, 3. A place is a number (1 goes first; past the end means last), or
first, last, up, down, before:<id> or after:<id>. 0 or out takes it out of
the queue without removing it — it can still be run by naming its id.
--threshold is the usage at which the rotation gives up on it and moves to
the next one.

--cwd is where its runs start when a request names no directory. --config is
the account's own CLI configuration — its memory files, skills and settings —
and the private home its credentials are staged in, which is why it must not
be the project directory itself.

  rota set 2 --order 1 --threshold 80    first in the queue, moves on at 80%
  rota set 2 --order up                  one place earlier
  rota set 2 --order before:5            right before account 5
  rota set 2 --order out                 out of the rotation
  rota set 2 --cwd ~/src/api --config ~/.rota/api-memory
  rota set 2                             what account 2 is set to

Flags:
`

func (c *cli) remove(args []string) error {
	if len(args) == 0 {
		return usageErr("usage: remove <id>...")
	}
	ids := make([]int, 0, len(args))
	for _, a := range args {
		id, err := parseID(a)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	// The whole set is checked before any of it is deleted. Remove takes the
	// home with it, and for a delegated account that directory is the
	// credential — so failing partway through would leave the accounts before
	// the failure destroyed on disk, still listed, and already reported as
	// removed. Whatever can be known in advance is settled here instead.
	for _, id := range ids {
		a := s.Find(id)
		if a == nil {
			return rota.WrapNoAccount(id)
		}
		if s.Busy(a) {
			return fmt.Errorf("%w: %s is running; stop it before removing the account", rota.ErrBusy, a)
		}
	}
	var removed []map[string]any
	for _, id := range ids {
		a := s.Find(id)
		if err := s.Remove(id); err != nil {
			// Something no check could have foreseen — an account claimed in
			// the moment since, a home that will not delete. What already
			// happened is written down anyway: an account whose credentials
			// are gone must not be left in the store looking usable.
			return errors.Join(err, s.Save())
		}
		removed = append(removed, map[string]any{"id": a.ID, "provider": a.Provider, "email": a.Email})
		if !c.json {
			fmt.Fprintf(c.out, "Removed %s account %d (%s).\n", a.Provider, a.ID, a.Label())
		}
	}
	if err := s.Save(); err != nil {
		return err
	}
	if c.json {
		return c.emit(map[string]any{"removed": removed})
	}
	return nil
}

const serveUsage = `rota serve [address] --token=T

Serves the HTTP API and its playground. The address may be a bare port,
which listens on every interface, or a full host:port; it defaults to
127.0.0.1:8787. The token is mandatory and may come from ROTA_TOKEN
instead of the command line, which keeps it out of the process table.

Flags:
`

// parseFlags allows flags before or after the positional arguments, which is
// what people actually type.
// bareResume rewrites a --resume with nothing after it into --resume=last,
// which every provider now understands as the most recent session.
//
// The flag package has no optional value: --resume would have to be either a
// string, which swallows whatever follows, or a bool, which then cannot take
// a session id. Rewriting keeps both spellings, and the rule is narrow — a
// --resume is bare only when the next word is another flag or there is no
// next word, so `--resume s-1` still names a session.
func bareResume(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Everything after this belongs to the vendor's CLI.
			return append(out, args[i:]...)
		}
		if a == "--resume" || a == "-resume" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				out = append(out, "--resume=last")
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// openStore opens the account store and settles its rotation order.
//
// Numbering a store written before rotation existed is the rotation
// package's rule, not the store's, so it happens here — at the one door
// every command goes through — rather than inside the store on load.
func openStore() (*store.Store, error) {
	s, err := store.Open("")
	if err != nil {
		return nil, err
	}
	rotation.Backfill(s)
	return s, nil
}

func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	return positional, nil
}

// signIn hands an account whose vendor CLI holds its own credentials to that
// CLI's own login.
//
// rota runs the CLI's own login rather than printing it, because the part
// that must not go wrong — pointing it at this account's private directory
// instead of the person's own — is exactly the part they would have to
// retype.
//
// The CLI runs as a child rather than replacing rota, which costs nothing —
// it still has the terminal it needs to show a URL and wait — and buys the
// one thing that must happen afterwards: reading back who it signed in as,
// so the account has a name instead of a random handle.
func (c *cli) signIn(id int, extra []string) error {
	// extra belongs to the vendor's login, which has options rota has no
	// business knowing about — which region to sign in to, say.
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	a := s.Find(id)
	if a == nil {
		return fmt.Errorf("%w; see `rota list`", rota.WrapNoAccount(id))
	}
	plan, ok := rota.LoginPlanFor(a, s.Home(a))
	if !ok {
		return fmt.Errorf("account %d does not sign itself in: rota holds its credential, so it is signed in already",
			id)
	}
	// The same claim a run takes. This hands the home to the vendor's own
	// login, which writes a new credential file over whatever is there — so of
	// everything that touches that directory, this is the one certain to
	// destroy what a running CLI is using. Held until the login is over and
	// what it signed in as has been read back.
	release, idle := s.Hold(a)
	if !idle {
		return fmt.Errorf("%w: %s is running; its login would replace the credential file that CLI is using",
			rota.ErrBusy, a)
	}
	defer release()
	if err := os.MkdirAll(s.Home(a), 0o700); err != nil {
		return err
	}
	path, err := exec.LookPath(plan.Bin)
	if err != nil {
		return rota.WrapNoBinary(plan.Bin, err)
	}
	home := s.Home(a)
	// The lock is not rota's to hold while a person reads a browser page.
	_ = s.Release()

	fmt.Fprintf(c.err, "rota: signing %s in, in %s\n", a, home)
	login := exec.Command(path, append(plan.Args, extra...)...)
	login.Env = rota.Environ(store.HostEnv(), &rota.Command{Env: plan.Env, Drop: plan.Drop})
	login.Stdin, login.Stdout, login.Stderr = os.Stdin, c.out, c.err
	if err := login.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitCode(ee.ExitCode())
		}
		return err
	}

	// Take the lock again to write down whose account this now is.
	s2, err := openStore()
	if err != nil {
		return err
	}
	defer s2.Close()
	again := s2.Find(id)
	if again == nil {
		return nil // removed while the browser was open; nothing to record
	}
	if err := rota.Adopt(again, home); err != nil {
		return err
	}
	if err := s2.Save(); err != nil {
		return fmt.Errorf("signed in, but the account could not be updated: %w", err)
	}
	fmt.Fprintf(c.err, "rota: %s is signed in\n", again)
	return nil
}

// ROTA_TOKEN is this command's variable, so this command is what says an
// agent must not see it: it authorizes running every account on the machine,
// and the child of a run is a coding agent with a shell. Declared here beside
// the flag that reads it, rather than in the SDK, which has no way to know
// what an application calls its own secrets.
func init() { store.HideFromAgents("ROTA_TOKEN") }

// serve runs the HTTP API and its playground. The token is mandatory: an
// account runner anyone can reach is not something to start by accident.
func (c *cli) serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var (
		roots     multiFlag
		token     = fs.String("token", os.Getenv("ROTA_TOKEN"), "bearer token every request must carry")
		dangerous = fs.Bool("allow-dangerous", false, "permit permission-bypass and full-access options")
		rawFlags  = fs.Bool("allow-raw-flags", false, "let callers pass flags straight to the vendor CLI (this undoes every other gate)")
		timeout   = fs.Duration("timeout", 10*time.Minute, "hard cap on one run")
		conc      = fs.Int("max-concurrent", 8, "how many CLIs may run at once")
		keep      = fs.Duration("refresh-every", 2*time.Minute, "how often to rotate expiring tokens and re-read usage in the background (0 or less turns it off)")
		certFile  = fs.String("tls-cert", "", "TLS certificate; without it the token travels in clear text")
		keyFile   = fs.String("tls-key", "", "TLS private key")
		quiet     = fs.Bool("quiet", false, "log only warnings and errors")
	)
	fs.Var(&roots, "root", "confine cwd, uploads and extra directories here (repeatable)")

	// The address is positional, so flags may come before or after it.
	var addr string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(c.out, serveUsage)
				fs.SetOutput(c.out)
				fs.PrintDefaults()
				return nil
			}
			return usageErr("%v", err)
		}
		if rest = fs.Args(); len(rest) == 0 {
			break
		}
		if addr != "" {
			return usageErr("serve takes one address, got %q and %q", addr, rest[0])
		}
		addr, rest = rest[0], rest[1:]
	}
	if *token == "" {
		return usageErr("serve needs --token=... (or ROTA_TOKEN in the environment)")
	}
	listen, err := listenAddr(addr)
	if err != nil {
		return usageErr("%v", err)
	}
	for i, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return usageErr("root %q: %v", r, err)
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return usageErr("root %q is not an existing directory", r)
		}
		roots[i] = abs
	}
	if (*certFile == "") != (*keyFile == "") {
		return usageErr("--tls-cert and --tls-key go together")
	}

	logger := slog.New(slog.NewTextHandler(c.err, &slog.HandlerOptions{Level: level(*quiet)}))
	// A flag of zero means "off" to a person, but zero means "the default"
	// to the option, so say off explicitly.
	refresh := *keep
	if refresh <= 0 {
		refresh = -1
	}
	srv, err := api.New(api.Options{
		Token: *token, Roots: roots, AllowDangerous: *dangerous,
		Timeout: *timeout, MaxConcurrent: *conc, Log: logger, AllowRawFlags: *rawFlags,
		RefreshEvery: refresh,
	})
	if err != nil {
		return err
	}
	scheme := "http"
	if *certFile != "" {
		scheme = "https"
	}
	fmt.Fprintf(c.err, "rota %s serving on %s://%s\n", wire.Version, scheme, listen)
	if len(roots) == 0 {
		fmt.Fprintln(c.err, "warning: no --root given, so a caller may name any directory on this machine")
	}
	if *certFile == "" && !strings.HasPrefix(listen, "127.0.0.1") && !strings.HasPrefix(listen, "localhost") {
		fmt.Fprintln(c.err, "warning: no TLS off the loopback address; the bearer token travels in clear text")
	}
	if *dangerous {
		fmt.Fprintln(c.err, "warning: --allow-dangerous is on; callers may bypass every permission check")
	}
	if *rawFlags {
		fmt.Fprintln(c.err, "warning: --allow-raw-flags is on; a caller can pass any vendor flag, which undoes --root and --allow-dangerous")
	}
	server := &nethttp.Server{
		Addr:    listen,
		Handler: srv.Handler(),
		// A slow client must not be able to hold a connection open for
		// ever. There is no write deadline: a streamed run legitimately
		// takes as long as the run does, and the run has its own timeout.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}

	// Shut down on the first signal and wait for work in flight; a second
	// signal stops waiting, for whoever is in a hurry.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, 1)
	go func() {
		if *certFile != "" {
			errs <- server.ListenAndServeTLS(*certFile, *keyFile)
			return
		}
		errs <- server.ListenAndServe()
	}()
	select {
	case err := <-errs:
		if errors.Is(err, nethttp.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		stop()
		fmt.Fprintln(c.err, "\nrota: shutting down; waiting for runs in flight (interrupt again to stop now)")
		shutdown, cancel := context.WithTimeout(context.Background(), *timeout+10*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		// Shutdown waits for handlers but cannot cancel them, and each
		// vendor CLI is in its own process group. Stop ends the runs, so
		// nothing is left behind still spending.
		srv.Stop()
		if err != nil {
			return fmt.Errorf("shutting down: %w", err)
		}
		return nil
	}
}

// level is how much the server says about itself.
func level(quiet bool) slog.Level {
	if quiet {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// listenAddr turns what a person typed into an address net.Listen accepts. A
// bare port means every interface, because someone who writes "8787" rather
// than "127.0.0.1:8787" is asking to be reachable from elsewhere.
func listenAddr(in string) (string, error) {
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

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// place renders an account's position in the rotation; an account left out
// of it shows a dash rather than a zero, which would read as "first".
func place(a *rota.Account) string {
	if !rotation.InQueue(a) {
		return "-"
	}
	return strconv.Itoa(a.Order)
}

// checkedAgo says how old a quota reading is. An unmetered provider has no
// limits to read, so "never checked" would misreport a fact rather than a gap.
func checkedAgo(a *rota.Account) string {
	v := wire.Describe(a)
	v.Threshold = rotation.Cutoff(a)
	switch {
	case !v.Metered:
		return "n/a"
	case v.CheckedAgo == "":
		return "never"
	}
	return v.CheckedAgo
}

// headline renders the one usage number the rotation judges an account by.
func headline(a *rota.Account) string {
	if !rota.Metered(a.Provider) {
		return "n/a"
	}
	if a.QuotaAt == 0 {
		return "?"
	}
	return fmt.Sprintf("%.0f%%", a.Percent())
}

// summarize renders a quota as one table cell, most important window first.
func summarize(q *rota.Quota) string {
	if q == nil || len(q.Windows) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(q.Windows))
	for _, w := range q.Windows {
		s := fmt.Sprintf("%s %.0f%%", w.Name, w.Percent)
		if in := wire.Countdown(w.ResetsAt); in != "" {
			s += " (" + in + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "  ")
}
