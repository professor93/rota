# rota as a small ecosystem: TUI, daemon, SSH, events

Status: design, awaiting approval. Date: 2026-11-01. Base: main at 140f2fb (v1.3.0).

## Why

rota already runs many accounts of many agent CLIs. Using it still means
remembering commands, keeping a terminal open per chat, and noticing limits by
looking. The owner wants one small, beautiful place to manage accounts and to
reach any running chat from any device, the way Claude Code's Remote Control
lets a phone continue a local session, but on their own server, without
claude.ai in the middle, and across accounts.

Success looks like this: `rota` is the only thing they open on a laptop;
`ssh` or a browser from anywhere shows the same chats, each one separate; a
limit or a finished task reaches them on Telegram; and a project that imports
`rota/lib` cannot tell any of this happened.

## Goals

What the owner asked for, in their words where it matters:

1. A TUI with every rota feature: switch, reorder, max usage percent, run
   interactive, single request, "all of it". Convenient and beautiful, built
   on best practices.
2. All of it outside the SDK. Projects using rota as an SDK must see no effect.
   The SSH dependency is not in the SDK.
3. A background service: rota runs as a daemon, reachable over HTTP/web and
   over an SSH server that the daemon itself is.
4. From web or SSH, view, write to and manage a chat. Each session works
   separately and mixes with others as little as possible, in the sense of
   Remote Control: named sessions in a list, open one, leave without stopping
   it, several devices in sync.
5. On a daemon, a session runs as if emulated: a real interactive CLI on an
   emulated terminal, not a headless conversion of it.
6. Events can be listened to and caught, for example notifications by webhook
   or a Telegram bot.
7. A small ecosystem, not a big one, and excellent at account switching and
   multi-account use.

## Non-goals

- Anything in `lib`, or any new requirement in the root `go.mod`.
- A hand-rolled SSH or crypto implementation.
- A structured chat view with its own protocol. Web and SSH show the terminal.
- rota's own LLM gateway, MCP plugin, self-update, Web Push, marketplace,
  spend dashboards, or a global account switch that rewrites `~/.claude`.
- Windows support for held sessions and SSH. Linux comes first, then macOS, and
  Windows refuses them by name. The TUI's account features may work there, but
  nothing promises it.

## Anti-goals: failure even if it works

