# Phase 5: events and sinks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The rota daemon has one small bus of typed events (accounts, sessions, viewers, the daemon itself) in a bounded ring, streamed by `GET /v1/events` as server-sent events, and pushes them to the sinks `server.toml` names (Telegram, webhook, command). No sink is on unless the file asks for one.

**Architecture:** The event's shape, its type names, the `on` patterns, the SSE framing and the ring are a new root package, `internal/events`, which the program's module imports the way it imports `internal/holdwire`. No Go event type becomes public. `api` publishes from the places where the things happen: the background refresh, the terminal session, the held session, `New` and `StopRefreshing`. It streams the ring over `GET /v1/events` on every server. The sinks are `cmd/rota/notify`, which uses the standard library only. It runs in the daemon process and reads `/v1/events` over `ctl.sock` through `cmd/rota/client`. It keeps a status file that `rota serve status` and phase 4's Services tab read.

**Tech Stack:** Go 1.27, standard library only (no new requirement in either module). The plan also uses SSE over `net/http`, HMAC-SHA256 and the Telegram Bot API `sendMessage`, which tests fake with a loopback listener.

**Spec:** `docs/superpowers/specs/2026-11-01-tui-daemon-ssh-design.md`. Read section 11 (Events), the "Sending and attention" part of section 13, sections 2 (registry) and 6 (daemon), "Public surface", "The owner's answers" and "Calls made without asking". Phase 1's plan, `docs/superpowers/plans/2026-11-06-phase-1-holder-daemon.md`, says what the holder, `heldTerm`, `ctl.sock` and `client` already are.

**Base:** `main` at `b9fd476` (v1.4.0-beta.10, which contains phase 1). This plan needs nothing from phases 2, 3, 4 or 6. *Hook points for other phases* names what they call or read.

## Global Constraints

- The root `go.mod` gains no `require`. `cmd/rota/go.mod` gains nothing in this phase: the design says "Sinks use only the standard library". `sh scripts/module.sh check` prints `module: ok` after every task.
- **Natural by default.** A `server.toml` with no `[[events.sink]]` sends nothing, starts no notifier, and writes no `sinks.json`. It also logs nothing about events. The one new thing such a server has is the route `GET|POST /v1/events`, which answers only someone who asks. No existing test's expectation changes, except where a task says so.
- **Secrets.** These never appear in a log line, an event, the status file, `--print-config`, an HTTP answer or argv: a bot token, a webhook URL's path or query, an HMAC key, the server's bearer token. `url` is tagged `secret`. Every error a sink reports goes through `notify.cleanErr` (Task 9). An event's `name` and `text` go through `wire.Unsecret` and `events.Clean` in `Server.emit` (Task 4).
- **Tests never reach the internet.** Telegram is faked by a loopback `httptest` server through the seam `notify.telegramAPI`. Webhooks go to loopback listeners. A command sink's program is a fake from `internal/fakecli`, never an sh script. No test starts a real claude, codex or grok. No test touches the keychain, `~/.claude`, `~/.codex`, `~/.rota`, or the installed `rota`.
- `<testenv>` is `env -u ROTA_CLAUDE_HOME -u CLAUDE_CONFIG_DIR -u ROTA_ACCOUNT_ID -u ROTA_ACCOUNT -u ROTA_PROVIDER -u ROTA_TOKEN ROTA_HOME="$(mktemp -d)"`, and it prefixes every `go test`.
- **The local CI run**, in this order from the repository root:
  1. `test -z "$(gofmt -l <every changed .go file>)"`
  2. `for os in linux darwin windows; do GOOS=$os go vet work || echo "vet $os FAILED"; done`
  3. `<testenv> go test -count=1 work`, then `<testenv> go test -count=1 -race work`
  4. `sh scripts/module.sh check`

  It passes when gofmt and vet print nothing, every test line is `ok`, and the last line is `module: ok`. `go test ./...` at the root skips the program, so no step uses it. A timing test that fails is run again alone with `-count=3` before anything is concluded, and the report names it.
- A task marked **docker** is also run under Linux in Docker by the integrator, serially. The implementer never runs docker.
- Linux first, then macOS. Windows builds, vets and passes or skips: `internal/events`, `api` and `cmd/rota/notify` are portable, and only the command sink's process-group kill has a unix file and a twin.
- No new verb. The only new flag is `rota serve status --test <sink|all>`. Every new route and flag goes into `docs/reference.md` in the task that adds it, or `docs_test` fails.
- New files stay under about 600 lines.
- Branches: each task on `p5-t<N>`, cut from `phase-5` as its group starts. The integrator fast-forwards or cherry-picks onto `phase-5`, which is cut from `phase-5-plan`. No PR, no tag, no version bump. Nothing goes to `main` without the owner's word. Never force-push.
- Commit subjects are one plain sentence saying what is now true. They have no body, no trailer, and no mention of AI or of any assistant.

### The event, as every task uses it

One JSON object. The first seven keys are always present, as the spec lists them. The rest are present only on the types named here:

| key | type | meaning |
|---|---|---|
| `id` | integer | Microseconds since the Unix epoch, made strictly increasing (Ruling R4) |
| `type` | string | one of the types below |
| `time` | string | RFC 3339 in UTC |
| `account` | integer | the account's id, `0` for none |
| `session` | string | the session's id, `""` for none |
| `name` | string | what the subject is called: the session's name (Ruling R12) when `session` is set, else the account's label, else `""` |
| `text` | string | words that follow `name` in a sentence, or a whole sentence when `name` is `""` (Ruling R13) |
| `reason` | string | `session.attention`: `permission`, `question`, `done`, `bell`, `title` or `quiet`. `session.ended`: `exited`, `stopped`, `interrupted` or `moved`. `session.started`: `resumed` or `moved`, else absent. `account.threshold`: `up` or `down`. `account.dead`: `login` or `token`. `account.rotated`: `spent`, `dead`, `credit`, `left`, `removed`, `back`, `order` or `none`. `viewer.*`: the principal's `via` |
| `exit` | integer | `session.ended`: the CLI's exit code, `-1` when interrupted |
| `window`, `percent`, `limit` | string, number, integer | `account.threshold` and `account.reset`: the window's name, its reading, the account's threshold |
| `viewer` | string | `viewer.*`: the principal's name |
| `sink` | string | `test`: the sink it is for, absent for every sink |

The types are `account.threshold`, `account.reset`, `account.dead`, `account.rotated`, `session.started`, `session.ended`, `session.attention`, `session.quiet`, `viewer.joined`, `viewer.left`, `daemon.started`, `daemon.stopped`, and `test`, which is the test button's own. A stream may also carry a `gap` frame, which is never published and never matched.

### The words, pinned for the tests

`name` + `" "` + `text` is the sentence a person reads (`events.Sentence`). Percentages are rounded to whole numbers. Times are `15:04 UTC`. Durations are `45s`, `12m`, `1h 3m`, `2h` or `2d 4h`. A folder is the base name of the session's cwd, never the path.

