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
| the command | `./cmd/rota` | all of the above |

The table's short names are this repository's layout; the module paths are
`github.com/professor93/rota` and, for the SDK alone,
`github.com/professor93/rota/lib` — the one line an outside program needs.

`rota/lib` is a Go library and nothing else. You call it with Go values and
get Go values back — begin a login, finish it, refresh a token, read a
quota, stage a credential, run an agent — and it has no opinion about where
accounts are kept, which one you ought to spend, what a form should show, or
how any of it looks as JSON. Those are decisions an application makes, so
they live in packages an application can take or leave. The command and the
HTTP server are two applications that took them; a third is free to keep
accounts in Postgres, pick by price, and never import a line of the rest.

**The whole project has no third-party dependencies.** There is no `go.sum`,
nothing to audit, and nothing to update. Go 1.27 removed the last two
temptation: `encoding/json/v2` is standard library now, so the
hand-rolled JSON layer is gone and nothing here reaches for a
faster JSON package. It was not always so: the server
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
file back afterwards in case the CLI rotated the token in it. That is the act
of running an agent, which is what the SDK is for. Where *accounts* live is
still none of its business.

It also reads no environment at all — a test scans the sources so the rule
cannot erode. The child's environment is what the application passes as
Command.BaseEnv; which variables are secret is the application's fact, kept
in `rota/store` (HideFromAgents, HostEnv), where `ROTA_HOME` is declared by
the package that defines it and `ROTA_TOKEN` by the command. lib never
hears either name.

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
`encoding/json/v2`, and nothing else outside the standard library.

```sh
go build -trimpath -ldflags='-s -w' -o rota ./cmd/rota
ln -sf "$PWD/rota" ~/.local/bin/rota      # or anywhere on your PATH
```

A link rather than a copy, so rebuilding is one command and the next `rota`
you type is the one you just built. `go install ./cmd/rota` works too and puts
it in `$(go env GOPATH)/bin`, but then every rebuild needs installing again.

### On encoding/json/v2

rota reads and writes JSON with `encoding/json/v2`. In Go 1.27 the original
package is still the original implementation — v1 is only rebuilt on top of
v2 under a GOEXPERIMENT — so this is a real change of parser, not a rename.

It is not a drop-in, and `lib/jsonx.go` is the one place that says how rota
handles the five differences that matter here:

| | encoding/json | v2 |
|---|---|---|
| `omitempty` on a number or bool | omits the zero | keeps it |
| `omitempty` on a `RawMessage` | omits when empty in Go | omits when it *encodes* empty, so a deliberate `{}` disappears |
| a nil slice or map | `null` | `[]`, `{}` |
| object names | matched case-insensitively | matched exactly |
| duplicate names, invalid UTF-8 | last wins, replaced | rejected |

The first three change what rota writes, so every affected field now says
`omitzero`, which means under both packages exactly what `omitempty` used to
mean here, and nil slices are still written as null. `lib/jsonwire_test.go`
pins the resulting bytes, because they are the on-disk account store, the
HTTP replies and what `rota --json` prints.

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
rota login <login-id> <code>  # finish it with the code from the page
rota login <login-id>         # finish a delegated login, which takes no code (grok)
rota login 2                  # sign account 2 in through its own CLI
rota list                     # every account, in rotation order, with usage
rota list --short             # the same, without asking any provider anything
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
```

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

Usage is read from the cached quota, refreshed if older than five minutes,
and only for the accounts the queue actually looks at. A provider that
publishes no usage endpoint reports nothing, so it is treated as unspent
rather than dropped — otherwise an unmetered account could never be picked.
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
```

The id is a number rather than a name so that it can never be mistaken for
one of a CLI's own flags, which is not true of any short alias.

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
aged past five minutes. A request should never be the thing that discovers
its credential expired, and the rotation decides from stored numbers — a
stale one sends work to an account that is already spent. Nothing there is
fatal: a provider that cannot be reached leaves its account exactly as it
was, and the next sweep tries again. `--refresh-every 0` turns it off, and
the command line still refreshes what it is about to use.

### One file for the server

Everything above can be written down once instead of typed every time.
`rota serve` reads `server.toml` from the store directory — `$ROTA_HOME/
server.toml`, or `~/.rota/server.toml` — and `--config PATH` reads another.
The default file is allowed not to exist, and then nothing has changed; a
file somebody names has to be there, because naming one that is not is a
typo nobody would otherwise be told about.

