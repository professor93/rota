# Phase 2: attach, the chord and the SSH server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A person reaches any held session from a terminal: `rota run --attach <session>` on this machine, and `ssh -p 2222 host attach <session>` from anywhere. Both hand the screen to the session, read the `Ctrl-\` chord, crop rather than garble on a smaller window, and give the terminal back as they found it. The SSH server runs inside the daemon (and inside foreground `rota serve` when asked) and is off unless `[ssh] enabled`.

**Architecture:** Four new pieces, all in the program's module:
- `cmd/rota/client` gains a terminal WebSocket (`Attach`, `Term`) and an in-process transport (`InProcess`) to `Server.LocalHandler`.
- `cmd/rota/attach` is the one attach loop, written against a `Terminal` interface: the chord parser, a local copy of the session's screen (`hold.Screen` plus `vtscan.Scanner`) that both passes bytes through and crops, overlays repaired from that copy, reconnects and session switching.
- `cmd/rota/sshd` is `golang.org/x/crypto/ssh` with public keys only. It serves a fixed command list, and each connection reaches the API through `LocalHandler` with the principal its key proved, over `client.InProcess`.
- `cmd/rota` wires the chord key, `run --attach` and the SSH listener into the daemon and foreground serve.

The root module gains only config fields (`[ssh]`, `ssh_keys`, `detach_key`), an SSH-only user, and `vtscan.CtrlKey`.

**Tech Stack:** Go 1.27. `golang.org/x/crypto v0.58.0` (`ssh`) and `golang.org/x/term v0.47.0` go in `cmd/rota/go.mod` only, which moves `golang.org/x/sys` from v0.41.0 to v0.49.0 there. The plan reuses the existing `hold.Screen` (x/vt), `internal/vtscan`, `internal/pty`, `internal/holdwire`, and the terminal WebSocket of `api` unchanged.

**Spec:** `docs/superpowers/specs/2026-11-01-tui-daemon-ssh-design.md`. Read sections 4, 6, 7 and 8 and all of section 13 (*The terminal*, *Files and reach*), plus "Delivery, in order", "Public surface", "The owner's answers" and "Calls made without asking". Read phase 1's plan (`docs/superpowers/plans/2026-11-06-phase-1-holder-daemon.md`) for what exists, and `docs/reference.md` *A terminal on the server*, *The control socket* and *Held sessions and the daemon* for the protocol as shipped.

## Global Constraints

- The root module gains no `require`, no `replace` and no `go.sum`. New third-party code goes only into `cmd/rota/go.mod`, through `GOPROXY=off GOSUMDB=off sh scripts/module.sh tidy`, which works offline because every module needed is in the machine's module cache (checked while writing this plan; see Rulings). Never query proxy.golang.org or sum.golang.org. `sh scripts/module.sh check` prints `module: ok` after every task. The only modules this phase adds are `golang.org/x/crypto v0.58.0` (Task 6) and `golang.org/x/term v0.47.0` as a direct requirement (Task 9).
- `<testenv>` is `env -u ROTA_CLAUDE_HOME -u CLAUDE_CONFIG_DIR -u ROTA_ACCOUNT_ID -u ROTA_ACCOUNT -u ROTA_PROVIDER -u ROTA_TOKEN ROTA_HOME="$(mktemp -d)"`, and it prefixes every `go test`.
- **The local CI run** is, from the repository root:
  1. `test -z "$(gofmt -l .)"`
  2. `for os in linux darwin windows; do GOOS=$os go vet work || echo "vet $os FAILED"; done`
  3. `<testenv> go test -count=1 work`
  4. `<testenv> go test -count=1 -race work`
  5. `sh scripts/module.sh check`

  It passes when gofmt and vet print nothing, every test line is `ok`, and the last line is `module: ok`. Never `go test ./...` from the root, which skips the program. A timing-sensitive failure is rerun alone with `-count=3` before it counts, and named in the report.
- Docker is the integrator's alone. A task marked **docker** is queued for the integrator's serial `docker run --rm -v "$PWD":/src -w /src golang:1.27 sh -c 'env -u CLAUDE_CONFIG_DIR ROTA_HOME=$(mktemp -d) go test -count=1 -race work'` before its group is merged.
- Tests never touch the real `~/.rota`, `~/.claude`, `~/.ssh`, `~/.codex` or the keychain, never start a real claude, codex or grok, and never run the installed `rota` or the system's `ssh`. SSH tests use the `golang.org/x/crypto/ssh` client with keys generated in the test, on `127.0.0.1:0`. Fake CLIs come from `internal/fakecli` (the tty fake via `Spec{Tty: true}`), and a test that waits for one to come up does so through `fakecli.Up` or `fakecli.WaitReady`. Process-level tests in `cmd/rota` set `ROTA_AS_RUN=1`, use `shortHome(t)` and `newDaemonRig(t)`, and drive the person's terminal with `internal/pty` on a pty master.
- Linux comes first, then macOS. Windows must build and vet, and its tests pass or skip. On Windows, `rota run --attach` refuses with `holdwire.ErrPlatform` (`held sessions and the rota daemon run on linux and macOS`), and foreground `rota serve` with `[ssh] enabled` refuses with `the SSH server runs on linux and macOS`. A file that touches a signal, a termios or a unix socket carries `//go:build linux || darwin` and has a twin for other platforms.
- Natural by default: `[ssh] enabled` is false, and nothing listens unless the file says so. An auto-started daemon (`--ctl-only`) opens no SSH port, whatever the file says. With no `--attach` and no `[ssh]`, every command prints byte-identical output and JSON, and no existing test's expectation changes except where a task says so.
- No token, key material or conversation text reaches argv, a log, an SSH answer or an event. SSH logs name the user, the key's SHA256 fingerprint and the address, never a key, and log the command word without its arguments.
- Values copied from the spec:
  - `[ssh] listen` defaults to `127.0.0.1:2222`; non-loopback is allowed. `host_key` defaults to `<store>/ssh_host_ed25519_key`, made `0600`.
  - SSH takes public keys only, with no passwords and no keyboard-interactive, and three tries. It opens only `session` channels. Forwarding, agent, subsystems and any `env` but `TERM` are refused.
  - The SSH commands are a fixed list, `list`, `attach <id>`, `watch <id>` and `new [flags]`, and nothing else is parsed. There is no shell over SSH beyond what `[terminal] shell = true` already allows.
  - The chord is `Ctrl-\`, read as the byte 0x1C, as kitty CSI-u, as modifyOtherKeys and across split reads. Its keys are `d` leave, `n`/`p` next/previous session, `k` take the keyboard, `z` fit to this window, `?` show the keys, and `Ctrl-\` again to send it through. With no key within about 400 ms, one hint row is drawn and later repainted. The chord is shown once before a person's first attach, and the key is configurable.
  - Attaching never resizes; only `Ctrl-\ z` fits. Attaching clears the viewer's terminal first. Detaching waits briefly for output to drain, resets every mode (alt screen, cursor keys, bracketed paste, mouse and its encoding, focus, kitty flags on both screens, cursor), and prints one line: why it ended, and how to come back. A viewer smaller than the session gets a cropped render from the emulator with a note, never raw bytes wrapped.
- No new verb: `run` gains `--attach`, and the SSH server lives inside `rota serve` and the daemon. Every new flag of package `main` goes into `docs/reference.md` in the task that adds it, or `docs_test` fails.
- New files stay under about 600 lines.
- Two tasks in one parallel group may share a package (`attach`, `sshd`), never a file. Such a task declares no top-level name beyond those in its Interfaces block, except names that begin with its file's stem (for example `chordFlush` in `chord.go`).
- Release-note bullets for this phase are written by Task 13 alone, under `### New in the next beta` in `docs/releases/1.4.0.md`. Released sections stay byte-identical (`git diff b9fd476 -- docs/releases/` shows only additions under that heading). No version bump, no tag, no merge to main.
- Branches: each task runs on `p2-t<N>`, cut from the head of the integration branch `phase-2`. `phase-2` starts at this plan's commit with `bl-screen` merged in: its reviewed head, 2e3f2f4 when this was written. The integrator merges a whole group into it before the next group is cut. Every git command uses `git -C <absolute path>`. Never force-push.
- Commit subjects are one plain sentence of what is now true, with no body, no trailer and nothing about AI or assistants.

## Review Focus