| type (reason) | text |
|---|---|
| `account.threshold` (up) | `reached its 80% limit: 5h at 82%` |
| `account.threshold` (down) | `is under its 80% limit again: 5h at 12%` |
| `account.reset` | `has its 5h window back: it had reached 95%` |
| `account.dead` (login) | `needs a login: ` + `Account.WhyDead()`, or, for a login that ended and is not marked dead, `its login ended on 2026-12-01 UTC; rota login 3 makes a new one` |
| `account.dead` (token) | `can no longer be reached from session refactor: the token it was started with ended at 21:20 UTC` |
| `account.rotated` | `is first in the rotation now: work@x.com reached its 80% limit`. The part after the colon, by reason: `needs a login`, `is out of credit`, `left the rotation`, `was removed`. `back` reads `is first in the rotation now: it can be used again`, and `order` reads `is first in the rotation now: the rotation was reordered` |
| `account.rotated` (none) | `No account is available in the rotation: every account in it is spent, needs a login or is out of credit` (name `""`) |
| `session.started` | `started by alice on work@x.com (claude) in app`. `by <who> ` is left out when nobody is named. A shell reads `started a shell in app`. A shared terminal reads `is shared from a terminal on this machine`. Resumed reads `was resumed on work@x.com (claude) in app`, and moved reads `moved to home@x.com (claude)` |
| `session.ended` (exited) | `ended with exit 0 after 1h 3m` |
| `session.ended` (stopped) | `was stopped by alice after 12m (exit 129)`. A stop by `idle` reads `was stopped after 12h with nobody attached`, and one by `shutdown` reads `was stopped as its server shut down` |
| `session.ended` (interrupted) | `was interrupted after 2h: its holder went away` |
| `session.attention` | permission: `is waiting for your permission`. question: `is asking you something`. done: `has finished and is waiting for you`. bell: `rang its bell`. title: `is idle again`. quiet: `has gone quiet` |
| `session.quiet` | `has printed nothing for 30s` |
| `viewer.joined` | `has alice attached, to watch`, or `…, to control` |
| `viewer.left` | `no longer has alice attached` |
| `daemon.started` | `The rota daemon 1.4.0-beta.11 started for /home/me/.rota with 3 sessions running and 2 stopped` |
| `daemon.stopped` | `The rota daemon for /home/me/.rota is stopping; its 3 running sessions go on` |
| `test` | `A test event from local` (the principal's name) |

### server.toml, as Task 3 reads it

```toml
[events]
link = ""                    # where a pushed message links a session from: https://box.tail1234.ts.net:8787
conversation_names = false   # name sessions in pushed messages by their conversation too, not only by --label or folder

[[events.sink]]
type = "telegram"            # "telegram", "webhook" or "command"
name = ""                    # what logs, status and --test call it; empty is its type
on = ["account.*", "session.attention", "session.ended"]   # empty is every event
bot_token_file = "/home/me/.rota/telegram.token"           # telegram: absolute, its owner's alone
chat_id = "123"                                            # telegram

[[events.sink]]
type = "webhook"
url = "https://example.com/hook"                           # https, or http to a loopback address; a secret
secret_file = "/home/me/.rota/hook.secret"                 # optional: HMAC-SHA256 in X-Rota-Signature

[[events.sink]]
type = "command"
command = ["notify-send", "rota"]                          # a name on PATH or an absolute path; the event goes to stdin
```

## Review Focus

1. **A sink that hangs or keeps failing**: Telegram down, a webhook that never answers, a command that never exits, a token file that is not there. Every other sink, the daemon and `serve stop` must carry on. That sink retries, then drops, says so once, and shows `failing`. Tests: Task 9 `TestASlowSinkDelaysNobodyElse` and `TestStopIsBoundedByAStuckSink`, Task 12 `TestACommandThatNeverExitsIsKilled`, and Task 13 `TestABrokenSinkHurtsNothing`.
2. **A secret leaking**:
   - the bot token, which net/http repeats inside a dial error's URL
   - a webhook URL whose path holds a Slack-style key
   - an Anthropic key inside an account's dead reason
   - all of these in the log, `sinks.json`, `--print-config`, an event's text or a status line

   Nothing may show any of them. Tests: Task 3 `TestPrintMasksTheWebhookURL`, Task 4 `TestEventTextCarriesNoSecret`, Task 9 `TestSecretsNeverReachTheLogOrTheFile`, Task 11 `TestADialErrorDoesNotRepeatTheToken`, and Task 13 `TestTheDaemonLogHoldsNoSinkSecret`.
3. **A stream's life**:
   - it outlives the server's 5-minute `ReadTimeout`
   - its reader stops reading
   - it keeps a shutdown waiting
   - it reconnects with `Last-Event-ID` across a daemon restart
   - a page sign-in that expires

   The stream must stay open, cost the server nothing, end promptly when shutdown begins, and resume without duplicates. Tests: Task 4 `TestAStreamOutlivesReadTimeout`, `TestAStuckReaderIsLetGo`, `TestStreamsEndWhenShutdownBegins`, `TestLastEventIDSurvivesARestart` and `TestAnExpiredSignInEndsItsStream`.
4. **Repeat storms**:
   - a daemon restart re-announcing every account over its threshold and every session waiting
   - a phone that reconnects every few seconds
   - a CLI that rings its bell in a loop
   - a reading that fails and then returns
   - the echo of every keystroke

   Each must say a thing once. Tests: Task 6 `TestTheFirstLookIsABaseline` and `TestAFailedReadingChangesNothing`, Task 7 `TestViewersArePeopleNotConnections`, and Task 8 `TestAnAdoptedSessionStartsFromABaseline`, `TestBellsAreRateLimited` and `TestEchoIsNotActivity`.
5. **`daemon.stopped` on the way out.** The last event must reach every sink before the process exits, and `serve stop` must not wait past its bound because of a sink. Tests: Task 9 `TestDaemonStoppedEndsTheRun`, and Task 13 `TestStopDeliversDaemonStopped`.

## Rulings

Each is the plan's own call where the spec is silent: what, why, and the cost if wrong.

- **R1** The shared contract is a root `internal/events`. Why: the spec wants no public event type, and the producer (`api`) and the readers (`client`, `notify`) must agree on one shape. `holdwire` already sets the precedent. Cost: a change to the shape touches both modules in one commit.
- **R2** Every `api.Server` has the ring and `GET|POST /v1/events` in its API group, the foreground `rota serve` too. Sinks run only in the daemon, as the spec says ("over `ctl.sock`"). A foreground serve whose file has sinks says once at start: `events: [[events.sink]] is sent by the rota daemon (rota serve start); this foreground server sends none`. `daemon.*` events exist only in a daemon. Why: one stream costs nothing, and phase 6's page can use it on either kind of server. Cost: foreground users who want pushes must run the daemon.
- **R3** Sinks run in every daemon, including one started with `--ctl-only` or started automatically by `--detach`. Why: `--ctl-only` is about ports, and a sink opens none. Cost: outbound messages from a daemon nobody started by name. See *For the owner*.
- **R4** An id is `max(now.UnixMicro(), last+1)`. So ids keep rising across a daemon restart, and a `Last-Event-ID` from before a restart replays the new daemon's whole ring and nothing twice. Cost: a wall clock stepped back by more than the gap delays a reconnecting client's events until the clock catches up.
- **R5** The ring keeps 1024 events. A `since` that is older than everything kept gets one `gap` frame first.
- **R6** What is true when the daemon starts is a baseline: accounts, adopted sessions, an adopted session's registry. Nothing about it is announced. `daemon.started` says how many sessions it took up. Cost: a crossing that happened while no daemon ran is never pushed.
- **R7** Account events are computed after each background sweep (`refresh_every`, default 2 minutes, plus the early sweep after a reset). With `refresh_every = "0s"` there are none. Cost: up to 2 minutes of latency.
- **R8** `account.threshold` is `rotation.Spent` changing between two readings that both exist. A missing reading changes no latch. There is no hysteresis, because usage only rises inside a window.
- **R9** `account.reset` fires once per window. That window read above 0%, and then one of these happened: its `ResetsAt` moved later by more than 1 minute, or `ResetsAt` passed and the reading fell, or `ResetsAt` passed and the window is gone.
- **R10** `account.dead` (reason `login`) fires on `Dead || LoginEnded()` going from false to true, and re-arms silently on a new login. `account.dead` (reason `token`) fires once per live session whose `token_until` passes. Both can be said for one claude login.
- **R11** `account.rotated` is `rotation.Pick` over the whole queue changing. Busy accounts are ignored, since busyness is momentary. An empty pick is `none`.
- **R12** A session's name in an event is its `--label`, else its folder, else its id. It is never the conversation's name or the title. With `[events] conversation_names = true`, `notify` swaps in the daemon's full name (`GET /v1/terminals/{id}`) before a sink sees the event. The stream itself never carries it.
- **R13** `text` follows `name` (see *The words*). So swapping the name never makes the sentence wrong.
- **R14** Viewers are announced by person (`via` + name), with a 10-second settle, so a reconnect says nothing. `local` (the control socket) is never announced. Nothing is said about a viewer leaving a session that has ended.
- **R15** Attention is computed every second for live held sessions only, from this precedence:
  - the registry's `waitingFor`: `permission` when it contains "permission", else `question`
  - its `status` going from busy to `idle`: `done`
  - with no registry: a bell, at most one each 30 s
  - with no registry: the title going from a working glyph (U+2800–U+28FF, `✢ ✶ ✻ ✽ ·`) to `✳` and staying there 2 s: `title`
  - with neither registry nor title signal: `quiet`

  `session.quiet` fires 30 s (`heldQuiet`) after activity. Output is not activity in these cases: in the first 10 s after a session is linked, within 2 s of a fit, within 50 ms of a keystroke, or below the offset that adoption replays. Both are held back, not dropped, while a viewer has typed into that session in the last 20 s. Cost if wrong: an attention signal arrives late or not at all on a CLI whose signals we read wrong. Nothing changes how the CLI runs.
- **R16** The holder counts OSC 9 and OSC 777 as bells. So a notification's text, which may be chat content, never crosses the link.
- **R17** `POST /v1/events` takes only `{"type":"test","sink":"<name>"}`, with control. A test event goes to the sink it names, or to every sink, whatever their `on` says.
- **R18** `notify` keeps `<store>/terminals/sinks.json` (0600, written atomically, at most once a second, no secrets). It records each sink's state and last test. `rota serve status` and phase 4 read it. A file whose `pid` is not the running daemon's is ignored.
- **R19** Delivery:
  - each sink has a queue of 64, and when full the oldest event is dropped
  - one attempt is bounded at 15 s
  - retries wait 1, 2, 5, 15, 30 and 60 s, and then the event is dropped
  - an event older than 10 minutes is dropped
  - `Retry-After` and Telegram's `retry_after` are honoured up to 5 minutes

  Each sink delivers in order.
- **R20** A webhook URL is `https://…`, or `http://` to a loopback address. There are no redirects. The URL is a secret, and its target is shown as `scheme://host`. The signature is `X-Rota-Signature: sha256=<hex HMAC-SHA256 of the body>`, with `X-Rota-Event: <type>` and `X-Rota-Delivery: <id>` beside it. 2xx is delivered. 429, 5xx and network errors are retried, and any other code is dropped. Loosening the scheme rule later breaks nobody. See *For the owner*.
- **R21** Telegram sends plain text: the sentence, then `\n<link>/terminal#<session>` when both are set. The text is cut at 4096 runes, and web previews are off. The token is read once at sink start from a file that is its owner's alone (`owner.Only`). `401`, `403`, `400` and `404` are dropped. The API base is a test seam, not a key.
- **R22** A command sink's argv comes from the file, as a name on `PATH` (looked up at sink start) or an absolute path. Its stdin is the event's JSON and `\n`. Its environment is the daemon's, without `ROTA_TOKEN` and without the variable `auth.token_env` names. Its stdout is discarded, and the first line of stderr (≤ 200 bytes) goes into the error. At 30 s its process group is killed. Exit 0 is delivered and exit 75 (`EX_TEMPFAIL`) is retried. Anything else, and the timeout, is dropped.
- **R23** A sink's name defaults to its type, and two sinks with one name are refused. Names match `^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`. An `on` pattern is a type, `<prefix>.*` for a prefix some type has, or `*`. Anything else is refused at load with the list of types. An empty `on` means every event.
- **R24** A wrong `[events]` fails `LoadConfig`, as any wrong key does. A sink that cannot run (its file unreadable or open to others, its program missing, its network down) is `failing` and logged, and never stops the daemon.
- **R25** Streams have these limits:
  - heartbeat `: ping` every 25 s
  - the request's read deadline cleared, a 30 s write deadline per write
  - at most 64 streams per server, then 503
  - a stream ends after its last event once `StopRefreshing` (or `Stop`) is called
  - a page sign-in that has expired or signed out ends its stream at the next heartbeat
- **R26** `StopRefreshing`, which both serve modes call as shutdown begins, also publishes `daemon.stopped` (a daemon only) and closes the ring. After `srv.Stop()`, the daemon waits up to 10 s (`eventsDrain`) for `notify` to finish. Cost: a sink slower than that misses `daemon.stopped`.
- **R27** `[events] link` is a key of its own, because the daemon cannot know the address a phone reaches it at. Empty sends no link.
- **R28** `rota serve status` lists the running sinks, and `rota serve status --test <sink|all>` probes them. This adds no new verb.
- **R29** `emit` clears C0 and C1 control bytes and DEL. It cuts `name` at 200 bytes and `text` at 1000 on a rune boundary, and runs both through `wire.Unsecret`.

## For the owner

Neither blocks the work. The plan proceeds with the recommendation, and changing either later is one line and a note.

1. **Sinks in a daemon nobody started by name (R3).** `rota run --detach` and the TUI start a `--ctl-only` daemon on their own. Should its sinks send?
   - (a) Yes, always. **Recommended**: the person wrote the sinks, and a daemon opens no port for them.
   - (b) Only in a daemon from `serve start` or the unit.
   - (c) Yes, and the auto-start line says `events: N sinks are on`.
   - (d) Only with `[events] auto = true`.
   - (e) Refuse the auto-start while sinks are configured.
   - (f) Ask on the terminal at auto-start.
2. **Webhooks over plain HTTP (R20).** Events carry account labels (e-mails) and folder names.
   - (a) https anywhere, and http only to loopback. **Recommended**, and loosening it later breaks nobody.
   - (b) http to loopback and to private ranges (RFC 1918, Tailscale CGNAT).
   - (c) http anywhere, with a warning at start.
   - (d) http anywhere with `allow_http = true` on that sink.
   - (e) https only, with no loopback exception.
   - (f) http anywhere.

## Hook points for other phases

These names and signatures are fixed now, so phases 2, 3, 4 and 6 can code against them before this phase lands. A task that finds one wrong stops and asks the integrator. It never renames one. Whichever phase lands second adds the call.

| For | Hook | What it is |
|---|---|---|
| 3, 4, 6 | `GET /v1/events?since=<id>&on=<pattern,…>` | SSE: `id:`, `event:`, `data:` (the event JSON), with `: ping` heartbeats and `retry: 3000` first. `Last-Event-ID` beats `since`. No `since` means from now. Watch role. A page signed in by cookie uses `new EventSource("/v1/events")`. A bearer token is sent with `fetch` and the header, since EventSource cannot set it |
| 4 | `POST /v1/events` `{"type":"test","sink":"telegram"}` → 202 `{"id":N}` | the test button. Control role |
| 3, 4 | `func (c *client.Client) Events(ctx context.Context, since int64, on ...string) (*client.EventStream, error)`, `func (s *client.EventStream) Next() (events.Event, error)`, `func (s *client.EventStream) Close() error` | `since < 0` is from now, and `0` is all kept. `Next` gives `io.EOF` when the daemon ends the stream, and an `Event{Type: events.Gap}` for a gap |
| 4 | `func (c *client.Client) TestEvent(sink string) (int64, error)` | `""` is every sink |
| 4 | `notify.ReadStatus(terminals string) (*notify.StatusFile, error)`, `notify.Describe(e api.EventsSection) []notify.Status`, `notify.Test(c *client.Client, terminals, sink string, within time.Duration) ([]notify.Outcome, error)` | the Services tab's sink list and its test button. `Test` blocks, so it runs as a `tea.Cmd` |
| 4 (api, resume and move) | `func (s *Server) sessionStarted(ts *termSession, by, why, text string)` | Every path that lists a new running session calls it once, after listing and before `go ts.run()`. `why` is `whyResumed` or `whyMoved`. `text` `""` is the default words. A path through `startHeld` sets the unexported `termRequest.why` instead |
| 4 (api, move) | `func (ts *termSession) endingAs(why, text string)` | called before a CLI is hung up for a move, so its `session.ended` says `moved` rather than `stopped` |
| 2 | (no call) | An SSH attach through `GET /v1/terminals/{id}/ws` with a `[[users]]` principal is announced as `viewer.joined` with that `via` and name. `local` is never announced. A TUI preview that attaches over WS from SSH is announced too, so phase 4 may prefer a preview that does not attach |
| 2, 4, 6 | merge points | Phase 2 edits `api/config.go` (`Config`, `Check`, `Print`), `docs/server.toml` and `runDaemon` in `cmd/rota/daemon_unix.go`. Phase 4 edits `guardedIn` and `roleFor`. Phases 2 and 4 edit `statusDoc` in `cmd/rota/daemon.go`. Everyone edits `docs/reference.md` and `docs/releases/1.4.0.md`. Each edit this plan makes there is a few added lines, named in its task |

## File Structure

| Path | Responsibility |
|---|---|
| `internal/events/` (create) | The type names, `Event`, `Match`/`CheckPatterns`, `Sentence`/`Clean`, the SSE frame writer and reader, and `Ring` |
| `cmd/rota/hold/session.go` (modify) | OSC 9/777 counts as a bell |
| `api/config_events.go` (create), `api/config.go` (modify), `docs/server.toml` | `[events]` and `[[events.sink]]`: parse, check, print |
| `api/events.go` (create), `api/server.go`, `api/principal.go` (modify) | The ring on the server: `emit`, the stream, the test event, `daemon.*`, the attention ticker, and the hooks in the refresh |
| `api/events_accounts.go` (create) | `account.*` from each sweep |
| `api/events_sessions.go` (create), `api/terminal.go`, `api/terminals.go`, `api/held_spawn.go`, `api/share.go` (modify) | `session.started`/`ended`, `viewer.*`, and a session's token ending |
| `api/attention.go` (create), `api/held.go`, `api/adopt.go`, `api/fakeholder_test.go` (modify) | `session.attention` and `session.quiet` |
| `cmd/rota/client/events.go` (create) | `Events`, `EventStream`, `TestEvent` |
| `cmd/rota/notify/` (create) | `notify.go` (start, read, route, queues, retry), `status.go`, `redact.go`, `webhook.go`, `telegram.go`, `command.go` with `command_unix.go`/`command_other.go`, and `main_test.go` |
| `internal/fakecli/fakecli.go` (modify) | `Spec.StdinTo`, for the command sink's fake |
| `cmd/rota/events_daemon.go` (create), `cmd/rota/daemon_unix.go`, `cmd/rota/daemon.go`, `cmd/rota/main.go` (modify) | Start and drain `notify` in the daemon, the foreground note, sinks in `serve status`, and `--test` |
| `docs/reference.md`, `docs/releases/1.4.0.md` | The manual and the next beta's bullet |

## Parallel groups

Tasks in one group share no file and can run at once. A group starts when the groups it needs are on `phase-5`.

| Group | Tasks | Needs | Files each task owns |
|---|---|---|---|
| A | T1, T2 | — | T1: `internal/events/*`. T2: `cmd/rota/hold/session.go`, `cmd/rota/hold/bell_test.go` |
| B | T3, T4, T5 | A | T3: `api/config_events.go`, `api/config.go`, `api/config_events_test.go`, `docs/server.toml`. T4: `api/events.go`, `api/events_test.go`, `api/events_helpers_test.go`, `api/server.go`, `api/principal.go`, `api/events_accounts.go` (stub), `api/attention.go` (stub), `docs/reference.md`. T5: `cmd/rota/client/events.go`, `cmd/rota/client/events_test.go` |
| C | T6, T7, T8, T9 | B | T6: `api/events_accounts.go`, `api/events_accounts_test.go`. T7: `api/events_sessions.go`, `api/events_sessions_test.go`, `api/events_sessions_unix_test.go`, `api/terminal.go`, `api/terminals.go`, `api/held_spawn.go`, `api/share.go`. T8: `api/attention.go`, `api/attention_test.go`, `api/attention_held_test.go`, `api/held.go`, `api/adopt.go`, `api/fakeholder_test.go`. T9: `cmd/rota/notify/{notify,status,redact,main_test,notify_test,status_test}.go`, and stubs `webhook.go`, `telegram.go`, `command.go` |
| D | T10, T11, T12 | C | T10: `notify/webhook.go`, `notify/webhook_test.go`. T11: `notify/telegram.go`, `notify/telegram_test.go`. T12: `notify/command.go`, `notify/command_unix.go`, `notify/command_other.go`, `notify/command_test.go`, `internal/fakecli/fakecli.go` |
| E | T13 | D | `cmd/rota/events_daemon.go`, `cmd/rota/events_daemon_test.go`, `cmd/rota/events_daemon_unix_test.go`, `cmd/rota/daemon_unix.go`, `cmd/rota/daemon.go`, `cmd/rota/main.go`, `docs/reference.md` |
| F | T14 | E | `docs/reference.md`, `docs/releases/1.4.0.md` |

T6, T7 and T8 read `api/events_helpers_test.go` (T4's) and never change it. T7 uses `api/fakeholder_test.go` as it is, and only T8 changes it. T4 creates the stubs `events_accounts.go` and `attention.go`, which T6 and T8 own from then on. T9 creates the stubs `webhook.go`, `telegram.go` and `command.go`, which T10, T11 and T12 own from then on.

The critical path is T1 → T4 → T7 → T13 → T14.

---

### Task 1: The contract (`internal/events`)

**Files:**
- Create: `internal/events/event.go`, `internal/events/sse.go` and `internal/events/ring.go`
- Test: `internal/events/event_test.go`, `internal/events/sse_test.go` and `internal/events/ring_test.go`

**Interfaces:**
- Produces:
  - **Constants.** `AccountThreshold = "account.threshold"`, `AccountReset`, `AccountDead`, `AccountRotated`, `SessionStarted`, `SessionEnded`, `SessionAttention`, `SessionQuiet`, `ViewerJoined`, `ViewerLeft`, `DaemonStarted`, `DaemonStopped`, `Test = "test"`, `Gap = "gap"`, and `var Types = []string{…}`, which is every one but `Gap`, in the table's order.
  - **`type Event struct`**, with the fields and JSON keys of *The event*: `ID int64`, `Type string`, `Time time.Time`, `Account int`, `Session string`, `Name string`, `Text string`, `Reason string` (omitempty), `Exit *int`, `Window string`, `Percent *float64`, `Limit int`, `Viewer string` and `Sink string`. All those after `Text` are omitempty.
  - `func Match(patterns []string, typ string) bool` and `func CheckPatterns(patterns []string) error`.
  - `func Sentence(e Event) string` and `func Clean(s string, max int) string`.
  - **SSE.** `func Frame(e Event) []byte`, `func GapFrame(oldest int64) []byte`, `const Heartbeat = ": ping\n\n"`, `const Retry = "retry: 3000\n\n"`, `type Reader`, `func NewReader(r io.Reader) *Reader` and `func (r *Reader) Next() (Event, error)`.
  - **The ring.** `type Ring`, `func NewRing(keep int, now func() time.Time) *Ring`, `func (r *Ring) Publish(e Event) (Event, bool)` (the event with its id, and false once closed), `func (r *Ring) Since(id int64) (evs []Event, gap bool, wake <-chan struct{})`, `func (r *Ring) Last() int64`, `func (r *Ring) Close()` and `func (r *Ring) Closed() <-chan struct{}`.

- [ ] **Step 1: Write the failing tests.**
  - `TestMatch` is a table:
    - `nil`, `"session.ended"` → true
    - `["*"]`, `"test"` → true
    - `["account.*"]`, `"account.reset"` → true
    - `["account.*"]`, `"session.ended"` → false
    - `["session.ended"]`, `"session.ended"` → true
    - any patterns with `Gap` → false
  - `TestCheckPatterns` refuses `"session.atention"` with `on: "session.atention" names no event; the events are account.threshold, …, test`. It also refuses `"account."`, `""`, `"*.ended"` and `"nothing.*"`, and accepts `["*"]`, `["account.*","session.ended"]` and `nil`.
  - `TestSentence`: `{Name:"refactor", Text:"rang its bell"}` → `refactor rang its bell`, and `{Text:"A test event from local"}` → `A test event from local`.
  - `TestCleanRemovesControlsAndBounds`:
    - `"a\x1b[31mb\nc\x7f"` → `a [31mb c `
    - a 300-byte name cut at 200 ends on a rune boundary with `…`
    - C1 `\u009b` becomes a space
  - `TestFrameRoundTrips`: an Event with every field set, whose Text holds `\n` and `"`, comes back from `NewReader(bytes.NewReader(Frame(e))).Next()` equal. `Frame` is exactly `id: <id>\nevent: <type>\ndata: <one-line JSON>\n\n`.
  - `TestReaderSkipsWhatIsNotAnEvent`: `Retry`, `Heartbeat`, `: anything` and a blank line yield nothing. Then a `GapFrame(7)` yields `Event{Type: Gap}` with `Text` `events before 7 were not kept`. Then EOF is `io.EOF`.
  - `TestReaderBoundsALine`: a 70 KiB `data:` line is an error, never a panic or a hang.
  - `FuzzReader`, seeded with the frames above, never panics.
  - `TestIDsIncreaseAndFollowTheClock`:
    - with a fixed clock at `t`, three publishes give `t.UnixMicro()`, `+1` and `+2`
    - a clock moved back one second still gives `+3`
    - `Publish` sets `ID` only, and keeps the `Time` it was given
  - `TestSinceAndGap`:
    - `NewRing(3, …)` with five events: `Since(0)` gives the last three and `gap` true
    - `Since(<id of the second>)` gives three and `gap` false
    - `Since(Last())` gives none
  - `TestWakeClosesOnPublishAndClose`: `wake` from `Since` is open, then closed after one `Publish`. A fresh `wake` closes on `Close`.
  - `TestAClosedRingTakesNothing`: after `Close`, `Publish` returns false, `Closed()` is closed, and `Since(0)` still gives what was kept.
- [ ] **Step 2: Run them.** `go test -count=1 ./internal/events/` fails: the package does not exist.
- [ ] **Step 3: Implement.** The ring is a slice used circularly under one mutex. `wake` is one channel per generation, closed and replaced on each publish. `dropped` is the id of the newest event pushed out, and `gap` is `id < dropped`. The reader is `bufio.Scanner` with a 64 KiB buffer. It joins multi-line `data:` with `\n`, ignores `id:` (the JSON carries it) and `retry:`, and decodes `data` as an Event. The JSON is the encoding the root uses elsewhere (`encoding/json/v2`). `Time` is written in UTC.
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add internal/events && git commit -m "internal/events holds the shape of an event, its type names and patterns, its server-sent framing and a bounded ring whose ids keep rising across restarts"`

### Task 2: A notification rings the holder's bell

**Files:**
- Modify: `cmd/rota/hold/session.go` (the `res.Events` switch, around line 348)
- Test: `cmd/rota/hold/bell_test.go` (create, `//go:build linux || darwin`), using the rig of `session_test.go` as it is

**Interfaces:**
- Produces `holdwire.StateMsg.Bells`, which now counts BEL, OSC 9 and OSC 777. `StateMsg` itself does not change. Task 8 reads it.

- [ ] **Step 1: Write the failing test.** `TestANotificationCountsAsABellAndSaysNothing`: the fake child prints `BEL`, then `ESC]9;done ESC\`, then `ESC]777;notify;T;B BEL`. The last `State` the link gets has `Bells == 3`, and no `State` frame's JSON contains `done` or `T: B`.
- [ ] **Step 2: Run it.** It fails, with `Bells == 1`.
- [ ] **Step 3: Implement.** `case vtscan.EventBell, vtscan.EventNotify:` counts as one bell and sets `changed`.
- [ ] **Step 4: Verify.** **docker.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add cmd/rota/hold && git commit -m "A holder counts the notifications a CLI asks for as bells, so the daemon hears of them without their words"`

### Task 3: `[events]` in `server.toml`

**Files:**
- Create: `api/config_events.go`, `api/config_events_test.go`
- Modify: `api/config.go`. The `Config` struct gains `Events EventsSection \`toml:"events"\`` after `Store`. `Check` calls `c.checkEvents()` last. `Print` writes `[events]` after `[store]` and before `[[users]]`, through `printEvents`. Also modify `docs/server.toml`.
- Test: `api/config_events_test.go`

**Interfaces:**
- Consumes `events.CheckPatterns` and `events.Types` (T1).
- Produces public API, which the spec names:
  - `type EventsSection struct { Link string \`toml:"link"\`; ConversationNames bool \`toml:"conversation_names"\`; Sink []EventSink \`toml:"sink"\` }`
  - `type EventSink struct { Type, Name string; On []string; BotTokenFile, ChatID, URL, SecretFile string; Command []string }`, with the tags `type`, `name`, `on`, `bot_token_file`, `chat_id`, `url,secret`, `secret_file` and `command`
- Produces, unexported: `func (c *Config) checkEvents() error`, and `printEvents`, whose signature is the implementer's, built on `Print`'s own helpers.
- `Serve()` and `Options` do not change. The daemon reads `cfg.Events` itself (T13).

- [ ] **Step 1: Write the failing tests.**
  - `TestNoSinkIsTheDefault`: `DefaultConfig().Events` is zero. `Print` holds `\n[events]\n`, `link            = ""` and `conversation_names = false`, and no `[[events.sink]]`.
  - `TestSinksAreReadFromTheFile`: the three blocks of *server.toml, as Task 3 reads it* load. `From` has `events.sink.type`. `Sink[1].URL` is the URL.
  - `TestSinkRefusals` is a table. Each case is one file whose error names the entry with `entryName("events.sink", i, name)`:
    - type `"slack"` → `[[events.sink]] #1: type "slack" is not a sink; write "telegram", "webhook" or "command"`
    - telegram without `chat_id` → `... chat_id is empty; a telegram sink sends to one chat`
    - telegram with a relative `bot_token_file`, or webhook with a relative `secret_file` → `... must be an absolute path`
    - telegram with `url` set → `... url is a webhook's; a telegram sink has bot_token_file and chat_id`
    - webhook `http://example.com/x` → `... a webhook goes over https, or over http only to this machine (127.0.0.1, ::1, localhost)`
    - webhook `ftp://…` and webhook with no host are refused
    - command `[]` → `... command is empty; give the program and its arguments`
    - command `["./x"]` → refused as relative
    - a bad `on` → the T1 message, prefixed with the entry
    - two sinks called `telegram` → `[[events.sink]] #2: there is already a sink called "telegram"; give one of them a name`
    - a name with a space is refused
    - `[events] link = "box:8787"` → `events.link must be an http or https address, as a phone opens it`

    `http://127.0.0.1:9/hook`, `http://localhost/hook`, `https://x/y?z` and command `["notify-send","rota"]` load.
  - `TestPrintMasksTheWebhookURL` (Review Focus 2): with the webhook URL `https://hooks.example/services/T0/B0/XYZSECRET`, `Print` shows `url = "(set)"` and the masked header. `XYZSECRET` appears nowhere in the output, while `bot_token_file`'s path does.
  - `TestSampleFileIsTheDefaults` (existing) passes with the new `docs/server.toml` block.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, with the messages above. `docs/server.toml` gains `[events]` with the two keys at their defaults, plus a comment showing one sink of each type, commented out so that the defaults still load equal.
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add api/config.go api/config_events.go api/config_events_test.go docs/server.toml && git commit -m "server.toml takes [events] and [[events.sink]], refuses a sink it could not run safely, and prints a webhook's address only as set"`

### Task 4: The bus on the server, its stream, and the daemon's own events

**Files:**
- Create: `api/events.go`, `api/events_test.go`, `api/events_helpers_test.go`, `api/events_accounts.go` (the stub `type accountWatch struct{}` and `func (s *Server) lookAtAccounts(st *store.Store) {}`) and `api/attention.go` (the stub `func (s *Server) attendAll(now time.Time) {}`)
- Modify:
  - `api/server.go`:
    - `Server` gains `events *events.Ring`, `accounts accountWatch` and `streams atomic.Int32`
    - `New` makes the ring before `claimDaemon`, and in a daemon calls `s.daemonStarted()` last and runs `go s.keepAttending()`
    - `StopRefreshing` becomes `s.stopSweeps(); s.closeEvents()`
    - `nextReset` calls `s.lookAtAccounts(st)` before `NextReset`
    - `maintain` calls `s.lookAtAccounts(st)` after `st.Maintain`
    - `guardedIn` gains `"GET /v1/events": s.streamEvents` and `"POST /v1/events": s.postEvent`
    - `trackingWriter` gains `Unwrap() http.ResponseWriter`
  - `api/principal.go`: `roleFor` gains `"GET /v1/events": RoleWatch` and `"POST /v1/events": RoleControl`
  - `docs/reference.md`: two rows in the endpoint table

**Interfaces:**
- Consumes `events.*` (T1).
- Produces, for T6 to T8 and for phase 4:
  - `func (s *Server) emit(e events.Event) events.Event`. It sets `Time` to `s.now().UTC()`, cleans `Name` and `Text` (R29) and publishes. It may be called with any lock held: it takes only the ring's lock, never blocks, and never calls back.
  - `func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request)`, `func (s *Server) postEvent(w http.ResponseWriter, r *http.Request)`, `func (s *Server) closeEvents()`, `func (s *Server) daemonStarted()` and `func (s *Server) keepAttending()`.
  - The knobs `var eventBeat = 25 * time.Second`, `var eventWrite = 30 * time.Second`, `var attendEvery = time.Second`, `const eventRing = 1024`, `const eventStreams = 64` and the seam `var newEventRing = events.NewRing`, which `New` calls as `newEventRing(eventRing, s.now)`.
  - Test helpers in `events_helpers_test.go`:
    - `func published(s *Server, types ...string) []events.Event` reads `Since(0)` and filters it
    - `func waitPublished(t *testing.T, s *Server, typ string, within time.Duration) events.Event`
    - `func openStream(t *testing.T, h http.Handler, query string, header http.Header) (*events.Reader, func())` serves `h` on a loopback `httptest.Server`

The stream's rules, which the tests pin:
- **Where it starts.** `since` is a non-negative integer, or 400 `since must be an event id`. `Last-Event-ID` beats it, and with neither the stream starts at `Last()`.
- **Filtering.** `on` is comma-separated and checked with `CheckPatterns`, giving 400 with its message.
- **Headers and preamble.** `Content-Type: text/event-stream; charset=utf-8`, `Cache-Control: no-store` and `X-Accel-Buffering: no`. Then `Retry`, flushed.
- **Deadlines.** `http.NewResponseController(w).SetReadDeadline(time.Time{})` at once, and `SetWriteDeadline(now+eventWrite)` before each write. A write that fails ends the stream.
- **The loop.** Each turn writes a `GapFrame` if `gap`, then each event that matches. `last` advances past events that do not match. It flushes, then waits on `wake`, the request's context, the heartbeat or `Closed()`. On `Closed()` it drains `Since(last)` once more and returns.
- **Sign-ins.** At each heartbeat, a principal with `Via == "user"` whose `session` is no longer in `s.sessions.get`, or whose `Expires` has passed, ends the stream.
- **Limits.** More than `eventStreams` at once is 503 `too many event streams are open`.
- **`POST`.** It takes a body of at most 4 KiB, `{"type":"test","sink":"<≤64 bytes>"}`. Any other type is 400 `only a test event can be posted: {"type":"test","sink":"<name or empty for every sink>"}`. A closed ring is 503 `this server is stopping`. Otherwise it answers 202 `{"id":N}`.
- **The daemon's own events.** `daemon.started` comes after `sweepRecords`. Its counts are this server's live terminals and the stopped records from `holdwire.Scan`. The store is `s.opts.Dir`. `closeEvents` runs once. In a daemon it emits `daemon.stopped` with the live count, then closes the ring.

- [ ] **Step 1: Write the failing tests** (`api/events_test.go`).
  - `TestAStreamStartsWhereItIsAsked`: emit three events. `since=0` gives three in order with ids, `since=<first>` gives two, and with neither a fourth emitted afterwards is the only one.
  - `TestLastEventIDBeatsSince`.
  - `TestOnFilters`: `on=account.*` gives only account events. `on=session.atention` is 400 with T1's message.
  - `TestAGapIsSaid`: with the ring made small through a test seam `var newEventRing = events.NewRing`, a stale `since` gets a `gap` frame first.
  - `TestAStreamOutlivesReadTimeout` (Review Focus 3): `httptest.NewUnstartedServer` with `Config.ReadTimeout = 200ms`. The stream is still open after 1 s and receives an event emitted at 800 ms.
  - `TestAStuckReaderIsLetGo` (Review Focus 3): with `eventWrite = 100ms`, a raw connection that never reads while 5 000 events are emitted is closed within 2 s. `streams` returns to 0, and emitting never blocked longer than 10 ms.
  - `TestStreamsEndWhenShutdownBegins` (Review Focus 3): with a stream open, `StopRefreshing()` ends it with EOF, and `httptest.Server.Close()` returns within 1 s.
  - `TestHeartbeatsKeepAStreamAlive`: `eventBeat = 50ms` gives `: ping` lines.
  - `TestAnExpiredSignInEndsItsStream` (Review Focus 3): a `[[users]]` cookie stream ends at the first heartbeat after `s.sessions` drops that session.
  - `TestStreamsAreBounded`: the 65th is 503.
  - `TestEventsAreWatchAndTestIsControl`: a watch token reads the stream. A watch token's `POST` is 403, and `roleFor`'s table test passes.
  - `TestATestEventIsPublished`: `{"type":"test","sink":"tg"}` gives 202 with an id, and the event has `type test`, `sink tg` and the text `A test event from <name>`. `{"type":"account.dead"}` is 400.
  - `TestEventTextCarriesNoSecret` (Review Focus 2): `emit` of a Text holding `sk-ant-oat01-AAAA…` and `\x1b]0;x\x07` publishes neither.
  - `TestADaemonSaysItStartedAndIsStopping` (`//go:build linux || darwin`, with the fake holder as in `held_test.go`): one live session and one ended record. `daemon.started`'s text is `The rota daemon <wire.Version> started for <dir> with 1 session running and 1 stopped`. After `StopRefreshing`, the last event is `daemon.stopped`, and `Publish` is refused.
  - `TestLastEventIDSurvivesARestart` (Review Focus 3): two daemons in turn on one store. A `Last-Event-ID` from the first given to the second replays the second's `daemon.started`, and nothing from the first.
  - `TestAForegroundServerHasTheStreamAndNoDaemonEvents`: a server without `HoldCommand` streams, and never publishes `daemon.*`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, and the reference rows: `| GET | /v1/events | watch | the server's events as a stream (text/event-stream): since or Last-Event-ID, on |` and `| POST | /v1/events | control | publish a test event: {"type":"test","sink":"…"} |`.
