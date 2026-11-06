# Phase 1: the holder and the daemon Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

The spec's "Delivery, in order" puts `run --detach` and `send` into a held session in phase 1 as well, so they are here. `attach`, the chord and SSH belong to phase 2.

**Goal:** Every session the rota daemon runs lives in its own `rota serve hold` process, with a pty, a VT emulator and a scrollback ring, so it outlives the daemon. The daemon (`rota serve start|stop|status|install|uninstall`) re-adopts its holders and serves them over `<store>/terminals/ctl.sock` to the fixed principal `local`. `rota run --detach` and `rota send` drive the sessions. The release is the next free `v1.4.0-beta.N`.

**Architecture:** The holder lives in the program's module (`cmd/rota/hold`) because it uses `x/vt`. It owns one pty, one child CLI, one emulator, one ring, one socket and one flock. The daemon is `api.Server` with `Options.HoldCommand` set. A `heldTerm` backend sits under the existing `termSession` hub, holders are re-adopted from their records, and `Server.LocalHandler` serves `ctl.sock`. The two sides share small root packages with no dependencies: `internal/vtscan` (the bytes of a terminal), `internal/holdwire` (protocol, records, daemon lock), `internal/ring`, `internal/proc` and `internal/termenv`. Foreground `rota serve` keeps `localTerm` and does not change.

**Tech Stack:** Go 1.27 with only the standard library in the root. `github.com/charmbracelet/x/vt` v0.1.0 goes in `cmd/rota` only. The plan also uses the `internal/share` framing, flock, unix sockets, systemd `--user` units and launchd agents. Node 26 with `@xterm/headless` 5.5.0 makes the spike's goldens and runs nowhere else.

**Spec:** `docs/superpowers/specs/2026-11-01-tui-daemon-ssh-design.md`. Read sections 2 to 6, all of section 13, "Delivery, in order", "Public surface" and "Risks".

## Global Constraints

- The root module `github.com/professor93/rota` gains no `require`, no `replace` and no `go.sum`. New third-party code goes only into `cmd/rota/go.mod`, changed only through `sh scripts/module.sh tidy`. Never add `replace`. `sh scripts/module.sh check` prints `module: ok` after every task.
- The program pins `github.com/charmbracelet/x/vt v0.1.0` plus whatever `tidy` adds for it. No other new dependency comes in during this phase.
- `<testenv>` is `env -u ROTA_CLAUDE_HOME -u CLAUDE_CONFIG_DIR -u ROTA_ACCOUNT_ID -u ROTA_ACCOUNT -u ROTA_PROVIDER -u ROTA_TOKEN ROTA_HOME="$(mktemp -d)"`, and it prefixes every `go test`.
- **The local CI run** is these commands, in this order, from the repository root: `test -z "$(gofmt -l .)"`; `for os in linux darwin windows; do GOOS=$os go vet work || echo "vet $os FAILED"; done`; `<testenv> go test -count=1 work`; `sh scripts/module.sh check`. A task marked **race** also runs `<testenv> go test -count=1 -race work`. A task marked **docker** also runs `docker run --rm -v "$PWD":/src -w /src golang:1.27 sh -c 'env -u CLAUDE_CONFIG_DIR ROTA_HOME=$(mktemp -d) go test -count=1 -race work'`. The run passes when gofmt and vet print nothing, every test line is `ok`, and the last line is `module: ok`. `go test ./...` at the root skips the program, so no step uses it.
- Tests never touch the real `~/.rota`, `~/.claude` or the keychain, and they never start a real claude. Fake CLIs come from `internal/fakecli`, never from sh scripts. Use the existing `shortHome(t)` for any store that holds a socket. Process-level tests in `cmd/rota` set `ROTA_AS_RUN=1` in their environment, so every rota the code under test starts (a daemon, a holder) is the test binary acting as rota, as `TestARealSharedTerminalIsTypedAtFromBothEnds` already does. vim and less run only in the capture harness, with `HOME` set to a temp directory and `-u NONE`.
- Linux comes first, then macOS. On Windows, `rota serve hold|start|stop|status|install|uninstall` and `rota run --detach` refuse with `held sessions and the rota daemon run on linux and macOS`. The Windows CI job still builds, and its tests pass or skip. Any file that touches a pty, a signal or a unix socket carries `//go:build linux || darwin` and has a refusing twin for other platforms.
- Natural by default: nothing changes for anyone who does not use the new commands. Foreground `rota serve` has a nil `HoldCommand`. With no records under `<store>/terminals/`, `rota run` and `rota list` print byte-identical output and JSON. No existing test's expectation changes, except where a task moves code and says so.
- No token ever reaches argv, a log, `meta.json`, the holder request or an HTTP answer. The holder's argv is exactly `rota serve hold`.
- Paths: `<store>/terminals/<id>/` (0700) holds `sock` (0600), `lock`, `meta.json` (0600) and `holder.log`. `<store>/terminals/` holds `ctl.sock` (0600), `daemon.lock` and `daemon.pid`. Session ids are `runID()`: 16 hex characters. A socket path longer than 100 bytes is refused by name.
- Values copied from the spec: `[terminal] keep_stopped` defaults to `168h`. `max_sessions` keeps its default of 8. Logs rotate at 20 MiB with four archives. `--detach` from something that is not a terminal takes 120×40. Stopping is HUP, then KILL `termKillAfter` (3 s) later. The systemd unit has `KillMode=process`, and the launchd plist has `AbandonProcessGroup=true`.
- No new verb: `serve` grows, `run` gains `--detach`, and `send` learns held sessions. Every new command, flag, route and config key goes into `docs/reference.md` in the task that adds it, or `docs_test` fails.
- New files stay under about 600 lines. That is the spec's rule for the TUI, applied here too.
- Branches: implement on `phase-1`, cut from `plan-phase-1`. No PR. `main` moves only by fast-forward, and only on the owner's word.
- Commit subjects are one plain sentence of what is now true. The version commit's subject is just `1.4.0-beta.N`. No body, no trailer, no attribution and nothing about AI.

## Review Focus

1. **A holder killed mid-output** (kill -9, the OOM killer). The session must read `interrupted` with its conversation kept. The CLI's claim must be freed only once the CLI is gone. The daemon and every other session must carry on, and nothing may restart by itself. Tests: Task 5 Step 1 and Task 7 Step 2.
2. **Stale files after a crash or a reboot**: `ctl.sock`, a holder's `sock` or `daemon.pid` left behind. A new daemon must start. No client may take a dead file for a daemon, or a held lock with a silent socket for no daemon. A locked session's socket is never deleted. Tests: Task 7 Step 2, Task 9 Step 2 and Task 10 Step 1.
3. **Two daemons at once**: two `serve start` together, `--detach`'s auto-start racing a `serve start`, or the unit starting while a daemon started by hand runs. Exactly one may hold the lock, and the others must say which pid serves and exit. A holder takes a link only from the lock holder. Tests: Task 4 Step 1, Task 10 Step 1 and Task 11 Step 1.
4. **A resize from anywhere**: an attach, typing, the web page's auto-fit, a 0×0 or 5000×5000 request, or a resize while the child is exiting. Only an explicit fit by the keyboard holder may resize. Sizes stay bounded, the emulator follows the new size, and nothing panics. Tests: Task 4 Step 1 and Task 8 Step 1.
5. **The CLI exits while someone is attached or a send is waiting**. Viewers must get the exit frame with the code, and a waiting send must answer `ended` at once. The record must say ended with its exit code, the claim must be free, and the session must stay listed for `keep_stopped`. Tests: Task 4 Step 1, Task 6 Step 1, Task 7 Step 2 and Task 8 Step 1.

## Calls this plan makes where the spec is silent

Each line is the plan's own choice, for the owner to check.

