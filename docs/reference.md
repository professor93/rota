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
rota run 2 -m sonnet -e low "hi"   # every everyday run flag has a short: -m -e -s -c -r -t -S
rota run                      # open the rotation's account in its own CLI
rota run 2                    # open account 2's CLI, as it comes
rota set 2 --order 1          # put account 2 first in the queue (0 = out of it)
rota set 2 --threshold 80     # move on to the next account at 80% usage
rota set 2                    # what account 2 is set to
rota set 2 --cwd ~/src/api --config ~/.rota/api-memory
rota login 6                  # sign in an account whose CLI keeps its own credentials
rota remove 2 5               # forget accounts and their staged credentials
rota serve 8787 --token=T     # serve the HTTP API and its playground
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
```

The token is mandatory and is checked in constant time. Ten bad tokens from
one address within an hour block that address for an hour.

A running server keeps itself current: every two minutes it rotates any
access token close to expiring and re-reads the usage whose cached value has
aged past five minutes. A request should never be the thing that discovers
its credential expired, and the rotation decides from stored numbers — a
stale one sends work to an account that is already spent. Nothing there is
fatal: a provider that cannot be reached leaves its account exactly as it
was, and the next sweep tries again. `--refresh-every 0` turns it off, and
the command line still refreshes what it is about to use.

| Method | Path | |
|---|---|---|
| `GET` | `/` | unauthenticated and never rate-limited: what this is, its version, and where the page is. The only liveness answer — what a watchdog reads |
| `GET` | `/playground` | the playground, a single self-contained page |
| `GET` | `/v1/schema` | every provider, its models, efforts, defaults and fields |
| `GET` | `/v1/accounts` | accounts in rotation order, with usage, status, order, threshold and when limits were read (`?refresh=1`); `default` names the one a bare run would use |
| `GET` | `/v1/accounts/{id}/schema` | the models *that* account may actually use |
| `POST` | `/v1/run` | run a prompt on whichever account the rotation picks |
| `POST` | `/v1/accounts/{id}/run` | run a prompt on that account |
| `PATCH` | `/v1/accounts/{id}` | `{"order":1,"threshold":80,"cwd":"/srv/api","config_dir":"/srv/homes/api"}` — its place in the rotation, when to move on, and where it belongs |
| `DELETE` | `/v1/accounts/{id}` | forget it, and delete its staged credentials |
| `POST` | `/v1/login` | `{"provider":"claude"}` → `{id, url, kind}` |
| `POST` | `/v1/login/{id}` | `{"code":"..."}` → the account, or `{"status":"pending"}` |

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
| `text` | the agent said something, with `blocks` — see below |
| `thinking` | it thought something |
| `tool` / `tool_result` | it used a tool, and what came back |
| `blocked` | a tool it wanted was refused, with `tool` and `reason` |
| `usage` | a limit or token reading went by |
| `done` / `error` | how the run ended, with the exit status |
| `other` | something rota recognises but has nothing general to say about |

Every event carries a `seq`, so a gap is visible. Nothing is dropped: an
event type rota has never seen still arrives, as `other`. Set
`include_events` to get the provider's own event alongside rota's, in `raw`.

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
{"type":"done","exit_code":0,"is_error":false,"account":1,"session_id":"91ebe527…","duration_ms":3436}
```

rota speaks first, before the CLI has done anything, so a reader knows which
account is paying and which model and effort were resolved. Every event after
that carries its place in the stream, which is a reader's only way to notice a
gap. The last one says how it ended, whether that was an answer or a failure.

Without `--stream`, `--json` is still one indented document, as it always was.

### Reading the answer

An answer is markdown, usually with code in the middle of it. Both JSON
surfaces — the HTTP reply and `rota run --json` — carry the original text
and rota's reading of it, never one instead of the other:

- **`blocks`** splits it into prose and fenced code, each with its language,
  so a client showing the two differently does not need a markdown parser.
- **`ask`** is there when the run ended by asking something: the question,
  and the options when they were written as a list — with `multiple` when
  that list was a task list.

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
  #3 you@example.com  codex   01a048be  ~/src/api
  shared              claude  497f1383  ~/src/api
  ~/.claude holds 2648 conversations across 146 projects, shared by every
  account with no --config of its own.
```

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
a private home of their own. Claude Code is only pointed at one when the
account names a `--config`, so by default every claude account reads the same
`~/.claude` — those conversations are marked `shared` and counted once, because
they belong to no single account and listing them per account would say the
same work happened twice.

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
Claude Code reads whatever `~/.claude` the person running rota has, which is
right until an account is meant for one project.

The two must not be the same directory: the config directory is where a
credential file is written, and a working directory is a repository someone
will commit. Both must be absolute — a relative path means a different place
depending on where the process was started. A server given `--root` still
wins; an account cannot be pointed somewhere the server was told to stay
out of.

### The playground

`GET /playground` serves a page with no token of its own: it asks for one,
proves it against `/v1/accounts` — the first thing the page needs anyway, so
a right token costs no extra round trip — and keeps it only in that browser.

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

The server assumes its caller holds the token but is **not** trusted with the
machine. That assumption is what the following exist for.

`--root` (repeatable) confines every path a request names — the working
directory, uploads, extra directories, plugin directories, images, a debug
log, a settings or MCP file. The list is exhaustive by test, because a field
left out is one a caller can point at the token store: an agent that reads a
file and describes it is an exfiltration primitive. It confines what is
actually a path, too: grok writes its debug log where it is told, while Claude
Code's `--debug` takes a category filter and reaches no file at all. Without `--root` a caller
may name any directory, and the server says so at startup.

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
home outright, and a file naming a different ChatGPT account is never
adopted. Account ids are never reused, so a home cannot be inherited.

## Quota

Only claude publishes a usage endpoint. Readings are cached for **five
minutes** and refreshed on the first request past that age; `list -r` and
`?refresh=1` force one. Both listings say when each account's limits were
last read, and distinguish *not read yet* from *this provider has no limits
to read*:

```
ORDER  ID  PROVIDER  ACCOUNT          USAGE                            UNTIL  CHECKED  STATUS
1      1   claude    you@example.com  5h 5% (1h 11m)  7d 90% (11h 51m)  100%   2m ago   ok
2      3   codex     you@example.com  -                                 100%   n/a      ok
```

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
rather than as a fault.

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
| `api/playground.html` | The page served at `/playground` |
| `cmd/rota/main.go` | The command |