1. **The terminal is left broken** after an attach ends any way but `Ctrl-\ d`: the session ends, the daemon is killed or restarted, the SSH connection drops, `rota` gets SIGHUP or SIGTERM, a write fails. A person expects their terminal back: cooked mode, no mouse reporting, no bracketed paste or kitty flags, cursor shown, main screen, and one line saying why. Tests: Task 7 `TestEveryEndGivesTheTerminalBack`, Task 9 `TestLocalRestoresWhatItChanged`, Task 10 `TestAttachGivesTheTerminalBack` and `TestASignalGivesTheTerminalBack`.
2. **The chord is misread**: split across reads, sent as kitty CSI-u or modifyOtherKeys with press, repeat and release events, typed inside a bracketed paste, or `Ctrl-\ Ctrl-\`. A person expects exactly one action per chord, and every other byte typed exactly once and in order. Tests: Task 2 `TestEverySplitGivesTheSameEvents`, `TestAPasteIsNeverAChord` and `FuzzChord`.
3. **A phone attaches to the desk's session**: a 40×12 SSH window on a 100×30 session, a window resized while attached, a session fitted elsewhere while cropped. The desk must never be resized except by `z`, and the phone's screen must never hold a row longer than its window. Tests: Task 4 `TestACropNeverWritesPastTheWindow`, Task 11 `TestAPhoneNeverResizesTheDesk`.
4. **Hostile SSH input**: an unknown key, password or keyboard-interactive tries, port forwarding, agent forwarding, `env LD_PRELOAD=…`, an `sftp` subsystem, `list; rm -rf ~` as a command, a 1 MiB command line, a flood of connections that never authenticate. Each must be refused with nothing run, and the daemon must carry on. Tests: Task 6 Step 1, all of it.
5. **Keystrokes cross a boundary**: keys typed while `Ctrl-\ n` switches sessions, keys typed while a lost daemon is being redialled, or a watcher's keys. Nothing typed for one session may reach another, and a reconnect repaints but never replays input. Tests: Task 7 `TestSwitchingCarriesNoKeys` and `TestNothingTypedWhileAwayIsSent`, Task 10 `TestAttachSurvivesADaemonRestart`.

## Rulings

Each line says what was decided where the design is silent, why, and the cost if wrong.

1. **The attach keeps its own copy of the screen.** Every attach feeds the session's bytes into a local `hold.Screen` and `vtscan.Scanner` at the session's size. That copy repairs overlays, draws the crop, and repaints after a reconnect or a switch. `api` gains no "repaint me" frame. Why: one mechanism serves crop, hint row, help card and switching, and the API stays as shipped. Cost: about 0.5 MB and an emulator's CPU per viewer, and SSH viewers' copies live in the daemon. If it is wrong, an inbound `{"type":"repaint"}` can be added later; it is additive.
2. **How the crop works.** It is bottom-anchored, and it moves sideways only to keep the cursor in view. The last row of the window is the note. Mouse reporting is off while cropped, because its coordinates would point at the wrong cells. The crop draws on the window's alternate screen. Why: Claude Code draws its prompt at the bottom of the main screen. Cost: a full-screen program that draws at the top shows its bottom half first; `z` fixes it.
3. **The chord key is `[terminal] detach_key`** in `server.toml` (default `ctrl-\`). The daemon writes it into `daemon.pid`, so a local attach and an SSH attach agree. This is an exported `api.TerminalSection` field, so it is also listed under *For the owner*; the plan proceeds with it.
4. **An SSH principal is `{Name: user, Role: role, Via: "user"}`.** That is the same person rule as a page sign-in (`personOf`), so the keyboard follows one person across the web page, SSH and `ctl.sock`. Cost: `Via` cannot tell SSH from the page; sshd's own log lines say `ssh`.
5. **The SSH login name is not checked; the key decides who it is.** It is logged beside the user. Why: the key is the credential, as with `git@github.com`. Cost: none for security; a confusing log line at worst.
6. **Accepted key types**: `ssh-ed25519`, `ecdsa-sha2-nistp256/384/521`, `sk-ssh-ed25519@openssh.com`, `sk-ecdsa-sha2-nistp256@openssh.com`, and `ssh-rsa` keys of at least 2048 bits signed with SHA-2 only. Refused: certificates, `authorized_keys` options, DSA, and SHA-1 `ssh-rsa` signatures. The host key is ed25519 alone. Cost: a very old client cannot sign in.
7. **SSH limits.** The handshake and sign-in must finish within 20 s. At most 32 connections may be unauthenticated and 64 open; each may have 8 channels; a command line is at most 4096 bytes. Keepalives go every 30 s, and three missed close the connection. The version string is `SSH-2.0-rota`, with no version number. Cost: a person with more than 64 devices.
8. **The host key file.** It is created only at the default path; a `host_key` the file names must already exist. Either is refused unless only its owner can read it (`internal/owner`).
9. **`[ssh] enabled` with no `[[users]]` entry holding `ssh_keys` is refused by `Check`**, because nobody could sign in. Cost: one more line to write.
10. **A bare `ssh host` in this phase** prints the session list and the commands, then exits 0. Phase 3 sets `sshd.Options.TUI`.
11. **SSH `new`** takes `--account N`, `--label NAME`, `--cwd DIR`, `--shell`, `--json` and `-- <CLI args>`, and refuses a prompt, `-p` and `--print` as `--detach` does. It attaches when the channel has a pty, and otherwise prints the session. Its size is the pty's, or else 120×40. It runs with the daemon's environment, since `env` and `remote_control` are taken only over `ctl.sock`, so this phase has no Remote Control over SSH.
12. **The `list` format.** One line per session: `<id>  <status, or state when stopped>  <name>  <provider>/<account label>  <cwd>`, each value cleaned. `list --json` prints the documents of `GET /v1/terminals`.
13. **Exit codes, for `run --attach` and SSH alike.** 0 for a detach. When the session ends, the CLI's code clamped to 0–255, with -1 (interrupted) as 1. 1 for a lost or refused attach, and 2 for a usage error.
14. **Timings**, all test knobs:
    - output drain on detach: 150 ms
    - a partial escape sequence at the end of a read is held 25 ms
    - hint row after 400 ms
    - a note shows for 3 s; the first-attach card for 5 s or until a key
    - redial for 60 s, every 500 ms
    - a forced claim after 10 s, mirroring the server's `claimWait`
    - `Settle` waits for the DA1 answer at most 1 s
    - the crop is redrawn at most every 30 ms
15. **The chord's second key** may come as the letter, its capital, or with Ctrl held. An unknown key ends the chord, is dropped, and leaves the hint row up for its 3 s.
16. **`claim_request` is shown as a note.** The attach never grants or denies; the page does. Cost: a desk user cannot hand the keyboard over from a terminal.
17. **Switching sessions drops the keys typed between the chord and the new session's `hello`.** That is the anti-goal "a keystroke typed in one session lands in another" taken strictly.
18. **A watcher's keys are dropped client-side**, and the chord is still read. This spares the server a refusal per key.
19. **`n`/`p` order**: live, attachable sessions, oldest started first, wrapping round.
20. **`--attach` takes no account, no arguments, and none of `--detach`, `--share`, `--label` or `--remote-control`.** There is no `--watch` flag in the CLI this phase; SSH has `watch`, and the TUI's `w` uses `Options.Watch`.
21. **Foreground `rota serve` with `[ssh] enabled` needs `[routes] terminal`**, because SSH reaches terminals and nothing else. Its terminals are `localTerm`, attached by ring replay into the local screen copy.
22. **Raw mode in `attach` uses `golang.org/x/term`**, since package `attach` cannot import `main`'s `rawMode`. `--share` keeps its own. Cost: two raw-mode helpers until one is retired.
23. **The WebSocket client is written by hand** in `cmd/rota/client`, about 250 lines and no new dependency, as `api/ws.go` is.
24. **The first-attach marker** is `<terminals>/taught`, mode 0600, one person's name per line. Cost: a person seen under two names is shown the card twice.
25. **`attach.OneLine` is a copy of `main`'s `oneLine`**, because `bl-tables` is editing `width.go`. A later change can make `main` call it.
26. **The SSH version check.** `golang.org/x/crypto v0.58.0` (2026-10-09) needs `x/sys v0.49.0` and `x/term v0.47.0`. A throwaway module in the scratchpad tidied and built offline from the cache, with `GOPROXY=off GOSUMDB=off`, and nothing in the repository changed. Its go.sum lines, which Task 6's go.sum must contain:
    ```
    golang.org/x/crypto v0.58.0 h1:COkYLr4k7nDI4r6QtMML2AfzeIswWHAJthLS6K/M/54=
    golang.org/x/crypto v0.58.0/go.mod h1:Xh+kl5A+M0gMiWDytH2Hhr5Z7WbNCpv/eB/EFgfSk+w=
    golang.org/x/sys v0.49.0 h1:XbzkgYJHdqh/8m2Uu0W/dQv8nktxx4BFHp1M0gROTXA=
    golang.org/x/sys v0.49.0/go.mod h1:hNLxWAXmnKAxqDtdwIYC4bM9oQPEecfsnNMuSxOs3og=
    golang.org/x/term v0.47.0 h1:cpyOwQkr2Nzxe2p6KnrTG7Ei2wtzjsuSjhpXXepZ/BM=
    golang.org/x/term v0.47.0/go.mod h1:efAfAuG6RKABvg1ArGebRkvX6LXul14SM67o2e4mUCY=
    ```
27. **`sshd` imports neither `main` nor phase 3's TUI.** The TUI is reached through `Options.TUI`, a field that `cmd/rota` sets where it builds the options, not a package variable, so a test sets it per server.
28. **A user with `ssh_keys` and no password** is valid in `server.toml` and in `api.Options.Users`. It cannot sign in on the page, and its sign-in is refused exactly as a wrong password is: the same message, the same derivation time, and the same limiter count.
29. **A holder's screen that comes back repaints every viewer** (Task 14). `bl-screen` brings a retired screen back but tells nobody, so a viewer attached while it was retired keeps the ring replay it was given. The new signal is a `Screen` field in the holder's `State`. On its change from false to true, the daemon marks each viewer behind, and the existing *Falling behind* path sends `repaint` and the screen. The attach (Task 7) and the web page already handle that, so neither changes. An older holder never sends the field, so it never triggers. Cost: one spare repaint for each viewer when a screen comes back, at most four a second.

## For the owner

Both are decided below by the plan's recommendation, so nothing waits. Say otherwise and the named task changes before it runs.

1. **Where the detach key is set** (permanent, because it adds exported surface to a published module). Task 5 and Task 9 implement option (a).
   - (a) **Recommended:** `[terminal] detach_key` in `server.toml`, an `api.TerminalSection.DetachKey` field. One place serves local and SSH attaches, and the daemon passes it on through `daemon.pid`.
   - (b) `[ssh] detach_key`. No new `[terminal]` field, but local attaches read an SSH key.
   - (c) An environment variable, `ROTA_DETACH_KEY`, read by `rota run --attach` only. SSH stays `Ctrl-\`. No API surface.
   - (d) A flag, `--detach-key`, on `run --attach` and on SSH `attach`. No API surface, but it is typed every time.
   - (e) Fixed `Ctrl-\` in 1.4.0, configurable later. This departs from the spec's "the detach key is configurable".
2. **An agent inside a held session can sign in over SSH** with this user's own key, whether it sits in `~/.ssh` or an agent socket, and so escapes `ctl.sock`'s watch-only rule. That rule is "a guard against accidents, not a wall"; using a key is not an accident. Task 13 documents option (a).
   - (a) **Recommended:** document it as the same limit as the double-fork escape.
   - (b) On Linux, find the peer process of a loopback SSH connection through `/proc/net/tcp` and its inode, and make an inside caller watch-only. Linux only.
   - (c) Refuse SSH from loopback entirely. This breaks SSH from this machine itself, and this phase's own tests.
   - (d) Scrub `SSH_AUTH_SOCK` from held CLIs. This breaks `git` over SSH inside sessions.
   - (e) `[ssh] loopback = false` as an opt-in refusal.

## What phase 2 needs and gives

**Needs, all on main at b9fd476:**
- `api.Server.LocalHandler(*Principal)` and `personOf` (phase 1 Tasks 8 and 9).
- The terminal WebSocket with `repaint`, `keyboard`, `resized`, `fit` and the held keyboard rules (phase 1 Tasks 6 and 8).
- `client.Dial`, `Match`, `Resolve`, `List`, `Create` and `Session` (phase 1 Tasks 10 and 12).
- `hold.Screen` (`NewScreen`, `Write`, `Refill`, `Resize`, `Repaint`, `Cursor`, `OK`, `Close`), `holdwire.ScreenFits`, `holdwire.RunningDaemon`, `vtscan.Scanner`, `vtscan.ResetAll` and `vtscan.ResetModes`.
- `runDaemon`, `daemonPort`, `newDaemonRig`, `shortHome` and `holdFake`.
- From `bl-screen` (2e3f2f4, not yet on main): a retired screen comes back, and `screenNow()` and `renew` in `hold` (Task 14).

**Gives to phase 3 (TUI shell, Accounts, Ask).** Agreed with `plan-p3` on 2026-11-10:
- `client.InProcess(h http.Handler) (c *Client, stop func())` and `(*Client).HTTP() *http.Client`, so the TUI speaks every `/v1` route over `ctl.sock` or in process (Task 1).
- `attach.Command(c *client.Client, id string, o attach.Options) *attach.Cmd`, the `tea.ExecCommand` shape: `Run() error`, `SetStdin`, `SetStdout`, `SetStderr`, plus `SetWindow(size func() (cols, rows int), resized <-chan struct{})` for a TUI on SSH, and `Result() attach.Result`. `Ctrl-\ n/p` switch inside the attach. `Run` returns nil on a detach or an end, and the `Result` names the last session shown (Task 10).
- `attach.Options{Key, Watch, Settle, Teach, Again, Redial}`. The TUI sets `Settle` (stale replies dropped up to one DA1 answer) and `Teach: attach.TaughtFile(terminals)` (Tasks 3 and 7).
- `attach.DaemonKey(terminals string) byte` (Task 9).
- `sshd.Options.TUI func(ctx context.Context, s sshd.TUISession) error` and `sshd.TUISession{In, Out, Env, Cols, Rows, Resize, Client, Key}` (Task 6). The field is set in `cmd/rota/sshwire.go`'s `sshOptions` (Task 12). While it is nil, a bare `ssh` prints the list (Task 8).

**Gives to phase 4 (Sessions, moving a chat, Services):**
- `(*hold.Screen).Crop(x, y, cols, rows int) (hold.Window, bool)` and `(*hold.Screen).Size()`. The Sessions preview "renders the session at its own size and crops; it never resizes it": a watch-mode `client.Term` feeds a `hold.Screen`, and `Crop` gives styled rows a Lip Gloss box can hold (Task 4).
- `client.(*Client).Attach(id, true)` for that preview stream (Task 1).
- `holdwire.Daemon.SSH` and `holdwire.Daemon.SSHHostKey`, and `serve status --json`'s `"ssh":{"addr","host_key"}`, for the Services tab's "web and SSH addresses" (Task 12).

**Gives to phases 5 to 7:** an SSH viewer is an ordinary viewer of the terminal socket, so `viewer.joined` and `viewer.left` (phase 5) need nothing from here. Phase 7's reference pass finds the SSH and attach sections written (Task 13).

## Lanes running now

- **`bl-screen`** (a retired screen comes back; a repaint counts the size it is taking) is on branch `bl-screen` at 2e3f2f4, under review. Its report is `scratchpad/wave/bl-screen-report.md`.
  - It changed `cmd/rota/hold/emulate.go` and `session.go`, and nothing exported, on the wire or in `holdwire`.
  - Inside `hold`, `s.screen` can now be replaced, so it is read under `s.mu` or the emulator lock, or through `screenNow()`. `catchUp(limited bool) bool`, `batch(limited bool)`, `sized(z sized)` and `drawLimit(cells int64)` changed. `renew`, `renewFrom`, `screenNow`, `renewAfter`, `em.sizedAt` and `em.broken` are new.

  Phase 2 is built on it: `phase-2` contains it before group A is cut. Two tasks touch `cmd/rota/hold`:
  - Task 4 adds `crop.go`, which reads only `Screen`'s own fields (`mu`, `ok`, `em`, `settle`, `catch`). `bl-screen` left `screen.go` alone.
  - Task 14 consumes `screenNow()` and `renew` to say when the screen comes back.

  Nothing here plans `bl-screen`'s work.
- **`bl-holder`** (the daemon's 45 s SIGTERM backstop for holders) has so far changed `cmd/rota/hold/main.go`, which no task here touches. Tasks 9 and 12 change `cmd/rota/daemon_unix.go`. If `bl-holder` lands there first, those tasks are cut after it. Its conflict, if any, is in `runDaemon`'s stop path, which Task 12 also orders.
- `bl-tables` edits `width.go` and `held_list.go`, which no task here touches (Ruling 25).

## Parallel groups

| Group | Tasks, each with no file in common | Needs |
|---|---|---|
| A | 1 client terminal socket and in-process transport · 2 the chord · 3 the attach contract and its words · 4 the views (crop) · 5 settings · 6 the SSH server's transport and policy | main |
| B | 7 the attach loop · 8 SSH `list`, `new` and a bare `ssh` · 9 the local terminal and the chord key in `daemon.pid` · 14 a screen that comes back repaints its viewers | A |
| C | 10 `rota run --attach` and `attach.Command` · 11 SSH `attach` and `watch` | B |
| D | 12 SSH in the daemon and in foreground serve · 13 the manual and the notes | C |

The critical path is four groups long: 1 or 4 → 7 → 10 or 11 → 12. Task 13 writes from this plan's fixed strings and is checked against Task 12's code at integration.

## File Structure

| Path | Task | Responsibility |
|---|---|---|
| `cmd/rota/client/ws.go`, `term.go`, `inproc.go` (create); `client.go` (a `dial` field) | 1 | The terminal WebSocket; `Client` over any connection, `ctl.sock` or in-process |
| `cmd/rota/attach/chord.go` (create) | 2 | `Ctrl-\` and its second key, in every encoding and across reads |
| `cmd/rota/attach/types.go`, `lines.go`, `word.go` (create) | 3 | The package's contract (`Terminal`, `Options`, `Result`) and every sentence and overlay it draws |
| `cmd/rota/hold/crop.go`, `cmd/rota/attach/view.go`, `viewport.go` (create) | 4 | The session's screen at the session's size, passed through or cropped |
| `api/config.go`, `api/server.go`, `api/session.go`, `internal/vtscan/key.go`, `docs/server.toml` | 5 | `[ssh]`, `ssh_keys`, `detach_key`, the SSH-only user, the key names |
| `cmd/rota/sshd/{sshd,hostkey,keys,channel,words,dispatch}.go` (create); `cmd/rota/go.mod`, `go.sum`; `README.md`; `docs/reference.md` (the dependency sentence) | 6 | The SSH server: keys, limits, what a channel may ask, and which command runs |
| `cmd/rota/attach/run.go`, `link.go`, `settle.go`, `taught.go` (create) | 7 | The loop: link, view, chord actions, overlays, switching, redial, the end |
| `cmd/rota/sshd/{list,new,tui}.go` (create); `dispatch.go`; `cmd/rota/client/client.go` (`Create.Kind`) | 8 | SSH `list`, `new` and a bare `ssh` |
| `cmd/rota/attach/local_unix.go`, `local_other.go`, `daemonkey.go` (create); `internal/holdwire/daemon.go`; `cmd/rota/daemon_unix.go`; `cmd/rota/go.mod`, `go.sum` | 9 | The person's own terminal, and the chord key from the daemon |
| `cmd/rota/attach/command.go`, `cmd/rota/attach_cli.go` (create); `cmd/rota/share.go`, `main.go`, `detach.go`; `docs/reference.md` (the flag) | 10 | `rota run --attach` and the `tea.Exec` adapter |
| `cmd/rota/sshd/attach.go` (create); `dispatch.go`, `new.go` | 11 | SSH `attach` and `watch` |
| `internal/holdwire/wire.go`; `cmd/rota/hold/session.go`, `emulate.go`; `api/held.go`, `api/fakeholder_test.go`; `docs/reference.md` (*Falling behind*) | 14 | A holder says whether its screen is kept, and a screen that comes back repaints the viewers |
| `cmd/rota/sshwire.go` (create); `daemon_unix.go`, `daemon.go`, `main.go`; `internal/holdwire/daemon.go` | 12 | The SSH listener in both servers, and `serve start` and `status` |
| `docs/reference.md`, `docs/releases/1.4.0.md` | 13 | The manual and the notes |

---

### Task 1: The terminal socket, and a client in process (`cmd/rota/client`)

**Files:**
- Create: `cmd/rota/client/ws.go` (the RFC 6455 client: handshake, masked frames out, unmasked in, ping, pong and close), `cmd/rota/client/term.go`, `cmd/rota/client/inproc.go`, `cmd/rota/client/term_test.go`, `cmd/rota/client/inproc_test.go` and `cmd/rota/client/main_test.go`. The last holds `TestMain`, which calls `fakecli.Maybe()`; the package has none yet.
- Modify: `cmd/rota/client/client.go`. `Client` gains `dial func(ctx context.Context) (net.Conn, error)`, set by `newClient` to `dialOwn` on its socket and used by its `http.Transport`. Nothing else in the file changes.

**Interfaces:**
- Consumes `api.Server.LocalHandler`, and the terminal socket as `docs/reference.md` *The socket* describes it.
- Produces:
  - `func InProcess(h http.Handler) (c *Client, stop func())`. It is an `http.Server` serving `h` on a listener of `net.Pipe` connections. `stop` closes the listener and every connection it accepted, hijacked WebSockets included, and is safe to call twice.
  - `func (c *Client) HTTP() *http.Client`
  - `func (c *Client) Attach(id string, watch bool) (*Term, error)` opens `GET /v1/terminals/{id}/ws`, with `?mode=watch` when `watch`, and no `since`. An answer that is not 101 is a `*Refusal`, as `do` makes one.
  - `type Term`. `func (t *Term) Hello() Hello` is the first frame, read by `Attach`. `func (t *Term) Next() (Frame, error)` answers pings itself. `func (t *Term) Type(p []byte) error` sends a binary frame. `func (t *Term) Fit(cols, rows uint16) error` sends `{"type":"resize","cols":…,"rows":…,"fit":true}`. `func (t *Term) Claim(force bool) error` sends `{"type":"claim","force":…}`. `func (t *Term) Close() error` sends close 1000 and is safe concurrently with a blocked `Next`, which then returns. Writes are serialised. `Next` is called from one goroutine.
  - `type Hello struct { ID, Kind string; Cols, Rows uint16; Offset int64; Holder, HolderConn string; You Viewer; Repaint bool }` and `type Viewer struct { Name, Role, Conn string }`. A `null` holder is `""`.
  - `type Frame struct { Output []byte; Doc *Doc }`. Exactly one is set.
  - `type Doc struct { Type, Holder, HolderConn, Message, By, ID string; Cols, Rows uint16; Offset, From, To, Bytes int64; Code int }`
  - `type Closed struct { Code int; Reason string }`, an `error`, returned by `Next` for the server's close frame. An EOF without one is `io.ErrUnexpectedEOF`.
  - `const maxFrame = 1 << 20`. A longer frame from the server is an error, and the connection is closed.

- [ ] **Step 1: Write the failing tests.** They run against a real `api.Server` with `Routes{API, WebSocket, Terminal: true}`, served through `InProcess(srv.LocalHandler(&api.Principal{Name: "me", Role: api.RoleControl, Via: "user"}))`. The store is a temp `ROTA_HOME` whose `accounts.json` holds account 1 (claude, token `t`), and the tty fake is installed as `claude` on `PATH`. `main_test.go`'s `TestMain` calls `fakecli.Maybe()`.
  - `TestInProcessServesTheHandler`: `GET /v1/session` through `c.HTTP()` answers `{"name":"me","role":"control","via":"user"}`. After `stop()`, a request fails and a second `stop()` does not panic.
  - `TestAttachSpeaksTheTerminalSocket`: `Create` a terminal at 90×20. `Attach` gives a `Hello` with that id and 90×20, and output with `ready 90x20` arrives within 5 s. `Type([]byte("hi\r"))` gives output `got:hi`. `Fit(70,15)` gives a `Doc{Type:"resized",Cols:70,Rows:15}`. `Close()` ends `Next` with an error within 1 s.
  - `TestWatchTypesNothing`: with `watch` true, `Type("x\r")` is answered by a `Doc{Type:"error"}`, and no `got:x` follows.
  - `TestAnUnknownSessionIsARefusal`: `Attach("0000000000000000", false)` gives a `*Refusal` with `Status` 404.
  - `TestFramesSurviveOneByteReads`: a `net.Conn` wrapper that returns one byte per `Read` sits under `Term`. Output and documents still arrive whole.
  - `TestAServerCloseIsSaid`: the server stops (`srv.Stop()`). `Next` returns a `*Closed` with `Code` 1001.
  - `TestAnOversizedFrameIsRefused`: a hand-written peer sends a 2 MiB binary frame. `Next` errors and the connection is closed.
  - `TestPingsAreAnswered`: a hand-written peer sends a ping with payload `p1`, then output `x`. It reads back a masked pong carrying `p1`, and `Next` returns the output `x`, with no frame for the ping.
- [ ] **Step 2: Run them.** `<testenv> go test -count=1 ./cmd/rota/client/` fails: `InProcess`, `Attach` and `Term` do not exist.
- [ ] **Step 3: Implement.** The handshake sends `Sec-WebSocket-Version: 13`, a random 16-byte key and `Host: rota`, and checks `Sec-WebSocket-Accept`. Client frames are masked with a fresh 4-byte key each. A text frame is decoded into `Doc`, and `hello` only into `Hello`. Fragmented messages are reassembled up to `maxFrame`.
- [ ] **Step 4: Verify.** The local CI run passes. This is a **race** task.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota/client
  git -C <worktree> commit -m "The client attaches to a terminal over its WebSocket, and reaches any handler in process as well as over the control socket"
  ```