- The holder answers queries with these fixed values. DA1 `ESC[?62;22c`, DA2 `ESC[>1;10;0c`, XTVERSION `ESC P>|xterm(390) ESC\` and OSC 10/11 with a dark fg and bg. XTGETTCAP answers only `TN`, `Co` and `colors`. DECRQM answers only the modes that are tracked. The kitty keyboard query goes unanswered, so CLIs stay on legacy keys that every viewer can send. DSR 6n gets the emulator's cursor, or 1;1 when there is no emulator.
- The emulator's own replies are read and thrown away. x/vt writes them into an unbuffered pipe, and an unread pipe blocks its `Write`.
- Modes come from `vtscan`'s own tracker, not from x/vt, so they survive an emulator panic.
- A `Screen` repaints the visible screen only. Scrollback is not resent, and a repaint over 1 MiB falls back to ring replay.
- An emulator that falls more than a ring behind is retired to the fallback for that holder's life.
- The daemon chooses the account, as `planTerminal` does today. The holder runs `Launchable` and `Prepare`.
- `run --detach` computes `--remote-control`'s extra arguments on the client. `remoteControl` does not move until a later phase needs it.
- `termRequest.env` is accepted only from the `local` principal.
- Several new rules apply to held sessions only, so foreground `rota serve` does not change: stripping replies, explicit size via `"fit":true`, the keyboard following the person, and repainting a slow viewer.
- `local_user` must name a `[[users]]` entry that has role control.
- Additions to `meta.json` beyond the spec: `child_pid`, `child_start`, `kind`, `version`, `conversation_name`, `conversation_cwd` and `token_until`. The holder computes `token_until`, since only it sees the environment `Prepare` built.
- Showing each session's credential kind (section 13, "Files and reach") waits for phase 4's Sessions tab.
- A registry file counts only when its `procStart` (format `Mon Jan _2 15:04:05 2006`, local time, as seen in Claude Code 2.1.314 to 2.1.319) is within 1 s of the child's start.
- Send readiness has three rules. With a registry, the session is ready when `status` is `idle` and `waitingFor` is null. Without one, it is ready after 1.5 s with no output. The wait times out after 30 s. Enter goes 100 ms after the text. Paste markers are removed from the text.
- `status` is `quiet` after 30 s with no output.
- An interrupted session's age is measured from `meta.json`'s mtime, which the holder touches every hour.
- The daemon's files are `daemon.lock` and `daemon.pid` under `terminals/`. The log goes to `<store>/daemon.log`, unless `[server] log_file` names another file.
- `serve start` is idempotent and exits 0 when a daemon already runs. `serve status` exits 3 when no daemon runs. `serve stop` with no daemon exits 0. `--ctl-only` and `--yes` are new flags, `--force` is new on `send`, and a successful `send` prints nothing.
- The unit is named `rota.service` and the launchd label is `com.github.professor93.rota`. A store other than the default adds `-<8 hex>` to both. `install` refuses while a daemon started by hand runs.
- The daemon does not open `share.sock` unless `routes.terminal` is on, as today.
- `rota list --sessions` gains a `Held sessions:` section only when records exist.

## File Structure

| Path | Responsibility |
|---|---|
| `internal/vtscan/` (create) | Scans output for queries and their fixed answers, modes, title, bell and ground boundaries. Strips replies from input |
| `internal/ring/` (create) | `outRing` moved here, so that holder and daemon share one ring with absolute offsets |
| `internal/proc/` (extend) | `StartTime`, `BootID` and `PeerPID` |
| `internal/termenv/` (create) | `termEnv` moved here, plus the scrub list from section 13 |
| `internal/holdwire/` (create) | The holder protocol, request and ready line, `meta.json`, records scan, holder lock, daemon claim and paths |
| `internal/fakecli/` (extend) | New lines for the tty fake: `print <hex>`, `env NAME`, `envhas <text>` and `registry <status> [waiting]`. `got:` escapes control bytes |
| `cmd/rota/hold/` (create) | `Screen` (x/vt behind `recover()`), `Session` (pump, writer, link, send) and `Main` (`rota serve hold`) |
| `cmd/rota/client/` (create) | The `ctl.sock` client that `run --detach`, `send`, and later the TUI and SSH use |
| `cmd/rota/{held,daemon,ctlsock,unit,detach}*.go` (create) | `serve hold|start|stop|status|install|uninstall`, the `ctl.sock` listener, `--detach`, held rows in `list` |
| `api/held.go`, `api/adopt.go` (create) | `heldTerm`, spawning a holder, re-adoption, stopped records, foreign holders |
| `api/{terminal,terminals,server,principal,ws,config}.go` | The `detach` backend method, `HoldCommand`, `LocalHandler`, the `local` principal, the send route, `keep_stopped` and `local_user` |
| `scripts/module.sh` | Rule 5 seeds its empty module cache with the program's third-party modules, never with the root |
| `scripts/vt/golden.mjs` (create) | Goldens from `@xterm/headless` 5.5.0 for the spike |
| `docs/reference.md`, `README.md`, `docs/releases/1.4.0.md`, `lib/httpx.go:21`, `wire/wire.go:424` | The manual, the dependency sentence, the notes and the version |

---

### Task 1: The bytes of a terminal (`internal/vtscan`)

This is the scanner the spike measures modes with, so it comes first. Task 2 is the spike the spec opens phase 1 with.

**Files:**
- Create: `internal/vtscan/scan.go`, `internal/vtscan/reply.go`, `internal/vtscan/palette.go`
- Test: `internal/vtscan/scan_test.go`, `internal/vtscan/reply_test.go`

**Interfaces:**
- Produces, in the root and on the standard library alone:
  - `type Modes struct { AltScreen, CursorKeys, BracketedPaste, Focus, CursorHidden bool; Mouse, MouseEncoding, Kitty int }`. The JSON tags are `alt_screen`, `cursor_keys`, `bracketed_paste`, `focus`, `cursor_hidden`, `mouse` (0, 9, 1000, 1002 or 1003), `mouse_encoding` (0, 1005, 1006 or 1015) and `kitty`, all `omitempty`.
  - `func (m Modes) Set() []byte` gives the sequences that take a reset terminal to `m`, alternate screen excluded.
  - `func ResetAll() []byte` turns every tracked mode off, shows the cursor and leaves the alternate screen. Phase 2's detach uses it.
  - `type Scanner`, `func NewScanner() *Scanner`, `func (s *Scanner) Feed(p []byte) Result`, `func (s *Scanner) Modes() Modes` and `func (s *Scanner) Title() string`.
  - `type Result struct { Replies [][]byte; Ground int; CPR []CPR; Events []Event; ModesChanged bool }`. `Ground` is how many bytes of `p` end between sequences and on a whole rune, or -1 if no such point falls inside `p`.
  - `type CPR struct { End int; Private bool }`. It marks a `CSI 6 n` (or `CSI ? 6 n`) ending at byte `End` of `p`, which is left for the emulator to answer.
  - `func CPRReply(row, col int, private bool) []byte`.
  - `type Event struct { Kind EventKind; Text string }`, with the kinds `EventTitle`, `EventBell` and `EventNotify`.
  - `func CleanTitle(s string) string` strips one leading spinner or idle glyph and its space: `✳ ✢ ✶ ✻ ✽ ·` and U+2800–U+28FF. Phase 5's attention signal reads the glyph before it is stripped.
  - `type ReplyFilter` and `func (f *ReplyFilter) Strip(p []byte) []byte`, one per viewer connection.

- [ ] **Step 1: Write the failing tests.**
  - `TestAnswersAreFixed`, as a table of query and the exact reply. `ESC[c` and `ESC[0c` answer `ESC[?62;22c`. `ESC[>c` answers `ESC[>1;10;0c`. `ESC[>q` answers `ESC P>|xterm(390) ESC\`. `ESC[5n` answers `ESC[0n`. `ESC]10;? BEL` answers `ESC]10;rgb:d0d0/d0d0/d0d0 BEL`, and the ST form is answered with ST. `ESC]11;? ESC\` answers `ESC]11;rgb:0000/0000/0000 ESC\`. `ESC]4;1;? BEL` answers `ESC]4;1;rgb:cdcd/0000/0000 BEL`, from xterm's palette. `ESC P+q544e ESC\` answers `ESC P1+r544e=787465726d2d323536636f6c6f72 ESC\`. `ESC P+q5a5a ESC\` answers `ESC P0+r5a5a ESC\`. `ESC[?2004$p` answers `ESC[?2004;2$y`, then `ESC[?2004;1$y` after `ESC[?2004h`. `ESC[?9999$p` answers `ESC[?9999;0$y`. A query cut across two `Feed` calls is answered once, after the second.
  - `TestKittyQueryGoesUnanswered`: `ESC[?u` gets no reply.
  - `TestCPRIsLeftToTheEmulator`: `abc ESC[6n` gives no reply and `CPR == []CPR{{End: 7}}`.
  - `TestModesFollowTheStream`: `ESC[?1049h ESC[?1h ESC[?2004h ESC[?1002h ESC[?1006h ESC[?1004h ESC[=5;1u ESC[?25l` gives `Modes{AltScreen, CursorKeys, BracketedPaste, Focus, CursorHidden: true, Mouse: 1002, MouseEncoding: 1006, Kitty: 5}`. `ESC c` returns to `Modes{}`. `ESC[<u` after `ESC[>3u` brings back the previous flags.
  - `TestGroundIsBetweenSequencesAndWholeRunes`: `abc ESC[3` gives `Ground == 3`. `ok\xe2\x9c` gives 2. `\xb3` fed next gives 1.
  - `TestTitleBellAndNotify`: `ESC]0;work BEL` gives `EventTitle "work"` and no bell. A lone `BEL` gives `EventBell`. `ESC]9;done ESC\` gives `EventNotify "done"`. `ESC]777;notify;T;B BEL` gives `EventNotify "T: B"`. `CleanTitle("✳ Fix tests") == "Fix tests"`.
  - `FuzzScanner`, seeded with every query above. It never panics, and `Ground` stays in `[-1, len(p)]`.
  - `TestRepliesAreStrippedAndKeysAreNot`, as a table of input and output:
    - `x ESC[?62;22c y` → `xy`
    - `ESC[24;80R` → empty
    - `ESC[1;2R` (Shift+F3) kept
    - `ESC[A` kept
    - a lone `ESC` kept
    - `ESC]11;rgb:0000/0000/0000 ESC\` → empty
    - `ESC[?2004;1$y` → empty
    - `ESC[?1u` → empty
    - `ESC P>|xterm(390) ESC\` → empty
    - `ESC P1+r544e=… ESC\` → empty
    - an OSC reply split across two `Strip` calls → empty both times
    - a DCS left open past 4 KiB → passed through
- [ ] **Step 2: Run them.** `go test -count=1 ./internal/vtscan/` fails: the package does not exist.
- [ ] **Step 3: Implement.** Use one incremental tokenizer for both directions, with states ground, ESC, CSI, OSC, DCS and their string terminators. It keeps at most 4 KiB of an open sequence. `Strip` filters each frame on its own, except that a DCS or OSC begun and not ended is held into the next frame. A trailing lone `ESC`, or a CSI cut off at the end, passes through as typed, so the Escape key is never delayed. CPR-shaped `CSI Pn;Pn R` is stripped, except exactly `CSI 1;<2..16> R`.
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add internal/vtscan
  git commit -m "internal/vtscan answers a CLI's terminal queries with fixed values, tracks its modes, title and bell, and strips the replies a viewer's own terminal sends back"
  ```

### Task 2: The emulator spike, and the screen behind recover()

**Files:**
- Create: `cmd/rota/hold/screen.go`, `cmd/rota/hold/screen_test.go`, `cmd/rota/hold/capture_test.go`, `cmd/rota/hold/testdata/vt/` (captures, goldens, `sample.go`, `sample.ansi`), `scripts/vt/golden.mjs`, `scripts/vt/README`
- Modify: `internal/fakecli/fakecli.go` (`print <hex>`, and escaping in `got:`), `scripts/module.sh` (rule 5), `cmd/rota/go.mod` and `cmd/rota/go.sum` (through `tidy`), `README.md:11` and `docs/reference.md:44-47` (the dependency sentence)
- Test: the tests below

**Interfaces:**
- Consumes `vtscan.Modes` from Task 1.
- Produces, in package `hold`:
  - `type Screen`, `func NewScreen(cols, rows int) *Screen` and `func (s *Screen) Write(p []byte)`. `Write` takes whole sequences only, and its caller cuts at `vtscan`'s `Ground`.
  - `func (s *Screen) Resize(cols, rows int)` and `func (s *Screen) Cursor() (row, col int)`, both 1-based.
  - `func (s *Screen) Repaint(m vtscan.Modes) ([]byte, bool)`, `func (s *Screen) OK() bool`, `func (s *Screen) Close()` and `const maxRepaint = share.MaxFrame - 4096`.
  - Later phases: the TUI preview in phase 4 reads this same `Repaint` through the daemon, and phase 5's titles come from `vtscan`, not from here.

- [ ] **Step 1: The fake learns to print anything.** In `fakecli`'s tty mode, the line `print <hex>` writes the decoded bytes with no newline. `got:` now writes a byte below 0x20, or 0x7f, as `\xHH`. Printable input prints as before, so existing tests are unchanged.
- [ ] **Step 2: The capture harness.** `TestCaptureVT` is skipped unless `ROTA_VT_CAPTURE=1`. It runs each program on `internal/pty` at 80×24, with `TERM=xterm-256color`, `HOME=<tmp>`, `LESS=` and `LESSHISTFILE=-`. It writes the master's bytes to `testdata/vt/<name>.bin`:
  - `vim.bin` runs `vim -u NONE -N -i NONE -n -c 'syntax on' testdata/vt/sample.go` and stops before quitting, after `/func\r` and `:set number\r`.
  - `vim-quit.bin` is the same, run on through `:q!\r`.
  - `less.bin` runs `less -R testdata/vt/sample.ansi` and stops after `G` then `g`.
  - `fake.bin` is the fake tty CLI driven with `print` lines. Together they set alt screen, cursor keys, bracketed paste, mouse 1002 with 1006, focus and kitty flags 1. They also set a title, ring a bell, write 256-colour, truecolour, bold, italic, underline and inverse cells, write a wide rune, move the cursor and hide it.

  Run it with `ROTA_VT_CAPTURE=1 go test -count=1 -run TestCaptureVT ./cmd/rota/hold/` and commit the four `.bin` files. A real session's recording joins them only if the owner supplies one.
- [ ] **Step 3: The goldens.** `scripts/vt/golden.mjs <bin> <cols> <rows>` writes JSON to stdout:
  - `rows`: each row's text with trailing blanks trimmed.
  - `cells`: every cell that is not blank or has a non-default background, as `{x, y, ch, fg, bg, bold, italic, underline, inverse, dim}`. A colour is `"default"`, `"p<n>"` for the palette, or `"#rrggbb"`.
  - `cursor`: `{x, y, hidden}`, where `hidden` reads `term._core.coreService.isCursorHidden`.
  - `modes`: `{alt, cursorKeys, bracketedPaste, mouse, focus}`, from `term.modes` and `buffer.active.type`.

  `scripts/vt/README` says how to fetch `@xterm/headless` 5.5.0 into a `mktemp -d` directory: check the tarball against the registry's `dist.integrity` first, as `api/assets/xterm/README` does, and never commit it. Write `<name>.xterm.json` for each capture. xterm 5.5 cannot report mouse encoding or kitty flags, so for `fake.bin` those two are checked against the values its script set.
- [ ] **Step 4: Write the failing tests.**
  - `TestXVTMatchesXterm`, per capture. `NewScreen(80, 24)` is fed the bin, cut at `vtscan` ground points. `doc(screen)` (test-only, the same JSON shape) equals the golden in rows, cells and cursor.
  - `TestModesMatchXterm`, per capture: `Scanner.Modes()` after the bin equals the golden's `modes`.
  - `TestRepaintReproducesTheScreen`, per capture:
    - `Repaint(modes)` equals the committed `<name>.repaint.bin`.
    - A fresh `Screen` fed that repaint gives the same `doc`.
    - `<name>.repaint.xterm.json`, made by `golden.mjs` from the repaint bin, equals `<name>.xterm.json` in rows, cells, cursor and modes.
  - `TestAPanickingEmulatorLeavesReplay`: the test seam `var vtWrite = (*vt.Emulator).Write` is set to panic. `Write` returns, `OK()` is false, `Repaint` returns `nil, false`, and later `Write` calls are cheap no-ops.
  - `TestQueryRepliesNeverBlockTheEmulator`: 10,000 copies of `ESC[c` written in one call return within 2 s.
- [ ] **Step 5: Run them.** They fail: `Screen` does not exist.
- [ ] **Step 6: Implement `Screen`.**
  - Wrap `vt.NewEmulator`. A goroutine reads `Emulator.Read` until `Close` and throws everything away, because the holder is the only thing that answers. Every call into the emulator runs under one mutex and `recover()`.
  - `Repaint` writes, in this order:
    1. `ResetAll()` without its alt-screen part.
    2. `ESC[?1049h` or `ESC[?1049l`, as the session is.
    3. `ESC[0m ESC[H ESC[2J`.
    4. For each row, `ESC[<y>;1H`, then that row's styled cells, then `ESC[0m`.
    5. `m.Set()`.
    6. `ESC[<row>;<col>H`.
    7. `ESC[?25l` or `ESC[?25h`.

    Anything longer than `maxRepaint` is `false`.
- [ ] **Step 7: Rule 5 and the module.** Change rule 5 of `check`. Before it builds, it seeds `$tmp/modcache` from outside any module, with `GOMODCACHE=$tmp/modcache GOFLAGS=-modcacherw GOSUMDB=off GOPROXY="file://$(go env GOMODCACHE)/cache/download,$(go env GOPROXY)" go mod download <path>@<version>` for every `Require` in `go mod edit -json cmd/rota/go.mod` except `github.com/professor93/rota`. The build then runs as before with `GOPROXY=off`, so fetching the root still fails it, and `go.sum` still checks every hash. Run `sh scripts/module.sh tidy`. `git diff go.mod` at the root prints nothing.
- [ ] **Step 8: The bar (the spec's gate).** All five tests pass. If any capture still differs after fixes on rota's side alone (never a fork of x/vt), stop. Report the failing captures and their JSON diff to the owner, who chooses between the fallback in spec section 3 and another emulator. Under the fallback, `NewScreen` returns a `Screen` whose `OK()` is always false and the holder never advertises `screen`. Every later task stands as written.
- [ ] **Step 9: Docs.** `README.md:11` becomes ``and the SDK and the server packages have **zero third-party dependencies**; the `rota` program uses x/vt.`` At `docs/reference.md:44-47`, the sentence that ends `never reaches a program that imports them.` gains: `It uses github.com/charmbracelet/x/vt to keep the screen of each held session.`
- [ ] **Step 10: Verify.** This is a **race** and **docker** task, and the local CI run passes. `go list -m all` at the root prints only `github.com/professor93/rota`.
- [ ] **Step 11: Commit.**
  ```bash
  git add internal/fakecli cmd/rota/hold cmd/rota/go.mod cmd/rota/go.sum scripts README.md docs/reference.md
  git commit -m "The holder's screen is x/vt behind recover(), and on vim, less and the fake CLI it matches xterm cell for cell, mode for mode, and in what its repaint draws"
  ```

### Task 3: The shared contract (`internal/holdwire`, `internal/ring`, `internal/proc`)

**Files:**
- Create: `internal/holdwire/{wire.go,request.go,meta.go,records.go,daemon.go,paths.go}` and their tests, `internal/ring/ring.go` with `ring_test.go`, and `internal/proc/{start_linux.go,start_darwin.go,start_other.go,peer_linux.go,peer_darwin.go,peer_other.go}`
- Modify: `api/terminal.go:146-213` (`outRing` becomes `ring.Ring`) and `api/terminals.go:319-335` (`loginUntil` becomes `holdwire.LoginUntil`). The moved code changes no behaviour.
- Test: the tests below

**Interfaces:**
- `package ring`: `type Ring`, `func New(keep int) *Ring`, `func NewAt(keep int, end int64) *Ring`, `func (r *Ring) Write(p []byte)`, `func (r *Ring) Read(since int64) ([]byte, int64)`, `func (r *Ring) Oldest() int64` and `func (r *Ring) End() int64`.
- `package proc`: `func StartTime(pid int) (time.Time, error)`, `func BootID() (string, error)` and `func PeerPID(c *net.UnixConn) (int, error)`.
  - On Linux, start time is `/proc/<pid>/stat` field 22 with the boot time, the boot id is `/proc/sys/kernel/random/boot_id`, and the peer comes from `SO_PEERCRED`.
  - On darwin, start time is the first `timeval` of `kern.proc.pid.<pid>` (second resolution), the boot id is `kern.boottime` formatted, and the peer comes from `getsockopt(fd, 0 /*SOL_LOCAL*/, 0x002 /*LOCAL_PEERPID*/)`.
  - On other platforms, each returns an error.
- `package holdwire`:
  - `const Major = 1` and `var Majors = []int{1}`. The features are `FeatScreen = "screen"`, `FeatState = "state"`, `FeatSend = "send"` and `FeatStop = "stop"`.
  - Frames are `internal/share`'s `Conn`, with their own kind bytes: `Hello=1 Welcome=2 Refused=3 Output=4 Input=5 InputRefused=6 Resize=7 Resized=8 Exit=9 Since=10 ScreenAsk=11 Screen=12 State=13 Stop=14 Send=15 Sent=16 Ping=17 Pong=18`. Output, input, resize, exit, since and screen are the core set, and their meaning never changes within a major.
  - The messages are `HelloMsg{Majors []int; Features []string}`, `WelcomeMsg{Major int; Features []string; Version string; Oldest, End int64; Cols, Rows uint16}`, `RefusedMsg{Message string}`, `SinceMsg{Offset int64}`, `SizeMsg{Cols, Rows uint16}`, `ExitMsg{Code int}`, `ScreenAskMsg{Seq uint64}`, `ScreenHead{Seq uint64; OK bool; Offset int64; Cols, Rows uint16; Modes vtscan.Modes}`, `StateMsg{Title string; Bells uint64; Modes vtscan.Modes; Registry *Registry}`, `Registry{SessionID, Name, Cwd, Status, WaitingFor, Kind string; UpdatedAt int64}`, `SendMsg{Seq uint64; Text string; Force bool; WaitMS int}`, `SentMsg{Seq uint64; Code, Message string}` and `InputRefusedMsg{Seq uint64; Message string}`. `Code` is one of `SentOK=""`, `SentWaiting="waiting"`, `SentTimeout="timeout"` or `SentEnded="ended"`.
  - Payload helpers: `PutOutput(off int64, p []byte) []byte` and `Output(b []byte) (int64, []byte, error)` use an 8-byte big-endian offset. `PutInput` and `Input` do the same with a sequence number. `PutScreen(h ScreenHead, repaint []byte) ([]byte, error)` and `ScreenOf(b []byte) (ScreenHead, []byte, error)` write a compact JSON header, then `\n`, then the raw bytes.
  - `func Agree(ours, theirs []int) (int, bool)` returns the highest major both ends share.
  - `type Request struct { ID, Kind string; Account int; Args []string; Cwd, Name string; Cols, Rows uint16; Env []string; Scrollback int }`. The JSON keys are `id`, `kind` (`account` or `shell`), `account`, `args`, `cwd`, `name`, `cols`, `rows`, `env` and `scrollback`.
  - `type Ready struct { OK bool; PID int; TokenUntil time.Time; Error, Kind string }`. `func LoginUntil(a *rota.Account, env []string) time.Time` is `api.loginUntil` moved. `func Classify(err error) string` returns `KindBusy="busy"`, `KindReauth="reauth"` (reauth and dead token), `KindNoAccount="noaccount"`, `KindUnsupported="unsupported"`, `KindOutside="outside"` or `KindOther="other"`.
  - `type Meta struct`, with the JSON keys `id`, `name`, `kind`, `account`, `provider`, `holder_pid`, `holder_start`, `boot_id`, `child_pid`, `child_start`, `protocol`, `version`, `cwd`, `args`, `conversation`, `conversation_name`, `conversation_cwd`, `token_until`, `started`, and `ended` and `exit`, which stay absent until known. Times are RFC 3339 with nanoseconds.
  - `func ReadMeta(dir string) (*Meta, error)`, `func WriteMeta(dir string, m *Meta) error` (a temp file, then a rename, at 0600) and `func StoredArgs(provider string, args []string) []string`.
  - Paths: `func Dir(terminals, id string) string`; `SockPath`, `LockPath`, `MetaPath` and `LogPath`, each `(dir string) string`; `CtlSock`, `DaemonLock` and `DaemonPIDFile`, each `(terminals string) string`; `const MaxSockPath = 100`; and `func SockFits(path string) error`.
  - Records: `type State string`, with `StateLive`, `StateEnded`, `StateInterrupted`, `StateUnreadable` and `StateBroken`. `type Record struct { ID, Dir string; Meta *Meta; State State; Stopped time.Time; Err error }`.
  - `func Scan(terminals string) []Record` returns one record per directory whose name is a 16-hex id, newest first, and never fails as a whole. `func Holding(terminals string, account int) *Record` returns a live record on that account.
  - Locks: `func Lock(dir string) (release func(), err error)` is the holder's lock for life. `func Locked(path string) bool` is a non-blocking shared probe, let go at once.
  - The daemon claim: `type Daemon struct { PID int; Version string; Started, ProcStart time.Time; BootID string }`, `func ClaimDaemon(terminals, version string) (release func(), err error)`, `type DaemonRunning struct{ PID int }`, `func RunningDaemon(terminals string) (*Daemon, bool)` and `func SameProcess(pid int, start time.Time, boot string) bool`. `SameProcess` allows ±1 s and is checked before any signal.

- [ ] **Step 1: Write the failing tests.**
  - `TestFramesRoundTrip`: Output keeps its offset, Input keeps its sequence, and Screen keeps a header plus repaint bytes that contain `\n` and `0x00`.
  - `TestAgreePicksTheHighestCommonMajor`: `({1},{1,2})` gives 1, and `({2},{1})` gives false.
  - `TestStoredArgsDropResumeAndSessionFlags`:
    - claude `["--model","opus","--resume","abc","-c","--session-id=x","--fork-session","--permission-mode","plan","-r","d"]` → `["--model","opus","--permission-mode","plan"]`
    - a bare `--resume` before another flag is dropped alone
    - codex `["resume","abc","--model","o3"]` → `["--model","o3"]`
    - codex `["resume","--last"]` → `[]`
  - `TestMetaIsPrivateAndAtomic`: the file is 0600. A write that fails in its rename seam leaves the old file intact.
  - `TestScanTellsEveryStateApart`: five dirs.
    - live: locked with `Lock`
    - ended: has `exit`, unlocked
    - interrupted: no `exit`, unlocked
    - unreadable: `protocol: 9`, locked
    - broken: meta is not JSON

    All five come back with their states. `ctl.sock`, `daemon.lock` and a dir named `nothex` are ignored. An interrupted record's `Stopped` is `meta.json`'s mtime, and an ended one's is `ended`.
  - `TestTwoDaemonsCannotClaimOneStore`: a second `ClaimDaemon` returns `DaemonRunning{PID: os.Getpid()}`. After release, a claim succeeds. `RunningDaemon` is true only while one is held, and `daemon.pid` names this pid, version, boot id and start time.
  - `TestLockedNeverTakesTheLock`: `Locked` on a free path is false, and `Lock` afterwards succeeds.
  - `TestSameProcessIsAboutThisProcess`: true for this pid with its own `StartTime` and `BootID`. False with start+5 s, and false with another boot id.
  - `TestPeerPIDOfAUnixSocket`: dialling a listener in `os.MkdirTemp("", "rota")` gives `PeerPID == os.Getpid()`.
  - `TestRingStartsWhereItIsTold`: `NewAt(8, 100)` written with 4 bytes gives `Oldest 100` and `End 104`, and `Read(50)` starts at 100. The api's existing ring tests, `TestReplayFromAnOffsetAndTheGapWhenItIsTooOld` among them, pass unchanged.
  - `TestSockFits`: a 101-byte path is refused by a message that names it.
- [ ] **Step 2: Run them.** They fail: the packages do not exist.
- [ ] **Step 3: Implement**, moving `outRing` and its comment into `internal/ring`. `api` calls `ring.New(s.opts.Terminal.Scrollback)`.
- [ ] **Step 4: Verify.** This is a **race** and **docker** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add internal/holdwire internal/ring internal/proc api/terminal.go api/terminals.go
  git commit -m "Holder and daemon share one contract in internal/holdwire: a versioned protocol, private records whose state a flock decides, and a daemon claim only one process per store can hold"
  ```

### Task 4: A holder's session: pump, writer and link

**Files:**
- Create: `cmd/rota/hold/session.go`, `cmd/rota/hold/link.go`, `cmd/rota/hold/writer.go` and `cmd/rota/hold/send.go`, all `//go:build linux || darwin`
- Test: `cmd/rota/hold/session_test.go`

**Interfaces:**
- Consumes `Screen` (Task 2), `vtscan.Scanner` (Task 1), `ring.Ring`, `holdwire.*` and `proc.PeerPID` (Task 3).
- Produces:
  - `type Child interface { Read([]byte) (int, error); Write([]byte) (int, error); Resize(cols, rows uint16) error; Signal(os.Signal) error; Wait() int; Pid() int; Close() error }`.
  - `type Config struct { ID, Terminals string; Cols, Rows uint16; Scrollback int; Log func(msg string, args ...any); OnExit func(code int) }`.
  - `func Start(cfg Config, c Child) *Session`, and the methods `Serve(ln *net.UnixListener)`, `Stop()`, `SetRegistry(r *holdwire.Registry)`, `Done() <-chan struct{}` and `Code() int`.
  - The test knobs `var StopAfter = 3 * time.Second`, `var Beat = 10 * time.Second` and `var InputQueue = 1 << 20`.

The decisions, which the tests pin:
- **Pump.** It reads 32 KiB at a time, then runs `scanner.Feed`, sends replies to the writer ahead of everything and never refuses them, writes the ring, and wakes the other goroutines. It never waits on the link or on the emulator.
- **Emulator goroutine.** It reads the ring by offset up to the last ground point. It cuts at each `CPR.End` and answers with `CPRReply(screen.Cursor())`, or 1;1 when `!OK()`. If the ring overtakes it, it retires the screen with one log line.
- **Link writer.** It sends `Output` from the ring by offset, so a slow daemon costs a gap, which shows as a jump in the offset, never the child.
- **Linking.** The peer pid must equal `RunningDaemon(cfg.Terminals).PID` while that lock is held. Anyone else gets `Refused{"only the rota daemon serving this store may link to a session"}`. A valid new link replaces the old one, and the old one is closed.
- **Handshake.** `Hello` must arrive within 10 s, then `Agree`, then `Welcome`, then a `State`. Output starts at the daemon's `Since`, from `max(Offset, Oldest)`.
- **Input.** It goes through the writer queue, which holds `InputQueue` bytes. A full queue answers `InputRefused{seq, "the session is not taking keystrokes this fast; that input was not typed"}`.
- **Resize.** `child.Resize`, then `screen.Resize`, then `Resized`. After the child is gone it answers with no frame and does not panic.
- **ScreenAsk.** Answered with `Screen`, where `OK` is false when there is no emulator or the repaint is too big.
- **State.** Sent on a change of title, bell, modes or registry, at most four times a second.
- **Stopping.** A `Stop` frame, SIGHUP or SIGTERM sends the child SIGHUP, then SIGKILL after `StopAfter`.
- **Send.** If a registry is known, the session is ready when `Status == "idle"` and `WaitingFor == ""`. A `WaitingFor` answers `SentWaiting` unless `Force`. With no registry, the session is ready after 1.5 s with no output. The wait times out after `WaitMS` (default 30 s) with `SentTimeout`. When ready, the text has `ESC[200~` and `ESC[201~` removed, is wrapped in them if `BracketedPaste` is on, and is written. `\r` follows 100 ms later. If the child exits while a send waits, the answer is `SentEnded` at once.
- **Exit.** Drain the last output, send `Exit{code}` within 2 s, call `OnExit(code)`, then close `Done`.
- **Other frames.** Unknown kinds are ignored. `Ping` gets `Pong`. Pings go out every `Beat`, and a link dies after 3×`Beat` of silence.

- [ ] **Step 1: Write the failing tests.** They use a pipe-backed `fakeChild` that records signals, and a holdwire client that the test process connects after `ClaimDaemon`.
  - `TestALinkGetsEverythingSinceItsOffset`: the child prints three chunks. `Since(0)` gives offsets 0 and up, and a second link with `Since(5)` starts at 5.
  - `TestASlowLinkCostsAGapNotTheChild`: with `Scrollback` 1 KiB, the child prints 64 KiB to a link that does not read and finishes anyway. Reading afterwards shows an offset jump.
  - `TestOnlyTheDaemonMayLink` (Review Focus 3): without a claim the link is `Refused`. With the test's claim it gets `Welcome`, and a second valid link closes the first, which sees EOF.
  - `TestQueriesAreAnsweredWithNobodyAttached`: the child prints `ESC[c` and reads `ESC[?62;22c` with no link at all.
  - `TestCPRAnswersWhereTheCursorIs`: `abc ESC[6n` makes the child read `ESC[1;4R`.
  - `TestAFullInputQueueIsRefusedOutLoud`: with `InputQueue` 8 and a child that never reads, a 16-byte Input gets `InputRefused` with its sequence number.
  - `TestScreenStandsForAnOffsetBetweenSequences`: the child prints `ESC[?1049h ESC[2;3Hhi ESC[3`. The Screen's `Offset` is the length minus 3, and its modes have `AltScreen`. Once the rest arrives, the next Screen has a later offset.
  - `TestResizeIsBoundedAndSafeAfterTheEnd` (Review Focus 4): `Resize{5000,5000}` reaches the child as 1000×1000 and the Screen is 1000×1000. A resize after the child exits answers with no frame and does not panic.
  - `TestSendWaitsPastesAndPressesEnter`: after `ESC[?2004h`, `Send{Text:"hi\x1b[201~x"}` makes the child read `ESC[200~hix ESC[201~`, then `\r` at least 100 ms later, then `Sent` with an empty code.
  - `TestSendRefusesAWaitingSession`: after `SetRegistry(&Registry{Status:"waiting",WaitingFor:"permission"})` the answer is `SentWaiting`. With `Force` the text is typed.
  - `TestAPendingSendEndsWithTheChild` (Review Focus 5): a `Send` waits on a busy registry, the child exits, and `SentEnded` arrives before `Exit`.
  - `TestStopHangsUpThenKills`: a child that ignores HUP gets KILL after `StopAfter`, and `Exit` carries its code.
  - `TestTheExitReachesTheLinkLast`: the child prints `bye` and exits 3, and the client sees the Output `bye` before `Exit{3}`.
  - `TestUnknownFramesAreIgnored`: kind 99 leaves the link up, and `Ping` gets `Pong`.
- [ ] **Step 2: Run them.** They fail: `Start` does not exist.
- [ ] **Step 3: Implement** the four files. Keep every goroutine's loop under `recover()`, and log a panic to `cfg.Log`.
- [ ] **Step 4: Verify.** This is a **race** and **docker** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota/hold
  git commit -m "A holder's session answers its CLI's queries itself, keeps its screen and ring without waiting on anyone, links only to the daemon holding the store's lock, and types a message only when the session is ready"
  ```

### Task 5: `rota serve hold`

**Files:**
- Create: `cmd/rota/hold/main.go`, `cmd/rota/hold/child_unix.go`, `cmd/rota/hold/registry.go`, `cmd/rota/hold/main_other.go`, `cmd/rota/hold/testdata/registry-2.1.318.json`, `cmd/rota/held.go` (`serveHold`) and `internal/termenv/termenv.go`
- Modify: `cmd/rota/main.go:2088-2098` (`case "hold"` in serve's switch), `api/terminals.go:301-317` (`termEnv` becomes `termenv.Env`; the moved code changes no behaviour), `internal/fakecli/fakecli.go` (the tty lines `env NAME`, `envhas <text>` and `registry <status> [waiting]`) and `docs/reference.md` (a `rota serve hold` row)
- Test: `cmd/rota/hold_test.go`

**Interfaces:**
- `func Main(in io.Reader, out io.Writer, open func() (*store.Store, error)) int` in `hold`.
- `func Env(env []string) []string` and `func Scrub(env []string) []string` in `termenv`. `var Scrubbed = []string{"TMUX", "TMUX_PANE", "STY", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERM_SESSION_ID", "ITERM_SESSION_ID", "KITTY_WINDOW_ID", "KITTY_PID", "WEZTERM_PANE", "WEZTERM_UNIX_SOCKET", "ALACRITTY_WINDOW_ID", "ALACRITTY_SOCKET", "WT_SESSION", "WINDOWID", "VTE_VERSION", "KONSOLE_VERSION", "COLUMNS", "LINES", "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT", "CODEX_THREAD_ID"}`.
- The fake's new tty lines. `env NAME` prints `env NAME=<value>` or `env NAME unset`. `envhas <text>` prints `envhas yes` if any variable's name or value contains the text, else `envhas no`. `registry <status> [waiting]` writes `$CLAUDE_CONFIG_DIR/sessions/<pid>.json`, shaped like a Claude Code 2.1.318 file: `pid`, `procStart` (its own start, ANSIC, local time), `sessionId` `fake-<pid>`, `cwd`, `name` `fake`, `status`, `waitingFor` (null, or the second word), `kind` `interactive`, `startedAt`, `updatedAt` and `version` `2.1.318`.

`Main`, in this order:
1. If standard input is a terminal, print `rota serve hold is started by the rota daemon (rota serve start), never by hand` and exit 2. If this process does not lead its own session, refuse the same way.
2. Read one `Request` of at most 1 MiB. If `Env` is set, clear the environment, set `Env`, and put back `ROTA_HOME` as the process started with it.
3. Open the store, find `TerminalDir`, make `Dir(terminals, ID)` at 0700, check `SockFits`, and take `Lock`, held for life.
4. For `account`: look up the account, run `Launchable`, run `Prepare`, then close the store. For `shell`: run `$SHELL -l` with `store.HostEnv()`.
5. The child's environment is `termenv.Env(termenv.Scrub(env))`. Nothing in it ever names `ctl.sock`, and a holder never starts a daemon.
6. Call `store.KeepClaimsAcrossExec()`, then start the child on a pty of `Cols×Rows`, bounded the way `boundSize` bounds sizes.
7. Listen on `sock` at 0600, `Start` the session, and write the full `meta.json` with stored args, both pids, both start times, boot id, `protocol` 1, `version` `wire.Version` and `token_until` from `holdwire.LoginUntil`.
8. Print one `Ready{OK, PID, TokenUntil}` line, then point stdout and stderr at `/dev/null`.
9. For claude, poll `<home>/sessions/<childpid>.json` every 2 s. It counts only when `procStart`, parsed with `time.ANSIC` in local time, is within 1 s of the child's start. Each change goes to `SetRegistry`, and a new id, name or cwd is written to `meta.json`. A missing file keeps the last values.
10. Touch `meta.json` every hour. Log to `holder.log`, which moves to `holder.log.1` at 1 MiB.
11. On exit, write `ended` and `exit`, call `release()`, remove `sock`, and exit 0.

A failure before step 8 prints `Ready{Error, Kind: holdwire.Classify(err)}` and exits 1. It leaves no process running and no `meta.json`.

- [ ] **Step 1: Write the failing tests.** These are process-level. Each test runs `os.Args[0] serve hold` with `ROTA_AS_RUN=1` and `Setsid`, under `shortHome(t)`, with the tty fake on `PATH` as claude. The test claims the daemon lock so that it can link.
  - `TestAHolderRunsTheCLIAndSaysItIsReady`: the request asks for 120×40. `Ready.OK` is true and its pid is alive. `meta.json` is 0600 with every field above, `args` stored. A link with `Since(0)` shows `ready 120x40`.
  - `TestAHolderTakesNothingFromArgvAndKeepsNoToken`: on Linux, `/proc/<pid>/cmdline` after argv[0] is `serve\x00hold`. Neither `meta.json` nor `holder.log` contains the account's token `t`.
  - `TestAHolderRefusesAPerson`: standard input on a pty exits 2 with the message, and so does a holder that does not lead its own session.
  - `TestTheChildsEnvironmentIsScrubbed`: `Env` holds `TMUX=x`, `CLAUDECODE=1`, `TERM_PROGRAM=iTerm.app`, `CODEX_THREAD_ID=y` and `FOO=bar`. `env TMUX` prints unset, `env FOO` prints `FOO=bar`, `env TERM` prints `TERM=xterm-256color`, and `envhas ctl.sock` prints `envhas no`.
  - `TestTheRegistryShapeIsRead`: the parser reads the committed fixture `cmd/rota/hold/testdata/registry-2.1.318.json`, which has the keys of a real 2.1.318 file and made-up values, into `Registry{SessionID, Name, Cwd, Status: "idle", Kind: "interactive"}` with an empty `WaitingFor`. A copy with `waitingFor` set to `"permission"` gives that string, one with an object gives its `type` or `kind`, and one with a key missing leaves that field empty.
  - `TestTheConversationComesFromTheRegistry`: after `registry idle`, `meta.conversation` is `fake-<childpid>` within 5 s. A file with `procStart` an hour off is ignored. Once the file is removed, the id stays.
  - `TestTheClaimLivesWithTheCLI` (Review Focus 1): use a codex account whose tty fake is a codex. `store.Busy` is true while it runs. After kill -9 of the holder it is still true while the child lives. Once the child is gone (its pty master closed), it is false within 5 s.
  - `TestAFailedPrepareSaysWhyAndLeavesNothing`: account 9 gives `Ready{Kind:"noaccount"}` and exit 1. No child is alive and there is no `meta.json`.
  - `TestTheExitIsRecorded`: typing `bye\r` over the link records `ended` and `exit` 0. The lock is free, the `sock` is gone and the holder has exited.
- [ ] **Step 2: Run them.** They fail: `serve hold` is an unknown word.
- [ ] **Step 3: Implement**, and add the reference row: ``| `rota serve hold` | internal: one held session's process, started by the daemon with its request on standard input; refuses a terminal |``.
- [ ] **Step 4: Verify.** This is a **race** and **docker** task, and the local CI run passes, `TestReferenceDocumentsEveryCommand` included.
- [ ] **Step 5: Commit.**
  ```bash
  git add internal/termenv internal/fakecli cmd/rota api/terminals.go docs/reference.md
  git commit -m "rota serve hold runs one account's CLI on a pty of its own, in a session of its own, taking its request on stdin, keeping the account's claim with the CLI, and recording which conversation it is in"
  ```

### Task 6: `heldTerm`: the daemon's side of a holder

**Files:**
- Create: `api/held.go`, `api/held_test.go` and `api/fakeholder_test.go` (a fake holder run by the api test binary when `ROTA_FAKE_HOLDER=1`, dispatched from `api`'s `TestMain`)
- Modify: `api/server.go` (`Options.HoldCommand`; `New` claims; `Stop` detaches), `api/terminal.go` (`termBackend.detach()`, held sessions skipped by `sweepTerminals`, attach with a Screen), `api/terminals.go` (`startTerminal` spawns a holder) and `docs/reference.md` (the description fields)
- Test: the tests below

**Interfaces:**
- `Options.HoldCommand func() *exec.Cmd`. When nil, terminals are `localTerm` as before. `api` adds `ROTA_HOME=<absolute store dir>` and `Setsid` to the command it returns.
- `termBackend` gains `detach()`, which `localTerm` and `sharedTerm` implement as no-ops.
- A held session's description adds `"held": true`, `"state": "live"`, `"attachable": true`, `"pid"`, `"name"`, `"title"`, `"conversation"` and `"status"` (`running`, `needs_you` or `quiet`). `name` is the first that exists of label, `vtscan.CleanTitle(title)`, the registry name and the folder's base name. `needs_you` means the registry's `WaitingFor` is set. `quiet` means no output for 30 s.
- The WS `hello` adds `"repaint": true` when the replay is a Screen.

The decisions:
- `New` with `HoldCommand` refuses where `!pty.Supported`, saying `held sessions run on linux and macOS`. It calls `ClaimDaemon(terminals, wire.Version)`, and failure is `api: another rota daemon serves <store> (pid N)`. Terminal machinery runs even when `Routes.Terminal` is false.
- `startTerminal` with `HoldCommand` chooses the account, runs `Launchable` and resolves the cwd as today, without `Prepare`. It makes a new `runID()`, writes the `Request` to the holder's stdin and reads `Ready` within 30 s. Kinds map to statuses: busy and reauth are 409, noaccount is 404, unsupported is 502, outside is 400, and other is 500 with `internal error`. A busy answer appends ` — held by session <name> (<id>)` when `Holding` finds one. `Ready.TokenUntil` becomes the session's `tokenUntil`, so `token_until` is described as today. `go cmd.Wait()` reaps the holder.
- The link uses `NewAt(scrollback, Welcome.Oldest)`, then `Since(Oldest)`. An `Output` whose offset is beyond `ring.End()` is `gapOf` that many bytes first.
- If the link is lost while the lock is held, it is dialled again each second with `Since(ring.End())`. Once the lock is free: an `exit` in `meta.json` finishes the session with that code, and no `exit` finishes it with -1, described as `"state": "interrupted"`.
- `typeInto` sends `Input{seq}`. An `InputRefused` goes to the connection that typed it, through a map of the last 1024 sequence numbers. `hangUp` sends `Stop`. `end` sends SIGKILL to the holder pid after `SameProcess`. `detach` closes the link, and `run` returns without `finish`.
- `Stop` closes the share socket, then detaches held sessions and kills the others with `"shutdown"`. It cancels, waits for the keeper, and releases the daemon claim last.
- An attach with no `since` sends `ScreenAsk` and waits at most 500 ms. If it is `OK`, `hello.offset` is the Screen's offset, the repaint goes out, and then `ring.Read(offset)` follows, all under `ts.mu`. Otherwise the ring is replayed as today.

- [ ] **Step 1: Write the failing tests** (`//go:build linux || darwin`, with `HoldCommand` running the api test binary as the fake holder).
  - `TestAHeldTerminalLivesInItsOwnProcessAndSurvivesStop`: `POST /v1/terminals` gives a description with `held` and a `pid` that is not this process. After `Stop`, that pid is alive, its lock is held, and `daemon.lock` is free.
  - `TestTheDaemonClaimIsExclusive`: a second `New` with `HoldCommand` on the same `Dir` fails with an error naming this pid.
  - `TestWithoutHoldCommandNothingChanges`: a server with the terminal group and no `HoldCommand` writes no `daemon.lock`, and its description has no `held`. Every existing test in `api/terminal_test.go` passes unchanged.
  - `TestAttachSendsTheScreenThenTheRest`: the fake's repaint is `SCREEN` at offset N. `hello.offset` is N with `repaint` true, the first binary frame is `SCREEN`, and then come the bytes after N.
  - `TestARefusedKeystrokeIsSaidToWhoTyped`: the fake refuses a sequence number, and that connection gets an `error` frame carrying its message.
  - `TestALostLinkComesBackWithItsOffset`: the fake drops the link once. Output continues with no byte twice and none missing.
  - `TestTheExitReachesEveryViewer` (Review Focus 5): the fake exits 4 with two viewers attached. Both get `{"type":"exit","code":4}`.
  - `TestAHeldSessionIsNeverIdle`: with `IdleTimeout` 1 ms, `sweepTerminals` leaves it running.
  - `TestBusyNamesTheSession`: the fake answers `Kind busy`, and a live record on account 1 named `audit` exists. The 409 message contains `audit`.
- [ ] **Step 2: Run them.** They fail: `HoldCommand` does not exist.
- [ ] **Step 3: Implement.** Add the held fields to the `/v1/terminals` rows of `docs/reference.md`.
- [ ] **Step 4: Verify.** This is a **race** and **docker** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add api docs/reference.md
  git commit -m "With HoldCommand set the server holds each terminal through a holder process, claims its store's daemon lock, repaints an attaching viewer from the holder's screen, and lets go of held sessions when it stops"
  ```

### Task 7: Re-adoption, stopped sessions and foreign holders

**Files:**
- Create: `api/adopt.go` and `api/adopt_test.go`
- Modify: `api/terminals.go` (merging lists and descriptions, `DELETE` and attach for foreign holders), `api/terminal.go` (the `keep_stopped` sweep), `api/config.go` (`keep_stopped` in `TerminalSection`, `DefaultConfig`, `Check`, `Serve` and `Print`), `api/server.go` (`Terminal.KeepStopped`) and `docs/reference.md` (the config block and the `/v1/terminals` rows)
- Test: `api/adopt_test.go`, `api/config_test.go`

**Interfaces:**
- `Terminal.KeepStopped time.Duration`, defaulting to 168 h, and `TerminalSection.KeepStopped time.Duration` with tag `toml:"keep_stopped"`, defaulting to `168h`.
- In a daemon, `GET /v1/terminals` and `GET /v1/terminals/{id}` also describe stopped records: `"held": true`, `"status": "stopped"`, `"state"` (`ended`, `interrupted` or `broken`), `"resumable"` (an interrupted record with a conversation), `"conversation"`, `"conversation_cwd"`, `"ended"` and `"exit"`. A foreign holder is `"state": "live"`, `"attachable": false`, `"made_by": "<version>"`. Phase 4's Resume and Move read exactly these fields.

The decisions:
- `New` scans and judges each record on its own, with a 2 s link timeout each. One failure is logged by id.
- A live record with an agreed major is adopted as in Task 6, with `Since(0)`.
- A live record with no agreed major becomes a `foreignTerm`. Attach answers 409 `session <id> was made by rota <version>, which this rota cannot attach to; stop it, or attach with that version`. `DELETE` sends HUP to the holder pid after `SameProcess(holder_pid, holder_start, boot_id)`, and KILL after `termKillAfter` if it is still locked. A failed check is 409 `session <id> is no longer the process that started it; nothing was signalled`.
- A live record whose link fails is listed live, with `"reachable": false`, and is retried at every sweep.
- The sweep deletes the dir of an unlocked record whose `Stopped` is older than `KeepStopped`. It also deletes an unlocked dir with no `meta.json` that is over 1 h old, and the `sock` of any unlocked dir. It never touches a locked dir. It deletes only the record's own directory, never a worktree or anything else the session's CLI made. `max_sessions` counts adopted and foreign sessions.

- [ ] **Step 1: Write the failing tests.**
  - `TestARestartReadoptsWithOffsetsIntact`: output `one`, `Stop`, then the fake prints `two` 200 ms later with no daemon running. A new `New` describes the session live, and attaching with `since=<the old offset>` gives `two` and nothing twice.
  - `TestEveryRecordIsJudgedOnItsOwn`: broken, interrupted, ended and live records are all listed with their states, and the live one is adopted despite the broken one.
  - `TestAHolderOfAnotherMajorIsListedAndNeverKilled`: a fake that offers only major 9 is listed `attachable: false` with `made_by`. Two `New`/`Stop` cycles leave it alive. Attach is 409. `DELETE` delivers HUP, which the fake records. With `holder_start` edited an hour off, `DELETE` is 409 and nothing is signalled.
  - `TestStoppedSessionsStayForKeepStopped`: with `KeepStopped` 1 h, an ended record from 2 h ago is gone after a sweep and one from 30 min ago stays. A locked dir with an old mtime stays.
- [ ] **Step 2: The Review Focus tests.**
  - `TestAKilledHolderReadsInterrupted` (Review Focus 1): kill -9 the fake while it is linked. Within 2 s it is described as `state: interrupted` with its conversation. A second live session's output is unaffected, and no new holder process has started.
  - `TestAStaleSockIsClearedOnlyWhenUnlocked` (Review Focus 2): an unlocked dir keeps a `sock` file from a crash, and a sweep removes it. A locked dir's `sock` survives every sweep.
  - `TestAnEndedSessionStaysListed` (Review Focus 5): after the fake exits 0, `GET /v1/terminals/{id}` is still 200 after `keepEnded` with `state: ended` and `exit: 0`.
- [ ] **Step 3: Config tests.** In `TestKeepStoppedIsADuration`, the default `Print` line is `keep_stopped    = "168h"`. The value `0s` is refused with `terminal.keep_stopped must be longer than nothing`. A file's `"72h"` comes through `Serve` as `Terminal.KeepStopped`.
- [ ] **Step 4: Run them.** They fail.
- [ ] **Step 5: Implement**, adding `keep_stopped    = "168h"    # how long a stopped held session stays listed` to the reference's `[terminal]` block.
- [ ] **Step 6: Verify.** This is a **race** and **docker** task, and the local CI run passes.
- [ ] **Step 7: Commit.**
  ```bash
  git add api docs/reference.md
  git commit -m "A daemon that starts re-adopts every live holder on its own, lists ended and interrupted sessions for keep_stopped, and lists a holder it cannot read without ever killing it unasked"
  ```

### Task 8: Viewers and typists of a held session

**Files:**
- Modify: `api/terminal.go` (the person rule, the fit rule, repainting a slow viewer), `api/terminals.go` (stripping in `onTerm`, the send route), `api/principal.go` (`personOf` and the `roleFor` row), `api/config.go` and `api/server.go` (`local_user`) and `docs/reference.md`
- Test: `api/held_view_test.go`, `api/config_test.go`

**Interfaces:**
- `Options.LocalUser string`, and `AuthSection.LocalUser string` with tag `toml:"local_user"`.
- The route `POST /v1/terminals/{id}/send` takes control and the body `{"text":"…","force":false}`. It answers 200 `{"ok":true}`, or 409 `{"error":…,"reason":"waiting"|"keyboard"|"ended"|"not_held"}`, or 504 `{"error":…,"reason":"timeout"}`.
- The resize frame is `{"type":"resize","cols":N,"rows":M,"fit":true}`. Phase 2's `Ctrl-\ z` and phase 6's Fit button send `fit`.
- A viewer that fell behind gets `{"type":"repaint","offset":N}` and then the repaint's binary frame.
- `const heldNoFit = "this session's size changes only when a viewer asks to fit it"`.
- `func (s *Server) personOf(p *Principal) string`. A user is its name. `local` is the only control user if there is exactly one, else `LocalUser` if set, else `"local"`. Tokens and invites are `""`, which is never a person. Phase 2's SSH principals are users, so they follow the same rule.

These rules apply to held sessions only:
- Each connection's `vtscan.ReplyFilter` strips its binary frames before any role or keyboard check. A frame that held only replies types nothing and moves nothing.
- A connection whose person, if not empty, matches the keyboard holder's person types without ceremony when it is not watching and has role control. It takes the keyboard, with the audit line `keyboard moved`.
- A resize without `fit` is ignored, and `heldNoFit` is said once per connection. A resize with `fit` follows today's rule: only the holder resizes.
- On overflow, a viewer is marked behind, its queue drains, and it gets the repaint frame, the Screen and the ring's tail. It is never closed with 1013.
- `send` needs a held, live, attachable session. The keyboard must be free or held by the sender's person, otherwise 409 `keyboard` with `<name> holds the keyboard of this session`. It sends `Send{WaitMS: 30000}` and waits up to 35 s for `Sent`. Any other terminal is 409 `not_held`. `roleFor` gets `"POST /v1/terminals/{id}/send": RoleControl`.
- `Check` refuses a `local_user` that is not a `[[users]]` entry with role control: `auth.local_user names %q, who is not a [[users]] entry that may control`.

- [ ] **Step 1: Write the failing tests.**
  - `TestAViewersTerminalNeverAnswersTwice`: a watch connection sends `ESC[?62;22c` and gets no `error` frame, and the fake holder records no input. The holder of the keyboard sends `x ESC[?62;22c y`, and the fake gets `xy`.
  - `TestTheKeyboardFollowsThePersonNotTheConnection`: `[[users]] alice` signs in twice, from cookies A and B. A holds the keyboard. B types, so B now holds it and A is told. bob, a user, types and is refused `notHolding`. Two token principals keep the claim flow.
  - `TestALostKeyboardIsNotReclaimedByReconnecting`: alice's connection holds the keyboard. bob asks, waits out `claimWait`, and forces it. alice's connection drops and attaches again while bob is still attached. It does not hold the keyboard, and `hello.holder` is bob.
  - `TestLocalIsTheSoleControlUser`: users `me` (control) and `viewer` (watch) give `local` → `me`. Two control users and no `local_user` give `local`. With `local_user = "you"` it is `you`.
  - `TestAttachingAndTypingNeverResize` (Review Focus 4): a resize without `fit` gives `heldNoFit` once, the size is unchanged, and the fake gets no `Resize`. With `fit` and `{0,0}` the fake gets 80×24, and `resized` goes to everyone.
  - `TestASlowViewerIsRepaintedNotDropped`: `termQueueFor` gives the watch connection depth 1 while the fake floods. It is never closed with 1013 and receives `{"type":"repaint"}` and then a binary frame.
  - `TestSendTypesUnderTheKeyboardRules`: with the keyboard free, the answer is 200 and the fake got `Send`. With bob holding it, alice gets 409 `keyboard` naming bob. When the fake answers waiting, the route gives 409 `waiting`, and `force` gives 200. A `localTerm` is 409 `not_held`.
  - `TestSendAnswersEndedWhenTheCLIExits` (Review Focus 5): the fake exits while a send waits, and the route answers 409 `ended` within 1 s.
  - `TestLocalUserMustBeAControlUser`: the config refusals above.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.** Add the send row to the reference's endpoint table, and `local_user = ""` with its comment to the `[auth]` block.
- [ ] **Step 4: Verify.** This is a **race** and **docker** task, and the local CI run passes, `TestReferenceDocumentsEveryRoute` and the `roleFor` test included.
- [ ] **Step 5: Commit.**
  ```bash
  git add api docs/reference.md
  git commit -m "On a held session a viewer's terminal never answers twice, the keyboard follows whichever device its person types on, only an explicit fit resizes, a slow viewer is repainted rather than dropped, and send types under the same rules"
  ```

### Task 9: `ctl.sock` and `LocalHandler`

**Files:**
- Modify: `api/server.go` (`Handler` and `LocalHandler` from one table, through `guardedIn(terminal bool)` and `socketsIn(terminal bool)`; `guarded()` and `sockets()` keep their meaning), `api/principal.go` and `api/ws.go` (one helper exempts `token` and `local` from the Origin check)
- Create: `api/local_test.go`, `cmd/rota/ctlsock.go`, `cmd/rota/ctlsock_unix.go`, `cmd/rota/ctlsock_other.go` and `cmd/rota/ctlsock_test.go`
- Modify: `docs/reference.md` (a section, *The control socket*)
- Test: the tests below

**Interfaces:**
- `func (s *Server) LocalHandler(p *Principal) http.Handler` registers every group, terminals and the send route included. Every request carries `p`. No token, cookie or Origin is read, and `roleFor` decides. Phase 2's SSH server calls it in-process with the person its key authenticated.
- The principal is `Principal{Name: "local", Role: RoleControl, Via: "local"}`.
- `func listenCtl(srv *api.Server, terminals string, log *slog.Logger) (stop func(), err error)` in `cmd/rota`.

The decisions:
- The path is `holdwire.CtlSock` and must pass `SockFits`. A leftover file there is removed before listening. Only a caller holding the daemon claim calls this, since `api.New` took it. The socket is chmod 0600.
- `ConnContext` reads `proc.PeerPID`. If the peer's `getsid` (the existing `sessionOf`) is the holder pid or child pid of any live record, the connection gets `{Name:"local", Role: RoleWatch, Via:"local"}`. An unknown peer gets the same. Everyone else gets control.
- The http.Server uses `serve`'s timeouts. No header ever names a person.
- `Handler()` for TCP is unchanged.

- [ ] **Step 1: Write the failing tests.**
  - `TestLocalHandlerHasEveryGroup`: on a server with `Routes{API: true}` only, `LocalHandler` still answers `/v1/terminals` and `/v1/health`, while `Handler` answers 404 on `/v1/terminals` as before.
  - `TestLocalNeedsNoTokenAndNoOrigin`: `POST /v1/terminals` with `Origin: https://evil.example` passes the guard.
  - `TestLocalWatchCannotChange`: the watch principal gets 403 `watchOnly` on `POST /v1/terminals`.
  - `TestCtlSockAnswersOnlyItsOwner`: the socket is 0600 and `GET /v1/health` answers over it.
  - `TestACallerInsideAHeldSessionOnlyWatches`: a helper process is the test binary with `ROTA_CTL_PROBE=1`, started with `Setsid`, and prints the status of `POST /v1/terminals` over `ctl.sock`. A live record whose `child_pid` is the helper's pid, locked by the test, makes it print 403.
- [ ] **Step 2: The stale socket test (Review Focus 2).** In `TestAStaleCtlSockIsReplaced`, a regular file left at `ctl.sock` by a crash does not stop `listenCtl` under a claim, and `GET /v1/health` then answers.
- [ ] **Step 3: Run them.** They fail.
- [ ] **Step 4: Implement**, and document `ctl.sock`: who reaches it, what it serves and the watch-only rule.
- [ ] **Step 5: Verify.** This is a **race** and **docker** task, and the local CI run passes, `TestReferenceDocumentsEveryRoute` and the `roleFor` test included.
- [ ] **Step 6: Commit.**
  ```bash
  git add api cmd/rota docs/reference.md
  git commit -m "The daemon's control socket serves every route to the fixed principal local, whose authorization is the socket's own permissions, and anything running inside a held session may only watch there"
  ```

### Task 10: `rota serve start | stop | status`

**Files:**
- Create: `cmd/rota/daemon.go`, `cmd/rota/daemon_unix.go`, `cmd/rota/daemon_other.go`, `cmd/rota/daemon_test.go`, `cmd/rota/client/client.go` and `cmd/rota/client/client_test.go`
- Modify: `cmd/rota/main.go` (serve's switch gains `start`, `stop` and `status`; building the http.Server at `main.go:2285-2296` moves into `func newHTTPServer(addr string, h nethttp.Handler, logger *slog.Logger) *nethttp.Server`, shared with foreground serve, which still prints exactly what it printed) and `docs/reference.md` (the three verbs and the flags `--foreground`, `--ctl-only` and `--yes`)
- Test: `cmd/rota/daemon_test.go`

**Interfaces:**
- `rota serve start [--foreground] [--ctl-only] [--config FILE]`, `rota serve stop [--all] [--yes]` and `rota serve status`, which honours `rota --json`. `status` exits 0 when a daemon runs and 3 when none does.
- `--json` for status is `{"running","pid","version","started","store","ctl","http","sessions":{"live","stopped"}}`. Phase 4's Services tab reads the same through `client.Health` and `holdwire.RunningDaemon`.
- The first half of `package client`, which `status` and `stop --all` use and Task 12 completes. Phase 2's SSH `list`, and the TUI `Backend` in phases 3 and 4, use it as well:
  - `type Client` and `var ErrNoDaemon = errors.New("no rota daemon is running for this store")`.
  - `func Dial(terminals string) (*Client, error)`. The lock decides whether a daemon exists. A held lock with a silent socket is retried for 5 s and is then `the rota daemon (pid N) is not answering on <path>`, never `ErrNoDaemon`.
  - `func (c *Client) Health() error`, `List() ([]Session, error)` and `Stop(id string) error`.
  - `type Session struct { ID, Name, Label, Kind, State, Status, Provider, AccountLabel, Cwd, Conversation string; Account, PID int; Attachable, Resumable bool; Started time.Time; Ended *time.Time; Exit *int }`, read from the description documents of Tasks 6 and 7.
- `func daemonHoldCommand(exe string) func() *exec.Cmd` resolves `exe` once at start with `EvalSymlinks`, so an upgrade in place starts new holders from the new binary.
- The test knobs `var daemonReady = 10 * time.Second` and `var daemonStopWait = 15 * time.Second`.

The decisions:
- **start.** If `RunningDaemon` answers, print `rota daemon already running for <store> (pid N)` and exit 0. Otherwise run `<exe> serve start --foreground [--ctl-only] [--config F]` with `Setsid`, `Dir` set to `/`, an absolute `ROTA_HOME` and stdio on `/dev/null`. Try `client.Dial` and `Health` every 50 ms for up to `daemonReady`, then print `rota daemon started for <store> (pid N); control socket <path>`, with `; http on <addr>` when there is one. If the child exits first, print `the rota daemon did not start; the end of <log>:` with its last five lines, and exit 1.
- **--foreground.** Open the log first: `[server] log_file`, or else `<store>/daemon.log`, through `openLogFile`, rotating at 20 MiB with four archives. Nothing is written to stderr after that. TCP opens only when there is no `--ctl-only` and `cfg.From["server.listen"] == "file"`. The token is `cfg.Token()`, or else 32 random bytes in hex, held in memory. `RefreshEvery` stays at its default, so the daemon's one background refresh is the reading everyone shares. Set `HoldCommand`, call `api.New` (which claims the store) and `listenCtl`, then serve until SIGTERM or an interrupt, ignoring SIGHUP. Shut both servers down within `serveGrace`, then call `srv.Stop()`, which detaches the sessions.
- **stop.** With no daemon, print `no rota daemon is running for <store>` and exit 0. Otherwise send SIGTERM after `SameProcess` against `daemon.pid`, wait for the lock to come free within `daemonStopWait`, and print `rota daemon stopped; N sessions keep running`.
- **stop --all.** On a terminal it asks `stop the rota daemon and end N sessions? [y/N] `. Off a terminal without `--yes`, it is a usage error. It calls `client.Stop` for each live session in `client.List`, waits until each is ended, within `termKillAfter + 2s`, then stops the daemon. With no daemon running, it sends each live holder HUP after `SameProcess`, and KILL after 3 s if it is still locked.

- [ ] **Step 1: Write the failing tests.** These are process-level, with the test binary standing in for rota and the tty fake standing in for claude.
  - `TestStartStatusStop`: after `serve start`, `status` exits 0 and names the pid. After `stop`, `status` exits 3.
  - `TestTwoStartsMakeOneDaemon` (Review Focus 3): two `serve start` calls run at once. Both exit 0, `daemon.pid` names one pid, and that one alone holds the lock. The other's foreground child has exited, and its log says `another rota daemon serves`.
  - `TestStopLeavesSessionsAndAStartReadoptsThem`: after `POST /v1/terminals` over `ctl.sock`, then `stop`, the holder is alive and locked. After `start`, the list shows the same id live, and typing `x\r` over WS gives `got:x`.
  - `TestCtlOnlyOpensNoPort`: with `[server] listen = "127.0.0.1:<free port>"` in `server.toml`, `start --ctl-only` leaves the port closed, and plain `start` opens it.
  - `TestStopAllAsksFirst`: off a terminal, without `--yes`, it exits 2. With `--yes`, the records read ended and no daemon is running.
  - `TestTheDaemonLogsToTheStore`: `daemon.log` is 0600 with the start line, and the foreground child's stderr is empty.
  - `TestForegroundServeIsUnchanged`: `rota serve` with a token, through the existing `serveUntil` seam, writes no `daemon.lock` and no `ctl.sock`. Its stderr lines equal those before this task.
  - `TestDialNeverTrustsAFailedConnect` (Review Focus 2): the test holds the daemon claim and no socket is listening. `Dial` waits 5 s and fails with `is not answering`, not with `ErrNoDaemon`. With the claim released, `Dial` returns `ErrNoDaemon` at once.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, with reference rows for `rota serve start`, `rota serve stop` and `rota serve status`, their flags, and the table in spec section 6 comparing foreground and daemon mode.
- [ ] **Step 4: Verify.** This is a **race** and **docker** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota docs/reference.md
  git commit -m "rota serve start runs one daemon per store in the background with its control socket, a TCP port only where server.toml asks, and its log in the store; stop ends it and leaves every session running"
  ```

### Task 11: `rota serve install | uninstall`

**Files:**
- Create: `cmd/rota/unit.go`, `cmd/rota/unit_linux.go`, `cmd/rota/unit_darwin.go`, `cmd/rota/unit_other.go` and `cmd/rota/unit_test.go`
- Modify: `cmd/rota/main.go` (serve's switch) and `docs/reference.md`
- Test: `cmd/rota/unit_test.go`

**Interfaces:**
- `func systemdUnit(exe, store string, env []string) string` and `func launchdPlist(label, exe, store string, env []string) string`. Both are pure, so either can be tested on any unix.
- `var unitRun = func(name string, args ...string) error`.
- `func unitName(store string) (systemd, label string)` gives `rota.service` and `com.github.professor93.rota` for the default store. For any other store it adds `-<first 8 hex of sha256(absolute store)>` to both.

The unit, byte for byte. The `CLAUDE_CONFIG_DIR` line appears only when that variable is set:
```
[Unit]
Description=rota daemon for <store>

[Service]
Type=simple
ExecStart=<exe> serve start --foreground
Environment="PATH=<PATH>"
Environment="ROTA_HOME=<store>"
Environment="CLAUDE_CONFIG_DIR=<dir>"
KillMode=process
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```
- The plist has these keys: `Label`, `ProgramArguments` (`<exe> serve start --foreground`), `EnvironmentVariables` (the same three), `RunAtLoad` true, `KeepAlive` `{SuccessfulExit: false}`, `AbandonProcessGroup` true, `ProcessType` `Background`, and `StandardOutPath` and `StandardErrorPath` both `/dev/null`.
- On Linux, `install` writes `$XDG_CONFIG_HOME/systemd/user/<unit>`, falling back to `~/.config/systemd/user/<unit>`. It runs `systemctl --user daemon-reload` and then `systemctl --user enable --now <unit>`. When `/var/lib/systemd/linger/<user>` is missing, it prints `run "loginctl enable-linger <user>" so the daemon starts at boot and outlives your login`.
- On darwin, `install` writes `~/Library/LaunchAgents/<label>.plist` and runs `launchctl bootstrap gui/<uid> <plist>`.
- `uninstall` runs `systemctl --user disable --now <unit>`, removes the file and reloads. On darwin it runs `launchctl bootout gui/<uid>/<label>` and removes the file. Sessions keep running either way.
- If a daemon runs and this store's unit file does not exist, `install` refuses: `a rota daemon is running for <store> (pid N), started by hand; rota serve stop it, then install`. If the file exists, `install` rewrites it and restarts the unit.
- On Windows both commands refuse by name.

- [ ] **Step 1: Write the failing tests.**
  - `TestTheUnitKeepsSessions`: `systemdUnit` equals the text above for a fixed exe, store and env, and leaves out `CLAUDE_CONFIG_DIR` when it is unset.
  - `TestThePlistKeepsSessions`: `launchdPlist` holds `AbandonProcessGroup` true and the three variables.
  - `TestInstallRunsTheSupervisor`: with `HOME` and `XDG_CONFIG_HOME` in temp dirs and `unitRun` recorded, the file is written and the commands run in order.
  - `TestUninstallRemovesIt`: the commands run in order and the file is gone.
  - `TestInstallRefusesWhileAHandStartedDaemonRuns` (Review Focus 3): while the test holds the claim, `install` exits 1 with the message.
  - `TestAnotherStoreGetsAnotherUnit`: `unitName` gives `rota.service` for the default store and `rota-xxxxxxxx.service` for another.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, with reference rows for `rota serve install` and `rota serve uninstall`. Say there that a supervisor does not pass `ROTA_TOKEN`, so a daemon's TCP port wants `auth.token_file` or `[[users]]`.
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota docs/reference.md
  git commit -m "rota serve install makes the daemon a systemd user unit or a launchd agent that keeps sessions running through a restart, with the installer's PATH, ROTA_HOME and CLAUDE_CONFIG_DIR, and uninstall takes it away"
  ```

### Task 12: The client, and `rota run --detach`

**Files:**
- Create: `cmd/rota/detach.go` and `cmd/rota/detach_test.go`
- Modify: `cmd/rota/client/client.go` and `cmd/rota/client/client_test.go` (the second half), `cmd/rota/share.go:84-141` (`takeShareFlags` lifts `--detach` into `shareAsk.detach`), `cmd/rota/main.go:908-958` and the run usage text, `api/terminals.go` (`termRequest.Env`) and `docs/reference.md`
- Test: the tests below

**Interfaces:**
- Consumes `Client`, `Dial`, `ErrNoDaemon`, `Session`, `List`, `Stop` and `Health` from Task 10.
- The second half of `package client`. Phase 2's SSH `new` and the TUI `Backend` in phases 3 and 4 use it:
  - `func Ensure(terminals string, start func() error) (c *Client, started bool, err error)`. When the lock is held it never starts anything.
  - The methods `Create(r Create) (Session, error)`, `Get(id string) (Session, error)`, `Resolve(nameOrID string) (Session, error)` and `Send(id string, r SendReq) error`.
  - `type Create struct { Account int; Args []string; Cwd string; Cols, Rows uint16; Label string; Env []string }`.
  - `type SendReq struct { Text string; Force bool }` and `type Refusal struct { Status int; Reason, Message string }`, which implements `error`.
  - `Resolve` matches an exact id first, then a unique id prefix of at least 4 characters, then an exact label or name of a live session. More than one match is an error that lists their ids.
- `termRequest.Env []string` with the JSON key `env`. It is accepted only from `Via == "local"`, and anyone else gets 403 `env is taken only over the control socket`.

The decisions for `run --detach`:
- `--detach` with `--share` is a usage error: `--share and --detach are two different terminals; choose one`.
- A prompt given as plain words rather than after `--` or `-i` is a usage error, and so are `-p` and `--print` among the CLI's arguments: `--detach hands the CLI its terminal, so it takes no prompt or -p: give the CLI's arguments after --`.
- The size comes from the stdin terminal (`pty.Size`), or else 120×40. The cwd is `os.Getwd()`. The environment is `store.HostEnv()`. `--remote-control` adds its arguments through `c.remoteControl` before the request goes.
- `Ensure` starts `serve start --ctl-only` when there is no daemon, and prints `rota: started the rota daemon for <store> (control socket only; rota serve stop ends it)`.
- On success it prints `rota: <account> via <bin> — held as session <name> (<id>); rota send <name> "..." types into it, rota list --sessions lists it` and exits 0. `--json` prints the `Session`.

- [ ] **Step 1: Write the failing tests.**
  - `TestDetachHandsTheCLIToAHolder`: `rota run 1 --detach --label refactor` returns 0. The session is live and named `refactor`. Over WS it says `ready 120x40`, because standard input in the test is not a terminal.
  - `TestDetachStartsADaemonAndSaysSo`: with no daemon, stderr carries the line, `daemon.pid` exists, and no TCP port is open.
  - `TestDetachRefusesAPromptAndPrint`: `rota run 1 --detach "fix it"` exits 2, `rota run 1 --detach -- -p hi` exits 2, and `--detach --share` exits 2.
  - `TestDetachCarriesTheCallersEnvironment`: with `FOO=bar` and `TMUX=x` set, `env FOO` prints `FOO=bar` and `env TMUX` prints unset.
  - `TestEnvIsTakenOnlyOverTheControlSocket`, in api: a token principal sending `env` gets 403.
  - `TestResolveByIDPrefixAndLabel`: an id prefix and a label both resolve. An ambiguous label is an error that names both ids.
  - `TestEnsureNeverStartsASecondDaemon`: while the test holds the claim with no socket listening, `Ensure` returns the `Dial` error and its `start` is never called.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, with a reference row for `--detach` under `rota run` and the `env` field in the `POST /v1/terminals` row.
- [ ] **Step 4: Verify.** This is a **race** and **docker** task, and the local CI run passes, `TestReferenceDocumentsEveryFlag` included.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota api/terminals.go docs/reference.md
  git commit -m "rota run --detach hands the CLI to a held session under the daemon, starting one with its control socket alone when none runs, with the caller's environment, size and folder"
  ```

### Task 13: `send` into a held session, and held sessions in `list --sessions`

**Files:**
- Modify: `cmd/rota/input.go:333-430` (`send` falls back to held sessions; adds `--force`), `cmd/rota/main.go:530-746` (the `list` section and its JSON; `handOver` and `answer` say which session holds a busy account) and `docs/reference.md`
- Create: `cmd/rota/held_list.go` and `cmd/rota/held_test.go`
- Test: `cmd/rota/held_test.go`

**Interfaces:**
- The new flag `rota send <run-or-session> "text" [--force]`.
- `rota list --sessions` prints `Held sessions:` only when `holdwire.Scan` finds records. Each row is `  ● <name>  #<account id> <account label>  <provider>  <status or state>  <cwd>  <first 8 of the session id>`, with `●` for live and `○` for stopped. The JSON gains `"held": [...]` only when that list is not empty. Neither needs a daemon.

The decisions:
- `send` tries the run socket first, unchanged. When there is no such run, it calls `client.Resolve`. With neither, it says `no run or session <target>; rota list --sessions shows what is running`.
- `--interrupt`, `--steer` and `--close` on a held session are a usage error: `those are for a run started with --input`.
- Success prints nothing, or `{"ok":true}` with `--json`.
- Every refusal exits 1:
  - `session <name> is waiting for a permission or a question, and Enter there may approve a tool; --force sends anyway`
  - `<who> holds the keyboard of <name>`
  - `session <name> did not become ready within 30s; nothing was typed`
  - `session <name> has ended`
- When `rota run <id>` fails with `ErrBusy` and `holdwire.Holding` finds a live session on that account, the error ends with ` — held by session <name> (<id>)`.

- [ ] **Step 1: Write the failing tests.**
  - `TestSendTypesIntoAHeldSessionByName`: in a session started with `--label api` whose fake printed `ESC[?2004h`, `rota send api "hello"` exits 0, and the screen over WS shows `got:\x1b[200~hello\x1b[201~`.
  - `TestSendRefusesAWaitingClaude`: after the fake's `registry waiting permission`, `send` exits 1 with the waiting message. With `--force` the text is typed.
  - `TestSendStillReachesARun`: `TestSendReachesARunningInput` and its neighbours pass unchanged.
  - `TestListShowsHeldSessionsOnlyWhenThereAreSome`: with no records, the output has no `Held sessions` and the JSON has no `held`. With one live and one interrupted record, both rows appear with `●` and `○`.
  - `TestABusyAccountNamesItsSession`: while a codex account is held, `rota run 2 -- --version` exits 1 with `held by session`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, with reference rows for `send`'s held form and `--force`, and the `Held sessions:` section described under *What is running, and what could resume*.
- [ ] **Step 4: Verify.** This is a **race** and **docker** task, and the local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git add cmd/rota docs/reference.md
  git commit -m "rota send types into a held session by name once it is ready, refusing one that waits on a permission unless forced, and rota list --sessions shows held sessions whenever there are any"
  ```

### Task 14: The manual, the notes and the next beta

**Files:**
- Modify: `docs/reference.md` (a section, *Held sessions and the daemon*, after *Sharing the terminal you are sitting at*, plus Layout rows for every new file), `docs/releases/1.4.0.md`, `lib/httpx.go:21` and `wire/wire.go:424`
- Test: the commands below

**Interfaces:**
- Produces the version `1.4.0-beta.N` and the notes that Task 15 publishes.

- [ ] **Step 1: The free number.** `git ls-remote --tags origin 'refs/tags/v1.4.0-beta.*' 'refs/tags/cmd/rota/v1.4.0-beta.*'` and `gh release list --repo professor93/rota` give the highest beta taken. N is one more, and is never a number that already exists. The proxy is not probed.
- [ ] **Step 2: The size.** `sh scripts/release.sh 2>/dev/null && wc -c < dist/rota-linux-amd64` gives the new size. The notes set it beside beta.2's 10.4 MB, to one decimal in MB, as the spec's "Risks" asks.
- [ ] **Step 3: The section in the manual.** Write it in the manual's voice, from these facts:
  - One process per session, and why: survival and isolation.
  - Where its files live.
  - Live, ended, interrupted and foreign, and `keep_stopped`.
  - What restarting or upgrading the daemon does to a session, which is nothing.
  - `ctl.sock` and `local`.
  - `local_user` and the keyboard's person.
  - Explicit fit.
  - The answers a holder gives to terminal queries.
  - That attach comes in a later beta.
- [ ] **Step 4: The notes.** Under `### New in 1.4.0`, before `### New in 1.4.0-beta.3`, add `### New in 1.4.0-beta.N` with one bullet each for:
  - held sessions, which run in a process of their own and survive the daemon
  - `rota serve start|stop|status|install|uninstall`
  - `rota run --detach`
  - `rota send` into a held session
  - `rota list --sessions`
  - `ctl.sock`
  - the holder answering queries
  - `keep_stopped`
  - `local_user`
  - the size

  Add a sentence that foreground `rota serve` is unchanged.
- [ ] **Step 5: Commit the docs.**
  ```bash
  git add docs/reference.md docs/releases/1.4.0.md
  git commit -m "The manual and the 1.4.0 notes describe held sessions, the daemon and its control socket"
  ```
- [ ] **Step 6: Bump.** `const Version = "1.4.0-beta.N"` goes in `lib/httpx.go:21` and `wire/wire.go:424`.
- [ ] **Step 7: Verify.**
  - The local CI run and its docker form pass.
  - `go run ./cmd/rota --version` prints `rota 1.4.0-beta.N`.
  - `git log --format=%B origin/main.. | grep -ciE 'co-authored|claude\.ai|anthropic|generated'` prints `0`.
- [ ] **Step 8: The owner's own checks, only on the owner's explicit yes.** A test may not start a real claude or touch the keychain, so these are for the owner to run, and the results go into the report rather than into git.
  1. On macOS, start `rota serve start` from Terminal.app, run `rota run <claude id> --detach`, quit Terminal.app, and `rota send <name> "/status"`. Then turn on `[routes] terminal` with a `[server] listen` and watch through `/terminal` that Claude Code still reads its login.
  2. Do the same under `rota serve install`, after logging out and back in.
  3. On Linux, run one held real claude session from start to `serve stop` and `serve start`, and check that it is still the same conversation.
- [ ] **Step 9: Commit and push.**
  ```bash
  git add lib/httpx.go wire/wire.go
  git commit -m "1.4.0-beta.N"
  git push -u origin phase-1
  ```

### Task 15: Release v1.4.0-beta.N (only on the owner's word)

Nothing in this task runs until the owner says so. It is Task 6 of the phase 0 plan with this phase's tag and notes.

**Interfaces:**
- Consumes everything above. `HEAD` of `phase-1` is the `1.4.0-beta.N` commit.

- [ ] **Step 1: Windows before any tag.** Run `gh workflow run ci.yml --repo professor93/rota --ref phase-1`, then `gh run watch --exit-status` on that run. `test (ubuntu-latest)`, `test (macos-latest)` and `windows` must all succeed. No tag is made until they do.
- [ ] **Step 2: The numbers are still free.** `git ls-remote --tags origin refs/tags/v1.4.0-beta.N refs/tags/cmd/rota/v1.4.0-beta.N` prints nothing.
- [ ] **Step 3: Root tag.** Run `git tag v1.4.0-beta.N && git push origin v1.4.0-beta.N`, then watch the `release.yml` run.
  - Gate, publish and all three smoke jobs succeed, and module is skipped.
  - `gh release view v1.4.0-beta.N --json isPrerelease -q .isPrerelease` prints `true`.
  - `gh api repos/professor93/rota/releases/latest -q .tag_name` still prints the last stable release.
- [ ] **Step 4: Release notes.** Write this head to `$N/head.md`, where `N=$(mktemp -d)`:

  > A pre-release for testing before 1.4.0. `install.sh` gives the last stable version, and `go install github.com/professor93/rota/cmd/rota@latest` gives the newest beta, which is this one. Held sessions and the daemon are new here; attach comes in a later beta. To try it:
  >
  > ```
  > curl -fsSL https://raw.githubusercontent.com/professor93/rota/main/install.sh | ROTA_VERSION=1.4.0-beta.N bash
  > go install github.com/professor93/rota/cmd/rota@v1.4.0-beta.N
  > ```
  >
  > It carries everything since 1.3.0: 1.3.1 and 1.3.2 were never published on their own.

  Then run `for f in "$N/head.md" docs/releases/1.4.0.md docs/releases/1.3.2.md docs/releases/1.3.1.md; do cat "$f"; echo; done > "$N/notes.md" && gh release edit v1.4.0-beta.N --notes-file "$N/notes.md"`.
- [ ] **Step 5: Pin.** `sh scripts/module.sh pin v1.4.0-beta.N` prints `module: tagged cmd/rota/v1.4.0-beta.N`. `git show --stat --format=%s HEAD` shows `cmd/rota requires rota v1.4.0-beta.N`, touching only `cmd/rota/go.mod` and `cmd/rota/go.sum`.
- [ ] **Step 6: Program tag.** Run `git push origin phase-1 cmd/rota/v1.4.0-beta.N`. In the new `release.yml` run, only `module` runs, and it succeeds.
- [ ] **Step 7: As the world sees it.** In `T=$(mktemp -d)`:
  - `GOBIN=$T go install github.com/professor93/rota/cmd/rota@v1.4.0-beta.N && $T/rota --version` prints `rota 1.4.0-beta.N`.
  - `go list -m github.com/professor93/rota@latest` still prints the last stable release.
- [ ] **Step 8: Main.** Run `git push origin phase-1:main`, a fast-forward. The `ci.yml` run on `main` is green on ubuntu, macos and windows.
- [ ] **Step 9: Rebuild.** In the main checkout, `git pull --ff-only && go build -trimpath -ldflags='-s -w' -o ~/.local/bin/rota ./cmd/rota && rota --version` prints `rota 1.4.0-beta.N`. Tell the owner the linux-amd64 size from Task 14.