- A consumer's build, `go.mod`, or `go.sum` changes.
- Restarting or upgrading the daemon kills running chats.
- One session's output, input, crash or slow client affects another.
- A keystroke typed in one session lands in another.
- Attaching from a phone resizes or garbles the desk terminal.
- A token appears in argv, a log, an event, or a TUI screen.
- A daemon or web/SSH port is open because the user ran something innocent.
- One 30,000-line TUI file (clauth's `app.rs`).

## Constraints from the codebase

- Module `github.com/professor93/rota`, Go 1.27, no requirements, no go.sum.
  `lib` reads no environment; `wire.Version` and `rota.Version` move together.
- Module versions are permanent; a nested `replace` breaks `go install @version`.
- `api` already has terminals on a pty: `termSession` (ring with absolute
  offsets, fan-out, slow reader closed with 1013, keyboard claim, audit,
  recording), `termBackend` interface with `localTerm` and a shared backend,
  `internal/share` framing, `internal/pty`, roles, `[[users]]`, invites,
  TOML `server.toml` that rota never writes.
- Commands stay unfragmented: new behaviour joins an existing verb.
- Natural by default: rota changes nothing a person did not ask for.
- A claude account runs in its mirror home with its own daemon; tokens and
  daemon are per account, MCP, plugins and skills stay common. Do not make
  more of it private without asking.
- Tests never drive a real interactive claude; throwaway `ROTA_HOME`, fake
  CLI through `internal/fakecli`, loopback ports.

## The shape

```
                     root module (no dependencies)
   lib  store  rotation  wire  message  sessions  api  internal/{pty,share,...}
                                 ^
                                 | requires, exact version
                     module .../cmd/rota (dependencies allowed)
   main  tui  sshd  hold  notify  client
```

```
   browser ──https/ws──┐
   ssh client ──ssh──> sshd ─┐      rota daemon: api.Server + sshd + events
   rota ──────────────────── ctl.sock ──> termSession per session
                                               │ unix socket
                     ┌─────────────────────────┼───────────────────┐
                 holder 1                  holder 2            holder 3
                 pty + emulator            pty + emulator      pty + emulator
                 claude (account 12)       codex (account 3)   claude (account 12)
```

### 1. The SDK boundary (measured)

Adding `require golang.org/x/crypto` to the shared `go.mod` raised a test
consumer's `x/crypto` from v0.31.0 to v0.58.0 although it imported only `lib`:
minimum version selection reaches through a package boundary. So the boundary
must be a module boundary.

`cmd/rota` becomes the nested module `github.com/professor93/rota/cmd/rota`,
with its own `go.mod` and `go.sum`, no `replace`. The root keeps every other
package and stays requirement-free. A nested module can import the root's
`internal/` by import path (verified with `go build`).

- `go install github.com/professor93/rota/cmd/rota@latest` keeps working: Go
  picks the longest matching module, and tags `v1.3.0` and older still resolve
  to the root.
- A committed `go.work` (`use . ./cmd/rota`) keeps `go build ./cmd/rota`,
  `scripts/release.sh` and day-to-day work unchanged. `go install @version`
  ignores it.
- Release becomes two tags in order: `vX.Y.Z` on the root; then a commit that
  sets `cmd/rota/go.mod` to `vX.Y.Z`; then `cmd/rota/vX.Y.Z`. `scripts/` gets
  a helper. `wire.Version` stays the app's displayed version. The tag comes
  before the fast-forward to main, so `@latest` never resolves to a pseudo-version.
- `go list ./...` at the root leaves `cmd/rota` out even under `go.work` (tested).
  So `ci.yml` and the `release.yml` gate add `go -C cmd/rota vet ./...` and
  `go -C cmd/rota test ./...`, or docs_test and parity stop running silently.
- `release.yml` changes: `--prerelease` when the tag contains `-`, wider tag
  patterns in smoke and module (lines 94, 146), and the `module` job triggered on
  `cmd/rota/v*`, because it cannot pass on the root tag alone.
- A new CI check fails if the root `go.mod` gains a `require`, one fails if
  `cmd/rota/go.mod` holds a `replace`, and one builds a throwaway consumer of
  `lib` and asserts its `go.sum` stays empty.
- `go mod tidy` in `cmd/rota` ignores `go.work` (tested), so a helper tidies it
  through a temporary `-modfile`.
- rota-sdk-test builds the binary by import path (`serve/helpers_test.go:39`)
  and skips when that build fails, so a broken split would pass with `serve`
  untested. The gate builds the binary itself, sets `ROTA_BIN`, and a failed
  build fails the gate. The suite adds no `require` on the app module, which
  would pull TUI and SSH dependencies into a consumer's `go.mod`.
- README and `docs/reference.md` change "the whole project has no third-party
  dependencies" to "the SDK and the server packages have none; the `rota`
  program uses Bubble Tea, Lip Gloss and x/crypto/ssh".

### 2. One process per session: the holder

Every session the daemon runs lives in its own detached process, `rota serve
hold`, started by the daemon and documented as internal, because docs_test
forces every command into the manual. This is the single decision that gives the
isolation and survival the owner asked for, and it extends what `--share`
already does: a rota beside a pty, linked to a server over a unix socket.

A holder owns exactly one thing: a pty, the child CLI, an emulator of its
screen, a scrollback ring and a socket. It:

- calls `setsid`, so stopping the daemon, or systemd restarting it, reaches no
  holder. The systemd unit sets `KillMode=process`; the launchd plist sets
  `AbandonProcessGroup=true`. Without these, a restart kills every chat.
- receives its request over a stdin pipe as one JSON document: account id,
  CLI arguments, folder, name, window size. Never a token, and nothing in argv,
  since arguments can hold a prompt.
- calls `store.Prepare` itself, so no credential crosses between processes and
  the account's run claim lives with it. Like `terminals.go:225-238`, it calls
  `Launchable` first and closes the store after. rota cannot rewrite a login
  under a running session.
- calls `store.KeepClaimsAcrossExec()` before starting the child, because the
  claim is close-on-exec: without this a killed holder would free the claim while
  the CLI lives, and a codex token could burn. The claim ends when the CLI does.
- listens on `<store>/terminals/<id>/sock`, directory 0700, socket 0600.
  It accepts one daemon link, and a new link replaces the old only when it
  comes from the process that holds the daemon lock; anything else is refused.
- holds a flock on `<store>/terminals/<id>/lock` for its whole life. Locked
  means alive: nothing deletes a locked session's socket, re-adoption and Resume
  decide by the lock, never by a failed connect.
- exits when the child exits, after recording the exit code.

The session record `<store>/terminals/<id>/meta.json` (0600, never secrets)
holds id, name, account, provider, holder pid, holder protocol, cwd, the CLI
arguments the person gave, conversation id when known, start and end time, exit
code. `--resume`-like and session-id flags are dropped from the stored
arguments, so Resume and Move carry model, permission mode and Remote Control
over without carrying the old conversation.

**Which conversation is this?** Claude Code keeps `sessions/<pid>.json` in the
account's home with `sessionId`, `cwd`, `name`, `status`, `waitingFor` and
`updatedAt` (keys read from version 2.1.316). The holder knows the child's pid,
so it reads that file and stays current through `/clear` and `/resume`, and it
learns the worktree's real folder. The file is undocumented, so it is read
defensively: a missing file or key leaves those fields unknown. rota never
guesses by matching transcripts. Two sessions in one folder are normal, and a
guess would resume the wrong chat. With no known conversation, Resume and Move
are disabled with the reason. Other providers have no such file in this version.

The daemon keeps everything multi-client: `termSession` stays the hub, gaining
a `heldTerm` backend beside `localTerm`. On start the daemon scans
`terminals/*/` and re-adopts every live holder, asking for bytes since the
offset it knows; a holder that is gone becomes a stopped session. A daemon
restart therefore looks to a viewer like a brief reconnect. For that to be true
`Server.Stop` must let go of held sessions: today `Stop` calls `endTerminals`,
then `kill("shutdown")` (`server.go:434`, `terminal.go:1236`), so `termBackend`
gains a detach that closes the link and leaves the holder running.

`api.Options.HoldCommand` selects this. Nil keeps `localTerm`, so a program
embedding `api.Server` behaves as it does today.

Holders outlive the daemon, so they keep running the old binary through an
upgrade. The holder protocol therefore has its own version, separate from
`share.Version`, which stays as it is for `--share`. The framing is reused; the
version is a major number plus a list of features said in the first frame.
Frames are additive, an unknown frame is ignored, and the core set (output,
input, resize, exit, since, screen) does not change meaning within a major. A
daemon keeps speaking every major it has shipped. A holder it still cannot read
is listed as running, made by that rota version, not attachable, with a Stop
action that signals its pid: HUP, then KILL, only after checking that the pid's
process start time and the boot id still match what `meta.json` recorded. It is
never killed automatically.

After a reboot, holders are gone. Their sessions stay in the list as stopped,
with one action, Resume, which starts a new holder on the same conversation.
Nothing restarts by itself. Stopped sessions stay listed for `[terminal]
keep_stopped`, default `168h`, because the config parser takes Go durations,
and `x` forgets one sooner.

### 3. Emulated: what a viewer sees

The holder feeds output through a VT emulator (`charmbracelet/x/vt`), so it
always knows the current screen. Attaching sends a `Screen` frame, a repaint of
the current screen, then live bytes. That removes the garbled replay that raw
ring bytes give a full-screen app, and it is why the holder lives in the
`cmd/rota` module: the emulator is a dependency.

The same screen gives the TUI a live preview of any session, and the title and
bell the CLI sends give it names and attention signals.

`x/vt` is at v0.1.0, so phase 1 starts with a spike. The input is bytes captured
from `vim`, `less` and the fake CLI, plus a recording of a real session if the
owner supplies one. Tests never drive a real interactive claude, so nothing
here starts one without the owner's explicit yes. The bar: for each capture, the
emulator's final screen matches `@xterm/headless` fed the same bytes, cell text
and colours, and a repaint from `Render` reproduces that screen in xterm.

If it fails, the fallback costs features, so it is named here. Attach replays
the ring, as the web page does today, and only a connection holding the keyboard
may trigger a repaint by resize. A small OSC and BEL scanner replaces the
emulator for titles and attention, and the preview becomes the last lines of the
ring with escapes stripped.

### 4. Sessions, in the sense of Remote Control

- **Named, listed, separate.** Name order: explicit, the title the CLI set,
  the conversation name from `sessions`, the folder. Every session has an id,
  a stable URL `/terminal#<id>`, a status (running, needs you, quiet, stopped)
  and viewers.
- **Open and leave.** Closing a viewer never stops a session. Stopping one is
  an explicit action with a confirm. Held sessions are exempt from
  `idle_timeout`: a long unattended task must survive having no viewers. The
  cost is that a codex, grok or kimi session keeps its account's exclusive claim
  for as long as it lives, so the refusal on such an account names the session
  that holds it, and `session.quiet` flags one forgotten. `idle_timeout` still
  governs non-held terminals.
- **In sync, from any device.** All viewers see the same screen. Typing:
  the same person on several devices need no ceremony, so the keyboard moves
  to whichever of their connections types. Between different people the
  existing claim, grant, deny and force flow stays. A watch role never types.
- **Who is the same person.** The `local` principal, used by the TUI on this
  machine, counts as a person from `[[users]]`: the only control user if there
  is exactly one, otherwise the one named by `[auth] local_user`. With neither
  it stays its own person and the claim flow applies. A bearer token is never a
  person for this rule.
- **Size is explicit.** Attaching never resizes, and neither does typing,
  so a stray tap on a phone cannot reflow the desk. Size changes only when a
  connection asks: `Ctrl-\ z`, or Fit on the web page. A smaller viewer sees a
  note, and the web page pans and scales instead.
- **No takeover loops.** A connection that lost the keyboard does not reclaim
  it by reconnecting (learned from clauth).
- **Files.** New sessions share the folder by default. A claude session can
  start isolated with Claude Code's own `--worktree`; rota adds nothing of its
  own, and other providers do not offer it.
- **Real Remote Control is orthogonal.** A new session may also turn on
  `--remote-control` where the account allows it; nothing else depends on it.

### 5. Independent work side by side

The owner wants, at one time, account A running a task in one session and
account B another, possibly on different providers, with neither touching the
other. Each holder is that independent worker. Account A's claude and account
B's codex share no process, pty, home, environment or socket. The rota daemon
only routes viewers to them, and a task keeps running whether or not it is up.

```
rota run 12 --detach --label refactor -- "move the auth code"    # claude, account 12
rota run 3  --detach --label audit    -- "review the migrations" # codex, account 3
rota run --attach refactor                                       # look, type, Ctrl-\ d
rota send audit "also check the indexes"
rota list --sessions
```

`--detach` and `--attach` join `run`, so there is no new verb. `--detach` is a
handover: its arguments go to the CLI as they would in an interactive `rota run`,
and `-p` is refused, because a headless run has no session to detach. The name
is the existing `--label`, as `--name` is Claude Code's own flag. `send` learns
held sessions: it types the text and Enter into that session, under the same
keyboard rules. Without a daemon, `--detach` starts one that serves `ctl.sock`
alone.

What may run in parallel is the rule rota already has, unchanged:

- A claude account takes many sessions at once. Its home is shared and Claude
  Code owns the login.
- A codex, grok or kimi account takes one at a time, because those CLIs rewrite
  a refresh token in their home and a second run would burn it for good. The
  TUI greys such an account out with that reason, and automatic choice steps to
  the next free account of the provider.
- `[terminal] max_sessions` caps the machine as a whole. Its default of 8 stays.

A person who wants worlds separated further runs a second daemon on another
store: `ROTA_HOME=~/.rota-b rota serve start`. The pid file and lock are per
store, so this needs no feature, and it has its own accounts, ports and users.

One daemon per account was rejected: it would add a port and a service per
account to remember, and give no isolation the holders do not already give.

### 6. The daemon

No new verb: `serve` grows. `rota serve` keeps running in the foreground.

```
rota serve start | stop | status
rota serve install | uninstall      # systemd --user unit, or launchd agent
```

There are two ways to run, and the existing one does not change:

| | `rota serve` (foreground) | daemon mode |
|---|---|---|
| Started by | a person, as today | `serve start`, the unit, the TUI |
| Runs | as today, flags and defaults unchanged | `rota serve start --foreground`, internal, under its supervisor |
| Sessions | `localTerm`, Ctrl-C ends them | holders, which outlive it |
| TCP | default `127.0.0.1:8787` with a token | only if `server.toml` sets `[server] listen` |
| `ctl.sock` | no | always |
| SSH | if `[ssh] enabled` | if `[ssh] enabled` |

- `start` double-forks into that mode, with a pid file and lock per store. `stop`
  ends the daemon only, never sessions. `rota serve stop --all` ends those too,
  after a confirm. Logs are files in the store, rotated at 20 MiB with four
  archives, and read with ordinary tools.
- `install` writes a unit whose `ExecStart` is `rota serve start --foreground`,
  enables it, and prints the `loginctl enable-linger` hint on Linux. Supervisors
  start with a bare environment, so the unit also writes the installer's `PATH`,
  `ROTA_HOME` and `CLAUDE_CONFIG_DIR`, or `claude` is not found and the mirror's
  source silently changes (`run.go:38`, `env.go:47`, `dirs.go:105`). Windows
  refuses by name.
- `api` refuses to start without a token (`server.go:235`, `main.go:2100`), so
  daemon mode generates a random token held in memory. `api` does not change for
  this.
- A new unix socket `<store>/terminals/ctl.sock` serves the same HTTP and
  WebSocket API with the fixed principal `local`, role control. Filesystem
  permissions are its authorization, as with `share.sock`. It never accepts an
  asserted person. `local` joins `token` in one shared helper exempt from Origin
  checks (`principal.go:260`, `ws.go:107`).
- `[routes]` governs TCP only. `ctl.sock` always has every group, terminals
  included, because only the owner can reach it. So the server builds two muxes
  from one route table. SSH needs `[ssh] enabled` alone. A file with only `[ssh]`
  opens no HTTP port. Existing rules stay for TCP: terminal routes off by default,
  TLS for non-loopback, auth mandatory.
- The TUI and `--detach` start the daemon by themselves only when a session is
  first needed, and say so. An auto-started daemon serves `ctl.sock` alone,
  whatever `server.toml` says: ports open only from `serve start` or the unit.
- The daemon also keeps usage fresh for everyone: one coalesced background
  refresh, cached in the store, so TUI, web and events share one reading.

### 7. SSH

`golang.org/x/crypto/ssh` directly, no framework: the surface is small, it is
the maintained library, and there is no fork in between. Sits in
`cmd/rota/sshd`, inside the daemon process. It reaches the API through one
in-process `Server.LocalHandler(*Principal)` over an in-memory pipe, with the
person its key authenticated, so `roleFor` stays the only table. A socket that
accepts an asserted person would let any process of the same user forge names in
the audit log and skip the keyboard claim, so none exists.

```toml
[ssh]
enabled = false
listen = "127.0.0.1:2222"     # non-loopback allowed: SSH encrypts
host_key = ""                 # default <store>/ssh_host_ed25519_key, made 0600

[[users]]
name = "me"
role = "control"
ssh_keys = ["ssh-ed25519 AAAA..."]   # password becomes optional when these exist
```

- Public keys only. No passwords, no keyboard-interactive, three tries.
- Only `session` channels. Forwarding, agent, subsystems and `env` other than
  TERM are refused. There is no shell unless `[terminal] shell = true`, the
  same opt-in as the web.
- `ssh -p 2222 host` opens the TUI. Non-interactive commands are a fixed list:
  `list`, `attach <id>`, `watch <id>`, `new [flags]`, nothing else is parsed.
- A control role here can run commands as the daemon's user, as on the web.
  The configuration comment says this plainly.

### 8. Attaching in a terminal

TUI and SSH attach the same way: the screen is handed to the session, bytes
pass straight through, so colours, mouse and keyboard behave natively and no
chrome competes with Claude Code.

Detach is a chord, `Ctrl-\` then a key. `Ctrl-]` was rejected because Claude
Code itself binds it.

```
Ctrl-\ d   back to the list        Ctrl-\ n / p   next / previous session
Ctrl-\ k   take the keyboard       Ctrl-\ z       fit to this window
Ctrl-\ ?   show these              Ctrl-\ Ctrl-\  send it through
```

On detach, every mode the CLI turned on is reset: alternate screen, mouse,
bracketed paste, keyboard protocol, cursor. The detach key is configurable.

### 9. The TUI

Bubble Tea v2 (`charm.land/bubbletea/v2`, v2.1.1), Lip Gloss v2, Bubbles v2.

Bare `rota` opens the TUI when stdin and stdout are both a terminal, and there
is no `tui` verb. Anywhere else, a script, a pipe, CI, it prints the short usage
and exits 0 exactly as it does today, so nothing that calls rota changes.
`rota --json` with no command keeps printing usage too.

Four tabs. Fewer than clauth's eight on purpose.

**Accounts.** One table: id, name, provider, order, 5h bar, 7d bar, reset, state.
Below it the selected account's every usage window, threshold, folder, config,
sessions mode, token lifetime.

```
 rota   Accounts  Sessions  Ask  Services              ● daemon  2 live
 #  account            5h                      7d                reset
 1  work@…   claude  ▕█████████░░░│░░░▏ 62%  ▕███░░░░░░░░░░░░▏ 21%  1h 3m · 21:20
 2  home@…   claude  ▕██░░░░░░░░░░│░░░▏ 12%  ▕█░░░░░░░░░░░░░░▏  7%  4h 10m · 00:27
 3  cdx@…    codex   ▕███████████████▏ 95%                     dead: re-login
 ⏎ run  ⇧↑↓ reorder  +/- max %  a ask  n login  e edit  d remove  ? help