- [ ] **Step 4: Verify.** **docker.** The local CI run passes, with `TestReferenceDocumentsEveryRoute` and the role-table test among the tests.
- [ ] **Step 5: Commit.** `git add api docs/reference.md && git commit -m "Every server keeps its events in a ring and streams them on GET /v1/events, takes a test event on POST, and a daemon says when it starts and when it stops"`

### Task 5: The client reads the stream

**Files:**
- Create: `cmd/rota/client/events.go`, `cmd/rota/client/events_test.go` (`//go:build linux || darwin`)

**Interfaces:**
- Consumes `events.Reader` and `events.Event` (T1). The route's behaviour (T4) is faked in the test by a handler that writes `events.Frame`.
- Produces the hooks in the table above: `Events`, `EventStream` (`Next`, `Close`) and `TestEvent`. `Events` sends `since` only when it is ≥ 0, and `on` joined by commas. It uses the request's context with no timeout. An answer that is not 200 is a `*Refusal`. `Close` makes a waiting `Next` return within 100 ms.

- [ ] **Step 1: Write the failing tests.** They use the `terminals(t)` and `ClaimDaemon` pattern of `client_test.go`, with an `http.Server` on `ctl.sock` whose handler records the query.
  - `TestEventsReadsInOrderToTheEnd`: three frames, then the handler returns. `Next` gives three events and then `io.EOF`. The query was `since=0&on=account.%2A`.
  - `TestEventsFromNowSendsNoSince`: `since = -1` leaves `since` out.
  - `TestAGapComesBackAsAnEvent`.
  - `TestCloseUnblocksNext`.
  - `TestARefusedStreamIsARefusal`: a 400 with `{"error":"on: …"}` gives `*Refusal{Status:400}` and that message.
  - `TestTestEventPostsAndSaysTheID`: the body is `{"type":"test","sink":"tg"}`, and 202 `{"id":42}` gives 42.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add cmd/rota/client && git commit -m "The daemon's client reads its event stream from where it is asked and posts a test event"`

### Task 6: Account events from each sweep

**Files:**
- Modify: `api/events_accounts.go`. It replaces T4's stub and keeps its two names.
- Test: `api/events_accounts_test.go`

**Interfaces:**
- Consumes `s.emit`, the stub's call sites in `nextReset` and `maintain` (T4), `rotation.Pick`, `Cutoff`, `Spent`, `Available`, `InQueue`, `rota.Account.Percent`, `Quota.Windows`, `Dead`, `WhyDead`, `LoginEnded`, `LoginUntil` and `OutOfCredit`.
- Produces:
  - `type accountWatch struct { mu sync.Mutex; last *accountsLook }`
  - `type accountLook struct { id, order, limit int; label, provider, why, window string; read, dead, spent, credit, queued, available bool; percent float64; windows map[string]windowLook }`
  - `type windowLook struct { percent float64; resets time.Time }`
  - `type accountsLook struct { byID map[int]accountLook; first int }`
  - `func lookAccounts(accounts []*rota.Account) accountsLook`
  - `func diffAccounts(prev, next accountsLook, now time.Time) ([]events.Event, accountsLook)`. The second result is `next` with the latches of accounts that were not read carried over from `prev`.
  - `func (s *Server) lookAtAccounts(st *store.Store)`. Its first call is the baseline.

- [ ] **Step 1: Write the failing tests.** They are tables over `diffAccounts`, with accounts built in memory.
  - `TestTheFirstLookIsABaseline` (Review Focus 4): `diffAccounts(accountsLook{}, X, now)` gives nothing for an account over its threshold, a dead one, and a first in the queue. `lookAtAccounts` twice on an unchanged store emits nothing.
  - `TestThresholdCrossesUpAndBackDown`:
    - 70% → 82% with threshold 80 gives one `account.threshold` with `reason up`, `percent 82`, `limit 80`, `window 5h` and the text from *The words*
    - 82 → 90 gives nothing
    - 90 → 12 gives `reason down`
  - `TestAFailedReadingChangesNothing` (Review Focus 4): 82% (spent), then `Quota == nil`, then 85%. That gives no events, and the latch stays spent throughout.
  - `TestAWindowResetIsSaidOnce`:
    - `ResetsAt` moved by 5h at 95% gives one `account.reset` with `window 5h`, `percent 95` and the text
    - the same reading again gives nothing
    - a `ResetsAt` 30 s later gives nothing
    - a window at 0% that resets gives nothing
  - `TestADeadAccountIsSaidOnce`:
    - `Dead` with `DeadReason "x"` gives one `account.dead` with `reason login` and text `needs a login: x`
    - still dead gives nothing
    - alive again gives nothing
    - dead again gives one
    - `LoginEnded` without `Dead` gives one, with the login-ended text
  - `TestTheRotationMoves`:
    - first #1, then #1 spent: `account.rotated` with `account 2` and `reason spent`
    - then #2 dead and #1 still spent: `account 0` and `reason none`
    - then #1 reset: `account 1` and `reason back`
    - #1 given order 0: `reason left`
    - #1 removed: `reason removed`
  - `TestANewAccountIsABaseline`: an account first seen in a later look emits nothing until it changes.
  - `TestAccountEventsFollowTheSweep`: a real store (the api tests' store helpers) has one account at 50% with threshold 60. Call `lookAtAccounts`, write 70% to the store, and call `lookAtAccounts` again. `published(s, "account.threshold")` has one event.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement** to R7–R11. The window named is the one `Percent()` takes its top from.
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add api/events_accounts.go api/events_accounts_test.go && git commit -m "The daemon says when an account crosses its threshold, gets a window back, needs a login, or stops being the rotation's first choice, once each and never for what it found at start"`