The file may hold the bearer token, so on unix rota refuses to start unless
it belongs to its owner alone, and says the command that fixes it:

```
/home/me/.rota/server.toml is readable by other users (mode 0644); it may
hold a token, so fix it with: chmod 600 /home/me/.rota/server.toml
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
listen = "127.0.0.1:8787"   # host:port, or a bare port for every interface
quiet  = false              # log warnings and errors only

[tls]
cert = ""                   # cert and key go together or not at all
key  = ""

[auth]
token       = ""            # the bearer token, if you keep it here
token_file  = ""            # or a file holding it, same permission rule
token_env   = "ROTA_TOKEN"  # or the name of the variable that holds it
session_ttl = "12h"         # how long a sign-in on the page lasts

[routes]
api        = true           # everything under /v1 except the sockets and health
playground = true           # GET /, GET /playground and the invite landing
websocket  = true           # the /ws routes
health     = true           # GET /v1/health, open and depending on nothing
terminal   = false          # the /v1/terminals routes; off until asked for

[terminal]                  # nothing here means anything unless routes.terminal
shell            = false    # also allow a plain login shell, not only CLIs
max_sessions     = 8
scrollback_bytes = 2097152  # output kept per terminal, for whoever attaches
idle_timeout     = "12h"    # a terminal nobody is attached to ends after this
record           = false    # keep each terminal's output under <store>/terminals/
record_max_bytes = 52428800

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

Three of those keys — `input_timeout`, `input_grace` and `replay` — have no
flag at all: a run that stays open was only ever configurable from the
library until now.

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

| Method | Path | |
|---|---|---|
| `GET` | `/v1/health` | unauthenticated and never rate-limited: `{"ok":true}` and nothing else. Its own group, so it survives switching the page off |
| `GET` | `/` | unauthenticated: what this is and its version. With the rest of the `playground` group |
| `GET` | `/playground` | the playground, a single self-contained page |
| `GET` | `/v1/schema` | every provider, its models, efforts, defaults and fields |
| `GET` | `/v1/accounts` | accounts in rotation order, with usage, status, order, threshold and when limits were read (`?refresh=1`); `default` names the one a bare run would use |
| `GET` | `/v1/accounts/{id}/schema` | the models *that* account may actually use |
| `POST` | `/v1/run` | run a prompt on whichever account the rotation picks |
| `POST` | `/v1/accounts/{id}/run` | run a prompt on that account |
| `GET` | `/v1/runs` | the runs staying open right now, and the ones that have just ended |
| `GET` | `/v1/runs/{id}` | one of them: its account, state, how many messages are waiting, whether anyone is reading |
| `GET` | `/v1/runs/{id}/events` | attach to its stream, `?since=N` replaying what was missed |
| `POST` | `/v1/runs/{id}/messages` | `{"text":"…","steer":false}` — one more message into it |
| `POST` | `/v1/runs/{id}/interrupt` | stop the tool it is running |
| `POST` | `/v1/runs/{id}/close` | no more messages: it finishes its turn and exits |
| `GET` | `/v1/ws` | a WebSocket that starts a run on whichever account the rotation picks and carries it both ways |
| `GET` | `/v1/accounts/{id}/ws` | the same, on that account |
| `GET` | `/v1/runs/{id}/ws` | attach a WebSocket to a run already going, `?since=N` replaying what was missed |
| `GET` | `/v1/terminals` | the terminals running right now, and the ones that have just ended |
| `GET` | `/v1/terminals/{id}` | one of them: what it runs, who is watching, who holds the keyboard |
| `POST` | `/v1/terminals` | `{"account":1,"args":[],"cwd":"…","cols":120,"rows":40,"label":"…"}` — start one |
| `DELETE` | `/v1/terminals/{id}` | end one: a hangup, then a kill three seconds later |
| `GET` | `/v1/terminals/{id}/ws` | attach to one, `?since=N` in bytes and `?mode=watch` to look without taking the keyboard |
| `PATCH` | `/v1/accounts/{id}` | `{"order":1,"threshold":80,"cwd":"/srv/api","config_dir":"/srv/homes/api","sessions":"own","long":"forget"}` — its place in the rotation, when to move on, where it belongs, where its conversations live (`shared`, `own`, or a directory — confined exactly as `config_dir` is), and `"long":"forget"` to throw away its long-lived token |
| `DELETE` | `/v1/accounts/{id}` | forget it, and delete the home rota made for it, staged credentials included; a `config_dir` somebody chose holds their memory and skills and stays |
| `POST` | `/v1/login` | `{"provider":"claude","long":false}` → `{id, url, kind}`; `"long":true` asks for a long-lived token instead |
| `POST` | `/v1/login/{id}` | `{"code":"..."}` → the account, or `{"status":"pending"}`; a long login answers `{"status":"long","long_until":"..."}` |
| `GET` | `/v1/session` | who this request is: `{name, role, via, expires}`, or `401` |
| `POST` | `/v1/session` | `{"name":"…","password":"…"}` → the same, and the session cookie |
| `DELETE` | `/v1/session` | end this session |
| `POST` | `/v1/invites` | `{"ttl":"10m"}` → `{"url","expires"}` — a single-use link that signs somebody in as a watcher |
| `GET` | `/invite/{code}` | unauthenticated: spends one of those and lands on the page |

`/v1/auth` and `/v1/auth/{id}` are the same two under their old names, kept
working for anything already calling them.

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
Closing the connection kills the CLI and everything it started. What travels
down it is rota's vocabulary, not the vendor's — see below.

Files can travel with the request — either `"files": [{"path":"a.txt",
"content":"<base64>"}]` or a multipart body whose `request` part is that same
JSON. They land in a directory private to the request, which is added to the
session and deleted afterwards.

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

That is one table in the code as well — route to role, consulted by the
middleware, checked against the routes actually registered by a test — so
the terminal, when it arrives, is rows in it rather than checks scattered
through handlers. A route nobody wrote a row for takes `control`.

A principal whose role does not cover what it asked for gets `403` and the
sentence *this sign-in may only watch; nothing here can be changed from it*.
It is not `401`, because there is nothing to try again with, and it is not
counted as a guess: somebody who proved who they are and clicked the wrong
thing must not cost their address its access.

#### The principals

The token `rota serve` is started with — `--token`, `ROTA_TOKEN`,
`[auth] token`, `token_file`, `token_env` — is the principal `token`, with
the role `control`. Nothing about it has changed.

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

Names are unique within each table and non-empty; a role is one of the two
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
but the code that meant to make it.

#### Invite links

| Method | Path | |
|---|---|---|
| `POST` | `/v1/invites` | (`control`) `{"ttl":"10m"}` → `{"url","expires"}`; ten minutes by default, a day at most |
| `GET` | `/invite/{code}` | unauthenticated; spends the code, signs the visitor in as a watcher, redirects to the page |

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
{"type":"error","error":"unknown field \"prompts\""}
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
conversations live within all this; see [One Claude world, one daemon per
account](#one-claude-world-one-daemon-per-account).

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
the one route group that is. Turning it on takes two lines:

```toml
[routes]
terminal = true   # needs api and websocket