```

The tick on a bar is that account's own threshold, and colour is relative to it,
not to a fixed 60/80. That is rota's idea of a limit, so the screen tells it.

- Run interactive: `⏎`, into a held session or, without one, directly.
- Switch: rota has a rotation, not a global account. `u` puts an account first.
  On a live chat, Sessions `m` moves it: see below.
- Reorder `⇧↑↓`, max usage `+/-` by 5 or typed, exclude from rotation `x`.
- Add an account from inside the TUI, see below. Re-login a dead account the
  same way, `l`.
- Edit covers every `rota set` flag. Remove asks, and names live sessions.

Adding an account is a short guided flow on `n`, and it needs no other terminal.
Every step works over SSH too:

1. Pick the provider. Hidden providers stay hidden.
2. Where the provider offers both, pick how long the token lasts:
   **short**, the refreshable login, which is what Remote Control and usage
   readings need; or **long**, about a year, inference only (`rota login
   --long`). Short is preselected and each option says what it gives up.
   Providers with one kind skip this step.
3. The login's own kind decides the rest:
   - `code`: show the URL, `o` opens it when a browser is on this machine,
     `c` copies it by OSC 52, which works over SSH, then a field waits for the
     pasted code as long as the login is valid and shows a countdown.
   - `device`: show the code and URL, poll by itself, and finish unprompted.
   - `apikey`: a masked field. The key never appears on screen or in a log.
   - `delegated`: hand the terminal to the provider's CLI, then come back.
4. The new account appears selected, with its usage being read.

A wrong code stays in the field with the error beside it, to fix and retry.
Leaving the flow collapses it to a footer indicator and keeps waiting; `n`
returns to it. The pending login is the same one `rota login <login-id>` can
finish, so nothing is lost if the TUI closes. Watch role sees no add or
re-login action.

A one-time login page serves a machine with no browser and an awkward paste.
`rota login --web [addr]`, or the same choice in the flow above, starts a small
server for this one login. It shows the login URL and takes the code, an API
key or a setup token. It is a flag on `login`, not a new verb, and it exists
only while it runs.

- With no address, rota lists this machine's interfaces and asks which one to
  listen on, with `127.0.0.1` preselected, reached through `ssh -L`. Each entry
  says who can reach it. `0.0.0.0` is offered last, with that warning.
- A non-loopback address gets HTTPS with a certificate made for this run, and
  rota prints its fingerprint to compare. Plain HTTP on a non-loopback address
  is refused, with no flag to allow it: the owner chose HTTPS only on
  2026-11-04, matching rota's rule for the server.
- The link carries a random secret of 256 bits, in the path, and any other path
  answers 404. It ends after one accepted login, after 10 minutes, or on Ctrl-C,
  whichever comes first, and the listener closes with it.
- The secret goes in a POST body and never in a URL. Five wrong tries end the
  page. Nothing submitted is logged or echoed, and the response sets no cache.
- It accepts one login and does nothing else, so it adds no route to `rota serve`.

**Sessions.** Live and stopped sessions at left, a live emulator preview at
right. `⏎` attach, `w` watch, `n` new (account, folder, name, worktree,
Remote Control), `r` resume, `m` move to another account, `s` stop, `/` filter.
Resumable conversations from `sessions` appear below live ones.

**Ask.** A single request through `Store.Run`, as `rota run` does, because
`lib.Run` alone takes no claim and adopts nothing: prompt box, account or automatic,
readings toggles for `--with`, streamed answer, `⌃s` keeps it open as `--input`.

**Services.** Daemon status, start, stop, install; web and SSH addresses;
viewers; audit tail; invites; event sinks with a test button. Editing config
opens `$EDITOR`: rota still never writes `server.toml`.

Rules for building it:

- Elm architecture. No I/O in `Update`; everything is a `tea.Cmd`. One package,
  one file per tab, none past about 600 lines.
- A `Backend` interface between the views and the world. `roleFor` stays the only
  enforcement; `Backend` merely hides what a role cannot do. Local TUI and SSH TUI
  run the same model. Resume, move, audit tail and service state need routes, and
  `PATCH /v1/terminals/{id}` takes what it can before a new path is added.
- `key.Binding` is the only source of keys, so `?` cannot drift from reality.
- One theme module: adaptive light and dark, truecolor, 256 and ANSI tiers,
  honours `NO_COLOR`.
- Width ladders: columns shed by priority, never reflow when a session starts,
  a clear message below 60×14. Mouse: click and wheel, toggled with `M`,
  because capture blocks text selection.
- Taken from clauth: toasts, two-step quit, contextual footer, stale readings
  fade, level-crossing bell that re-arms on falling, reset as `1h 3m · 21:20`.
- Golden tests at 60×20, 80×24, 120×40 with a throwaway store.

### 10. Moving a chat between accounts

This is where rota should be better than anything else. `m` on a session, or
the event action below: stop the CLI, then resume the same conversation under
the chosen account in a new holder, from the folder the registry reported, so a
worktree session resumes where its conversation lives. The session keeps its
id, name, stored arguments and viewers. Mid-turn work is lost to that turn, so a
running session warns first.

Moving follows the rule `rota run --resume` already has: within one provider
flavour, and `sessions.CopyForResume` brings the transcript to the target when
its home does not hold it, whatever the `--sessions` mode. That needs a known
conversation id, so in this version only claude sessions can move. Across
providers it is refused by name, because no CLI can read another's conversation.

### 11. Events

One small bus in the daemon: typed events, a bounded ring, sinks. Off by default.

| Event | Means |
|---|---|
| `account.threshold` | crossed its max usage, up or back down |
| `account.reset` | a window reset |
| `account.dead` | login or token failed, with the reason |
| `account.rotated` | rotation moved to another account |
| `session.started`, `session.ended` | with exit code |
| `session.attention` | the session wants a person |
| `session.quiet` | output stopped after activity |
| `viewer.joined`, `viewer.left` | someone attached or left |
| `daemon.started`, `daemon.stopped` | |

An event is JSON: `id, type, time, account, session, name, text`. It carries no
token and no conversation content. Level-crossing latches stop repeats. Sinks
retry with backoff and drop rather than block; one bad sink affects nothing.

```toml
[[events.sink]]
type = "telegram"                # or "webhook", "command"
bot_token_file = "/home/me/.rota/telegram.token"   # full path, as `token_file`
chat_id = "123"
on = ["account.*", "session.attention", "session.ended"]