### Task 7: Sessions and viewers

**Files:**
- Create: `api/events_sessions.go`, `api/events_sessions_test.go` and `api/events_sessions_unix_test.go` (`//go:build linux || darwin`)
- Modify:
  - `api/terminal.go`:
    - `termSession` gains `stoppedBy`, `endWhy` and `endText` (strings), `present map[string]*presence` and `tokenSaid bool`
    - `termConn` gains `via string`, set in `newTermConn`
    - `kill` records `by` into `stoppedBy`
    - `finish` calls `ts.sessionEnded(code)`
    - `attach` calls `ts.viewerCame(tc)`, and `detach` calls `ts.viewerWent(tc)`, each under `ts.mu`
    - `sweepTerminals` calls `s.tokensEnded(now)`
  - `api/terminals.go`: `termRequest` gains the unexported `why string`, and `startTerminal` calls `sessionStarted`
  - `api/held_spawn.go`: `startHeld` calls `sessionStarted`
  - `api/share.go`: the share's accept calls `sessionStarted`

  Each call is placed beside the existing `ts.audit("terminal created" | "terminal shared")`, before `go ts.run()`.

**Interfaces:**
- Consumes `s.emit` (T4) and the helpers.
- Produces the phase-4 hooks: `func (s *Server) sessionStarted(ts *termSession, by, why, text string)`, `func (ts *termSession) endingAs(why, text string)`, `const whyResumed = "resumed"` and `const whyMoved = "moved"`.
- Also produces:
  - `const whyExited = "exited"`, `whyStopped = "stopped"` and `whyInterrupted = "interrupted"`
  - `func (ts *termSession) eventName() string`, which is R12's name
  - `func (ts *termSession) sessionEnded(code int)`, `func (ts *termSession) viewerCame(tc *termConn)`, `func (ts *termSession) viewerWent(tc *termConn)` and `func (s *Server) tokensEnded(now time.Time)`
  - `var viewerSettle = 10 * time.Second`
