# Phase 3: the TUI shell, the theme, Accounts and Ask Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

The spec's "Delivery, in order" gives phase 3 the TUI shell, the theme, Accounts and Ask. Accounts includes the guided add-account flow on `n`, re-login of a dead account on `l`, and the one-time login page `rota login --web [addr]` (spec section 9). Sessions, moving a chat and Services are phase 4. `attach`, the chord and SSH are phase 2, which this plan consumes and does not re-plan.

**Goal:** Bare `rota`, with standard input and output both terminals, opens a Bubble Tea v2 TUI with the Accounts and Ask tabs. Accounts shows usage against each account's own threshold, reorders, edits and removes, runs the account's CLI on `⏎`, adds an account on `n` and signs a dead one in again on `l`, all without another terminal and the same over SSH. `rota login --web [addr]` serves a one-time page that takes the code, an API key or a setup token. Everything else rota does is unchanged.

**Architecture:** One package, `cmd/rota/tui`, in the Elm architecture: no I/O in `Update`, a `Backend` interface between the views and the world, and one implementation of it, over HTTP, against the api routes. That keeps `roleFor` the only enforcement. Locally with no daemon, the API is an in-process `api.Server` reached through phase 2's `client.InProcess`, so no port or socket opens and accounts, logins and Ask behave as `rota list`, `rota login` and `rota run` do in this process. Over SSH, the API is the daemon's `LocalHandler` for the key's user, so the same model runs there. The one-time page is `cmd/rota/loginpage`, standard library only, used by both `rota login --web` and the flow's `w`.

**Tech Stack:** Go 1.27. The program module gains `charm.land/bubbletea/v2` v2.1.1, `charm.land/lipgloss/v2` v2.0.6 and `charm.land/bubbles/v2` v2.2.1, and nothing else. The root module gains nothing. The page uses `crypto/tls`, `crypto/x509` and `net/http` only.

**Spec:** `docs/superpowers/specs/2026-11-01-tui-daemon-ssh-design.md`. Read sections 1, 6, 8, 9 and 13, "Delivery, in order", "Public surface", "The owner's answers" and "Calls made without asking". Both of the last two bind this plan.

**Base:** `origin/login-by-id` at `796dbaa` or later (lane L0). It provides `store.BeginLoginFor`, `rota.Login.Account`, `store.PendingTTL`, `store.Unfinishable` and the interactive terminal login in `cmd/rota/login_wait.go` (`afterBegin`, `await`, `finishOnce`, `finished`, `leftOpen`, `finishLater`, `timeLeft`, `expired`, `pollEvery`). This plan builds on those and redoes none of them.

## Global Constraints

- The root module `github.com/professor93/rota` gains no `require`, no `replace` and no `go.sum`. `lib`, `store` and `wire` gain no exported name in this phase. `api` grows only by what Task 3 lists. Third-party code goes into `cmd/rota/go.mod` only, through `sh scripts/module.sh tidy`, and only in Task 1. `sh scripts/module.sh check` prints `module: ok` after every task.
- Bare `rota` opens the TUI only when standard input and standard output are both terminals. Anywhere else (a script, a pipe, CI) it prints the short usage and exits 0 exactly as today, and `rota --json` with no command keeps printing usage. There is no `tui` verb.
- `--web [addr]` is a flag on `login`, not a verb. The page adds no route to `rota serve`.
- The page (spec, verbatim): a random secret of 256 bits in the path, any other path answers 404. It ends after one accepted login, after 10 minutes, or on Ctrl-C, whichever comes first, and the listener closes with it. Five wrong tries end the page. Nothing submitted is logged or echoed, and the response sets no cache. A non-loopback address gets HTTPS with a certificate made for this run, and rota prints its fingerprint. Plain HTTP on a non-loopback address is refused, with no flag to allow it. With no address, rota lists this machine's interfaces with `127.0.0.1` preselected, each entry says who can reach it, and `0.0.0.0` is offered last with that warning.
- Rules for the TUI (spec section 9): Elm architecture, with every effect a `tea.Cmd`. One package, one file per tab, and no non-test file past 600 lines. `key.Binding` is the only source of keys. One theme module: adaptive light and dark, truecolor, 256 and ANSI tiers, `NO_COLOR` honoured. Width ladders shed columns by priority and never reflow when a session starts. Below 60×14 the screen shows a clear message. Mouse is click and wheel, toggled with `M`. Goldens run at 60×20, 80×24 and 120×40 with a throwaway store.
- The watch role sees no add, re-login, edit, remove, reorder, threshold, exclude, run or Ask action. `roleFor` refuses them anyway.
- No token, key, setup token, refresh token or login `State` value ever reaches the screen, a log, argv, a toast or an error the TUI shows. Every label, reason and error drawn passes through `clean` (Task 7).
- `<testenv>` is `env -u ROTA_CLAUDE_HOME -u CLAUDE_CONFIG_DIR -u ROTA_ACCOUNT_ID -u ROTA_ACCOUNT -u ROTA_PROVIDER -u ROTA_TOKEN ROTA_HOME="$(mktemp -d)"`, and it prefixes every `go test`.
- **The local CI run** is these commands, in this order, from the repository root:
  - `test -z "$(gofmt -l .)"`
  - `for os in linux darwin windows; do GOOS=$os go vet work || echo "vet $os FAILED"; done`
  - `<testenv> go test -count=1 work`
  - `sh scripts/module.sh check`

  A task marked **race** also runs `<testenv> go test -count=1 -race work`. `go test ./...` at the root skips the program, so no step uses it. A timing test that fails is rerun alone with `-count=3` before anything is concluded.