[terminal]
shell            = false    # also allow a plain login shell
max_sessions     = 8
scrollback_bytes = 2097152  # output kept per terminal, for whoever attaches
idle_timeout     = "12h"    # a terminal nobody is attached to ends after this
record           = false    # keep each terminal's output under <store>/terminals/
record_max_bytes = 52428800
```

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
- Every change is broadcast as `{"type":"keyboard","holder":"alice"|null}`.

Anything a non-holder sends inward — bytes, or a resize — is answered with
`{"type":"error","message":"you are not holding the keyboard"}` and changes
nothing at all.

#### The routes

| | |
|---|---|
| `GET /v1/terminals` | every terminal, running and recently ended (watch) |
| `GET /v1/terminals/{id}` | one of them (watch) |
| `GET /v1/terminals/{id}/ws?since=N&mode=…` | attach (watch) |
| `POST /v1/terminals` | start one (control) |
| `DELETE /v1/terminals/{id}` | end one: a hangup, then a kill three seconds later (control) |

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
{"id":"9a4c1e77b2d05f31","kind":"account","account":{"id":1,"label":"you@example.com","provider":"claude"},
 "label":"fintech","cwd":"/srv/work","started":"2026-09-21T10:11:12+05:00","cols":120,"rows":40,
 "holder":"alice","viewers":[{"name":"alice","role":"control","since":"2026-09-21T10:11:14+05:00"}],
 "offset":20418,"ended":false,"recording":false,"token_until":"2027-03-04T00:00:00Z"}
```