- **The ended reason.** It is `endWhy` if `endingAs` set it. Otherwise `interrupted` when the backend is a `*heldTerm` whose `state` is `heldInterrupted`, else `stopped` when `stoppedBy != ""`, else `exited`.
- **A session that is let go** (`left`) emits nothing: `run` returns before `finish`. An adopted session never emits `session.started`.

- [ ] **Step 1: Write the failing tests.**
  - `TestSessionNamesNeverComeFromTheConversation` (pure): a label wins. With no label, `cwd=/x/proj` gives `proj`. With neither, the id. A held session's `convName` `secret-chat` never shows.
  - `TestEndedSaysWhyAndHowLong` (pure, `finish` on a session built in memory with a moved clock): `exited` gives `ended with exit 0 after 1h 3m` with `exit 0`. `stoppedBy "alice"` gives `stopped` and `was stopped by alice after 12m (exit 129)`. `idle` and `shutdown` read as in *The words*. `endingAs(whyMoved, "is moving to home@x.com")` gives `reason moved` and that text.
  - `TestViewersArePeopleNotConnections` (Review Focus 4, `viewerSettle = 50ms`):
    - alice attaches twice: one `viewer.joined` with `viewer alice`, `reason user` and `has alice attached, to control`
    - one connection drops: nothing
    - the other drops: `viewer.left` after the settle
    - she comes back within the settle: nothing
    - bob, a watcher, is announced `…, to watch`
  - `TestLocalIsNeverAnnounced`: `Via "local"` attach and detach give no `viewer.*`.
  - `TestNoViewerLeavesAnEndedSession`: the session ends with alice attached, and no `viewer.left` follows.
  - `TestATokenThatEndsIsSaidOnce`: `tokenUntil` one minute ago. Two `sweepTerminals` give one `account.dead` with `reason token`, `session <id>`, `account <id>` and the text.
  - `TestStartedAndEndedOnALocalTerminal` (unix, the tty fake from `internal/fakecli`, as `terminal_test.go` does): `POST /v1/terminals` gives `session.started` with `started by <name> on <label> (<provider>) in <folder>`. Typing `bye` gives `session.ended` with `reason exited`. `DELETE` on a second session gives `reason stopped`.
  - `TestHeldSessionsStartEndAndAreInterrupted` (unix, fake holder): a start gives `session.started`, and an exit 4 gives `session.ended` with `exit 4`. kill -9 of the holder gives `reason interrupted` and `exit -1`.
  - `TestARestartAnnouncesNoSession` (unix): `Stop`, then a second `New` on the store. The second server publishes no `session.started` and no `session.ended` for the adopted session, and its later exit gives one `session.ended`.
  - `TestWhyReachesTheStart`: `sessionStarted(ts, "alice", whyResumed, "")` gives `reason resumed` and `was resumed on …`. With `startHeld` and `req.why = whyMoved`, it gives `reason moved`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.** A `presence` is `{n int; timer *time.Timer}`, keyed `via + "\x00" + name`. Timers re-check `n == 0 && !ts.ended` under `ts.mu` before they emit.