### Task 2: The chord (`cmd/rota/attach/chord.go`)

**Files:**
- Create: `cmd/rota/attach/chord.go` and `cmd/rota/attach/chord_test.go`. There is no package doc comment; Task 3 writes it.

**Interfaces (all unexported; Task 7 is the only user):**
- `type chord struct`, made by `func newChord(key byte) *chord`. `key` is the prefix's control byte, 0x1C by default.
- `func (c *chord) feed(p []byte, now time.Time) []chordEvent`, `func (c *chord) tick(now time.Time) []chordEvent`, `func (c *chord) due() (time.Time, bool)`, `func (c *chord) armed() bool` and `func (c *chord) reset()`. `reset` drops held bytes and the armed state, for a switch.
- `type chordEvent struct { act action; bytes []byte }`, and `type action int` with `actType` (bytes to type), `actLeave`, `actNext`, `actPrev`, `actKeyboard`, `actFit`, `actHelp`, `actHint` and `actCancel`.
- The knobs `var hintAfter = 400 * time.Millisecond` and `var holdFor = 25 * time.Millisecond`.

The decisions, which the tests pin:
- **The prefix** is any one of:
  - the byte `key`
  - kitty CSI-u: `ESC [ <cp> ; <mods> [: <event>] u` with ctrl in the modifiers (mods−1 has bit 4) and event 1 or 2 (press or repeat)
  - modifyOtherKeys: `ESC [ 27 ; <mods> ; <cp> ~`

  `cp` is the key's character: `\` 92, `]` 93, `^` 94, `_` 95, a letter in lower case. For `^` and `_`, `6` (54) and `-` (45) with ctrl and shift also count.
- **While armed:** kitty release events (`:3u`) and the modifier keys' own codepoints (57441–57452) are ignored.
- **The second key** is a plain byte, an unmodified CSI-u (`ESC[<cp>u`, `;1u` or `;1:1u`), or the same letter with Ctrl held:
  - `d D ^D` → `actLeave`
  - `n N ^N` → `actNext`
  - `p P ^P` → `actPrev`
  - `k K ^K` → `actKeyboard`
  - `z Z ^Z` → `actFit`
  - `?`, or CSI-u 63 → `actHelp`
  - the prefix again, in any encoding → `actType` with exactly the bytes that second press came as
  - anything else → `actCancel`, and that key is dropped
- **Bracketed paste:** between `ESC[200~` and `ESC[201~` every byte is `actType`, and nothing is a chord.
- **Held bytes:** a trailing lone `ESC`, or a CSI cut off at the end of a read, is held. It goes when the next read completes it, or as `actType` once `holdFor` passes (`tick`). Nothing else is ever held.
- **The hint:** armed for `hintAfter` with no second key gives one `actHint`, and the chord stays armed.
- **Order:** events come in input order. Bytes not part of a chord are joined into as few `actType` events as order allows.

- [ ] **Step 1: Write the failing tests.**
  - `TestTheChordInEveryEncoding`, a table of input → events:
    - `"ab\x1cdcd"` → `[Type "ab", Leave, Type "cd"]`
    - `"\x1c\x1c"` → `[Type "\x1c"]`
    - `"\x1b[92;5u" + "d"` → `[Leave]`
    - `"\x1b[92;5:1u\x1b[92;5:3ud"` → `[Leave]`
    - `"\x1b[27;5;92~n"` → `[Next]`
    - `"\x1c\x1b[100u"` → `[Leave]`
    - `"\x1c\x04"` → `[Leave]`
    - `"\x1cZ"` → `[Fit]`
    - `"\x1c?"` → `[Help]`
    - `"\x1cx"` → `[Cancel]`
    - `"\x1c\x1b[92;5u"` → `[Type "\x1b[92;5u"]`
    - with key 0x1D: `"\x1c\x1dd"` → `[Type "\x1c", Leave]`
  - `TestEverySplitGivesTheSameEvents` (Review Focus 2): for every input in the table and every split point, `feed(a)`, then `feed(b)`, then `tick(+holdFor)` gives the same events as `feed(a+b)` once adjacent `actType` events are joined.
  - `TestAPasteIsNeverAChord` (Review Focus 2): `"\x1b[200~a\x1cdb\x1b[201~\x1cd"` → `[Type "\x1b[200~a\x1cdb\x1b[201~", Leave]`.
  - `TestALoneEscapeGoesAfterHoldFor`: `feed("\x1b")` gives nothing. `tick(+24ms)` gives nothing. `tick(+25ms)` → `[Type "\x1b"]`.
  - `TestTheHintComesOnceAfter400ms`: `feed("\x1c")`, then `tick(+399ms)` → `[]`, then `tick(+400ms)` → `[Hint]`, then `tick(+2s)` → `[]`, then `feed("d")` → `[Leave]`. `due()` names each deadline.
  - `TestResetForgetsTheChord`: `feed("\x1c")`, then `reset()`, then `feed("d")` → `[Type "d"]`.
  - `FuzzChord`: it never panics. The bytes of every `actType` event, joined, are always a subsequence of the input, except for send-through.