- Tests never start a real claude, codex or grok, never touch `~/.rota`, `~/.claude`, `~/.codex` or the keychain, and never run the installed `rota`. Fake CLIs come from `internal/fakecli`, provider endpoints are `httptest` servers, and stores are throwaway. No TUI test needs a terminal or a pty (see Task 1's driver), so every one runs headless in CI on ubuntu, macos and windows through the existing `go -C cmd/rota test ./...` step.
- Linux first, then macOS. On Windows, bare `rota` keeps printing usage (Ruling R4). The `tui` and `loginpage` packages build, vet and test there.
- Natural by default: no existing command's output, exit code or JSON changes, except `POST /v1/login` with `account` and no `token`, which Task 3 makes do what it says.
- Branches: the plan is on `phase-3-plan`. Implementation is on `phase-3`, cut from `origin/login-by-id` with the plan commit cherry-picked. Each task is on `p3-t<N>`, cut from `phase-3` once its dependencies are merged there. The integrator merges reviewed tasks into `phase-3` group by group. No PR. Never force-push, tag, merge to main or bump the version.
- Commit subjects are one plain sentence saying what is now true. No body, no trailer, no attribution and nothing about AI.
- Docs: `docs/reference.md` must read as one text after each task. New user-visible behaviour gets one bullet under `### New in the next beta` in `docs/releases/1.4.0.md`. Only Tasks 3, 8 and 21 write bullets there, so parallel tasks never collide. Released sections stay byte-identical: check with `git diff b9fd476 -- docs/releases/`.

## Review Focus

1. **A label, a dead reason or an error holding control bytes, a newline, a key-shaped string, or wide characters.** Examples: an e-mail with `ESC[2J` from a hostile reply, a reason quoting a server, a CJK or emoji label. The text must show as visible characters in its own column, never be interpreted, never shift another column, and have a key-shaped part redacted. Tests: Task 7 Step 1 (`TestCleanDropsControlsAndKeys`, `TestFitCountsColumnsNotBytes`) and Task 9 Step 1 (`TestALabelNeverEscapesItsCell`).
2. **The terminal shrinking below 60×14, to 0×0, or resizing while the add flow is open.** The size message replaces the screen, nothing panics, and on growing back the flow, the code typed so far and the countdown are all intact. Tests: Task 6 Step 1 (`TestTooSmallSaysSoAndForgetsNothing`) and Task 15 Step 1 (`TestAResizeKeepsTheFlowAndItsField`).
3. **A paste into the code or key field**: a bracketed paste with a trailing newline, CRLF or spaces, a paste while the field is not focused, or a key pasted into a code field. The field takes it trimmed and focused, submits once, never draws the key, and redacts a key-shaped value from any error. Test: Task 15 Step 1 (`TestAPasteFillsTheFieldOnceAndShowsNoKey`).
4. **The store changing underneath.** Another terminal removes, signs in or reorders an account while the TUI shows it, or a PATCH answers 404 or 409. The selection follows the id, an open edit says the account is gone, and refusals arrive as toasts. Tests: Task 9 Step 1 (`TestSelectionFollowsTheAccountNotTheRow`) and Task 14 Step 1 (`TestAnEditOfAnAccountRemovedElsewhereSaysSo`).
5. **The world failing mid-operation**: the daemon stopping while the header says it runs, a provider answering 500 or hanging on a login finish, an Ask stream cut, or the in-process API timing out. The failure arrives as a toast or beside the field, the TUI keeps answering keys, the header flips to `○ no daemon`, and the login stays parked. Tests: Task 2 Step 1 (`TestACallThatHangsEndsAtItsDeadline`), Task 6 Step 1 (`TestABackendFailureIsAToastNotACrash`), Task 11 Step 1 (`TestACutStreamEndsTheAnswerAndSaysSo`) and Task 15 Step 1 (`TestAProviderFailureKeepsTheField`).

## Rulings

Each is this plan's own decision where the spec is silent, with why and what it costs if wrong.

| # | Ruling | Why | Cost if wrong |
|---|---|---|---|
| R1 | Locally the TUI talks to an in-process `api.New` with `Token` random, `RefreshEvery: -1` and `Routes{API, WebSocket, Health}`, through `client.InProcess(srv.LocalHandler(&api.Principal{Name:"local", Role: api.RoleControl, Via:"local"}))`. `ctl.sock` is used only for sessions and the header. | One Backend for local and SSH, with `roleFor` as the one table. No daemon is needed for accounts, no port or socket opens, and Ask's folder, claim and lifetime match `rota run`. | One in-process server per TUI. Quitting waits for a refresh in flight, at most `store.RefreshTimeout`. |
| R2 | `GET /v1/accounts?refresh=stale` reads what is stale, as `rota list` does. The TUI asks for it on open, after each login and every 2 min, reads the cache every 15 s, and forces with `r`. | Over SSH nothing else reads a new account's usage without forcing every account. One mechanism serves local and SSH. | One query value of public HTTP surface. |
| R3 | `POST /v1/login {"account":N}` begins a login bound to N (`store.BeginLoginFor`). Today it is silently unbound. | The `l` flow needs it over HTTP. A request naming an account and getting a login anyone can finish is a bug. | A client that sent `account` alone and wanted an unbound login now gets a bound one, which is what it asked for. |
| R4 | Bare `rota` opens the TUI on linux and darwin only. On Windows, and with `TERM=dumb`, it prints usage as today. | The owner's platform rule is to skip rather than port, and a dumb terminal cannot draw it. | Windows users wait for a one-line change. |
| R5 | Phase 3 shows only the tabs it builds, Accounts and Ask. Tabs register at places 10, 20, 30 and 40, and phase 4 adds Sessions at 20 and Services at 40. | No placeholder screens, and no shared file to edit when phase 4 adds tabs. | The header shows two tabs until phase 4. |
| R6 | Tabs switch with `1`–`4` and `[` `]` whenever no text field has focus. `esc` leaves a field. | Ask's prompt takes digits. Tab and shift-tab move focus inside a form. | Muscle memory for tab-to-switch. |
| R7 | Mouse is off until `M`, and the choice is not saved. | Mouse capture blocks selecting the login URL with the mouse. | A click does nothing until `M`. |
| R8 | Every non-scoped window's bar carries the threshold tick, except at threshold 100. Colour is the percent over the threshold: below 0.8 good, below 1 warn, at 1 or more over. Under `Ascii` the state column says `over its N%`. | The rotation judges the highest non-scoped window against the threshold, and colour must not be the only signal. | A tick where the design picture shows none on 7d. |
| R9 | The reset cell is the window that decides usage: the highest non-scoped window, or the latest reset among those at or over the threshold. It reads `1h 3m · 21:20`, or `2d 1h · Thu 21:20` from 24 h on, in local time. | That reset is when the account is back. A clock time alone is ambiguous past a day. | A different window's reset shown. |
| R10 | A reading is stale when `checkedAt` is empty or older than 10 min, or when `usage_wait_until` is set. A stale reading is drawn Faint. | 10 min is five daemon sweeps. A provider asking rota to wait means the number is old. | Fading too early or too late. |
| R11 | The bell rings once, via `bellMsg` → `tea.Raw("\a")` with a toast, when an account crosses its threshold upward on a later listing. It re-arms when the account falls back below. | This is clauth's latch as the spec takes it. The first listing is not a crossing. | One bell per crossing. |
| R12 | The Accounts ladder is the exact step list in Task 7. | It is testable at 60, 80 and 120 columns and depends only on width. | Different columns at some widths. |
| R13 | `n` long is two approvals: the ordinary login makes the account, then `BeginLongLogin` adds the year-long token. `l` offers short (bound), long (browser approval, unbound, then checked) and a pasted `claude setup-token` token (bound by id). A long token that lands on another account is said. | `store.attachLong` makes no account and the design forbids new store API. `rota login --long` behaves the same today. | The person approves twice for `n` long. |
| R14 | The API key option shows for `rota.DefaultProvider` only. `l` on a key account asks for the key again. | Only claude takes pasted keys (`lib/claudekey.go`), and `paster` is unexported. | A later provider with keys needs one line. |
| R15 | A delegated login hands the terminal to `rota login <id>` via `tea.Exec` locally. Over SSH the step shows `on <host>: rota login <id>` with `c` to copy it. | The vendor CLI needs a real terminal on that machine, and an SSH channel has no local pty. | Over SSH it takes a shell on the host. Today only grok's alternative and the hidden kimi are delegated. |
| R16 | The code step starts with its field blurred, so `o`, `c` and `w` work. `tab` or `enter` focuses the field, and any paste focuses and fills it. | Typing `o` must not open a browser while a code is being typed. | One extra key for someone who types the code by hand. |
| R17 | One login flow at a time. `n` or `l` while one is set aside brings it back, and `x` abandons it in the TUI while the parked login runs out by itself. | A second flow would make the footer badge ambiguous. | Two logins need two minutes. |
| R18 | On quit, a parked login is printed with login-by-id's `leftOpen` wording: `The login stays open for 12m: rota login <id> <code>`. | Nothing is lost if the TUI closes (spec). | None. |
| R19 | `o` runs `open` on darwin, or `xdg-open` on linux when `DISPLAY` or `WAYLAND_DISPLAY` is set, with the URL as one argument. Otherwise, and over SSH, `o` is hidden. | The spec says "when a browser is on this machine". The URL holds a PKCE challenge and no secret. | No `o` on a headless linux box with a browser started by other means. |
| R20 | `c` copies through `clipboardMsg` → `tea.SetClipboard` (OSC 52), the page URL included. | OSC 52 works over SSH (spec). | A terminal that ignores OSC 52 copies nothing. The toast says the copy was sent, not that it landed. |
| R21 | A device step shows the URL's `user_code` query value in large text when there is one. It polls every `State["interval"]` seconds, else 5 s, as `pollEvery` does. | That matches the CLI. No shipped provider uses device today. | None. |
| R22 | Page details: the link is `/<43-char base64url secret>`, the form POSTs to the same path, and the value travels only in the form body (limit 64 KiB). The page ends at the earlier of 10 min and the login's own end. TLS 1.3 with an ECDSA P-256 certificate whose SANs are the chosen IPs, valid until the page ends plus 1 min. The fingerprint is `SHA-256` of the DER, colon hex. A host name is refused, and `localhost` means `127.0.0.1`. The default port is 8788, else the kernel's. Link-local and multicast addresses are not offered. | This reads "the secret goes in a POST body and never in a URL" as the submitted credential (see O3). A fixed port lets `ssh -L` be set up first. | See O2 and O3. |
| R23 | After `--web`, the next word is the address only if it contains `:`, parses as an IP, or is `localhost`. A bare number is an account id. `--web=ADDR` also works. `--web` with `--json`, or with a code, is a usage error. | `rota login --web 12` must stay account 12. | `--web 8788` must be written `--web :8788`. |
| R24 | Off a terminal, `--web` with no address takes `127.0.0.1` and says so. | It cannot ask, and loopback is the safe default. | None. |
| R25 | Ask sends the chosen account's `cwd` if it has one, else `Options.Cwd`. Locally `Options.Cwd` is empty, so the in-process server's own folder applies as for `rota run`. Over SSH it is the daemon user's home. Automatic choice uses `Listing.Default`. | The daemon runs in `/`. | In a race with the rotation, a run starts in the home rather than another account's folder. |
| R26 | Ask: `enter` sends, `alt+enter` (and `shift+enter` where reported) inserts a newline, `esc` stops an answer, `ctrl+s` keeps the run open (`--input`), `ctrl+x` closes it, and quitting closes it. | Natural by default: nothing keeps running unasked. | None. |
| R27 | Readings asked for with `--with` toggles show as indented JSON under the answer. | `rota run` says readings that exist only as JSON imply `--json`. | Plain rather than pretty readings. |
| R28 | `⏎` runs `rota run <id>` through `tea.Exec` when no daemon runs locally, and opens a held session and attaches when one runs or over SSH (Task 19). A non-zero exit toasts the last stderr line from a 4 KiB tail. | Spec: "into a held session or, without one, directly". The alternate screen hides what the CLI printed. | None. |
| R29 | Every Backend call carries a deadline: 30 s, login finish 60 s, Ask none but `esc`. | A hung daemon must not hang the TUI. | A legitimately slow finish is cut at 60 s and retried. |
| R30 | The quit window is 2 s. Toasts last 4 s, three at most. The header polls `Options.Daemon` every 5 s. | clauth's two-step quit and toasts. | None. |
| R31 | Every terminal effect (`tea.Exec`, clipboard, bell) goes through the shell as a package message (`handOffMsg`, `clipboardMsg`, `bellMsg`). | Headless tests can see and run them. Bubble Tea's own effect messages are unexported. | None. |
| R32 | Over SSH the colour profile comes from `TERM` and `COLORTERM` (`colorprofile.Env`), because the channel is not a terminal. | `Detect` would say `NoTTY` and strip everything. | A wrong tier on an odd `TERM`. |
| R33 | Bars use `▕█░│▏` and need a UTF-8 terminal. | Claude Code's own UI needs one too. | Garbled bars on a non-UTF-8 locale. |
| R34 | Phase 4 adds its session and service calls as a second interface (`SessionBackend`) in `Options`, not by editing `Backend`. | Phase 4's tasks then share no file with phase 3's. | None. |
| R35 | `Edit`: `enter` saves one PATCH of the changed fields. `Clear` sends `cwd ""`, `config_dir ""`, `sessions "shared"` and `remote_control false` after a confirm. | `rota set` does one write, and `--clear` means exactly those four. | None. |
| R36 | `+` and `-` step 5, clamped to 1..100, with one PATCH in flight per account and later presses coalesced. `%` types a value. `x` on an excluded account puts it back last. `u` puts it first. | A held key must not send 30 PATCHes. | None. |

## For the owner

These are security-sensitive or irreversible. The plan proceeds with the recommendation unless the owner picks another.

**O1. Task 1 must download Bubble Tea, Lip Gloss, Bubbles and their modules, and the wave's rule forbids proxy.golang.org and sum.golang.org.** Only the `.mod` and `.info` files are in the local cache. The added `require`s are permanent in every later `cmd/rota/v1.4.0-beta.N`, which the design approved.
1. **Recommended.** Allow Task 1's `sh scripts/module.sh tidy` to use the default proxy and checksum database, with `GOPRIVATE=github.com/professor93/*`, so rota's own unpushed versions never reach either. That rule exists because of rota's own versions, which this keeps away.
2. The owner runs that one tidy himself and pushes `go.mod` and `go.sum` on a branch Task 1 builds on.
3. `GOPROXY=direct` with `sum.golang.org` still checking. This also queries the checksum database.
4. `GOPROXY=direct GOSUMDB=off`. No checksum database check for about 20 new modules. Not recommended.
5. Build a `file://` proxy from git clones at the tagged commits and compare hashes by hand.
6. Drop Bubbles, so that `textinput`, `textarea`, `viewport` and `help` are written by hand. That needs fewer modules, but still Bubble Tea and Lip Gloss.

**O2. Over SSH, `w` opens the one-time page on the daemon's machine.** A control user could choose any interface there, `0.0.0.0` included. The page is HTTPS with a 256-bit secret and lasts at most 10 minutes.
1. **Recommended.** Same as the local TUI: any interface for control users, with the warnings.
2. Loopback only over SSH.
3. Any interface but `0.0.0.0` over SSH.
4. No `w` over SSH. The flow says `rota login --web` is run on the host.
5. Allowed over SSH only with a new `[ssh] login_page = true` in `server.toml`.

**O3. Requests to wrong paths on the page, and where the secret travels.** The spec says both "the link carries a random secret … in the path" and "the secret goes in a POST body and never in a URL". The plan reads the second as the submitted credential.
1. **Recommended.** Wrong paths answer 404 and are not counted. Five rejected submissions end the page. The value goes only in the POST body, to the secret path.
2. Wrong paths count toward the five. Any scanner on `0.0.0.0` could then end the page.
3. Wrong paths count separately, and 1000 of them end the page.
4. Wrong paths answer 404 and are rate-limited per client address.
5. The form POSTs to `/` with the secret as a body field, so only the first GET's URL carries it.
6. The link carries the secret in a fragment, and a script posts it. That needs JavaScript and departs from "in the path".

## What phase 3 needs from phase 2, and what it gives to phase 4

Phase 2 (plan-p2, `docs/superpowers/plans/2026-11-10-phase-2-attach-ssh.md` on `phase-2-plan`) owns these seams, with names agreed on 2026-11-10:
- **Phase 2's first group.** `func InProcess(h http.Handler) (c *Client, stop func())` in `cmd/rota/client/inproc.go`, and `func (c *Client) HTTP() *http.Client`. Tasks 12 and 20 need them. If phase 2's first group is not merged when Task 12 starts, Task 12 waits. The signature is fixed, so nobody builds a second copy.
- **Attach.** `func Command(c *client.Client, id string, o Options) *Cmd` in `cmd/rota/attach`. `*Cmd` has `Run`, `SetStdin`, `SetStdout` and `SetStderr` (Bubble Tea's `ExecCommand`), plus `SetWindow(size func() (cols, rows int), resized <-chan struct{})` and `Result() Result`. `Options{Key byte; Watch, Settle, Teach bool}`, `Result{Why; Session client.Session; Code int; Err error}`, and `func DaemonKey(terminals string) byte`. Task 19 needs them.
- **SSH.** `sshd.Options.TUI func(ctx context.Context, s sshd.TUISession) error`, with `TUISession{In io.Reader; Out io.Writer; Env []string; Cols, Rows int; Resize <-chan [2]int; Client *client.Client; Key byte}`, set where `cmd/rota` builds `sshd.Options`. Task 20 needs it.
- Phase 1's `client.Create`, `client.List`, `holdwire.RunningDaemon` and `holdwire.Scan` are already on main.

Phase 3 gives phase 4:
- The tab registry `register(place int, make func(e *env) tab)`, and the `tab`, `overlay`, `asider` and `step` interfaces (Task 6).
- `env`: `Backend`, `Options`, `Theme`, the latest `Listing`, `Daemon` and `Principal`, plus `now`, `tick`, `gate` and `call`.
- `Options.Attach` (Task 19), and `handOff` with `execTail` (Task 6), for Sessions' `⏎` and `w`.
- `newConfirm` (Task 14) for Sessions' `s` stop.
- `bar`, `level`, `resetText`, `deciding`, `stale`, `clean`, `fit` and `wrap`, with `accountsLayout` as the pattern for a ladder (Task 7), and the driver and golden helpers (Task 1).
- `tui.Run` with its SSH wiring (Task 20).
- Phase 4 adds `SessionBackend` beside `Backend` (R34), and `PATCH /v1/terminals/{id}` grows there.

## File Structure

| Path | Responsibility |
|---|---|
| `cmd/rota/tui/doc.go`, `theme.go`, `keys.go` | Package doc. The theme: tokens, tiers and `NO_COLOR`. The global key bindings |
| `cmd/rota/tui/backend.go`, `backend_http.go` | The `Backend` interface, its types, and its one implementation over the HTTP API |
| `cmd/rota/tui/options.go`, `env.go`, `tabs.go`, `overlay.go`, `effects.go`, `model.go`, `program.go` | The shell: options, shared state, tab registry, overlays, terminal effects, root model, `Run` |
| `cmd/rota/tui/chrome.go` | Header, footer, toasts, help, two-step quit, mouse toggle |
| `cmd/rota/tui/bars.go`, `ladder.go`, `text.go` | Usage bars, reset text, staleness. The width ladder. Cleaning and fitting text |
| `cmd/rota/tui/accounts.go`, `accounts_view.go` | The Accounts tab: listing, selection, keys and dispatch. Table and details |
| `cmd/rota/tui/accounts_act.go` | Reorder, first, exclude, threshold, refresh and the bell |
| `cmd/rota/tui/edit.go` | Edit every `rota set` flag. Remove. A confirm overlay |
| `cmd/rota/tui/login.go`, `login_steps.go` | The add and re-login flow. Its code and secret steps |
| `cmd/rota/tui/login_wait.go` | Device and delegated steps |
| `cmd/rota/tui/login_page.go` | `w`: the one-time page from the flow |
| `cmd/rota/tui/launch.go` | `⏎`: the account's CLI here, or a held session |
| `cmd/rota/tui/ask.go`, `ask_view.go` | The Ask tab |
| `cmd/rota/tui/testdata/golden/` | Plain-text goldens, and four styled ones |
| `cmd/rota/loginpage/page.go`, `cert.go`, `page.html`, `addrs.go` | The one-time page and where it may listen, standard library only |
| `cmd/rota/login_web.go` | `rota login --web [addr]` |
| `cmd/rota/tui_entry.go`, `tui_ssh.go` | Bare `rota` opens the TUI locally. SSH opens it |
| `api/server.go` | `refresh=stale`, `POST /v1/login {"account":N}`, and the `unfinishable` reason |
| `.gitattributes`, `README.md`, `docs/reference.md`, `docs/releases/1.4.0.md` | Goldens kept byte for byte. The dependency sentence, the manual and the notes |

## Parallel groups

Tasks in one group share no file and run at the same time. A group starts when every task it depends on is merged into `phase-3`.

| Group | Tasks (depends on) | Starts after |
|---|---|---|
| A | 1 (none), 2 (none), 3 (none), 4 (none), 5 (none) | the base |
| B | 6 (1, 2), 7 (1), 8 (4, 5) | A |
| C | 9 (6, 7, 4), 10 (6, 7), 11 (6, 7, 2), 12 (6, 2, phase 2's first group) | B |
| D | 13 (9), 14 (9), 15 (9, 3), 16 (9), 17 (9, 4, 5), 18 (9) | C |
| E | 19 (18, 12, phase 2's attach), then 20 (19, phase 2's sshd) | D and phase 2 |
| F | 21 (all) | E |

Task 9 creates one stub file each for Tasks 13 to 18, with the signatures in its Interfaces block. Each of those tasks replaces exactly its own stub, so group D shares no file. Without phase 2, groups A to D and Task 21's local half still finish. `⏎` then runs directly, and SSH prints phase 2's command list.

---

### Task 1: Dependencies, the theme and the headless driver

**Files:**
- Create: `cmd/rota/tui/doc.go`, `cmd/rota/tui/theme.go`, `cmd/rota/tui/keys.go`, `cmd/rota/tui/theme_test.go`, `cmd/rota/tui/harness_test.go`
- Modify: `cmd/rota/go.mod` and `cmd/rota/go.sum` (through `tidy`), `.gitattributes` (add `cmd/rota/tui/testdata/** -text`), `README.md:11`, `docs/reference.md:44-47` (the dependency sentence)

**Interfaces:**
- Produces:
  - `type Theme struct { Dark bool; Profile colorprofile.Profile; Text, Muted, Faint, Accent, Good, Warn, Over, Dead, Key, Selected, Rule, Title lipgloss.Style }`.
  - `func NewTheme(p colorprofile.Profile, dark bool) Theme`. Each token has a truecolor, a 256 and a 16-colour value for light and for dark, picked through `lipgloss.Complete(p)`. Under `Ascii`, no colour is set. `Selected` is Reverse, `Over` is Bold and `Faint` stays Faint.
  - `func Profile(out io.Writer, env []string, remote bool) colorprofile.Profile`. It is `colorprofile.Detect`, or `colorprofile.Env` when `remote` (R32). `NO_COLOR` gives `Ascii` either way.
  - `type globalKeys struct { Quit, Help, Mouse, Next, Prev, Jump key.Binding }` and `func newGlobalKeys() globalKeys`. The keys are `q` and `ctrl+c`, `?`, `M`, `]`, `[`, and `1`–`4`.
  - Test helpers in `harness_test.go`:
    - `func run(m tea.Model, msgs ...tea.Msg) (tea.Model, []tea.Msg)` sends each message to `Update` and runs every returned command inline, expanding `tea.BatchMsg` and feeding results back until quiet. It returns the effect messages it saw (`handOffMsg`, `clipboardMsg`, `bellMsg` once Task 6 defines them; any message type the model ignores).
    - `func plain(m tea.Model) string`, which is `ansi.Strip` of the view's content.
    - `func golden(t *testing.T, name, got string)` compares with `testdata/golden/<name>.txt` and rewrites it when `ROTA_UPDATE_GOLDEN=1`.
    - `func press(keys ...string) []tea.Msg` turns names such as `"q"`, `"shift+up"`, `"enter"`, `"alt+enter"` and `"ctrl+s"` into `tea.KeyPressMsg`.
    - `func size(w, h int) tea.WindowSizeMsg`.
    - `type fakeTicks`, with `func (f *fakeTicks) tick(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd` and `func (f *fakeTicks) advance(d time.Duration) []tea.Msg`. Timers fire only when a test advances.

- [ ] **Step 1: Confirm the names this plan uses.** Read the v2.1.1, v2.0.6 and v2.2.1 sources in the module cache after Step 4. The names are:
  - Bubble Tea: `tea.NewProgram`, `WithInput`, `WithOutput`, `WithEnvironment`, `WithColorProfile`, `WithWindowSize`, `WithContext`, `WithoutSignalHandler`, and `tea.View` with `AltScreen` and `MouseMode`.
  - Bubble Tea messages: `MouseModeCellMotion`, `KeyPressMsg`, `PasteMsg`, `WindowSizeMsg`, `BackgroundColorMsg.IsDark`, `RequestBackgroundColor` and `BatchMsg`.
  - Bubble Tea effects: `Exec`, `ExecCommand`, `SetClipboard`, `Raw`, `Quit` and `Tick`.
  - Lip Gloss and colour: `lipgloss.Complete`, `lipgloss.Width`, `colorprofile.Detect`, `Env`, `Writer`, and the profiles `TrueColor`, `ANSI256`, `ANSI`, `Ascii` and `NoTTY`.
  - x/ansi: `ansi.Strip`, `StringWidth` and `Truncate`.
  - Bubbles: `key`, `help`, `textinput` (`EchoPassword`), `textarea` and `viewport`.

  Any that differs goes into the task report with its real name. Every later task uses the report's name.
- [ ] **Step 2: Write the failing tests.**
  - `TestEveryTierRendersItsOwnColours`: `NewTheme(p, true).Good.Render("x")`, written through `colorprofile.Writer{Profile: p}`, contains:
    - `38;2;` for `TrueColor`
    - `38;5;` for `ANSI256`
    - `\x1b[3Nm` or `\x1b[9Nm` for `ANSI`
    - neither `38;` nor `48;` for `Ascii`, where `Selected.Render("x")` contains `\x1b[7m`
  - `TestNoColorIsHonoured`: `Profile(&buf, {"TERM=xterm-256color","COLORTERM=truecolor","NO_COLOR=1"}, true) == colorprofile.Ascii`, and without `NO_COLOR` it is `TrueColor`.
  - `TestLightAndDarkDiffer`: at `TrueColor`, every colour token's foreground differs between `dark` true and false.
  - `TestTheDriverRunsCommandsInline`: a toy model whose `Init` batches two commands and a 1 s tick. `run` delivers both messages, and the tick message arrives only after `advance(time.Second)`.
- [ ] **Step 3: Run them.** `go test ./cmd/rota/tui/` fails: the package does not exist.
- [ ] **Step 4: Implement, and tidy** (needs O1). Run `sh scripts/module.sh tidy` with the three modules imported by `theme.go`, `keys.go` and the tests. `git diff go.mod` at the root prints nothing, and `go list -m all` at the root prints only `github.com/professor93/rota`.
- [ ] **Step 5: The emulator gate.** MVS raises `ultraviolet` and `x/ansi` under `x/vt` v0.1.0. `<testenv> go test -count=1 ./cmd/rota/hold/` must pass unchanged: `TestXVTMatchesXterm`, `TestModesMatchXterm` and `TestRepaintReproducesTheScreen`. If `x/vt` no longer builds or a golden differs, stop and report the diff. The owner then chooses between pinning an older Bubble Tea and redoing phase 1's spike on a newer `x/vt`.
- [ ] **Step 6: Docs.** The dependency sentence becomes `the `rota` program uses Bubble Tea, Lip Gloss and Bubbles for its TUI, and github.com/charmbracelet/x/vt to keep the screen of each held session.` If phase 2 has already added x/crypto/ssh, keep it in the same sentence.
- [ ] **Step 7: Verify.** The local CI run passes.
- [ ] **Step 8: Commit.**
  ```bash
  git add cmd/rota/tui cmd/rota/go.mod cmd/rota/go.sum .gitattributes README.md docs/reference.md
  git commit -m "The program takes Bubble Tea, Lip Gloss and Bubbles, and its TUI has one theme for light and dark terminals at every colour depth, NO_COLOR included"
  ```

### Task 2: The Backend, over HTTP

**Files:**
- Create: `cmd/rota/tui/backend.go`, `cmd/rota/tui/backend_http.go`, `cmd/rota/tui/main_test.go`, `cmd/rota/tui/backend_http_test.go`
- Test: `cmd/rota/tui/backend_http_test.go`

`main_test.go`'s `TestMain` isolates the environment as `api/api_test.go`'s does. It calls `fakecli.Maybe()`, points every provider endpoint at `claudecode.Unreachable`, sets `HOME`, `USERPROFILE` and `CLAUDE_CONFIG_DIR` to temp directories, and unsets `ROTA_CLAUDE_HOME`, `ROTA_ACCOUNT_ID`, `ANTHROPIC_CONFIG_DIR` and `XDG_CONFIG_HOME`. No third-party import is in these files, so the task runs beside Task 1.

**Interfaces:**
- Produces, in `backend.go`:
  ```go
  type Backend interface {
      Who(ctx context.Context) (Principal, error)                                  // GET /v1/session
      Accounts(ctx context.Context, how Refresh) (Listing, error)                  // GET /v1/accounts[?refresh=stale|1]
      Change(ctx context.Context, id int, c Change) (wire.Account, error)          // PATCH /v1/accounts/{id}
      Remove(ctx context.Context, id int) error                                    // DELETE /v1/accounts/{id}
      BeginLogin(ctx context.Context, b LoginBegin) (*rota.Login, error)           // POST /v1/login
      FinishLogin(ctx context.Context, id, code string) (LoginEnd, error)          // POST /v1/login/{id}
      PasteKey(ctx context.Context, key, label string) (LoginEnd, error)           // POST /v1/login {"key","label"}
      PasteToken(ctx context.Context, account int, token string) (LoginEnd, error) // POST /v1/login {"account","token"}
      Ask(ctx context.Context, q Question, each func(message.Event)) (wire.End, error) // POST /v1/run or /v1/accounts/{id}/run
      SendRun(ctx context.Context, run, text string) error                         // POST /v1/runs/{id}/messages
      InterruptRun(ctx context.Context, run string) error                          // POST /v1/runs/{id}/interrupt
      CloseRun(ctx context.Context, run string) error                              // POST /v1/runs/{id}/close
  }
  type Principal struct{ Name, Role, Via string }
  type Refresh int // RefreshNone, RefreshStale, RefreshForce
  type Listing struct { Accounts []wire.Account; Default int }
  type Change struct { Order *string; Threshold *int; Cwd, ConfigDir, Sessions *string; RemoteControl *bool; ForgetLong bool }
  type LoginBegin struct { Provider string; Long bool; Account int }
  type LoginEnd struct { Pending bool; Account int; Provider, Email, Status string; Delegated bool; LoginCommand, LongUntil, Warning string }
  type Question struct { Account int; Prompt, Cwd string; With []string; Input bool }
  ```
  - `func NewHTTP(hc *http.Client, base string) Backend`.
  - A non-2xx answer is a `*client.Refusal{Status, Reason, Message}` whose `Message` has passed `wire.Unsecret`.
  - `{"status":"pending"}` is `LoginEnd{Pending: true}` with no error.
  - `Ask` posts `{"prompt", "cwd", "with", "input", "stream": true}` with `Accept: application/x-ndjson`. Each `message.Event` line goes to `each`. The line whose `type` is `done` or `error` is decoded as `wire.End` and returned.
  - A non-stream answer is read up to 8 MiB.
- Consumes `client.Refusal` from `cmd/rota/client` (phase 1).

- [ ] **Step 1: Write the failing tests.** Each runs against a real `api.New(api.Options{Dir: <throwaway>, Token: "t"})` served by `httptest.NewServer(srv.LocalHandler(&api.Principal{Name:"local", Role: api.RoleControl, Via:"local"}))`. The store is seeded as `newHarness` seeds one. A recording wrapper around the handler keeps each request body.
  - `TestWhoSaysTheRole`: control gives `Principal{Name:"local", Role:"control", Via:"local"}`, and a watch principal gives `Role:"watch"`.
  - `TestAccountsReadTheListing`: two seeded accounts come back in rotation order with `Default` set. `RefreshForce` sends `?refresh=1`, and `RefreshStale` sends `?refresh=stale`, checked on the recorder only, since Task 3 implements it.
  - `TestChangeSendsOnlyWhatChanged`: `Change{Threshold: ptr(80)}` sends exactly `{"threshold":80}`, and the answer's `Threshold` is 80. `ForgetLong` sends `"long":"forget"`.
  - `TestRefusalsKeepStatusAndReason`: removing account 99 gives `*client.Refusal{Status: 404}`. A refusal whose message holds `sk-ant-api03-…` comes back redacted.
  - `TestBeginAndFinishALogin`: with a fake claude token endpoint (the `approvalsByCode` shape), a wrong code is a 400 `Refusal` and the right one is `LoginEnd{Account: 3, Status: "added"}`. A device test provider registered as `t-tui-device` gives `Pending: true` twice, then `added`.
  - `TestBeginForAnAccountSendsTheAccount`: the recorder holds `{"account":12}`.
  - `TestPastedSecretsNeverComeBack`: `PasteKey("sk-ant-api03-…", "")` succeeds against a fake console endpoint, and neither the `LoginEnd` nor any error text contains the key. The same holds for `PasteToken`.
  - `TestAskStreamsEvents`: the fake claude comes from `fakecli.Lines` with stream-json `init`, `assistant` text `four` and `result`. `each` sees `text "four"`, and `End{ExitCode: 0, Account: 1}` comes back.
  - `TestAnOpenAskTakesMoreMessages`: a fakecli `Echo` claude is asked with `Input: true`. `SendRun(runID, "again")` is answered in the same stream, and `CloseRun` ends it with `done`.
  - `TestACallThatHangsEndsAtItsDeadline` (Review Focus 5): a handler that never answers ends `Accounts` with a context-deadline error when its context ends after 50 ms.
- [ ] **Step 2: Run them.** They fail: `NewHTTP` does not exist.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/backend.go cmd/rota/tui/backend_http.go cmd/rota/tui/main_test.go cmd/rota/tui/backend_http_test.go
  git commit -m "The TUI reaches accounts, logins and runs through one Backend over rota's own HTTP API, so the role table stays the only judge of what it may do"
  ```

### Task 3: What the API needs for the TUI

**Files:**
- Modify: `api/server.go` (`listAccounts`, `loginBegin`, `loginFinish`) and `docs/reference.md` (the `GET /v1/accounts`, `POST /v1/login` and `POST /v1/login/{id}` rows), plus one bullet in `docs/releases/1.4.0.md`
- Create: `api/login_tui_test.go`

**Interfaces:**
- Produces:
  - `GET /v1/accounts?refresh=stale` runs `st.Refresh(ctx, false, accounts...)` under a run slot, as `?refresh=1` takes one, with no cooldown.
  - `POST /v1/login {"account":N}` with no `token` calls `st.BeginLoginFor(ctx, N)` and answers the login (`account` set).
    - With `"long":true` it is 400 `long asks for a year-long token, which goes on whichever account approves it, so it names no account`.
    - With a `provider` other than the account's it is 400 `account N is a <p> account`.
    - On a delegated account it is 409 `{"error":"account N signs in through its own CLI: rota login N runs it","reason":"delegated"}`.
    - An unknown account is 404.
  - `POST /v1/login/{id}`: a refusal for which `store.Unfinishable(err)` holds keeps its status (400) and adds `"reason":"unfinishable"`.
- Consumes `store.BeginLoginFor` and `store.Unfinishable` from the base.

- [ ] **Step 1: Write the failing tests** (with `newHarness`).
  - `TestALoginForAnAccountOverHTTPIsBoundToIt`: `POST /v1/login {"account":1}` gives `account: 1`. A stranger's approval is refused and leaves `accounts.json` unchanged, and account 1's approval finishes it.
  - `TestALoginForAnAccountTakesNoLongAndNoOtherProvider`: the two 400s above.
  - `TestADelegatedAccountIsSentToItsCLI`: a delegated account gives 409 with `reason: delegated`.
  - `TestAnUnfinishableRefusalSaysSo`: with the account removed after the begin, the finish gives 400 `reason: unfinishable`, and the login stays parked.
  - `TestStaleRefreshReadsOnlyWhatIsStale`: a fake usage endpoint counts calls. An account read a minute ago is not asked. One never read is asked once, and asked again by a second `refresh=stale` only once it is stale. `refresh=1` still applies its per-account cooldown.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, and write the three reference rows in the table's voice. The notes bullet: `POST /v1/login {"account":N}` begins a login only that account's approval finishes, a refusal no approval can put right says `"reason":"unfinishable"`, and `GET /v1/accounts?refresh=stale` reads what is stale as `rota list` does.
- [ ] **Step 4: Verify.** The local CI run passes, `TestReferenceDocumentsEveryRoute` and the `roleFor` test included.
- [ ] **Step 5: Commit.**
  ```bash
  git add api docs/reference.md docs/releases/1.4.0.md
  git commit -m "Over HTTP a login can be begun for one account, an unfinishable refusal says so, and the listing can read only what is stale"
  ```

### Task 4: The one-time login page

**Files:**
- Create: `cmd/rota/loginpage/page.go`, `cmd/rota/loginpage/cert.go`, `cmd/rota/loginpage/page.html` (embedded), `cmd/rota/loginpage/page_test.go`

**Interfaces:**
- Produces, in package `loginpage`, standard library only:
  ```go
  type Want string // Code = "code", Key = "key", Token = "token"
  type Options struct {
      Addr     string    // "host:port", "host" (port DefaultPort), or ":port" (127.0.0.1)
      Want     Want
      Provider, Approve, LoginURL string // what the page says: whose login, who to approve as, the link (empty for Key and Token)
      Ends     time.Time // the login's own end; the page ends at the earlier of this and Lifetime
      Submit   func(ctx context.Context, value string) (done string, err error)
      Now      func() time.Time
  }
  type Page struct { URL, Fingerprint string; Done <-chan End; Close func() }
  type End struct { Why Why; Message string }
  type Why int // Accepted, Tries, TimedOut, Stopped, Refused
  func Serve(ctx context.Context, o Options) (*Page, error)
  func Final(err error) error // a Submit error that ends the page (Why Refused) instead of counting a try
  const (DefaultPort = 8788; Tries = 5)
  var Lifetime = 10 * time.Minute
  ```
- The decisions (R22, O3):
  - The secret is 32 bytes from `crypto/rand` in base64url with no padding. `GET` and `HEAD` on `/<secret>` serve the page. `POST` there takes the form field `value` from the body only, up to 64 KiB. Anything else on that path is 405, and every other path is 404, uncounted.
  - Every answer sets `Cache-Control: no-store`, `Pragma: no-cache`, `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, and `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`.
  - The page needs no JavaScript. It shows the provider, who to approve as, the login link with `rel=noreferrer`, one field (`type=password` for Key and Token, `autocomplete=off`) and the time it ends.
  - A rejected value counts a try and shows `Submit`'s error, never the value. The fifth ends the page.
  - `Final` ends it at once. An accepted value shows `done` and ends it once that answer is written.
  - The `http.Server` has `ErrorLog` writing to `io.Discard`, `ReadHeaderTimeout` of 10 s and no logging anywhere.
  - A loopback `Addr` (127.0.0.0/8, ::1, or `localhost`) serves plain HTTP. Any other serves TLS 1.3 with `cert.go`'s certificate. `0.0.0.0` and `::` put every non-loopback interface IP in the SANs.
  - A host name is refused: `give the page an address, not a name: <name>`.
  - A busy default port falls back to the kernel's. An explicit port that is busy is an error.

- [ ] **Step 1: Write the failing tests.**
  - `TestTheLinkCarriesTheSecretAndNothingElseAnswers`: the path secret decodes to 32 bytes. `/`, `/x`, `/<secret>x`, `/<secret>/` and `/<secret>?value=1` (which serves the page and reads no value) give 404, 404, 404, 404 and 200. Every header above is present.
  - `TestTheValueGoesInTheBodyAndIsNeverEchoed`: a POST of `value=CODE-123` reaches `Submit` as `CODE-123`, and no answer, accepted or rejected, contains `CODE-123`. A POST with `value` in the query and none in the body reaches `Submit` as empty.
  - `TestFiveWrongTriesEndThePage`: five rejections end with `Tries`, and a sixth connection is refused.
  - `TestWrongPathsDoNotCountAsTries` (O3): 200 requests to random paths leave it open, and the right value then gives `Accepted`.
  - `TestAFinalRefusalEndsThePage`: `Submit` returning `Final(err)` gives `Refused` with that message.
  - `TestTheFirstAcceptedLoginEndsIt`: `Accepted`, then the listener is closed.
  - `TestTheTimeLimitEndsIt`: with `Lifetime` 100 ms it gives `TimedOut`, and so does an `Ends` 50 ms away. Cancelling the context gives `Stopped`.
  - `TestLoopbackIsPlainAndElsewhereIsTLS`: `127.0.0.1:0` gives `http://`. A non-loopback IP of this machine (skipped when there is none) gives `https://`. A client that pins `Fingerprint` connects, its SHA-256 equals the served leaf's, the negotiated version is TLS 1.3, and a plain HTTP request to that port fails.
  - `TestANameIsRefusedAndLocalhostIsLoopback`.
  - `TestAnOversizedBodyCountsNoTry`: 65 KiB gives 413, and `Tries` is unchanged.
  - `TestNothingIsLogged`: the server's `ErrorLog` writes to `io.Discard`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/loginpage
  git commit -m "A one-time login page serves one login behind a 256-bit secret, over HTTPS with a certificate of its own off loopback, and closes after one login, five wrong tries or ten minutes"
  ```

### Task 5: Where the page may listen

**Files:**
- Create: `cmd/rota/loginpage/addrs.go`, `cmd/rota/loginpage/addrs_test.go`

**Interfaces:**
- Produces:
  - `type Choice struct { Addr, Iface, Who string; Warn bool }`.
  - `func Choices() ([]Choice, error)` reads `net.Interfaces`, and `func choices(ifs []iface) []Choice` is the pure part.
  - `type iface struct { Name string; Up, Loopback bool; Prefixes []netip.Prefix }`.
- The order and the words:
  1. `127.0.0.1`: `only this machine; from another one, through ssh -L`.
  2. RFC 1918 and `fc00::/7`: `machines on the local network of <iface>`.
  3. `100.64.0.0/10`: `machines on the carrier-grade NAT or Tailscale network of <iface>`.
  4. Any other address, with `Warn`: `anyone on the internet who can reach <iface>`.
  5. `0.0.0.0`, last, with `Warn`: `every interface: anyone who can reach this machine on any of them`.

  Down interfaces, link-local and multicast addresses and `::1` are left out.

- [ ] **Step 1: Write the failing tests.**
  - `TestLoopbackComesFirstAndEverywhereLast`, on a table of interfaces: lo, eth0 at 192.168.1.20/24, tailscale0 at 100.101.1.2/32, a public 203.0.113.7/24, a down en1, fe80::1/64 and 169.254.3.3/16. The result is exactly items 1 to 5 above, with `Iface` filled.
  - `TestChoicesOnThisMachine`: `Choices()` starts with `127.0.0.1` and ends with `0.0.0.0`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/loginpage/addrs.go cmd/rota/loginpage/addrs_test.go
  git commit -m "The login page offers this machine's addresses loopback first and every interface last, each saying who can reach it"
  ```

### Task 6: The shell

**Files:**
- Create: `cmd/rota/tui/options.go`, `cmd/rota/tui/env.go`, `cmd/rota/tui/tabs.go`, `cmd/rota/tui/overlay.go`, `cmd/rota/tui/effects.go`, `cmd/rota/tui/model.go`, `cmd/rota/tui/program.go`, and the stub `cmd/rota/tui/chrome.go` (Task 10 replaces it)
- Test: `cmd/rota/tui/shell_test.go`, `cmd/rota/tui/rules_test.go`, `cmd/rota/tui/program_test.go`

**Interfaces:**
- Consumes `Theme`, `Profile` and `globalKeys` (Task 1), and `Backend` with its types (Task 2).
- Produces:
  ```go
  type Options struct {
      Backend Backend
      In io.Reader; Out io.Writer; Env []string           // nil: os.Stdin, os.Stdout, os.Environ()
      Cols, Rows int; Resize <-chan [2]int                // set over SSH
      Remote bool; Host, Cwd string
      Daemon  func(ctx context.Context) Daemon            // polled every 5 s
      Exec    func(args ...string) *exec.Cmd              // this rota with args, on the terminal; nil over SSH
      OpenURL func(url string) error                      // nil where no browser is (R19)
      Attach  func(ctx context.Context, account, cols, rows int) (tea.ExecCommand, error) // Task 19; nil = direct
      Now func() time.Time; Loc *time.Location
      Tick func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd // default tea.Tick
      Profile *colorprofile.Profile                       // tests force one
  }
  type Daemon struct { Running bool; PID int; Live []Live; Err string }
  type Live struct { ID, Name string; Account int }
  type Result struct { Pending *rota.Login }
  type Model struct{ /* unexported */ }
  func New(o Options) *Model
  func Run(ctx context.Context, o Options) (Result, error)

  type env struct { b Backend; o Options; th Theme; who Principal; list Listing; daemon Daemon; width, height int }
  func (e *env) now() time.Time
  func (e *env) tick(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd
  func (e *env) control() bool
  func (e *env) gate(bs ...*key.Binding)        // disables control-only bindings for a watch principal
  func (e *env) call(d time.Duration, f func(ctx context.Context) tea.Msg) tea.Cmd // a Cmd running f under a deadline (R29)

  type tab interface {
      Title() string
      Update(msg tea.Msg) (tab, tea.Cmd)
      View(width, height int) string
      Keys() []key.Binding
      Typing() bool          // a field has focus: digits, [ ] and q go to it
      Busy() string          // what quitting would end, or ""
      Leave(ctx context.Context) // called by Run after the program ends: close what is open
  }
  func register(place int, make func(e *env) tab) // Accounts 10, Sessions 20, Ask 30, Services 40

  type overlay interface {
      Update(msg tea.Msg) (overlay, tea.Cmd)
      View(width, height int) string
      Keys() []key.Binding
      Closed() bool
  }
  type asider interface {     // an overlay the shell can keep while it is put aside: the login flow
      overlay
      Aside() bool
      Badge(now time.Time) string
      Return()
      Pending() *rota.Login
  }
  type step interface {
      Update(msg tea.Msg) (step, tea.Cmd)
      View(width int) string
      Keys() []key.Binding
      Typing() bool
  }
  type stepDone struct { End LoginEnd; Err error } // a step is finished: End, or an Err nothing will put right

  // messages
  type listingMsg struct { Listing Listing; Err error; How Refresh }
  type daemonMsg  struct { Daemon Daemon }
  type refreshMsg struct { How Refresh }
  type selectMsg  struct { Account int }
  type openMsg    struct { O overlay }
  type toastMsg   struct { Text string; Level int } // levelInfo, levelWarn, levelErr
  func toast(level int, format string, a ...any) tea.Cmd // cleaned where it is drawn (Task 10)
  // effects (R31): the shell turns them into Bubble Tea's own commands
  type handOffMsg   struct { Cmd tea.ExecCommand; Done func(error) tea.Msg }
  type clipboardMsg struct { Text string }
  type bellMsg      struct{}
  func handOff(c tea.ExecCommand, done func(error) tea.Msg) tea.Cmd
  func copyText(text string) tea.Cmd
  func openURL(e *env, url string) tea.Cmd
  type tailed struct{ /* *exec.Cmd and a 4 KiB ring of its stderr */ } // implements tea.ExecCommand
  func execTail(c *exec.Cmd) *tailed  // keeps the last 4 KiB of the command's stderr for a toast (R28)
  func (t *tailed) Last() string      // the last non-empty line of that tail
  ```
- The decisions:
  - **Init.** `Init` calls `Who`, then `Accounts(RefreshStale)`, `Daemon`, and `tea.RequestBackgroundColor`. Ticks every 15 s ask `Accounts(RefreshNone)`, every 2 min `Accounts(RefreshStale)` and every 5 s `Daemon`. A `refreshMsg` asks at once.
  - **Routing.** Key messages go to the open overlay, else the active tab. Every other message goes to every tab and to the overlay or asider.
  - **Global keys.** `1`–`4`, `[`, `]`, `q` and `M` act only when the focused view is not `Typing()`. In stub chrome, `q` quits at once.
  - **Size.** Below 60 columns or 14 rows, the view is `rota needs a terminal of at least 60×14; this one is W×H`.
  - **Effects.** `handOffMsg` becomes `tea.Exec`, `clipboardMsg` becomes `tea.SetClipboard`, and `bellMsg` becomes `tea.Raw("\a")`.
  - **Run.** It builds the program with `WithContext`, `WithInput`, `WithOutput`, `WithEnvironment` and `WithColorProfile(Profile(out, env, Remote))`, adding `WithWindowSize(Cols, Rows)` and `WithoutSignalHandler()` when `Remote`. It forwards `Resize` as `WindowSizeMsg` and sets `AltScreen` true. After the program ends it calls each tab's `Leave` under a 10 s context and returns the asider's `Pending()`.

- [ ] **Step 1: Write the failing tests** (a fake tab and a fake backend in `shell_test.go`).
  - `TestTabsRegisterInPlace`: places 30 then 10 registered give the header order 10, 30.
  - `TestKeysGoToTheFocusAndEverythingElseToAll`.
  - `TestTabsSwitchByNumberAndBracketUnlessTyping` (R6).
  - `TestTooSmallSaysSoAndForgetsNothing` (Review Focus 2): 59×14, 60×13, 0×0 and 1×1 show the message. Returning to 80×24 shows the fake tab's state unchanged, and nothing panics.
  - `TestTheListingIsReadOnOpenAndKeptFresh` (R2): with `fakeTicks`, `Init` gives `Who` and `Accounts(Stale)`. Advancing 15 s gives `Accounts(None)`, and 2 min gives `Accounts(Stale)`. A `refreshMsg{RefreshForce}` gives `Accounts(Force)`.
  - `TestABackendFailureIsAToastNotACrash` (Review Focus 5): `Accounts` fails, a `toastMsg` with the error follows, the next tick tries again, and keys still work.
  - `TestTheDaemonIsPolled`: a `Daemon` func returning `Running` then not gives two `daemonMsg`.
  - `TestEffectsBecomeTerminalCommands`: `handOffMsg`, `clipboardMsg` and `bellMsg` each produce a non-nil command and leave the model's view unchanged.
  - `TestExecTailKeepsTheLastWords`: a fakecli fake writing 6 KiB and then `boom` to stderr, run through `execTail`, gives `Last() == "boom"`, and the writer the program set still received every byte.
  - `TestWatchGatesControlKeys`: with `Who` answering watch, a binding passed to `gate` is disabled.
  - `TestUpdateDoesNoIO`: a counting backend sees no call while `Update` runs on every message above. Calls happen only when the returned commands run. The helper `assertNoIO(t, b *countingBackend, f func())` is reused by later tasks.
  - `TestRunHeadless` (`program_test.go`): `Run` with `In` an `io.Pipe` reader, `Out` a buffer, `Remote: true`, `Cols: 80` and `Rows: 24` ends when `q` is written and returns `Result{}`. On Windows, if Bubble Tea cannot read a pipe there, skip with that reason. The model tests above still run.
  - `TestNoFileIsPastSixHundredLines` (`rules_test.go`): every non-test `.go` file in the package has at most 600 lines.
  - `TestKeysComeOnlyFromBindings` (`rules_test.go`): no non-test file other than `keys.go` matches `\.String\(\)\s*(==|!=)` or `switch\s+\w+\.String\(\)`, and every `key.NewBinding(` is inside a function whose name ends in `Keys`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui
  git commit -m "The TUI's shell routes keys to what has focus, keeps the listing fresh through the Backend, asks for every terminal effect by message, and runs headless as well as on a terminal"
  ```

### Task 7: Bars, resets, text and the width ladder

**Files:**
- Create: `cmd/rota/tui/bars.go`, `cmd/rota/tui/ladder.go`, `cmd/rota/tui/text.go`, `cmd/rota/tui/bars_test.go`, `cmd/rota/tui/ladder_test.go`, `cmd/rota/tui/text_test.go`

**Interfaces:**
- Consumes `Theme` (Task 1).
- Produces:
  - `func bar(th Theme, pct float64, threshold, cells int) string` gives `▕` + cells + `▏ ` + the percent right-aligned in 4 columns (R8, R33).
    - Filled cells are `round(pct/100·cells)`, drawn `█`. Empty cells are `░`, Faint.
    - The tick `│` sits at `round(threshold/100·cells)`, clamped to `[0, cells-1]`, with no tick at threshold 100.
    - The style is chosen by `level(pct, threshold)`.
  - `func level(pct float64, threshold int) int` returns `lvGood`, `lvWarn` or `lvOver`.
  - `func resetText(at, now time.Time, loc *time.Location, short bool) string` (R9) gives `1h 3m · 21:20`, `2d 1h · Thu 21:20`, `<1m · 21:20` and `now`, and `1h 3m` when `short`. The zero time gives `""`.
  - `func deciding(a wire.Account) (w wire.Window, ok bool)` (R9).
  - `func stale(a wire.Account, now time.Time) bool` (R10).
  - `func clean(s string) string` drops every C0 control, the C1 controls, `ESC` and `DEL`, turns newlines and tabs into spaces, and passes the result through `wire.Unsecret`.
  - `func fit(s string, w int) string` truncates by display width with `…` and pads to `w`. `func wrap(s string, w int) []string`.
  - `type col struct { Key string; W int; Shown bool }`, and `func accountsLayout(width int) []col`. Phase 4 writes its own ladder in the same shape.
- The Accounts ladder (R12). The keys are `place` (2), `id` (3), `account` (20), `provider` (6), `5h` (bar 16 plus 7), `7d` (bar 16 plus 7), `reset` (17) and `state` (16, flex), with a gap of 2. While the shown columns are wider than `width`, take the next step:
  1. account to 16
  2. drop id
  3. drop provider
  4. bars to 10
  5. reset short (8)
  6. account to 12
  7. bars to 8
  8. state to 8
  9. drop 7d
  10. bars to 6
  11. account to 10

  Spare width goes to `state`, then to `account` up to 24. The result depends on `width` alone.

- [ ] **Step 1: Write the failing tests.**
  - `TestTheBarTicksAtTheThreshold`: `bar(th, 62, 75, 16)` stripped is `▕██████████░░│░░░▏  62%`. At threshold 100 there is no tick. 0% and 100% fill none and all.
  - `TestColourIsRelativeToTheThreshold`: `level(59, 75)` is good, `level(61, 75)` warn, `level(75, 75)` over, and `level(79, 100)` good.
  - `TestResetReadsAsCountdownAndClock`: with `now` at Tuesday 2026-11-10 20:17 UTC and `loc` UTC, `at` = now+1h3m gives `1h 3m · 21:20`, now+49h gives `2d 1h · Thu 21:17`, and a past time gives `now`.
  - `TestTheDecidingWindowIsTheOneThatHoldsItBack`: with 5h at 40 and 7d at 90 under threshold 80, the 7d window decides. With both at 100, the later reset decides. Scoped windows never decide.
  - `TestStaleAfterTenMinutesOrAWait`.
  - `TestCleanDropsControlsAndKeys` (Review Focus 1): `"a\x1b[2Jb\nc\x07"` gives `a[2Jb c`, and `"x sk-ant-api03-AAAA…"` is redacted.
  - `TestFitCountsColumnsNotBytes` (Review Focus 1): `fit("日本語ab", 5)` has display width 5 and ends in `…`. An emoji with ZWJ counts as 2.
  - `TestTheLadderShedsInOrder`: at 120 every column shows with account 16. At 80 there is no id and no provider, bars are 8 and reset is short. At 60 there is no id, provider or 7d, and bars are 8. Each total is at most its width.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/bars.go cmd/rota/tui/ladder.go cmd/rota/tui/text.go cmd/rota/tui/bars_test.go cmd/rota/tui/ladder_test.go cmd/rota/tui/text_test.go
  git commit -m "A usage bar shows the account's own threshold as a tick and is coloured against it, a reset reads 1h 3m · 21:20, and the account table sheds columns in one fixed order as the terminal narrows"
  ```

### Task 8: `rota login --web [addr]`

**Files:**
- Create: `cmd/rota/login_web.go`, `cmd/rota/login_web_test.go`
- Modify: `cmd/rota/main.go` (`login` takes `--web` through `takeWeb` beside `takePasted`, and `loginUsage` and the usage text gain the flag), `cmd/rota/login_wait.go` (`afterBegin` hands a `--web` login to `awaitWeb`), `cmd/rota/paste.go` (`loginKey` and `loginToken` take the value from the page when `--web`), `docs/reference.md` (the `--web` row under `rota login`, and a paragraph, *A one-time page for the code*, after the terminal login's), plus one bullet in `docs/releases/1.4.0.md`

**Interfaces:**
- Consumes `loginpage.Serve`, `Options`, `Final`, `Choices`, `DefaultPort` (Tasks 4 and 5), and the base's `finishOnce`, `finished`, `leftOpen`, `finishLater`, `expired`, `timeLeft` and `loginTerminal`.
- Produces:
  - `type webAsk struct { Addr string; Asked bool }`.
  - `func takeWeb(args []string) (rest []string, w *webAsk, err error)` (R23).
  - `func (c *cli) pickAddr(w *webAsk) (string, error)`.
  - `func (c *cli) awaitWeb(l *rota.Login, want loginpage.Want, submit func(ctx context.Context, v string) (string, error)) error`.
  - The seam `var pageChoices = loginpage.Choices`.
- The decisions:
  - `--web` works with a new login (bare, a provider, `--long`), with `<account-id>` (the bound login), with `--key [--label]`, with `<account-id> --token`, and with `<login-id>` and no code, which serves a page for that parked login.
  - With `--json`, or with a code after a login id, it is the usage error `--web serves a page for a person to open, so it takes no --json and no code`.
  - **No address, on a terminal** (`loginTerminal(c.in)`): stderr shows `Where should the one-time page listen?`, then one numbered line per `Choice` as `  N) <addr>  <who>`, a warned line ending ` — warning`, then `Choice [1]: `. Enter means 1, and anything else asks again.
  - **No address, off a terminal**: it takes `127.0.0.1` and says `the one-time page listens on 127.0.0.1; give --web an address for another` (R24).
  - **What it prints**, on stdout:
    - `Open the one-time page: <URL>`, then `It ends after one login, five wrong tries, 10 minutes or ^C.`
    - On loopback: `From another machine: ssh -L <port>:127.0.0.1:<port> <user>@<host>, then open the link there.`
    - Off loopback: `It is HTTPS with a certificate made for this run; its SHA-256 fingerprint is <fp> — check that the browser shows the same.`
  - **How it ends.**
    - `Accepted`: prints what `finished` prints and exits 0.
    - `Tries`: exits 1 with `five wrong tries closed the page; ` and `finishLater(l)`.
    - `TimedOut`: `expired(l)` if the login itself ran out, else `the page closed after 10 minutes; ` and `finishLater(l)`.
    - `^C`: exits 130 through `leftOpen`.
    - `Refused`: exits 1 with the message.
  - `submit` opens the store per call. A code uses `FinishLogin`, where `store.Unfinishable` and `rota.ErrNoLogin` become `loginpage.Final`. A key uses `LoginKey`, and a token uses `PasteLong`. Nothing submitted is printed.

- [ ] **Step 1: Write the failing tests** (with the base's `seedTwoClaude`, `approvalsByCode` and `typing`).
  - `TestLoginWebFinishesFromThePage`: `rota login 12 --web 127.0.0.1:0` prints the URL. A POST of `value=AS13` is refused on the page with `which is account 13`. A POST of `value=AS12` prints `Refreshed claude account 12` and exits 0, and the page is closed.
  - `TestLoginWebAsksWhereOnATerminal`: `typing("2\n")` with `pageChoices` giving loopback, a private choice and everywhere picks the second, and the menu shows the warning on `0.0.0.0`.
  - `TestLoginWebOffATerminalTakesLoopback`.
  - `TestLoginWebRefusesJSONAndACode`: both exit 2.
  - `TestAnAddressIsNeverAnAccountID`: `rota login --web 12` is account 12 on the default address, `--web :0 12` is the same, and `--web=127.0.0.1:0` works.
  - `TestLoginWebTakesAKeyUnseen`: `rota login --key --web 127.0.0.1:0` with a fake console endpoint adds the account, and neither stdout nor stderr contains the key.
  - `TestLoginWebLeavesTheLoginOpenOnAnInterrupt` (unix): SIGINT gives exit 130, `rota login <id> <code>` on stderr, and the login still parked.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, and add the reference row and paragraph. The notes bullet: `rota login --web [addr]` serves a one-time page that takes the code, an API key or a setup token, for a machine with no browser or an awkward paste, with the address rules above.
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes, `TestReferenceDocumentsEveryFlag` included.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota docs/reference.md docs/releases/1.4.0.md
  git commit -m "rota login --web serves a one-time page for the code, a key or a setup token, asking where it should listen, and ends with the login it finishes"
  ```

### Task 9: The Accounts tab

**Files:**
- Create: `cmd/rota/tui/accounts.go`, `cmd/rota/tui/accounts_view.go`, `cmd/rota/tui/accounts_test.go`, and goldens `accounts-60x20.txt`, `accounts-80x24.txt` and `accounts-120x40.txt`
- Create, as stubs whose bodies return `nil` or a closed overlay: `accounts_act.go` (Task 13), `edit.go` (Task 14), `login.go` (Task 15), `login_wait.go` (Task 16), `login_page.go` (Task 17), `launch.go` (Task 18)

**Interfaces:**
- Consumes the shell (Task 6), and `bar`, `level`, `resetText`, `deciding`, `stale`, `clean`, `fit` and `accountsLayout` (Task 7).
- Produces:
  - `type accountsTab struct`, registered at place 10, and `func newAccounts(e *env) tab`.
  - `type accountKeys struct { Up, Down, Run, MoveUp, MoveDown, More, Less, Typed, Out, First, Add, Relogin, Edit, Remove, Refresh key.Binding }`, with the keys `↑/k`, `↓/j`, `enter`, `shift+up/K`, `shift+down/J`, `+/=`, `-`, `%`, `x`, `u`, `n`, `l`, `e`, `d` and `r`. Every one but `Up`, `Down` and `Refresh` passes through `e.gate`.
  - The stub signatures, which later tasks fill in:
    ```go
    // accounts_act.go (Task 13)
    func (a *accountsTab) move(acct wire.Account, place string) tea.Cmd
    func (a *accountsTab) nudge(acct wire.Account, delta int) tea.Cmd
    func (a *accountsTab) typedThreshold(acct wire.Account) overlay
    func (a *accountsTab) toggleOut(acct wire.Account) tea.Cmd
    func (a *accountsTab) crossed(before, after []wire.Account) tea.Cmd
    // edit.go (Task 14)
    func newEdit(e *env, acct wire.Account) overlay
    func newRemove(e *env, acct wire.Account) overlay
    func newConfirm(e *env, title, body string, yes func() tea.Cmd) overlay
    // login.go (Task 15)
    func newLogin(e *env, again *wire.Account) asider // nil: a new account
    // login_wait.go (Task 16)
    func newDeviceStep(e *env, l *rota.Login, who string) step
    func newDelegatedStep(e *env, account int) step
    // login_page.go (Task 17)
    type pageAsk struct { Want loginpage.Want; Login *rota.Login; Approve string; Account int; Label string }
    func newPageStep(e *env, p pageAsk) step
    // launch.go (Task 18)
    func (a *accountsTab) launch(acct wire.Account) tea.Cmd
    ```
- The decisions:
  - **The table.** Rows are in `Listing` order, with columns from `accountsLayout(width)`.
    - `place` is the order, or `-` when out of the rotation.
    - `account` is `clean(name or email)`.
    - The bars are the `5h` and `7d` windows by name. An account without them shows `api key`, `n/a` or `?` in that cell.
    - `reset` is `resetText(deciding)`.
    - `state` is one of: `dead: l signs in` (with the reason in the details), `limited`, `over its N%`, `out of rotation`, `login ends in 2d`, `out of credit (3h)` or `ok`.
    - A stale row's numbers are Faint.
  - **The details pane** fills the rows the table leaves. It holds:
    - every window, with its bar, percent and reset
    - the threshold
    - the place
    - the folder, the config, and the sessions mode
    - the credential kind: login, long token or API key
    - `login_until` and `long_until`, with days left, and `no_credit_until`
    - the status and its dead reason, wrapped
    - when it was checked, and the provider's wait if any
    - the quota note
  - **Scrolling.** The table scrolls to keep the selection visible.
  - **Selection.** It follows the account id across listings. A vanished id selects its neighbour, and `selectMsg` selects an id.
  - **Empty listing.** `No accounts yet. n adds one.`, or for watch `No accounts yet.`
  - **The bell.** Each `listingMsg` after the first calls `a.crossed(old, new)`.
  - **No reflow.** A `daemonMsg` never changes the table's layout.

- [ ] **Step 1: Write the failing tests** (a fake backend with the design's three accounts):
  - `work@…`: claude, 62%/21%, threshold 75
  - `home@…`: claude, 12%/7%
  - `cdx@…`: codex, 95%, dead

  The clock is fixed at Tuesday 2026-11-10 20:17 UTC.
  - `TestTheTableAtThreeSizes`: goldens at 60×20, 80×24 and 120×40.
  - `TestALabelNeverEscapesItsCell` (Review Focus 1): the labels `"\x1b[2Jevil"`, `"日本語のアカウント"` and `"👨‍👩‍👧 family"` give every line a display width of at most the terminal's, no `ESC` inside a cell, and every column starting at the same offset on every row.
  - `TestSelectionFollowsTheAccountNotTheRow` (Review Focus 4): a listing that reorders keeps the same id selected, and one without it selects the neighbour.
  - `TestDetailsShowEveryWindowAndSetting`.
  - `TestStaleReadingsFade`: the styled view of a stale row has the Faint attribute on its percent.
  - `TestNoReflowWhenASessionStarts`: `daemonMsg` with 0 and then 2 live sessions gives identical table lines.
  - `TestKeysReachTheirActions`: each key calls its stub, recorded through test hooks on the stub variables.
  - `TestWatchSeesNoChangeKeys`: `Keys()` for a watch principal holds only `↑`, `↓` and `r`.
  - `TestManyAccountsScroll`: 30 accounts at 80×24 keep the selection visible past the bottom.
  - `TestAccountsUpdateDoesNoIO`, with `assertNoIO`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui
  git commit -m "The Accounts tab shows every account's usage against its own threshold, with when it resets and what state it is in, and the selected account's every window and setting below"
  ```

### Task 10: The shell's chrome

**Files:**
- Modify: `cmd/rota/tui/chrome.go` (replaces Task 6's stub)
- Create: `cmd/rota/tui/chrome_test.go`, and the golden `chrome-80x24.txt`

**Interfaces:**
- Consumes the shell (Task 6) and `Theme` (Task 1).
- Produces:
  - `func (m *Model) header(width int) string`, `func (m *Model) footer(width int) string`, `func (m *Model) chromeKey(k tea.KeyPressMsg) (handled bool, cmd tea.Cmd)` and `func (m *Model) helpView(width, height int) string`.
- The decisions:
  - **Header.** ` rota   <tabs>` on the left, the active tab in Accent. On the right:
    - `● daemon  N live` when running, Good
    - `○ no daemon`, Muted
    - when `Remote`, also `  <name> · <role>`
  - **Footer.** The focused view's `Keys()` short help, then `? help  q quit`. When an asider is aside, its `Badge` comes first.
  - **Toasts.** Drawn above the footer, right-aligned, at most three, each for 4 s (R30).
  - **Quit.** The first `q` or `ctrl+c` shows `press q again to quit`, plus `; <Busy()> and <badge> end too` when any are set. A second within 2 s quits.
  - **Help.** `?` toggles an overlay listing every enabled binding of the focused view and the global ones, grouped.
  - **Mouse.** `M` toggles `MouseMode` between none and cell motion (R7). A click on a tab switches to it, a click on a table row selects it, and the wheel scrolls.

- [ ] **Step 1: Write the failing tests.**
  - `TestTheHeaderSaysWhatRuns`: three cases, as above.
  - `TestQuitTakesTwoPresses`: `q`, then `q` within 2 s quits. After 2 s a `q` asks again. `ctrl+c` behaves the same. With a busy tab the toast names it.
  - `TestHelpListsExactlyTheEnabledKeys`: the help overlay's lines equal the enabled bindings' help, for control and for watch.
  - `TestToastsStackAndFade`: four toasts show the last three, and they are gone after `advance(4s)`.
  - `TestMouseIsOffUntilM`.
  - `TestTheChromeAt80x24`: a golden.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/chrome.go cmd/rota/tui/chrome_test.go cmd/rota/tui/testdata/golden/chrome-80x24.txt
  git commit -m "The TUI's header says whether a daemon runs, its footer shows only the keys that work where you are, toasts come and go, and quitting takes a second press"
  ```

### Task 11: The Ask tab

**Files:**
- Create: `cmd/rota/tui/ask.go`, `cmd/rota/tui/ask_view.go`, `cmd/rota/tui/ask_test.go`, and the golden `ask-80x24.txt`

**Interfaces:**
- Consumes the shell (Task 6) and `Backend.Ask`, `SendRun`, `InterruptRun` and `CloseRun` (Task 2).
- Produces:
  - `type askTab struct`, registered at place 30.
  - `type askKeys struct { Send, Newline, Focus, BackFocus, Left, Right, Toggle, KeepOpen, Stop, CloseRun key.Binding }`, every one but `Focus` and `BackFocus` gated.
- The decisions (R25, R26, R27):
  - **Layout.** The answer is a `viewport` on top. Below it is the prompt, a `textarea` 3 rows high growing to 6, with `InsertNewline` rebound to `alt+enter` and `shift+enter`. Under that is one line: `account: auto (now #1 work@…)` or `account: #3 cdx@…`, the readings from `message.Names` as `[x] blocks`, and `keep open: off`.
  - **Focus.** `tab` and `shift+tab` move between the prompt, the account and the readings. `←` and `→` cycle `auto` plus every account in listing order, or move between readings. `space` toggles a reading.
  - **Asking.** `enter` sends a `Question{Account, Prompt, Cwd, With, Input: keepOpen}`.
    - The stream runs in a command that pushes events into a channel, and a `waitEvent(ch)` command delivers one `askEventMsg` at a time.
    - Text events are shown with `eventStream`'s rule: delta fragments, then the whole piece skipped when it repeats them.
    - A `tool` event is one Muted line, `· tool <name>`.
    - The end line is `#<account> · <duration> · $<cost> · exit <code>`, followed by readings as indented JSON.
  - **Keeping it open.** `ctrl+s` sets keep-open for the next question. While a run is open, `enter` sends through `SendRun` with the `run_id` from its init event. `esc` sends `InterruptRun` while it is answering, and `ctrl+x` sends `CloseRun`.
  - **Stopping and leaving.** `esc` on a one-shot answer cancels its context. `Busy()` is `an answer is streaming` or `a run is open`. `Leave` closes an open run.
  - **Watch.** The tab shows `asking runs an account, which the watch role may not do` and takes no keys but focus.
  - **Refusals.** A refusal, for example a busy codex account (409), is said in Over under the prompt, cleaned.

- [ ] **Step 1: Write the failing tests.**
  - `TestAskStreamsTheAnswer`: a fake backend emits a text delta `fo`, then `four`, then whole `four` and a tool `Bash`. The view shows `four` once and `· tool Bash`.
  - `TestAskPicksAutoOrAnAccount`.
  - `TestReadingsAreToggledAndShownAsJSON`.
  - `TestAKeptOpenRunTakesMoreMessages`: `ctrl+s`, `enter`, a second prompt and `enter` give `SendRun(runID, …)`. `ctrl+x` gives `CloseRun`.
  - `TestEscStopsAnAnswer`: the fake sees its context cancelled.
  - `TestLeavingClosesAnOpenRun`.
  - `TestACutStreamEndsTheAnswerAndSaysSo` (Review Focus 5): the fake returns an error mid-stream. The partial answer stays, and the error is under it.
  - `TestWatchCannotAsk`.
  - `TestAskOverTheRealAPI`: `NewHTTP` against an in-process server with a fakecli claude. The answer `four` shows, and the end line names account 1.
  - `TestAskAt80x24`: a golden.
  - `TestAskUpdateDoesNoIO`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/ask.go cmd/rota/tui/ask_view.go cmd/rota/tui/ask_test.go cmd/rota/tui/testdata/golden/ask-80x24.txt
  git commit -m "The Ask tab sends one request through the rotation or a chosen account, streams the answer with the readings asked for, and can keep the run open for more"
  ```

### Task 12: Bare `rota` opens the TUI

**Files:**
- Create: `cmd/rota/tui_entry.go`, `cmd/rota/tui_entry_test.go`, `cmd/rota/tui_entry_unix_test.go`
- Modify: `cmd/rota/main.go` (the `len(argv) == 0` arm), `docs/reference.md` (one sentence under *The command*: bare `rota` on a terminal opens the TUI, and anywhere else prints the short usage)

**Interfaces:**
- Consumes `tui.Run`, `tui.Options`, `tui.NewHTTP` (Tasks 2 and 6), `client.InProcess` and `(*Client).HTTP` (phase 2), `client.DialNow` and `List`, `holdwire.RunningDaemon` and `Scan`, and the base's `timeLeft` and `finishLater`.
- Produces:
  - `var tuiTerminal = func(in io.Reader, out io.Writer) bool` is true only when both are `*os.File` terminals, `TERM` is not `dumb`, and `runtime.GOOS` is linux or darwin (R4).
  - `var startTUI = tui.Run`.
  - `func (c *cli) openTUI() error`.
  - `func localDaemon(terminals string) func(ctx context.Context) tui.Daemon`. When the lock is held, it is `DialNow` then `List` with the live sessions. Otherwise it is live `holdwire.Scan` records, for holders outliving a stopped daemon, with `Running` false.
  - `func selfCmd(args ...string) *exec.Cmd` runs `os.Executable()` with `args` and `os.Environ()`.
  - `func browser() func(url string) error` (R19).
- The decisions:
  - **The entry.** `run` with no argv, `!c.json` and `tuiTerminal(c.in, stdout)` returns `c.report(c.openTUI())`. Otherwise it is exactly as today.
  - **openTUI.**
    1. Open the store once for its directory and `TerminalDir()`, then close it.
    2. `api.New` as R1, with `Log` discarding.
    3. `cl, stop := client.InProcess(srv.LocalHandler(local))`.
    4. `startTUI(ctx, Options{Backend: tui.NewHTTP(cl.HTTP(), "http://rota"), Daemon: localDaemon(t), Exec: selfCmd, OpenURL: browser(), Host: os.Hostname()})`.
    5. Then `stop()` and `srv.Stop()`.
    6. A `Result.Pending` prints `The login stays open for <timeLeft>: <finishLater>` to stderr (R18).

- [ ] **Step 1: Write the failing tests.**
  - `TestBareRotaOffATerminalPrintsUsage`: `run(nil, &out, &err)` prints `shortUsage`, exits 0, and `startTUI` is never called. This mirrors the existing test.
  - `TestBareRotaOnATerminalOpensTheTUI`: with `tuiTerminal` true, `startTUI` is recorded with a non-nil `Backend`, `Exec` and `Daemon`.
  - `TestJSONAloneStillPrintsUsage`.
  - `TestTheLocalTUIOpensNoPortAndNoSocket`: `openTUI` with `startTUI` calling `Who` and `Accounts` and returning. Neither `daemon.lock`, `ctl.sock` nor `share.sock` exists afterwards, the Backend answered from a throwaway store, and no new listening TCP socket shows in `/proc/net/tcp` on Linux (skipped elsewhere).
  - `TestAPendingLoginIsSaidOnTheWayOut`.
  - `TestATerminalIsBothEnds` (unix, `internal/pty`): a pty on both ends is true. A pty in and a pipe out is false, and `TERM=dumb` is false.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes. The integrator runs the docker form.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota docs/reference.md
  git commit -m "Bare rota on a terminal opens the TUI over an API served inside the process, opening no port or socket, and prints the short usage everywhere else as before"
  ```

### Task 13: Moving accounts, thresholds and the bell

**Files:**
- Modify: `cmd/rota/tui/accounts_act.go` (replaces Task 9's stub)
- Create: `cmd/rota/tui/accounts_act_test.go`

**Interfaces:**
- Consumes `accountsTab` (Task 9) and `Backend.Change` (Task 2).
- Produces the five methods of Task 9's stub. The decisions follow R11 and R36.
  - `move` sends `Change{Order: place}` with `up`, `down`, `first` or `last` and `out`. After the answer it asks `refreshMsg{RefreshNone}`, because a move shifts the others.
  - `nudge` keeps one change in flight per id and coalesces later presses.
  - `typedThreshold` is a one-field overlay: 1–100, `enter` applies, and a bad value shows `a threshold is a whole percentage from 1 to 100`.
  - `toggleOut` puts an excluded account back last, and otherwise sends `out`.
  - `crossed` emits `bellMsg` and a toast, `claude/a@b crossed its 80% (5h 82%)`, for each id whose `Percent` went from below its `Threshold` to at or above it. It re-arms when the account falls back below.
  - A refusal is a toast, cleaned.

- [ ] **Step 1: Write the failing tests.**
  - `TestMovesSendOnePlaceEach`.
  - `TestNudgesCoalesce`: ten `+` presses while one change is in flight send two PATCHes, the second with the summed value, clamped to 100.
  - `TestTypedThresholdChecksItsValue`.
  - `TestExcludeAndPutBack`.
  - `TestTheBellRingsOnceAndRearms`: listings at 70, then 85, then 90, then 70, then 85 against 80 give a bell on the second and the fifth only. The first listing at 85 rings nothing.
  - `TestARefusedMoveIsAToast`.
  - `TestActUpdateDoesNoIO`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/accounts_act.go cmd/rota/tui/accounts_act_test.go
  git commit -m "In the TUI an account moves up, down, first or out of the rotation, its threshold steps by five or is typed, and crossing it rings the bell once until it falls back"
  ```

### Task 14: Edit, remove and confirm

**Files:**
- Modify: `cmd/rota/tui/edit.go` (replaces Task 9's stub)
- Create: `cmd/rota/tui/edit_test.go`

**Interfaces:**
- Consumes `Backend.Change` and `Remove` (Task 2) and `e.daemon.Live` (Task 6).
- Produces `newEdit`, `newRemove` and `newConfirm` with Task 9's signatures.
- The decisions (R35):
  - **The Edit form.** The fields are Order (the `rota set --order` grammar), Threshold, Folder, Config, Sessions (`shared`, `own` or a directory) and Remote Control (on or off, claude only). There are two actions: `forget long token` (only with `long_until`) and `clear`.
    - `tab` and `shift+tab` move between them, `enter` saves, and `esc` cancels.
    - Saving sends one `Change` of the changed fields.
    - A 400 shows its message beside the field it names, or at the top.
    - `clear` asks `newConfirm` first.
  - **Remove.** It says `Remove <provider> account N (<label>)? Its home and staged credentials go with it.`, followed by `N live session(s) on it: <name> (<id8>), …; rota refuses to remove an account a CLI runs on` when any are live. `y` removes and `n` or `esc` keeps it.
  - **Gone.** When the account leaves the listing while an edit or remove is open, the overlay says `account N is gone (removed elsewhere)`, and `enter` closes it.

- [ ] **Step 1: Write the failing tests.**
  - `TestEditSavesOnlyWhatChanged`.
  - `TestEditShowsTheRefusalBesideItsField`: `config_dir` inside the store gives the API's message beside Config.
  - `TestClearAsksFirstAndSendsTheFour`.
  - `TestForgetLongShowsOnlyWithALongToken`.
  - `TestRemoveNamesLiveSessions`.
  - `TestAnEditOfAnAccountRemovedElsewhereSaysSo` (Review Focus 4): a listing without the account gives the gone message. A save that races it gets 404, and the message is shown.
  - `TestConfirmIsYesOrNo`.
  - `TestEditUpdateDoesNoIO`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/edit.go cmd/rota/tui/edit_test.go
  git commit -m "In the TUI every rota set flag is one form saved in one write, and removing an account asks first and names the sessions running on it"
  ```

### Task 15: The add and re-login flow

**Files:**
- Modify: `cmd/rota/tui/login.go` (replaces Task 9's stub)
- Create: `cmd/rota/tui/login_steps.go`, `cmd/rota/tui/login_test.go`, and the golden `login-80x24.txt`

**Interfaces:**
- Consumes `Backend.BeginLogin`, `FinishLogin`, `PasteKey` and `PasteToken` (Task 2), the API from Task 3, `newDeviceStep`, `newDelegatedStep` and `newPageStep` (stubs until Tasks 16 and 17), `copyText` and `openURL` (Task 6), `store.PendingTTL`, `wire.LoginProviders` and `rota.LongLived`.
- Produces:
  - `newLogin`, which returns an `asider`.
  - In `login_steps.go`: `func newCodeStep(e *env, l *rota.Login, who string) step` and `func newSecretStep(e *env, s secretAsk) step`.
  - `type secretAsk struct { Want loginpage.Want; Login *rota.Login; Account int; Label bool }`. With `Key` and no `Login` it is `PasteKey`. With `Token` it is `PasteToken(Account)`. With a `Login` of kind `apikey` it is `FinishLogin(l.ID, value)`.
  - `func timeLeft(ends, now time.Time) string` gives `12m left`, and `45s left` in the last minute.
- The decisions (R13, R14, R16, R17, R18):
  - **New account (`n`).**
    1. Pick the provider from `wire.LoginProviders()`, with `rota.DefaultProvider` preselected.
    2. For a provider with `rota.LongLived` or `== rota.DefaultProvider`, pick the kind, `short` preselected. Each option says what it gives up:
       - `short`: `A login rota refreshes; usage readings and Remote Control need it.`
       - `long`: `A year, inference only: no usage readings or Remote Control. Two approvals: the login that makes the account, then the year-long token.`
       - `API key`: `A Claude Console key, paid per token: no usage readings or Remote Control; it starts out of the rotation.`
    3. Others skip the kind. After the begin, the login's `Kind` picks the step: `code` gives the code step, `device` the device step, and `apikey` the secret step, plus the choice `let <provider> sign itself in` when `Delegated`. That choice finishes with no code, then shows the delegated step for the new account.
  - **Again (`l`) on account N.**
    - A `Credential: "apikey"` account goes straight to the secret step, with `the same key again keeps this account; a different one makes an account of its own`.
    - A claude login account picks short (bound, `LoginBegin{Account: N}`), long (`LoginBegin{Long: true}`, then: `the long token went on account M, which approved it` when M ≠ N) or a setup token (the secret step, `Token`).
    - Any other account takes `LoginBegin{Account: N}`. A refusal with reason `delegated` shows the delegated step.
  - **The code step.**
    - It shows `Open this and approve access as <who>`, with `, which is <provider> account N; no other account's approval will finish it` when bound, then the URL wrapped, `o open` (only with `OpenURL`), `c copy`, `w one-time page`, and the field, blurred, with `timeLeft`.
    - `tab` or `enter` focuses the field. A `tea.PasteMsg` focuses it and fills it trimmed of spaces, CR and LF.
    - `enter` in the field submits once. Further `enter` presses wait for the answer.
  - **An answer to a submission.**
    - `Pending` says `Not approved yet; approve in the browser, then give it again.`
    - A `Refusal` with status 404, or with reason `unfinishable`, ends the flow with the message and `n begins another`.
    - Any other error stays beside the field with the value kept (Review Focus 3).
    - Success sends `stepDone`.
    - Reaching `ends` locally ends the flow: `the login <id> has expired; n begins another`.
  - **The secret step.** A `textinput` with `EchoPassword`, plus a label field when `Label`. The value is never put into the view, a toast or an error.
  - **Done.** It closes, emits `selectMsg{End.Account}` and `refreshMsg{RefreshStale}`, and toasts `Added <p> account N`, `Refreshed <p> account N` or `Long-lived token stored for #N, until <date>`. `n` long then begins the long login on the new account.
  - **Aside and back.** `esc` with the field blurred puts the flow aside, with the badge `login <id6> · 12m left · n returns`. `n` or `l` returns it. `x` abandons it with a toast saying the login stays parked until it runs out. `Pending()` is the parked login.
  - **Watch.** For a watch principal `n` and `l` are already gated out by Task 9.

- [ ] **Step 1: Write the failing tests** (a scripted fake backend unless noted).
  - `TestAddAClaudeAccountWithACode`: `n`, `enter` (claude), `enter` (short), then a paste of `WRONG`. The error is beside the field and `WRONG` is still in it. A paste of `GOOD` then gives `selectMsg{14}` and the toast `Added claude account 14`.
  - `TestTheKindSaysWhatEachGivesUp`: the three texts above, and codex has no kind step.
  - `TestAgainIsBoundToTheAccount`: `l` on account 12 sends `LoginBegin{Account: 12}`, and the step says `no other account's approval will finish it`.
  - `TestALongTokenThatWentElsewhereIsSaid`.
  - `TestAPasteFillsTheFieldOnceAndShowsNoKey` (Review Focus 3): a paste of `"  CODE\r\n"` gives `CODE` and one submit. `sk-ant-api03-…` pasted into the secret step never appears in `plain(view)` or any toast, and an error that echoes it shows it redacted.
  - `TestAProviderFailureKeepsTheField` (Review Focus 5): a 502 shows beside the field and keeps the value, and the next submit goes to the same login.
  - `TestUnfinishableAndExpiredEndTheFlow`.
  - `TestTheFlowGoesAsideAndComesBack`: `esc` shows the badge in the footer, `n` returns to the same field and value, and `x` abandons.
  - `TestAResizeKeepsTheFlowAndItsField` (Review Focus 2): a resize to 50×10 and back to 80×24 keeps the same field value and countdown.
  - `TestADelegatedAccountGoesToItsStep`: a refusal with reason `delegated` opens `newDelegatedStep`.
  - `TestNeverDrawsTheLoginState`: no `State` value of the login appears in the view.
  - `TestTheFlowOverTheRealAPI`: `NewHTTP` and an in-process server with the `approvalsByCode` endpoint. `l` on account 12 with `AS13` gives `which is account 13` beside the field, and `AS12` refreshes account 12.
  - `TestTheFlowAt80x24`: a golden.
  - `TestLoginUpdateDoesNoIO`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/login.go cmd/rota/tui/login_steps.go cmd/rota/tui/login_test.go cmd/rota/tui/testdata/golden/login-80x24.txt
  git commit -m "In the TUI n adds an account and l signs one in again in a short guided flow, a wrong code staying in its field beside the reason, and a flow put aside keeps waiting"
  ```

### Task 16: Device and delegated logins

**Files:**
- Modify: `cmd/rota/tui/login_wait.go` (replaces Task 9's stub)
- Create: `cmd/rota/tui/login_wait_test.go`

**Interfaces:**
- Consumes `Backend.FinishLogin` (Task 2), `handOff`, `execTail`, `copyText` and `openURL` (Task 6), and `Options.Exec` and `Host`.
- Produces `newDeviceStep` and `newDelegatedStep` with Task 9's signatures (R15, R21).
  - **Device step.** It shows `Approve as <who> at <URL>`, the `user_code` query value large when present, `o` and `c`, and `Waiting for the approval (12m left)`.
    - It polls `FinishLogin(l.ID, "")` every `State["interval"]` seconds, else 5 s, through `e.tick`.
    - `Pending` waits again.
    - A refusal that is neither 404 nor `unfinishable` is shown, and polling goes on.
    - Success sends `stepDone`.
  - **Delegated step, locally.** It says `<provider> signs this account in itself; enter hands it the terminal`. `enter` calls `handOff(execTail(e.o.Exec("login", N)), …)`. On return, a zero exit sends `stepDone{End{Account: N, Status: "refreshed"}}` and asks for a refresh. A non-zero exit shows the tail's last line.
  - **Delegated step, over SSH** (`Exec` nil). It says `on <Host>, run: rota login N`, with `c` copying `rota login N`.

- [ ] **Step 1: Write the failing tests.**
  - `TestADeviceLoginFinishesUnprompted`: pending twice, then added after two advances of the poll interval, with no key pressed.
  - `TestTheDeviceCodeIsShown`: the URL `…?user_code=ABCD-EFGH` shows `ABCD-EFGH`.
  - `TestTheIntervalComesFromTheLogin`.
  - `TestADelegatedLoginHandsOverTheTerminal`: `enter` emits a `handOffMsg`. The test runs its `Cmd`, a fakecli fake exiting 0, and feeds `Done(nil)`, which gives `stepDone`.
  - `TestAFailedHandOffShowsItsLastWords`: the fake writes `boom` to stderr and exits 3, and the step shows `boom`.
  - `TestOverSSHTheCommandIsShownAndCopied`: `c` gives `clipboardMsg{"rota login 6"}`.
  - `TestWaitUpdateDoesNoIO`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/login_wait.go cmd/rota/tui/login_wait_test.go
  git commit -m "In the TUI a device login finishes by itself once approved, and a login a CLI makes itself gets the terminal handed to it, or its command over SSH"
  ```

### Task 17: The one-time page from the flow

**Files:**
- Modify: `cmd/rota/tui/login_page.go` (replaces Task 9's stub)
- Create: `cmd/rota/tui/login_page_test.go`

**Interfaces:**
- Consumes `loginpage.Serve`, `Choices` and `Final` (Tasks 4 and 5), `Backend` (Task 2) and `copyText` (Task 6).
- Produces `newPageStep` and `pageAsk` with Task 9's signatures, plus the seam `var pageChoices = loginpage.Choices`.
- The decisions (O2):
  - `w` from a code or secret step opens this step. It lists the `Choice`s with `127.0.0.1` selected, the `Who` text beside each, and the `Warn` ones in Warn.
  - `enter` serves with `Submit`:
    - `FinishLogin(l.ID, v)` for a code
    - `PasteKey(v, label)` or `PasteToken(Account, v)` for a secret
    - a 404 or `unfinishable` becomes `loginpage.Final`
  - **While serving**, it shows the page URL with `c` to copy it, the fingerprint when TLS, the `ssh -L` line on loopback, `ends after one login, five wrong tries or 10 minutes`, and the countdown. `esc` closes the page and returns to the previous step.
  - **The ends.** `Accepted` sends `stepDone` with the Backend's `LoginEnd`. `Tries` and `TimedOut` return to the previous step with the reason. `Refused` sends `stepDone{Err}`.
  - The page runs in this process, which over SSH is the daemon, so it listens on the daemon's machine.

- [ ] **Step 1: Write the failing tests.**
  - `TestThePageFinishesTheFlow`: `127.0.0.1` is chosen. The test POSTs `value=GOOD` to the URL the view shows (read from the step, not scraped), and `stepDone` carries `Account: 14`.
  - `TestTheChoicesSayWhoCanReachThem`, with `pageChoices` faked.
  - `TestEscClosesThePage`: the listener is closed afterwards.
  - `TestFiveWrongTriesReturnToTheField`.
  - `TestThePageURLIsCopiedNotLogged`: `c` gives a `clipboardMsg` with the URL, and no toast contains the secret.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/login_page.go cmd/rota/tui/login_page_test.go
  git commit -m "The TUI's login flow can serve the same one-time page rota login --web does, on an address it asks for, and finishes when the page is used"
  ```

### Task 18: `⏎` runs the account's CLI here

**Files:**
- Modify: `cmd/rota/tui/launch.go` (replaces Task 9's stub)
- Create: `cmd/rota/tui/launch_test.go`

**Interfaces:**
- Consumes `handOff` and `execTail` (Task 6), and `Options.Exec` and `Attach`.
- Produces `launch` with Task 9's signature (R28):
  - A dead account toasts `account N is dead: l signs it in again`.
  - With `Options.Attach` set and either `e.daemon.Running` or `Remote`, it calls the held path. Until Task 19 that path is the direct one.
  - With `Exec`, it is `handOff(execTail(e.o.Exec("run", N)), …)`. On return it asks `refreshMsg{RefreshStale}`, and a non-zero exit toasts `rota run N exited <code>: <last stderr line>`.
  - With neither, it toasts `this connection has no terminal to hand over; Sessions (in a later beta) attaches over SSH`.

- [ ] **Step 1: Write the failing tests.**
  - `TestEnterRunsTheCLIOnTheTerminal`: with `Exec` returning a fakecli fake exiting 0, the `handOffMsg` is run and the listing is refreshed afterwards.
  - `TestAFailedRunSaysWhy`: exit 3 with `boom` on stderr gives the toast.
  - `TestADeadAccountIsNotRun`.
  - `TestWithoutATerminalItSaysSo`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/launch.go cmd/rota/tui/launch_test.go
  git commit -m "Enter on an account hands the terminal to its CLI, as rota run does, and the TUI comes back when it ends, saying why when it failed"
  ```

### Task 19: `⏎` into a held session (needs phase 2's attach)

**Files:**
- Modify: `cmd/rota/tui/launch.go` (the held path), `cmd/rota/tui_entry.go` (`Options.Attach` for a local TUI)
- Create: `cmd/rota/tui/launch_held_test.go`, `cmd/rota/tui_held_unix_test.go`

**Interfaces:**
- Consumes `attach.Command`, `attach.Options`, `attach.DaemonKey` and `(*attach.Cmd).Result` (phase 2), `client.Create` (phase 1), `store.HostEnv()` and `pty.Size`.
- Produces:
  - `func localAttach(terminals string) func(ctx context.Context, account, cols, rows int) (tea.ExecCommand, error)` in `cmd/rota`. It dials, then `Create{Account, Cwd: os.Getwd(), Cols, Rows, Env: store.HostEnv()}`, then `attach.Command(c, s.ID, attach.Options{Key: attach.DaemonKey(terminals), Settle: true, Teach: <first attach of this process>})`.
  - In `launch`: `e.o.Attach(ctx, N, e.width, e.height)` and then `handOff`. On return it toasts `left session <name>; it keeps running` (`Result.Why` Detached) or `session <name> ended (exit N)`. A create refusal (409 busy) toasts its message, which names the holding session.

- [ ] **Step 1: Write the failing tests.**
  - `TestEnterOpensAHeldSessionWhenADaemonRuns`: a fake `Attach` is called with the account and the size, its `ExecCommand` is handed off, and the toast follows.
  - `TestABusyAccountNamesItsSession`.
  - `TestLocalAttachCreatesAndAttaches` (unix, process-level as in phase 1, with `ROTA_AS_RUN=1` and the tty fake): a daemon runs, `localAttach(...)(ctx, 1, 80, 24)` gives a command whose `Run`, fed `x\r` and then `Ctrl-\ d`, returns `nil` with `Result().Why == attach.Detached`, and the session is still live.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes. The integrator runs the docker form.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/tui/launch.go cmd/rota/tui/launch_held_test.go cmd/rota/tui_entry.go cmd/rota/tui_held_unix_test.go
  git commit -m "Enter on an account opens it in a held session and attaches when a daemon runs, and detaching comes back to the TUI with the session still running"
  ```

### Task 20: `ssh` opens the TUI (needs phase 2's sshd)

**Files:**
- Create: `cmd/rota/tui_ssh.go`, `cmd/rota/tui_ssh_test.go`
- Modify: the file where phase 2 builds `sshd.Options` (named in phase 2's plan) to set `TUI: sshTUI`, and `docs/reference.md` (one sentence in phase 2's SSH section: `ssh -p <port> host` opens the same TUI, with what the key's user may do)

**Interfaces:**
- Consumes `sshd.TUISession` (phase 2), `tui.Run` and `tui.NewHTTP` (Tasks 2 and 6), and `attach.Command` (phase 2).
- Produces `func sshTUI(ctx context.Context, s sshd.TUISession) error`, which runs:
  ```go
  tui.Run(ctx, tui.Options{Backend: tui.NewHTTP(s.Client.HTTP(), "http://rota"), In: s.In, Out: s.Out, Env: s.Env, Cols: s.Cols, Rows: s.Rows, Resize: s.Resize, Remote: true, Host: <hostname>, Cwd: <the daemon user's home>, Daemon: <s.Client.List>, Attach: <Create without Env, then attach.Command(s.Client, id, attach.Options{Key: s.Key, Settle: true}) with SetWindow from the session's size and Resize>})
  ```
  `Exec` and `OpenURL` stay nil. A `Result.Pending` is written to `s.Out` with the stays-open line, and `run it on <host>` is added.

- [ ] **Step 1: Write the failing tests.**
  - `TestSSHRunsTheSameModel`: a fake `TUISession` over `io.Pipe` and an in-process client for a watch user. `q` and `q` end it, and the view has `· watch` in the header and no `n` in the footer.
  - `TestSSHSeesTheDaemonAlways`.
  - `TestSSHLeavesAPendingLoginOnTheHost`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** This is a **race** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota docs/reference.md
  git commit -m "ssh to the rota daemon opens the same TUI, allowed what the key's user may do and no more"
  ```

### Task 21: Goldens on a throwaway store, the manual and the notes

**Files:**
- Create: `cmd/rota/tui/store_golden_test.go`, `cmd/rota/tui/docs_test.go`, and goldens `store-accounts-{60x20,80x24,120x40}.txt`, `store-login-80x24.txt`, `store-ask-80x24.txt` and `styled-accounts-80x24-{truecolor,256,ansi,nocolor}.txt`
- Modify: `docs/reference.md` (a section, *The TUI*, after *Every command, and every flag it takes*, and Layout rows for `cmd/rota/tui/` and `cmd/rota/loginpage/`), `README.md` (one paragraph and the bare `rota` line), `docs/releases/1.4.0.md` (the TUI bullets)

**Interfaces:**
- Consumes everything above.

- [ ] **Step 1: The store goldens.** A throwaway `ROTA_HOME` is seeded with the design's three accounts (quota readings at fixed times, thresholds, one dead). An in-process `api.New` is served through `httptest` and `LocalHandler`, `NewHTTP` is the Backend, the clock is fixed, `Loc` is UTC, and `Profile` is `Ascii`. Goldens: Accounts at 60×20, 80×24 and 120×40, `n` in the flow at 80×24 with a fake token endpoint, and Ask at 80×24 with a fakecli claude.
- [ ] **Step 2: The styled goldens.** Accounts at 80×24 is rendered through `colorprofile.Writer` at `TrueColor`, `ANSI256` and `ANSI`, and with `NO_COLOR=1` (`Ascii`). The four files differ from one another, and the `NO_COLOR` one holds no `38;` or `48;`.
- [ ] **Step 3: `TestReferenceDocumentsEveryKey`** (`docs_test.go`): every enabled binding's help key of every registered tab and overlay appears in `docs/reference.md`'s *The TUI* section. That keeps the manual to `key.Binding`, the one source.
- [ ] **Step 4: The manual.** Write *The TUI* in the manual's voice, from these facts:
  - when bare `rota` opens it, and when it prints usage, Windows and `TERM=dumb` included
  - the tabs this beta has
  - the Accounts table and what the tick and colours mean
  - every key, from the bindings
  - the add and re-login flow, with its kinds and steps, `o` `c` `w`, and setting it aside
  - the one-time page
  - Ask and keeping a run open
  - that nothing opens a port, and that the local TUI serves the API inside its own process
  - `M` and selecting text
  - `NO_COLOR`
  - the 60×14 floor
  - SSH: the same model, under the key's role

  Add the Layout rows.
- [ ] **Step 5: The notes.** Under `### New in the next beta`, add one bullet for the TUI (bare `rota`, Accounts, Ask, the flow, SSH) and one for its size. `rota login --web` and the API already have theirs, from Tasks 8 and 3.
- [ ] **Step 6: The size.** Build `GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' ./cmd/rota` at `b9fd476` and at this branch's head, in `mktemp -d` worktrees made with `git -C <abs> worktree add`. Put both sizes, to one decimal in MB, in the notes bullet and in the report, so the owner hears the number (spec, "Risks").
- [ ] **Step 7: Verify.**
  - This is a **race** task, and the local CI run passes.
  - `git diff b9fd476 -- docs/releases/` touches only the next-beta section.
  - `git log --format=%B origin/login-by-id.. | grep -ciE 'co-authored|claude\.ai|anthropic|generated'` prints `0`.
- [ ] **Step 8: Commit and push.**
  ```bash
  git add cmd/rota/tui docs/reference.md README.md docs/releases/1.4.0.md
  git commit -m "The TUI is drawn the same from a throwaway store at three sizes and four colour depths, and the manual and the notes describe it key by key"
  git push -u origin phase-3
  ```