- [ ] **Step 4: Verify.** **docker.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add api && git commit -m "The server says when a session starts or ends, why and after how long, when a person attaches to one or leaves it, and when the token a session runs on ends"`

### Task 8: Attention and quiet

**Files:**
- Modify:
  - `api/attention.go`, replacing T4's stub
  - `api/held.go`:
    - `heldTerm` gains `attn attention`, `bells uint64`, `keyAt`, `typedAt`, `fitAt`, `linkedAt` and `activeAt` (all `time.Time`), `adopted bool` and `replayTo int64`
    - `noteState` keeps `msg.Bells`
    - `read` calls `h.noteOutput(off, now)` on `Output` and sets `fitAt` on `Resized`
    - `typeFrom` sets `keyAt`, and `typedAt` when `tc != nil`
  - `api/adopt.go`: the `heldTerm` literal gets `adopted: true`, `replayTo: w.End` and `linkedAt: s.now()`
  - `api/fakeholder_test.go`: the typed lines `state title <text>`, `state status <s> [waiting]` and `state bell` each send a `State` frame

  T8 does not edit `api/held_spawn.go`, which is T7's in this group. A session `startHeld` made has a zero `linkedAt`, and `noteOutput` sets it to `now` at the first output. So the start window runs from the CLI's first bytes.
- Test: `api/attention_test.go` (pure) and `api/attention_held_test.go` (`//go:build linux || darwin`)

**Interfaces:**
- Consumes `s.emit` and `keepAttending` (T4), `holdwire.Registry`, and `StateMsg.Bells` (T2: a holder from before T2 still counts BEL).
- Produces:
  - `type attnIn struct { Now, LastActive, TypedAt time.Time; Registry *holdwire.Registry; Title string; Bells uint64; Adopted bool }`
  - `type attention struct { … }`, the latches
  - `func (a *attention) step(in attnIn) (reason string, quiet bool)`. `reason` is `""` for none, and at most one of each comes per step.
  - `func (h *heldTerm) noteOutput(off int64, now time.Time)`, whose caller holds `h.mu`
  - `func (s *Server) attendAll(now time.Time)`, which emits `session.attention` with `reason` and `session.quiet`, each with the session's `eventName`, `account` and `session`
  - `const attnStart = 10 * time.Second`, `attnFit = 2 * time.Second`, `attnEcho = 50 * time.Millisecond`, `attnTyping = 20 * time.Second`, `attnTitle = 2 * time.Second` and `attnBell = 30 * time.Second`. Quiet uses `heldQuiet`.
- Rules (R15):
  - Typing within `attnTyping` defers both kinds, and keeps every latch armed.
  - A registry counts once its `Status` or `WaitingFor` has been non-empty, and then bell, title and quiet-attention are off.
  - An adopted session's first step is a baseline.

- [ ] **Step 1: Write the failing tests** (pure tables on `step`, with a moved `Now`).
  - `TestWaitingForIsPermissionOrQuestion`:
    - `WaitingFor "permission"` gives `permission` once
    - `"permission"` again gives nothing
    - `""` then `"ask_user"` gives `question`
  - `TestDoneIsBusyThenIdle`: `busy` then `idle` gives `done`. `idle` from the start gives nothing.
  - `TestBellsAreRateLimited` (Review Focus 4): bells 1, 2, 3 within 30 s give one `bell`, and 31 s later a new bell gives another. With a registry, bells give nothing.
  - `TestTheTitleTurningIdle`: `⠂ Task` then `✳ Task` held 1 s gives nothing, and held 2 s gives `title`. `✳` without a working glyph before gives nothing.
  - `TestQuietAfterActivity`: activity at t, then t+29 s gives nothing, and t+30 s gives `quiet` true. No registry and no title gives `reason quiet` as well. With a registry, `quiet` true only. No new activity gives nothing again.
  - `TestTypingDefersAndDoesNotDrop`: `WaitingFor` set 5 s after `TypedAt` gives nothing. At `TypedAt + 20 s` it gives `permission`.
  - `TestAnAdoptedSessionStartsFromABaseline` (Review Focus 4): `Adopted` with `WaitingFor "permission"` at the first step gives nothing, and a later change to `""` and back gives `permission`.
  - `TestEchoIsNotActivity` (Review Focus 4, on `noteOutput` with a `heldTerm` built in memory). These give no `activeAt`:
    - output 30 ms after `keyAt`
    - output 1 s after `fitAt`
    - output within 10 s of `linkedAt`
    - output at an offset below `replayTo`

    Output 1 s after a key does.
  - `TestAttentionReachesTheBus` (unix, fake holder): a held session, then the typed line `state status busy`, then `state status idle waiting` sent with `s.clock` moved and `attendAll` called. That gives `session.attention` with `reason question`, then `permission` after `state status idle permission`. `published` shows the session's name, never the fake's conversation name.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** **docker.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add api && git commit -m "A held session says when it needs a person, from its registry first and its bell, title or silence otherwise, never while someone is typing into it and never twice"`

### Task 9: The notifier

**Files:**
- Create in `cmd/rota/notify/`:
  - `notify.go`, `status.go`, `redact.go`, `main_test.go` (`TestMain` calls `fakecli.Maybe()`), `notify_test.go` and `status_test.go`
  - the stubs `webhook.go`, `telegram.go` and `command.go`, each one constructor returning `errors.New("<type> sinks are not built yet")`

**Interfaces:**
- Consumes `api.EventsSection` and `api.EventSink` (T3), `client.Events`, `client.TestEvent`, `client.DialNow` and `client.Get` (T5 and phase 1), and `events.*` (T1).
- Produces, for T10–T13 and phase 4:
  - `type Options struct { Events api.EventsSection; Terminals string; Log *slog.Logger; Version string; HideEnv []string; Dial func(ctx context.Context, since int64) (Source, error); Lookup func(session string) (string, error) }`. A nil `Dial` or `Lookup` is the control socket.
  - `type Source interface { Next() (events.Event, error); Close() error }`
  - `type Notifier`, `func Start(o Options) (*Notifier, error)` (`nil, nil` with no sink) and `func (n *Notifier) Wait(until time.Time) bool` (true at once on nil)
  - `type sender interface { send(ctx context.Context, e events.Event) error; secrets() []string }`
  - `type env struct { version, link string; hide []string; log *slog.Logger }`
  - `func newSender(s api.EventSink, e env) (sender, error)` dispatches to `newWebhook`, `newTelegram` and `newCommand`, each `func(api.EventSink, env) (sender, error)`
  - `func again(err error, after time.Duration) error` and `func dropped(err error) error`, which classify a failed attempt
  - `func redact(s string, secrets []string) string` and `func cleanErr(err error, secrets []string) string`. `cleanErr` unwraps `*url.Error` to its `Err`, redacts, and joins the result onto one line of at most 300 bytes.
  - `func targetOf(s api.EventSink) string` gives `chat <id>`, `<scheme>://<host>` or `<base of argv[0]>`
  - `type Status struct { Name, Type, Target string; On []string; State string; Sent, Dropped int; LastSent time.Time; LastError string; LastErrorAt time.Time; LastTest *TestResult }`, with the JSON keys `name`, `type`, `target`, `on`, `state` (`waiting`, `ok`, `failing` or `off`), `sent`, `dropped`, `last_sent`, `last_error`, `last_error_at` and `last_test`. The times are `omitzero`.
  - `type TestResult struct { ID int64; OK bool; Error string; At time.Time }`
  - `type StatusFile struct { PID int; Updated time.Time; Sinks []Status }`
  - `func StatusPath(terminals string) string` (`<terminals>/sinks.json`), `func ReadStatus(terminals string) (*StatusFile, error)`, `func Describe(e api.EventsSection) []Status` (`off`, reads no file) and `func Test(c *client.Client, terminals, sink string, within time.Duration) ([]Outcome, error)`
  - `type Outcome struct { Sink string; Answered, OK bool; Error string }`
  - The knobs `var backoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}`, `queueDepth = 64`, `attemptTimeout = 15 * time.Second`, `maxAge = 10 * time.Minute`, `maxAfter = 5 * time.Minute`, `statusEvery = time.Second` and `redialMax = 30 * time.Second`