`token_until` is when the login inside will lapse — the long-lived token's
date where the account has one, the access token's expiry otherwise — so
somebody looking at a terminal that has been open all day can see it coming.
`offset` is how many bytes it has printed altogether, which is what `since`
is measured in.

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
| `hello{id,kind,cols,rows,offset,holder,you:{name,role,conn},viewers}` | the first frame, always |
| `keyboard{holder}` | who holds it now, `null` for nobody |
| `claim_request{by,id}` | somebody is asking the holder for it |
| `resized{cols,rows}` | the terminal is a different size now |
| `viewers{list}` | who is attached: name, role and since — never an address |
| `gap{from,to}` | `since` was older than the scrollback; these bytes are gone |
| `exit{code}` | the CLI ended |
| `error{message}` | a frame that was read and will not be acted on |
| `pong` | the answer to `ping` |

In:

| | |
|---|---|
| *binary* | input: the bytes of somebody typing, control characters and all |
| `resize{cols,rows}` | holder only |
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
comes first and says exactly which bytes were lost.

**Falling behind.** Each connection has a bounded queue and the terminal
never waits for one: a reader that stops reading is closed with `1013` and
reattaches with `since`. One slow watcher cannot stall the CLI or anybody
else watching it.

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

#### Limits, the audit log and recording

`max_sessions` caps the terminals running at once; past it, `POST` answers
`409` and says which setting it was. `idle_timeout` ends a terminal nobody
has been attached to for that long — measured from the last person leaving,
not from the last keystroke. Stopping the server ends every terminal: each
one is a CLI in a session of its own, which is exactly what a signal to rota
does not reach. An ended terminal stays listed for five minutes, with its
exit code, so a client reattaching a moment late reads the end rather than a
404 it cannot tell from a wrong id.

The audit log is always on, and never carries a keystroke or a line of
output: `terminal created` (id, kind, account, by), `terminal attached` and
`terminal detached` (id, name, role), `keyboard taken`, `keyboard released`,
`keyboard granted` and `keyboard forced` (id, from, to), `terminal killed`
(id, by) and `terminal ended` (id, exit).

With `record = true`, what each terminal **printed** is appended to
`<store>/terminals/<id>.out`, up to `record_max_bytes`, with the description
above written beside it as `<id>.json` when it ends. Input is never recorded
— what reaches the file is the same bytes every attached client was sent, and
nothing else. The files are mode `0600` and the directory `0700`: a recording
is a transcript of somebody's working session.

### The playground

`GET /playground` serves a page with no credential of its own. It asks
`GET /v1/session` first. Signed out, it offers a name and a password, with
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

Light and dark follow the system, with a toggle that overrides it. The page
is self-contained: no fonts, scripts or styles from anywhere else, because
it is usually served on a loopback address with no route out. A test runs
its actual JavaScript against this server's own schema — typing, toggling,
filtering, building a schema, running, streaming, reordering the rotation —
so a changed field or response shape cannot break it silently.

### Safety

The server assumes its caller holds a credential but is **not** trusted with
the machine. That assumption is what the following exist for.

Two things about credentials, before the rest. **Without TLS a password
travels in clear text exactly as the token does** — the server warns at start
when `[[users]]` are configured, no certificate is given and the address is
not the loopback. And **`control` is the power to run commands on this
machine**: the agents this server starts have a shell, so handing somebody
that role is handing them the machine, and `watch` exists so that showing
somebody what is happening does not have to be.

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
| `claude` | OAuth + PKCE, paste code | 5-hour, 7-day, per-model | `claude` |
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
| `claude` | `CLAUDE_CODE_OAUTH_TOKEN` | yes — live |
| `codex` | private `CODEX_HOME` holding `auth.json` | yes — live |
| `kimi` | private `KIMI_CODE_HOME`, the CLI's own credential inside it | flags verified against the real CLI; the login itself is Kimi's to complete |
| `grok` | `XAI_API_KEY` + private `GROK_HOME` | flags verified against the real CLI; no account to test a run |

The child environment is made unambiguous: every variable rota sets, and
every competing one that would outrank it, is removed before rota's own value
is added. Runtimes disagree on which duplicate wins — libc and Node take the
first, Python the last — so the child only ever sees one. For `claude`,
`ANTHROPIC_BASE_URL` is dropped too: a stray one would send the OAuth token
to whatever host it names.