- [ ] **Step 2: Run them.** `<testenv> go test -count=1 ./cmd/rota/attach/` fails: `newChord` does not exist.
- [ ] **Step 3: Implement** `chord.go` as one incremental state machine (ground, ESC, CSI with parameters, paste), keeping at most 64 bytes of an open sequence. Anything longer is `actType` as it came.
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota/attach/chord.go cmd/rota/attach/chord_test.go
  git -C <worktree> commit -m "The attach chord is read as one byte, as kitty and modifyOtherKeys keys and across split reads, and never inside a paste"
  ```

### Task 3: The attach contract and its words (`types.go`, `lines.go`, `word.go`)

**Files:**
- Create: `cmd/rota/attach/types.go` (with the package doc comment), `cmd/rota/attach/lines.go`, `cmd/rota/attach/word.go`, `cmd/rota/attach/lines_test.go` and `cmd/rota/attach/word_test.go`

**Interfaces:**
- Consumes `client.Session` (existing) and `client.Client` (existing).
- Produces:
  - `const DefaultKey byte = 0x1c`
  - `type Terminal interface { io.Reader; io.Writer; Size() (cols, rows int); Resized() <-chan struct{} }`. `Read` is what the person types, `Write` is their screen, and `Resized` fires when their window changes.
  - `type Why int`, with `Detached`, `Ended`, `Lost` and `Refused` (from 1).
  - `type Result struct { Why Why; Session client.Session; Code int; Err error }`
  - `func (r Result) ExitCode() int` follows Ruling 13.
  - `func (r Result) Line(o Options) string` gives the one line. The name is `OneLine(r.Session.Name)`, or the id when that is empty. `again` is `o.Again(Word(r.Session))`, or `"rota run --attach " + Word(r.Session)` when `o.Again` is nil.
    - Detached: `rota: detached from <name>; <again> returns to it`
    - Ended with a code of 0 or more: `rota: <name> ended, exit <code>`
    - Ended with a negative code: `rota: <name> was interrupted: its holder went without its CLI's exit`
    - Lost: `rota: lost <name>: <OneLine(err)>; <again> tries again`
    - Refused: `rota: <OneLine(err)>`
  - `type Options struct { Key byte; Watch, Settle bool; Teach func(person string) bool; Again func(word string) string; Redial func() (*client.Client, error) }`, documented as in *What phase 2 needs and gives*.
  - `func KeyName(k byte) string`: 0x1C → `Ctrl-\`, 0x1D → `Ctrl-]`, 0x1E → `Ctrl-^`, 0x1F → `Ctrl-_`, 0x01–0x1A → `Ctrl-A`…`Ctrl-Z`.
  - `func Word(s client.Session) string` is what to type to reach `s`. It is the name when the name is non-empty, unchanged by `OneLine`, and matches `^[A-Za-z0-9._@%+=:,/-]+$`. Otherwise it is the name in single quotes when it is clean but needs quoting. Otherwise it is the id.
  - `func OneLine(s string) string` is `main`'s `oneLine`, copied (Ruling 25).
  - Unexported, for Task 7:
    - `func hintRow(key byte, cols int) string`
    - `func helpCard(key byte, teach bool) []string`
    - `func drawRow(row int, text string, cols int) []byte`: `ESC 7`, then the row, then reverse video, then the text cut or padded to `cols`, then `ESC[0m` and `ESC 8`
    - `func drawCard(lines []string, cols, rows int) []byte`: centred, with a one-cell border, cut to fit, saving and restoring the cursor as `drawRow` does
    - `func cropNote(name string, key byte, sessCols, sessRows, winCols, winRows int) string`

The copy is fixed. With K = `KeyName(key)`:
- `hintRow`: `K  d leave · n/p next/prev · k keyboard · z fit · ? help · K itself`
- `helpCard`, one string per line, with `teach` adding the first line:
  ```
  rota: to leave, press K then d; the session goes on
  rota: after K, press
    d        leave; the session goes on
    n  p     next, previous session
    k        take the keyboard
    z        fit the session to this window
    ?        these keys
    K        send K itself
  ```
- `cropNote`: `rota: <name> is <C>×<R>, this window <c>×<r>; K z fits it here`

- [ ] **Step 1: Write the failing tests.**
  - `TestTheLinesSayWhyAndHowBack`: one case for each `Why`, plus Ended -1 and a custom `Again` (`func(w string) string { return "attach " + w }`), each against its exact string above.
  - `TestExitCodes`: Detached → 0, Ended 3 → 3, Ended -1 → 1, Ended 300 → 255, Lost → 1, Refused → 1.
  - `TestWordQuotesOrUsesTheID`: `api` → `api`, `my api` → `'my api'`, `a\x1bb` → the id, `""` → the id.
  - `TestKeyNames`: the table above.
  - `TestDrawnRowsFitTheWindow`: `drawRow(24, hintRow(0x1c, 20), 20)` has exactly 20 printable cells between its escapes, and `drawCard` at 30×8 writes no row longer than 30 cells, measured with `x/ansi`'s `StringWidth`.
  - `TestOneLineCleansControls`: `"a\tb\nc\x1b"` → `"a b c "`.
- [ ] **Step 2: Run them.** They fail: the package has no `Result`.
- [ ] **Step 3: Implement** the three files.
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota/attach/types.go cmd/rota/attach/lines.go cmd/rota/attach/word.go cmd/rota/attach/lines_test.go cmd/rota/attach/word_test.go
  git -C <worktree> commit -m "An attach has one contract, a terminal, options and a result, and says in one fixed line why it ended and how to come back"
  ```

### Task 4: What the person sees: pass-through and crop

**Files:**
- Create: `cmd/rota/hold/crop.go`, `cmd/rota/hold/crop_test.go`, `cmd/rota/attach/view.go`, `cmd/rota/attach/viewport.go` and `cmd/rota/attach/view_test.go`

**Interfaces:**
- Consumes `hold.NewScreen`, `Write`, `Refill`, `Resize`, `Repaint`, `Cursor`, `OK` and `Close`. It also consumes `vtscan.NewScanner`, `Feed`, `Modes`, `ResetModes`, `Modes.Set` and `holdwire.ScreenFits`. It uses nothing from Tasks 2 or 3, which run beside it: the note's text comes in as a function, and the view draws its own last row.
- Produces, in `hold` (exported, because phase 4's preview uses it):
  - `type Window struct { Rows [][]byte; Row, Col int; CursorIn bool }`
  - `func (s *Screen) Crop(x, y, cols, rows int) (Window, bool)`: exactly `rows` entries. Each is the styled cells of that screen row from column `x` for `cols` cells. A row starts from `ESC[0m` and ends with `ESC[0m`, and has no cursor movement and no trailing blanks. Cells past the screen are blank. A wide character cut by either edge is a space. `Row` and `Col` are the cursor's place in the window, and `CursorIn` says whether it is inside. It is false, with nothing, when there is no emulator.
  - `func (s *Screen) Size() (cols, rows int)`
- Produces, in `attach` (unexported; Task 7 is the only user, from one goroutine):
  - `func newView(out io.Writer, sessCols, sessRows, winCols, winRows int, note func(sessCols, sessRows, winCols, winRows int) string) *view`. Task 7 passes a closure over Task 3's `cropNote`.
  - Methods: `output(p []byte)`, `resized(cols, rows int)`, `window(cols, rows int)`, `redraw()`, `flush() bool` (it says whether it drew), `cropped() bool`, `screenOK() bool` and `close()`.
  - `func viewport(sessCols, sessRows, winCols, winRows, curRow, curCol, prevX int) (x, y int)`
  - `var cropEvery = 30 * time.Millisecond`. Task 7 calls `flush` no more often than this.

The decisions:
- **The local copy.** The view keeps a `hold.Screen` at the session's size, created only when `holdwire.ScreenFits` allows, and a `vtscan.Scanner`. Every `output` is fed to both, as `hold.Session.catchUp` feeds its screen: cut at the scanner's `Ground`, the rest kept for the next `output`, with `Refill` once per call.
- **Pass-through,** when `winCols >= sessCols && winRows >= sessRows`. `output` writes the bytes to `out` unchanged. `redraw` writes `Screen.Repaint(scanner.Modes())`, or nothing when the screen is not OK.
- **Crop,** otherwise. Entering it writes `ESC[?1049h`. Then whenever modes change it writes `vtscan.ResetModes()` and `m.Set()`, with `m` the session's modes less `Mouse` and `MouseEncoding`. `output` writes nothing itself and marks the view dirty. `flush` draws only the rows whose bytes changed since the last draw, each as `ESC[<r>;1H` + row + `ESC[0m ESC[K`. Then the note on the last row, as `ESC[<rows>;1H ESC[7m` + `note(…)` cut to the window's width + `ESC[0m ESC[K`, and then the cursor at its place, or hidden when it is outside or `CursorHidden`. Leaving the crop writes `ESC[?1049l`, then `redraw`.
- **With no local screen** (too large, or panicked) a crop draws only the note on a cleared window, with the session's bytes never written raw.
- **`viewport`:** `shown = winRows-1`. `y` is `max(0, sessRows-shown)`, moved up just enough to keep `curRow` in view. `x` starts at `prevX`, is moved just enough to keep `curCol` in `[x, x+winCols)`, and is clamped to `[0, max(0, sessCols-winCols)]`.
- **Changes of size.** `resized` resizes the screen, or drops it when it no longer fits, and re-chooses the mode. So does `window`.

- [ ] **Step 1: Write the failing tests.**
  - `TestCropShowsAWindowOntoTheScreen` (hold): a 20×5 screen fed `ESC[2;1H0123456789abcdefghij ESC[3;1H` plus `ESC[31mred ESC[0m` and a wide `世` at column 9. `Crop(5,1,10,3)`:
    - row 0 has the text `56789abcde`
    - row 1 has `red` drawn from column 0 with `ESC[31m`
    - the `世` cut at the edge is a space
    - the cursor at screen (2,3) gives `Row 1, Col -2` with `CursorIn` false
  - `TestCropPastTheScreenIsBlank` (hold): `Crop(15,3,10,4)` on a 20×5 screen has four rows, the last two empty.
  - `TestViewportFollowsTheCursor`, a table:
    - (100,30,40,12, cur 29,5, prev 0) → (0,19)
    - cur (29,70) → x 31
    - cur (10,0) → y 10
    - a window as wide as the session → x 0
  - `TestPassThroughWritesTheBytesAsTheyCame`: view 80×24 on an 80×24 window. `output("hi\x1b[1m")` writes exactly those bytes. `redraw()` writes a repaint that contains `hi`.
  - `TestACropNeverWritesPastTheWindow` (Review Focus 3): a 100×30 session in a 40×12 window, fed 30 lines of 100 `x`s, with a note function that writes the four sizes and is 60 characters long. After `flush`, no row the view wrote, split on `ESC[<r>;1H`, holds more than 40 printable cells. The last row holds the note cut to 40, starting `100×30`, and no byte of the session's output reached `out` raw.
  - `TestACropDrawsOnlyWhatChanged`: after one `flush`, `output("\x1b[30;1Hz")` and `flush` write one row and the note.
  - `TestACropTurnsTheMouseOffAndKeepsTheKeys`: the session turns on `?1002h ?1006h ?2004h ?1h`. The cropped view writes `?2004h` and `?1h`, and no `?1002h`.
  - `TestTheWindowGrowingLeavesTheCrop`: `window(100,30)` writes `ESC[?1049l` and then a repaint, and later `output` passes through.
  - `TestNoScreenMeansANoteNotBytes`: a 1000×1000 session in an 80×24 window draws the note and never the session's bytes.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.** `crop.go` takes `s.mu`, runs `settle()` and reads `s.em` under `catch()`, as `Repaint` does. It does not edit `screen.go` (`bl-screen` owns it).
- [ ] **Step 4: Verify.** The local CI run passes. This is a **race** task.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota/hold/crop.go cmd/rota/hold/crop_test.go cmd/rota/attach/view.go cmd/rota/attach/viewport.go cmd/rota/attach/view_test.go
  git -C <worktree> commit -m "An attach keeps its own copy of the session's screen, passes bytes through when the window holds the session, and otherwise draws a crop that follows the cursor and says how to fit"
  ```

### Task 5: Settings: `[ssh]`, `ssh_keys`, `detach_key`, and a user who signs in by key

**Files:**
- Modify:
  - `api/config.go`: the types, `DefaultConfig`, `Check`, `checkPrincipals` and `Print`.
  - `api/server.go`: the `User` doc and `checkUsers`.
  - `api/session.go`: `signIn`.
  - `docs/server.toml`: the `[ssh]` block, `detach_key`, and a commented `ssh_keys` line in its users example.
- Create: `internal/vtscan/key.go`, `internal/vtscan/key_test.go` and `api/ssh_config_test.go`

**Interfaces:**
- `type SSHSection struct { Enabled bool; Listen string; HostKey string }`, with the toml tags `enabled`, `listen` and `host_key`, as `Config.SSH SSHSection` with tag `ssh`.
- `UserEntry.SSHKeys []string` with tag `ssh_keys`, and `TerminalSection.DetachKey string` with tag `detach_key`.
- `DefaultConfig` sets `SSH: SSHSection{Listen: "127.0.0.1:2222"}` and `Terminal.DetachKey: "ctrl-\\"`.
- `func vtscan.CtrlKey(name string) (byte, error)` reads `ctrl-` plus one character, case-insensitively. It accepts `a`–`z` except `h`, `i`, `j` and `m`, plus `\`, `]`, `^` and `_`, and returns the character's control byte.
- `api.User.Password` may be empty: such a user signs in by SSH key alone (Ruling 28). `api.Options` gains nothing; Task 12 reads `Config.SSH` and `UserEntry.SSHKeys` directly.

The decisions:
- **`Check`, in this order, after the existing principal checks:**
  - A `[[users]]` entry with an empty password and no `ssh_keys` keeps today's message: `password is empty; make one with `rota serve passwd <name>``.
  - Each key line is `<type> <base64> [comment]`. Its type is one of Ruling 6's. Its base64 decodes, and the type inside it, as the first SSH-wire string, equals the type written. A line with options before the key is refused. Each refusal is `[[users]] "<name>": ssh_keys #<n>: <why>`, with these whys:
    - `is not "<type> <base64> [comment]"`
    - `"<type>" is not a key type rota takes; use ssh-ed25519, ecdsa-sha2-nistp256/384/521, ssh-rsa, or a security key's sk- type`
    - `its base64 does not decode`
    - `says it is <a> and holds a <b> key`
    - `options before the key, such as from=, are not read; give the key alone`
  - The same key under two users is refused: `[[users]] "<b>": ssh_keys #<n> is also "<a>"'s; one key is one person`.
  - With `ssh.enabled`:
    - an empty `ssh.listen` is `ssh.listen is empty; write a host:port such as 127.0.0.1:2222`
    - otherwise `ListenAddr(ssh.listen)` must succeed, as `ssh.listen: …`
    - no user with `ssh_keys` is `ssh.enabled is true, and no [[users]] entry has ssh_keys, so nobody could sign in`
  - A non-empty `ssh.host_key` must be absolute: `ssh.host_key must be an absolute path, not %q`.
  - `terminal.detach_key` must pass `vtscan.CtrlKey`, or `terminal.detach_key %q is not a key rota can leave a session on; write ctrl- and one of a-z (not h, i, j or m), \, ], ^ or _`.
- **`Print`:**
  - `detach_key` goes after `share` in `[terminal]`.
  - `[ssh]` (`enabled`, `listen`, `host_key`) goes after `[terminal]` and before `[store]`.
  - Each user gets `ssh_keys = ["…", …]` after `password`, and only when the user has keys.
- **`checkUsers`** keeps a user whose password is empty, with `pw == nil`. **`signIn`** checks that user against `s.decoy` and refuses with the wrong-password answer.

- [ ] **Step 1: Write the failing tests.**
  - `TestSSHDefaultsAndPrint`: `DefaultConfig().SSH` is `{false, "127.0.0.1:2222", ""}`, and `Terminal.DetachKey` is `ctrl-\`. `Print` has the lines `enabled         = false`, `listen          = "127.0.0.1:2222"` and `detach_key      = "ctrl-\\"`, and `TestPrintIsAFileThisCanRead` still passes. `TestSampleFileIsTheDefaults` passes on the new `docs/server.toml`.
  - `TestKeysMakeThePasswordOptional`: a user `me` with role control, one ed25519 key and no password loads. The same user with neither key nor password gives today's message.
  - `TestKeyLinesAreChecked`: one case per refusal above, each against its exact message.
  - `TestSSHNeedsSomebodyToSignIn`: `enabled = true` with no keys → the message. An empty listen → the message. A relative `host_key` → the message.
  - `TestDetachKeyNames` (`vtscan`): `ctrl-\` → 0x1C, `CTRL-]` → 0x1D, `ctrl-a` → 0x01. `ctrl-m`, `ctrl-[`, `alt-x` and `ctrl-` are errors. The config refusal carries its message.
  - `TestAKeyOnlyUserCannotSignInOnThePage`: a server with `Users: []User{{Name: "me", Role: RoleControl}}`. `POST /v1/session` with `{"name":"me","password":""}` and with `"x"` both answer 401 `that name and password do not go together`, and the limiter counts both.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.** The key check reads the SSH wire format with the standard library alone: a 4-byte big-endian length, then the name.
- [ ] **Step 4: Verify.** The local CI run passes. `git diff b9fd476 -- go.mod` is empty.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add api internal/vtscan docs/server.toml
  git -C <worktree> commit -m "server.toml takes an [ssh] section, ssh_keys for each user, and the key that leaves an attached session, and a user with keys needs no password"
  ```

### Task 6: The SSH server: keys, limits and what a channel may ask (`cmd/rota/sshd`)

**Files:**
- Create: `cmd/rota/sshd/sshd.go` (`Options`, `Server`, `Listen`, `Serve`, `Close` and the limits), `hostkey.go`, `keys.go`, `channel.go` (one session channel: requests, `Channel` and `exit-status`), `words.go`, `dispatch.go` (`command`, `commands` and `usage`), `sshd_test.go`, `keys_test.go` and `words_test.go`
- Modify:
  - `cmd/rota/go.mod` and `cmd/rota/go.sum`: `go -C cmd/rota mod edit -require=golang.org/x/crypto@v0.58.0`, then `GOPROXY=off GOSUMDB=off sh scripts/module.sh tidy`. The go.sum holds Ruling 26's lines.
  - `README.md:12`: add `x/crypto/ssh` to the list of what the `rota` program uses (`… uses x/vt and x/crypto/ssh.`).
  - `docs/reference.md:48`: add the same to the sentence that names x/vt: `and golang.org/x/crypto/ssh for its SSH server`.

  Phase 3 adds Bubble Tea, Lip Gloss and Bubbles to the same two sentences. Whichever lands second extends them and never replaces them.

**Interfaces:**
- Consumes `api.Principal` and `api.Role` (existing), and `client.Client` (existing, as a type only).
- Produces:
  - `type User struct { Name string; Role api.Role; Keys []string }`
  - `type Options struct { Listen, HostKey string; NewHostKey bool; Users []User; Handler func(p *api.Principal) http.Handler; Log *slog.Logger; Key byte; Taught func(person string) bool; TUI func(ctx context.Context, s TUISession) error }`. `HostKey` is absolute. `NewHostKey` makes it when it is missing. `Handler` is the server's `LocalHandler`.
  - `type TUISession struct { In io.Reader; Out io.Writer; Env []string; Cols, Rows int; Resize <-chan [2]int; Client *client.Client; Key byte }`. `Env` holds only `TERM=…`.
  - `type Server` and `func Listen(o Options) (*Server, error)`, which binds, loads or makes the host key, and parses every user's keys. `func (s *Server) Serve() error` returns nil after `Close`. `func (s *Server) Addr() string`, `func (s *Server) Fingerprint() string` (`SHA256:…`), and `func (s *Server) Close() error`, which closes the listener and every connection and waits for their goroutines.
  - `type Channel struct { In io.Reader; Out, Err io.Writer; PTY bool; Term string; Cols, Rows int; Resize <-chan [2]int; User User; Login string; Handler http.Handler; Key byte; Taught func(string) bool }`. `Handler` is `o.Handler(&api.Principal{Name: User.Name, Role: User.Role, Via: "user"})`, made once per connection.
  - `type command func(ctx context.Context, ch *Channel, args []string) int` and `var commands = map[string]command{}`, which Tasks 8 and 11 fill.
  - `func words(line string) ([]string, error)` splits on spaces and tabs, honours single and double quotes, and has no escapes, globs or variables. An unbalanced quote is an error.
  - The knobs `var handshakeWait = 20 * time.Second` and `var keepaliveEvery = 30 * time.Second`. The constants `maxUnauthed = 32`, `maxConns = 64`, `maxChannels = 8` and `maxLine = 4096`.
  - `const usage` is the text a bare or wrong command prints:
    ```
    rota over ssh takes one of:
      list [--json]                      every session the daemon holds or lists
      attach <session>                   look and type; Ctrl-\ then d leaves
      watch <session>                    look only
      new [--account N] [--label NAME] [--cwd DIR] [--shell] [--json] [-- ARGS]
    ```

The decisions:
- **`ssh.ServerConfig`:**
  - `PublicKeyCallback` matches `key.Marshal()` against the parsed keys exactly. It returns `Permissions{Extensions: {"rota-user": name}}`, and on no match `errors.New("no")`.
  - `MaxAuthTries: 3` and `ServerVersion: "SSH-2.0-rota"`.
  - `PublicKeyAuthAlgorithms` is Ruling 6's list with `rsa-sha2-256` and `rsa-sha2-512` and without `ssh-rsa`. There is no `PasswordCallback` and no `KeyboardInteractiveCallback`.
- **Keys at start:**
  - `ssh.ParseAuthorizedKey` on each line. Options, certificates and RSA under 2048 bits are refused, naming `[[users]] "<name>" ssh_keys #<n>`.
  - The host key: with `NewHostKey` and no file, `ed25519.GenerateKey`, then `ssh.MarshalPrivateKey`, written 0600 through a temp file in the same directory and a rename. Otherwise the file must exist and pass `internal/owner`'s check.
- **Connections:**
  - `Accept` first counts connections. Past `maxConns`, or past `maxUnauthed` still signing in, a connection is closed at once.
  - The handshake runs under a `handshakeWait` deadline, cleared once signed in.
  - A keepalive global request (`keepalive@openssh.com`, want-reply) goes every `keepaliveEvery`, and three without an answer close the connection.
  - Global requests from the client are refused, except its own keepalives.
- **Channels:** only `session` is accepted, up to `maxChannels`; any other type is `ssh.Prohibited`. In a session channel:
  - `pty-req` records TERM and size, and is accepted.
  - `window-change` sends to `Resize`, dropping the oldest when the buffer of 4 is full.
  - `env` is accepted for `TERM` alone.
  - `shell` runs the bare command; `exec` runs `words` of the command, which must be under `maxLine`.
  - `subsystem`, `x11-req`, `auth-agent-req@openssh.com`, `signal` and `break` are refused.

  After the command returns, the channel sends `exit-status` and closes.
- **Dispatch:**
  - An empty line, or `shell`, is the bare command. Here it is `usage` on `Out` and exit 0; Task 8 changes that.
  - The first word is looked up in `commands`. Unknown, or a parse error, is `rota over ssh runs list, attach, watch and new, and nothing else` plus `usage` on `Err`, exit 2.
- **Logging:**
  - `ssh signed in` with name, login, fingerprint and address.
  - `ssh refused` with address and the fingerprints offered, at most three.
  - `ssh command` with name and the first word.
  - `ssh signed out` with name.

- [ ] **Step 1: Write the failing tests** (Review Focus 4). A test helper starts `Listen` on `127.0.0.1:0` with a temp host key, users `me` (control) and `eye` (watch) with generated ed25519 keys, a `Handler` that answers `GET /v1/session` with the principal, and a recording test command `echo` put into `commands` by the test.
  - `TestOnlyAListedKeySignsIn`: an unlisted key fails. `me`'s key signs in. A client offering four unlisted keys and then `me`'s is disconnected before the fourth.
  - `TestNoPasswordAndNoKeyboardInteractive`: a client with only `ssh.Password("x")` and `KeyboardInteractive` fails, and the server's advertised methods (the client's `ssh: unable to authenticate, attempted methods [none password]` error) list `publickey` alone.
  - `TestOnlySessionChannels`: `OpenChannel("direct-tcpip", …)` is rejected with `Prohibited`. `SendRequest("tcpip-forward", true, …)` answers false. A ninth session channel is rejected.
  - `TestARequestIsRefusedByName`: `env LD_PRELOAD=x` → false. `env TERM=xterm` → true. `subsystem sftp` → false. `auth-agent-req@openssh.com` → false. `x11-req` → false.
  - `TestTheCommandIsAFixedList`: `exec "list; rm -rf ~"` (with `list` not registered in this test) → exit 2, `usage` on stderr, and the recording command never ran. `exec "echo a 'b c'"` runs `echo` with `["a", "b c"]` and exit 0. A command line of 5000 bytes → exit 2.
  - `TestTheChannelKnowsItsPerson`: through `echo`, `ch.Handler` answers `GET /v1/session` with `{"name":"me","role":"control","via":"user"}` for `me`'s key and `watch` for `eye`'s. `ch.Login` is the name the client typed.
  - `TestHostKeyIsMadeOnceAndKeptPrivate`: with `NewHostKey` and no file, one is made 0600 and is ed25519. A second `Listen` gives the same `Fingerprint`. A 0644 file is refused. A named missing file without `NewHostKey` is refused.
  - `TestKeysAreCheckedAtStart`: an RSA 1024 key, a certificate and a `from="x" ssh-ed25519 …` line each refuse `Listen`, naming the user and the number.
  - `TestAHandshakeHasADeadline`: with `handshakeWait` at 200 ms, a TCP connection that sends nothing is closed within 1 s. With `maxUnauthed` connections open and silent, one more is closed at once, and `me` still signs in once they time out.
  - `TestTheLogNamesNoKey`: after one sign-in and one refusal, the log has the fingerprints, and no base64 of any key.
  - `TestCloseEndsEveryConnection`: a connection is signed in with a channel open. `Close()` returns within 2 s, and the client sees EOF.
  - `TestWordsParse` (`words_test.go`): `new --label "a b" -- --model 'x y'` → `[new --label "a b" -- --model "x y"]`. `a"b` → error. Tabs split.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.** No file in `sshd` carries a build tag: x/crypto/ssh builds everywhere, and Task 12 refuses on Windows.
