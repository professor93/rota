# rota

Run several AI coding CLIs across several accounts, without ever switching
the account you are logged into.

A *rota* is a roster of people taking turns, and that is what this keeps.
Register each account once, give it a place in the queue, and ask:
`rota run "…"` on the command line, or `POST /v1/run` over HTTP. rota takes
the first account still under its threshold and moves on to the next when it
is spent. Name one instead — `rota run 2 …`, `POST /v1/accounts/2/run` — and
that decision is yours again.

Providers: **claude**, **codex**, **grok**. A fourth, kimi, is built in but
not offered for login until its service completes a sign-in; see Providers.

One library, and the rest are examples of using it:

| Piece | Import path | Depends on |
|---|---|---|
| the SDK: accounts, logins, tokens, running an agent | `rota/lib` | the Go standard library, nothing else |
| an optional account store: files, or your own backend | `rota/store` | `rota/lib` |
| which account to spend, and when to move on | `rota/rotation` | `rota/lib`, `rota/store` |
| the shapes a transport needs: views, uploads, fields | `rota/wire` | `rota/lib` |
| reading an answer: blocks, events, questions | `rota/message` | `rota/lib` |
| what the vendor CLIs are doing: running now, resumable conversations | `rota/sessions` | `rota/lib`, `rota/store` |
| an HTTP server with a playground | `rota/api` | all of the above |
| the command, a module of its own | `./cmd/rota` | all of the above |

The table's short names are this repository's layout; the module paths are
`github.com/professor93/rota` and, for the SDK alone,
`github.com/professor93/rota/lib` — the one line an outside program needs.
The command is the module `github.com/professor93/rota/cmd/rota`, which
requires the other at a released version.

`rota/lib` is a Go library and nothing else. You call it with Go values and
get Go values back — begin a login, finish it, refresh a token, read a
quota, stage a credential, run an agent — and it has no opinion about where
accounts are kept, which one you ought to spend, what a form should show, or
how any of it looks as JSON. Those are decisions an application makes, so
they live in packages an application can take or leave. The command and the
HTTP server are two applications that took them; a third is free to keep
accounts in Postgres, pick by price, and never import a line of the rest.

**The SDK and the server packages have no third-party dependencies.** Their
module, `github.com/professor93/rota`, has no `go.sum`, nothing to audit, and
nothing to update. The `rota` program is a module of its own, so whatever it
comes to need never reaches a program that imports them. It uses
github.com/charmbracelet/x/vt to keep the screen of each held session.
Go 1.27 removed the last two temptation: `encoding/json/v2` is standard
library now, so the hand-rolled JSON layer is gone and nothing here reaches
for a faster JSON package. It was not always so: the server
began on a router framework, which cost 23 modules and 100 packages —
including an HTTP/3 stack, a MongoDB driver, protobuf, a YAML parser and a
2018 commit of an abandoned library — to provide path matching the standard
library has done since Go 1.22, plus a map alias. Removing it took the binary
from 21.2 MB to 7.4 MB.

`rota/lib` takes values and returns values: it reads and writes none of *your*
storage — a program that keeps accounts in its own database never links the
store package. Verified by building an application against `rota/lib` alone:
its binary contains no symbol from `rota/store`, `rota/rotation`, `rota/wire`
or `rota/message`.

It does touch the disk, in one place and for one reason: staging a credential
file into the private home a vendor CLI is about to read, and reading that
file back afterwards in case the CLI rotated the token in it — for claude,
the account's own login in Claude Code's credential store inside that home.
That is the act of running an agent, which is what the SDK is for. Where
*accounts* live is still none of its business.

It also reads no environment at all — a test scans the sources so the rule
cannot erode. The child's environment is what the application passes as
Command.BaseEnv; which variables are secret is the application's fact, kept
in `rota/store` (HideFromAgents, HostEnv), where `ROTA_HOME` is declared by
the package that defines it and `ROTA_TOKEN` by the command. lib never
hears either name.