Every child is also told which account it is: `ROTA_PROVIDER` is the
provider name, `ROTA_ACCOUNT_ID` the account's id as a decimal, and
`ROTA_ACCOUNT` the label rota itself shows — the e-mail, else the shortened
UUID, else `account-N`. All three are set for every provider, and an
inherited one from an outer rota session is replaced, never doubled. They
exist because the credential alone does not say: Claude Code's `/status`
does report `Auth token: CLAUDE_CODE_OAUTH_TOKEN`, and the billing lands on
the right account, but any e-mail it or a status line displays is read from
the shared `~/.claude.json`, which names whoever last signed in through the
keychain. A status line, a hook, or anything else that wants to name the
account should prefer `ROTA_ACCOUNT`.

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
`N ⧉` sub-sessions. It logs in on its own. Started outside rota, by a plain
`claude`, it has no token of rota's and falls back to the keychain login, so
everything it hosts is billed to that account whichever window you typed in.

So every claude account gets a configuration directory of its own:
`~/.rota/homes/claude-<id>`, kept as a mirror of `~/.claude` — or of whatever
`CLAUDE_CONFIG_DIR` already named — with a symlink to every entry and one to
`~/.claude.json`, refreshed on every launch.
Everything is shared except the `daemon*` files, `.credentials.json` and
`sessions/`, which stay the account's own — and the conversations, which are
shared until the account says otherwise. Claude Code writes through the
links, so the account keeps your settings, memory, skills, plugins, trust
decisions and history, files its transcripts where they always went, and
resumes the same old conversations. What it does not share is the daemon: it
starts one for that directory, and that one inherits the account's token.
`/status` inside it reports `Auth token: CLAUDE_CODE_OAUTH_TOKEN`, and
`ROTA_ACCOUNT` names the account for a status line. `sessions/` is the
registry of live sessions — each process's socket and the key that opens it
— and is kept apart for the same reason as the daemon: shared, it would let
a window of one account attach to a session another account's daemon hosts,
and pay for it. So the agent view under an account lists that account's
background sessions, and no other's. A mirror built by an earlier rota that
still links a name no longer shared loses that link on the next launch: a
link in the mirror is always rota's own, a real entry is the account's.

Four things worth knowing:

- `rota set <id> --config DIR` opts into a fully separate directory instead:
  no mirror, and nothing shared with your own.
- If the mirror cannot be built — symlinks refused on Windows without
  developer mode, an unwritable home — rota prints a warning and runs Claude
  Code in its own directory and daemon, as it did before. The run still
  happens, and the token still decides who pays for it.
- A daemon already running from a plain `claude` keeps its own login until it
  exits, so sessions already open under it go on being billed to that login
  until they are reopened under rota — `rota run 12 -- --resume <session-id>`.
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
todos had stayed behind. Everything else in the directory is settings,
memory, skills and plugins, and goes on being shared whatever this says.
Claude Code's own `sessions/` is a different thing entirely: the registry of
live processes and their socket keys, never shared in any mode.

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
at exactly the moment it starts to matter. The flag is cleared for that one
file, and the kernel releases it when the CLI finally exits, however it exits.

The same lock holds the background maintenance off. A running server renews
tokens and reads usage every two minutes, and both of those rotate a refresh
token: doing that under a CLI that is holding the old copy invalidates it, and
the next thing the CLI does with it is refused for good. Maintenance skips an
account while it runs and catches it on the next pass.

The lock lives in the home the CLI owns, so it holds across rota processes as
well as within one. Claude Code is not affected: rota passes its token in the
environment, so two runs share nothing.

### Private homes, and who owns the token

Three providers hand credentials over as a file. rota stages those under
`~/.rota/homes/<provider>-<id>/`, never in the CLI's own config directory, so
the account you are logged into elsewhere is untouched. The directories
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

## Quota

Only claude publishes a usage endpoint. Readings are cached for **five
minutes** and refreshed on the first request past that age; `list -r` and
`?refresh=1` force one. Both listings say when each account's limits were
last read, and distinguish *not read yet* from *this provider has no limits
to read*:

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

A spent unscoped window shows as `limited`; a dead credential as `re-auth
needed`. Model-scoped windows (a separate weekly Fable budget, say) are shown
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

rota refreshes a token within five minutes of expiry, and only while running
a command. A refresh is never retried: if the reply was lost, the retry would
reuse a rotated token and kill the lineage. Treat `re-auth needed` as normal
rather than as a fault. The refusal that ended the lineage is kept: `rota
list` shows it in parentheses after `re-auth needed` — `re-auth needed
(invalid_grant: refresh token reused)` — and JSON carries it as `deadReason`.

### A token that outlives the window