- [ ] **Step 4: Verify.**
  - The local CI run passes. This is a **race** task.
  - `sh scripts/module.sh check` prints `module: ok`.
  - `GOWORK=off go -C cmd/rota list -m golang.org/x/crypto` prints v0.58.0.
  - `go list -m all` at the root, with `GOWORK=off`, prints only the root.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota/sshd cmd/rota/go.mod cmd/rota/go.sum README.md docs/reference.md
  git -C <worktree> commit -m "rota has an SSH server of its own on x/crypto/ssh that admits listed public keys alone and opens only session channels running a fixed list of commands"
  ```

### Task 7: The attach loop (`attach.Run`)

**Files:**
- Create: `cmd/rota/attach/run.go` (`Run`, the loop and the end), `cmd/rota/attach/actions.go` (what each chord action does), `cmd/rota/attach/link.go` (`source` and `link`, and their `*client.Client` form), `cmd/rota/attach/settle.go`, `cmd/rota/attach/taught.go` and `cmd/rota/attach/run_test.go`

**Interfaces:**
- Consumes:
  - From Task 1: `client.(*Client).Attach`, `List`, `Term`, `Hello`, `Frame`, `Doc` and `Closed`.
  - From Task 2: `newChord` and its events.
  - From Task 3: `Terminal`, `Options`, `Result`, `Why`, `hintRow`, `helpCard`, `drawRow`, `drawCard` and `OneLine`.
  - From Task 4: `newView` and its methods, and `cropEvery`. Its `note` argument is `func(sc, sr, wc, wr int) string { return cropNote(name, key, sc, sr, wc, wr) }`.
  - `vtscan.ResetAll` and `vtscan.ReplyFilter`.
- Produces:
  - `func Run(ctx context.Context, t Terminal, c *client.Client, id string, o Options) Result`
  - `func TaughtFile(terminals string) func(person string) bool`. It reads and appends `<terminals>/taught` (0600, one name per line, written with `O_APPEND`). It returns true the first time it is asked about a name, and false when the file cannot be written.
  - Unexported: `type source interface { open(id string, watch bool) (link, error); list() ([]client.Session, error) }`, `type link interface { Hello() client.Hello; Next() (client.Frame, error); Type([]byte) error; Fit(cols, rows uint16) error; Claim(force bool) error; Close() error }`, and `func run(ctx context.Context, t Terminal, src source, id string, o Options) Result`.
  - The knobs `var drainFor = 150 * time.Millisecond`, `var noteFor = 3 * time.Second`, `var teachFor = 5 * time.Second`, `var redialFor = 60 * time.Second`, `var redialEvery = 500 * time.Millisecond`, `var forceAfter = 10 * time.Second` and `var settleFor = time.Second`.

The loop, in order:
1. **`Settle`.** With `Settle`, write `ESC[c` and hold what is read.
   - When a DA1 answer (`ESC[?…c`) arrives, it and everything held before it are dropped.
   - When none comes within `settleFor`, what was held passes through `vtscan.ReplyFilter`, and what is left is typed.
2. **Open.** Write `ESC[H ESC[2J`, then `open(id, o.Watch)`. A refusal is `Refused`. Make the view from `Hello` and `t.Size()`. If `o.Teach` is set and `Teach(hello.You.Name)` is true, draw `drawCard(helpCard(key, true))`. It goes at the first key or after `teachFor`, followed by `view.redraw()`, and the key is handled as usual.
3. **Three goroutines and one lock.** One reads `t` into the chord. One reads `link.Next` into the view. One ticks the chord's `due`, note expiry and `cropEvery`. Every write to `t`, and every call on the view, holds one mutex, and no read happens while it is held.
4. **The chord's events:**
   - `actType` goes to `link.Type`, unless watching or switching.
   - `actLeave`: stop reading input, keep writing output for `drainFor`, then end `Detached`.
   - `actNext` / `actPrev`: `list()`, keep live and attachable sessions ordered by `Started`, and move from the current one, wrapping. With no other session, the note is `rota: no other live session`. Otherwise run `chord.reset()`, close the link, drop input until the new `Hello`, and re-open with a fresh view, after `ESC[H ESC[2J`.
   - `actKeyboard`: `Claim(false)`. When it was asked within `forceAfter` and is still not held, the note is `rota: asked <holder> for the keyboard; K k again after 10s takes it`. A second `k` at or after `forceAfter` sends `Claim(true)`.
   - `actFit`: watching, it is the note `rota: watching; this window cannot fit the session`. At the session's size, it is the note `rota: the session is already this size`. Otherwise `Fit(t.Size())`.
   - `actHelp`: draw the card until the next key, then `redraw`.
   - `actHint`: `drawRow(rows, hintRow(…))`, and `redraw` when the chord ends.
   - `actCancel`: the hint row stays for `noteFor`, then `redraw`.
5. **Frames from the daemon:**
   - Output goes to `view.output`, and Docs as follows.
   - `resized` and `repaint` go to `view.resized(cols, rows)`.
   - `keyboard` is remembered (we hold it when `HolderConn == hello.You.Conn`).
   - `error`: the note is `rota: <message>`, at most once per message per `noteFor`.
   - `claim_request`: the note is `rota: <by> asks for the keyboard; they may take it in 10s`.
   - `exit`: end `Ended` with its code.
   - `gap`, `viewers` and `pong` are ignored.

   Notes are drawn only when `view.screenOK()`, so they can be repaired.
6. **The window.** `Resized()` calls `view.window(t.Size())`, and never sends a resize.
7. **A lost link** is a `Next` error other than an `exit`. With `o.Redial` set, the note is `rota: the rota daemon went away; waiting for it (K d leaves)`. Retry `Redial()` and then `open(id)` every `redialEvery` for `redialFor`. Input meanwhile reaches only the chord. A redial whose `open` is refused looks the session up in `list()`. A stopped session ends `Ended`, with its `Exit`, or -1 without one; anything else ends `Refused` with the refusal. A redial that fails for `redialFor` ends `Lost`. With no `Redial`, the loss ends `Lost` at once. A `ctx` cancel ends `Lost` with `ctx.Err()`.
8. **The end, every way:** close the link, `view.close()`, and write `ESC[0m ESC[r` plus `vtscan.ResetAll()` to `t`. The caller prints `Result.Line`.

- [ ] **Step 1: Write the failing tests** with a fake `Terminal` (pipes, a settable size and a `Resized` channel) and a scripted fake `source`, whose links take frames from a channel and record `Type`, `Fit` and `Claim`. Timing knobs are shortened.
  - `TestDetachDrainsResetsAndSaysSo`: hello 80×24 and output `hi`, then input `\x1cd`. The result is `Detached`, the terminal's output contains `hi` and ends with `ESC[0m ESC[r` + `ResetAll()`, and the link typed nothing.
  - `TestEveryEndGivesTheTerminalBack` (Review Focus 1), a table of ends, each with output ending in the reset and the right `Why`:
    - `exit{3}` → `Ended`, 3
    - the link erroring with no `Redial` → `Lost`
    - `ctx` cancelled → `Lost`
    - `Terminal.Write` failing → `Lost`
    - the open refused → `Refused`
  - `TestTypingReachesTheSession`: `abc` gives `Type("abc")`. `\x1c\x1c` gives `Type("\x1c")`.
  - `TestWatchTypesNothing`: with `Watch`, `abc` gives nothing to `Type`, and `\x1cd` still detaches.
  - `TestSwitchingCarriesNoKeys` (Review Focus 5): sessions A and B are live. Input `\x1cn`, then `xyz` before B's hello, then `q` after it. A typed nothing after the chord, and B typed exactly `q`. A link that has been closed is never typed into.
  - `TestNextWrapsAndSaysWhenAlone`: three sessions in `Started` order, with A current. `n n n` visits B, C and A. With one session the note is said.
  - `TestFitSendsThisWindow`: window 90×20 on a 100×30 session, then `\x1cz`, gives `Fit(90,20)`. When watching it gives no `Fit`, and the note.
  - `TestTheKeyboardIsAskedThenForced`: `\x1ck` gives `Claim(false)`. A keyboard doc names bob. `\x1ck` before `forceAfter` is the note with no claim. After it, `Claim(true)`.
  - `TestTheHintRowAndItsRepair`: `\x1c` with no key for `hintAfter` writes the hint text. `d` then detaches. `\x1cx` writes the hint, then a repaint after `noteFor`.
  - `TestTheChordIsTaughtOnce`: `Teach` returns true for `me`, so the card's first line is written. With false, it is not.
  - `TestAWindowChangeNeverResizes`: changing the fake's size and firing `Resized` sends no `Fit` and re-chooses the view (`ESC[?1049h` when it shrinks below the session).
  - `TestADaemonRestartIsAReconnect`: the link errors, `Redial` fails twice, then works, and the new link's hello carries a repaint, which is written. Typing after that reaches the new link. The result after `\x1cd` is `Detached`.
  - `TestNothingTypedWhileAwayIsSent` (Review Focus 5): keys typed between the loss and the new hello reach no link.
  - `TestRedialGivesUp`: with `redialFor` at 100 ms and `Redial` failing, the result is `Lost` and the reset is written.
  - `TestSettleDropsStaleReplies`: input `ESC]11;rgb:0/0/0 BEL ESC[?62;22c a`, with `Settle`, gives output beginning `ESC[c` and `Type("a")` only. With no answer within `settleFor`, `ESC[1;2R b` gives `Type("\x1b[1;2Rb")`, as ReplyFilter keeps Shift+F3.
  - `TestTaughtFileRemembers`: in a temp dir, a name is true once and false after. The file is 0600 and one line per name, and a second `TaughtFile` on the same directory says false for that name too.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.** `run.go` holds `Run`, `run`, the three goroutines and the end. `actions.go` holds step 4's actions and step 5's documents.
- [ ] **Step 4: Verify.** The local CI run passes. This is a **race** task: run the package `-race -count=10`.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota/attach
  git -C <worktree> commit -m "An attach hands a terminal to a held session, reads the chord, repairs what it drew from its own copy of the screen, switches sessions without carrying keys, and gives the terminal back on every end"
  ```

### Task 8: SSH `list`, `new`, and a bare `ssh`

**Files:**
- Create: `cmd/rota/sshd/list.go`, `cmd/rota/sshd/new.go`, `cmd/rota/sshd/tui.go`, `cmd/rota/sshd/commands_test.go` (a rig with a real `api.Server`: localTerm, the tty fake, `Routes{API, WebSocket, Terminal}`), and `cmd/rota/sshd/main_test.go` with `TestMain` calling `fakecli.Maybe()`
- Modify: `cmd/rota/sshd/dispatch.go`, which registers `list` and `new`, and makes the bare command `o.TUI` or the list and then `usage`. Also `cmd/rota/client/client.go`: `Create` gains `Kind string`, and `createDoc` gains `kind,omitempty`. No other group-B task touches that file.

**Interfaces:**
- Consumes `client.InProcess` (Task 1), `client.Create`, `List`, `Match` and `Session`, Task 6's `Channel`, `commands`, `TUISession` and `usage`, and `attach.OneLine` (Task 3).
- Produces:
  - `func clientFor(ch *Channel) (*client.Client, func())`, which is `client.InProcess(ch.Handler)`.
  - The `list` and `new` commands.
  - When `Options.TUI` is set and the channel has a pty, the bare command calls it with `TUISession{In, Out, Env: ["TERM=<term>"], Cols, Rows, Resize, Client, Key}` and returns 0, or 1 with the error on `Err`.

The decisions:
- **`list`** prints one line per session, as in Ruling 12, with two spaces between values and each value through `attach.OneLine`. With none, it prints `no sessions`. `list --json` prints `{"terminals":[…]}`, each the session's own document. Any other argument is exit 2 with `usage`.
- **`new`** parses its flags with a `flag.FlagSet` that writes nothing: `--account N`, `--label NAME`, `--cwd DIR`, `--shell`, `--json`, and the CLI's arguments after `--`.
  - A word before `--` that is not a flag, or `-p` or `--print` among the arguments, is exit 2: `new hands the CLI its terminal, so it takes no prompt or -p: give the CLI's arguments after --`.
  - `--shell` with `--account` or arguments is exit 2: `--shell runs a login shell, which takes no account or arguments`.
  - It calls `Create{Account, Args, Cwd, Label, Cols, Rows}` with the pty's size, or 120×40, and with `--shell`, `Kind: "shell"`.
  - A `*client.Refusal` is its `Message` on `Err`, exit 1.
  - Success, in this task, prints `held as session <name> (<id>)`, or the document with `--json`, and exit 0. Task 11 makes a pty channel attach instead.
- **The bare command with `TUI` nil** prints `list`'s output, a blank line, then `usage`, and exit 0.

- [ ] **Step 1: Write the failing tests.**
  - `TestListPrintsEverySession`: two terminals labelled `api` and `web` give two lines, each with id, `running`, name, `claude/a@x` and the cwd. `list --json` decodes to two documents whose `id`s match. `list --x` is exit 2.
  - `TestNewStartsASession`: `new --label job -- --model m` with no pty gives exit 0 and `held as session job (<id>)`. The session's `args` contain `--model m`, and its size is 120×40. With a pty of 70×20 (no attach yet in this task, so `--json`), the size is 70×20.
  - `TestNewRefusesAPromptAndPrint`: `new fix it`, `new -- -p hi` and `new --shell -- x` are exit 2 with the messages.
  - `TestAWatcherCannotStartOne`: `eye`'s `new` is exit 1 with `this sign-in may only watch; nothing here can be changed from it`.
  - `TestAShellNeedsTheFileToSaySo`: `new --shell` on a server without `Terminal.Shell` is exit 1 with the API's refusal.
  - `TestABareSSHListsAndExplains`: `shell` with a pty prints the list, then `usage`, and exit 0.
  - `TestABareSSHOpensTheTUIWhenThereIsOne`: with `TUI` set to a recorder, a pty of 80×24 and TERM `xterm-256color`, the recorder gets `Cols` 80, `Env` `["TERM=xterm-256color"]` and a `Client` whose `GET /v1/session` names `me`. A `window-change` reaches its `Resize`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes. This is a **race** task.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota/sshd cmd/rota/client/client.go
  git -C <worktree> commit -m "Over SSH, list shows every session, new starts one with the window's size and the daemon's environment, and a bare ssh lists them and says the commands"
  ```

### Task 9: The person's terminal, and the chord key from the daemon

**Files:**
- Create:
  - `cmd/rota/attach/local_unix.go` (`//go:build linux || darwin`)
  - `cmd/rota/attach/local_other.go`
  - `cmd/rota/attach/daemonkey.go`
  - `cmd/rota/attach/local_unix_test.go`
  - `cmd/rota/daemon_key_test.go`
- Modify:
  - `internal/holdwire/daemon.go` (`DetachKey` and `Update`) and `internal/holdwire/daemon_test.go`
  - `cmd/rota/daemon_unix.go` (`runDaemon` records the key)
  - `cmd/rota/go.mod` and `cmd/rota/go.sum`: `golang.org/x/term v0.47.0` becomes direct, through `GOPROXY=off GOSUMDB=off sh scripts/module.sh tidy`

**Interfaces:**
- Consumes `Terminal` and `DefaultKey` (Task 3), `vtscan.CtrlKey` (Task 5), `cfg.Terminal.DetachKey` (Task 5), and `pty.Size`.
- Produces:
  - `func Local(in, out *os.File) (t Terminal, restore func(), err error)`. On unix it puts `in` in raw mode with `term.MakeRaw`, sizes with `pty.Size(in)`, and turns SIGWINCH into `Resized`. `restore` puts the termios back, stops the signal, and is safe to call twice. Elsewhere it returns `holdwire.ErrPlatform`. `in` that is not a terminal is `attach: standard input is not a terminal`.
  - `holdwire.Daemon.DetachKey string`, with JSON `detach_key,omitempty`, and `func holdwire.Update(terminals string, f func(d *Daemon)) error`, with the same claim check `RecordHTTP` makes. `RecordHTTP(t, a)` becomes `Update(t, func(d *Daemon) { d.HTTP = a })`.
  - `func DaemonKey(terminals string) byte` is the running daemon's `DetachKey` through `vtscan.CtrlKey`, or `DefaultKey` when there is none or it does not parse.
  - `runDaemon`, right after `HoldsDaemon`, calls `Update` with `cfg.Terminal.DetachKey`. A failure is one warning in the log.

- [ ] **Step 1: Write the failing tests.**
  - `TestLocalRestoresWhatItChanged` (Review Focus 1): on a pty from `internal/pty`, `Local(slave, slave)` turns `ICANON` and `ECHO` off. `restore()` turns them on again, and a second `restore()` does nothing. `Size()` follows `pty.Resize` on the master, and `Resized` fires.
  - `TestLocalRefusesAPipe`: `Local` on an `os.Pipe` end gives the error.
  - `TestUpdateKeepsTheRest` (holdwire): after `ClaimDaemon`, `Update` with `DetachKey` and then `RecordHTTP` leave both set, and the pid unchanged. `Update` without the claim fails.
  - `TestTheDaemonSaysItsChordKey` (`cmd/rota`, process-level, `newDaemonRig`): `server.toml` with `[terminal] detach_key = "ctrl-]"` and `serve start` give `daemon.pid` a `"detach_key": "ctrl-]"`, and `attach.DaemonKey(terminals)` is 0x1D. With no file it is 0x1C.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes, and `sh scripts/module.sh check` prints `module: ok`. This is a **race** and **docker** task.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota/attach cmd/rota/daemon_unix.go cmd/rota/daemon_key_test.go internal/holdwire cmd/rota/go.mod cmd/rota/go.sum
  git -C <worktree> commit -m "An attach can hold this machine's own terminal raw and give it back, and the daemon tells every attach which key leaves a session"
  ```

### Task 14 (group B): A screen that comes back repaints its viewers

Numbered last because it was added for `bl-screen`'s result. It runs in group B, after `bl-screen` is in `phase-2` (Ruling 29).

**Files:**
- Modify:
  - `internal/holdwire/wire.go`: `StateMsg.Screen`. Also add "or the screen is retired, for now" to `ScreenHead`'s doc, among the cases where `OK` is false.
  - `internal/holdwire/wire_test.go`
  - `cmd/rota/hold/session.go` and `cmd/rota/hold/emulate.go`: the holder's `kept` flag, set where `bl-screen`'s retire and `renew` swap the screen.
  - `api/held.go`: `noteState` and the repaint of viewers.
  - `api/fakeholder_test.go`: the fake holder can send a `State` with `Screen`.
  - `docs/reference.md`, *Falling behind*: `bl-screen`'s sentence that an attached viewer is not repainted when the screen comes back becomes `a viewer attached meanwhile is repainted once it is`.
- Create: `cmd/rota/hold/screen_state_test.go` and `api/held_screen_test.go`

**Interfaces:**
- Consumes `bl-screen`'s `screenNow()`, `renew` and the retire paths in `emulate.go`, and phase 1's `tc.behind` and the writer's repaint in `api/terminal.go`, unchanged.
- Produces:
  - `holdwire.StateMsg.Screen bool` with JSON `screen,omitempty`: the holder keeps a screen now.
  - In `hold`, `Session.kept bool` under `s.mu`. Every change bumps `s.gen`, so the link's state sender sends it at most four times a second as today, and `stateLocked` sets `m.Screen = s.kept`.
  - In `api`, `func (h *heldTerm) noteState(msg *holdwire.StateMsg) (cameBack bool)` and `func (ts *termSession) repaintViewers()`. The second marks every held viewer behind and wakes its writer, so it gets `repaint{offset,cols,rows}` and the screen by the existing path.

The decisions:
- **The first `State` of each link** sets what the daemon knows and repaints nobody.
- **A later `State` whose `Screen` goes from false to true** repaints every viewer once.
- **A holder that never sends the field** (an older one) never repaints anyone.
- **No new viewer frame,** and no change to the attach or the page.

- [ ] **Step 1: Write the failing tests.**
  - `TestTheStateSaysWhetherTheScreenIsKept` (hold, on `bl-screen`'s rig with `renewing` short):
    - after linking, a `State` has `Screen` true;
    - a flood that overtakes the screen brings a `State` with `Screen` false within 1 s;
    - once quiet and renewed, a `State` with `Screen` true.
  - `TestAScreenThatComesBackRepaintsItsViewers` (api, with the fake holder): two viewers attach while the fake's last `State` said `Screen: false`. The fake then sends `State{Screen: true}`, and both viewers receive `{"type":"repaint",…}` and then a binary frame within 1 s. A second `State{Screen: true}` repaints nobody.
  - `TestAFirstStateRepaintsNobody`: a fresh link's first `State{Screen: true}` sends no `repaint`.
  - `TestAnOlderHolderNeverRepaints`: `State` frames without the field, before and after a viewer attaches, send no `repaint`.
  - `TestStateRoundTripsScreen` (holdwire): `Screen` survives encoding, and its absence decodes as false.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes. This is a **race** and **docker** task.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add internal/holdwire cmd/rota/hold api docs/reference.md
  git -C <worktree> commit -m "A holder says whether it keeps a screen, and when a retired one comes back every viewer of the session is repainted from it"
  ```

### Task 10: `rota run --attach`, and `attach.Command`

**Files:**
- Create: `cmd/rota/attach_cli.go`, `cmd/rota/attach_cli_unix_test.go`, `cmd/rota/attach_cli_test.go`, `cmd/rota/attach/command.go`, `cmd/rota/attach/command_test.go`, and `cmd/rota/attach/main_test.go` with `TestMain` calling `fakecli.Maybe()`. No earlier attach test file declares one.
- Modify:
  - `cmd/rota/share.go`: `shareAsk.attach string`, and `takeShareFlags` lifts `--attach <s>` and `--attach=<s>`.
  - `cmd/rota/main.go`: in `run`, `if ask.attach != "" { return c.attachTo(id, rest, ask, remote) }` before the `ask.detach` check. In the run usage, the example line `  rota run --attach api              this terminal on held session api; Ctrl-\ d leaves` after the `--detach` example, and the line `  rota run --attach <session>   look at and type into a held session here` under the existing `rota run [id] --detach` line at `main.go:59`.
  - `cmd/rota/detach.go`: `sendWord` returns `attach.Word(*s)`.
  - `docs/reference.md`: a row for `--attach` in *Every command, and every flag it takes*, beside `--detach`.

**Interfaces:**
- Consumes `attach.Run`, `TaughtFile` (Task 7), `Local` and `DaemonKey` (Task 9), `Word` and `Result` (Task 3), and `client.Dial` and `Resolve`.
- Produces:
  - `func (c *cli) attachTo(id int, rest []string, ask shareAsk, remote remoteAsk) error`
  - `func Command(c *client.Client, id string, o Options) *Cmd`
  - `func (x *Cmd) SetStdin(io.Reader)`, `SetStdout(io.Writer)`, `SetStderr(io.Writer)`, `SetWindow(size func() (cols, rows int), resized <-chan struct{})`, `Run() error` and `Result() Result`. `Run` uses `Local` when stdin and stdout are terminals and no window was set. Otherwise it wraps the three streams and `SetWindow` into a `Terminal`. It prints `Result.Line(o)` on stderr, and returns nil for `Detached` and `Ended`, and `Result.Err` otherwise.

`attachTo`, in order. Each refusal is exit 2 unless it says otherwise:
1. `id != 0` → `--attach takes a session, not an account: rota run --attach <session>`.
2. Words left in `rest` → `--attach takes no prompt or CLI arguments`.
3. `ask.detach`, `ask.on`, `ask.label` or `remote.on` → `--attach joins a session that is running; it takes no --detach, --share, --label or --remote-control`.
4. `!pty.Supported` → `holdwire.ErrPlatform`, exit 1.
5. Stdin or stdout not a terminal → `--attach needs a terminal; rota send <session> "text" types into one from a script`.
6. `daemonStore()`, then `client.Dial`. `ErrNoDaemon` is exit 1: `no rota daemon is running for <store>, so there is no session to attach to; rota run <id> --detach starts one`.
7. `Resolve(ask.attach)`. Its error is exit 1, as `send`'s is. A session whose `State` is not `live` is exit 1: `session <name> has ended; rota list --sessions shows it`.
8. `Local(os.Stdin, os.Stdout)`, with `restore` deferred. SIGTERM and SIGHUP cancel the context given to `Run`.
9. `Run(ctx, t, cl, s.ID, Options{Key: DaemonKey(terminals), Teach: TaughtFile(terminals), Redial: func() (*client.Client, error) { return client.Dial(terminals) }})`.
10. `restore()`, then `Result.Line` on stderr. The exit code is `Result.ExitCode()`.

- [ ] **Step 1: Write the failing tests.**
  - `TestAttachFlagRefusals` (`attach_cli_test.go`, `call`): `run 1 --attach api`, `run --attach api -- x`, `run --attach api --detach`, `run --attach api --share`, `run --attach` (no value) and `run --attach api` with a pipe for stdin each exit 2 with their messages.
  - `TestAttachRunsTheSessionHere` (unix, process-level, `newDaemonRig`): start a daemon and a session labelled `api` at 100×30. Run `rota run --attach api` on a 100×30 pty.
    - The master shows `ready 100x30` within 10 s.
    - Typing `hi\r` shows `got:hi`.
    - Typing `\x1cd` makes it exit 0, and its stderr (a pipe) is `rota: detached from api; rota run --attach api returns to it\n`.
    - The session is still live in `serve status`.
  - `TestAttachGivesTheTerminalBack` (Review Focus 1): while attached, `rota serve stop --all --yes` ends the session. `rota run --attach` exits with the CLI's code. The pty's termios (read on the master with the ioctl `noecho_unix.go` uses) has `ICANON` and `ECHO` on, and the master's last bytes contain `vtscan.ResetAll()` and then `ended`.
  - `TestAttachSurvivesADaemonRestart` (Review Focus 5): while attached, `serve stop`, then `serve start` within 5 s. The master shows a repaint, typing `x\r` shows `got:x` once, and nothing typed during the gap shows up.
  - `TestAttachWithNoDaemon`: exit 1 with the message.
  - `TestAttachLeavesOnTheDaemonsKey`: with `detach_key = "ctrl-]"`, `\x1c` is typed through (`got:` shows `\x1c`), and `\x1d d` detaches.
  - `TestCommandIsAnExecCommand` (attach package): `Command` with `SetStdin` on a pipe, `SetStdout`, `SetStderr` and `SetWindow` at 80×24, over `client.InProcess` on a real `api.Server` with a localTerm and the tty fake, as Task 1's rig is. `Run()` returns nil after `\x1cd`, `Result().Why` is `Detached`, and stderr has the line.
  - `TestASignalGivesTheTerminalBack` (Review Focus 1, unix, process-level): SIGTERM to an attached `rota run --attach api` makes it exit 1. The pty's termios has `ICANON` and `ECHO` on again, its stderr has `rota: lost api:`, and the session is still live.
  - `TestSendWordIsTheAttachWord`: the existing `--detach` tests' expected hint lines are unchanged.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, with the reference row: `` | `--attach <session>` | this terminal on a held session, by its name, label or id: what it shows, and what you type goes to it; `Ctrl-\` then `d` leaves it running (`[terminal] detach_key` changes the key). Linux and macOS | ``.
- [ ] **Step 4: Verify.** The local CI run passes, `TestReferenceDocumentsEveryFlag` and the short-help width tests included. This is a **race** and **docker** task.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota docs/reference.md
  git -C <worktree> commit -m "rota run --attach puts this terminal on a held session until Ctrl-\\ d, through a daemon restart, and gives the terminal back as it found it"
  ```

### Task 11: SSH `attach` and `watch`

**Files:**
- Create: `cmd/rota/sshd/attach.go` and `cmd/rota/sshd/attach_test.go`, on Task 8's rig
- Modify: `cmd/rota/sshd/dispatch.go`, which registers `attach` and `watch`, and `cmd/rota/sshd/new.go`, where a pty channel attaches to the session it started.

**Interfaces:**
- Consumes `attach.Run`, `Options` and `Result` (Tasks 3 and 7), `clientFor` (Task 8), `client.Match` and `List`, and `Channel.Key` and `Channel.Taught` (Task 6).
- Produces:
  - `type chanTerm struct` wraps a `*Channel` as an `attach.Terminal`. `Size` is the latest of `Cols`/`Rows` and `Resize`. `Resized` fires on each `Resize`.
  - `attach <session>` and `watch <session>`.

The decisions:
- No pty is exit 2: `attach needs a terminal: ssh -t <host> attach <session>`, or `watch` for watch.
- The session is resolved with `client.Match` over `List`. Its error is exit 1 on `Err`, and a stopped session is `session <name> has ended`, exit 1.
- `attach.Run(ctx, chanTerm, c, id, Options{Key: ch.Key, Watch: <watch>, Teach: ch.Taught, Again: func(w string) string { return "attach " + w }})`. `ctx` ends with the connection. `Result.Line` goes on `Err`, and the exit is `Result.ExitCode()`.
- `new` with a pty attaches as `attach` does, after the create.

- [ ] **Step 1: Write the failing tests.**
  - `TestAttachOverSSH`: a session `api` at 80×24, and `ssh -t … attach api` with a pty of 80×24. Output shows `ready 80x24`, typing `hi\r` shows `got:hi`, and `\x1cd` ends with exit-status 0 and stderr `rota: detached from api; attach api returns to it`.
  - `TestAttachNeedsATerminal`: no pty → exit 2 and the message.
  - `TestWatchNeverTypes`: `watch api` typing `x\r` gives no `got:x`, and `\x1cd` leaves.
  - `TestAPhoneNeverResizesTheDesk` (Review Focus 3): a session at 100×30, and `attach` from a 40×12 pty, fed 30 lines of 100 characters.
    - `GET /v1/terminals/{id}` still says 100×30.
    - No row the client receives has more than 40 cells.
    - The last row holds `100×30`.
    - `\x1cz` makes the session 40×12, and raw output follows.
    - A `window-change` to 120×40 sends no resize.
  - `TestTheEndOfASessionEndsTheChannel`: the CLI exits 3 while attached, so the exit-status is 3 and stderr has `ended, exit 3`.
  - `TestNewAttachesWhenThereIsATerminal`: `new --label x` with a pty shows `ready`, and `\x1cd` gives `detached from x`.
  - `TestTwoPeopleKeepTheirKeyboards`: `me` attached, then `eye` (watch) attached, and `eye`'s typing never reaches the CLI.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes. This is a **race** task.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota/sshd
  git -C <worktree> commit -m "Over SSH, attach and watch put the connection on a held session as rota run --attach does, cropped and never resized on a smaller window"
  ```

### Task 12: SSH in the daemon and in foreground `rota serve`

**Files:**
- Create: `cmd/rota/sshwire.go`, `cmd/rota/ssh_daemon_unix_test.go` and `cmd/rota/sshwire_other_test.go`
- Modify:
  - `cmd/rota/daemon_unix.go`: `runDaemon` and `daemonStatus`.
  - `cmd/rota/daemon.go`: `statusDoc`, `sayStatus`, and the start line.
  - `cmd/rota/main.go`: `serve`, the foreground path.
  - `internal/holdwire/daemon.go` and its test: the `SSH` and `SSHHostKey` fields.
  - `cmd/rota/daemon_unix_test.go`: `startedLine` allows `; ssh on <addr>`. That is the one existing test expectation this task changes.

**Interfaces:**
- Consumes `sshd.Listen` and `Options` (Task 6, with Tasks 8 and 11's commands), `holdwire.Update` (Task 9), `vtscan.CtrlKey`, `Config.SSH` and `UserEntry.SSHKeys` (Task 5), `attach.TaughtFile` (Task 7), and `srv.LocalHandler`.
- Produces:
  - `func sshOptions(cfg *api.Config, home, terminals string, srv *api.Server, log *slog.Logger) (sshd.Options, error)`. `HostKey` is `cfg.SSH.HostKey`, or `filepath.Join(home, "ssh_host_ed25519_key")` with `NewHostKey` true. `Users` are the `[[users]]` entries with keys, with role from `Role`. `Handler` is `srv.LocalHandler`. `Key` comes from `vtscan.CtrlKey(cfg.Terminal.DetachKey)`, and `Taught` is `TaughtFile(terminals)`. `TUI` is nil; phase 3 sets it here. It returns `the SSH server runs on linux and macOS` when `!pty.Supported`.
  - `holdwire.Daemon.SSH string` and `holdwire.Daemon.SSHHostKey string`, with JSON `ssh,omitempty` and `ssh_host_key,omitempty`.
  - `serve status --json` gains `"ssh": {"addr": "…", "host_key": "SHA256:…"}`, or `"ssh": null` when there is none. Its text gains the line `ssh on <addr>, host key <fingerprint>`.
  - `serve start`'s line gains `; ssh on <addr>` after any `; http on <addr>`.

The decisions:
- **The daemon** listens only when `cfg.SSH.Enabled && !ctlOnly`, right after `daemonPort`. A listen failure is a daemon that did not start. After listening it runs `Update(… SSH, SSHHostKey)`, logs `rota daemon serves ssh` with addr and fingerprint, and serves.
- **The daemon's stop order:** SSH `Close` first (its attaches end Lost, with their line), then the TCP port and `stopCtl` as today, then `srv.Stop()`.
- **Foreground `serve`:** with `cfg.SSH.Enabled`, a server without `Routes.Terminal` is a usage error: `[ssh] enabled needs [routes] terminal in rota serve: SSH reaches the terminals and nothing else; rota serve start serves held sessions without it`. Otherwise it listens after `api.New`, says `rota <version> serving ssh on <addr> (host key <fingerprint>)`, and closes SSH before `srv.Stop()` on shutdown.
- **A file with only `[ssh]`** opens no HTTP port, as `daemonListens` already says.

- [ ] **Step 1: Write the failing tests.** These are process-level, with `newDaemonRig` and a `server.toml` written 0600. That file holds `[ssh] enabled = true`, `listen = "127.0.0.1:<free>"`, and a `[[users]]` entry `me`, role control, with a test key, no password and `local_user` unset.
  - `TestTheDaemonServesSSH`:
    - `serve start` prints `; ssh on 127.0.0.1:<port>`.
    - `<store>/ssh_host_ed25519_key` exists, 0600.
    - `serve status --json` has `ssh.addr` and `ssh.host_key` equal to the key's fingerprint.
    - An SSH client running `list` sees a held session made over `ctl.sock`.
    - `attach` on a pty shows its repaint, and `\x1cd` leaves it live.
  - `TestTheKeyboardFollowsMeFromDeskToPhone`: `rota run --attach api` on a pty (as `local`, which is `me`), and `me` attached over SSH. Typing on SSH moves the keyboard (the desk's attach receives `keyboard` with SSH's conn), and typing at the desk moves it back. Both reach the CLI.
  - `TestCtlOnlyOpensNoSSH`: `serve start --ctl-only` with the same file leaves the port closed, and `daemon.pid` has no `ssh`.
  - `TestOnlySSHOpensNoHTTP`: the file without `[server] listen` gives `status --json` `"http": null` and an `ssh`.
  - `TestStoppingTheDaemonEndsSSHFirst`: an SSH `attach` is open. `serve stop` makes the SSH client see stderr `rota: lost api:` and the channel close, and the holder stay alive and locked.
  - `TestForegroundServeServesSSH`: through the existing `serveUntil` seam, `rota serve --token x` with the file plus `[routes] terminal = true` says the ssh line, and `list` answers over SSH.
  - `TestForegroundSSHNeedsTerminals`: the same without `routes.terminal` exits 2 with the message.
  - `TestSSHOptionsRefuseWindows`, in `cmd/rota/sshwire_other_test.go` (`//go:build !linux && !darwin`): `sshOptions` returns `the SSH server runs on linux and macOS`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes. This is a **race** and **docker** task.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add cmd/rota internal/holdwire
  git -C <worktree> commit -m "The daemon and rota serve run the SSH server when [ssh] enabled says so, never for an auto-started daemon, and serve status names its address and host key"
  ```

### Task 13: The manual and the notes

**Files:**
- Modify: `docs/reference.md` and `docs/releases/1.4.0.md`
- Test: the commands below

**Interfaces:** it consumes this plan's fixed strings. At integration it is checked against Tasks 10 to 12 as merged.

- [ ] **Step 1: A section in the manual, *Attaching from a terminal*,** after *Held sessions and the daemon*. Write it in the manual's voice, from these facts:
  - `rota run --attach <session>`: what it needs (a terminal, a daemon, a live session) and how it finds the session (as `send` does).
  - The chord table from spec section 8, `detach_key`, and the hint row. The card shown once, and where that is remembered.
  - Size: attaching never resizes, `z` fits, and a smaller window is cropped from the bottom with a note.
  - The exit line and exit codes (Ruling 13).
  - A daemon restart is a reconnect of up to 60 s.
  - Switching sessions types nothing typed in between.
  - Watching.
  - The terminal is given back on every end.
  - Delete the bullet *Attaching from a terminal comes in a later beta* at `docs/reference.md:2549`, and say there that `rota run --attach` and SSH attach.
- [ ] **Step 2: A section, *Over SSH*,** after it:
  - The `[ssh]` block and `ssh_keys`, copied from `docs/server.toml`, with the sentence *a control role here can run commands as the daemon's user, as on the web*.
  - Public keys only, the accepted types (Ruling 6) and three tries.
  - Only session channels, and what is refused.
  - The four commands with their flags and the `list` format (Rulings 11 and 12).
  - A bare `ssh` and what phase 3 will put there: *the TUI, in a later beta*.
  - The host key, its fingerprint in `serve status`, and how to compare it on first connect.
  - Who is the same person (Ruling 4), and that the login name is not checked (Ruling 5).
  - The daemon serves SSH when the file says so, except when auto-started. Foreground `serve` serves it with `routes.terminal`. Windows refuses.
  - The limits (Ruling 7).
  - The *For the owner* item 2 limit, as chosen.

  Also add the `[ssh]`, `detach_key` and `ssh_keys` lines to *One file for the server*'s block, rows for `cmd/rota/attach/`, `cmd/rota/sshd/` and `cmd/rota/sshwire.go` in *Layout*, and `serve status`'s `ssh` field where `serve status` is described.
- [ ] **Step 3: The notes.** Under `### New in the next beta` (made above the newest released beta's section if it is missing), one bullet each for:
  - `rota run --attach` and the chord
  - the crop for a smaller window
  - the SSH server and its four commands
  - `ssh_keys` and users with no password
  - `detach_key`
  - `serve status` naming SSH
  - the size of `dist/rota-linux-amd64` beside beta.10's, to one decimal in MB, measured with `GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o "$T/rota" ./cmd/rota` in `T=$(mktemp -d)`, once on the merged phase-2 head and once at `b9fd476`
- [ ] **Step 4: Verify.**
  - The local CI run passes: `TestReferenceDocumentsEveryCommand`, `TestReferenceDocumentsEveryFlag` and `TestReferenceDocumentsEveryRoute`.
  - `git -C <worktree> diff b9fd476 -- docs/releases/` adds lines only under `### New in the next beta`.
  - `docs/reference.md` reads as one text: no section says attach comes later.
- [ ] **Step 5: Commit.**
  ```bash
  git -C <worktree> add docs/reference.md docs/releases/1.4.0.md
  git -C <worktree> commit -m "The manual and the 1.4.0 notes describe attaching from a terminal, the chord, and the SSH server"
  ```

## After the groups

The integrator merges group D into `phase-2`, runs the local CI run and docker once more, and runs the consumer suite (`rota-sdk-test`, `ROTA_BIN` from the merged head). The integrator then asks for the whole-branch review. A live check with a real terminal emulator and a phone's SSH client, using a throwaway `ROTA_HOME` and the fake CLI, is the owner's to approve. Merge, version and tag wait for the owner's word.