- Rules (R17–R19, R24):
  - The reader dials with `since=0` first and then with the last id, backing off up to `redialMax`. It logs `events: some events were missed` once per gap. It stops reading after routing `daemon.stopped`.
  - `test` goes to the sink it names, or to every sink. Every other type goes to each sink whose `On` matches.
  - With `ConversationNames` and a session id, `Lookup` replaces `Name`. It is cached 30 s, and a failed lookup keeps the name.
  - A sender that fails to build is `failing`, with its cleaned error, and gets nothing.
  - The log says `events: <name> failing: <err>` when a sink starts failing, `events: <name> sends again` when it recovers, and `events: <name> dropped N events: <why>` at most once a minute.
  - `Wait` waits for the reader and the queues. At `until` it cancels in-flight attempts, counts the remainder as dropped, writes the status file and returns false.

- [ ] **Step 1: Write the failing tests** with a fake `Source` (a channel) and fake senders (a test-only `newSender` seam: `var makeSender = newSender`).
  - `TestNoSinkStartsNothing`: `Start(Options{})` is `nil, nil`. No file is made, and nothing is logged.
  - `TestEventsReachTheSinksTheyAreFor`: sink A with `on ["account.*"]` and sink B with `on nil`. A gets one of the three events, and B gets all three.
  - `TestATestEventGoesToTheSinkItNames`: `{type test, sink B}` reaches B alone, whatever its `on`. `sink ""` reaches both.
  - `TestASlowSinkDelaysNobodyElse` (Review Focus 1): A blocks forever, and B gets ten events within 200 ms.
  - `TestRetriesBackOffThenDrop` (backoff 1 ms):
    - three `again` failures then success delivers once, with `Sent 1`
    - always `again` gives `Dropped 1` after 7 attempts, one log line, and state `failing`
    - `dropped(...)` is never retried
    - `again(err, 50ms)` waits at least 50 ms
  - `TestAFullQueueDropsTheOldest`: `queueDepth = 2` with a blocked sender. Five events give `Dropped 3`, and the last two are the ones delivered.
  - `TestTheStreamComesBackFromWhereItWas`: the source fails after id 5, and the second `Dial` is asked `since=5`.
  - `TestStatusFileSaysWhatHappened`: the file is 0600, with `pid = os.Getpid()`, states `ok` and `failing`, and `last_test` holding the test's id and outcome. `ReadStatus` reads it back.
  - `TestSecretsNeverReachTheLogOrTheFile` (Review Focus 2): a sender whose `secrets()` is `["TOKEN123", "/services/T0/XYZ"]` fails with `Post "https://h/botTOKEN123/x": dial tcp: refused` and `…/services/T0/XYZ…`. Neither secret is in the log or the file.
  - `TestDaemonStoppedEndsTheRun` (Review Focus 5): after `daemon.stopped` is routed, the reader stops. `Wait` returns true once that event is delivered.
  - `TestStopIsBoundedByAStuckSink` (Review Focus 1): with a sender that never returns, `Wait(now+100ms)` returns false within 300 ms and counts the event dropped.
  - `TestConversationNamesAreOptIn`: `Lookup` is called only with `ConversationNames` true, and the sink then sees the looked-up name.
  - `TestDescribeReadsNoFile`: `Describe` of a telegram sink whose token file does not exist gives `{Name telegram, Type telegram, Target "chat 123", State off}`.
  - `TestTestWaitsForEachAnswer`: a fake daemon (`client_test`'s pattern) answers `POST /v1/events` with `{"id":7}`. The status file gets `last_test.id 7` for both sinks 100 ms later. `Test(c, dir, "", time.Second)` gives two answered outcomes. An unknown sink is `no event sink is called "x"; the sinks are a, b`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add cmd/rota/notify && git commit -m "The notifier reads the daemon's events once, gives each sink its own queue that retries and then drops, and writes down how each sink is doing without a secret in it"`

### Task 10: The webhook sink

**Files:**
- Modify: `cmd/rota/notify/webhook.go`, replacing the stub
- Test: `cmd/rota/notify/webhook_test.go`

**Interfaces:**
- Consumes `sender`, `env`, `again`, `dropped`, `targetOf` (T9) and `owner.Only`.
- Produces `func newWebhook(s api.EventSink, e env) (sender, error)`. It reads `secret_file` with `owner.Only`, trims it, and refuses one that is empty.
- The request (R20) is `POST`, with a body of the event's JSON and these headers:
  - `Content-Type: application/json`
  - `User-Agent: rota/<version>`
  - `X-Rota-Event`
  - `X-Rota-Delivery`
  - `X-Rota-Signature: sha256=<hex>` only with a secret

  `CheckRedirect` returns `http.ErrUseLastResponse`, so a 3xx is a dropped failure `webhook answered 302 Found`. `secrets()` is the URL, its path and query, and the key.

- [ ] **Step 1: Write the failing tests** (an `httptest` loopback server).
  - `TestAWebhookIsSignedAndLabelled`: the receiver checks `hmac.Equal` of HMAC-SHA256(key, body) and the headers, and decodes the body to the event.
  - `TestAWebhookWithoutASecretIsUnsigned`.
  - `TestServerErrorsAreTriedAgainAndOthersAreNot`: 503 gives `again`, 429 with `Retry-After: 2` gives `again` whose wait is 2 s, and 404 and 302 give `dropped`. The test reads the classified error and does not sleep.
  - `TestAWebhookErrorHoldsNoURL` (Review Focus 2): a closed port with the URL `http://127.0.0.1:<p>/services/T0/XYZ?k=SECRET`. `cleanErr(err, s.secrets())` contains neither `XYZ` nor `SECRET`.
  - `TestASecretFileOthersCanReadIsRefused` (unix): at 0644, `newWebhook` fails with `owner.Only`'s message, and holds no key.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add cmd/rota/notify/webhook.go cmd/rota/notify/webhook_test.go && git commit -m "A webhook sink posts each event signed with HMAC-SHA256, follows no redirect, and tries again only where the server asked it to"`

### Task 11: The Telegram sink

**Files:**
- Modify: `cmd/rota/notify/telegram.go`, replacing the stub
- Test: `cmd/rota/notify/telegram_test.go`

**Interfaces:**
- Consumes the T9 contract.
- Produces `func newTelegram(s api.EventSink, e env) (sender, error)` and `var telegramAPI = "https://api.telegram.org"`.
- The send (R21) is `POST <telegramAPI>/bot<token>/sendMessage` with `{"chat_id":…,"text":…,"disable_web_page_preview":true}`. It reads `{"ok","error_code","description","parameters":{"retry_after"}}` back.
- The token comes from `bot_token_file`, read with `owner.Only`. It is trimmed, must be at most 256 bytes, and is refused when empty with `bot_token_file <path> holds no bot token`. `secrets()` is the token and `bot<token>`.

- [ ] **Step 1: Write the failing tests** (`telegramAPI` pointed at a loopback `httptest` server).
  - `TestAMessageIsTheSentenceAndTheLink`: with `link = "https://box:8787"`, a `session.attention` gives the text `refactor is waiting for your permission\nhttps://box:8787/terminal#<id>` on path `/bot<token>/sendMessage` with `chat_id` `123`. With `link` empty, or no session, there is no second line.
  - `TestALongMessageIsCut`: 5000 runes are cut to 4096.
  - `TestTelegramSaysWhenToTryAgain`: 429 with `retry_after: 3` gives `again` whose wait is 3 s. `retry_after: 9999` is capped at `maxAfter`. 401 gives `dropped` with `telegram answered 401: Unauthorized`.
  - `TestADialErrorDoesNotRepeatTheToken` (Review Focus 2): with `telegramAPI` a closed loopback port, `cleanErr(err, s.secrets())` does not contain the token.
  - `TestATokenFileOthersCanReadIsRefused` (unix).
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add cmd/rota/notify/telegram.go cmd/rota/notify/telegram_test.go && git commit -m "A Telegram sink sends each event as one plain message linking its session, with a bot token read from a file of its owner's and repeated nowhere"`

### Task 12: The command sink

**Files:**
- Modify: `cmd/rota/notify/command.go`, replacing the stub
- Create: `cmd/rota/notify/command_unix.go` (`Setpgid`, and a kill of `-pgid`) and `cmd/rota/notify/command_other.go` (`Process.Kill`)
- Modify: `internal/fakecli/fakecli.go`. `Spec` gains `StdinTo string`, which writes what was read on stdin to that path before exiting. Its doc line goes beside `Touch`.
- Test: `cmd/rota/notify/command_test.go`

**Interfaces:**
- Consumes the T9 contract and `fakecli.Install`.
- Produces `func newCommand(s api.EventSink, e env) (sender, error)` (`exec.LookPath(argv[0])` at build) and `var commandTimeout = 30 * time.Second`.
- One run (R22):
  - stdin is the JSON and `\n`
  - the environment is `os.Environ()` without `e.hide`
  - stdout is discarded, and stderr is kept to 4 KiB
  - `WaitDelay` is 2 s
  - exit 0 is nil, exit 75 is `again(…, 0)`, and anything else or the timeout is `dropped("command exited 3: <first stderr line>")`

- [ ] **Step 1: Write the failing tests.**
  - `TestACommandGetsTheEventOnStdin`: with a fake from `fakecli.Spec{StdinTo: f}`, `f` holds the event's JSON line.
  - `TestACommandGetsNoBearerToken`: `ROTA_TOKEN=x` and `MYTOK=y` are set, with `hide ["ROTA_TOKEN","MYTOK"]`. The fake is `Spec{Stderr: []string{"{{env:ROTA_TOKEN|none}} {{env:MYTOK|none}}"}, Exit: 3}`, and the dropped error ends `none none`.
  - `TestExitCodesDecideWhatHappens`: 0 gives nil, 75 gives `again`, and 3 gives `dropped` with the first stderr line.
  - `TestACommandThatNeverExitsIsKilled` (Review Focus 1): `commandTimeout = 200ms` with `Sleep 10s`. It is `dropped` within 3 s, and on unix no process of its group is left (`kill(-pgid, 0)` gives `ESRCH`).
  - `TestAMissingProgramFailsTheSinkNotTheDaemon`: `["rota-no-such-program"]` makes `newCommand` fail with `exec`'s message.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git add cmd/rota/notify internal/fakecli && git commit -m "A command sink hands each event to a program on its stdin, never in its arguments and never with the server's token, and kills it if it hangs"`

### Task 13: Events in the daemon, and in `rota serve status`

**Files:**
- Create: `cmd/rota/events_daemon.go`, `cmd/rota/events_daemon_test.go` and `cmd/rota/events_daemon_unix_test.go` (`//go:build linux || darwin`)
- Modify:
  - `cmd/rota/daemon_unix.go`, `runDaemon`:
    - after `listenCtl` succeeds, `ev := startEvents(cfg, terminals, log)`
    - after `srv.Stop()`, `if !ev.Wait(time.Now().Add(eventsDrain)) { log.Warn("events were still being sent as the rota daemon stopped") }`
  - `cmd/rota/daemon_unix.go`, `daemonStatus`: it fills `Sinks`
  - `cmd/rota/daemon.go`: `statusDoc` gains `Sinks []notify.Status \`json:"sinks,omitempty"\``, and `sayStatus` prints a line for each
  - `cmd/rota/daemon.go`, `serveStatus`: a `--test` flag
  - `cmd/rota/main.go`: after the foreground serve's `serveNotes` loop, `for _, note := range eventNotes(cfg) { say(note) }`
  - `docs/reference.md`: the `--test` row under `rota serve status`

**Interfaces:**
- Consumes `notify.*` (T9–T12), `client` (T5) and the route (T4).
- Produces:
  - `func startEvents(cfg *api.Config, terminals string, log *slog.Logger) *notify.Notifier`. It removes a stale `sinks.json` when no sink is configured. `HideEnv` is `ROTA_TOKEN` and `cfg.Auth.TokenEnv`. It logs `events: 2 sinks: telegram, webhook`.
  - `func eventNotes(cfg *api.Config) []string`, which is R2's note
  - `func statusSinks(terminals string, pid int) []notify.Status`, from `ReadStatus` when its `pid` matches
  - `func sinkLine(s notify.Status, now time.Time) string`: `events: telegram ok, 12 sent, last 3m ago`, `events: webhook failing since 10:14: webhook answered 502 Bad Gateway` or `events: command waiting`
  - `func (c *cli) testSinks(terminals, sink string) error`
  - `var eventsDrain = 10 * time.Second`
- **`--test`.**
  - With no daemon it gives the existing no-daemon exit 3.
  - With no sinks running it gives `no event sinks are running; [[events.sink]] in server.toml adds one` and exit 1.
  - It prints `<name>: delivered`, `<name>: failed: <error>` or `<name>: no answer within 20s` for each sink.
  - It exits 0 only when every sink delivered.
  - `--test` and `--json` print `[{"sink","answered","ok","error"}]`.

- [ ] **Step 1: Write the failing tests.**
  - `TestEventNotesSpeakOnlyOfSinks` (pure): no sinks gives `nil`, and one gives R2's line.
  - `TestSinkLines` (pure): the three shapes above.
  - `TestTheDaemonPushesItsEvents` (process-level, `ROTA_AS_RUN=1`, with the tty fake as claude, as in `daemon_unix_test.go`). `server.toml` has:
    - a webhook to a loopback `httptest` server with a 0600 `secret_file`
    - a command sink whose `fakecli` program has `StdinTo` and `on = ["session.*"]`

    `serve start`. The webhook gets `daemon.started` with a valid signature. `rota run 1 --detach --label e2e` makes both get `session.started` named `e2e`. `rota send e2e "bye"` puts `session.ended` with `exit 0` in the command's file.
  - `TestServeStatusShowsAndTestsSinks`: in the daemon above, `serve status` prints two `events:` lines and its `--json` has `sinks`. `serve status --test all` exits 0 with `webhook: delivered` and `command: delivered`. `--test nope` exits 1 naming the sinks.
  - `TestStopDeliversDaemonStopped` (Review Focus 5): `serve stop` returns only after the webhook got `daemon.stopped`. With the webhook answering after 1 s, stop still returns within `daemonStopWait`.
  - `TestABrokenSinkHurtsNothing` (Review Focus 1): a third sink, a webhook to a closed loopback port, is `failing` in `serve status`, while the other two deliver and the session runs.
  - `TestTheDaemonLogHoldsNoSinkSecret` (Review Focus 2): the webhook URL's path holds `XYZSECRET`. `daemon.log` and `sinks.json` contain neither `XYZSECRET` nor the HMAC key.
  - `TestAForegroundServeSaysItSendsNone`: `rota serve` with a sink in its file prints the note through the `serveUntil` seam, and makes no `sinks.json`.
  - `TestNoSinksMeansNothingNew`: a daemon with no `[events]` has no `sinks.json`, no `events:` lines in its log, and no `sinks` in `serve status --json`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, with the reference row: ``| `rota serve status --test SINK` | send a test event to that sink, or `all`, and say what each did |``.
- [ ] **Step 4: Verify.** **docker.** The local CI run passes, `TestReferenceDocumentsEveryFlag` included.
- [ ] **Step 5: Commit.** `git add cmd/rota docs/reference.md && git commit -m "The daemon pushes its events to the sinks server.toml names and lets them finish as it stops, and rota serve status shows each sink and can test it"`

### Task 14: The manual and the notes

**Files:**
- Modify: `docs/reference.md` and `docs/releases/1.4.0.md`

**Interfaces:**
- Consumes everything above.

- [ ] **Step 1: The section.** Write *Events*, after *Held sessions and the daemon*, in the manual's voice, from these facts:
  - *The event* and *The words*, as tables
  - the stream, with `since`, `Last-Event-ID`, `on`, `gap`, heartbeats and its end
  - what each type is computed from, and when (R6–R15), in a sentence each
  - `[events]` and each sink's keys, with the config block
  - delivery, its retries and its drops
  - the status, and `--test`
  - what is never sent or logged (R12, R20–R22, R29)
  - that Telegram is send-only, that answering from it is not in this version, and that a foreground serve sends none

  Add Layout rows for `internal/events` and `cmd/rota/notify`. Add `[events]` to the config block of *One file for the server*. Then read the whole manual once more, so that it reads as one text.
- [ ] **Step 2: The bullet.** Under `### New in the next beta` in `docs/releases/1.4.0.md` (create it above `### New in 1.4.0-beta.10` if it is missing), add one bullet: **Events, and sinks that push them.** It covers:
  - the types
  - `GET /v1/events`
  - `[[events.sink]]` for Telegram, webhook and command
  - the test event and `rota serve status --test`
  - that nothing is sent unless a sink is configured
  - that no token or chat content is ever in one

  `git diff b9fd476 -- docs/releases/` shows only additions under that heading.
- [ ] **Step 3: Verify.** The local CI run passes. Also run `git log --format=%B origin/main.. | grep -ciE 'co-authored|claude\.ai|anthropic|generated'`, which prints `0`.
- [ ] **Step 4: Commit and push.** `git add docs && git commit -m "The manual describes events, the stream and the sinks, and the notes for the next beta say what they are" && git push -u origin phase-5`

---

## Self-review

- **Spec coverage, section 11.**
  - The bus and its bounded ring are T1 and T4.
  - The account types are T6, the session types T7 and T8, and the viewer types T7.
  - `daemon.*` is T4. The JSON fields are T1.
  - "No token and no conversation content" is R12, R16, R29 and T4.
  - The latches are T6–T8. "Retry with backoff, drop, one bad sink affects nothing" is T9.
  - The three sinks are T10–T12. SSE for watch and control is T4. "argv never from event text" is T12.
  - Standard library only, in `cmd/rota/notify`, over `ctl.sock`, is T9 and T13. Stripping the Telegram token is T9 and T11. Send-only with the session's URL is T11.
  - The two sources of `session.attention` are T2 and T8. "A chat moves only when the owner confirms" is untouched: no event acts.
- **Spec coverage, section 13, sending and attention.** The reason words are T8. Quiet's ignore rules are R15 and T8. "No sink fires while the keyboard holder has typed recently" is R15 and T8. "`--label` or folder unless the owner opts in" is R12, T7 and T9.
- **Spec coverage, Risks.** "`--config` account on an env token … now raised as `account.dead`" is R10 and T7.
- **Spec coverage, Public surface.** "`[events]` … exported `api.Config` fields and `Print` is extended" is T3. Nothing else public is added: `Server` gains no exported method, and `StopRefreshing`'s documented behaviour grows (R26).
- **Left out on purpose.** Answering from Telegram (out of scope by the spec). An `account.threshold` action that moves a chat (phase 4's `m`). Web toasts (phase 6). The TUI's level-crossing bell (phase 3, which may read the stream).