[[events.sink]]
type = "webhook"
url = "https://example.com/hook"
secret_file = "/home/me/.rota/hook.secret"   # HMAC-SHA256 in X-Rota-Signature
```

- `GET /v1/events` streams the same events by SSE for watch and control roles.
- `command` runs a program with the JSON on stdin, argv never built from
  event text.
- Sinks use only the standard library and live in `cmd/rota/notify`. They read
  `/v1/events` over `ctl.sock`, so no Go event type becomes public.
- A Telegram URL holds the bot token and net/http errors repeat the URL, so sink
  errors are stripped of it before anything is logged.
- Telegram is send-only in this version; a Telegram message opens the session's
  URL. Answering from Telegram is out of scope.

`session.attention` has two sources, and neither changes how the CLI runs. For
claude, `status` and `waitingFor` in the registry file from section 2, exact
while that file keeps its shape. For every provider, and for claude when the
file does not answer, BEL and OSC 9/777 from the emulator and quiet after
activity, which is approximate. rota passes no hooks and adds no flags, as
natural by default asks.

A chat moves only when the owner confirms it. `account.threshold` on a live
session offers `m` in the TUI and names the session on Telegram.

### 12. Web

`/terminal` stays the page. It gains: a session list with names and status like
Remote Control's, New with the same fields as the TUI, resume and move, and,
for phones, as the owner chose on 2026-11-04:

- a composer: a text box where a whole message is written with the phone's own
  keyboard, autocorrect and IME, and sent once as one bracketed paste followed by
  Enter, so two devices of one person never interleave keystrokes;
- quick buttons for what a CLI asks: 1, 2, 3, Esc, Enter, Tab, arrows, Ctrl-C.
  When the registry says the session waits for a permission or a question, the
  buttons come forward;
- the composer and buttons show on `(hover:none) and (pointer:coarse)`, with a
  toggle remembered per device, not on any touch screen.

The phone still shows the one terminal. No structured chat view and no second
protocol: input always goes to the pty. A read-only conversation view is not
part of 1.4.0. Theme tokens match the TUI.

### 13. Rules learned from herdr, zellij, claude-squad and happy

Studied on 2026-11-04 at the owner's request. Each rule below closes a failure
one of them shipped; where a rule and an earlier section differ, this one wins.

**The terminal.**
- The holder is the only thing that answers a CLI's terminal queries: device
  attributes, XTVERSION, DSR, XTGETTCAP, OSC 4/10/11 colours, with fixed answers
  matching a common modern terminal, whether or not anybody is attached. The
  daemon strips query replies coming back from xterm.js, the TUI and SSH, so a
  viewer's terminal never answers twice, and a reply never counts as typing or
  moves the keyboard. (herdr and zellij both had replies leak into panes.)
- The emulator runs behind `recover()`. A panic in it drops that session to ring
  replay and the OSC scanner of section 3; the pty, the CLI and the ring carry on.
  The loop copying pty output into the ring never waits on the emulator or on the
  daemon link.
- A `Screen` frame is taken when the parser is between sequences, states the
  ring offset it stands for, and restores the modes too: alternate screen,
  cursor-key mode, bracketed paste, mouse mode and encoding, focus reporting,
  kitty keyboard flags, cursor visibility. The spike's bar adds "modes match".
- Attaching clears the viewer's terminal first. Detaching waits briefly for
  output to drain, resets every mode above, and prints one line: why it ended
  and `rota run --attach <name>`.
- The `Ctrl-\` chord is read as the byte 0x1C, as kitty CSI-u and as
  modifyOtherKeys, and across split reads. The TUI attaches through
  `tea.Exec`, and drops stale terminal replies up to the answer of one DA1 query
  rather than by a timer. If no key follows `Ctrl-\` within about 400 ms, one
  hint row is drawn and then repainted from the emulator. The chord is shown
  once before a person's first attach.
- A viewer smaller than the pty gets a cropped render from the emulator, with a
  note, until it fits explicitly; raw bytes are never wrapped into it. The TUI
  preview renders the session at its own size and crops; it never resizes it.
- A viewer that falls behind gets a fresh `Screen` frame and continues; it is not
  closed with 1013, which on a phone becomes a reconnect loop.
- A held session never starts at 0×0: `--detach` from something that is not a
  terminal takes 120×40, and Resume starts at the requester's size.

**The process.**
- The holder applies an explicit scrub list to the child's environment, beyond
  secrets: `TMUX`, host terminal ids, and the marks a CLI puts on what it
  starts — `CLAUDECODE`, `CLAUDE_CODE_CHILD_SESSION`, `CLAUDE_CODE_SESSION_ID`,
  `CLAUDE_CODE_SESSION_ATTENDED`, `CLAUDE_CODE_ENTRYPOINT`, `CODEX_THREAD_ID`,
  `CODEX_SESSION_ID`, `CODEX_TOOL_CALL_ID`. An account's CLI also gets its
  provider's own drop list (lib's `claudeCompeting` and `codexCompeting`),
  which names exactly everything that would sign it in some other way, send it
  elsewhere, or say it is nested. `run --detach` and New from the
  TUI send the caller's environment, filtered like `termEnv`, in the stdin
  request, so `PATH`, keys and direnv match the shell; sessions started from the
  web or SSH get the daemon's, and the New form says so.
- `meta.json` records the holder's process start time and the boot id. A
  `sessions/<pid>.json` counts for a session only when its pid's start time
  matches the child's. When that file is missing, the last known conversation id
  is kept, never cleared.
- Keystrokes go through a separate pty writer; when its queue is full the sender
  is refused visibly, never dropped with a log line.
- Re-adoption handles each `terminals/<id>` on its own, so one broken record
  hides nothing. "Ended" (the child exited, or Stop) and "interrupted" (the
  holder vanished) are different states; only interrupted sessions lead with
  Resume.
- Only the store lock proves a daemon is alive. Holders never start one, and
  every link carries pings with a deadline.
- The ctl.sock path never enters a session's environment. A caller whose
  session id is a live holder's is watch-only on ctl.sock, so an agent inside a
  session cannot drive the others.
- On macOS, phase 1 tests that a held claude still reads its keychain item after
  the starting terminal closes and under `serve install`, since a detached or
  launchd process can leave the login session's bootstrap namespace.

**Sending and attention.**
- `rota send`, the composer and the TUI reply box wait until the session is
  ready (registry status, otherwise the first quiet after output, with a
  timeout), use bracketed paste when the child has it on, and send Enter
  separately. While the registry says the session waits for a permission or a
  question, `send` refuses unless forced, because Enter there may approve a tool.
- rota never matches a CLI's UI text and never answers for the person: no
  auto-yes.
- `session.attention` carries a reason: permission, question or done, from
  `waitingFor`, else bell, title or quiet. The window title is a second source:
  Claude Code's ✳ title means idle and a spinner means working, which also gives
  codex an attention signal. Spinners are stripped from names.
- `session.quiet` ignores output in the first seconds after start, after a fit,
  and echo within about 50 ms of a keystroke. No sink fires while the keyboard
  holder has typed recently.
- Telegram and webhooks carry the session's `--label` or folder, never a name
  derived from the conversation, unless the owner opts in: push text otherwise
  leaks chat content.

**Files and reach.**
- Stop and `x` never touch a worktree; Sessions shows a worktree's path and
  copies it. The New form says that a worktree lacks gitignored files.
- A phone reaches the daemon directly: through Tailscale or WireGuard, or with a
  real certificate, which the reference names as the ways in. The Services tab
  can show an invite as a QR code.
- Each session shows its account and credential kind: login, long token or API
  key, since a person must see what they are paying with.

Rejected, with the reason: one server owning every pty (herdr, zellij: one fault
ends all sessions); fd-passing upgrades; restarting the CLI to hand it to a phone
(happy: lost permission mode and Remote Control, wrong billing); a hosted relay;
injected defaults or hooks; the size of the last or smallest viewer; WASM plugins;
tmux as the backend; tools that own git branches.

## Delivery, in order

Each phase is a branch, reviewed and tested before the next; merge to main only
on the owner's word.

0. Module split, `go.work`, CI, release script, suite, docs. No behaviour change.
1. Holder, emulator spike, `heldTerm`, re-adoption, `ctl.sock`, `serve` verbs,
   `run --detach`, `send` into a held session.
2. `attach`, the chord, SSH server.
3. TUI shell, theme, Accounts, Ask.
4. Sessions, moving a chat, Services.
5. Events and sinks.
6. Web page additions.
7. Reference, README, consumer suite, 1.4.0.

Everything here is part of 1.4.0, as the owner decided on 2026-11-04: it ships
as `v1.4.0-beta.2`, `beta.3` and so on, each with the matching
`cmd/rota/v1.4.0-beta.N`, after the published `v1.4.0-beta.1` (logins and API
keys), and becomes a stable `v1.4.0` only when nothing is left. When a module's
only tag is a beta, `@latest` installs the beta (tested on a local proxy); how
the nested module meets that during the betas is an open question to the owner.
Tags are permanent, so a beta number is never reused. Betas are GitHub
pre-releases, so `install.sh` stays on stable. Suite gates use stable numbers
only, as `since()` reads a beta as at least `1.4.0`.

## Public surface

`lib`, `store` and `wire` gain nothing for this design. `api` grows by what cannot
be avoided: the config parser refuses unknown keys, so `[ssh]`, `[events]` and
`ssh_keys` are exported `api.Config` fields and `Print` is extended, plus
`Options.HoldCommand` and `Server.LocalHandler`. `termEnv` and `remoteControl` move
so the holder can share them.

Every phase runs what CI runs, `gofmt` first, vet on three targets, tests for
both modules, Linux in Docker for anything touching pty, sockets or processes.

## Risks

- `x/vt` is young; the fallback and what it costs are in section 3.
- Claude Code's `sessions/<pid>.json` is undocumented. If it changes, Resume and
  Move on live sessions turn off with a reason and attention gets approximate.
  Nothing breaks, and a test against a captured file shape catches it. When an
  interactive start lands in Claude Code's agent view, the child's pid may have
  no registry entry; those fields stay unknown.
- Binary size grows; phase 0 records it, and the owner should hear the number.
- Two tags per release add a step that can go wrong; the helper and the
  empty-go.sum check exist for that.
- Claude Code's flags and rendering change; hooks are opt-in and everything
  else reads a terminal, which is the most stable interface it has.
- An `--config` account on an env token can expire under a long-lived session.
  Known, shown as `token_until`, and now raised as `account.dead`.

## Left to the builder

Package internals, exact frame numbers, spacing and colours within the theme
rules, retry timings, the fixed SSH command output format, how `meta.json` is
written atomically, the size of the emulator's scrollback.

## The owner's answers, 2026-11-01

Direction approved, final approval after the chosen projects are studied.
Nested `cmd/rota` module, one holder per session, one daemon, `rota serve
start|stop|status|install`, chats move only when confirmed, Bubble Tea v2 with
Lip Gloss v2, bare `rota` opens the TUI, `Ctrl-\` then `d`, four tabs, a session
never stops by itself, stopped sessions stay 7 days, the keyboard follows the
device that types, size changes only on an explicit fit, one folder with
worktree optional, x/crypto/ssh directly, web as terminal plus session list plus
phone input bar (refined on 2026-11-04 to a composer plus quick buttons), Telegram
send-only, betas before a stable release. Later the same week: everything ships
as 1.4.0 betas; during them `go install ...@latest` may give the nested module's
beta, said in the README; the API key account stays out of the rotation; the
four projects above were studied (section 13). The owner
asked that nothing here conflict with the project.

## Calls made without asking

- `cmd/rota` as the nested module, rather than a second binary.
- One holder process per session, for every daemon session.
- `serve start|stop|...` rather than a new `daemon` verb.
- x/crypto/ssh directly rather than Wish.
- Bubble Tea v2 rather than a hand-written TUI, departing from the zero-dependency
  habit because the owner asked for beautiful and for best practices.
- One rota daemon with a holder per session, rather than a daemon per account.
- No new verb: `run --attach` and `run --detach`.
- `Ctrl-\` chord; keyboard moves freely within one person's devices.
- Audited against the project on 2026-11-01; findings are applied in place above.
- Held sessions exempt from `idle_timeout`, stopped ones kept 7 days.
- Foreground `rota serve` unchanged; holders only in daemon mode.
- `local` counts as the sole control user, or `[auth] local_user`.
- Typing never resizes; size changes only on an explicit fit.
- Conversation identity from Claude Code's registry file, never a guess.
- Holder protocol versioned apart from `--share`, every shipped major readable.
- Move limited to claude, through the existing `CopyForResume` rule.