Eight hours is fine for a command and wrong for everything else. rota hands
Claude Code the access token in `CLAUDE_CODE_OAUTH_TOKEN`, and a process that
is already running keeps the value it started with: Claude Code refuses by
design to adopt another after a 401 on a token it was given that way. So a
window left open overnight, a daemon, or one long session ends at `Please run
/login · 401` however diligently rota refreshes its own copy. Nothing rota
can do to the store reaches a process that already has the old one.

The answer is a second credential for the same account, good for a year:

```sh
rota login --long             # prints a login id and a URL
                              # approve in the browser AS THAT ACCOUNT
rota login <login-id> <code>  # finish it
```

```
long-lived token stored for #12 claude/manager.ican@gmail.com, good until 2027-09-21; every launch uses it from now on.
```

Every launch then uses it — `rota run`, a handover, a server request, a
session — and nothing else does. Usage, rotation and identity stay with the
ordinary login, which is the one that can read them: by the provider's own
design a long-lived token is inference-only, so it can drive Claude Code and
cannot read a profile or a usage endpoint. It is not a replacement for
`rota login claude`; it is a second key to the same door.

Because it is a credential of its own, a launch that has one does not refresh
first — a provider that refuses a spent refresh token no longer stops a run
that never needed it. That also softens the dead-account rule in one narrow
place: an account whose ordinary login has died still runs **when you name
it** — `rota run 12`, a handover, an HTTP request with an account id — and
rota says once, on stderr or in the server log, that usage is unknown, why
the login died, and when the long token runs out. The rotation still skips
dead accounts: naming one is a decision, being handed one is not.

Which account the token belongs to is never guessed. The exchange says who
approved, and that identity must match an account rota already holds;
approving as anybody else is refused and nothing is stored, as is a reply
that names no account at all. So log the account in normally first.

`rota list` says nothing about the token for eleven months, then one line
while it is within thirty days of expiry, and one more if it lapses — after
which every launch is quietly back on the eight-hour token. `--json` and
`GET /v1/accounts` carry `long_until` (RFC 3339); the token itself is never
printed, logged or sent anywhere. `rota set <id> --long forget` throws it
away, leaving the ordinary login alone.

One risk, plainly: a year-long credential sitting in `~/.rota/accounts.json`
is worth far more to a thief than an eight-hour one. If that file ever
leaves your machine, revoke the token from the provider's account settings —
and treat the file the way you treat a private key.

## Storage

By default `~/.rota/accounts.json`, mode 0600, written atomically, with an
exclusive lock held for the duration of each command so two rota processes
cannot overwrite each other's rotated tokens. `ROTA_HOME` moves the
directory. It holds live refresh tokens — treat it like a private key.

On Windows rota builds, runs and locks: store and session state take real
exclusive locks through kernel32's LockFileEx, with the same one-writer
promise flock gives unix. The vendor CLI runs as a child rather than
replacing the process, which is why nothing there needs to survive an exec.
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

Two things still touch the world, unavoidably: the network, and — for codex
and kimi, whose CLIs read credentials only from a file — a credential staged
into the account's own directory. `Stage` is the only core verb that writes
— and `StagePlan` is Stage without the disk: the command plus the credential
files as values, for an application that stores files its own way, with
`AdoptFrom` reading them back through any fs.FS. Providers that pass their
credential in the environment stage nothing either way.

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
| `store/` | The optional account store: `Backend`, `FileBackend`, locking, logins on disk, `Maintain`. Outside lib on purpose — an SDK does not need storage |
| `lib/oauth.go`, `lib/httpx.go` | OAuth verdicts, HTTP helpers, PKCE |
| `rotation/` | The queue: order, threshold, and which account a bare run takes. Outside lib on purpose — which account to spend is an application's policy |
| `wire/` | `Upload`, `End`, the account view, and the request vocabulary described for forms. Outside lib on purpose — a library has no opinion about JSON or labels |
| `message/` | Reading a finished answer: blocks, the normalized event vocabulary, `ask`. Outside lib on purpose — the SDK has no business knowing what markdown is |
| `api/server.go`, `api/run.go` | Routing, the token, the rate limit, requests and streaming |
| `api/runs.go`, `api/ws.go` | The runs that stay open, and the WebSocket that carries one both ways |
| `api/playground.html` | The page served at `/playground` |
| `api/config.go` | `server.toml`: the schema, its defaults, what it validates, and `--print-config` |
| `internal/toml/` | The part of TOML that file uses, and a refusal by name for the rest |
| `docs/server.toml` | The whole schema at its defaults, kept honest by a test |
| `cmd/rota/main.go` | The command |