**In this manual.** [How it works](#how-it-works-and-why-nothing-is-faked) ·
[Install](#install) · [The command](#the-command) ·
[The HTTP API](#the-http-api) · [Models and effort](#models-and-effort) ·
[Providers](#providers) · [Quota](#quota) ·
[Token lifetimes](#token-lifetimes) · [Storage](#storage) ·
[Using the library](#using-the-library) · [Performance](#performance) ·
[Layout](#layout)

## How it works, and why nothing is faked

rota is not a proxy. It never speaks a provider's inference API, never
rewrites a request, and never pretends to be someone else's client. It
launches the vendor's real CLI with a credential you own, so the provider
sees its genuine first-party client because it *is* that client.

Only what no CLI exposes is implemented here: logging an account in,
refreshing its token, and reading its quota — plain documented OAuth
endpoints, called with an honest `rota/<version>` user agent — the version `rota version` prints.

Nothing global changes: a plain `claude` or `codex` in another terminal keeps
whatever account it was already using, and two rota runs can sit on two
different accounts at once.

## Install

The short way is the installer in the repository root — see the top-level
README. What follows builds from source.

Go 1.27 or newer — released 2026-08-19, and the default GOTOOLCHAIN=auto
fetches it on its own. rota uses one thing that arrived in it,
`encoding/json/v2`.

```sh
go build -trimpath -ldflags='-s -w' -o rota ./cmd/rota
ln -sf "$PWD/rota" ~/.local/bin/rota      # or anywhere on your PATH
```

A link rather than a copy, so rebuilding is one command and the next `rota`
you type is the one you just built. `go install ./cmd/rota` works too and puts
it in `$(go env GOPATH)/bin`, but then every rebuild needs installing again.

To test a checkout, name the workspace rather than the root:

```sh
go vet work && go test work
```

The program is a module of its own, and `./...` at the root stops at a
module's edge, so `go test ./...` tests the SDK and the server and leaves the
program out. `work` is every package of both modules `go.work` lists.

### On encoding/json/v2

rota reads and writes JSON with `encoding/json/v2`. In Go 1.27 the original
package is still the original implementation — v1 is only rebuilt on top of
v2 under a GOEXPERIMENT — so this is a real change of parser, not a rename.

It is not a drop-in, and `lib/jsonx.go` is the one place that says how rota
handles the six differences that matter here:

| | encoding/json | v2 |
|---|---|---|
| `omitempty` on a number or bool | omits the zero | keeps it |
| `omitempty` on a `RawMessage` | omits when empty in Go | omits when it *encodes* empty, so a deliberate `{}` disappears |
| a nil slice or map | `null` | `[]`, `{}` |
| a map's keys | written sorted | written in whatever order the map hands them out, which changes from run to run |
| object names | matched case-insensitively | matched exactly |
| duplicate names, invalid UTF-8 | last wins, replaced | rejected |

The first four change what rota writes, so every affected field now says
`omitzero`, which means under both packages exactly what `omitempty` used to
mean here, nil slices are still written as null, and rota asks the package
for a map's keys in sorted order (`jsonv2.Deterministic`), so the same value
is always the same bytes. A struct's fields
keep the order they are declared in. `lib/jsonwire_test.go` pins the
resulting bytes, because they are the on-disk account store, the HTTP replies
and what `rota --json` prints.

The last two change what rota can read, and only for JSON that arrives from
somewhere else — a provider's token endpoint, a vendor CLI's event stream, an
API request body. Those keep the old rules, applied in one named place rather
than assumed at nine call sites: tightening them would have rejected replies
that have always been accepted, and the failure would have been a silently
empty field rather than an error.

Be honest about what this bought: on rota's own benchmarks it is a wash —
argv building, event scanning and account rendering all land within noise of
where they were. The reasons to have done it are that v1 is now the legacy
implementation, that rejecting duplicate object names is a better default
than taking the last one, and that v2 does not escape HTML, which is what
every JSON path here already had to ask for by hand.

## The command

```sh
rota "summarize this repo"    # the usual thing: no verb at all
rota 2 "summarize this repo"  # ...on account 2

rota login                    # start a claude login: prints an id and a URL
rota login codex              # ...for another provider
rota login --long             # a year-long token for an account already here
rota login --key              # an account on a Claude Console API key, pasted, not shown
rota login 2 --token          # paste a `claude setup-token` token as account 2's long-lived one
rota login <login-id> <code>  # finish it with the code from the page
rota login <login-id>         # finish a delegated login, which takes no code (grok)
rota login 2                  # sign account 2 in through its own CLI
rota list                     # every account, in rotation order, with usage
rota list --short             # the same, reading no usage from any provider
rota list --sessions          # ...and what is running, and what could resume
rota list claude -r           # one provider, forcing a quota refresh
rota run "summarize this repo"     # ask whichever account the rotation picks
rota run 2 "summarize this repo"   # ask account 2 instead
rota run 2 --stateless "2+2?"      # no session, no settings/memory, throwaway claude home (claude, codex)
rota run 2 "explain it" --with deltas  # each fragment as it is written, not just each finished piece
rota run 2 "explain it" --with raw   # the provider's own events too, as JSON
rota run 2 "explain it" --with blocks,ask   # readings beside the answer: fences split, the question read
rota run 2 -m sonnet -e low "hi"   # every everyday run flag has a short: -m -e -s -c -r -t -S
rota run 2 "start here" --input    # keep the run open and type more messages into it
rota send 7f3c1a5d "and the tests?"  # send into that run from another terminal
rota run                      # open the rotation's account in its own CLI
rota run 2                    # open account 2's CLI, as it comes
rota run 2 --share            # the same, watchable from this machine's terminal page
rota run --share=watch --label api   # ...offered to be read and never typed into
rota set 2 --order 1          # put account 2 first in the queue (0 = out of it)
rota set 2 --threshold 80     # move on to the next account at 80% usage
rota set 2                    # what account 2 is set to
rota set 2 --cwd ~/src/api --config ~/.rota/api-memory
rota set 2 --sessions own     # its conversations are nobody else's (shared by default)
rota set 2 --long forget      # throw away its long-lived token
rota login 6                  # sign in an account whose CLI keeps its own credentials
rota remove 2 5               # forget accounts, and the homes rota made for them
rota serve 8787 --token=T     # serve the HTTP API and its playground
rota serve --config ./server.toml   # one file instead of the flags below
rota serve --print-config     # what it would serve with, and where each value came from
rota serve passwd alice --role control  # a [[users]] block to paste into that file
rota serve token ci --role watch        # a second bearer token, and its [[tokens]] block
rota serve invite --ttl 10m   # ask a running server for a single-use watcher's link
rota serve start              # this store's daemon, whose held sessions outlive every terminal
rota serve status             # whether it runs, where, and how many sessions it holds
rota serve stop               # stop it; its sessions go on running (--all ends them too)
rota version                  # which rota this is
```

### Every command, and every flag it takes

`--json` works before a command or among its flags and turns that command's
output into one machine-readable document. `rota --help` is the paragraph
version of this; `rota <command> -h` is one command's flags with their
defaults. `ls` and `rm` are accepted for `list` and `remove`; `help` is
`--help`; and `rota version`, or `rota --version`, prints the version.

**`rota login [provider | login-id | account-id] [code]`** — one verb for
every way an account is signed in.

| | |
|---|---|
| `--long` | ask for a year-long token for an account already here — see *A token that outlives the window* |
| `--key` | an account of its own on a Claude Console API key, read with echo off from the terminal, or as one line of standard input — see *An account on an API key* |
| `--label <name>` | with `--key`: what to call the account; otherwise it is `api key ...` and the key's last four characters |
| `--token` | after an account id: a token from `claude setup-token`, read the same way, stored as that account's long-lived token |
| `--provider <name>` | the provider, spelled out where a bare name would read as something else |

Neither `--key` nor `--token` takes its credential on the command line, where
every process on the machine and the shell's history could read it: written
there, it is refused without being repeated.

Anything after an account id is passed to the vendor CLI's own login
(`--region mainland-cn`, say).

**`rota list [provider]`**

| | |
|---|---|
| `-r`, `--refresh` | read the quotas now, rather than using the five-minute cache |
| `-s`, `--short` | what is already known, reading no usage; a home that needs rescuing is rescued all the same (see *Stranded*) |
| `--sessions` | also what the CLIs are running and what could be resumed |
| `--all` | with `--sessions`: every conversation, not five per account |

**`rota run [id] [prompt]`** — with no prompt it opens the CLI itself.

| | |
|---|---|
| `-p`, `--print` | the prompt as a flag; also how a prompt that is nothing but digits is written |
| `-m`, `--model` | the model; the provider's default when empty |
| `-e`, `--effort` | reasoning effort, for providers that have one |
| `-s`, `--stream` | print events as they happen rather than one answer at the end |
| `--input` | keep the run open and read more messages from stdin; implies `--stream` |
| `--with <names>` | readings to add beside the answer: a comma list, or the flag repeated |
| `--cwd <dir>` | where the run starts, when the account names none |
| `-t`, `--timeout` | give up after this long |
| `--permission-mode` | how the agent asks before acting |
| `--sandbox` | sandbox profile, for providers that have one |
| `--system-prompt` | instructions added before the prompt |
| `-r`, `--resume [id]` | continue an earlier conversation; on its own, the most recent |
| `-c`, `--continue` | continue the most recent conversation in this directory |
| `--session <id>` | name a new conversation with this id, so it can be resumed later |
| `--fork` | with `--resume`: branch into a new conversation instead of continuing it |
| `--json-schema` | constrain the answer to this JSON Schema |
| `-S`, `--stateless` | no session saved, no settings, memory or rules read (claude, codex) |
| `-v` | also report, on stderr, which account ran and how to resume |
| `--json` | the whole result: cost, usage, session id, exit status |
| `-i`, `--interactive` | open the CLI as it comes instead of asking it anything |
| `--share[=control\|watch]` | open the CLI here *and* offer this terminal to a rota server on this machine |
| `--label <name>` | what to call that shared terminal in the listing |
| `--remote-control[=name]` | open a claude account's Claude Code with its own Remote Control on, turning the account's setting on first — see *Remote Control* |
| `--` | everything after it belongs to the vendor CLI, untouched |

**`rota send <run-id> [text]`** — into a run started with `--input`.

| | |
|---|---|
| `--steer` | interrupt first, so the message starts the next turn |
| `--interrupt` | stop the tool the agent is running, and send nothing |
| `--close` | no more messages: the run finishes its turn and exits |

**`rota set <id>`** — with no flags it prints what the account is set to.

| | |
|---|---|
| `--order <place>` | a number, or `first`, `last`, `up`, `down`, `before:<id>`, `after:<id>`, `0`/`out` |
| `--threshold <pct>` | the usage at which the rotation moves on, 1 to 100 |
| `--cwd <dir>` | where this account's runs start |
| `--config <dir>` | its own CLI configuration, and the private home its credentials are staged in; never another account's home, and for claude not changed while Claude Code runs in the present one. A claude account with one runs on a token there, without Remote Control |
| `--sessions <where>` | where a claude account's conversations live: `shared`, `own`, or a directory |
| `--remote-control on\|off` | a claude account's own `.claude.json`, which Claude Code's Remote Control needs — see *Remote Control* |
| `--long forget` | throw away its long-lived token; the only value the flag takes |
| `--clear` | forget `cwd`, `config`, `sessions` and `remote-control`, back to the defaults |

**`rota remove <id>...`** takes no flags. A claude account's daemon is
stopped first, and on macOS its keychain item goes before its home does.

**`rota serve [address]`** — every flag here can be written in `server.toml`
instead; see *One file for the server*.

| | |
|---|---|
| `--config <path>` | the file to read the rest of this from (default `<store dir>/server.toml`) |
| `--print-config` | print what it would serve with, and where each value came from, then exit |
| `--token <secret>` | the bearer token every request must carry; prefer `ROTA_TOKEN` |
| `--root <dir>` | confine cwd, uploads and extra directories here; repeatable |
| `--timeout` | hard cap on one run |
| `--max-concurrent` | how many CLIs may run at once |
| `--refresh-every` | how often to rotate expiring tokens and re-read usage; `0` turns it off |
| `--tls-cert`, `--tls-key` | the certificate pair; together or not at all |
| `--allow-dangerous` | permit permission-bypass and full-access options |
| `--allow-raw-flags` | let callers pass flags straight to the vendor CLI |
| `--quiet` | log warnings and errors only |
| `--log-file <path>` | log to this file instead of stderr; absolute, rotated at 20 MiB — see *A log file instead of the terminal* |

**`rota serve passwd <name>`** and **`rota serve token <name>`** take
`--role control|watch` and print a block to paste. **`rota serve invite`**
takes `--ttl` (ten minutes by default, a day at most) and `--config`, and
asks a server that is already running for the link. None of the three serves
anything; see *The principals*.

**`rota serve watch <id>`** is internal: started by `rota run`, it keeps the
account's login alive while its CLI runs (see *Watched*). It is not meant to be
typed.

Held sessions run on Linux and macOS:

| | |
|---|---|
| `rota serve start` | start this store's daemon in the background and say its pid and control socket, and its port if it opens one; a daemon already running is named and left alone, and the command exits `0` either way — see *[The daemon](#the-daemon)* |
| `--foreground` | be the daemon in this process instead, as a supervisor runs it: it logs to its file and says nothing on stderr |
| `--ctl-only` | serve the control socket alone, whatever `server.toml` says about a port |
| `--config <path>` | the file to read the rest of this from (default `<store dir>/server.toml`) |
| `rota serve stop` | stop the daemon; every session it holds goes on running, for the next daemon to take up. Refused, with or without `--all`, inside a held session |
| `--all` | end every session as well, after asking `stop the rota daemon and end N sessions? [y/N]`; with no daemon running, end the sessions its holders still hold |
| `--yes` | with `--all`, end them without asking; off a terminal `--all` needs it |
| `rota serve status` | whether a daemon runs, its pid, version, control socket and port, and how many sessions are running and stopped; `--json` for the same as a document. Exits `0` when a daemon runs and `3` when none does |
| `rota serve hold` | internal: one held session's process, started by the daemon with its request on standard input; refuses a terminal |

The whole `[routes]` and `[terminal]` groups — the terminal page among them —
have no flag at all and are set in the file only.

### The rotation

Every account holds a place in a queue and a threshold. A request that names
no account takes the first account in the queue that is neither spent nor
dead; when it passes its threshold the next one takes over.

```sh
rota set 3 --order 1              # account 3 goes first
rota set 2 --order 2              # then account 2
rota set 5 --order 0              # account 5 sits it out — still runnable by id
rota set 3 --threshold 80         # stop spending 3 at 80% and move to 2
rota set 3 --order 1 --threshold 80   # or both, in one write
```

The order is a plain number starting at 1, so inserting an account between
two others is one command rather than a re-shuffle. **0 is not a position**:
it keeps an account out of the queue entirely, which is how a spare, a
personal account or a broken one stays registered without ever being picked.
The default threshold is 100 — spend an account fully before moving on —
which is deliberate: spreading work evenly across accounts arrives at all of
them being half-spent at once, and the reason to hold several is to have one
that is still whole.

Usage is read from the cached quota, refreshed if older than five minutes
or once a window it reported has reset, and only for the accounts the queue
actually looks at: an account spent until five is picked again at five, not
when the cache runs out. A provider that publishes no usage endpoint reports
nothing, so it is treated as unspent rather than dropped — otherwise an
unmetered account could never be picked.
`rota list` marks the account a bare `rota run` would use.

Accounts added before rotation existed are numbered by id the first time they
are loaded, once; after that, an account left at 0 is a decision and nothing
renumbers it.

The verb is optional for the one thing you do most: `rota "..."` is
`rota run "..."`, and `rota 2 "..."` names the account. Only an argument that
could not be a command is read that way — one with a space in it, or one after
`-p` — so a mistyped `rota lst` stays an error rather than becoming a question
you are charged for.

**`rota run [id] "<prompt>"`** asks one account and prints the answer. rota
speaks its own vocabulary — `--model`, `--effort`, `--stream`, `--resume` —
and builds the right headless command for whichever CLI the account belongs
to, because knowing that Claude Code wants `-p`, Codex wants its `exec`
subcommand and Grok Build wants a prompt file is rota's job, not yours. It is
never interactive: a CLI that would otherwise stop to ask about the directory
or a permission is given the flags that make it answer instead.

In text mode the answer is the only thing printed — nothing to filter out of
a pipe. `-v` adds which account ran and how to resume, on stderr; `--json`
replaces the output with the full result: cost, usage, session id and exit
status.

```sh
rota run "what does this package do?"    # the rotation chooses
rota run 1 "and the tests?" --resume 30040947-e103-4d58-8b0d-46417297cb1b
rota run 1 "..." -v                      # which account, and how to resume
rota --json run 1 "..." | jq -r .cost_usd
```

The prompt is positional, so nothing needs a flag. A leading bare number is
read as the account id, which is the one ambiguity: to ask a question that is
nothing but digits, write it as `rota run -p 42`.

Conversations carry on: every answer has a session id, `--resume` continues
from it, `--continue` picks up the most recent one in this directory, and
`--fork` branches off instead of adding to it. The two CLIs spell forking
differently; rota does not make you care.

Two escape hatches remain for when rota's vocabulary is not what you want:

```sh
rota run 1                        # no prompt: open the CLI itself, as it comes
rota run 1 -i                     # the same, said explicitly
rota run 1 -- --any --vendor-flag # hand it these arguments, untouched
rota run 1 --share                # the same, and watchable from this machine's page
```

The id is a number rather than a name so that it can never be mistaken for
one of a CLI's own flags, which is not true of any short alias.

`--share` is the same handover with rota left standing above it, so that the
terminal can also be watched from a rota server on this machine — see
*Sharing the terminal you are sitting at*.

`run` never calls a usage endpoint — it only refreshes the token when one is
about to expire — so a scripted run stays fast and cannot exhaust the
per-account quota budget. The answer goes to stdout and nothing else does;
with `-i` or `--`, stdout belongs to the CLI. Either way the CLI's exit
status becomes rota's.

Login is one command and never opens a browser. `rota login` prints a URL and
a short id; open the URL yourself, approve, then pass the code back with that
id.

One command, because the difference between the two it used to be — whether
rota holds the credential or the vendor CLI keeps its own — is rota's business
rather than yours. rota knows which an account needs, so the argument decides:
a provider name starts a new account, a login id finishes one, and an account
id hands that account to its own CLI's login. Opening the URL yourself is also
what makes a *second* account possible: a
normal browser window reuses the session already signed in, so use a private
window for the next one. Several logins can be open at once, across providers
as well as within one; a rejected code costs one retry, not a whole login.

## The HTTP API

```sh
rota serve --token=$(openssl rand -hex 32)      # 127.0.0.1:8787
rota serve 8787 --token=T --root /srv/work      # a bare port means 0.0.0.0
ROTA_TOKEN=T rota serve                          # keeps it out of the process table
rota serve --config /etc/rota/server.toml        # or write it all down once
```

The token is mandatory and is checked in constant time. Ten bad tokens from
one address within an hour block that address for an hour. It is one
principal among several — see *Who is asking* below for the people and the
other tokens a file may name, each with a role. Prefer
`ROTA_TOKEN` to `--token`: a command line is in the process table, where
every process on the machine can read it — including the agents this server
starts, which have a shell. The server warns once at startup when it sees
the flag.

A running server keeps itself current: every two minutes it rotates any
access token close to expiring and re-reads the usage whose cached value has
aged past five minutes, and a few seconds after a usage window resets it
reads that account again rather than waiting for the next sweep. A request
should never be the thing that discovers its credential expired, and the
rotation decides from stored numbers — a stale one sends work to an account
that is already spent. Nothing there is fatal: a provider that cannot be
reached leaves its account exactly as it was, and the next sweep tries
again. `--refresh-every 0` turns it off, and the command line still
refreshes what it is about to use.

### One file for the server

Everything above can be written down once instead of typed every time.
`rota serve` reads `server.toml` from the store directory — `$ROTA_HOME/
server.toml`, or `~/.rota/server.toml` — and `--config PATH` reads another.
The default file is allowed not to exist, and then nothing has changed; a
file somebody names has to be there, because naming one that is not is a
typo nobody would otherwise be told about.

The file may hold the bearer token, so on unix rota refuses to start if
group or others can read or write it, and says the command that fixes it.
Windows has neither those bits nor that convention, so there is no such
check there:

```
/home/me/.rota/server.toml is readable or writable by other users (mode
0644); it is its owner's alone, so fix it with: chmod 600
/home/me/.rota/server.toml
```

**A flag beats the environment, the environment beats the file, and the file
beats the default.** A flag counts only when it was actually typed: its own
default is just another way of spelling the default. `ROTA_TOKEN` beats a
token written in the file.

`docs/server.toml` in this repository is the whole schema with every key at
its default and a line of comment each; a test loads it and insists it comes
out equal to the defaults, so it cannot drift. In short:

```toml
[server]
listen   = "127.0.0.1:8787" # host:port, or a bare port for every interface
quiet    = false            # log warnings and errors only
log_file = ""               # an absolute path to log to instead of stderr

[tls]
cert = ""                   # cert and key go together or not at all
key  = ""

[auth]
token       = ""            # the bearer token, if you keep it here
token_file  = ""            # or a file holding it, same permission rule
token_env   = "ROTA_TOKEN"  # or the name of the variable that holds it
session_ttl = "12h"         # how long a sign-in on the page lasts
local_user  = ""            # the [[users]] entry this machine's own terminal types as

[routes]
api        = true           # everything under /v1 except the sockets and health
playground = true           # GET / and GET /playground
websocket  = true           # the /ws routes
health     = true           # GET /v1/health, open and depending on nothing
terminal   = false          # /v1/terminals, GET /terminal; off until asked for

[terminal]                  # nothing here means anything unless routes.terminal
shell            = false    # also allow a plain login shell, not only CLIs
max_sessions     = 8
scrollback_bytes = 2097152  # output kept per terminal, for whoever attaches
idle_timeout     = "12h"    # a terminal nobody is attached to ends after this
keep_stopped     = "168h"   # how long a stopped held session stays listed
record           = false    # keep each terminal's output under <store>/terminals/
record_max_bytes = 52428800 # how much of one terminal is kept; input never is
share            = true     # take terminals offered by `rota run --share` here

[runs]
timeout         = "10m"     # hard cap on one run
max_concurrent  = 8         # how many CLIs may run at once
input_timeout   = "1h"      # hard cap on a run that stays open
input_grace     = "1m"      # how long an open run survives unread
replay          = 1000      # events kept for a reader that reattaches
refresh_every   = "2m"      # "0s" turns the background sweep off
roots           = []        # confine cwd, uploads and extra directories here
allow_dangerous = false
allow_raw_flags = false

[store]
dir = ""                    # empty is $ROTA_HOME or ~/.rota
```

Much of that has no flag at all and never will: the whole of `[routes]` and
`[terminal]`, all of `[auth]` but the token itself, `store.dir`, and the
three keys a run that stays open is governed by — `input_timeout`,
`input_grace` and `replay`, which were configurable from the library alone
until this file existed. A command line is for what changes between one
server and the next run of it; who may sign in, and whether this server
holds terminals, is not that.

`[routes]` switches whole groups on and off. A group that is off is not
registered, so its paths answer `404` exactly as a path this server never
had would: nothing tells a stranger that a door is there but shut. What each
group needs is a table rather than a chain of conditions, so a group added
later is a row in it:

| group | needs | because |
|---|---|---|
| `api` | — | |
| `playground` | `api` | the page has nothing to call without it |
| `websocket` | `api` | a socket is the same run by another door |
| `health` | — | a probe that goes off with the page is not a probe |
| `terminal` | `api`, `websocket` | a terminal is started over the one and carried on the other |

`terminal` is the one group that is off in the defaults, and the one with a
rule about the address: with it on and a listen address that is not the
loopback, `tls.cert` and `tls.key` are required, because a control sign-in on
that port runs commands on this machine. See *A terminal on the server*.

Asking for one of the middle two without `api` is refused by name. With
`websocket = false` the playground is told so by `/v1/schema` and stops
offering a run that stays open, rather than opening a socket at nothing.

`rota serve --print-config` prints the whole schema as TOML and exits
without serving, each line saying where its value came from — `# default`,
`# file`, `# env ROTA_TOKEN`, `# flag --timeout`, `# argument`. No secret is
printed, only whether there is one: every field the schema marks `secret` —
the token, a user's password, a token entry's digest — comes out as
`"(set)"`, so the output can be pasted where the file itself could not. With
nothing secret set it is a file rota reads straight back; with something set
it says at the top that it is a report and not a file to load.

rota has no database, and this file configures none. What persists is the
store directory: the accounts, the homes rota stages for them and, now, this
file.

Every route this server can register is here, with the role it takes. `open`
means no credential at all — the page, the assets, the health probe and the
three doors a sign-in goes through, which cannot be asked to prove who they
are first. Everything else is `watch` (reading) or `control` (changing);
see *Who is asking, and what they may do*.

| Method | Path | Role | |
|---|---|---|---|
| `GET` | `/v1/health` | open | never rate-limited: `{"ok":true}` and nothing else. Its own group, so it survives switching the page off |
| `GET` | `/` | open | what this is and its version. With the rest of the `playground` group |
| `GET` | `/playground` | open | the playground, a page over the API |
| `GET` | `/terminal` | open | the terminal page. With the `terminal` group, not this one: a server may serve either page without the other |
| `GET` | `/assets/{file}` | open | what both pages are made of in common — `page.css` and `page.js`; present wherever either page is |
| `GET` | `/assets/xterm/{file}` | open | the vendored terminal emulator, with the `terminal` group |
| `GET` | `/invite/{code}` | open | spends a single-use link and lands on a page — the terminal where there is one, the playground otherwise |
| `GET` | `/v1/session` | open | who this request is: `{name, role, via, expires}`, or `401` |
| `POST` | `/v1/session` | open | `{"name":"…","password":"…"}` → the same, and the session cookie |
| `DELETE` | `/v1/session` | open | end this session |
| `GET` | `/v1/schema` | watch | every provider, its models, efforts, defaults and fields |
| `GET` | `/v1/accounts` | watch | accounts in rotation order, with usage, status, order, threshold, `long_until`, `login_until`, `no_credit_until`, `remote_control` and when limits were read (`?refresh=1`); `default` names the one a bare run would use |
| `GET` | `/v1/accounts/{id}/schema` | watch | the models *that* account may actually use |
| `POST` | `/v1/run` | control | run a prompt on whichever account the rotation picks |
| `POST` | `/v1/accounts/{id}/run` | control | run a prompt on that account |
| `GET` | `/v1/runs` | watch | the runs staying open right now, and the ones that have just ended |
| `GET` | `/v1/runs/{id}` | watch | one of them: its account, state, how many messages are waiting, whether anyone is reading |
| `GET` | `/v1/runs/{id}/events` | watch | attach to its stream, `?since=N` replaying what was missed |
| `POST` | `/v1/runs/{id}/messages` | control | `{"text":"…","steer":false}` — one more message into it |
| `POST` | `/v1/runs/{id}/interrupt` | control | stop the tool it is running |
| `POST` | `/v1/runs/{id}/close` | control | no more messages: it finishes its turn and exits |
| `GET` | `/v1/ws` | control | a WebSocket that starts a run on whichever account the rotation picks and carries it both ways |
| `GET` | `/v1/accounts/{id}/ws` | control | the same, on that account |
| `GET` | `/v1/runs/{id}/ws` | watch | attach a WebSocket to a run already going, `?since=N` replaying what was missed |
| `GET` | `/v1/terminals` | watch | the terminals running right now, and the ones that have just ended; a held session, in a process of its own under the rota daemon, adds `"held":true`, `state` (`live`, `ended`, `interrupted` or `broken`), `attachable`, `reachable`, `pid` (its holder), `name`, `title`, `conversation`, `conversation_cwd`, `resumable`, `ended_at` once it has stopped, `made_by` for a holder this rota cannot attach to (`unknown` when its record does not say), and `status` (`running`, `needs_you`, `quiet` or `stopped`). The daemon also lists the held sessions its store's records describe: the stopped ones, for `keep_stopped`, and the running ones it is not linked to |
| `GET` | `/v1/terminals/{id}` | watch | one of them: what it runs, who is watching, who holds the keyboard, and the same held fields, for a stopped held session too |
| `POST` | `/v1/terminals` | control | `{"account":1,"args":[],"cwd":"…","cols":120,"rows":40,"label":"…"}` — start one; `503` when the server is stopping as it starts, and a held session started by then goes on running for the next daemon to take up |
| `DELETE` | `/v1/terminals/{id}` | control | end one: a hangup, then a kill three seconds later, or five for a held session, whose holder kills its own CLI at three and records how it ended; a running held session the daemon is not linked to is hung up by signal, once however often it is asked, and only while its pid is still the holder its record names: `409` when it is not, or when its record names no process this rota can check |
| `GET` | `/v1/terminals/{id}/ws` | watch | attach to one, `?since=N` in bytes and `?mode=watch` to look without taking the keyboard; what may be sent *back* in is decided per frame. A held session the daemon is not linked to answers `409`, saying why |
| `POST` | `/v1/terminals/{id}/send` | control | `{"text":"…","force":false}` — type a message into a held session the way a person would, once it is ready for one: the text, then Enter. It is typing, so the keyboard must be free or held by the sender's own person, and until it is typed nobody else types into the session but one Esc or Ctrl-C. `200 {"ok":true}` once typed; `409` with `reason` `waiting` (the CLI waits on its person, where Enter may approve something; `"force":true` types anyway), `keyboard` (with `holder`), `ended`, or `not_held` (a terminal that is not a held session, or one the daemon is not linked to); `504` with `reason` `timeout` when it never became ready in 30 seconds |
| `PATCH` | `/v1/accounts/{id}` | control | `{"order":1,"threshold":80,"cwd":"/srv/api","config_dir":"/srv/homes/api","sessions":"own","remote_control":true,"long":"forget"}` — its place in the rotation, when to move on, where it belongs, where its conversations live (`shared`, `own`, or a directory — confined exactly as `config_dir` is), whether a claude account keeps its own `.claude.json` for Claude Code's Remote Control, and `"long":"forget"` to throw away its long-lived token |
| `DELETE` | `/v1/accounts/{id}` | control | forget it, and delete the home rota made for it, staged credentials included; a `config_dir` somebody chose holds their memory and skills and stays, but for the account's login in it. A claude account's daemon is stopped first, and its macOS keychain item removed before its home |
| `POST` | `/v1/login` | control | `{"provider":"claude","long":false}` → `{id, url, kind}`; `"long":true` asks for a long-lived token instead. `{"key":"sk-ant-api…","label":"…"}` makes an account on an API key at once → `{id, status, credential, order, label}`, with `warning` when the key could not be checked; `{"account":1,"token":"sk-ant-oat…"}` stores a `claude setup-token` token as that account's long-lived one → `{"long_until":"...","status":"long"}`, and an API key sent as `token` is refused with `400` by its prefix, without being sent anywhere. Neither comes back or is logged |
| `POST` | `/v1/login/{id}` | control | `{"code":"..."}` → the account, or `{"status":"pending"}`; a long login answers `{"long_until":"...","status":"long"}` |
| `POST` | `/v1/auth` | control | what `POST /v1/login` was called before the API and the command agreed on one word; kept working for anything already calling it |
| `POST` | `/v1/auth/{id}` | control | likewise for `POST /v1/login/{id}` |
| `POST` | `/v1/invites` | control | `{"ttl":"10m"}` → `{"url","expires"}` — a single-use link that signs somebody in as a watcher |

The `/v1/terminals` and `/assets/xterm/` rows exist only where
`routes.terminal` is on, and the pages and `/assets/` only where a page is
served. A group that is off is not registered, so its paths answer `404`
like any path this server never had.

```sh
curl -H "authorization: Bearer $T" -X POST localhost:8787/v1/accounts/1/run \
  -d '{"prompt":"summarize this repo","model":"sonnet","effort":"low"}'

curl -H "authorization: Bearer $T" -X POST localhost:8787/v1/run \
  -d '{"prompt":"summarize this repo"}'          # the rotation chooses
```

Both replies carry `"account"`, so a caller that left the choice to the
rotation still learns which account answered — including a streamed one,
whose terminal `done` event names it too. An empty rotation answers `409`
rather than `404`: the accounts exist, none of them is available. A model
belongs to one provider, so pinning one to a request that has not chosen a
provider yet is refused if the rotation lands somewhere else — leave `model`
out, or name the account.

`{"stream": true}` switches the reply to Server-Sent Events; ask for
`Accept: application/x-ndjson` to get one JSON object per line instead.
Closing the connection stops the CLI and everything it started: on Linux and
macOS it is asked with SIGTERM and killed three seconds later if it has not
gone, because Claude Code rotates its refresh token and a process killed in the
middle of saving one loses the login. What travels
down it is rota's vocabulary, not the vendor's — see below.

Files can travel with the request — either `"files": [{"path":"a.txt",
"content":"<base64>"}]` or a multipart body whose `request` part is that same
JSON. They land in a directory private to the request, which is added to the
session and deleted afterwards.

### A log file instead of the terminal

A server logs to stderr. `log_file` in `[server]`, or `--log-file PATH`,
sends every line it writes there instead — the start-up line and its
warnings, each request it logs, the http server's own errors — so it can run
with nothing on the terminal. Like `quiet`, it is the file's key and a flag,
and no environment variable. `quiet` still decides how much is written. The
lines are the ones stderr would have shown, except that the server's own
plain lines — the start-up line, the warnings, `routes:` and `shutting
down` — begin with an RFC 3339 time in the file, as slog's lines carry
`time=`, so a restart in a log that is appended to can be dated. Without a
log file, stderr is exactly what it always was.

The one exception is a server that fails to start or stops on its own: the
reason goes into the file and to stderr as well, so a failure is never
silent. Anything refused before the file is open — the configuration, the
file itself — is said on stderr alone, there being no log yet to put it in.

The path must be absolute and its directory must already exist. It is
opened for appending, so a restart adds to it, and made with mode 0600.
Every time it is opened — at the start, and again after each rotation — a
link at the path is refused rather than followed and a FIFO there is
refused at once rather than waited on (both on unix), and what was opened
must be a plain file; on unix it must also be its owner's alone, a file
group or others can read or write being refused the way `server.toml` is.
Windows has no such bits, so the mode is not checked there.

```
server.log_file: /var/log/rota/rota.log is readable or writable by other
users (mode 0644); it is its owner's alone, so fix it with: chmod 600
/var/log/rota/rota.log
```

When a write would take the file past 20 MiB it is moved to `rota.log.1`,
the older ones move up by one, the fifth goes, and a new file is started:
four old files are kept, the newest as `.1`, so a server whose rotations
succeed fills a hundred mebibytes at most. The log is moved first and the
older files only after it: if it cannot be moved, nothing older is touched,
writing goes on into the same file, which keeps growing, and the next try
waits for another 20 MiB. A `rota.log.0` or `rota.log.folding` that a crash
left in the middle of a rotation joins the chain as the newest old files
when the log is opened, and before the next rotation, rather than written over; one that
cannot be moved, such as another user's in a shared directory, is left where
it is and costs no older file. If the new file cannot be opened after a
rotation, the log goes to stderr until it can: the file is tried again
after a minute, or once another 20 MiB has gone to stderr, whichever is
first, and each switch, either way, is said once on stderr. A server that
then stops with an error says it there once, not twice.

One log file belongs to one server. Two servers given the same path each
rotate it on their own count, and one may move the file out from under the
other.

Nothing new is logged for having a file, and none of the lines holds a
token or a password.

### The daemon

```sh
rota serve start              # in the background, for the store ROTA_HOME names
rota serve start --ctl-only   # ...on its control socket alone
rota serve status             # exits 0 with a daemon, 3 without
rota serve stop               # the daemon goes; its sessions stay
rota serve stop --all --yes   # the sessions go too
```

A store can have one daemon: a server that holds its sessions in processes
of their own, which outlive it, and that this machine's commands reach over
its control socket. `rota serve` in the foreground is unchanged by it, and
the two differ like this:

| | `rota serve` (foreground) | the daemon |
|---|---|---|
| Started by | a person, as always | `rota serve start`, or a supervisor running `rota serve start --foreground` |
| Runs | with its flags and defaults, unchanged | `rota serve start --foreground`, with the file's settings and no flags but `--ctl-only` and `--config` |
| Sessions | terminals in its own process; stopping it ends them | held sessions, one holder process each, which outlive it |
| TCP | `127.0.0.1:8787` by default, with a token | only where `server.toml` sets `[server] listen`, and never with `--ctl-only` |
| `ctl.sock` | no | always |
| Token | `--token`, `ROTA_TOKEN` or the file, and it will not start without one | `ROTA_TOKEN` or the file's, or else 32 random bytes held in memory and never written |
| Logs | stderr, or `log_file` | `<store>/daemon.log`, or `log_file`; never stderr |

**Starting.** `rota serve start` runs `rota serve start --foreground` in a
session of its own, in `/`, with its standard streams on `/dev/null` and
`ROTA_HOME` set to the store's absolute path, so a store named relatively
is still the one meant. It reads the configuration first, as the daemon
will, so a file the daemon would refuse is refused at the terminal. Then it
waits up to ten seconds for the daemon to answer on its control socket and
says

```
rota daemon started for /home/me/.rota (pid 4242); control socket /home/me/.rota/terminals/ctl.sock; http on 127.0.0.1:8787
```

the last part only with a port. A daemon that stops before it answers is
said with the last five lines it wrote to its log, and the exit is `1`. A
daemon already running is `rota daemon already running for <store> (pid N)`
and exit `0`, whether it was found before anything started or won the store
from this start's own daemon, which then stops, saying in the log that
*another rota daemon serves* the store: two starts at once make one daemon,
and both exit `0`. Once started, a daemon takes up every session still
running on its store, as one restarted would.

A daemon serves the store it is started for, which is the one `serve stop`
and `serve status` look at, so a `server.toml` whose `[store] dir` names
another store is refused; start it with `ROTA_HOME` set to that one instead.
A daemon runs in `/`, so a relative `[runs] roots` entry would name a folder
under `/`: in daemon mode one is refused, *runs.roots: "work" is relative; a
daemon runs in /, so every root must be absolute in daemon mode*.

The holders it starts are `rota serve hold` from the path rota was started
by — links kept as typed, found on `PATH` as the shell found it — resolved
once at start. A rota replaced in place at that resolved path starts new
holders from the new binary. An upgrade that removes it, as a versioned
install does when it moves its link on, is followed: the path rota was
started by is resolved again, and new holders come from where it leads now.

**While it runs** it logs to `<store>/daemon.log`, or to `[server] log_file`,
rotated as *[A log file instead of the
terminal](#a-log-file-instead-of-the-terminal)* says, with every line
carrying its `pid`. A port it opens is bound, and its certificate read,
before it says it has started, and the warnings foreground serve prints for
a port go into the log. `daemon.pid` names it, and the port it serves on,
for `serve status` to read. A hangup does not stop it.

**Stopping.** Run from inside a held session — by the agent in one, or by
anything it started — `rota serve stop` is refused, with or without `--all`:
*rota serve stop is refused inside held session <id>; run it from outside
the session*. It tells by the same rule the control socket lets such a caller
only watch by, and is the same guard against accidents, not a wall.
Elsewhere it sends the daemon SIGTERM — only while its pid
is still the process `daemon.pid` names — and waits up to fifteen seconds for
it to let go of the store. Within ten seconds the daemon closes its port and
its control socket, giving what they are serving all but the last second to
finish; then it ends every WebSocket either of them handed over, each told
`1001` *the server is stopping*, lets go of every session, still running,
and lets go of the store last; the last second is for those goodbyes to go
out. It says `rota daemon stopped; N sessions keep running`. With no daemon
it says `no rota daemon is running for <store>` and exits `0`.

`rota serve stop --all` ends the sessions as well. At a terminal it asks
first; anything but `y` or `yes` stops nothing and exits `1`, and off a
terminal it is a usage error without `--yes`. Each running session is asked
to end over the control socket — a hangup, and a kill five seconds later for
a holder still there — and waited for until its lock is free, which its
holder lets go of only once its CLI has gone; then the daemon is stopped:
`rota daemon stopped; N sessions ended`. With no daemon running, each
holder still running is hung up directly, by the same rule, and killed if
its lock is still held five seconds later. A session that would not end is
named, with why, and the exit is `1`.

**Status.** `rota serve status` reads `daemon.pid` for the daemon and asks
it for its sessions, or with no daemon reads their records. A daemon that
holds the store and does not answer within five seconds is said as that,
and is an error, never *no daemon*. With `--json`:

```json
{
  "running": true,
  "pid": 4242,
  "version": "1.4.0",
  "started": "2026-11-07T10:00:00Z",
  "store": "/home/me/.rota",
  "ctl": "/home/me/.rota/terminals/ctl.sock",
  "http": null,
  "sessions": {"live": 2, "stopped": 1}
}
```

`http` is the address of its port, or `null`; with no daemon, `running` is
`false`, `pid` is `0`, `version` is empty and `started` is `null`.

### Who is asking, and what they may do

Every request is made by a **principal**: a name to put in the log, a role
that decides what it may ask for, and how it arrived. A request is resolved
to one in that order — a bearer token first, then the session cookie — and a
request with neither is `401` in the words it has always been.

There are two roles and there is no third. `watch` may read everything and
change nothing; `control` may do anything this server does, which includes
running a coding agent on this machine with a shell. A role between them
would have to be decided route by route, which is what the table below is
instead of.

| | `watch` | `control` |
|---|---|---|
| every `GET` under `/v1` — accounts, usage, the runs, a run's event stream | yes | yes |
| attach to a run over a WebSocket, read-only | yes | yes |
| the page | yes, read-only | yes |
| start a run, over HTTP or a socket | no | yes |
| message, steer, interrupt, close a run | no | yes |
| `PATCH` and `DELETE` an account, sign one in | no | yes |
| make an invite | no | yes |
| list the terminals, and attach to one | yes, read-only | yes |
| start or kill a terminal, and hold its keyboard | no | yes |

That is one table in the code as well — route to role, consulted by the
middleware, checked against the routes actually registered by a test — which
is why the terminals are rows in it rather than checks scattered through
handlers, and why the frames a terminal's socket takes are a second table
beside it. A route nobody wrote a row for takes `control`.

A principal whose role does not cover what it asked for gets `403` and the
sentence *this sign-in may only watch; nothing here can be changed from it*.
It is not `401`, because there is nothing to try again with, and it is not
counted as a guess: somebody who proved who they are and clicked the wrong
thing must not cost their address its access.

#### The principals

The token `rota serve` is started with — `--token`, `ROTA_TOKEN`,
`[auth] token`, `token_file`, `token_env` — is the principal `token`, with
the role `control`. Nothing about it has changed. The daemon's control
socket has one more, `local`, which no request names; see
*[The control socket](#the-control-socket)*.

Beside it the file may name people and more tokens:

```toml
[[users]]
name     = "inoyat"
role     = "control"
password = "pbkdf2-sha256$600000$<salt, base64>$<hash, base64>"

[[tokens]]
name   = "ci-watch"
role   = "watch"
sha256 = "<hex of sha256(the token)>"
```

Names are unique within each table and non-empty, and no user or token is
called `local`, which is the control socket's own; a role is one of the two
words; a password must parse, with at least 100 000 iterations, a salt of at
least 16 bytes and a 32-byte hash; a digest is 64 hexadecimal characters.
Every refusal names the entry — `[[users]] "inoyat": password: …` — because
whoever reads it is looking at a file with several of them in it.

**rota never writes this file.** A program that edits a file somebody else
also edits has to merge, and a merge of the file that decides who may reach
a server is a class of bug nobody needs. Three commands print instead:

```sh
rota serve passwd inoyat --role control   # asks twice, echoes nothing, prints [[users]]
rota serve token ci-watch --role watch    # prints the token once, and [[tokens]]
rota serve invite --ttl 10m               # asks a running server for a watcher's link
```

`serve passwd` turns the terminal's echo off with one `ioctl` — `TCGETS` on
Linux, `TIOCGETA` on the BSDs and macOS, in two small build-tagged files,
because the standard library is the whole dependency list. Where it cannot,
it says so before asking rather than silently showing what is typed. With
standard input redirected it reads one line, which is how a script uses it.

`serve token` prints the token once and nowhere else: the file holds only
its SHA-256, so a `server.toml` somebody reads gives them nothing to send. A
lost token is replaced, not recovered.

#### Signing in on the page

| Method | Path | |
|---|---|---|
| `GET` | `/v1/session` | the current principal `{name, role, via, expires}`, or `401`. The page asks this first |
| `POST` | `/v1/session` | `{"name":"…","password":"…"}` → the same, and the cookie |
| `DELETE` | `/v1/session` | ends the session and clears the cookie |

The cookie is `rota_session`: `HttpOnly`, `SameSite=Strict`, `Path=/`, and
`Secure` exactly when the request that earned it arrived over TLS — marking
it `Secure` on a plain `http` server would mean the browser never sent it
back. Its value is 32 random bytes and never appears in a response body.

Sessions live in memory and **die with the server**. That is a decision, not
a gap: rota has no database, the alternative is a file of live credentials
on disk, and a server that restarts has usually just been upgraded or
reconfigured. Expiry is absolute, `auth.session_ttl` from the moment of
signing in, and expired rows are dropped as they are passed rather than by a
sweeper nobody is paying for.

A sign-in that fails answers one sentence — *that name and password do not go
together* — whether the name is unknown or the password is wrong, and costs
the same PBKDF2 derivation either way: an unknown name is checked against a
decoy at the same cost, so the time an answer takes cannot be read as "that
name exists". Failures go through the same per-address block as bad tokens:
ten within an hour and that address gets `429` for an hour, the right
password included, because the alternative is an unlimited password oracle.

**Cross-site.** A request that changes something and is authorized by a
cookie must carry an `Origin` — or, failing that, a `Referer` — whose host is
this server's own, or it is `403` with *a request from another site cannot
use this session*. `SameSite=Strict` already stops the cases browsers agree
on; this is the second lock, and it costs nothing because a browser puts
`Origin` on every request that is not a plain navigation. A WebSocket
upgrade authorized by a cookie is held to the same rule: any page on the
internet may open one, and the browser would attach the cookie. Requests
authorized by a bearer token are exempt — nothing attaches one to a request
but the code that meant to make it — and so is everything on the control
socket, which no page can open.

#### Invite links

| Method | Path | |
|---|---|---|
| `POST` | `/v1/invites` | (`control`) `{"ttl":"10m"}` → `{"url","expires"}`; ten minutes by default, a day at most |
| `GET` | `/invite/{code}` | unauthenticated; spends the code, signs the visitor in as a watcher, redirects to `/terminal` where that group is on and to `/playground` otherwise |

A code is 32 random bytes and one use: spending it is taking it out of the
table, so two browsers racing the same link cannot both get in. The session
it makes is `watch`, named `invite-` and six characters of the spent code —
enough to tell one visit from another in the log, and not enough to be the
code. A link that has been used, has expired, or was invented answers `404`
as a plain page and counts as a guess, because guessing one is the only way
in that needs no password. The code itself is never logged.

`rota serve invite` is a client, not a server: it reads the same
configuration the server did to find the address, the TLS settings and a
control token, calls `POST /v1/invites` on whatever is listening there, and
prints the URL. With no control token configured it says so and stops.

#### Health

`GET /v1/health` answers `{"ok":true}` to anybody, with no rate limit, and
says nothing else — not even a version, because whoever can reach it has
proved nothing. It is its own route group so that a server with its page
switched off, which takes `GET /` with it, still has something a watchdog
can read.

#### The control socket

The rota daemon — a server holding a store's held sessions — also listens on
a unix socket, `<store>/terminals/ctl.sock`. It is the daemon's API for this
machine's own user: what a command on this machine talks to.

**Who reaches it.** The socket is `0600` in a directory that is `0700` — a
terminals directory somebody opened to others is closed again before the
socket is made, and one the daemon cannot close is refused — and a
connection from any user but the daemon's, as the kernel names it, is
closed before it is served. That is its whole authorization. Nothing on a
request is read to find out who is asking — no token, no cookie, no
`Origin`, and no header ever names anybody — and a wrong token there is not
a guess. The `Origin` rule is not applied there at all, whoever the
principal is: it protects a cookie from a page, and no page can open a unix
socket. Every request is the principal `local`, `"via":"local"`, and the
role table decides what it may do, as for anyone.

**What it serves.** Every route that takes a principal — the API, the
WebSockets, and the terminals with their `send` — whatever `[routes]` says:
routes choose what a port opens, and this is not a port. Beside those it
answers `GET /v1/health`, and `GET /v1/session` with
`{"name":"local","role":…,"via":"local"}`, so a client can ask whether it
may change anything before it tries. There is no page, asset or invite
landing, and no signing in or out: nothing that reaches a unix socket is a
browser. An invite made over the socket links to the `Host` the client
sent, which on a socket is whatever it wrote, so put the port's address in
its place before handing the link on.

**Inside a held session it only watches.** A caller is `local` with
`control`, unless it is running inside a held session — the agent in it, or
anything that agent started. Then it is `local` with `watch`: it may list,
describe and attach, every change is `403` *this sign-in may only watch;
nothing here can be changed from it*, its typing is refused in the same
words, and it never holds a keyboard. The daemon tells by the caller's
session, which the caller cannot say for itself: the kernel gives the pid at
the other end of the socket, `getsid` the session that pid is in, and a
session led by the holder or the CLI of any session whose lock is held is
inside one. A caller the daemon cannot place is given `watch` as well. The
watch-only rule is a guard against accidents, not a wall: a process that
calls `setsid` leaves its session and escapes it.

**At the keyboard** `local` is a person, as a user signed in on the page is
(*[Roles, and the keyboard](#roles-and-the-keyboard)*): two clients of the
socket are one person on two devices, and with `[auth] local_user` — or a
single `[[users]]` entry that may control — it is that user's device. That
is why no user or token may be called `local`: either would read as the
socket wherever a name is shown. A file with one is refused at start-up
with *`[[users]] "local"`: the name "local" is reserved for the control
socket's own principal; choose another*, and `rota serve passwd local` and
`rota serve token local` are refused in the same words.

**Only the daemon listens there.** The socket is opened by the process
holding the store's daemon claim, and by no other. Holding it, the daemon
removes whatever a daemon that crashed left at the path before it listens; a
server without the claim is refused and leaves the file alone, since it may
be a live daemon's. A path longer than a hundred bytes is refused by name,
since a unix socket's address cannot hold it. Stopping the daemon removes
the socket — before it lets go of the claim, and only while the file at the
path is still that socket, so a daemon that took the store over is never
left without its own. It runs on linux and macOS, as the daemon does.

#### What is written down

One line each for a sign-in, a sign-out, an invite made, an invite used and
a refusal by role, with the name, the role and the address. Never a
password, a token, an invite code or a cookie value.

### One field per thing

A request field names what a caller wants, not the flag some vendor spells
it with. Where two CLIs mean the same thing, rota offers one field and each
argv builder does its own translating — the way `effort` has always sat
behind `--effort` and `--reasoning-effort`:

| field | what each CLI is actually given |
|---|---|
| `json_schema` | `--json-schema` inline for claude and grok; codex is handed a file and `--output-schema` |
| `fork_session` | `--fork-session`, or codex's `exec fork` subcommand |
| `ephemeral` | `--ephemeral`, or claude's `--no-session-persistence` |
| `continue` | `-c`, `--continue`, or codex's `resume --last` |
| `resume` | `--resume <id>`, codex's `resume <id>`, kimi's `-S <id>` |

A session id may also live in a sibling account of the same provider: the
transcript is copied into the target's home before the launch, so a
conversation continues where the quota ran out — on the next account.
Credentials never move with it.

`resume: "last"` means the most recent session on every provider — it used
to be codex's word and a literal session id to everyone else. On the command
line a bare `--resume` says the same thing; `--resume <id>` still names one.

Fields that are genuinely peculiar to one CLI keep that CLI's name and are
refused elsewhere, by name, rather than dropped silently. Two that look
alike stay apart on purpose: grok's `always_approve` overrides the
permission mode and is gated as dangerous, while codex's `approve_for_me`
answers inside its sandbox and is not. One name would mean one gate.

### One vocabulary for four CLIs

The four CLIs describe the same handful of happenings in four unrelated
event vocabularies. A client reading a rota stream learns one:

| | |
|---|---|
| `init` | first, before the CLI starts: which account, provider, model, effort and directory |
| `text` | the agent said something; with `blocks` when asked for — see below |
| `thinking` | it thought something |
| `text` / `thinking` with `delta` | one fragment of a piece still being written; only when partial messages were asked for — see below |
| `tool` / `tool_result` | it used a tool, with `tool`, `tool_id` and the tool's own `input` — the file a Read opened, the command a Bash ran — and what came back |
| `blocked` | a tool it wanted was refused, with `tool` and `reason` |
| `usage` | a limit or token reading went by; a token reading carries `usage` with `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens` |
| `input` | what became of a message sent into an open run: `state` is `accepted`, `answered` or `failed`, with its `id`, and `reason` when it failed |
| `interrupted` | an interrupt the CLI acknowledged, with its `id` |
| `idle` | every message sent so far has been answered; the run is waiting for more |
| `ping` | a heartbeat on a run that stays open, so a proxy and a client both go on believing it is there. It carries no `seq` — it is not something the run did — and appears on no other run |
| `done` / `error` | how the run ended, with the exit status, and the totals: `num_turns`, `cost_usd` and the provider's own `usage` |
| `other` | something rota recognises but has nothing general to say about |

Every event carries a `seq`, so a gap is visible. Nothing is dropped: an
event type rota has never seen still arrives, as `other`. Set
`include_events` (`--with raw` on the command line) to get the provider's own
event alongside rota's, in `raw`.

Token readings come when a provider gives them: codex at the end of each
turn, and claude at the end of each message when partial messages were asked
for — its `message_delta` is the one reading a claude run gives while it is
still going. Each is that message's own count, not a running total; the total
is on `done`. claude's limit reading (`rate_limit_event`) is a `usage` event
with no numbers.

`init` exists because the CLI's own opening event knows nothing about the
account, and a caller should not have to wait for a run to end to learn
which model it is paying for. The finished reply carries `model` and
`effort` for the same reason: an empty request field means the provider's
default, and only rota can say what that resolved to.

### Watching a run happen

`--stream` prints events as they arrive instead of one answer at the end, in
whichever form was asked for.

```sh
rota run 1 "..." --stream           # the prose, as it is written
rota --json run 1 "..." --stream    # one JSON object per line
```

Text mode prints what the agent said and nothing else. JSON mode is
newline-delimited: exactly one complete object per line, never a pretty-printed
document, so `while read line` and `jq -c` both work on it unchanged. They are
the same events `POST /v1/run` sends, so a caller can move between the two
transports without changing what reads them.

```json
{"type":"init","seq":1,"account":1,"provider":"claude","model":"claude-opus-5","effort":"high"}
{"type":"text","seq":4,"account":1,"provider":"claude","session_id":"91ebe527…","text":"ndjson works"}
{"type":"done","exit_code":0,"is_error":false,"account":1,"session_id":"91ebe527…","duration_ms":3436,"num_turns":1,"cost_usd":0.0159,"usage":{"input_tokens":10,"output_tokens":53,…}}
```

`--with raw` (`include_events` over HTTP) attaches the provider's own line to
each event, in `raw`, and in a buffered reply keeps every line the CLI
printed in `events` — for claude, whose buffered run prints one document,
that is the one document; the stream is where every line is. It is machine
output by nature, so it implies `--json`.

```sh
rota run 1 "..." --stream --with raw   # every event, with the provider's line in raw
rota run 1 "..." --with raw            # one document, with the whole event stream in events
```

rota speaks first, before the CLI has done anything, so a reader knows which
account is paying and which model and effort were resolved. Every event after
that carries its place in the stream, which is a reader's only way to notice a
gap. The last one says how it ended, whether that was an answer or a failure.

A `text` or `thinking` event arrives when that piece is complete — a
one-turn answer is one `text` event, near the end — so it is also the sign
that the piece has ended. `--with deltas` (`include_partial_messages` over HTTP)
asks for the fragments too, as the model writes them. In text mode they are
printed as they come, and the whole piece is not printed again. In JSON mode
each fragment is its own event, marked `"delta":true`, and the whole piece
still follows unmarked, so a reader that ignores deltas sees what it always
saw, and one that shows them skips the whole it has already shown in parts.
The first delta is when a piece started; the unmarked event is when it ended.

```sh
rota run 1 "..." --with deltas          # the prose, fragment by fragment; implies --stream
rota --json run 1 "..." --with deltas   # each fragment an event of its own
```

```json
{"type":"text","seq":4,"account":1,"provider":"claude","session_id":"91ebe527…","text":"ndjson ","delta":true}
{"type":"text","seq":5,"account":1,"provider":"claude","session_id":"91ebe527…","text":"works","delta":true}
{"type":"text","seq":6,"account":1,"provider":"claude","session_id":"91ebe527…","text":"ndjson works"}
```

Fragments belong to a stream, so `include_partial_messages` without `stream`
is refused before anything is spent, rather than the reply quietly becoming a
stream the caller did not ask for. The framing the CLI sends around fragments
— a message opening, a block starting or ending, a signature — is no event at
all: it says nothing a client could show that the whole piece does not.
claude and grok stream fragments; codex and kimi do not have the flag.

Without `--stream`, `--json` is still one indented document, as it always was.

### Talking to a running run

`--input` keeps the run open after its first answer: the CLI keeps its
standard input, and rota reads more messages from its own — one per line —
while the answers stream out. It is the same process and the same
conversation, so there is no resume and nothing to carry across. It implies
`--stream`, and not `--json`: text mode prints the answers and nothing else,
as it always does.

```sh
rota run 1 "start here" --input          # the prose, and more lines to send
rota --json run 1 "start here" --input   # the same, as events
echo "just this" | rota run 1 --input    # the first line is the prompt
```

A line beginning with `/` is a command rather than a message:

| | |
|---|---|
| `/interrupt` | stop the tool the agent is running |
| `/steer <text>` | interrupt, then send, so the message starts the next turn |
| `/close` | no more messages: the run finishes its turn and exits |

The end of stdin means `/close`. A line that starts with `{` is taken as
claude's own input vocabulary and written down the pipe as it is, for a
caller that would rather compose it than have rota compose it.

A run started this way prints an id on stderr, and carries it as `run_id` on
its opening event. `rota send` reaches it from another terminal, or from a
script, over a unix socket in `<store>/runs/<id>.sock`:

```sh
rota send 7f3c1a5d "also check the tests"      # one more message
rota send 7f3c1a5d "do this instead" --steer   # interrupt first
rota send 7f3c1a5d --interrupt                 # stop what it is doing
rota send 7f3c1a5d --close                     # let it finish
```

It prints `accepted <id>`, `interrupted <id>` or `closed`, and with `--json`
the reply as it came. The answer itself comes out where the run is printing,
not where the send was typed. A run whose socket could not be opened — a
directory that will not take one, a path too long for the platform's limit —
says so when it starts and goes on running; only sending into it from
elsewhere is lost.

Three event types say what became of each message, so a client never has to
guess: `input` with `state` `accepted`, `answered` or `failed`;
`interrupted`, carrying the interrupt's own id; and `idle`, when everything
sent has been answered.

```json
{"type":"init","seq":1,"account":1,"provider":"claude","run_id":"7f3c1a5d2e4b9c10"}
{"type":"input","seq":5,"account":1,"provider":"claude","id":"a41f9c0d","state":"accepted"}
{"type":"text","seq":8,"account":1,"provider":"claude","text":"…"}
{"type":"input","seq":9,"account":1,"provider":"claude","id":"a41f9c0d","state":"answered"}
{"type":"idle","seq":10,"account":1,"provider":"claude"}
```

`accepted` and `answered` are two facts, not one said twice. A message sent
mid-turn is folded in at the agent's next tool boundary and never echoed
back, so the only thing the CLI says about it is `queued_turn_count` on each
result: how many messages it has taken but not yet made turns of. At a result
carrying zero, everything accepted before it has been answered; at a result
carrying N, the last N are still waiting. That count is what `answered` is
derived from, and it has one honest limit: a message written in the same
instant claude prints a result can be reported answered one turn early. It is
never lost, and never reported answered twice.

Only Claude Code has a streaming input today. Every other CLI refuses
`--input` by name, before the run costs anything.

The same run is reachable over HTTP. `{"input": true}` on `POST /v1/run` or
`POST /v1/accounts/{id}/run` starts it and streams it, in SSE or NDJSON as
usual. It needs `"stream": true`, and is refused by name without it. The
opening event carries `run_id`, and that id is how everything below reaches
the run while it runs:

| | |
|---|---|
| `POST /v1/runs/{id}/messages` | `{"text":"…","steer":false}` → `202 {"id":"a41f9c0d","state":"accepted"}` |
| `POST /v1/runs/{id}/interrupt` | → `202 {"id":"6b2e…"}`, the id the acknowledgement will carry |
| `POST /v1/runs/{id}/close` | → `200 {"ok":true}`, and again the same if said twice |
| `GET /v1/runs/{id}/events?since=N` | the stream again, from the event after N |
| `GET /v1/runs/{id}` | `{"id","account","provider","session_id","state","pending","attached","since","events"}` |
| `GET /v1/runs` | `{"runs":[…]}`, the same shape: what is open now, and what has just ended |

`state` is `running`, `idle` — everything sent has been answered — or `ended`.
An id nobody knows is `404`; a run that has ended is `409`, whether the
request was a message or an interrupt; more than a hundred messages waiting
for an answer is `409` too, and a message with no text, or one over 64 KB, is
`400`.

```sh
curl -N -H "authorization: Bearer $T" -H "accept: application/x-ndjson" \
  -X POST localhost:8787/v1/run -d '{"prompt":"start here","stream":true,"input":true}'

curl -H "authorization: Bearer $T" -X POST localhost:8787/v1/runs/7f3c1a5d/messages \
  -d '{"text":"also check the tests"}'
```

The connection is not the run. When a reader drops, the run keeps going for
`InputGrace` (one minute by default) and is then interrupted and closed, so
an agent is never left spending for nobody; a negative grace ends it with its
reader. Reattaching cancels that: `GET /v1/runs/{id}/events`
replays every kept event after `since` and then goes on live, and a browser's
own reconnect works the same way — SSE frames carry `id: <seq>`, and the
`Last-Event-ID` header is read as `since`. A run keeps its last `Replay`
events (1000 by default) and stays addressable for five minutes after it ends,
so a reader that comes back late gets the tail and the `done` rather than a
404. One reader at a time: a new one replaces the old, which is closed.

An idle stream gets `ping` every fifteen seconds — the comment line `: ping`
on SSE, `{"type":"ping"}` on NDJSON, with no `seq`, since it is not something
the run did. `InputTimeout` (one hour) is the hard cap on the whole run,
separate from `--timeout` because a conversation is expected to sit idle
between messages where a one-shot run is not.

The run slot is held for the life of the run, not the life of the request:
one open run is one of the `--max-concurrent` agents this server will run at
once, from the moment it starts to the moment it ends.

#### Over a WebSocket

The endpoints above are one run reached through many requests. A WebSocket is
the same run over one connection, carrying both directions: every event out,
and messages, interrupts and the close back in. Nothing about a run is
different for having been started this way — the endpoints reach it too, and a
client may use whichever suits it.

| | |
|---|---|
| `GET /v1/ws` | start a run on whichever account the rotation picks |
| `GET /v1/accounts/{id}/ws` | start one on that account |
| `GET /v1/runs/{id}/ws?since=N` | attach to a run already going, from the event after N |

On the two that start a run, the **first frame** is the request — the same
body `POST /v1/run` takes, with `"type":"start"` — and `input` and `stream`
are true whatever it says, because that is what a socket is. It is checked
exactly as the body is: an unknown field, a model that account may not use, a
CLI with no streaming input are all refused by name before anything is spent.
A refusal is one frame and then a close:

```json
{"error":"unknown field \"prompts\"","type":"error"}
```

Everything else out is an event, one frame each, carrying the same JSON the
NDJSON stream carries and no newline after it: the run's own events from
`init` to the terminal `done` or `error`, which is the last frame before the
close. Attaching replays what was missed from `since` and then goes on live,
as `/events` does; a socket replaces whatever was reading the run, HTTP or
otherwise.

What goes in, after the start, is one of four:

| | |
|---|---|
| `{"type":"message","text":"…","steer":false,"ref":"c1"}` | one more message, or a steer: interrupt first, so it starts the next turn |
| `{"type":"interrupt","ref":"c2"}` | stop the tool it is running |
| `{"type":"close","ref":"c3"}` | no more messages: it finishes its turn and exits |
| `{"type":"raw","line":{…},"ref":"c4"}` | one line in the CLI's own input vocabulary, written down the pipe as it is |

Each is answered with an ack, carrying the `ref` it was sent under — `ref` is
the client's own name for a frame, echoed back, and may be left out. A
message and an interrupt are acked with the id their events will carry; a
close and a raw line have none.

```json
{"type":"ack","ref":"c1","id":"a41f9c0d","state":"accepted"}
{"type":"ack","ref":"c2","error":"run 7f3c1a5d has ended"}
```

An ack is a convenience, not the answer: `input`, `interrupted` and `idle`
still arrive as events, so a client that ignores acks entirely loses nothing.

**The token.** A browser cannot put a header on a WebSocket, so the bearer
token may travel as a subprotocol instead: `Sec-WebSocket-Protocol: rota,
bearer.<token>`, of which the server echoes only `rota` back. An
`Authorization: Bearer` header works too, for a client that can set one. It
may not travel in the query string, and one sent there is ignored: a URL is
written to every access log on the way, and a token in a log is a token given
away. Either way it is checked before the upgrade — a wrong or missing token
is an ordinary `401`, with no connection taken over, and counts towards the
same brute-force block every other endpoint is behind.

**The close codes.** `1000` the run ended or the client said goodbye, `1001`
the server let this socket go — a peer that answered none of three heartbeats,
or a shutdown — `1002` the framing itself was wrong, `1007` a text frame that
was not UTF-8, `1008` a frame this server will not act on, including a start
it refused and anything that is not one JSON object, `1009` a message past the
1 MB cap, `1011` a start that failed for a reason that was this server's own.

The heartbeat is the protocol's own: a ping every fifteen seconds, and a peer
that has answered nothing for three of them is dropped and the run left with
nobody reading it — which starts the same grace period a dropped HTTP reader
does, so reconnecting to `/v1/runs/{id}/ws?since=<last seq>` picks it up where
it stopped. Messages are capped at 1 MB, fragmented frames are reassembled,
and a ping from the client is answered with its own payload.

```js
const ws = new WebSocket("ws://localhost:8787/v1/accounts/1/ws", ["rota", "bearer." + token]);
ws.onopen = () => ws.send(JSON.stringify({ type: "start", prompt: "start here" }));
ws.onmessage = e => console.log(JSON.parse(e.data));
// later
ws.send(JSON.stringify({ type: "message", text: "also check the tests", ref: "c1" }));
```

### Reading the answer

The reply is the answer as the CLI gave it. rota adds nothing to it and
reads nothing out of it unless asked: someone used to `claude -p` sees what
`claude -p` says. A reading is asked for by name, with `--with` on the
command line — a comma list, the flag repeated, or both — and with `"with"`
over HTTP; a name nobody knows is refused by name before anything is spent.
A reading that only exists as JSON implies `--json`; `deltas` implies
`--stream`; `hooks`, `subagents` and `suggestions` change only what the CLI
prints, so text mode stays text mode.

```sh
rota run 1 "..." --with blocks,ask          # both readings
rota run 1 "..." --with ask --with blocks   # the same
```

```json
{"prompt": "...", "with": ["blocks", "ask"]}
```

- **`blocks`** splits the answer at fences into prose and code, each with
  its language, so a client showing the two differently does not need a
  markdown parser. On the reply, and on every whole `text` event of a
  stream; never on a fragment.
- **`ask`** is there when the run ended by asking something: the question,
  and the options when they were written as a list — with `multiple` when
  that list was a task list. On the reply only.
- **`raw`**: the provider's own line on each streamed event, or every line
  it printed in a buffered reply, in `events`. The same as `include_events`.
- **`deltas`**: each fragment as the model writes it, marked `delta`, before
  the whole piece — see "Watching a run happen". The same as
  `include_partial_messages`; implies a stream.
- **`code`**: the fenced code alone, as `{lang, text}` in order.
- **`files`**: the paths the agent's tools named, `read` and `written`, in
  first-seen order. Read, Glob and Grep read; Edit, MultiEdit, Write and
  NotebookEdit write. What a Bash command touched is not seen.
- **`timing`**: `at` on every streamed event, milliseconds since `init`,
  and `timing` with `first_text_ms`, `first_tool_ms`, `total_ms`.
- **`quota`**: the account's usage windows after the run — one usage call,
  claude only; the store's reading is refreshed by it.
- **`tools`**: a tally of tool calls by name, and `blocked` for the refused.
- **`stats`**: `events`, `by_type`, `fragments`, `bytes` read, `truncated`.
- **`argv`**: the command line rota ran, and `env_set` and `env_dropped`,
  the names of the variables set for it and kept from it — never values.
- **`plain`**: the answer with markdown flattened, for a place that will
  not render it.
- **`links`**: the URLs in the answer, in order, once each; not from code.
- **`account`**: `account_label`, `order` and `threshold`.
- **`hooks`**, **`subagents`**, **`suggestions`**: ask claude for its hook
  lifecycle, for what delegated subagents say (their events carry
  `subagent`, the call that delegated), and for a predicted follow-up. The
  same as `include_hook_events`, `forward_subagent_text` and
  `prompt_suggestions`.
- **`stderr`**: on a failed run with no answer, stderr copied into `result`,
  for a client that reads one field. The one reading that touches `result`.

On a stream, the readings that need the whole run — files, timing, tools,
stats, quota, account — ride on `done`. Files, tools, stats and timing are
made from the events the CLI prints, and a buffered claude run prints one
document with none in it; asking for them makes rota ask the CLI to stream
while the reply stays one document. The original text is always there
beside a reading, never replaced by it, except by `stderr` on request.

`ask` is inference over prose, and worth taking as a hint rather than a
contract. In an interactive session these arrive as real structures: a
permission prompt, a question with radio buttons, a text box. None of it
survives the headless interface — the tool that asks structured questions is
not even offered there — so the model asks in sentences like anyone else.
Options therefore come from a markdown list only: an inline "use foo, or
bar?" is reported as a question with no options, because splitting a
sentence on "or" invents choices nobody offered.

For the same reason a headless CLI never asks permission. It refuses and
tells the model, which is what `blocked` reports.

### What is running, and what could resume

The playground's **Running** section shows all of this in a page, and
`rota list --sessions` adds two sections: the vendor CLIs and editors open
right now, and the conversations `--resume` could pick up. Five per account by
default; `--all` for the rest. Over HTTP it is `GET /v1/accounts?sessions=1`,
with `&recent=0` for all of them.

```
Running instances:
  ●  claude     #1 you@example.com  ~/src/api                    2m, session af7fda4d
  ●  GoLand     -                   ~/src/api                    pid 40803

Sessions:
  #3 you@example.com  codex   01a048be  -                        ~/src/api
  shared              claude  497f1383  rota run account resolution  ~/src/api
  ~/.claude holds 2648 conversations across 146 projects, shared by every
  account with no --config of its own.
```

On a terminal narrower than a section its rows stay one line each and are
cut to fit: the name and directory first (a directory keeps its end, the name
its start), each kept eight columns wide until the account has given way as
far, then the account, then the CLI or editor, and the session id last. A pipe
or a file gets every cell whole but a conversation's name, which is never more
than 44 columns anywhere. Either way a name or directory is measured in the
columns it takes on screen, two a character for Chinese, Japanese, Korean and
most emoji, so its row lines up, and a tab, a newline or any other control
character in one is shown as a space.

A claude conversation is shown by its own name, the one its resume picker
uses: Claude Code writes a title for a conversation as it learns what it is
about, rewriting it on every turn, and the newest one is what is shown. One
it never titled is shown by the prompt it opened with, first line only, and
a conversation that is neither titled nor asked anything shows a dash.
`--json` carries the whole name as `name`, untruncated. The name is read
from the two ends of the transcript and never from the middle — the last
quarter megabyte for the title, the first 64KB for the opening prompt —
because these files reach tens of megabytes and a listing reads one per row:
a title older than that window is a name not found rather than a listing
that reads a gigabyte. Only the conversations actually shown are opened, so
the count in the last line costs nothing. codex and grok record no such name
and show a dash.

Three things are being read, and they know different amounts:

- **What rota started** is the only source that knows *which account*. rota
  writes it down in `running.json` when a run begins. A run that hands the
  terminal over is replaced through `execve` and keeps its process id, so that
  entry goes on describing the CLI that took rota's place; a run that is killed
  never removes its own entry, so reading the file is what prunes the dead.
- **Editors** write a lock file naming the workspace they have open. rota reads
  the workspace, the process and the editor's name from it — never the
  authentication token those files also carry.
- **Everything else running** comes from the process list. A working directory
  takes `/proc` on Linux and `lsof` on macOS; where it cannot be read the
  instance is still listed, with a note saying what is missing.

Attribution is exact for codex, grok and kimi, which are always launched with
a private home of their own. A claude account without a `--config` runs in a
mirror of the person's `~/.claude` — its own directory, the same files — so
every such account still reads the same conversations: those are marked
`shared` and counted once, because they belong to no single account and
listing them per account would say the same work happened twice.

The shared home cuts the other way too: claude injects the identity stored
there into every session's context, so a model asked "what is my account
email" answers with the shared home's login, whatever token the run carries.
Billing always follows the token — verify with usage, not by asking the
model. `--stateless`, or an account with `--config`, gives the run a home of
its own and a truthful (empty) identity.

Each CLI files its conversations differently, and rota reads three of the four
layouts. Claude Code keeps one file per conversation under a folder named for
the project with the separators mangled; codex files them by date, under a
`rollout-` name; grok nests a folder per conversation inside a folder named for
the project, percent-encoded. None of those names is trusted for the directory
— every one of the three records the working directory inside the conversation
itself, and that is what is shown.

kimi keeps its conversations somewhere rota has not been able to confirm, so a
kimi account is reported as unlisted rather than as none. Inventing a layout
would mean showing the wrong conversations, which is worse than showing none
and saying so.

### Where an account belongs

An account can be tied to one project, so several accounts can serve
several projects without being told which on every request:

```sh
rota set 2 --cwd ~/src/api --config ~/.rota/api-memory
```

`--cwd` is where its runs start when the request names no directory of its
own. `--config` is the account's own CLI configuration — its memory files,
skills and settings — and the private home its credentials are staged in.
Unset, codex, grok and kimi still get a private home of rota's own, and
Claude Code reads the person's own `~/.claude` through a mirror of it — the
same files, a daemon of its own — which is right until an account is meant
for one project. `--sessions shared|own|<dir>` says where a claude account's
conversations live within all this, and `--remote-control on|off` whether it
keeps a `.claude.json` of its own for Claude Code's Remote Control; see [One
Claude world, one daemon per account](#one-claude-world-one-daemon-per-account).

The two must not be the same directory: the config directory is where a
credential file is written, and a working directory is a repository someone
will commit. Both must be absolute — a relative path means a different place
depending on where the process was started. Nor may the config directory be
one of rota's own: another account's home is where that account's credential
is staged, and the store is where every refresh token is kept, so both `rota
set` and `PATCH` refuse them, links followed. A server given `--root` still
wins; an account cannot be pointed somewhere the server was told to stay
out of, and a `config_dir` outside every root is refused when it is set. A
`sessions` directory is judged by both rules and for the same reason: rota
creates folders there and links to them, so a caller who could name one
anywhere could have rota writing where the server was told not to.

### A terminal on the server

A terminal is an account's CLI running on a pseudo-terminal that lives in the
server: the interactive `claude` you would get from `rota run 1`, started
once, kept when the browser tab closes, and attachable by anyone the roles
allow. Several people may watch the same one; exactly one of them at a time
holds the keyboard.

It is not a general remote shell. What it runs is an account's CLI, launched
exactly as the handover launches it — the token refreshed or the long-lived
one used, the credential staged, the account's own Claude world mirrored —
with `TERM=xterm-256color` and `COLORTERM=truecolor` added and the request's
`args` appended verbatim. A plain login shell is available, and only if the
file says so.

It runs on **linux and macOS**. A pseudo-terminal is a Unix device; a server
asked for one anywhere else refuses to start, naming the platform, rather
than failing at the first terminal somebody opens.

**It is off.** Not "off in the sample file" — off in the defaults, which is
the one route group that is. Turning it on takes one line:

```toml
[routes]
terminal = true   # needs api and websocket
```

The `[terminal]` section then governs it: the login shell, how many terminals
may run at once, how much scrollback is kept, how long an unattended one
lives, whether output is recorded, and whether terminals offered from this
machine are taken. Every key and its default is in
*[One file for the server](#one-file-for-the-server)*, and in
`docs/server.toml`; the ones that need more than a line of comment are
explained below.

**And it needs TLS off the loopback.** With `routes.terminal` on and a listen
address that is not `127.0.0.1` or `localhost`, `tls.cert` and `tls.key` are
required and the server refuses to start without them. The reason is the
sentence worth saying plainly: *a control sign-in on this port runs commands
on this machine, as the user the server runs as.* Everything else rota serves
answers questions about accounts and runs; this one is a shell prompt. A
sign-in that travels in clear text is a sign-in given away.

#### Roles, and the keyboard

The two roles mean here what they mean everywhere: **watch** may look at
anything and change nothing, **control** may do anything this server does. A
watcher may attach to a terminal and is sent every byte it prints — and its
own bytes never reach the CLI. That is enforced in the one function that
writes to the terminal, by the identity of the connection and the role behind
it, not by what the page chose to draw.

Among the people who may type, one holds the keyboard:

- A control principal attaching takes it if nobody has it. `?mode=watch` asks
  to look without taking it, which is how you read over somebody's shoulder.
- `{"type":"claim"}` takes it if it is free. If somebody has it, they get
  `claim_request` and may answer `grant` or `deny`.
- A claim nobody has answered for ten seconds may be repeated with
  `{"type":"claim","force":true}`, which takes it. Forcing earlier is refused
  with how long is left; forcing without having asked is refused too.
- `{"type":"release"}` frees it.
- When the holder's socket drops the keyboard stays theirs for five seconds,
  so a reconnect by the same person gets it back rather than finding somebody
  else typing. After that it is free.
- Every change is broadcast as `{"holder":"alice"|null,"type":"keyboard"}`.

Anything a non-holder sends inward — bytes, or a resize — is answered with
`{"message":"you are not holding the keyboard","type":"error"}` and changes
nothing at all.

A **held session** keeps those rules and adds five, which a terminal the
server runs itself does not have:

- **The keyboard follows the person.** One person attached from several
  devices types on whichever of them without asking: typing (or `claim`) on
  a connection whose person holds the keyboard elsewhere moves it there, and
  everyone is told. A person is a user signed in on the page, by name; the
  control socket's principal, `local`, is the one `[[users]]` entry that may
  control when there is exactly one, the entry `[auth] local_user` names
  when there are more, and a person of its own when neither says. A bearer
  token or an invite is never a person, so its connections keep the claim
  flow. A connection that lost the keyboard to somebody else does not get it
  back by attaching again.
- **Only a fit resizes.** Attaching never resizes, and neither does typing.
  A `resize` without `"fit":true` is ignored, and said once per connection
  as `this session's size changes only when a viewer asks to fit it`; one
  with it is the holder's, as ever, at most 1000×1000, with a 0 meaning 80
  columns or 24 rows.
- **A viewer's terminal never answers twice.** What a connection sends first
  has its own terminal's answers taken out — to the colour, cursor and
  device queries it sees replayed, which the holder answered when the CLI
  asked — so a watcher's terminal answering is not a watcher typing, and a
  frame that held nothing else does nothing at all.
- **A send is not typed over.** While a `send` waits to be typed, it holds
  the session for its person: anybody else's typing is refused, said to
  each connection at most once every 2 seconds as `a send from <name> is
  waiting to be typed into this session; type once it is in`, so no
  half-typed line goes in ahead of it and is submitted by its Enter. While a
  send waits, only its sender's person may type, and nobody under a token's
  send; anyone at the keyboard may still press Esc or Ctrl-C once, a single
  key in a frame of its own, which stops the turn and leaves the send to be
  typed. A send whose asker went
  away still holds it until its holder answers, for at most 35 seconds.
- **Falling behind repaints.** A viewer that cannot keep up with the output
  is not closed with `1013`; see *Falling behind* below.

#### The routes

| | |
|---|---|
| `GET /v1/terminals` | every terminal, running and recently ended (watch) |
| `GET /v1/terminals/{id}` | one of them (watch) |
| `GET /v1/terminals/{id}/ws?since=N&mode=…` | attach (watch) |
| `POST /v1/terminals` | start one (control) |
| `DELETE /v1/terminals/{id}` | end one: a hangup, then a kill three seconds later, or five for a held session (control) |
| `POST /v1/terminals/{id}/send` | type a message into a held session once it is ready, then Enter (control) |

```bash
curl -X POST localhost:8787/v1/terminals -H "Authorization: Bearer $ROTA_TOKEN" \
  -d '{"account":1,"args":["--resume","af7fda4d"],"cols":120,"rows":40,"label":"fintech"}'
```

`account` left out lets the rotation choose, as a run does. `kind:"shell"`
runs `$SHELL -l` (or `/bin/sh`) with the server's environment instead, and is
refused with a plain sentence unless `terminal.shell` is true. `cwd` is
resolved and confined by the same rule, and refused in the same words, as a
run's working directory.

The answer is the description, which is also what `GET` returns and what the
page's information panel is built from:

```json
{"account":{"id":1,"label":"you@example.com","provider":"claude"},"cols":120,"cwd":"/srv/work",
 "ended":false,"holder":"alice","id":"9a4c1e77b2d05f31","kind":"account","label":"fintech",
 "offset":20418,"recording":false,"rows":40,"started":"2026-09-21T10:11:12+05:00",
 "token_until":"2027-03-04T00:00:00Z","viewers":[{"name":"alice","role":"control","since":"2026-09-21T10:11:14+05:00"}]}
```

`token_until` is when the login inside will lapse — for a claude terminal on
the account's own stored login, when that login itself ends, if Claude Code
has recorded it, since Claude Code refreshes the token inside; for one on a
token, the long-lived token's date or the access token's expiry — so
somebody looking at a terminal that has been open all day can see it coming.
`offset` is how many bytes it has printed altogether, which is what `since`
is measured in.

A held session — one the rota daemon runs in a holder process of its own,
which outlives the daemon — adds `"held": true`; `state`, which is `live`,
`ended`, or `interrupted` for a holder that went without recording an exit;
`attachable`; `pid`, the holder's; `title`, the one the CLI set;
`conversation`, the CLI's own session id when it reports one; `status`,
which is `running`, `needs_you` while the CLI waits on a permission or a
question, `quiet` after 30 seconds with no output, or `stopped`; and `name`,
the first there is of the label, the title without its spinner, the CLI's
own name for the conversation, and the folder. A held session is never
ended for being idle. `reachable` is whether the daemon's link to its
holder is up; `conversation_cwd` is the folder the conversation is in,
which for a worktree is not `cwd`; `resumable` is an interrupted session
whose conversation is known; and `ended_at` is when a stopped one stopped.

A daemon that starts takes up every holder still running on its store, as
if it had started it, and a viewer's offsets go on meaning the same bytes:
a restart looks like a brief reconnect. Each session's directory is judged
on its own, so one broken record hides nothing. A holder whose link fails is
listed `live` with `"reachable": false` and tried again at every sweep — its
lock, not a silent socket, says whether it is still there. A holder made by
a rota this one cannot attach to — or one whose record this rota cannot
read, while its lock says it is running — is listed `live` with
`"attachable": false` and `made_by`, the version that made it as far as its
record says, counts under `max_sessions`, and is never stopped but by a
`DELETE`. A held session that has stopped is still listed once the five
minutes an ended terminal stays are over: from its record, for
`[terminal] keep_stopped` after it stopped — `ended` with its exit,
`interrupted` (exit `-1`) for a holder that went without recording one, a
reboot or a kill, or `broken` for a record that cannot be read. Nothing
restarts by itself. An interrupted session's age, and its `ended_at`, are
measured from the moment the daemon saw its holder go, which is written into
its record, or with no daemon there from the last time its holder touched
it; `ended_at` reads the same before and after the daemon stops keeping it. Once `keep_stopped` has passed, the
session's directory under `<store>/terminals/` is deleted, and nothing else:
never a worktree, and never a directory whose lock is held. `max_sessions`
counts every running holder on the store, these ones included.

#### The socket

One connection carries both directions, and the framing is what tells the two
kinds of traffic apart: **binary frames are the terminal's bytes**, text
frames are documents. The token travels as it does on every other socket here
— `Sec-WebSocket-Protocol: rota, bearer.<token>`, an `Authorization` header,
or, for a page whose person has signed in, the session cookie, which is held
to the same Origin rule.

Out:

| | |
|---|---|
| *binary* | output: write it to the terminal emulator as it arrives |
| `hello{id,kind,cols,rows,offset,holder,you:{name,role,conn},viewers}` | the first frame, always; `repaint:true` on a held session attached without `since`, whose first binary frame is then its current screen, standing for `offset`; a held session's also has `holder_conn` |
| `keyboard{holder}` | who holds it now, `null` for nobody; a held session's also has `holder_conn`, the `conn` of the connection holding it, since one person may be attached from several devices |
| `repaint{offset,cols,rows}` | a held session's viewer that fell behind: the next binary frame is the screen at that size, standing for `offset`, and output goes on from there |
| `claim_request{by,id}` | somebody is asking the holder for it |
| `resized{cols,rows}` | the terminal is a different size now |
| `viewers{list}` | who is attached: name, role and since — never an address |
| `gap{from,to,bytes}` | bytes you will never be sent: `since` was older than the scrollback (`from` to `to`, before any output), or, mid-stream, `bytes` of output that never reached this server — a shared terminal's queue that overflowed, or a held session's holder that printed past what its link carried; the offsets go on from `to`. A held session's viewer that is behind is not sent a mid-stream one, since its repaint goes on from past it; one repainted without a screen is sent one after the reset, for the scrollback it is not replayed |
| `exit{code}` | the CLI ended |
| `error{message}` | a frame that was read and will not be acted on |
| `pong` | the answer to `ping` |

In:

| | |
|---|---|
| *binary* | input: the bytes of somebody typing, control characters and all |
| `resize{cols,rows,fit?}` | holder only; on a held session only with `"fit":true` |
| `claim{force?}` / `release` | ask for the keyboard, or let it go |
| `grant{to}` / `deny{to}` | the holder answering a `claim_request`, by its `id` |
| `ping` | the one thing a watcher may send |

Which role each of those takes is one table in the server beside the table of
routes, so "who may do this?" is still read in one screen.

**Replay.** `hello.offset` is the absolute offset of the first byte you will
be sent; add the length of every binary frame to it and you have the number
to reconnect with. `?since=N` gives you everything from there; saying nothing
gives you the whole scrollback, which is what a page opening a terminal for
the first time wants. If `N` is older than `scrollback_bytes`, a `gap` frame
comes first and says exactly which bytes were lost. A `gap` can also come in
the middle of the output, for bytes this server itself never received; the
next binary frame then starts at its `to`.

**Falling behind.** Each connection has a bounded queue and the terminal
never waits for one: a reader that stops reading is closed with `1013` and
reattaches with `since`. One slow watcher cannot stall the CLI or anybody
else watching it. A held session's viewer is not closed for its output:
the output it fell behind on is thrown away, and once it has caught up
with what was queued it is sent `repaint{offset,cols,rows}`, the screen as
it is now as the next binary frame, and the output after `offset`. The
screen is asked for twice; a holder with none to give gets the viewer a
reset (`ESC c`) as that frame, a `gap` for the scrollback before the last
64 KiB, and those 64 KiB. What a held session's viewer is replayed — on
attaching, or after a repaint — comes in frames of at most 64 KiB, each
with its own write deadline, and one viewer is repainted at most four
times a second. Documents still arrive, and the ones that came while it
was behind arrive after the repaint; a viewer owed more documents than it
has room for (64 waiting to be written, or 1024 waiting for its repaint) is
closed with `1013` after all, since a repaint does not say them again.

```js
const ws = new WebSocket(`wss://host/v1/terminals/${id}/ws`);
ws.binaryType = "arraybuffer";
ws.onmessage = e => {
  if (typeof e.data === "string") return onDocument(JSON.parse(e.data));
  offset += e.data.byteLength;
  term.write(new Uint8Array(e.data));
};
term.onData(d => ws.send(d));                       // binary: what was typed
term.onResize(({cols, rows}) => ws.send(JSON.stringify({type:"resize", cols, rows})));
```

#### The terminal page

`GET /terminal` is where a terminal is watched and typed at. It is in the
`terminal` group, not the playground's: a server that holds terminals and
serves no playground has this page, and a server with no terminals does not
have it at all. It is the same origin and the same sign-in as the playground
— one cookie, one bearer token in that tab's storage — and each page carries
a link to the other, shown only where the other one is being served.

Three regions, which stack on a narrow screen.

**The list**, on the left: every terminal this server holds, running ones
first and ended ones after, each saying what it is, which account is paying
for it, who holds its keyboard and how many people are watching — or *ended,
code N*. A control principal also gets **New terminal**: which account (or
the rotation's choice), a label, arguments, a folder, and a plain shell where
`terminal.shell` allowed one. Arguments are split the way a shell splits
them, as far as quoting goes; nothing is expanded. The list is refreshed
when a terminal is started or killed, when a frame about the open terminal
arrives, and otherwise every ten seconds while somebody is looking at it.

**The terminal**, in the middle, with one strip above it: whether the socket
is *connecting*, *live*, *reconnecting in Ns* or *ended, code N*; and the
keyboard, which has exactly three states —

- you hold it, and may **Release** it;
- nobody holds it, and you may **Take the keyboard**;
- somebody else holds it, and you may **Ask for the keyboard**. That sends a
  `claim`; the page then counts down the same ten seconds the server counts,
  and offers **Force** when they are up and nobody has answered. Offering it
  sooner would be a lie: the server refuses it.

The holder's tab shows a request from anybody else as a prompt with **Grant**
and **Deny**, which are `grant{to}` and `deny{to}` under the asking
connection's own id. A watcher is shown none of this, only a *watching*
badge — and a key pressed by somebody who is not holding is not sent to be
refused: it goes nowhere, and the keyboard control flashes once.

The terminal is fitted to its container for whoever holds the keyboard,
which sends `resize` when the fitted size changes; for everybody else it is
set to the session's own `cols`×`rows` and scrolls inside the container
rather than lying about how wide the terminal on the server is. On a held
session that `resize` is not a fit, so the server leaves the size alone and
says so once.

**Reconnecting** is the socket's own rules, followed exactly. The page adds
every binary frame's length to `hello.offset`, so it always knows the offset
it has actually been shown, and it reattaches with `?since=` that number —
never with what the server last said. A first attach says nothing, which
replays the whole scrollback. On a held session the screen after a `repaint`
frame, or after a `hello` with `repaint`, is not output: the offset is the
one that frame said, and only what follows the screen adds to it. A
mid-stream `gap` moves the offset to its `to`. Close code `1013` is "you
fell behind", and it goes back at once; anything else waits 1, 2, 4, 8 and
then 15 seconds. A `gap` frame is written into the terminal itself as a dim
line, *… N bytes were not kept …*, because a terminal that looked continuous
when it was not is worse than a visible hole. An `exit` stops all of it. There is never a
second socket for one terminal, and leaving the page or choosing another
terminal closes the one there is with a clean `1000`.

**The information**, on the right: the account and its provider, the label,
the folder, how long ago it started, the size, who holds the keyboard, who
is watching and as what, whether it is being recorded — and **the login
inside lapses in …**, from `token_until`, which turns into a warning under
an hour, because nothing else on the screen shows that the CLI in there will
stop working at a particular time. A control principal also gets **Kill**,
behind a confirmation. An ended terminal reads the same way, with *exited,
code N*, and its scrollback is still replayed for as long as the server
keeps it.

#### Vendored assets

The emulator is [xterm.js](https://xtermjs.org) — `@xterm/xterm` 5.5.0 and
`@xterm/addon-fit` 0.10.0, both MIT — carried in the rota binary and served
from `/assets/xterm/`. Nothing is fetched from another host at run time, by
this page or any other: rota is usually on a loopback address with no route
out, and a page that asked a CDN for the code that draws somebody's shell
would be both broken there and a third party's to change.

Each file was taken from the npm tarball for that exact version after the
tarball had answered the `dist.integrity` sha512 in the registry's own
metadata. `api/assets/xterm/README` records the versions, those integrity
strings, the SHA-256 of every vendored file and the three steps to update;
a Go test recomputes those SHA-256s from what is embedded, so none of it can
be edited quietly. The files are served with their right types, `nosniff`
and a year of caching, under a `?v=` query carrying this server's version —
so an upgraded rota is a different URL rather than a stale script.

The pages' own Content-Security-Policy admits `'self'` for scripts and
styles, which is these files and the two shared ones and nothing else: no
remote host, and no `unsafe-eval`, which the emulator does not need.

#### Limits, the audit log and recording

`max_sessions` caps the terminals running at once; past it, `POST` answers
`409` and says which setting it was. `idle_timeout` ends a terminal nobody
has been attached to for that long — measured from the last person leaving,
not from the last keystroke. Stopping the server ends every terminal: each
one is a CLI in a session of its own, which is exactly what a signal to rota
does not reach. An ended terminal stays listed for five minutes, with its
exit code, so a client reattaching a moment late reads the end rather than a
404 it cannot tell from a wrong id. It is then held by nobody: `holder` is
`null` in the description and in the listing once a terminal has ended,
because there is no keyboard left to hold.

The audit log is always on, and never carries a keystroke or a line of
output: `terminal created` (id, kind, account, by), `terminal attached` and
`terminal detached` (id, name, role), `keyboard taken`, `keyboard released`,
`keyboard granted` and `keyboard forced` (id, from, to), `terminal killed`
(id, by) and `terminal ended` (id, exit); for a held session, `terminal
adopted` (id, kind, account, pid) when a daemon takes it up, `terminal
relinked` (id, pid) when its link comes back, `keyboard moved` (id, from,
to) when it follows its person to another connection, and `terminal sent`
(id, by, force, answer) for a send, without its text — and, for one that was offered from
a local terminal, `terminal shared` (id, account, label, pid, mode) and
`terminal unshared` (id, exit) beside the last of those.

With `record = true`, what each terminal **printed** is appended to
`<store>/terminals/<id>.out`, up to `record_max_bytes`, with the description
above written beside it as `<id>.json` when it ends. A held session's
recording is carried on by the next daemon from where in the output it had
got to, so nothing is recorded twice; where output never reached it — a gap,
or more printed with no daemon there than its holder kept — `<id>.gaps`
gets a line `<bytes in the file> <offset in the output>` before the file
goes on. Input is never recorded
— what reaches the file is the same bytes every attached client was sent, and
nothing else. The files are mode `0600` and the directory `0700`: a recording
is a transcript of somebody's working session.

### Sharing the terminal you are sitting at

The terminals above are the server's own: it started the CLI and it holds the
pseudo-terminal. This is the other direction. You are at a terminal, you run
the CLI there as you always did, and the same session also appears on the
page — to be watched, or to be typed into from a browser while you keep
typing at your desk.

```sh
rota run 1 --share                  # the CLI as it comes, and watchable
rota run --share=watch --label api  # offered to be read, under that name
rota run 1 --share -- --some-flag   # with the CLI's own arguments
```

**What it does not change.** `rota run <id>` with no prompt hands the terminal
over by replacing rota with the CLI; `--share` prepares the account in exactly
the same way — the token refreshed or the long-lived one used, the credential
staged, the account's own Claude world mirrored, the instance recorded — and
then runs the CLI on a pseudo-terminal with rota still above it, copying both
ways. The CLI's exit code is rota's. `SIGWINCH` resizes it, `SIGHUP` and
`SIGTERM` hang it up, and the terminal's modes are put back on every path out
— a normal exit, a signal, a panic. Without `--share`, nothing whatever
changes: the exec handover is still the exec handover.

**Sharing never blocks or slows the terminal.** This is the rule the rest
follows from. What you type goes straight into the CLI and is never carried
near the link. What the CLI prints is written to your screen first and offered
to the server afterwards, into a bounded queue — a few mebibytes — that drops
rather than waits. A server that is slow, that went away, or that was never
started costs you nothing: the bytes it missed are counted and announced to
whoever is watching as a `gap`, and the page writes that hole into the
terminal rather than pretending the output was continuous.

**The socket.** A server with `[routes] terminal = true` and `[terminal]
share = true` — the default — listens on `<store>/terminals/share.sock`, mode
`0600` inside a directory that is `0700`. **Who may share a terminal is
decided by the filesystem**: the only process that can connect is one run by
the user the server runs as. There is no token, because a token would be a
second secret protecting something the kernel already protects. A stale
socket left by a killed server is replaced at start-up; one removed on
shutdown.

The sharer dials it, and if nobody is listening it keeps running and retries
every few seconds — quietly, because sharing is an extra and not a condition
for running. Before the CLI takes the screen rota prints one line saying
which of the two happened: *shared to the rota server on this machine as
terminal `<id>`*, or *no rota server is listening at `<path>`; sharing begins
when one is*. A server that refuses — a protocol version that does not match,
or `max_sessions` already reached — says so in a sentence rota prints once.

The protocol is length-prefixed frames, four bytes big-endian and a type
byte. Sharer to server: `hello` (version, account, label, folder, size, pid,
mode), `output` (bytes), `resized`, `gap` (how much never left), `exit`.
Server to sharer: `welcome` (the terminal's id) or `refused` (a sentence, and
whether it is worth coming back), `input` (bytes), `kill`. `ping` and `pong`
go both ways. One comment in `internal/share` is the whole of it.

**On the page** a shared terminal is a terminal like any other — the same
list, the same socket, the same replay with absolute offsets, the same
one-holder keyboard among web connections, the same `?since=`, the same
`1013` for a reader that falls behind, the same audit lines. It is
`kind: "shared"` and carries `pid` and `mode`, and it says *shared from a
local terminal*.

Two things about it are different, and both are facts about where it is:

- **Its size follows the window it was started in.** A `resize` from the page
  is answered with `{"message":"this terminal's size follows the window it was
  started in","type":"error"}` and changes nothing; the page draws it at
  the session's `cols`×`rows` and follows `resized`, even for the holder.
- **The person at the terminal can always type**, because their keystrokes
  never pass through the server at all. With `--share=watch` nobody on the
  page ever can: `claim` and input are refused with *this terminal is shared
  to be watched only*, whatever role the sign-in has, and the page shows no
  keyboard controls to anybody.

**Ending it.** The CLI exiting sends `exit` with its code. `DELETE
/v1/terminals/{id}` sends `kill`, and the sharer hangs up its child and
insists three seconds later. The link breaking without an `exit` is the third
ending: the terminal ends with code `-1`, `lost: true`, and the description
says *the sharing process went away*. Shared terminals count against
`max_sessions` like the rest.

It runs on **linux and macOS**, and is refused by name anywhere else:
`--share` needs a pseudo-terminal for the CLI, a raw mode for the window you
are at, and a signal when that window is dragged. Standard input that is not
a terminal is refused too, with *--share needs a terminal to share* — there
would be nothing to share.

### The playground

`GET /playground` serves a page with no credential of its own. It is one of
two: the terminal page is the other, described above, and they share an
origin, a sign-in, a stylesheet and a script — `/assets/page.css` and
`/assets/page.js`, which hold the palette, the header strip, the credential,
the session and the sign-in, so there is one copy of that and not two that
drift apart. Each page links to the other where the other is served.

It asks `GET /v1/session` first. Signed out, it offers a name and a password, with
the bearer token underneath it as the way this page has always been used —
and a token is still proved against `/v1/accounts`, the first thing the page
needs anyway, so a right one costs no extra round trip. A token is kept in
that tab and nowhere else; a sign-in is a cookie the page never sees.

Signed in, the header says who and as what, with a way out. A `watch`
principal — somebody who signed in with that role, a watch token, or a
visitor who followed an invite — gets the same page with the run button, the
message box and every account control disabled, and one sentence saying
*watching: this sign-in cannot change anything*. Lists, usage and a run's
stream work as they always did. The server refuses those acts regardless;
the page is so that nobody has to find that out by being refused.

Five sections down the left, reachable by their number keys: **Ask**,
**Accounts**, **Running**, **Sign in**, **Console**. That is the whole page — one rail,
one working column, one output column, and no nested tab bars to lose your
place in.

**Ask** is generated from `/v1/schema`, so it offers exactly what the server
accepts — the models *that account* may use, the effort levels its provider
has, and nothing belonging to a different CLI. The request vocabulary is
seventy-odd fields, which is too many to scroll and too many to hide, so it
is both: grouped into Essentials, Session, Context, Permissions and Output,
collapsed except the first, each header carrying a count of what it holds so
nothing set is ever out of sight — and a filter above them that searches
every field's name, label and description at once and opens whatever it
matched. Every field carries a sentence explaining what it does; options
that bypass a safety check are marked, and disabled outright when the server
was started without `--allow-dangerous`.

Some field types get a real editor rather than a text box: lists become
removable chips, maps become key/value rows, files are dropped or picked and
sent with the request, and a JSON Schema can be built field by field — name,
type, required — or written by hand, whichever is quicker. The Run button is
pinned below the form and says which account is about to be spent.

**Accounts** is the rotation itself, in the order it is spent: each row's
place can be typed in or nudged with the arrows beside it, its threshold set
against the usage bar it will be judged by, and the account the next run
would take is marked. A row at 0 is greyed out — still there, still runnable
by id, never picked. Removing an account is on the row it belongs to. On Ask
the account selector opens on *Rotation*, and the footer says which account
that currently means.

A streamed run with **input** on is opened over a WebSocket rather than
fetched, and a box appears under the output column while it is open: a line
to send, a Steer checkbox beside it, Stop for the tool it is running and
Close for the run. Enter sends. The events fill the Response tab exactly as a
streamed run's do — it is the same stream — and a refused frame says so in the
status line. If the connection breaks before the run ends the page reconnects
to it from where it got to, three times, which the run's grace period is long
enough for. A run with input off is the fetch it always was.

The right column has three tabs. **Response** shows the answer, then the
run's account, cost, duration and session, then every event — all
syntax-coloured, and streaming runs fill it in as they arrive. **Request**
shows the endpoint it will be posted to and the exact JSON body, updating as
you type. **History** keeps the last 40 runs in that browser's local storage
— the body that was sent, where it went and what came back — and *View*
opens any of them in those same two panes, marked as history with the way
back to the live request. *Load* puts one into the form instead. Event logs
are counted rather than stored: they can be larger than everything else put
together.

Light and dark follow the system, with a toggle that overrides it. Both
pages are self-contained: no fonts, scripts or styles from anywhere but this
server, because it is usually served on a loopback address with no route
out. A test runs their actual JavaScript against this server's own schema —
typing, toggling, filtering, building a schema, running, streaming,
reordering the rotation; and, on the other page, attaching, replaying,
taking the keyboard and reconnecting — so a changed field or response shape
cannot break either of them silently.

### Safety

The server assumes its caller holds a credential but is **not** trusted with
the machine. That assumption is what the following exist for.

Two things about credentials, before the rest. **Without TLS a password
travels in clear text exactly as the token does** — the server warns at start
when `[[users]]` are configured, no certificate is given and the address is
not the loopback. And **`control` is the power to run commands on this
machine**: the agents this server starts have a shell, so handing somebody
that role is handing them the machine, and `watch` exists so that showing
somebody what is happening does not have to be. The terminal is the sharpest
edge of that, which is why it is the one route group that is off in the
defaults and the one that refuses a listen address off the loopback without
a certificate — see *A terminal on the server*.

`--root` (repeatable) confines every path a request names — the working
directory, uploads, extra directories, plugin directories, images, a debug
log, a settings or MCP file. The list is exhaustive by test, because a field
left out is one a caller can point at the token store: an agent that reads a
file and describes it is an exfiltration primitive. It confines what is
actually a path, too: grok writes its debug log where it is told, while Claude
Code's `--debug` takes a category filter and reaches no file at all. Without `--root` a caller
may name any directory, and the server says so at startup.

What `--root` confines is what a *request* names. It does not confine what
the agent's own tools read once it is running: an agent with a shell reads
whatever the server's operating-system user can read, roots or not, and
`--allow-dangerous` has nothing to do with it — that gate is about the
agent's permission prompts, not the filesystem. The answer is to run `rota
serve` as a user that can read only the roots and the store, so that the
most an agent can reach is what it was given.

`--allow-dangerous` is required before a request may use
`bypassPermissions`, `dangerously_skip_permissions` or a full-access sandbox;
without it those are refused with 403.

Everything that would hand the child code, or configuration rota did not
choose, is refused for a caller under limits, because any one of them makes
the rest decorative.

**Raw vendor flags** (`args`) — every option rota gates has a flag that undoes
it, and keeping a deny-list current across vendor releases is a game rota
loses. **An inline settings or MCP document** — Claude Code's settings may
carry an `env` block and an MCP server is a command line with an environment,
either of which sends the OAuth token wherever the request likes; a *path* to
such a file is still accepted. **Plugin URLs** — a plugin is code fetched from
wherever the URL points, and a plugin carries hooks, which are the commands
the settings gate above exists to refuse; `plugin_dirs` inside a `--root`
stays, because that is content the operator chose. **Config overrides**
(`config`) — codex's configuration names the endpoint the run is sent to, so
`-c model_providers.x.base_url=…` sends the prompt, the context and the
credential to a host the caller picked. That was tested against the real
binary: it posted the whole request to a listener on localhost and waited for
the reply, which is also how an agent is told what to do next. rota already
drops `OPENAI_BASE_URL` from the child environment for the same reason; the
config route was the same act by another door.

`--allow-raw-flags` re-opens all four for an operator who is the only caller,
and it is one flag rather than four because they are one question: whether
this caller is trusted with the machine. Anyone holding it could write the
flags by hand anyway.

rota's own secrets never reach the agent: `ROTA_TOKEN` and `ROTA_HOME` are
removed from the child environment unconditionally, as are the proxy and
certificate variables that would otherwise intercept a token without touching
any base URL.

Requests are capped (64 MB total, 16 MB per file, 32 files), a run's stderr
is kept to its last 64 KB, and a 500 says only that — internal errors are
logged, not returned. Loopback by default; without `--tls-cert`/`--tls-key`
the token travels in clear text, so put the server behind a reverse proxy if
you expose it. TLS, when used, floors at 1.2.

A `SIGTERM` drains in-flight requests and then stops the agents themselves —
each runs in its own process group precisely so a signal to rota does not
reach it, which would otherwise leave them running and spending.

## Models and effort

Each provider carries its own model list, its own effort levels and its own
defaults, so a model belonging to another provider is refused before anything
is spent:

```
codex has no model "gpt-5.6-sol"; it accepts: gpt-5.6-terra, gpt-5.6-luna, gpt-5.5, gpt-5.4-mini
```

| Provider | Models | Effort | Default |
|---|---|---|---|
| `claude` | claude-opus-5, claude-fable-5, claude-sonnet-5, claude-haiku-4-5-20251001 (aliases `opus`, `fable`, `sonnet`, `haiku`) | low, medium, high, xhigh, max | opus-5 + high |
| `codex` | per account — see below | low, medium, high, xhigh, max, ultra | the CLI's choice + medium |
| `kimi` | whatever the account's own config.toml lists — rota advertises none | — | the CLI's own |
| `grok` | grok-4.6, grok-4.5 | low, medium, high, xhigh | grok-4.6 + high |

An alias reaches the CLI as a full id, so a run stays reproducible after an
alias moves. Defaults are sent explicitly rather than left to the CLI, so a
run does not change meaning when the CLI changes its own default.

**codex is per account.** Which models a ChatGPT login may use depends on its
plan, and asking for one outside it fails only *after* the session starts —
`the 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT
account`. So rota reads the entitlement list the CLI caches inside that
account's own home and checks against it, and names no default model at all:
the CLI is better placed to pick one than any fixed answer here.

A provider with no effort setting refuses the field outright rather than
dropping it silently, and never advertises it.

## Providers

| Provider | Login | Quota reported | Launches |
|---|---|---|---|
| `claude` | OAuth + PKCE, paste code; or paste a Console API key | 5-hour, 7-day, per-model; none for a key | `claude` |
| `codex` | OAuth + PKCE, paste redirect URL | none published | `codex` |
| `grok` | paste an API key, or delegate the login | none published | `grok` |
| `kimi` | delegated: its own CLI signs in | none published | `kimi` |

`kimi` is hidden from `rota login`, from `POST /v1/login` and from the
playground's sign-in list: its service has not completed a sign-in for this
build. The SDK still carries it, the schema marks it `hidden`, and an
account already on it still runs.

How each credential reaches its CLI:

| Provider | Route | Verified |
|---|---|---|
| `claude` | the account's own login, in Claude Code's credential store inside a private `CLAUDE_CONFIG_DIR`; `CLAUDE_CODE_OAUTH_TOKEN` for a run with no home, a dead login on a long token, and Windows; `ANTHROPIC_API_KEY` for an account on an API key, always | the environment route live; the stored route built on Claude Code's measured behaviour, to be confirmed live |
| `codex` | private `CODEX_HOME` holding `auth.json` | yes — live |
| `kimi` | private `KIMI_CODE_HOME`, the CLI's own credential inside it | flags verified against the real CLI; the login itself is Kimi's to complete |
| `grok` | `XAI_API_KEY` + private `GROK_HOME` | flags verified against the real CLI; no account to test a run |

The child environment is made unambiguous: every variable rota sets, and
every competing one that would outrank it, is removed before rota's own value
is added. Runtimes disagree on which duplicate wins — libc and Node take the
first, Python the last — so the child only ever sees one. For `claude`,
`ANTHROPIC_BASE_URL` is dropped too: a stray one would send the OAuth token
to whatever host it names — and with it every variable that would sign
Claude Code in some other way or move its credential store
(`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_PROFILE`, the Bedrock,
Vertex, Foundry, Mantle and Claude Platform on AWS switches,
`CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR`, `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR`,
`CLAUDE_CODE_OAUTH_REFRESH_TOKEN`, `CLAUDE_CODE_OAUTH_SCOPES`,
`CLAUDE_CODE_SUBSCRIPTION_TYPE`, `CLAUDE_CODE_RATE_LIMIT_TIER`,
`CLAUDE_CODE_SIMPLE`, which reads no stored login at all,
`CLAUDE_SECURESTORAGE_CONFIG_DIR`, `ANTHROPIC_FEDERATION_RULE_ID`,
`ANTHROPIC_ORGANIZATION_ID` and `CLAUDECODE`), each by its exact name. A launch on
the account's stored login drops `CLAUDE_CODE_OAUTH_TOKEN` as well, so no
token reaches the child at all.

A settings file can put those variables back. Claude Code writes the `env`
block of every settings file it reads into its own environment, over what it
inherited, and `apiKeyHelper` is a command whose output it sends as the
credential, ranked above the account's own login — so `ANTHROPIC_BASE_URL` in
`~/.claude/settings.json`, which every account reads through its mirror,
undoes the drop above. rota edits none of these files and refuses no run for
them, because what they say may be meant. Every launch of a claude account —
`rota run` with or without a prompt, a session, a run or a terminal of
`rota serve`'s — reads the settings files that launch's Claude Code will read:
`settings.json` in the configuration directory it runs in;
`.claude/settings.json` and `.claude/settings.local.json` in the directory it
starts in, and the local one at the root of the git repository that directory
is in (the main checkout's, for a worktree); and the machine's managed
settings, `managed-settings.json` and the `*.json` files in
`managed-settings.d` beside it, in `/etc/claude-code`,
`/Library/Application Support/ClaudeCode` or `C:\Program Files\ClaudeCode`.
When one sets `apiKeyHelper`, or sets in `env` `CLAUDE_CONFIG_DIR` or any
variable a claude launch drops except `CLAUDECODE` — the proxy and certificate
variables among them — the launch prints one warning naming each such file,
its links resolved, and the keys it sets, never a value:

```
warning: claude/you@example.com: settings Claude Code reads put back what
rota keeps out of this account's environment, so its login may go to another
host or another credential be used in its place:
/home/you/.claude/settings.json sets env.ANTHROPIC_BASE_URL
```

A proxy or certificates in your own settings or the machine's managed ones
are what a corporate network sets on purpose, and Claude Code's own docs put
them there, so those are said as what they are, after anything else the same
launch has to say. In a project's `.claude/settings.json` or
`.claude/settings.local.json`, which come with whatever repository is checked
out, a proxy and a certificate are how that repository could intercept the
token, so there they are named with the risk above:

```
warning: claude/you@example.com: Claude Code will use the proxy or the
certificates its settings name, which rota otherwise keeps out of an
account's environment; nothing is wrong if they are your network's:
/home/you/.claude/settings.json sets env.HTTPS_PROXY
```

A file is read as Claude Code reads it — a byte-order mark at its start
dropped, and one starting `FF FE` read as UTF-16 — and up to the same 2 MiB.
A key set to `""`, which is how a settings file cancels an inherited variable,
is not counted, and a file that is not one JSON object, or is over 2 MiB, is
not read. A run that names `setting_sources` is held to the sources it names,
managed settings counting whatever it says, so a `rota serve` run, which names
none unless asked, is checked against the managed files alone; a hermetic run
has no user settings. A `rota serve` terminal is checked in the directory it
starts in, and the server writes the warning to its log. Some settings are
not read: the macOS configuration profile and the Windows registry, which can
carry managed settings; the settings an account's organisation sends from
claude.ai, which Claude Code caches per login in `remote-settings.json`; and
the output of a `policyHelper` that managed settings name, which rota would
have to run to see. For the SDK, `Store.Prepare` takes the CLI to start
in the calling process's directory, as the handover does; `Store.PrepareIn(ctx,
a, dir)` is the same launch for a CLI started in `dir`.

Every child is also told which account it is: `ROTA_PROVIDER` is the
provider name, `ROTA_ACCOUNT_ID` the account's id as a decimal, and
`ROTA_ACCOUNT` the label rota itself shows — the e-mail, else the shortened
UUID, else `account-N`. All three are set for every provider, and an
inherited one from an outer rota session is replaced, never doubled. They
exist because the credential alone does not say: the billing lands on the
right account, but any e-mail Claude Code or a status line displays is read
from `.claude.json`, which an account shares with you by default and which
names whoever last signed in there. A status line, a hook, or anything else
that wants to name the account should prefer `ROTA_ACCOUNT`.

Every child, whatever its CLI, is told one thing more: `ROTA_CLAUDE_HOME`,
your own Claude Code directory — the directory when yours came from an
explicit `CLAUDE_CONFIG_DIR`, and set but empty when it is the default
`~/.claude`. It is not a secret, and it is how a rota started anywhere inside
that child — in a shell a codex session opened as much as in a Claude Code
window — knows whose directory is yours without guessing: it uses
`ROTA_CLAUDE_HOME` when it is
set and ignores the `CLAUDE_CONFIG_DIR` it inherited — an account's home, or a
hermetic run's throwaway directory. A rota launched by one that did not say
(`ROTA_ACCOUNT_ID` set, `ROTA_CLAUDE_HOME` not) takes the default; only a rota
nobody launched takes `CLAUDE_CONFIG_DIR` for yours.

`codex`: `CODEX_ACCESS_TOKEN` belongs to a separate "Agent Identity" feature
and a ChatGPT OAuth token placed there is refused, so a staged `auth.json`
under a private `CODEX_HOME` is the only route that carries a ChatGPT login.
Its browser lands on a `localhost` page that will not load, because rota runs
no callback server: copy the whole URL out of the address bar and pass it as
the code.

`kimi` is Kimi Code, and it keeps its own credentials — so, like grok, it is
delegated: rota reserves the private directory, `rota login <id>` runs the
CLI's own device flow inside it, and rota holds no token. rota staged a file
into `KIMI_SHARE_DIR` once, which was modelled on the older `kimi-cli` wheel
and is wrong for this program: the variable does not appear in its binary at
all. `KIMI_CODE_HOME` is the one that isolates it.

Its headless vocabulary is the smallest of the four — `-p` for the prompt,
`--output-format text|stream-json`, `-m`, `-S` to resume, and permission
settings that are separate switches (`--plan`, `-y`, `--auto`) rather than a
mode. rota models all of it, and no model list: `-m` takes an alias defined
in the account's own config, so there is nothing for rota to check against
and refusing an unfamiliar name would be inventing a rule.

`rota login <id>` passes anything after the id to the vendor's own login, for
the options rota has no business knowing about — `--region mainland-cn` or
`--region global`, since Kimi's two regions are separate services and a home
signed into one cannot talk to the other.

**Its login does not currently complete**, and the fault is not rota's: run
`kimi login` directly, in a scratch directory, with rota nowhere near it, and
it fails the same way. Measured, it issues a device code, polls once, and
gives up after **fourteen seconds** with "The server had an error while
processing your request" — Kimi's service answering an unapproved code with
an error rather than the `authorization_pending` the standard prescribes and
the binary already understands. Both regions do it, a fresh home does it, and
`KIMI_CODE_INFINITE_RETRY=1` does not help. It succeeds only if the code is
approved inside that window, which it once was.

Everything on rota's side of that is finished and tested: the command lines
it builds are accepted by the CLI itself, the private home isolates it, and
`rota run <id> -i` opens its session so a login can be completed by hand.

`grok` is xAI's own Grok Build CLI, and it is the one provider whose
credential rota cannot always hold. Four routes were tried against the real
binary:

| Route | Result |
|---|---|
| `XAI_API_KEY` | works — the key reaches xAI |
| `GROK_HOME` | works — isolates session, config, memory and worktrees |
| `GROK_API_KEY` (what rota used to set) | the string does not appear in the binary at all |
| a staged `auth.json` | rejected — the file is a map keyed by `issuer::client_id`, not the flat document its field names suggest |
| an external auth provider | the helper is never run for grok's own models |

So grok offers two logins. Paste an API key from console.x.ai and rota holds
it, exactly like every other provider. Or paste nothing, and rota registers a
**delegated** account: it reserves a private directory and signs the CLI in
there itself.

```sh
rota login grok      # press return at the key prompt
rota login 6         # starts grok's device flow in that account's directory
```

`rota login` runs the vendor's own login rather than printing it for you to
copy, because the part that must not go wrong — pointing it at this account's
directory instead of your own `~/.grok` — is exactly the part a person would
have to retype. Afterwards grok keeps its credentials there and refreshes
them itself; rota supplies only the isolation that keeps two accounts apart,
and holds no token. It does read back *who* was signed in, so the account
shows an address in `list` rather than a random handle. A run against an
account that has not been signed in yet says so and names the command,
rather than passing the CLI's own "not signed in" through.

Worth noting for anyone tempted to retry the injection route: the client id
grok's own device flow uses is the very one rota's former flow used. The flow
was never the problem — only the shape of the file the tokens land in.

Grok Build's flags resemble Claude Code's without matching them — the prompt
is a value rather than a switch, the system prompt is an "override", `--allow`
and `--deny` replace the tool lists — so rota speaks a third vocabulary for
it. A test feeds a real command line to the real binary and fails if it
answers with a usage error, which is the only way to know a vendor's flag
still exists.

### One Claude world, one daemon per account

A session is one conversation and the transcript it leaves behind. The daemon
is Claude Code's helper — one per configuration directory — and it is what
hosts background sessions, the agent view, sessions you attach to and the
`N ⧉` sub-sessions. It signs in with whatever login its directory holds.
Started outside rota, by a plain `claude`, it uses yours, so everything it
hosts is billed to you whichever window you typed in.

So every claude account gets a configuration directory of its own:
`~/.rota/homes/claude-<id>`, kept as a mirror of `~/.claude` — or of whatever
`CLAUDE_CONFIG_DIR` already named — with a symlink to every entry that is
yours to share and one to `~/.claude.json`, refreshed on every launch. Claude
Code writes through the links, so the account keeps your settings, memory,
skills, plugins, MCP servers, trust decisions and history, files its
transcripts where they always went, and resumes the same old conversations.
What it does not share is its own running Claude Code: a daemon of its own,
signed in with a login of its own, kept in Claude Code's credential store
inside that directory — see *A login of its own*.

**What accounts share, and what they never do.** Every account has its own
login and its own daemon, and nothing an account's processes authenticate
with is ever read by another's: two accounts running at once cannot confuse
each other's tokens. Everything else is your world, shared by default:
settings, memory, skills, plugins, the configuration file with its MCP
servers and trusted folders — and so also the identity record in that file —
and the conversations, unless the account keeps its own (*Where the
conversations live*). Background sessions and agent teams are part of the
conversations and follow that setting, so an account's agent view lists
whatever conversations it shares. What is never shared, in any mode, is what
belongs to one login or one daemon:

| Never shared | Why |
|---|---|
| `.credentials.json`, and on macOS the keychain item named for the directory | the login and its refresh token |
| `daemon*` | the daemon's lock, log, status and auth cooldown |
| `sessions/` | the registry of live session processes and the keys that open them |
| `bridge-spawn` | Remote Control's own state for this login |
| `statsig`, `mcp-needs-auth-cache.json`, `telemetry` | per-login caches and spools: feature flags, which MCP servers want a sign-in, what is waiting to be reported |
| `policy-limits.json`, `remote-settings.json`, `remote-settings-helper-consent`, `remote-settings-consent.json`, and any name that is one of them followed by a dot, such as `policy-limits.json.signature.json` or `remote-settings.json.synced-at` | the policy and the settings the login's organisation set, cached, with what the login agreed to of them: shared, one organisation's would apply under another account, and two accounts of one organisation would keep replacing each other's consent |
| `.session_ingress_token`, and any name that is it followed by a dot | the token a self-hosted runner gives one session |
| any `*.lock` | locks Claude Code takes for this directory's own processes |
| `.claude.json` and `backups/` — with Remote Control on | the identity Remote Control acts as, and Claude Code's backups of that file |

Shared, any of these would let one account's processes sign in with
another's login, or one window reach a session another account's daemon
hosts and pay for it on the wrong quota. MCP servers that sign in with OAuth
are authorized per account home in both modes, because those tokens live in
each home's credential store.

**One home, one account.** An account's home holds that account's login and
no other: `rota set --config` and `PATCH config_dir` refuse a directory that
is already another account's home, whatever its provider, and a store that
already has two accounts in one home launches neither there until one is
given another — each would read the other's login as a rotation of its own.
One directory under two spellings is one home: on a volume that ignores case,
as macOS's and Windows' do, directories are compared by what they are rather
than by how they are written.

**A directory you chose.** An account given a `--config` directory runs on a
token in its environment there, exactly as in 1.2.0, refreshed by rota as it
always was. rota keeps no login in such a directory: it reads no credential
store there and writes none, runs no `security` command for it, stops no
daemon there, changes no `.claude.json`, and removes nothing from it when the
account is removed. That directory may be your own, and rota cannot always
tell — a server started by launchd sees no `CLAUDE_CONFIG_DIR` of yours — so
it keeps away from every login there. Such an account has no Remote Control
(below). In a home rota made, a login somebody else made there is replaced
when nothing runs there.

A mirror built by an earlier rota that still links a name no longer shared
loses that link — but only on the first launch that finds nothing of Claude
Code's running in that home: a running daemon has the directory open through
the link, and swapping it underneath would split its state. A link in the
mirror is always rota's own, a real entry is the account's.

Four things worth knowing:

- `rota set <id> --config DIR` opts into a fully separate directory instead:
  no mirror, nothing shared with your own, and a token in the environment
  rather than a login of its own (above). Giving an account one reads the
  home rota kept its login in first — Claude Code may have rotated the login
  there since rota last looked, and that rotation is taken and saved — and
  then takes the login out of that home, keychain item first, so no copy of
  its refresh token stays behind; the account goes on with the tokens it
  holds. That is refused while anything of Claude Code's runs in the home,
  and while the home is in the hold. Clearing it with `--clear` gives the
  account the home rota keeps for it, written from nothing on the next
  launch; moving between two directories you chose reads and removes
  nothing. The move goes before anything else the same `rota set` or `PATCH`
  changes, so a refused one leaves the account as it was.
- If the mirror cannot be built — symlinks refused on Windows without
  developer mode, an unwritable home — rota prints a warning and runs Claude
  Code anyway. The run still happens, and its login still decides who pays.
- A daemon already running from a plain `claude` keeps your login until it
  exits, so sessions already open under it go on being billed to you until
  they are reopened under rota — `rota run 12 -- --resume <session-id>`.
  Whatever Claude Code creates inside the mirror later, caches and its own
  daemon files included, is the account's own and is never linked back. If
  there is no `~/.claude` to mirror yet, the account simply starts with a
  fresh one.
- A link can stop being one. Claude Code writes some files by writing a
  temporary file and renaming it over the path, and a rename puts a real file
  where rota's link was: that entry quietly becomes the account's own copy,
  which it reads and writes while yours is never touched again. The next
  launch notices every entry that is a real file where a link belongs, names
  them in a warning and leaves them exactly as they are. Move the file aside
  and the launch after that links it again.

#### Remote Control

Claude Code's own Remote Control — `claude --remote-control`, or
`/remote-control` inside a session — works under a rota account, with two
conditions. It is refused for a token handed over in the environment, so the
account must be running on its own stored login; and it acts as whoever the
directory's `.claude.json` names, which by default is your own file, shared
by every account. So it is a setting:

```sh
rota run 2 --remote-control           # turn it on and open a session with it
rota run 2 --remote-control=laptop    # the same, under that name
rota set 2 --remote-control on        # or turn it on, then /remote-control inside a session
rota set 2 --remote-control off       # and back
```

With it on, the account keeps a `.claude.json` of its own: started as a copy
of yours, without your identity, then given the account's — its uuid, e-mail
and organisation, which is what Remote Control looks for — and from then on
its own. What that costs: MCP servers and trusted folders added inside the
account are seen only there, and ones you add later are seen there only as
far as a one-way fold brings them — every launch adds to the account's copy
the top-level settings, projects and MCP servers yours has and it lacks, and
marks a folder trusted when you have trusted it since. Nothing the account's
copy holds is removed or overwritten, your identity is never brought over,
and your own file is only ever read. Claude Code's `backups/` of the file are
the account's own while the file is.

Turning it off puts the account's copy aside as `.claude.json.own` — never
deleted — and links yours again; turning it on again brings the same copy
back. Either way the switch happens on the first launch that finds nothing of
Claude Code's running in the account's home: until then rota says that the
setting takes effect when the account's running sessions end, and
`rota run --remote-control` refuses rather than start a session whose Remote
Control would act as the wrong account.

An account with `--config` has no Remote Control in 1.3.0: it runs on a
token, which Remote Control refuses. `rota run --remote-control`, `rota set
--remote-control on` and `PATCH remote_control` refuse it, saving nothing —
`it runs on a token because its configuration directory is one you chose, and
Remote Control needs the home rota keeps for an account (clear --config)` —
and so does naming such a directory for an account that has it on. Its
`.claude.json` is never touched. An account on an API key is refused the same
three ways, by name — `runs on an API key, and Claude Code's Remote Control
refuses API keys: it needs a claude.ai login` — see *An account on an API key*.

One caveat is Claude Code's own: a login in another configuration directory
on the same machine can interrupt one account's Remote Control.

### A login of its own

A claude account runs on a login of its own: the access token and the
refresh token behind it, in Claude Code's credential store inside the
account's home — `.credentials.json` there, or on macOS the keychain item
Claude Code names for that directory (`Claude Code-credentials-` and the
first eight hex digits of the SHA-256 of the directory path), which it reads
before the file. Nothing that authenticates is put in the environment.
Claude Code refreshes the login by itself and shares it between everything
it runs in that home — each window, the daemon, the background sessions the
daemon hosts — by its own protocol: whichever refreshes first writes the
store, and the rest read it back. That is what keeps a home's processes
signed in for as long as the login lives, and what Remote Control needs.

**Coming from 1.2.** An account whose login is alive keeps the login rota
holds: the first launch in its home writes that login there, and there is
nothing to do. An account whose login has died comes back with `/login`
inside one of its windows — rota confirms the new login with the provider
and takes it — or with `rota login`.

A token in the environment could not do this: it is frozen into the process
it was given to. A daemon keeps the one it started with for life and hands
it to every background session it starts afterwards; the ordinary token
lasts about eight hours, and the provider revokes an access token the moment
its successor is issued, so everything the daemon hosted ended at `OAuth
token revoked · Please run /login`, and logging in to rota again changed
nothing for the processes already running.

The rule rota keeps is one owner per login. **While anything of Claude
Code's is alive in a home, that home owns the login**: rota reads the store
there and launches on what it finds, and does not write the store, refresh
the login, or rewrite the home's `.claude.json`. Alive means a rota run
holding the account, a `daemon.lock` naming a live process, or a
`sessions/<pid>.json` naming one — and a record nobody can read counts as
alive. When nothing is alive, rota may refresh, and a refresh is followed at
once — after the store is saved — by writing the new login into the home.
There are two exceptions, a home whose daemon has given up and a store Claude
Code blanked: see *Stranded* and *Blanked*, below.

- **Reading.** Before anything uses the account's tokens, the home's store
  is read. A rotation by Claude Code keeps the login's own
  `refreshTokenExpiresAt` and is taken with no network call. A login that
  expires before the account's own token is behind, and so is the one rota
  wrote before a refresh of its own — which rota goes on remembering as its
  own write, however often the home is looked at, because that is what
  proves it spent (*Writing*): the account's current login goes in on the
  next launch that may write. A login with another `refreshTokenExpiresAt` — or any living
  login in the home of an account marked dead — is somebody running `/login`
  inside that home: rota asks the provider whose it is, with that login's own
  access token, and takes it when it is this account's (a dead account comes
  back that way); somebody else's is not taken, the account's own tokens stay
  as they were, and rota says so, naming both. A login Claude Code blanked is
  not taken for the end of the account when it was the account's current one:
  see *Blanked*, below. A store that is there and cannot be read is not an
  empty one: see the hold, below.
- **The hold.** Sometimes the home holds something rota cannot place: a
  login made inside Claude Code that the provider cannot be asked about right
  now (its access token has lapsed, say, the morning after), a store that
  cannot be read while something runs there, no login at all in a home that
  was signed in on the account's own while Claude Code still runs there (a
  sibling window ran `/logout`, or the store was caught mid-replace), or a
  macOS keychain that does not answer — then rota says to unlock the login
  keychain for the session, or to keep a long-lived token for the account.
  In the hold rota refreshes nothing for that account, writes nothing into
  the home and stops nothing there, and says so in one line, such as `Claude
  Code in its home holds a login made there that rota has not been able to
  confirm yet; it is used as it is`. A launch joins that login as it is, and
  as soon as the provider can say whose it is, the next launch or listing
  takes it. Nothing for you to do but launch it; if it never settles,
  `rota login` gives the account a fresh login. With no usable login in the
  home, a run goes on a token only when it has a good one — a long-lived
  token, or an access token that has not expired.
- **A refused token, never again.** A refresh token the provider refused is
  never presented again: the account is dead, and a dead account is refreshed
  nowhere — not by a launch, a listing or maintenance. When its home holds a
  login made inside Claude Code that is waiting to be confirmed, the account
  says so in its reason, and it still launches in its home on that login,
  since starting Claude Code there is what makes it confirmable; once
  confirmed, it is alive again. Otherwise only `rota login`, or `/login`
  inside one of its windows, brings it back. A dead account in the hold whose
  access token has lapsed is left to `rota run` for the same reason, and rota
  presents nothing for it.
- **Writing.** Nothing is written when the home already holds the account's
  current login. Otherwise the login goes in when nothing is alive, when rota
  is rescuing the home (*Stranded*, *Blanked*), or when the home still holds
  the very login rota refreshed away: that one is spent, and every window on it
  would be signed out at its next refresh anyway. Only the
  login entry is replaced, every other secret Claude Code keeps beside it —
  MCP servers' OAuth tokens among them — stays, and the file is written
  privately through a rename. On macOS the new file is written and synced
  first, then the keychain item is removed, then the file is renamed into
  place — a removal that fails changes nothing. The item is read once more
  just before it is removed, and left alone if it is not what rota read: `/login`
  writes it without Claude Code's refresh lock, and a fresh login written in
  that moment is the next look's to take. Claude Code moves the login
  back into the keychain on its next refresh, and rota never writes a
  keychain item. A file left beside a keychain item is stale and is removed,
  unless its login is the newer of the two: when the keychain refuses a write
  Claude Code keeps the login in the file, so that is the one rota reads, and
  the older item is removed instead, when nothing is alive. A blanked item is
  older than any file that holds a login, and a blanked file never wins: rota
  takes the file's login, and since Claude Code reads the item and never the
  file while an item exists, the blanked item goes even with something alive,
  once what it kept beside the login is in the file.
  From the moment the provider answers a refresh of rota's, the home's login
  is spent, so a home that cannot then be read or written is an error naming
  the account. A write is tried a few times, under the refresh lock when there
  is one, what is owed is saved with the token, and the next look of any kind
  finishes it with no other refresh.
  With Claude Code alive in the home, a launch joins the login that is there
  and rota says when the account's current one takes over — when those
  sessions end, or at once in one where you run `/login`; and when the home
  holds no usable login at all — windows from before this version, on tokens
  in their environment — the launch goes on a token too, without Remote
  Control, until they are closed. When that home was never on the account's
  stored login, its refresh token lives in rota's store alone, so an expired
  one is refreshed for that run, as it always was; when it was, a window there
  may still hold the token, and that is the hold. A launch never starts a
  Claude Code that is not signed in.
- **Refreshing.** Usage is read with the token the account has, while Claude
  Code runs or not; a 401 is read as a rotation and the home is read again
  before one more try. Only an expired token needs rota to refresh, and that
  waits for a quiet home out of the hold; until then the account keeps its
  last reading. *Stranded* and *Blanked* come first, at every look, and have
  nothing to do with readings: a reading still fresh, or `--short`, does not
  put them off. A refresh the provider refuses as dead is not believed until
  the home has been read again — a sibling may have refreshed first, and its
  login, confirmed, is taken. A store whose lock a run has released — the
  reading `--with quota` takes after a run — rotates nothing at all, and does
  not so much as try an account's claim.
- **Stranded.** Claude Code's daemon refreshes the login itself, and now and
  then the refresh fails, on a plain `~/.claude` as much as in a home of
  rota's. When the access token has run out by then, the daemon writes
  `{"status":"auth_required"}` into `daemon-auth-status.json` and the sessions
  it hosts say `Login expired · Please run /login`. It goes on retrying every
  thirty seconds without logging a failure, while rota, seeing something alive,
  waited for it to go away. The login can be alive all that time. So rota
  refreshes such a home although it is alive, writes the login into the store
  under whatever is running, and says so in one line. rota expects Claude Code
  to read the login from its store, as its code path suggests it does; no live
  daemon has been observed recovering this way. It takes all of this: rota
  keeps the login in that home, the account is alive and out of the hold, the
  home holds the account's own current login, the store's lock is held,
  something is alive there, the status file says `auth_required` about the
  token that ran out, and that token truly expired at least ten minutes ago.
  What else runs there does not matter: a window open for days, or a run
  holding the account, has had its chance by then. The status file outlives
  recovery, so on its own it means nothing, and one older than the token that
  ran out means nothing either. It is also all rota has to go on: where Claude
  Code does not write it, on its storage-v5 path say, a home is not found
  stranded. A launch heals it too: when the refresh does not get through it
  joins the home as it always did. A refusal goes as any refusal does: the home
  is read again first, a sibling's login is taken, and otherwise the account is
  dead.
- **Blanked.** When a refresh of its own comes back refused, Claude Code
  empties the login in its store, keeping everything else. That is its verdict,
  and it has been measured not to be final: a daemon reported no token for days
  on a login whose next refresh by rota worked. So when the login it blanked
  was the account's current one, and the account is not dead and holds its own
  refresh token, rota presents that token once before calling it dead, whether
  the home is alive or quiet. Nothing in the home is at risk, because the home
  holds no login any more, and no status file is needed. On success the login
  goes in; rota expects whatever is running there to find it, and no live
  process has been observed doing so. On a refusal the account is dead, says
  why, and the token is never presented again: the refusal is saved, tried
  again if the save fails, and kept by the process until it is. Any failure
  that is not a refusal — an error the provider answered with, or no answer at
  all — leaves the account alive and says so, and the next look tries again,
  as every refresh of rota's does; until then a launch goes on a
  token that is good or does not go, because Claude Code started there would
  not be signed in. A blanked login that is not the account's current one is a
  home with nothing usable in it, as ever. An account marked dead before it got
  this chance stays dead: `rota login`, or `/login` inside one of its windows.
- **Watched.** `rota run` with no prompt replaces itself with the CLI, so no
  rota is left to notice a home going stranded or blanked under a window open
  for days. Before that handover, on a claude account whose login lives in its
  home and whose launch took the stored route, it starts one watcher, `rota
  serve watch <id>`: in a session of its own, at `/`, with stdin, stdout and
  stderr on `/dev/null` and no other descriptor — the run's claim is still
  close-on-exec when it starts, so the watcher never holds it — and not the
  CLI's child, nor its parent. The CLI's process, terminal, signals,
  environment and arguments are what they were. Every five minutes the watcher
  takes the store's lock and, while anything is alive in the home (a daemon, a
  live session, a run holding the account), makes the same look `rota list`
  starts with, for that account alone: every rule above holds, and a healthy
  home is left to Claude Code. It goes when the home goes quiet, when the
  account is removed, dead, given another home or moved to the environment
  route, and on SIGTERM or SIGHUP — never in the middle of a look. One watcher
  per home: it holds `.rota-watch.lock` there, and a second goes at once. A
  store it cannot open is tried again next time. What it says goes to
  `watch.log` in the store, moved aside to `watch.log.1` at one MiB. `--share`
  starts the same watcher; a question, a hermetic run, the HTTP API and Windows
  start none, and `rota serve` maintains accounts as it always did beside it.
  No watcher has yet been seen rescuing a home under a live Claude Code.
- **The refresh lock.** Through all of that rota holds Claude Code's own
  refresh lock (`.oauth_refresh.lock` in the home), for one account at a time,
  from reading the home again until the login is in it, and touches it every
  five seconds as Claude Code's own holders do, because Claude Code takes a
  lock for dead after a minute untouched. A lock somebody holds leaves this look
  to them. A lock somebody took meanwhile is theirs, and is neither touched nor
  removed.
- **Changing route.** A launch remembers which route the account's last one
  in the home took, and decides its own before anything else. When it is
  another, and the home's daemon is running with nothing beside it but the
  sessions it hosts, that daemon was started on the other route and would go
  on serving its old credential to every background session, so it is
  stopped (`claude daemon stop --any`, with nothing authenticating in its
  environment), and rota says so. A daemon beside a window somebody has open
  is left alone: stopping it would not make the home quiet. No daemon in a
  directory you chose is ever stopped: rota keeps no login there, so no
  route of its own ever changed. When the launch goes onto the stored login while that daemon
  has to stay, rota says that background sessions started meanwhile use its
  old credential until the home goes quiet.
- **Runs without the home.** A hermetic run and `--stateless` take a
  throwaway directory, so they go on a token in the environment: the
  long-lived one when there is one, else the account's own access token. An
  expired one is refreshed while nothing runs in the home, or while what runs
  there holds no login of its own and the home was never on the account's
  stored login; while a Claude Code there holds the account's login, or in
  the hold, the run is refused and told that a long-lived token is what such
  runs are for.
- **Removing.** `rota remove` first asks of every account it names whether
  it can go, and the asking changes nothing: a window open in a home rota
  made, a record nobody can read, or a run holding the account with no daemon
  alive that could be what holds it refuses, and then none of them is
  touched. Then, account by account, it stops the daemon in a home rota made,
  waits a few seconds for the home to go quiet, and refuses, naming what is
  still alive, if it does not or a run still holds the account; then the
  keychain item goes, and then the directory. A directory you chose loses
  nothing, and nothing running there is stopped.

Your own login — your Claude Code directory, and its keychain item — is
never read or written. Your directory is `~/.claude`, or the
`CLAUDE_CONFIG_DIR` rota was started with when nothing launched it (see
`ROTA_CLAUDE_HOME`). An account with `--config` — your own directory or any
other you chose — stays on a token in the environment, so no login in such a
directory is ever read or written. Two limits: Windows keeps the
environment route in every case, and on macOS a home rota makes whose path is
not plain ASCII — a store kept under such a path — cannot keep a login at
all, because the keychain item's name cannot be
worked out without Unicode normalisation and rota could not follow it — such
an account runs on a token, without Remote Control, and rota says so once per
launch.

#### Where the conversations live

Sharing the conversations is the useful default and not the only answer, so
it is a setting:

```sh
rota set 2 --sessions own          # this account's conversations are its own
rota set 2 --sessions ~/work/chats # or they live here — as may another account's
rota set 2 --sessions shared       # back to the default
```

| `--config` | `--sessions` | where the conversations are |
| --- | --- | --- |
| unset | unset | your own Claude Code directory, through the mirror's links: every account sees and resumes every conversation |
| unset | `own` | the account's own home, as real folders among the links — nobody else reads them |
| unset | a directory | that directory, linked into the mirror |
| set | unset or `own` | wherever that directory keeps them, which is Claude Code's own behaviour; rota touches nothing |
| set | a directory | that directory, linked inside the one you chose |

What moves is a fixed set of entries: `projects`, `file-history`, `todos`,
`session-env`, `tasks`, `jobs`, `teams`, `paste-cache`, `shell-snapshots`
and `history.jsonl`. Those are the things a conversation is keyed by or
derived from — transcripts alone would resume into a session whose edits and
todos had stayed behind — background sessions and agent teams among them.
Everything else in the directory is settings, memory, skills and plugins,
and goes on being shared whatever this says. Claude Code's own `sessions/` is
a different thing entirely: the registry of live processes and their socket
keys, never shared in any mode.

The setting can be changed at any time, and the next launch converges on it:
links are re-pointed, links the new mode does not want are removed, and a
directory the account was pointed at is created before anything is linked to
it. What is never touched is a real entry — conversations an account made
while keeping them to itself stay exactly where they are, and rota says so
rather than replacing them:

```
warning: claude/you@example.com holds its own projects in
/Users/you/.rota/homes/claude-2, where a link to /Users/you/.claude is
expected; the account reads these and not yours. Move them aside to share
again.
```

It is the same warning for anything else in the way, which is how a link that
a rename turned into a real file comes to light: `settings.json` written
through a temporary file and renamed over the link is a copy of its own from
then on, and the next launch names it among the rest rather than letting the
account read a fork nobody knows about. Nothing is moved or replaced either
way — move the entry aside yourself and the next launch links it again.

Give one directory to several accounts and those accounts share their
conversations with each other and with nobody else — a team of accounts on
one project, say, with your own `~/.claude` left out of it. `rota list
--sessions` reads such a folder once rather than filing the same
conversations under each account that names it.

One consequence worth stating: `rota remove <id>` deletes the home rota made
for an account, so an account with `--sessions own` loses its conversations
with it. The command says so as it goes. A directory you named is yours and
is left alone, as a `--config` directory is.

### One run at a time, where the CLI owns the credential

codex, grok and kimi are handed a private home and rewrite the credential file
in it as they go, rotating the refresh token in place. Two runs on one such
account are two processes each believing that home is theirs: the second
staging overwrites the token the first has already rotated to, and the next
adoption reads back a spent one. These providers refuse a spent refresh token
for good, so the account does not recover.

Which providers those are is asked as "does the CLI keep its credentials in
that home", which is two kinds of provider rather than one. A provider that
*adopts* does so because its CLI rewrites the file as it runs. A provider that
*delegates* hands the CLI the whole login, so the credential it obtains lives
there and nowhere else. Kimi is the second without being the first — rota holds
no token of its own for it, so there is nothing to adopt — and its access token
lasts fifteen minutes, which makes it the one whose file is rewritten most
often. Asking only the first question left it unguarded.

rota cannot make that safe — the CLIs assume the home is theirs — so such an
account runs one at a time. The rotation steps past one that is busy, so a
second request is answered by the next account rather than refused; only an
account named by id, or a rotation with nothing else free, is refused:

```
error: account is already running: codex/you@example.com keeps its own
credential file, and two runs would spend the same refresh token
```

Over HTTP that is `409`. It is a refusal rather than a wait, because waiting
would mean holding the account store while an agent runs, which would stop
every other command; and a caller's sensible answer is to name another account
or come back, not to queue.

Everything that writes into that home takes the same claim: a run, the
interactive handover, the maintenance below, and `rota login <id>`, which hands
the home to the vendor's own login and is the one write certain to replace the
credential file outright. `rota remove` refuses while an account is running
rather than deleting the directory its agent is authenticating from — nothing
there is undoable once the files are gone, and for a delegated account that
directory *is* the credential. Removing several checks the whole set before
deleting any of them, so a refusal partway through cannot leave the earlier
ones destroyed on disk and still listed.

The handover is the awkward one. It replaces rota with the CLI through
`execve`, and Go opens every file close-on-exec, so the claim would be dropped
at exactly the moment it starts to matter. The flag is cleared for the
handover alone, immediately before it, and the kernel releases the claim when
the CLI finally exits, however it exits. Everywhere rota stays the parent — a
run, an open session, a terminal on the server, `--share` — the claim keeps
the flag and rota holds it until the run ends, so a child started meanwhile,
another account's CLI or a shell, never inherits it and never keeps an
account looking busy.

The same lock holds the background maintenance off. A running server renews
tokens and reads usage every two minutes, and both of those rotate a refresh
token: doing that under a CLI that is holding the old copy invalidates it, and
the next thing the CLI does with it is refused for good. Maintenance skips an
account while it runs and catches it on the next pass.

The lock lives in the home the CLI owns, so it holds across rota processes as
well as within one. Claude Code is the exception by design: it runs many
processes on one home and one login, so its runs take the claim *shared* —
any number at once, and the rotation never steps past a claude account
because it is running — and what that claim holds off is anything that would
write its login or refresh it: maintenance, a refresh, `rota remove`. Those
wait for the claim to be free *and* for nothing of Claude Code's to be alive
in the home; see *A login of its own*.

### Private homes, and who owns the token

Codex and Claude Code take credentials as a file, and kimi and a delegated
grok keep their own. rota stages those under `~/.rota/homes/<provider>-<id>/`,
never in the CLI's own config directory, so the account you are logged into
elsewhere is untouched. The directories
persist: they hold the CLI's session history, caches and — for codex — its
model entitlements, and more importantly these CLIs rotate the refresh token
in place. A rotated token thrown away leaves rota holding a spent one, which
providers reject permanently.

So before overwriting a staged file rota reads it back and asks one question:
*is this refresh token the one I wrote, or one the CLI rotated since?* It
remembers a fingerprint of what it staged; a file that differs from both the
store and that fingerprint was rotated by the CLI and is adopted, while a
file that merely lags behind a refresh rota did itself is overwritten. A
fresh login marks the old staged file as superseded, `remove` deletes the
home outright — when it is one rota made; a `--config` directory is the
person's own and is left alone — and a file naming a different ChatGPT
account is never adopted. Account ids are never reused, so a home cannot be
inherited.

A codex home can have a daemon of Codex's own in it. An interactive codex
session starts Codex's app-server daemon in its `CODEX_HOME` by default and
leaves it running when the session ends, and that daemon refreshes the login
itself when it falls due — five minutes before expiry, rota's margin too — and
writes the rotated tokens to `auth.json`. While Codex's record of it there
(`app-server-daemon/daemon.pid`) names a live process, the home owns the login:
rota reads back what the daemon wrote and keeps it, and refreshes nothing
itself, neither for a run nor in a server's maintenance, because the daemon
would then present a refresh token rota had already spent. A run stages the
login as ever, and the CLI refreshes a token that is due when it starts. An
`auth.json` that already holds the login rota would write is left as it is,
so what Codex adds to the file stays and a rotation the daemon has just
written is never written over. Once the daemon is gone the login is rota's to
refresh again. A server's maintenance first writes the login into the home and
saves the store, and refreshes nothing in a sweep that cannot save; what it
refreshes is saved at once and then goes into the home, as a run's refresh
does, so a daemon started there later reads the token that is still good. A
token read back from the home, or the store's own token found there, is
recorded as the one the home holds, the same as one rota wrote, so a file rota
has since refreshed past is never taken for a newer rotation and read back over
the good token. Every release up to 1.4.0-beta.5 read such a file back once
the server's maintenance had refreshed a token Codex rotated in the home, and
the account went to `re-auth needed`.

## Quota

Only claude publishes a usage endpoint, and only for a login: a claude
account on an API key has none, is never asked about, and shows `api key`
under USAGE. Readings are cached for **five
minutes** and refreshed on the first request past that age; `list -r` and
`?refresh=1` force one. A reading taken before one of its windows resets is
refreshed on the first request after the reset (five seconds after, for the
provider to roll the window over), however young it is. A reading taken
after the reset it reports is not: a provider slow to roll a window over is
asked again when the five minutes are up, not on every look. Both listings
say when each account's limits were last read, and distinguish *not read yet*
from *this provider has no limits to read*:

```
┌───┬────┬────────┬─────────────────┬──────────────────┬───────┬─────────┬────────┐
│ # │ ID │ CLI    │ ACCOUNT         │ USAGE            │ UNTIL │ CHECKED │ STATUS │
├───┼────┼────────┼─────────────────┼──────────────────┼───────┼─────────┼────────┤
│ 1 │ 1  │ claude │ you@example.com │ 5h 5% (1h 11m)   │ 100%  │ 2m ago  │ ok     │
│   │    │        │                 │ 7d 90% (11h 51m) │       │         │        │
├───┼────┼────────┼─────────────────┼──────────────────┼───────┼─────────┼────────┤
│ 2 │ 3  │ codex  │ you@example.com │ -                │ 100%  │ n/a     │ ok     │
└───┴────┴────────┴─────────────────┴──────────────────┴───────┴─────────┴────────┘
```

`#` is the account's place in the rotation, `-` when it is out of it. Each
usage window has a line of its own under USAGE, most important first; rows
that span lines are ruled apart, one-line rows (as in `--short`) are not.
A terminal narrower than the table gets wrapped cells: STATUS first, to 24
columns, and then ACCOUNT, to 16, an address breaking before its "@".
Narrower still, CHECKED and then UNTIL are left out whole; then STATUS wraps
on to 16 columns, and only then is CLI left out. `--short` leaves out CLI and
then CHECKED, once ACCOUNT is down to 16. Past that STATUS and ACCOUNT wrap
on to 10 columns, and only then is USAGE cut, to 8, between the words of a
reading, each line keeping its start and ending in one "…"; narrower yet,
STATUS and ACCOUNT and then USAGE go down to the width of their headers. A
header is never cut, and neither are `#` and ID. A note under the table names
every column left out, which a wider terminal or `rota list --json` shows.
The notes under the table wrap at their spaces too, and a pipe or a file gets
the table as it is, with nothing left out.
Widths are counted in the columns a character takes on screen, two for
Chinese, Japanese, Korean and most emoji, a flag or a family of several
characters among them, so a name in them lines up too.

A spent unscoped window shows as `limited`; a dead credential as `re-auth
needed`; an API key a run found out of credit as `out of credit` with how long
the rotation still passes it by, `limited` in JSON (see *An account on an API
key*). Model-scoped windows (a separate weekly Fable budget, say) are shown
but never count as spent, because nobody knows which model a session will
use.

## Token lifetimes

Measured, not assumed — from live accounts:

| Provider | Access token | Refresh token |
|---|---|---|
| `claude` | 8 hours | rotates occasionally; `invalid_grant` means dead |
| `codex` | 10 days | rotates on **every** refresh; a reused one is rejected permanently |
| `kimi` | **15 minutes** | not published |
| `grok` | never | n/a — it is an API key |
| `claude` on an API key | never | n/a — it is an API key |

rota refreshes a token within five minutes of expiry, and only while running
a command — and a claude login kept in its home only while nothing of Claude
Code's runs there, because then Claude Code refreshes it itself, unless that
home is stranded or blanked; and a codex login only while no daemon of
Codex's is alive in its home, for the same reason. A refresh is never retried
within one look: if the reply was lost, the retry would reuse a rotated token
and kill the lineage. Treat `re-auth needed` as normal rather than as a
fault. The refusal that ended the lineage is kept: `rota
list` shows it in parentheses after `re-auth needed` — `re-auth needed
(invalid_grant: refresh token reused)` — and JSON carries it as `deadReason`.

### A token that outlives the window

A claude account's windows and daemon no longer need one: they run on the
account's own login in its home, and Claude Code keeps it fresh for as long
as the login lives (*A login of its own*). What still takes a token in the
environment is everything without that home — a hermetic or `--stateless`
run, which gets a throwaway directory — and an account whose login has died.
A token given that way is frozen into the process: Claude Code refuses by
design to adopt another after a 401 on it, so an eight-hour one simply ends a
long run. For those, there is a second credential for the same account, good
for a year:

```sh
rota login --long             # prints a login id and a URL
                              # approve in the browser AS THAT ACCOUNT
rota login <login-id> <code>  # finish it
```

```
long-lived token stored for #12 claude/manager.ican@gmail.com, good until 2027-09-21; a run without the account's home, or on a dead login, uses it from now on.
```

Those runs then use it, and nothing else does. Usage, rotation and identity
stay with the ordinary login, which is the one that can read them: by the
provider's own design a long-lived token is inference-only, so it can drive
Claude Code and cannot read a profile or a usage endpoint — nor be used for
Remote Control. It is not a replacement for `rota login claude`; it is a
second key to the same door.

Because it is a credential of its own, a run that uses it does not refresh
first — a provider that refuses a spent refresh token no longer stops a run
that never needed it. That also softens the dead-account rule in one narrow
place: an account whose ordinary login has died still runs **when you name
it** — `rota run 12`, a handover, an HTTP request with an account id — on the
long token, and rota says once, on stderr or in the server log, that usage is
unknown, why the login died, and when the long token runs out. The rotation
still skips dead accounts: naming one is a decision, being handed one is not.
A login that dies on the refresh a launch makes falls back the same way.

Which account the token belongs to is never guessed. The exchange says who
approved, and that identity must match an account rota already holds;
approving as anybody else is refused and nothing is stored, as is a reply
that names no account at all. So log the account in normally first.

`rota list` says nothing about the token for eleven months, then one line
while it is within thirty days of expiry, and one more if it lapses — after
which those runs are back on the eight-hour token. It says the same about the
ordinary login in its last five days, when Claude Code has recorded when it
ends, and in the last three the STATUS column says so too — `ok (login ends in
2d)`, then `ok (login ended 3h ago)` once it is over, because sessions that
outlive a login stop making progress and cannot recover by themselves. `--json` and `GET /v1/accounts` carry `long_until` and `login_until`
(RFC 3339); the token itself is never printed, logged or sent anywhere.
`rota set <id> --long forget` throws it away, leaving the ordinary login
alone.

One risk, plainly: a year-long credential sitting in `~/.rota/accounts.json`
is worth far more to a thief than an eight-hour one. If that file ever
leaves your machine, revoke the token from the provider's account settings —
and treat the file the way you treat a private key.

The same token can be made by Claude Code itself, with `claude setup-token`,
and pasted instead of approved through rota:

```sh
claude setup-token            # Claude Code's own: prints a token, sk-ant-oat…
rota login 12 --token         # paste it; it is not shown as you type
```

It goes on the account you name, into exactly the field `rota login --long`
fills, good for a year counted from the moment you paste it. Such a token is
inference-only and cannot say whose it is or when it was made, so rota cannot
check either, and says so in one line when it stores it: a token from another
claude.ai account would bill that account, and one made months ago ends sooner
than rota shows. Name the account you approved as, and paste it when it is new. It never creates an account, an account on an API key
takes none, and an API key pasted here is refused by name, from its prefix on
this machine, without being sent anywhere.

### An account on an API key

A key from the Claude Console makes a claude account of its own, billed per
token to that Console's API credit rather than to a plan:

```sh
rota login --key                    # paste it; it is not shown as you type
rota login --key --label "work credit"
echo "$KEY" | rota login --key      # or one line of standard input
```

```
Added claude account 7 (api key ...WXYZ): it runs on an API key, and every run on it is paid for from that key's credit.
It is out of the rotation, so it runs only by id (`rota 7 "..."`); `rota set 7 --order last` puts it in.
Claude Code asks once, the first time it opens on this account, whether to use the key: say yes.
```

The key is never taken from the command line, never printed, logged or put in
an error; `--label` names the account, and otherwise its last four characters
are the only part of it anything shows. `list --json` and the HTTP listing carry
raw fields only — `name`, `email`, `uuid`, never the table's display label — so
an unnamed key account has `"credential":"apikey"` and no name, and `api key ...`
and the last four characters exist in the table alone. It is stored in `accounts.json` beside
every other credential, mode 0600. It is checked once, at login, with one
request to the API's model list: a `401` or `403` refuses it, and nothing is
stored; anything else — no network, a timeout, a server error — takes it with a
warning. A key starts `sk-ant-api` and a `claude setup-token` token
`sk-ant-oat`, which is how Claude Code itself tells them apart; rota reads the
prefix only to refuse the wrong one with a sentence. The account is named by a
fingerprint of the key, so pasting the same key again is the same account — a
new `--label` renames it, and nothing else you chose about it changes.

**It starts out of the rotation** (order 0), because every run on it costs
money: it runs when you name it, and `rota set <id> --order <place>` puts it in
the queue like any other account. In the queue it is unmetered — no usage is
ever read for it, so it is never spent — and USAGE says `api key` rather than a
percentage nobody measured.

**When its credit runs out**, the API refuses every request with `Your credit
balance is too low to access the Anthropic API`, before any work and without
charging, and Claude Code ends the run on it: `rota run` prints Claude Code's
`Credit balance is too low` and exits as it did, and the server answers as for
any failed run — `502` for a buffered one, an error at the end of a streamed
one. Only Claude Code's own verdict counts, its error result saying exactly
that, never an answer that quotes it. rota writes down what that run said once it
has ended, never in the middle of one: for an hour the account is **out of
credit**, and the rotation passes it by, as it passes by a spent window.
`rota list` shows `out of credit (59m)` under STATUS and a note saying what
ends it; JSON says `"status":"limited"` with `no_credit_until`. It still runs
by id. The hour is over early when a run on it is answered by the API — buy
credit in the Console, under Billing, then `rota 7 "..."` — or when the key is pasted again
with `rota login --key`. After the hour it is tried again like any account,
and a run still refused starts another hour. Only a run rota reads the end of
counts: a question, `--input`, and both over HTTP. A run handed to the CLI's
own terminal — `rota run 7` with no prompt, `-i`, `--`, `--share`, the web
terminal — ends where rota cannot see it, and says nothing about the credit.

It runs like any claude account, in its own home, with your settings and
conversations as the `--sessions` setting says, its own daemon, hermetic and
headless runs included — on the environment route, with the key in
`ANTHROPIC_API_KEY`, which Claude Code sends as `X-Api-Key`. Every other
credential variable — `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_AUTH_TOKEN`, the
Bedrock and Vertex switches, a base URL and the rest — is dropped, so the key
is the only one. No login is written into its home, nothing is refreshed, and
a long-lived token is never used for it.

What it cannot do:

- **Remote Control.** Claude Code refuses API keys for it, so `--remote-control`
  and `rota set --remote-control on` refuse such an account by name.
- **Usage.** There is no usage endpoint for a key; the Console shows what it spent.
- **A long-lived token.** That is a claude.ai login's, and this is not one.
- **claude.ai features** that need a login, such as claude.ai connectors.

The first time Claude Code opens interactively on such an account it asks
whether to use the key, once. Say yes. Claude Code records your answer in
`customApiKeyResponses` in `.claude.json`, keeping the key's last twenty
characters there, as it does for any key in `ANTHROPIC_API_KEY` — and a key
account's `.claude.json` is your own, shared with every account, like any
claude account's without Remote Control: rota makes nothing more of an account
private than its tokens and its daemon. So the approval is in your shared file,
and any account, or your own Claude Code, that is later handed the same key is
not asked again. `/config` has the toggle to change it. A headless run (`rota 7
"..."`) uses the key without asking. The agent the CLI starts can read its own
environment, key included, as it can read the token of any other account.

## Storage

By default `~/.rota/accounts.json`, mode 0600, written atomically, with an
exclusive lock held for the duration of each command so two rota processes
cannot overwrite each other's rotated tokens. `ROTA_HOME` moves the
directory. It holds live refresh tokens, and long-lived ones where an
account has one — treat it like a private key.

The rest of what persists is in that directory too, and nowhere else: the
private homes under `homes/` — a claude account's login among them, in
`.credentials.json` there or, on macOS, the keychain item Claude Code names
for that home — the server's own `server.toml`, the socket a
run that stays open is reached through (`runs/<id>.sock`), and, where the
server holds terminals, `terminals/` — the share socket and any recordings,
mode `0600` inside a directory that is `0700`. rota has no database.

On Windows rota builds, runs and locks: store and session state take real
exclusive locks through kernel32's LockFileEx, with the same one-writer
promise flock gives unix. The vendor CLI runs as a child rather than
replacing the process, which is why nothing there needs to survive an exec.
Claude Code stays on a token in its environment there, as it always has.
Two honest gaps remain: process discovery shells out to ps/lsof, which
Windows lacks — running CLIs are reported as unreadable rather than guessed
— and kimi's prompt rides its command line on every platform, because its
CLI offers no other door.

## Using the library

The core takes values and returns values. It stores nothing, so an
application can keep accounts in a database, a request body, or anywhere
else:

```go
import rota "github.com/professor93/rota/lib"

l, _ := rota.Begin(ctx, "claude")     // l.URL to approve, l.Kind how to finish
tok, _ := l.Complete(ctx, code)       // rota.ErrAuthPending: ask again
a := rota.NewAccount(1, "claude", tok)

changed, _ := rota.Refresh(ctx, a)    // in memory; persist if changed
q, _ := rota.Usage(ctx, a)            // nil when the provider publishes none

res, _ := rota.Run(ctx, a, home, nil, rota.Spec{Prompt: "hi", Model: "sonnet"}, nil, os.Stdout)
fmt.Println(res.Result, res.SessionID, res.CostUSD)
```

A credential somebody pasted needs no `Begin`. `Paste(ctx, "claude", s)` tells
the two kinds apart by shape and says which in `Token.Credential`. A Console
API key (`"apikey"`) is checked once against `ClaudeModelsEndpoint` (within
five seconds, or the context's own deadline when that is sooner) and named by
a fingerprint of the key; only the API's `401`/`403` is an error, and any other
failure to ask comes back as `unverified` beside the token. `NewAccount` makes
it an account whose `APIKey()` is true, which `Usage` never asks about and
which `Stage` launches with `ANTHROPIC_API_KEY` alone. `Result.OutOfCredit()`
says a run on it ended because its credit is gone, and `Account.NoCreditUntil`
is where an application keeps until when that verdict stands:
`Account.OutOfCredit()` reads it, `Status()` is `limited` meanwhile, and a key
pasted again clears it. `rotation.Available` passes such an account by, and
`store.Run` and `store.Start` set it for `store.CreditPause`, an hour, when
the run ends that way, and clear it when a run on it is answered, after the
CLI has ended and under the store's lock, taken again. A session's end is
written by a goroutine, so a program about to exit calls `Store.Wait` after
its last session. A `claude setup-token`
token (`"setup-token"`) comes back as the one `BeginLong` would have obtained,
good for a year from now, for `Account.ApplyLong` on whichever account the
caller knows it belongs to. `PasteLong(provider, s)` takes that token alone and
refuses an API key by its prefix, asking nobody. Every other provider refuses
both.

`Run` is one question and one answer. `Start` is the same request left open:
set `Spec.Input` alongside `Stream` and the CLI keeps its standard input, so
the run takes more messages after its first answer — the same process, the
same context, no resume. `Send` delivers one, `Steer` interrupts and then
sends so the message starts the next turn, `Interrupt` only stops what is
running, `Close` says there is nothing more, and `Wait` returns the `Result`
`Run` would have given. `Notices` reports what became of each: `accepted`
when a message has reached the CLI, `answered` when a turn carrying it has
finished, `interrupted` when the CLI acknowledges an interrupt, and `idle`
when nothing is waiting. `SendRaw` writes a line the caller composed in the
CLI's own input vocabulary, untracked. Claude Code is the only CLI with a
streaming input today, so every other one refuses `input` by name. On the
command line this is `--input` and `rota send`; over HTTP it is `"input": true`
and the `/v1/runs` endpoints; over a WebSocket it is the `/ws` routes — all in
"Talking to a running run".

Two things still touch the world, unavoidably: the network, and a
credential staged into the account's own directory — for codex, whose CLI
reads credentials only from a file, and for claude, whose login lives in
Claude Code's own credential store in that directory. `Stage` is the only
core verb that writes — and `StagePlan` is Stage without the disk: the
command plus the credential files as values, for an application that stores
files its own way, with `AdoptFrom` reading them back through any fs.FS.
Providers that pass their credential in the environment stage nothing either
way, and neither does claude given no home.

Claude Code's home is shared by many of its processes at once, and lib says
so: `SharedHome`, `StoresLogin` (whether a launch keeps the login in the
home) and `HoldsLogin` (whether a home's store holds one Claude Code can
use). Its planned store is the login alone — `MergeClaudeCredentials` puts
it into the store already there without touching the rest, and
`Account.StagedWritten` records the write — and it must only be written while
no Claude Code process is alive in that home, which lib cannot see and
`rota/store` can. A login somebody made inside the home comes back from
adoption as a `*NewLogin` for the application to check with the provider and
`Accept` or `Refuse`; nothing of it is taken before. A store that is there and
cannot be read is `ErrUnreadableLogin`, never an empty one. `ClaudeHomeSpent`
says whether a home still holds the login recorded as written there and
since refreshed away — certainly spent, so the account's current login may
go over it even while Claude Code runs there; adoption keeps that record
however often the home is read. `JoinHome` starts
the CLI on whatever login a home holds, writing nothing — for a login the
application must not touch yet. On macOS `Stage` and `Adopt` themselves keep
to the environment route, because Claude Code keeps the login in a keychain
item there that neither can read; the stored route on macOS goes through
`StagePlan` and `AdoptFrom`, with the application handing over the item's
content and removing the item before it writes, as `rota/store` does. A
hermetic run of a command staged on the stored route is given the account's
token on the way, since its throwaway directory holds no login.

Choosing which account to spend is **not** in the library. lib
authenticates accounts, builds command lines and runs them; it takes no view
on which account anyone ought to spend, because that is a policy an
application chooses — a different one might round-robin, or pick by price, or
ask a human. The rule rota itself uses lives in `rotation`, over the same
values:

```go
queue := rotation.Queue(accounts)     // Order >= 1, lowest first, ties by id
a, err := rotation.Pick(accounts)     // the first one not spent or dead
rotation.Sort(accounts)               // the same order, for a listing
rotation.Cutoff(a)                    // its threshold, or DefaultThreshold
a.Percent()                           // the fullest window covering the account
```

`Account.Order` and `Account.Threshold` stay in lib, carried and not
interpreted: a store has to persist them, and what an order of 0 or an unset
threshold means is decided in `rotation`. `Account.Percent` stays too — the
fullest window covering an account is a reading of the provider's own quota,
not a rule about it.

`Pick` reads only what the caller already holds — it makes no network call,
because deciding which account to spend must not depend on a provider being
reachable. Refresh the quotas you want it to see first; `rotation.Choose`
does exactly that, honouring the five-minute cache.

Writing a different rule means importing `rota/lib` and ignoring
`rota/rotation` — nothing in the SDK will argue.

Everything else about a request and its answer is a value too, so a transport
never has to invent shapes: `Spec` and `Result`, `wire.Account` (via `Describe`) for listings, `Field`
for generated forms, `Upload` plus `StageUploads` for files travelling with a
request, and `End` for the last event of a stream.

### Verdicts are typed, not spelled

Every refusal carries a sentinel a program can match, so a transport maps
conditions rather than sentences. The message stays free to be reworded:

```go
switch {
case errors.Is(err, rota.ErrDangerous):      // asked to bypass a permission check
case errors.Is(err, rota.ErrOutsideRoots):   // a path outside what the caller allowed
case errors.Is(err, rota.ErrInvalidRequest): // an unknown model, a bad enum, no prompt
case errors.Is(err, rota.ErrReauth):         // the credential is finished
case errors.Is(err, rota.ErrAuthPending):    // a device login, not approved yet
case errors.Is(err, rota.ErrUnsupported):    // this provider cannot do that at all
}
```

### If you want persistence anyway

`rota/store` is an optional package — a separate import, so ignoring it
costs nothing — that adds the bookkeeping an application would otherwise
write itself: ids that are never reused, matching a login to the account it
belongs to, a lock so two processes cannot overwrite each other's rotated
tokens, and saving *before* a run starts rather than after it fails.

```go
import "github.com/professor93/rota/store"

s, _ := store.Open("")                        // the built-in FileBackend
s, _ := store.NewStore(myDatabaseBackend{})   // or your own bytes
defer s.Close()

l, _ := s.BeginLogin(ctx, "codex")            // parks the login for another process
a, added, _ := s.FinishLogin(ctx, l.ID, code)
errs := s.Refresh(ctx, false)                 // quota for metered providers, cached
res, _ := s.Run(ctx, a, rota.Spec{Prompt: "hi"}, nil, os.Stdout)
```

```go
type Backend interface {
	Load() ([]byte, error)          // the whole accounts blob; (nil, nil) on a first run
	Save(blob []byte) error         // must be atomic
	Lock() (unlock func(), err error)
	HomeRoot() string               // where per-account CLI homes are staged
}
```

Keeping accounts as rows instead of a blob needs no `Backend` at all: use the
library verbs directly, with `rota.MatchIdentity` for the identity rule; never-reused ids are a
one-line rule your own store writes.

### Adding a provider

One file, one interface. The optional abilities are separate interfaces, so a
provider without them simply does not implement them:

```go
type Provider interface {
	Name() string
	Begin(ctx context.Context) (url string, state map[string]string, err error) // state["kind"]: code | device | apikey | delegated
	Complete(ctx context.Context, code string, state map[string]string) (*Token, error)
	Launch(a *Account, home string) (*Command, error)         // Command{Bin, Env, Drop}
}

type Refresher      interface{ Refresh(ctx context.Context, a *Account) (*Token, error) } // ErrDeadToken ends a lineage
type Identifier     interface{ Identify(ctx context.Context, accessToken string) (*Identity, error) }
type Meter          interface{ Quota(ctx context.Context, accessToken string) (*Quota, error) }
type Catalog        interface{ Models() []Model; Efforts() []string; Defaults() (model, effort string) }
type AccountCatalog interface{ ModelsFor(a *Account, home string) []Model }   // the plan decides the list
type Adopter        interface{ Adopt(a *Account, home string) error }         // the CLI rewrote its own file
type HomeSharer     interface{ StoresLogin(a *Account, home string) bool; HoldsLogin(fsys fs.FS) bool; Join(a *Account, home string) *Command } // many processes, one login
type Delegator      interface{ LoginPlan(a *Account, home string) LoginPlan } // the CLI keeps its own credential
```

Register it from `init()` with `rota.Register`. Quota normalizes to named
percentages, so a 5-hour window and a credit balance land in the same table.

## Performance

The paths a run actually touches are benchmarked, so a change that makes rota
slower or greedier shows up as a number (`cd lib && go test -bench . -benchmem`):

| Path | Cost |
|---|---|
| building and checking a command line | ~13 µs, 66 allocations |
| merging the child environment | ~1.5 µs, **1 allocation** |
| reading a streamed run's events | **13 allocations** per 200 events |
| describing every field of every provider | ~9 µs |
| opening the store (per request) | ~116 µs |
| saving it (two fsyncs) | ~13 ms |

Everything above happens once or twice per run, against a run that lasts
seconds — so the only figure that governs anything is the third. The save is
the slowest and stays that way on purpose: it is two fsyncs, one for the file
and one for the rename, because a refresh token that reached the disk only
half way is an account lost. Building a command line
got slower on purpose: it now round-trips the request through JSON to find
which fields were set, which is what lets rota refuse a field the chosen CLI
cannot honour instead of dropping it silently.

The event reader is the one that matters — a long streaming session prints
thousands of lines and rota cares about a handful — so it copies through a
reused buffer and looks for an outcome type before deciding whether to decode
a line at all. Doing that took it from 3,417 allocations per 200 events to 13.

That prefilter is also the one place where being clever nearly cost
correctness: an early version matched `"type":` and then the value, which
silently dropped the entire result when a CLI emitted `{"type": "result"}`
with a space. It now looks for the outcome types anywhere in the line, which
no whitespace or field ordering can defeat, and errs toward decoding — a
needless unmarshal costs microseconds, a lost result costs the run.

## Layout

| File | What it holds |
|---|---|
| `lib/core.go` | The value-in, value-out verbs: Begin, Complete, Refresh, Usage, Stage |
| `lib/account.go`, `lib/accounts.go` | The `Account` model, identity matching, id allocation |
| `lib/provider.go` | The interfaces, `Command`, the registry |
| `lib/catalog.go` | Models, efforts and defaults, per provider and per account |
| `lib/run.go` | `Spec`, `Limits`, `Result`: building a command line and running it |
| `lib/flavors.go` | Which CLI understands which request field, and the refusal when one does not |
| `lib/errors.go` | The typed verdicts every refusal carries |
| `lib/jsonx.go` | How rota reads and writes JSON, and what it keeps from the package v2 replaced |
| `lib/bench_test.go` | What the paths a run touches actually cost |
| `lib/token.go` | `Token`, `Identity`, `Quota`, lenient timestamps, JWT reading |
| `lib/launch.go`, `lib/policy.go` | The child environment, staging adoption, quota rules |
| `lib/claude.go` … `lib/grok.go` | One provider each |
| `lib/claudehome.go` | Claude Code's two routes, reading and planning its stored login, and the credential-store merge |
| `store/` | The optional account store: `Backend`, `FileBackend`, locking, logins on disk, `Maintain`. Outside lib on purpose — an SDK does not need storage |
| `lib/oauth.go`, `lib/httpx.go` | OAuth verdicts, HTTP helpers, PKCE |
| `rotation/` | The queue: order, threshold, and which account a bare run takes. Outside lib on purpose — which account to spend is an application's policy |
| `wire/` | `Upload`, `End`, the account view, and the request vocabulary described for forms. Outside lib on purpose — a library has no opinion about JSON or labels |
| `message/` | Reading a finished answer: blocks, the normalized event vocabulary, `ask`. Outside lib on purpose — the SDK has no business knowing what markdown is |
| `sessions/` | What the vendor CLIs are doing: the runs rota started, the editors holding one open, and the conversations `--resume` could pick up |
| `store/mirror.go` | The Claude world each account gets: what is linked, what stays its own, and the warning for an entry that stopped being a link |
| `store/settings.go` | The warning at launch for a Claude Code settings file that sets back what the launch drops |
| `store/claude.go` | A claude account's login in its home: what is alive there, reading, writing and refreshing it, the daemon, removal |
| `store/watch.go`, `cmd/rota/watch*.go` | The watcher an interactive run leaves: its looks, its lock, starting it in a session of its own, its log |
| `store/claudejson.go` | An account's own `.claude.json` for Remote Control: switching it, the identity in it, and the one-way fold |
| `internal/claudecode/` | The macOS keychain item and `claude daemon stop`, behind variables every test replaces |
| `internal/proc/` | Whether a process is still alive, for the session listing and the store alike |
| `api/server.go`, `api/run.go` | Routing, the token, the rate limit, requests and streaming |
| `api/runs.go`, `api/ws.go` | The runs that stay open, and the WebSocket that carries one both ways |
| `api/principal.go` | Who is asking: the two roles, the route table, the middleware and the cross-site rule |
| `api/session.go`, `api/passwd.go` | Signing in on the page, invite links, health, and how a password is derived |
| `api/terminal.go`, `api/terminals.go` | The terminals this server holds: the pseudo-terminal, the scrollback, the keyboard, and the routes over them |
| `api/share.go`, `internal/share/` | A terminal offered from this machine, and the framing the two ends speak |
| `internal/pty/` | The pseudo-terminal itself, on linux and macOS, and `Supported` everywhere else |
| `api/playground.html` | The page served at `/playground` |
| `api/terminal.html` | The page served at `/terminal` |
| `api/assets/page.css`, `api/assets/page.js` | What both pages are made of in common |
| `api/assets/xterm/` | The vendored terminal emulator, with its licences and its provenance |
| `api/config.go` | `server.toml`: the schema, its defaults, what it validates, and `--print-config` |
| `internal/toml/` | The part of TOML that file uses, and a refusal by name for the rest |
| `docs/server.toml` | The whole schema at its defaults, kept honest by a test |
| `docs/releases/` | What each released version changed |
| `go.work`, `cmd/rota/go.mod` | The command as a module of its own, and the workspace that builds it against the packages beside it |
| `scripts/module.sh` | Checks the line between the two modules, tidies the command's against this checkout, and pins it to a root tag at a release |
| `cmd/rota/main.go` | The command |
| `cmd/rota/share.go` | `rota run --share`: the CLI on a pseudo-terminal, and the link to the server |
| `cmd/rota/serveauth.go` | `rota serve passwd`, `token` and `invite` |
