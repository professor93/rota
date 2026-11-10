# Phase 4: Sessions, moving a chat, and Services Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The TUI gets its two remaining tabs. **Sessions** lists every held session, live and stopped, and the conversations that could be resumed. It previews the selected one live and attaches, watches, starts, resumes, moves, stops and forgets. **Services** shows the daemon, its addresses, its viewers, its sinks and its audit tail, and starts, stops, installs, edits, invites and tests from one place. The daemon learns to resume a stopped session under its own id, and to move a chat to another claude account. A live session moves in place and keeps its viewers.

**Architecture:**
- **The server side** is one new route, `PATCH /v1/terminals/{id}` (resume, move, forget), and one new field, `resume` on `POST /v1/terminals`. All the Go is unexported code in `api`, plus a few names in `internal/holdwire`, the contract the daemon and the holder already share.
- **Arguments.** Resume and Move never replay a session's saved arguments. They rebuild them (`holdwire.ResumeArgs`) and relaunch through the one launch path every session takes (`prepareHeld` → `chooseTerminal` → `argsFor`). The Remote Control rule and the disable-Remote-Control settings are therefore decided again for the account the session lands on.
- **A live move swaps in place.** The far end of the session's `termSession` is swapped: the old holder's CLI is hung up, and a new holder takes the same id with its ring going on from the same offset. Viewers stay attached and are repainted.
- **The TUI side** is `cmd/rota/tui` only. Two interfaces, `SessionBackend` and `ServiceBackend`, sit in `Options` beside phase 3's `Backend` (phase 3 R34). The program's `main` fills them from the daemon's client, locally and over SSH alike.

**Tech Stack:** Go 1.27. Neither module gains a requirement. Task 18 (a QR code) is the only exception, and only on the owner's answer to O2. The TUI uses phase 3's Bubble Tea v2, Lip Gloss v2 and Bubbles v2, phase 2's `hold.Screen.Crop` and `attach.Command`, and phase 5's `notify` package.

**Spec:** `docs/superpowers/specs/2026-11-01-tui-daemon-ssh-design.md`. Read sections 2, 4, 9 (*Sessions*, *Services*, the rules), 10, 13 and "Delivery, in order". "The owner's answers" and "Calls made without asking" bind this plan.

**Base:**
- This plan: `phase-4-plan`, cut from main `b9fd476`.
- Implementation: `phase-4`, cut from `phase-4-plan`. The integrator merges into it, as each is reviewed: lane `sec-rc`, then `phase-2`, `phase-3` and `phase-5`.
- Each task's row in *Parallel groups* says what must be in `phase-4` before the task is cut.

## Global Constraints

- **The SDK is touched as little as possible.** The root module gains no `require`, no `replace` and no `go.sum`. It gains **no exported name in a public package**: `lib`, `store`, `wire`, `rotation`, `message`, `sessions` and `api` get only unexported code.
  - The root gains five exported names, all in `internal/holdwire`, which nothing outside this module tree can import:
    - `ResumeArgs` and `MoveThinking` (Task 1)
    - `Request.Conversation`, `Request.ConversationCwd` and `Request.Offset` (Task 2)
  - `cmd/rota` cannot hold them: `api`, in the root, writes the request and computes the arguments.
  - `sh scripts/module.sh check` prints `module: ok` after every task.
- **New public HTTP surface**, and nothing more (spec: "`PATCH /v1/terminals/{id}` takes what it can before a new path is added"):
  - `PATCH /v1/terminals/{id}` with the bodies in *The HTTP contract*
  - `resume` on `POST /v1/terminals`
  - the socket frame `moved`
  - the refusal reasons listed below
  - `resumable` now also true for an ended session (Ruling R5)
- **Moving** (spec section 10, verbatim):
  - "within one provider flavour, and `sessions.CopyForResume` brings the transcript to the target when its home does not hold it, whatever the `--sessions` mode"
  - "in this version only claude sessions can move. Across providers it is refused by name"
  - "The session keeps its id, name, stored arguments and viewers. Mid-turn work is lost to that turn, so a running session warns first"
  - "from the folder the registry reported, so a worktree session resumes where its conversation lives"
  - "With no known conversation, Resume and Move are disabled with the reason" (section 2)
  - "A chat moves only when the owner confirms it" (section 11)
- **Never replay saved arguments raw** (the coordinator's ruling). Resume and Move rebuild them with `ResumeArgs` and relaunch through `argsFor`. Over TCP, arguments that ask for Remote Control are refused (`403`). Over the control socket they go through `readyRemoteControl`. An account whose Remote Control setting is off gets sec-rc's `--settings {"disableRemoteControl":true}` from that same path, decided for the account the session lands on, never carried from the old launch.
- **Sizes** (section 13): "A held session never starts at 0×0 … Resume starts at the requester's size". With no size given, it is 120×40. A live session that moves keeps its size, and the preview never resizes ("renders the session at its own size and crops; it never resizes it").
- **Files and reach** (section 13): "Stop and `x` never touch a worktree; Sessions shows a worktree's path and copies it." "Each session shows its account and credential kind: login, long token or API key."
- **The detach key** is a fixed `Ctrl-\` in 1.4.0 (`attach.DefaultKey`), with no config.
- **Natural by default.** Nothing here changes an existing command's output, exit code or JSON, except `resumable` (R5). Nothing starts a daemon except `n` and `r` in the TUI, which say so (section 6). Nothing moves a chat without a confirm.
- **TUI rules** (spec section 9, and phase 3 Task 6's `rules_test.go`):
  - Elm architecture, and no I/O in `Update`.
  - One file per tab, and no non-test file past 600 lines.
  - `key.Binding` is the only source of keys, and every `key.NewBinding(` sits in a function whose name ends in `Keys`.
  - Fields come only from `newInput(th)` and `newArea(th)` (phase 3 `theme.go`). No `tea.Sequence`, and no `bubbles/list`.
  - Width ladders; the 60×14 floor; goldens at 60×20, 80×24 and 120×40.
  - Tests in package `tui` drive models with `run(t, m, msgs...)` and `await(t, m, msgs...)`. Each test has its own command registry, so none uses `t.Parallel`.
- **The watch role never types**, and it sees no start, resume, move, stop, forget, service, invite or test action. `roleFor` refuses them anyway.
- **Isolation.** Every new test package that starts a CLI, a terminal or a store has the `TestMain` of `cmd/rota/main_test.go:30-83`. That means `fakecli.Maybe()`, `claudecode.StandIn()`, every provider endpoint offline, a temp `HOME`, `USERPROFILE` and `CLAUDE_CONFIG_DIR`, and `ROTA_CLAUDE_HOME`, `ROTA_ACCOUNT_ID`, `ANTHROPIC_CONFIG_DIR` and `XDG_CONFIG_HOME` unset. `api`, `cmd/rota`, `cmd/rota/client`, `cmd/rota/hold` and `cmd/rota/tui` already have it.
- **What tests never do:**
  - run a real claude, codex or grok
  - touch the keychain, `~/.claude`, `~/.codex` or `~/.rota`
  - run the installed `rota`
  - use sh scripts as fakes; fakes come only from `internal/fakecli` and `api`'s fake holder (`ROTA_FAKE_HOLDER=1`)
- `<testenv>` is `env -u ROTA_CLAUDE_HOME -u CLAUDE_CONFIG_DIR -u ROTA_ACCOUNT_ID -u ROTA_ACCOUNT -u ROTA_PROVIDER -u ROTA_TOKEN ROTA_HOME="$(mktemp -d)" GOPRIVATE='github.com/professor93/*'`.
- **The local CI run**, from the repository root:
  1. `test -z "$(gofmt -l <changed .go files>)"`
  2. `for os in linux darwin windows; do GOOS=$os go vet work || echo "vet $os FAILED"; done`
  3. `<testenv> go test -count=1 work`, then `<testenv> go test -count=1 -race -timeout 30m work`
  4. `sh scripts/module.sh check`

  Never `go test ./...` at the root. A timing test that fails is rerun alone with `-count=3` before it counts, and named in the report. A task marked **docker** is run under Linux in Docker by the integrator, serially; implementers never run docker.
- **Platforms.** Linux first, then macOS. Windows builds and vets. Held sessions refuse there by name, as today, and the two tabs say `held sessions and the rota daemon run on linux and macOS`. Test files that need unix sockets carry `//go:build linux || darwin`.
- **Branches and commits.**
  - Each task is on `p4-t<N>`, cut from `phase-4` when its group starts. Commands use `git -C <absolute path>`.
  - No force-push, no tag, no merge to main, no version bump, no PR.
  - A commit's subject is one plain sentence of what is now true, with no body, no trailer and nothing about AI.
- **Docs.** `docs/reference.md` must read as one text after each task.
  - Task 4 adds the `PATCH /v1/terminals/{id}` row, because `TestReferenceDocumentsEveryRoute` requires it.
  - Every other line in the manual, and every bullet under `### New in the next beta` in `docs/releases/1.4.0.md`, is Task 19's, so parallel tasks never share either file.
  - Released sections stay byte-identical: `git diff b9fd476 -- docs/releases/`.
- **Names inside one package.** Tasks in one group may share a package, never a file. A task declares no top-level name beyond its Interfaces block, except names that begin with its file's stem.

## Review Focus

1. **A Resume or Move that fails half-way.** The old CLI was stopped, but the new one cannot start: the target died after the check, the holder failed, the daemon began to stop, or a Stop came. A person expects nothing lost: the session ends as stopped, its conversation is still known (`resumable`), the answer says what happened and that `r` resumes it where it was, and the viewers are given the exit. Tests: Task 7 `TestAMoveThatCannotStartLeavesItResumable`, `TestAStopDuringAMoveEndsIt` and `TestTheDaemonStoppingMidMoveLeavesItResumable`; Task 4 `TestAResumeThatCannotStartLeavesTheRecord`.
2. **Saved arguments that carry something they must not replay.** The cases are:
   - a prompt, or `--` with a prompt
   - `--worktree` or `-w`
   - leftover `--resume`, `-c` or `--session-id`
   - an unknown flag with a word after it
   - `--remote-control`
   - sec-rc's injected `--settings {"disableRemoteControl":true}`
   - a person's own `--settings` that turns Remote Control on

   The expected behaviour: a prompt is never sent again, Remote Control never starts unreadied, it is refused over TCP, and disabling is decided again for the target account. Tests: Task 1 `TestResumeArgsNeverReplayAPrompt`; Task 4 `TestResumeHoldsRemoteControlToTheRule` and `TestResumeDecidesDisablingAfresh`.
3. **Two things at once.** Two Resumes or two Moves of one session, a Stop during a Move, typing while a session is being swapped, the old holder's kill timer firing after the swap. Exactly one new holder may start, the Stop wins, no keystroke reaches the wrong CLI, and the new holder is never signalled by the old one's timer. Tests: Task 4 `TestTwoResumesStartOneHolder`; Task 7 `TestAStopDuringAMoveEndsIt`, `TestTypingDuringAMoveReachesNoCLI` and `TestTheOldKillNeverReachesTheNewHolder`.
4. **The conversation cannot be found where it was.** Its worktree folder was deleted, the account it ran on was removed, or no home holds its transcript. Expected: a refusal naming the folder; a move from a removed account still starts on the target; a CLI that cannot find the conversation ends at once with its exit, said in a toast. Tests: Task 4 `TestAResumeIntoAFolderThatIsGoneIsRefused`; Task 6 `TestAMoveFromARemovedAccountStillStarts`; Task 12 `TestASessionThatEndsAtOnceIsSaid`.
5. **The TUI's list changing underneath.** A session ends, is forgotten elsewhere, moves to another account, or the daemon stops, while it is selected, previewed, or open in the move picker or the confirm. Expected: the selection follows the id, the preview says the session ended or reconnects, an overlay says the session is gone, and nothing panics. Tests: Task 10 `TestSelectionFollowsTheSession` and `TestTheDaemonStoppingSaysSo`; Task 11 `TestThePreviewSaysWhenItsSessionEnds`; Task 14 `TestThePickerSaysWhenItsSessionIsGone`.

## Rulings

Each is this plan's call where the spec is silent: what was chosen, why, and what it costs if wrong.

| # | Ruling | Why | Cost if wrong |
|---|---|---|---|
| R1 | **A live Move swaps in place.** `termSession.run` loops: after the old holder's CLI ends, a pending handover starts a new holder under the same id, its ring starting at `ts.end()`. Then `ts.back` is replaced, the account fields are updated, every viewer is sent `moved` and is repainted (phase 2's `repaintViewers`), and the loop goes on. No `exit` frame is sent. | The spec says the session keeps its "viewers". Swapping in place needs no change to the attach (phase 2) or the page (phase 6), and offsets never go backwards, so recordings and `since` stay right. | The run loop is more complex. If it proves wrong, the fallback is to end the session and have viewers reattach to the same id, which phase 2's redial and phase 6's re-select already do. |
| R2 | **A stopped session resumes under its own id and directory.** A new holder takes `terminals/<id>/` once its lock is free, rewrites `meta.json` (seeded with the conversation), and goes on from the old output end. That end is the recording's when recording is on, `ts.end()` when the ended session is still in memory, and otherwise 0. The recording continues. | Same id, same name, same `/terminal#<id>` link. A recording never overlaps itself. | `holder.log` holds both lives of the session. |
| R3 | **`ResumeArgs` rebuilds the arguments.** `--resume <conversation>` comes first. Then the saved arguments, less these: <br>• positionals and everything after `--` <br>• `-p` and `--print` <br>• `--worktree` and `-w` with their optional value <br>• `--resume`, `-r`, `--continue`, `-c`, `--session-id`, `--fork-session` and `--from-pr` <br>• sec-rc's injected `--settings` value <br>• the word after a flag it does not know <br>Value flags keep their value, using sec-rc's `valueFlags` table. A short list of variadic flags keeps every following word. | A stored prompt would run its task again (the `StoredArgs` doc comment). Dropping what is unknown errs on the safe side. | A variadic flag missing from the list loses its second and later values. An unknown value flag loses its value. Claude Code then says so. |
| R4 | **The folder is `conversation_cwd`, else `cwd`.** A folder that is gone is refused, `400`: *the folder session <name>'s conversation was in, <dir>, is gone; rota run <account> --resume <conv> resumes it from another*. | Claude Code finds a conversation by the project folder it ran in. | A deleted worktree's conversation needs `rota run`. |
| R5 | **`resumable` means stopped (ended or interrupted), claude, on a known conversation.** Only an interrupted session leads with Resume: in the TUI `⏎` resumes it, and on the page the button is primary. An ended one offers `r`. | The spec says "only interrupted sessions lead with Resume", so ended ones can resume too. Phase 6 Task 11 already reads it that way. | A reader that took `resumable` to mean "interrupted" is now told true for ended sessions as well. The reference says so. |
| R6 | **A Move asks first, and the server says what it costs.** When the session is live, or the target account's organization (`rota.Account.Org`) differs from the session's account's or either is unknown, the answer is `409 {"reason":"confirm","error":<sentence>,"warnings":[…]}`. The move happens only when the body also says `"confirm":true`. A stopped session moving within one organization needs no confirm. | "A chat moves only when the owner confirms it." It tells every surface, scripts included, in one set of words, and it is the one place O1's policy lives. | Two round trips per move. |
| R7 | **Size.** A live move keeps the session's size. A stopped resume or move takes `cols`/`rows` from the body. Neither or zero gives 120×40, bounded by `boundSize`. | "Attaching never resizes"; "Resume starts at the requester's size". | A phone that sends no size gets 120×40. |
| R8 | **A Stop during a Move wins**: the new holder is not started, and the session ends as stopped by that person. **The daemon stopping mid-move** leaves the session stopped and resumable. If the new holder has already started, it is let go running, for the next daemon. | Nobody's stop is ignored. | None. |
| R9 | **Forget** is `PATCH {"forget":true}`, for stopped sessions only. It removes `terminals/<id>/` under that directory's lock, exactly as `keep_stopped`'s expiry does. Recordings and transcripts stay, and nothing else is touched. | One verb for one act. A `DELETE` that both stops and forgets would turn a double click into data loss. | A recording outlives its record, as it does today at expiry. |
| R10 | **`POST /v1/terminals {"resume":"<conversation>"}`** is for a claude account only. The transcript is copied in first (`CopyForResume`), and `--resume <id>` goes first among the arguments. Arguments that already pick a conversation are refused. The field mirrors `POST /v1/run`'s `resume`. | A conversation from the list can move even when its own account cannot start (a login past its 30-day end). | One field of public surface (O3). |
| R11 | **`env` on `PATCH` follows `POST`.** It is taken only over the control socket, and an empty one is refused. The local TUI sends its own environment, filtered as `run --detach` filters it. SSH and the web get the daemon's. | One rule. | None. |
| R12 | **Conversation rows.** `r` and `⏎` resume a conversation on its own account; a shared conversation goes to the first claude account, in rotation order, whose `sessions` is shared. `m` picks another account through `POST … resume`. The TUI shows `MoveThinking` itself first, since it cannot tell organizations apart (`wire.Account` has no org, and gains none). | The TUI must say the thinking cost before a move, and only the server knows the orgs. | An unneeded note between two accounts of one organization. |
| R13 | **The preview is a watch attach.** It uses `client.Attach(id, true)` (phase 2), feeds a local `hold.Screen` at the session's size, and is cropped bottom-anchored to the pane. It opens 250 ms after the selection rests, only while Sessions is shown, one at a time. | It needs no new route (the owner's SDK rule) and is live. | The preview counts as a watching viewer: the page reads `1 watching` while a TUI previews. Over SSH, phase 5 announces it as `viewer.joined` after its 10 s settle. |
| R14 | **Polling.** `GET /v1/terminals` every 2 s while Sessions is shown, otherwise every 15 s. Conversations (`GET /v1/accounts?sessions=1&recent=10`) on the first show, every 60 s, and after each start, resume, move, stop or forget. No event stream in this phase. | The listing is cheap, and the sessions scan reads other programs' files. | Up to 2 s of lag. |
| R15 | **`n`, `r` and a conversation's `⏎` start and then attach at once**. `Ctrl-\ d` comes back to the list. | That is what a person pressing them wants next. | One chord for someone who wanted it in the background. |
| R16 | **New's fields**: account (the rotation's choice or an id), folder, name (`label`), worktree (a claude account chosen by id; appends `--worktree`), and Remote Control (claude, and offered only when `Defaults().RemoteControl`, which is over the control socket). There is no arguments field. The folder defaults to the account's own `cwd`, else `Defaults().Cwd`. | The spec's five fields. | Arguments need `rota run --detach`. |
| R17 | **In New**, a codex, grok or kimi account that a live held session runs on is greyed, with `busy: held by session <name>`. A dead account is greyed with its state. Neither can be chosen. | Spec section 5. | A run outside the daemon still answers `409` on start. |
| R18 | **The list's order is fixed.** Live sessions first, newest started first. Then stopped ones, newest stopped first. Then conversations, newest first, skipping any whose id is a listed session's `conversation`. A status change never moves a row. | Phase 6 Ruling 6: a row moving under a finger is a mis-tap. | A session that needs you may sit lower; the list header counts it. |
| R19 | **Services over SSH** shows the daemon, its addresses, its viewers and its sinks. Instead of start, stop, install, uninstall, edit and the audit tail, it says `on <host>: rota serve stop` (or the command for each). Invite and sink test go through `roleFor`. | Stopping the daemon from inside it would cut the connection asking. Filesystem permissions authorize the audit log locally, and nothing does over SSH (O4). | An SSH person runs those commands on the host. |
| R20 | **The audit tail** is local only. It reads the last 64 KiB of the daemon's log. It shows the 20 newest lines whose `msg` starts with `terminal `, `keyboard `, `invite `, `signed ` or `remote control`, each as `HH:MM msg key=value…`, cleaned. | It reads a 20 MiB log without reading 20 MiB. | Older lines are not shown. |
| R21 | **Service state comes from the store's files**, locally and over SSH alike (the TUI over SSH runs inside the daemon): `daemon.pid` (phase 2 adds `ssh` and `ssh_host_key`), the records, `sinks.json` (phase 5) and the unit file. | No new route. | A watch SSH user sees the pid, version, addresses and sink names. |
| R22 | **An invite's link** is built from `[events] link` when set. Otherwise it is the daemon's HTTP address, with this machine's host name in place of an unspecified address, and `https` when `[tls]` is set. Only the code is taken from the API's answer. It lasts 10 minutes. `v` is hidden when the daemon serves no HTTP. | The API builds the link from the `Host` header, which on a socket is made up (reference: *The control socket*). | Behind a proxy, `[events] link` must be set. |
| R23 | **The service actions** run `rota serve start`, `stop`, `install` and `uninstall` as a child, with no standard input and their output captured. The last line becomes a toast. The TUI confirms `stop` and `uninstall` first. `stop --all` is never offered. | One implementation, the command's. | None. |
| R24 | **`e` hands the terminal** to `$VISUAL`, else `$EDITOR`, else `vi`, split on spaces, with `<store>/server.toml` as one more argument. Afterwards it says *server.toml is read when the daemon starts: s stops it, and s again starts it*. rota never writes the file. | Spec section 9. | A daemon started with `--config` elsewhere is not the file edited. The line names the path. |
| R25 | **The tabs are registered** (`register(20, newSessions)`, `register(40, newServices)`) in one file made by Task 19, which also writes their keys into the manual. | Phase 3's `TestReferenceDocumentsEveryKey` checks every registered tab, and only one task may edit the manual at once. | Inside `phase-4`, the tabs appear only from Task 19 on. |
| R26 | **The shell sends `shownMsg{Place}`** to every tab when the active tab changes, and once after `Init`. This is a phase 3 `model.go` edit. | The preview and polling must know whether they are seen. | None. |
| R27 | **Call deadlines (`e.call`).** The list 10 s, conversations 30 s, create and resume 60 s, move 90 s, stop and forget 10 s, invite 10 s, sink test 15 s, a service action 70 s (`serve stop` waits up to a minute). | Phase 3 R29's rule, sized per call. A live move is a hang-up, the holder's settle and a new holder. | A move cut at 90 s: the server finishes it anyway, and the next poll shows it. |
| R28 | **The `moved` frame** `{"type":"moved","account":{"id","label","provider"}}` goes to every viewer after a live move. Clients that do not know it ignore it, as both clients already do with unknown frames. | Viewers can update their account line at once. | One more frame type. |
| R29 | **The credential kind** is derived from the listing, as phase 6 Ruling 10 does: `credential` `apikey` is API key, a future `long_until` is long token, anything else is login. Phase 6's optional P4-3 is not served. | No API change. | Wrong only inside a long token's last day. |
| R30 | **`PATCH` refusal reasons**, beside the existing statuses: `running`, `moving`, `conversation`, `provider`, `confirm`, `not_held` and `foreign`. | A program can act on them. | None. |
| R31 | **An account crossing its threshold** while a live held session runs on it gets one toast in Sessions, *#12 work@… is over its 80%: m moves <name> to another account*, and an `over` mark on that row. It re-arms when the account falls back under. This is computed from phase 3's listings (`level`), not from events. | Spec section 11: "`account.threshold` on a live session offers `m` in the TUI". | None. |
| R32 | **The phase 5 hooks** are called where phase 5 Task 7 named them. `req.why = whyResumed` or `whyMoved` goes on resume and stopped-move starts. For a live move: `ts.endingAs(whyMoved, …)` before the hang-up, `ts.sessionEnded(code)` and `s.sessionStarted(ts, by, whyMoved, "")` at the swap, and `ts.endingAs("", "")` when it fails. A task whose base lacks `api/events_sessions.go` leaves exactly those calls out, says so in its report, and the integrator adds them when phase 5 lands ("whichever phase lands second adds the call"). | The names are fixed (coordinator, 2026-11-10). | None. |

## For the owner

The plan proceeds with each recommendation unless the owner picks another. The task named changes before it runs.

**O1. Moving a claude chat to an account of another organization loses its earlier thinking with Sonnet 5.5, Haiku 5.5 and Fable 5.2, not with Opus 5.5** (Claude Code 2.1.322). Claude Code then says the next responses are "slower and use more tokens". This is irreversible for that conversation's thinking while it stays on the other organization. Task 6's `moveCosts` holds the policy.
1. **Recommended.** Warn, and move on confirm (R6). The sentence names the models, and it is skipped when both accounts are known to be in one organization.
2. Refuse a cross-organization move unless the saved arguments name an Opus model (`--model opus` or `claude-opus-*`).
3. Refuse cross-organization moves entirely. Moving stays within one organization.
4. Move without the thinking note. Only the mid-turn warning stays.
5. Warn as in 1, and add a second, typed confirm (`move` typed out) when the session is live.
6. Warn as in 1, and have the picker list same-organization accounts first. The server would sort them, since `wire.Account` has no org.

**O2. The QR code for an invite** (spec section 13: "The Services tab can show an invite as a QR code"). A dependency is a `require` in every later `cmd/rota/vX` for good. Task 18 waits on this answer.
1. **Recommended.** `rsc.io/qr` v0.2.0 (BSD, no dependencies), downloaded under phase 3 O1's rule (`GOPRIVATE=github.com/professor93/*`, the default proxy and checksum database).
2. `github.com/skip2/go-qrcode` (MIT, larger).
3. A hand-written encoder in `cmd/rota/tui` (byte mode, error correction M, versions 1–10), checked against vectors from the owner's machine.
4. No QR code in 1.4.0. The link only, with `c` to copy it.
5. `qrencode -t UTF8` when it is on `PATH`, otherwise the link only.

**O3. New public HTTP surface**, permanent once published:
- the `PATCH` bodies and reasons
- `POST … resume`
- the `moved` frame
- `resumable` widened to ended sessions

The options:
1. **Recommended.** As planned.
2. No `POST … resume`. A conversation resumes on its own account only, and moves by `r` and then `m`. A conversation whose own account cannot start cannot then move from the TUI.
3. Forget as `DELETE` on a stopped record, instead of `PATCH {"forget":true}`.
4. Move's costs as a preview call, `GET /v1/terminals/{id}?move_to=N`, instead of the `409 confirm` round trip.
5. Keep `resumable` interrupted-only, and add `resume_ok` for ended sessions.

**O4. The audit tail and service state over SSH.** This departs from the spec's "audit tail and service state need routes", under the owner's SDK rule.
1. **Recommended.** Local only for the audit tail, and state read from the store's files (R19–R21). No route.
2. `GET /v1/audit` for the control role, served by `api` from the daemon's log.
3. `GET /v1/daemon` (watch) for the state, with `?audit=N` for control.
4. Audit lines as a new event type in phase 5's ring, read by `GET /v1/events`.
5. No audit tail in the TUI. The Services tab names the log's path.

## What phase 4 takes from other phases, and what it gives

A task that finds one of these names wrong stops and asks the integrator. It never renames one.

| From | Exactly what | Tasks |
|---|---|---|
| lane `sec-rc` (0cd9e6f or its approved successor) | `holdwire.AsksRemoteControl(args []string, cwd string) (bool, error)` and its unexported `valueFlags`, in `internal/holdwire/remote.go`. In `api`: `localPrincipal(r)`, `remoteArgs(r, a, args, cwd)`, `argsFor(r, st, a, req, cwd)`, `readyRemoteControl(r, st, a)`, `remoteArgsLocal` and `settingsLocal`. The disable-Remote-Control launch argument, `--settings {"disableRemoteControl":true}`, added on that path for a claude account whose setting is off, through the one constant sec-rc names for it. | 1, 4, 5, 6 |
| phase 2 Task 1 | `func (c *client.Client) Attach(id string, watch bool) (*client.Term, error)`, `(*Term).Hello() client.Hello`, `(*Term).Next() (client.Frame, error)`, `(*Term).Close() error`, `client.Hello{ID, Kind; Cols, Rows uint16; …}`, `client.Frame{Output []byte; Doc *client.Doc}`, `client.Doc{Type; Cols, Rows uint16; Code int; …}`, `client.InProcess(h http.Handler) (*Client, func())`, `(*Client).HTTP() *http.Client` | 8, 9, 11 |
| phase 2 Task 4 | `func (s *hold.Screen) Crop(x, y, cols, rows int) (hold.Window, bool)`, `(*hold.Screen).Size() (cols, rows int)`, `hold.Window{Rows [][]byte; Row, Col int; CursorIn bool}`, and the existing `hold.NewScreen`, `Write`, `Refill`, `Resize`, `Cursor`, `OK` and `Close`, with `vtscan.NewScanner` | 11 |
| phase 2 Task 8 | `client.Create.Kind` in `cmd/rota/client/client.go`. A shared file only: Task 3 is cut after it. | 3 |
| phase 2 Task 10 | `attach.Command(c *client.Client, id string, o attach.Options) *attach.Cmd`, with `(*Cmd).Result() attach.Result`, `SetWindow(size func() (int, int), resized <-chan struct{})`, `attach.Options{Key byte; Watch, Settle bool; Teach func(string) bool; Again func(string) string; Redial func() (*client.Client, error)}`, `attach.DefaultKey`, `attach.TaughtFile(terminals)` and `(attach.Result).Line(o attach.Options) string` | 17 |
| phase 2 Task 12 | `holdwire.Daemon.SSH` and `holdwire.Daemon.SSHHostKey` (in `daemon.pid`) | 16 |
| phase 2 Task 14 | `func (ts *termSession) repaintViewers()` in `api` | 7 |
| phase 3 Task 1 | `Theme`, `NewTheme`, `newInput(th Theme) textinput.Model`, `newArea`, and the test helpers `run(t, m, msgs...)`, `await`, `plain`, `golden`, `press`, `size` and `fakeTicks` | 8, 10–15, 19 |
| phase 3 Task 6 | `Options`, `env` (`o`, `th`, `who`, `list`, `width`, `height`; `now`, `tick`, `control`, `gate`, `call`), `tab`, `overlay`, `register`, `handOff`, `copyText`, `toast`, `toastMsg`, `listingMsg`, `openMsg`, `execTail` and `tailed.Last`; and `model.go`, where Task 8 adds `shownMsg` | 8, 10–15 |
| phase 3 Task 7 | `clean`, `fit`, `wrap`, `col`, `bar`, `level`, `deciding` and `resetText` | 10, 13, 14, 15 |
| phase 3 Task 14 | `newConfirm(e *env, title, body string, yes func() tea.Cmd) overlay` | 12, 14, 15 |
| phase 3 Task 12 and Task 20 | `cmd/rota/tui_entry.go` (`openTUI`, and `selfCmd`, which Task 16 runs `rota serve` with) and `cmd/rota/tui_ssh.go` (`sshTUI`), where Task 17 sets the two new `Options` fields | 16, 17 |
| phase 3 Task 21 | `TestReferenceDocumentsEveryKey` and the manual's *The TUI* section | 19 |
| phase 5 Task 3 | `api.Config.Events` (`api.EventsSection`, with `Link`) | 16 |
| phase 5 Task 7 | `func (s *Server) sessionStarted(ts *termSession, by, why, text string)`, `func (ts *termSession) endingAs(why, text string)`, `func (ts *termSession) sessionEnded(code int)`, `whyResumed`, `whyMoved`, and the unexported `termRequest.why` (R32) | 4, 6, 7 |
| phase 5 Task 9 | `notify.ReadStatus(terminals string) (*notify.StatusFile, error)`, `notify.Describe(e api.EventsSection) []notify.Status`, `notify.Test(c *client.Client, terminals, sink string, within time.Duration) ([]notify.Outcome, error)`, `notify.Status`, `notify.StatusFile{PID; Updated; Sinks}` and `notify.Outcome{Sink; Answered, OK bool; Error string}` | 16 |

**What this gives phase 6.** Task 11's contract is kept as proposed: `PATCH /v1/terminals/{id}` with `{"resume":true,"cols":C,"rows":R}`, and with `{"account":N}`, each answering 200 with the description. This plan adds two things:
- A move that costs something answers `409 {"reason":"confirm","error":<sentence>,"warnings":[…]}`, and is made by sending the same body again with `"confirm":true`.
- `resumable` is true for ended sessions too.

The one phase-6 file that changes is `api/assets/term/actions.js`, Task 11's own. On a `409` with `reason` `confirm`, `termMove` asks `confirm(r.doc.error)`, and on yes sends the body again with `confirm: true`. Its own live-session confirm text goes, because the server's sentence says it. Task 11's tests gain `a move that costs something asks with the server's words`. Nothing else in phase 6 changes.

**What this gives phase 7.** The manual's sections on Sessions, Services, Resume and Move are written by Task 19.

## The HTTP contract (Tasks 4–7, and phase 6 Task 11)

`PATCH /v1/terminals/{id}`, role control, in the terminal group. The body, read strictly:

```json
{"resume": true, "cols": 120, "rows": 40, "env": ["PATH=…"]}
{"account": 13, "confirm": true, "cols": 120, "rows": 40, "env": ["PATH=…"]}
{"forget": true}
```

- **Exactly one** of `resume`, `account` and `forget`. Anything else is `400` *a change to a terminal is one of resume, account or forget*.
- **`env`** is taken only over the control socket (`403 envLocal`). An empty one is `400 envEmpty`.
- **`cols` and `rows`** apply only to a stopped session (R7).

| Answer | When |
|---|---|
| `200` the description (same `id`, `state` `live`) | resume, move |
| `200 {"ok":true}` | forget |
| `404 {"error":"no terminal <id>"}` | unknown id, or for a move an unknown account (`no account <N>`) |
| `400` | two of the three; `account` equal to the session's own (*session <name> is on account N already*); a folder that is gone (R4) |
| `403` | `env`, or saved arguments asking for Remote Control, over TCP (sec-rc's sentences) |
| `409 reason running` | resume or forget of a session that is running |
| `409 reason moving` | a resume, move or stop arrives while one is under way |
| `409 reason not_held` | a terminal this server runs itself (foreground `rota serve`) |
| `409 reason foreign` | a holder made by a rota this one cannot attach to |
| `409 reason conversation` | a shell, a provider other than claude, or no known conversation, each with its own sentence |
| `409 reason provider` | a move to an account of another flavour: *session <name> is a claude session and account N is a codex account; no CLI can read another's conversation* |
| `409 reason confirm` | a move that costs something and has no `"confirm":true` |
| `409`, `502` | as `POST /v1/terminals` answers: a busy or dead account, `max_sessions`, a holder that failed |

**The confirm sentence.** These pieces are pinned by tests and joined with single spaces. `<name>` is the session's name, and `<who>` is `#N <label>`.
- `Move <name> to <who>?`
- When live: `Its CLI is stopped and starts again there on the same conversation; whatever it is doing in this turn is lost.`
- When the organizations differ: `<who> is in another organization: ` followed by `holdwire.MoveThinking`.
- When either organization is unknown: `rota cannot tell whether <who> is in the same organization: ` followed by `holdwire.MoveThinking`.

The `warnings` are the codes `turn`, `organization` and `organization_unknown`.

`holdwire.MoveThinking` is, verbatim: `Claude Code keeps Claude's earlier thinking to the organization it was written in with Sonnet 5.5, Haiku 5.5 and Fable 5.2, not with Opus 5.5, so with those models that thinking is not used there, and Claude Code says the next responses may be slower and use more tokens.`

**`POST /v1/terminals`** gains `"resume": "<conversation id>"` (R10). It is refused `400` for a shell (*a shell has no conversation to resume*) and for a provider other than claude (*resume names a Claude Code conversation; a <provider> account resumes with its own CLI's arguments*). It is also refused `400` beside arguments that pick a conversation themselves: *resume picks the conversation, so args may not pick one too (--resume, --continue, --session-id, --fork-session or --from-pr)*.

**The socket frame** `{"type":"moved","account":{"id":13,"label":"home@x.com","provider":"claude"}}` goes to every viewer after a live move, before the repaint.

## File Structure

| Path | Task | Responsibility |
|---|---|---|
| `internal/holdwire/resume.go` (create), `records.go` (`Resumable`) | 1 | What Resume and Move carry over, what moving costs in words, and what can resume |
| `internal/holdwire/request.go`, `cmd/rota/hold/main.go`, `cmd/rota/hold/session.go`, `cmd/rota/hold/emulate.go` | 2 | A holder that goes on from an offset and knows its conversation from the start |
| `cmd/rota/client/change.go` (create), `client.go` | 3 | `Change`, `Create.Resume`, and the description's extra fields |
| `api/held_change.go` (create), `held_spawn.go`, `terminal.go`, `held.go`, `principal.go`, `server.go`, `fakeholder_test.go`; `docs/reference.md` (one row) | 4 | The route, resume, forget, and the launch path split into prepare, spawn and list |
| `api/terminals.go` | 5 | `resume` on a new terminal |
| `api/held_move.go` (Task 4's stub, then Task 6) | 6 | A move: its checks, its costs, and a stopped session's move |
| `api/held_handover.go` (Task 6's stub, then Task 7), `api/terminal.go` | 7 | A live session's move in place |
| `cmd/rota/tui/sessions_backend.go` (create), `options.go`, `model.go`, `p4_fakes_test.go` (create) | 8 | The contract: the two interfaces, their types, `shownMsg` and `sessionsMsg`, and the test fakes |
| `cmd/rota/tui/sessions_client.go` (create) | 9 | `SessionBackend` over the daemon's client |
| `cmd/rota/tui/sessions.go`, `sessions_view.go`, `sessions_layout.go` (create), and the stubs `preview.go`, `sessions_act.go`, `sessions_new.go`, `sessions_move.go` | 10 | The Sessions tab: list, details, ladder, filter, keys |
| `cmd/rota/tui/preview.go` | 11 | The live preview |
| `cmd/rota/tui/sessions_act.go` | 12 | Attach, watch, resume, stop, forget, copy |
| `cmd/rota/tui/sessions_new.go` | 13 | New |
| `cmd/rota/tui/sessions_move.go` | 14 | The move picker, and the offer when an account crosses its limit |
| `cmd/rota/tui/services.go`, `services_view.go` (create), and the stub `invite_qr.go` | 15 | The Services tab |
| `cmd/rota/tui_services.go` (create) | 16 | `ServiceBackend` from the store's files and `rota serve` |
| `cmd/rota/tui_sessions.go` (create), `tui_entry.go`, `tui_ssh.go` | 17 | Both backends wired into the local TUI and the SSH TUI |
| `cmd/rota/tui/invite_qr.go`, `cmd/rota/go.mod`, `go.sum` | 18 | The QR code (O2) |
| `cmd/rota/tui/phase4_tabs.go` (create), goldens, `docs/reference.md`, `docs/releases/1.4.0.md` | 19 | Registration, the manual, the notes |

## Parallel groups

Tasks in one group share no file. A group starts when every task it names, and every outside interface its tasks need, is in `phase-4`. The two tracks, server (S) and TUI (U), run side by side.

| Group | Tasks (depends on) | Needs from outside | Starts after |
|---|---|---|---|
| S-A | 2 (none), 3 (none) | nothing. Task 2 shares `cmd/rota/hold/session.go` with phase 2 Task 14 and phase 5 Task 2, and Task 3 shares `client.go` with phase 2 Tasks 1 and 8, so each is cut once those are in `phase-4`, or merges a few lines | the base |
| S-B | 1 (none), 5 (none) | lane `sec-rc` | sec-rc in `phase-4` |
| S-C | 4 (1, 2) | sec-rc; phase 5 Task 7 (optional, R32) | S-A, S-B |
| S-D | 6 (4) | as S-C | S-C |
| S-E | 7 (6) | phase 2 Task 14; phase 5 Task 7 (optional) | S-D and phase 2 Task 14 |
| U-A | 8 (3) | phase 2 Task 1; phase 3 Tasks 1, 6 and 7 | S-A's Task 3, phases 2 and 3 |
| U-B | 9 (8), 10 (8), 15 (8), 16 (8) | 9: phase 2 Task 1. 10, 15: phase 3 Tasks 7 and 14. 16: phase 2 Task 12, phase 5 Tasks 3 and 9 | U-A |
| U-C | 11 (10), 12 (10), 13 (10), 14 (10) | 11: phase 2 Task 4. 12, 14: phase 3 Task 14 | U-B's Task 10 |
| U-D | 17 (9, 16) | phase 2 Task 10; phase 3 Tasks 12 and 20 | U-B |
| F | 19 (all but 18) | phase 3 Task 21 | S-E, U-C, U-D |
| G | 18 (15) | the owner's answer to O2 | U-B's Task 15, at any time after |

Tasks 1, 2, 3 and 5 need no other phase at all, and go first. The critical path is 2 → 4 → 6 → 7 → 19 on the server, and 8 → 10 → 11 → 19 in the TUI. Groups S-A to S-E need nothing from the TUI, and groups U-A to U-C need nothing from the server tasks but Task 3, because the TUI tasks test against fakes. Task 17 and Task 19 are where the two tracks meet.

---

### Task 1: What Resume and Move carry over (`internal/holdwire`)

**Files:**
- Create: `internal/holdwire/resume.go`, `internal/holdwire/resume_test.go`
- Modify: `internal/holdwire/records.go` (`Resumable` and its doc comment), `internal/holdwire/records_test.go`

**Interfaces:**
- Consumes sec-rc's `valueFlags` and `remoteFlags` (`internal/holdwire/remote.go`, same package), and sec-rc's constant for the injected disable-Remote-Control `--settings` value.
- Produces:
  - `func ResumeArgs(stored []string, conversation string) (args []string, dropped int)`. These are a claude session's saved arguments rebuilt by R3, with `--resume <conversation>` first. `dropped` counts the person's words left out: positionals, the words after `--`, and the word after an unknown flag. It is only ever logged as a number.
  - `const MoveThinking` with the exact text in *The HTTP contract*.
  - `func (r *Record) Resumable() bool`. It is true for state ended or interrupted, `Meta.Kind == KindAccount`, `rota.Flavor(Meta.Provider) == "claude"` and `Meta.Conversation != ""`.
  - Unexported `resumeDrop`, the flags dropped together with their optional value, and `resumeVariadic`: `--add-dir`, `--allowedTools`, `--allowed-tools`, `--disallowedTools`, `--disallowed-tools`, `--tools`, `--mcp-config`, `--betas`, `--plugin-dir` and `--plugin-url`.
- The decisions:
  - `--remote-control` and `--rc` are kept, with the name after them when one follows that does not start with `-`. Remote Control is decided by the launch path, not here.
  - `--settings V` and `--settings=V` are kept unless `V` is sec-rc's injected value.
  - An inline `--flag=value` is kept whole.

- [ ] **Step 1: Write the failing tests.**
  - `TestResumeArgsNeverReplayAPrompt`, a table, with conversation `c1`:
    - `[--model opus "fix the bug"]` → `[--resume c1 --model opus]`, dropped 1
    - `[--permission-mode plan -- "do x" y]` → `[--resume c1 --permission-mode plan]`, dropped 2
    - `[--worktree wt1 --model m]` → `[--resume c1 --model m]`
    - `[-w]` → `[--resume c1]`
    - `[--resume old -c --session-id x --fork-session --from-pr 12 --model m]` → `[--resume c1 --model m]`
    - `[--add-dir a b --model m]` → kept whole after `--resume c1`
    - `[--frobnicate hello]` → `[--resume c1 --frobnicate]`, dropped 1
    - `[-p hi]` → `[--resume c1]`, dropped 1
    - `[--model=opus prompt]` → `[--resume c1 --model=opus]`, dropped 1
    - `[remote-control]` → `[--resume c1]`, dropped 1
    - `[]` → `[--resume c1]`
  - `TestResumeArgsLeaveRemoteControlToTheLaunch`:
    - `[--remote-control]` and `[--rc desk --name api]` are kept as they are.
    - `[--settings <sec-rc's value>]` and `[--settings=<sec-rc's value>]` are dropped, with dropped 0.
    - `[--settings {"remoteControlAtStartup":true}]` is kept.
  - `TestResumableMeansStoppedOnAKnownConversation`: these are true:
    - ended with a conversation
    - interrupted with one

    These are false:
    - live
    - unreadable
    - broken
    - codex with a conversation
    - a shell
    - claude with none
  - `TestMoveThinkingNamesTheModels`: it contains `Sonnet 5.5`, `Haiku 5.5`, `Fable 5.2`, `not with Opus 5.5` and `slower and use more tokens`.
  - `FuzzResumeArgs`: it never panics. The result starts with `--resume c1`, holds no word that stood after `--`, and is never longer than `len(stored)+2`.
- [ ] **Step 2: Run them.** `<testenv> go test -count=1 ./internal/holdwire/` fails: `ResumeArgs` does not exist.
- [ ] **Step 3: Implement** `resume.go` as one pass over the arguments, and widen `Resumable`.
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add internal/holdwire && git -C <worktree> commit -m "Resume and Move rebuild a session's arguments without its prompt or its old conversation, and a stopped claude session on a known conversation can be resumed whether it ended or was interrupted"`

### Task 2: A holder that takes a session up where it left off

**Files:**
- Modify:
  - `internal/holdwire/request.go` and `request_test.go`
  - `cmd/rota/hold/session.go` (`Config.Offset`; the ring is `ring.NewAt(cfg.Scrollback, cfg.Offset)`)
  - `cmd/rota/hold/emulate.go`, wherever the screen's offset is counted from
  - `cmd/rota/hold/main.go`, which seeds `meta.json` and passes `Offset`
- Create: `cmd/rota/hold/offset_test.go`, `cmd/rota/hold_resume_unix_test.go` (package `main`, process-level, on `hold_rig_test.go`'s `holdRequest` and `startHolderProc`)

**Interfaces:**
- Produces:
  - `holdwire.Request.Conversation string` (`conversation,omitempty`)
  - `holdwire.Request.ConversationCwd string` (`conversation_cwd,omitempty`)
  - `holdwire.Request.Offset int64` (`offset,omitempty`)
  - `hold.Config.Offset int64`
- The decisions:
  - Every offset the holder says goes on from `Offset`: `Welcome.Oldest` and `End`, `Output`, `ScreenHead.Offset`, and a `Since` answered from what is kept.
  - `meta.json` starts with `conversation` and `conversation_cwd` from the request. A registry that names a conversation replaces them, as today. One that never appears leaves them.
  - A negative `Offset` ends the holder before Ready, with *the request's offset is negative*.
  - The daemon always starts holders of its own binary, so no older holder ever reads these fields.

- [ ] **Step 1: Write the failing tests.**
  - `TestRequestCarriesTheConversationAndTheOffset` (holdwire): the three fields round-trip, and their absence decodes as zero.
  - `TestASessionGoesOnFromItsOffset` (hold, on its rig's fake child): `Start(Config{Offset: 1000})`, then the child prints `hi`.
    - A link's welcome has `Oldest 1000`.
    - The first `Output` is at 1000.
    - `Since{0}` is answered from 1000.
    - A `ScreenAsk` is answered at offset 1002.
  - `TestTheRequestedConversationIsRecordedFromTheStart` (cmd/rota, unix):
    - A holder is started on the tty fake with `Conversation: "c1"` and `ConversationCwd: <dir>`. `meta.json` says `c1` and `<dir>` before the fake prints anything.
    - After the fake's `registry idle` line, it says the registry's id, `fake-<childpid>`.
  - `TestANegativeOffsetIsRefused`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** **race** and **docker**. The local CI run passes, and `TestXVTMatchesXterm` and the rest of `./cmd/rota/hold/` pass unchanged.
- [ ] **Step 5: Commit.** `git -C <worktree> add internal/holdwire cmd/rota/hold cmd/rota/hold_resume_unix_test.go && git -C <worktree> commit -m "A holder can take a session up where its output left off and know its conversation from the start"`

### Task 3: The client changes a held session

**Files:**
- Create: `cmd/rota/client/change.go`, `cmd/rota/client/change_test.go`
- Modify: `cmd/rota/client/client.go` (`Create.Resume`, `createDoc.Resume`, `Session`'s new fields, `session`, `MarshalJSON`)

**Interfaces:**
- Produces:
  - `type Change struct { Resume bool; Account int; Confirm, Forget bool; Cols, Rows uint16; Env []string }`. As JSON it is `resume`, `account`, `confirm`, `forget`, `cols`, `rows` and `env`, each left out when zero.
  - `func (c *Client) Change(id string, ch Change) (Session, error)`. This is `PATCH /v1/terminals/{id}`. Forget's `{"ok":true}` gives a zero `Session`. A refusal is a `*Refusal` as `do` makes one, its `Reason` included (`confirm`). It has a body, so it is given `holderWait`.
  - `Create.Resume string`, sent as `resume`.
  - `Session` gains `ConversationCwd`, `WaitingFor`, `Holder` and `MadeBy string`, `Reachable bool`, `Viewers []SessionViewer`, `Cols` and `Rows uint16`, and `TokenUntil time.Time`.
    - The JSON keys are `conversation_cwd`, `waiting_for`, `holder` (`null` reads as `""`), `made_by`, `reachable`, `viewers`, `cols`, `rows` and `token_until`.
    - `type SessionViewer struct { Name, Role string; Since time.Time }` is read from `{"name","role","since"}`. It is named so as not to collide with phase 2's `client.Viewer`.

- [ ] **Step 1: Write the failing tests**, with a handler on a `ctl.sock` that records the method, the path and the body (the pattern of `client_test.go`).
  - `TestChangeSendsOneOfResumeMoveOrForget`: `Change{Resume: true, Cols: 100, Rows: 30}` sends `PATCH /v1/terminals/0123456789abcdef` with exactly `{"resume":true,"cols":100,"rows":30}`. `Change{Account: 13, Confirm: true}` sends `{"account":13,"confirm":true}`, and `Change{Forget: true}` sends `{"forget":true}`. A 200 description decodes into `Session`.
  - `TestAConfirmIsARefusalWithItsReason`: `409 {"error":"Move x to #13 h?","reason":"confirm","warnings":["turn"]}` gives `*Refusal{Status: 409, Reason: "confirm", Message: "Move x to #13 h?"}`.
  - `TestCreateCarriesResume`: the body has `"resume":"c1"`, and no `resume` when it is empty.
  - `TestSessionReadsWhoWatchesAndWhereItsConversationIs`: every new field from a description document, with `holder: null` read as `""`.
  - `TestAMadeSessionWritesTheNewFields`: a `Session` made in Go marshals and unmarshals back equal.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota/client && git -C <worktree> commit -m "The client resumes, moves or forgets a held session, starts one on a conversation, and reads who watches a session and where its conversation is"`

### Task 4: `PATCH /v1/terminals/{id}`: resume and forget

**Files:**
- Create:
  - `api/held_change.go`
  - `api/held_move.go`, a stub: `moveHeld` answers `409` with reason `not_held` and *moving a session is not built yet*, which Task 6 replaces
  - `api/held_change_test.go`
  - `api/held_change_unix_test.go` (`//go:build linux || darwin`)
- Modify:
  - `api/held_spawn.go`: `startHeld` is split into `prepareHeld`, `spawnLinked` and `startHeldAs`, and changes no behaviour
  - `api/terminal.go`: `finish` drops through `dropTerminalIf`, and `recordedEnd` is factored out of `newRecorder`
  - `api/held.go`: `describe`'s `resumable` follows R5
  - `api/principal.go`: the row `"PATCH /v1/terminals/{id}": RoleControl`, beside the other terminal rows
  - `api/server.go`: the route in `guardedIn`'s terminal group
  - `api/fakeholder_test.go`: the fake honours `Request.Offset`, `Conversation` and `ConversationCwd`, and gains `FAKE_HOLDER_FAIL_IF=<path>`, which fails Ready with kind `other` once that path exists
  - `docs/reference.md`: the route's row, so that `TestReferenceDocumentsEveryRoute` passes

**Interfaces:**
- Consumes `holdwire.ResumeArgs` and `Record.Resumable` (Task 1), the `Request` fields (Task 2), sec-rc's `argsFor`, `localPrincipal`, `envLocal` and `envEmpty`, and phase 5's `termRequest.why` and `whyResumed` (R32).
- Produces, all unexported:
  ```go
  type termChange struct { Resume bool; Account int; Confirm, Forget bool; Cols, Rows uint16; Env []string } // json: resume, account, confirm, forget, cols, rows, env
  type changeRefusal struct { code int; reason, msg string; warnings []string } // Error() is msg; written {"error","reason"[,"warnings"]}
  type heldAs struct { id, conversation, convCwd string; offset int64 }
  func (s *Server) changeTerminal(w http.ResponseWriter, r *http.Request)
  func (s *Server) resumeHeld(r *http.Request, id string, ch *termChange) (*termSession, error)
  func (s *Server) forgetHeld(r *http.Request, id string) error
  func (s *Server) moveHeld(r *http.Request, id string, ch *termChange) (*termSession, error) // stub; Task 6
  func (s *Server) stoppedRecord(id string) (*holdwire.Record, error) // the fresh record of a stopped held session, or a changeRefusal
  func (s *Server) takeStopped(id string) error  // under termsMu: refuses a live or pending id, drops an ended one from s.terms, marks it pending
  func (s *Server) startStopped(r *http.Request, id string, req *termRequest, as *heldAs) (*termSession, error) // reserveTerminal, terminalCwd, takeStopped, startHeldAs
  func (s *Server) prepareHeld(r *http.Request, kind string, req *termRequest, cwd string, as *heldAs) (hreq *holdwire.Request, label, provider string, err error)
  func (s *Server) spawnLinked(hreq *holdwire.Request) (*heldTerm, *holdwire.WelcomeMsg, *holdwire.Ready, error)
  func (s *Server) startHeldAs(r *http.Request, kind string, req *termRequest, cwd string, list func(*termSession) bool, as *heldAs) (*termSession, error)
  func (s *Server) dropTerminalIf(ts *termSession) // deletes s.terms[ts.id] only while it is ts
  func recordedEnd(dir, id string) int64           // where a recording's output got to, as newRecorder computes it
  func heldSize(cols, rows uint16) (uint16, uint16) // 0 or missing gives 120×40, then boundSize
  ```
- The decisions:
  - **`changeTerminal`.** It reads the body strictly and enforces *exactly one*. It applies the `env` gate exactly as `createTerminal` does. It dispatches to the three handlers and writes `200 view()`, `200 {"ok":true}`, a `changeRefusal`, or anything else through `s.report`. A server without `HoldCommand` answers `409 not_held`.
  - **`resumeHeld`, in order:**
    1. `stoppedRecord(id)`.
    2. `Resumable()`, else `409 conversation`, with these sentences:
       - a shell: *a shell has no conversation to resume*
       - another provider: *only a Claude Code session resumes: rota knows its conversation from Claude Code's own registry*
       - claude without one: *rota never learned which conversation session <name> was in; rota run <account> --resume <id> resumes one by its id*
    3. `args, n := ResumeArgs(m.Args, m.Conversation)`.
    4. `req := termRequest{Account: m.Account, Args: args, Cwd: <R4>, Label: m.Name, Env: ch.Env}`, with `heldSize(ch.Cols, ch.Rows)`.
    5. `as := heldAs{id, m.Conversation, m.ConversationCwd, max(recordedEnd(…), <the ended ts's end>)}`.
    6. `startStopped`.
    7. Audit `terminal resumed`, with `by` and `dropped n`.
  - **What the rest of the launch path decides.** `prepareHeld` runs `chooseTerminal` (which applies `Launchable`) and `argsFor` (sec-rc's rule: `403` over TCP, `readyRemoteControl` over the socket, and the disable-Remote-Control `--settings` for an account whose setting is off) for the account the session lands on.
  - **`startHeldAs`.** It is `startHeld` with `as`. It takes `as.id` (else `runID()`), seeds the request's conversation and `Offset`, and records with `recordTerminalFrom(ts, as != nil)`.
  - **Failures.** A holder that fails leaves the record as it was, and still resumable.
  - **`forgetHeld`.** `stoppedRecord`, then the directory's own lock (`holdwire.Lock`; `ErrHeld` is `409 running`), then `os.RemoveAll(dir)`, then `dropTerminalIf` of an ended one, then audit `held session forgotten` with `by`. A worktree and the recordings are never touched (R9).
  - **The record path.** `stoppedRecord` uses `holdwire.Judge`:
    - a running record is `409 running`
    - an unreadable one is `409 foreign`
    - a terminal that is not held is `409 not_held`
    - an unknown id is `404`

- [ ] **Step 1: Write the failing tests.**
  - `TestTheRouteIsControlAndInTheTerminalGroup` (`held_change_test.go`): the role-table test passes with the new row. A watch principal gets `403`. A server whose terminal group is off answers `404`.
  - `TestAChangeIsOneThing`: `{}`, `{"resume":true,"forget":true}` and `{"x":1}` are each `400`.
  - The rest are in `held_change_unix_test.go`, on `withHeld` and the fake holder, with `FAKE_HOLDER_CONVERSATION=conv-1` and a fake that exits on the typed line `exit 0`.
  - `TestResumeStartsTheSameSessionOnItsConversation`:
    - The session is started with args `[--model opus "fix it"]` and has exited.
    - `PATCH {"resume":true,"cols":100,"rows":30}` gives `200` with the same `id`, `state` `live` and 100×30.
    - The fake's logged request has the same `ID`, `Args` `[--resume conv-1 --model opus]`, `Conversation` `conv-1`, and `Offset` at least the old end.
    - `GET /v1/terminals` lists the id once.
  - `TestAnEndedAndAnInterruptedSessionAreBothResumable`: `resumable` is true in both descriptions, and false for a codex session.
  - `TestResumeRefusesWhatItCannot`, with each sentence:
    - live: `409 running`
    - a codex session, a shell, and a claude session with no conversation: `409 conversation`
    - an unknown id: `404`
    - a foreground server: `409 not_held`
  - `TestResumeHoldsRemoteControlToTheRule`, with saved args `[--remote-control]`:
    - From a token principal: `403` with sec-rc's sentence, no holder started, and the account's setting unchanged.
    - Over the socket: `200`, the setting is on, and the args carry `--remote-control`.
  - `TestResumeDecidesDisablingAfresh`, with saved args holding sec-rc's injected `--settings`:
    - For an account whose setting is now on, the new request has no disabling `--settings`.
    - For one whose setting is off, it has exactly one.
  - `TestEnvIsTakenOnlyOverTheSocket`: from a token, `403 envLocal`. Over the socket, the fake's request carries it. `[]` is `400`.
  - `TestTwoResumesStartOneHolder` (Review Focus 3): two PATCHes at once. One `200` and one `409 moving`, and the fake log holds one request.
  - `TestAResumeKeepsTheRecordingWhole`: with `Record` on, output before and after the resume is in one `.out` file in order, with no byte twice and no gap line.
  - `TestAnEndedSessionInMemoryIsReplacedOnce`: with `keepEnded` at 100 ms, a resumed session is still listed and live after 300 ms (`dropTerminalIf`).
  - `TestAResumeIntoAFolderThatIsGoneIsRefused` (Review Focus 4): `conversation_cwd` is removed. The answer is `400` with R4's sentence, and the record is unchanged.
  - `TestAResumeThatCannotStartLeavesTheRecord` (Review Focus 1): with `FAKE_HOLDER_FAIL_IF` present, the answer is `500` and the session is still listed stopped and resumable.
  - `TestForgetTakesTheRecordAway`:
    - A stopped session: `200`. Then `GET` is `404`, its directory is gone, and its `cwd` and its recording are untouched.
    - A live one: `409 running`.
  - Where phase 5 is on the base: `TestAResumeIsSaidAsResumed`, a `session.started` with `reason resumed`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement**, and the row: `` | `PATCH` | `/v1/terminals/{id}` | control | a held session changed: `{"resume":true,"cols","rows"}` starts a stopped one again on its conversation, `{"account":N}` moves it to another claude account, `{"forget":true}` forgets a stopped one; the answer is the description, or `409` with a `reason` | ``.
- [ ] **Step 4: Verify.** **race** and **docker**. The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add api docs/reference.md && git -C <worktree> commit -m "PATCH /v1/terminals/{id} resumes a stopped session under its own id on its conversation, or forgets it, holding its arguments to the Remote Control rule"`

### Task 5: A new terminal on a conversation

**Files:**
- Modify: `api/terminals.go`. `termRequest.Resume string` is sent as `resume,omitempty`. `argsFor` applies it, and `startTerminal` refuses it for a shell.
- Create: `api/terminals_resume_test.go` (`//go:build linux || darwin`)

**Interfaces:**
- Consumes sec-rc's `argsFor(r, st, a, req, cwd)` and `sessions.CopyForResume`.
- Produces `resume` on `POST /v1/terminals` (R10), with the three refusals in *The HTTP contract*.
- The decisions:
  - **Order in `argsFor`:** the checks, then `CopyForResume(st, a, req.Resume)`, then `--resume <id>` prepended, then sec-rc's Remote Control rule over the result.
  - **A copy that fails** is logged, and answers `500` *the conversation <id8> could not be copied to account N*.
  - **Both paths take it,** a server-run terminal and a held one, since both go through `argsFor`.

- [ ] **Step 1: Write the failing tests.**
  - `TestANewSessionResumesAConversation`, for a held session (fake holder) and a server-run one (the tty fake, whose argv is recorded):
    - Account B is given `resume: "c1"`, and the transcript is only in account A's home, written as `sessions/handoff_test.go` lays one out.
    - The argv starts `--resume c1`.
    - The transcript is now in B's home.
  - `TestResumeIsClaudeOnlyAndNotForAShell`.
  - `TestResumeBesideAConversationFlagIsRefused`: `args: ["-c"]` with `resume`.
  - `TestResumeGoesThroughTheRemoteControlRule`: `resume` with `args: ["--remote-control"]` from a token is `403`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** **race** and **docker**. The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add api && git -C <worktree> commit -m "A new terminal can resume a Claude Code conversation by its id, with the transcript copied to its account first"`

### Task 6: Moving a session: its checks and its costs, and a stopped session's move

**Files:**
- Modify: `api/held_move.go`, which replaces Task 4's stub
- Create:
  - `api/held_handover.go`, a stub: `moveLive` answers `409 running` with *a live session moves once its CLI is stopped; s stops it*, which Task 7 replaces
  - `api/held_move_test.go` (pure)
  - `api/held_move_unix_test.go`

**Interfaces:**
- Consumes everything Task 4 produces, `rota.Account.Org`, `store.Launchable`, sec-rc's `remoteArgs`, `sessions.CopyForResume`, and phase 5's `whyMoved` (R32).
- Produces:
  ```go
  type movePlan struct { name string; live bool; from, to *rota.Account; req termRequest; as heldAs; costs []string }
  func (s *Server) planMove(r *http.Request, ts *termSession, rec *holdwire.Record, ch *termChange) (*movePlan, error)
  func moveCosts(live bool, from, to *rota.Account) []string // "turn", "organization", "organization_unknown"
  func moveSentence(name string, to *rota.Account, costs []string) string
  func (s *Server) moveHeld(r *http.Request, id string, ch *termChange) (*termSession, error)
  func (s *Server) moveLive(r *http.Request, ts *termSession, p *movePlan) (*termSession, error) // stub; Task 7
  ```
- **Before anything changes**, in this order, so a refusal costs nothing:
  1. The session: held, not foreign, claude, on a known conversation. Each refusal is as in Task 4.
  2. The target: it exists (`404 no account N`), it is not the session's own account (`400`), it is the same flavour (`409 provider`), and it passes `Launchable`.
  3. `ResumeArgs`.
  4. `remoteArgs(r, to, args, cwd)`, which has no side effect, so `403` over TCP comes before any confirm.
  5. The costs.
  6. Without `confirm`, and with any cost, `409 confirm` with `moveSentence` and `warnings`.
- **A stopped session:**
  1. `CopyForResume(st, to, conversation)`. A failure is `500`, with the sentence of Task 5.
  2. `startStopped` with `req.why = whyMoved`.
  3. Audit `terminal moved` with `account`, `from` and `by`.
- **A live session** goes to `moveLive`.
- **`from`** is `nil` when the session's account has been removed. Its organization is then unknown, and nothing is copied from it.

- [ ] **Step 1: Write the failing tests.**
  - `TestTheCostsOfAMove` (pure), a table:
    - stopped, same org → none
    - stopped, orgs `o1` and `o2` → `organization`
    - stopped, either org `""` or `from` nil → `organization_unknown`
    - live, same org → `turn`
  - `TestTheConfirmSaysWhatItCosts` (pure): each sentence of *The HTTP contract*, exactly.
  - `TestMoveAStoppedSessionToAnotherClaudeAccount`:
    - Accounts A and B are both in org `o1`.
    - The answer is `200`, with the same id and the `account` B.
    - The transcript is copied to B's home.
    - The fake's request names B, with `--resume conv-1` first.
  - `TestAMoveAcrossOrganizationsAsksFirst`:
    - Orgs `o1` and `o2`: `409` with reason `confirm`, warnings `["organization"]` and the exact sentence, and nothing started.
    - With `confirm: true`: `200`.
  - `TestAMoveThatWouldBeRefusedIsRefusedBeforeItAsks`: from a token, with saved `--remote-control`: `403`, never `409 confirm`, and B's setting unchanged.
  - `TestAMoveAcrossProvidersIsRefusedByName`, `TestAMoveToItsOwnAccountIsABadRequest` and `TestAMoveToADeadAccountChangesNothing`. Each leaves the record exactly as it was.
  - `TestOnlyClaudeSessionsMove`: a held codex session gives `409 conversation`.
  - `TestAMoveFromARemovedAccountStillStarts` (Review Focus 4):
    - Account A is removed. Without confirm: `409 confirm` with `organization_unknown`.
    - With it: `200` on B.
  - Where phase 5 is on the base: `TestAStoppedMoveIsSaidAsAMove`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** **race** and **docker**. The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add api && git -C <worktree> commit -m "A stopped claude session moves to another claude account, asking first when that costs Claude's earlier thinking"`

### Task 7: A live session moves in place

**Files:**
- Modify:
  - `api/held_handover.go`, which replaces Task 6's stub
  - `api/terminal.go`:
    - `termSession` gains `hand *handover`
    - `run` becomes a loop
    - `kill` lets a Stop cancel a handover
    - `kill`'s timer ends only the far end it was set for
- Create: `api/held_handover_unix_test.go`

**Interfaces:**
- Consumes `planMove`, `prepareHeld`, `spawnLinked` and `changeRefusal` (Tasks 4 and 6), phase 2's `repaintViewers`, and phase 5's `endingAs`, `sessionEnded`, `sessionStarted` and `whyMoved` (R32).
- Produces:
  ```go
  type handover struct { hreq *holdwire.Request; label, provider string; to *rota.Account; by string; done chan error; cancel string }
  func (s *Server) moveLive(r *http.Request, ts *termSession, p *movePlan) (*termSession, error)
  func (ts *termSession) takeOver(code int) (next bool) // run's step after its far end returns with a handover set
  ```
- **`moveLive`:**
  1. `prepareHeld` for the target. Remote Control is readied here, before the hang-up.
  2. Under `ts.mu`: refuse with `409 moving` when the session has ended, is being killed or has been let go of, or has a handover. Otherwise set `hand`.
  3. Audit `terminal moving` with `account` and `by`. Then `ts.endingAs(whyMoved, "is moving to <label>")`, then `ts.kill(by)`.
  4. Wait on `hand.done` for at most 90 s. The answer is `ts.view()` or the error.
- **`run`:** `code := back.run(ts)`. A session let go of returns. One with a handover calls `takeOver(code)`, and on true loops. Otherwise it calls `finish(code)`.
- **`takeOver`:**
  1. A `cancel` set by a Stop: `endingAs("", "")`, answer `409 moving` with *a stop by <who> ended the move; the session is stopped*, and return false.
  2. Wait for the directory's lock to be free, polling every 50 ms for up to `holdSettle` plus 1 s.
  3. `s.isStopping()` fails it.
  4. `CopyForResume`.
  5. `hreq.Offset = ts.end()`, then `spawnLinked`.
  6. If Stop has begun meanwhile, the new holder is let go running (`left`), and the answer is `503` *this server is stopping; session <name> moved and goes on running, for the next rota daemon*.
  7. Otherwise, under `ts.mu`, set `back`, `account`, `label`, `provider`, `tokenUntil`, `cwd` (the folder it started in), `killing = false` and `hand = nil`, then `tell` the `moved` frame.
  8. After the lock: `repaintViewers()`, audit `terminal moved`, `sessionEnded(code)`, then `sessionStarted(ts, by, whyMoved, "")`, then `done <- nil`, and return true.
- **Any failure after the hang-up:** `endingAs("", "")`, then answer `502` *session <name> was stopped, but could not be started on #N: <why>; r resumes it where it was*, and return false, so `finish` sends the exit and the record is resumable.
- **`kill`:** a later call with a `by` while a handover's hang-up is under way sets `hand.cancel = by`, and phase 5's `stoppedBy` where it exists, before its early return. The timer's `back.end()` runs only while `ts.back` is still `back`.

- [ ] **Step 1: Write the failing tests** (fake holder, accounts A and B in one org, a viewer attached by WebSocket):
  - `TestAMoveKeepsTheSessionAndItsViewers`:
    - `PATCH {"account":B,"confirm":true}` gives `200` with the same id and `account` B.
    - The viewer's socket is never closed and gets no `exit`. It gets `moved` with B, then `repaint`, then the new holder's output, at offsets past every one before.
    - The fake log holds two requests, the second with `Offset` equal to the old end.
  - `TestALiveMoveAsksFirst`: without `confirm`, the answer is `409 confirm` whose sentence says the turn is lost, and the CLI still runs.
  - `TestAStopDuringAMoveEndsIt` (Review Focus 3), with `FAKE_HOLDER_RECORD_LATE` slowing the old end:
    - A `DELETE` meanwhile gives `200`.
    - The PATCH gives `409 moving`, with the stop's sentence.
    - The log holds one request, and the session is stopped.
  - `TestAMoveThatCannotStartLeavesItResumable` (Review Focus 1): `FAKE_HOLDER_FAIL_IF`'s path is made after the first holder starts. The PATCH gives `502` with the sentence, the viewer gets `exit`, and the record is ended and `resumable`.
  - `TestTypingDuringAMoveReachesNoCLI` (Review Focus 3), with `FAKE_HOLDER_RECORD`: keystrokes sent between the hang-up and the swap reach neither holder's record, and the viewer gets an error frame.
  - `TestTheOldKillNeverReachesTheNewHolder` (Review Focus 3): with `termKillAfter` short and `FAKE_HOLDER_SIGNALS` on the new holder, no signal reaches it within 2 s of the swap.
  - `TestTheDaemonStoppingMidMoveLeavesItResumable` (Review Focus 1): `srv.Stop()` while the old holder settles. The PATCH fails, and a second server on the store lists the session stopped and resumable.
  - Where phase 5 is on the base: `TestALiveMoveIsSaidAsAMove`, which gives `session.ended` and then `session.started`, both with reason `moved`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** **race** and **docker**. The local CI run passes, and every existing `api` terminal test passes unchanged.
- [ ] **Step 5: Commit.** `git -C <worktree> add api && git -C <worktree> commit -m "A live session moves to another account in place, keeping its id and its viewers, and a stop during the move ends it"`

### Task 8: The TUI's contract for sessions and services

**Files:**
- Create: `cmd/rota/tui/sessions_backend.go`, `cmd/rota/tui/p4_fakes_test.go`, `cmd/rota/tui/contract_test.go`
- Modify:
  - `cmd/rota/tui/options.go`: `Options` gains `Sessions SessionBackend` and `Services ServiceBackend`
  - `cmd/rota/tui/model.go`: `shownMsg` (R26)

**Interfaces:**
- Consumes phase 3's `Options`, `env`, `tab` and `register` (Task 6), `client.Session` and `SessionViewer` (Task 3), and `client.Hello` and `client.Frame` (phase 2 Task 1).
- Produces, in `sessions_backend.go`:
  ```go
  type SessionBackend interface {
      Sessions(ctx context.Context) ([]client.Session, error)                 // GET /v1/terminals; client.ErrNoDaemon when none runs
      Conversations(ctx context.Context) ([]Conversation, error)               // GET /v1/accounts?sessions=1&recent=10
      Create(ctx context.Context, n NewSession) (s client.Session, startedDaemon bool, err error)
      Resume(ctx context.Context, id string, cols, rows int) (client.Session, error)
      Move(ctx context.Context, id string, account int, confirm bool, cols, rows int) (client.Session, error)
      Forget(ctx context.Context, id string) error
      Stop(ctx context.Context, id string) error
      Watch(ctx context.Context, id string) (termStream, error)               // a watch attach for the preview
      Attach(id string, watch bool) (Attachment, error)                       // phase 2's attach, to hand the terminal to
      Defaults() Defaults
  }
  type termStream interface { Hello() client.Hello; Next() (client.Frame, error); Close() error } // *client.Term
  type Attachment interface { tea.ExecCommand; Said() string }             // Said: the one line the attach ended with
  type Defaults struct { Cwd, Host, Store string; OwnEnv, RemoteControl bool }
  type NewSession struct { Account int; Cwd, Label, Resume string; Worktree, RemoteControl bool; Cols, Rows int }
  type Conversation struct { Account int; Label, Provider, ID, Name, Dir string; At time.Time; Shared bool } // sessions.Session's JSON

  type ServiceBackend interface {
      Service(ctx context.Context) (Service, error)
      Act(ctx context.Context, act string) (said string, err error)  // "start", "stop", "install", "uninstall"; Service.Local only
      Edit() *exec.Cmd                                                // $EDITOR on server.toml; nil where there is no terminal to hand
      Invite(ctx context.Context, ttl time.Duration) (Invite, error)
      TestSink(ctx context.Context, sink string) ([]SinkOutcome, error)
  }
  type Service struct {
      Running bool; PID int; Version string; Started time.Time
      Store, Ctl, Log, Config, Unit string; Installed bool
      HTTP, SSH, SSHKey string // SSHKey: the host key's SHA256 fingerprint
      Live, Stopped int
      Sinks []Sink; Audit []AuditLine // Audit nil when not Local
      Local bool; Host, Err string
  }
  type Sink struct { Name, Type, Target, State, LastError, LastTest string; Sent, Dropped int; LastSent, LastErrorAt time.Time }
  type SinkOutcome struct { Sink string; Answered, OK bool; Error string }
  type AuditLine struct { At time.Time; Text string }
  type Invite struct { URL string; Expires time.Time }

  type shownMsg struct{ Place int }                                  // the shell: the tab now shown
  type sessionsMsg struct { List []client.Session; Err error }        // Sessions' poll, read by Services too
  ```
- Produces, in `p4_fakes_test.go`, for Tasks 10 to 16 and 19:
  - `type fakeSessions struct`: scripted answers per method, and `calls() []string`, each call written as `Resume(<id>,<cols>x<rows>)` and so on.
  - `type fakeStream struct`: a `termStream` fed from a channel. It records `Close`, and fails the test if anything else is called on it.
  - `type fakeServices struct`, and `type fakeAttachment struct`: `Run` calls a func, and `Said` returns a set line.
  - `func newP4Env(t *testing.T, o Options, role string, list Listing) *env`, at 80×24 with `NewTheme(colorprofile.Ascii, true)` and the fixed clock of phase 3's tests (Tuesday 2026-11-10 20:17 UTC).
- `Options.Sessions` or `Services` being nil means held sessions are not offered here. Each tab says `held sessions and the rota daemon run on linux and macOS` on Windows, and `no rota daemon can be reached from here` otherwise.

- [ ] **Step 1: Write the failing tests** (`contract_test.go`).
  - `TestShownIsSentOnEverySwitch`: two fake tabs at places 10 and 20. `Init` gives each `shownMsg{10}`. The key `2` gives both `shownMsg{20}`, and `[` gives `shownMsg{10}`.
  - `TestTheFakesAreBackends`: `var _ SessionBackend = (*fakeSessions)(nil)`, and the same for `fakeServices`, `fakeStream` and `fakeAttachment`. `*client.Term` satisfies `termStream`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes, with phase 3's `rules_test.go` included.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota/tui && git -C <worktree> commit -m "The TUI has one contract for held sessions and the daemon's services, and every tab hears which one is shown"`

### Task 9: `SessionBackend` over the daemon's client

**Files:**
- Create: `cmd/rota/tui/sessions_client.go`, `cmd/rota/tui/sessions_client_test.go` (`//go:build linux || darwin` for the tests that start terminals)

**Interfaces:**
- Consumes Task 8's contract, `client.Client`'s `List`, `Create`, `Change`, `Stop` and `Attach` (phase 1, Task 3, phase 2 Task 1), and `client.ErrNoDaemon`.
- Produces:
  ```go
  type SessionsConfig struct {
      API      *http.Client                                              // accounts-level reads: the in-process server locally, the SSH user's client over SSH
      Dial     func(start bool) (c *client.Client, started bool, err error) // the daemon; start may start it
      Attach   func(c *client.Client, id string, watch bool) Attachment
      Env      func() []string                                           // the caller's environment, filtered; nil sends none
      Defaults Defaults
  }
  func NewSessions(cfg SessionsConfig) SessionBackend
  ```
- The decisions:
  - `Sessions`, `Resume`, `Move`, `Forget`, `Stop`, `Watch` and `Attach` call `Dial(false)`. Only `Create` calls `Dial(true)`.
  - `Create` sends `Args: ["--worktree"]` when `Worktree` is set. It also passes `Cwd`, `Label`, the size, `Env: cfg.Env()` when `Env` is set, `RemoteControl` and `Resume`.
  - `Resume` and `Move` send `Env` the same way.
  - `Conversations` reads `GET http://rota/v1/accounts?sessions=1&recent=10` through `API`, up to 8 MiB, and decodes its `sessions` array.
  - `Watch` is `c.Attach(id, true)`.

- [ ] **Step 1: Write the failing tests**, against `api.New` without `HoldCommand` (server-run terminals with the tty fake), served by `client.InProcess(srv.LocalHandler(local))`.
  - `TestSessionsListsWhatTheDaemonHolds`: two created terminals are listed with their names.
  - `TestNoDaemonIsAnAnswer`: a `Dial` giving `client.ErrNoDaemon` makes `Sessions` return it.
  - `TestCreateStartsTheDaemonOnlyForASession`: a `Dial` fake records its `start` argument. `Create` asks `true` and returns `startedDaemon`. `Sessions` asks `false`.
  - `TestANewSessionCarriesWhatWasChosen`: the request has `args ["--worktree"]`, `remote_control: true`, `resume`, `cwd`, `label`, the size and `env`. With `Env` nil, there is no `env`.
  - `TestChangesArePATCHes`, with a recording handler: `Resume`, `Move` with confirm, and `Forget` send Task 3's bodies.
  - `TestWatchStreamsTheTerminal`: `Watch` gives a hello at the terminal's size, then its output.
  - `TestConversationsComeFromTheListing`: a seeded claude transcript is listed with its id, name, dir and account.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** **race**. The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota/tui && git -C <worktree> commit -m "The TUI reaches held sessions through the daemon's client, and asks for a daemon to be started only when a session is started"`

### Task 10: The Sessions tab

**Files:**
- Create:
  - `cmd/rota/tui/sessions.go`: state, polling, selection, filter and dispatch
  - `cmd/rota/tui/sessions_view.go`: rows and details
  - `cmd/rota/tui/sessions_layout.go`: the ladder
  - `cmd/rota/tui/sessions_test.go`
  - goldens `sessions-60x20.txt`, `sessions-80x24.txt` and `sessions-120x40.txt`
- Create, as stubs with the signatures below, returning `nil` or a closed overlay: `preview.go` (Task 11), `sessions_act.go` (Task 12), `sessions_new.go` (Task 13), `sessions_move.go` (Task 14).

**Interfaces:**
- Consumes Task 8, and phase 3's `env`, `toast`, `clean`, `fit`, `col` and `resetText`.
- Produces:
  ```go
  type sessionsTab struct { /* e, keys, list, convs, sel, filter, over map[int]bool, pv *preview, … */ }
  func newSessions(e *env) tab // place 20; registered by Task 19
  type sessionsKeys struct { Up, Down, Attach, Watch, New, Resume, Move, Stop, Forget, Copy, Filter key.Binding }
  func sessionsTabKeys() sessionsKeys // ↑/k, ↓/j, enter, w, n, r, m, s, x, c, /
  type sessRow struct { S *client.Session; C *Conversation }
  func (t *sessionsTab) rows() []sessRow         // R18's order, filtered
  func (t *sessionsTab) current() (sessRow, bool)
  type conversationsMsg struct { List []Conversation; Err error }
  type sessionChangedMsg struct { Session client.Session; Attach bool; Said string } // a start, resume or move was answered
  func sessionsLayout(width int) (list, side int, cols []col)
  // the stubs, which later tasks fill in:
  type preview struct{} // Task 11
  func newPreview(e *env) *preview
  func (p *preview) follow(s *client.Session) tea.Cmd // nil: stop previewing
  func (p *preview) Update(msg tea.Msg) tea.Cmd
  func (p *preview) View(width, height int) string
  func (p *preview) close()
  func (t *sessionsTab) attach(s client.Session, watch bool) tea.Cmd // Task 12
  func (t *sessionsTab) resume(r sessRow) tea.Cmd
  func (t *sessionsTab) stop(s client.Session) overlay
  func (t *sessionsTab) forget(s client.Session) overlay
  func (t *sessionsTab) copyPath(s client.Session) tea.Cmd
  func newSessionForm(e *env, live []client.Session) overlay // Task 13
  func newMovePicker(e *env, r sessRow) overlay             // Task 14
  func (t *sessionsTab) offerMoves(before, after Listing) tea.Cmd
  ```
- **Polling and messages** (R14):
  - `shownMsg{20}` starts the 2 s poll and any other place sets 15 s. The first show also asks for conversations, as does each `sessionChangedMsg`, and a 60 s tick does too.
  - `sessionsMsg` replaces the list. `client.ErrNoDaemon` empties it and shows the no-daemon line, and any other error is a toast.
  - Each `listingMsg` after the first calls `offerMoves(before, after)`.
- **Selection** follows the session's id, or the conversation's id, across polls. A vanished id selects its neighbour. A `sessionChangedMsg` selects its session, and calls `attach(s, false)` when `Attach` is set.
- **Keys:**
  - `⏎` on a live session is `attach(s, !control)`. On a stopped resumable one, and on a conversation, it is `resume`. On any other stopped one it toasts why it cannot be resumed.
  - `w` is `attach(s, true)`.
  - `r`, `s`, `x`, `m` and `n` are gated (`e.gate`) and applied only where they mean something: `r` stopped or conversation, `s` live, `x` stopped, `m` claude.
  - `c` copies.
  - `/` opens the filter. That is a `newInput` field matching name, label, account label, folder and conversation name, case-insensitively. `esc` clears it and leaves it, and `Typing()` is true while it has focus.
  - A `tea.MouseClickMsg` on a row selects it, and the wheel moves the selection.
- **The ladder** (`sessionsLayout`):
  - 100 columns or more: the list is 44 wide, then a gap of 2, then the preview and details.
  - 80 to 99: a list of 38.
  - Below 80: the list takes the width, and the details go under it with no preview.
  - The row columns are `mark` 2, `name` flex 10–24, `status` 11, `account` 14 (`#12 work@…`, which shrinks to `#12` below a list of 44), and `age` 7 (dropped below 50), with a gap of 1.
- **Words** (the page's, phase 6 seams):
  - `running` in Good, `needs you` in Warn, `quiet` in Muted.
  - Stopped: `ended 0`, `ended 3`, `interrupted` or `broken`, in Faint.
  - `made by <v>` and `not answering` in Warn.
  - Group lines: `Live · N`, with ` · M need you` when there are any, then `Stopped · N` and `Conversations · N`.
  - An account crossing its limit (R31) gets `over` in Warn after its account.
- **Details**, in this order (lines with no value are left out):
  1. name
  2. status, with `: waiting for permission` or `: waiting for an answer`
  3. title
  4. `#12 work@… · claude · login`: the credential kind by R29
  5. `folder <cwd>`
  6. `worktree <conversation_cwd> · c copies it`, when it differs from the folder
  7. `conversation <first 8>`
  8. `started 21:20 (2h 3m)` or `stopped 2h ago`
  9. `keyboard <holder>` or `keyboard free`, then ` · N watching`
  10. `size 120×40`
  11. for a foreign session: `made by rota <v>; s stops it`
  12. for a stopped one: how it ended (`ended, exit 0`, or `interrupted: its holder went away`), then ` · r resumes it`, or why it cannot be resumed
- **Empty lists:**
  - No daemon: `No rota daemon is running for this store. n starts a session, and the daemon with it.` For the watch role it is `No rota daemon is running.`
  - None: `No sessions yet. n starts one.`
- `Busy()` is `""`, because sessions run on. `Leave` closes the preview.

- [ ] **Step 1: Write the failing tests.** The fake backend holds:
  - live `refactor` (needs you, account 12)
  - live `audit` (codex account 3, running)
  - stopped `fix-tests` (interrupted, resumable)
  - ended `old` (exit 0, not claude)
  - two conversations, one of them `refactor`'s, which must not be listed

  The tests:
  - `TestTheListAtThreeSizes`: the three goldens.
  - `TestOrderNeverJumps`: three polls in which `refactor`'s status changes and the order the server gives is shuffled leave the same row order.
  - `TestSelectionFollowsTheSession` (Review Focus 5): the selected session vanishes, so its neighbour is selected. It comes back on a later poll, and the selection stays on the neighbour.
  - `TestTheDaemonStoppingSaysSo` (Review Focus 5): an `ErrNoDaemon` poll shows the no-daemon line and nothing panics. The next poll restores the list.
  - `TestANameNeverEscapesItsCell`: names `"\x1b[2Jevil"`, `"日本語のセッション"` and `"👨‍👩‍👧 x"` keep every line at most the width, with no `ESC` inside a cell and each column at one offset.
  - `TestTheFilterNarrowsAndEscClears`, `TestDetailsSayAccountCredentialAndWorktree` (an `apikey` account reads `API key`, and a future `long_until` reads `long token`) and `TestWatchSeesNoChangeKeys` (`Keys()` holds `↑ ↓ enter w c /` only).
  - `TestEnterDoesWhatTheRowAllows`: each stub is called for its row.
  - `TestPollingFollowsWhatIsShown`: with `fakeTicks`, the interval is 2 s after `shownMsg{20}` and 15 s after `shownMsg{10}`.
  - `TestSessionsUpdateDoesNoIO`: `fakeSessions.calls()` stays empty while `Update` runs. Calls appear only when the commands run.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes, with `TestNoFileIsPastSixHundredLines` included.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota/tui && git -C <worktree> commit -m "The Sessions tab lists live and stopped sessions and resumable conversations in an order that never jumps, with each one's account, credential, folder and worktree"`

### Task 11: The preview

**Files:**
- Modify: `cmd/rota/tui/preview.go`, replacing Task 10's stub
- Create: `cmd/rota/tui/preview_test.go`

**Interfaces:**
- Consumes `SessionBackend.Watch`, `termStream` and `shownMsg` (Task 8), `hold.NewScreen`, `Write`, `Refill`, `Resize`, `Cursor`, `OK`, `Close`, `Crop` and `Size` (phase 2 Task 4), and `vtscan.NewScanner`.
- Produces Task 10's preview signatures, and `type previewMsg struct{ gen int; ended, lost string }`.
- The decisions (R13):
  - **Opening.** `follow` waits 250 ms (`e.tick`) under a generation number. Only the newest generation opens a stream, so moving fast past five sessions opens one.
  - **Feeding.** A reader goroutine runs inside the command. It feeds a `hold.Screen` at the hello's size, and only when `holdwire.ScreenFits` allows it. It cuts each burst at the scanner's ground and keeps the rest for the next, with `Refill` once per burst, as phase 2's attach view does. It sends a `previewMsg` at most every 100 ms. A `resized` document resizes the screen.
  - **The end.** An `exit` document, or `io.EOF`, ends the preview with `<name> ended, exit N`. Another error shows `the preview lost its link; it tries again` and redials after 2 s while the session is still selected.
  - **Drawing.** `View` calls `Crop(0, y, width, height)`, with `y = max(0, rows-height)`, moved up only as far as the cursor's row needs. Each row is padded with `fit`. Under `colorprofile.Ascii` the rows are passed through `ansi.Strip`. A session too large to keep a screen reads `too large to preview: W×H; ⏎ attaches`.
  - **What it never does.** It never calls anything on the stream but `Hello`, `Next` and `Close`, so it never types and never resizes.
  - **Closing.** `close` runs on `shownMsg` for any other place, on `follow(nil)`, and on `Leave`.

- [ ] **Step 1: Write the failing tests**, with a `fakeStream`.
  - `TestThePreviewShowsTheBottomOfTheScreen`: a 100×30 hello, then lines `L01`…`L30`. `View(40, 10)` shows `L21`…`L30`, each at most 40 cells.
  - `TestThePreviewFollowsTheCursorUp`: the cursor at row 5 shows rows 5 to 14.
  - `TestThePreviewNeverTypesOrResizes`: the fake fails on any other call.
  - `TestASplitEscapeIsHeldUntilWhole`: `ESC[3` then `1mred` gives a red `red` and no stray `[31m`.
  - `TestThePreviewOpensOnlyAfterTheSelectionRests`: five `follow` calls within 200 ms open one stream.
  - `TestThePreviewSaysWhenItsSessionEnds` (Review Focus 5): an `exit` document gives the ended line. An error gives the lost line, and a redial 2 s later.
  - `TestATooLargeSessionIsNotPreviewed`: a 1000×1000 hello.
  - `TestThePreviewClosesWhenTheTabIsHidden`: `shownMsg{10}` closes the stream.
  - `TestNoColorStripsThePreview`: there is no `ESC` in the view under `Ascii`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** **race**. The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota/tui/preview.go cmd/rota/tui/preview_test.go && git -C <worktree> commit -m "The Sessions tab previews the selected session live at its own size, cropped to the pane and never resized"`

### Task 12: Attach, watch, resume, stop, forget and copy

**Files:**
- Modify: `cmd/rota/tui/sessions_act.go`, replacing Task 10's stub
- Create: `cmd/rota/tui/sessions_act_test.go`

**Interfaces:**
- Consumes Task 8, `handOff`, `copyText`, `toast` and `newConfirm` (phase 3), and `sessionChangedMsg` (Task 10).
- Produces Task 10's five action signatures, and `type attachedMsg struct{ ID, Said string; Err error }`.
- The decisions (R15):
  - **`attach`** builds the `Attachment` inside a command (`e.call`) and hands it off. `attachedMsg` then toasts `clean(Said)` and asks for a poll.
  - **`resume` on a session** calls `Resume(id, e.width, e.height)`, which is the requester's size. Then `sessionChangedMsg{Attach: true, Said: "resumed <name>"}`.
  - **`resume` on a conversation** calls `Create(NewSession{Account, Resume: c.ID, Cwd: c.Dir, Cols, Rows})`. The account is the conversation's own, or for a shared one the first claude account in listing order whose `sessions` is `""` or `shared` (R12).
  - **A started daemon** gives a toast: `started the rota daemon for <store> (control socket only; rota serve stop ends it)`.
  - **`stop`** is a confirm, *Stop <name>? Its CLI is sent a hangup. A worktree it used is left as it is.*, then `Stop`.
  - **`forget`** is a confirm, *Forget <name>? Its record goes; the conversation stays in the account's home, and rota run <account> --resume <conv8> still resumes it.*, then `Forget`.
  - **`copyPath`** copies `conversation_cwd`, else `cwd`, with the toast `copied <path>`.
  - **Refusals** are toasts, cleaned.
  - **A session that ends right after it starts.** When a session this tab started or resumed is listed as ended within 10 s, the toast is `<name> ended right after it started (exit N); its conversation may not be in this account's home` (Review Focus 4).

- [ ] **Step 1: Write the failing tests.**
  - `TestEnterAttachesAndComesBack`: a `handOffMsg` with the attachment. Running its `Done` gives the toast with `Said`, and a poll.
  - `TestWatchAttachesWatching`: for a watch principal, `⏎` asks for `Attach(id, true)` too.
  - `TestResumeStartsAtThisSizeAndAttaches`: at 100×30 the call is `Resume(id,100x30)`, and then an attach follows.
  - `TestResumingAConversationCreatesOnItsAccount`, including the shared case.
  - `TestStartingTheDaemonIsSaid`.
  - `TestStopAndForgetAskFirst`: `n` and `esc` send nothing. `y` sends one call each.
  - `TestCopyGivesTheWorktree`.
  - `TestARefusalIsAToast`.
  - `TestASessionThatEndsAtOnceIsSaid`.
  - `TestActUpdateDoesNoIO`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota/tui/sessions_act.go cmd/rota/tui/sessions_act_test.go && git -C <worktree> commit -m "In the Sessions tab enter attaches, w watches, r resumes, s stops, x forgets and c copies a worktree's path"`

### Task 13: New

**Files:**
- Modify: `cmd/rota/tui/sessions_new.go`, replacing Task 10's stub
- Create: `cmd/rota/tui/sessions_new_test.go`, and the golden `new-80x24.txt`

**Interfaces:**
- Consumes `SessionBackend.Create` and `Defaults` (Task 8), `newInput` (phase 3), the listing, and `sessionChangedMsg` (Task 10).
- Produces `newSessionForm` with Task 10's signature (R16, R17).
- **The fields:**
  - **Account:** `auto (now #1 work@…)`, then every account in listing order. A busy exclusive account or a dead one is Faint, with its reason, and is skipped by `←`/`→`.
  - **Folder:** prefilled with the account's `cwd`, else `Defaults().Cwd`.
  - **Name.**
  - **Worktree:** enabled only for a claude account chosen by id. Disabled, it reads `a worktree is Claude Code's: choose a claude account`.
  - **Remote Control:** shown only when `Defaults().RemoteControl`, for `auto` or a claude account.
  - **Start.**
- **The notes:**
  - Under Worktree: `Claude Code makes a git worktree for the session; files git ignores, such as .env, are not in it.`
  - Under Folder, with `OwnEnv`: `It starts with this terminal's environment.` Without: `It starts with the environment of the rota daemon on <host>, not your shell's.`
- **Keys.** `tab` and `shift+tab` move between fields. `←` and `→` choose, `space` toggles, `enter` on Start submits, and `esc` cancels.
- **The answer.** Success is `sessionChangedMsg{Attach: true}`. A refusal stays beside Start, with every field kept.

- [ ] **Step 1: Write the failing tests.**
  - `TestNewSendsWhatWasChosen`: `NewSession{Account: 12, Cwd, Label: "api", Worktree: true, RemoteControl: true, Cols: 80, Rows: 24}`.
  - `TestTheWorktreeIsClaudesByID`.
  - `TestTheFormSaysWhoseEnvironment`: both lines.
  - `TestABusyAccountIsGreyedWithItsSession`: a codex account with a live held session reads `busy: held by session audit`, and cannot be chosen.
  - `TestRemoteControlIsOfferedOnlyWhereItIsTaken`.
  - `TestARefusalKeepsTheForm`.
  - `TestAResizeKeepsTheForm`: 50×10 and back to 80×24 keep every field.
  - `TestTheFormAt80x24`, a golden.
  - `TestNewUpdateDoesNoIO`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota/tui/sessions_new.go cmd/rota/tui/sessions_new_test.go cmd/rota/tui/testdata/golden/new-80x24.txt && git -C <worktree> commit -m "n in the Sessions tab starts a session on an account or the rotation's choice, in a folder, with a name, a worktree or Remote Control, and says whose environment it starts with"`

### Task 14: Moving a chat from the TUI

**Files:**
- Modify: `cmd/rota/tui/sessions_move.go`, replacing Task 10's stub
- Create: `cmd/rota/tui/sessions_move_test.go`, and the golden `move-80x24.txt`

**Interfaces:**
- Consumes:
  - `SessionBackend.Move` and `Create` (Task 8)
  - `newConfirm`, `bar` and `level` (phase 3)
  - `holdwire.MoveThinking` (Task 1)
  - `client.Refusal`, with `Reason` `confirm`
  - `sessionsTab.over` and `sessionChangedMsg` (Task 10)
- Produces `newMovePicker` and `offerMoves` with Task 10's signatures.
- **The picker** lists every account of the row's flavour except the row's own. Each row is `#N label`, the 5h and 7d bars, and its state. A dead account is Faint, reads `needs a login`, and cannot be chosen. A row that is not claude toasts `only Claude Code sessions move in this version` instead of opening.
- **For a session:**
  1. `Move(id, N, false, e.width, e.height)`.
  2. A `*client.Refusal` with `Reason` `confirm` opens `newConfirm(e, "Move <name>?", r.Message, yes)`, where yes is `Move(id, N, true, …)`. The words are the server's, and the TUI adds none (R6).
  3. Success toasts `moved <name> to #N <label>` and sends `sessionChangedMsg{Attach: false}`. The preview's stream stays, and it is repainted.
- **For a conversation:** under the list, for any account other than the conversation's own, `rota cannot tell whether #N is in the same organization: ` followed by `holdwire.MoveThinking`. Then `enter` sends `Create(NewSession{Account: N, Resume: c.ID, Cwd: c.Dir, …})` and `sessionChangedMsg{Attach: true}` (R12).
- **A row gone** from the next poll: the picker or confirm reads `session <name> is gone (forgotten, or moved elsewhere)`, and `enter` closes it (Review Focus 5).
- **`offerMoves`** (R31): for each account whose deciding window goes from below its threshold to at or over it, while a live held session runs on it:
  - one toast, *#12 work@… is over its 80%: m moves <name> to another account*
  - `over[12] = true`

  When the account falls back below, `over` is cleared and the offer re-arms. The first listing is a baseline.

- [ ] **Step 1: Write the failing tests.**
  - `TestThePickerOffersOnlyAccountsOfTheSameKind`.
  - `TestAMoveThatCostsSomethingAsksWithTheServersWords`: the confirm's body equals the refusal's message. `y` gives `Move(id,13,confirm)`.
  - `TestAMoveThatCostsNothingGoesAtOnce`.
  - `TestAConversationMoveSaysTheThinkingCostItself`.
  - `TestThePickerSaysWhenItsSessionIsGone`.
  - `TestAnAccountOverItsLimitOffersMOnce`: listings at 70, 85, 90, 70 and 85 against 80 give a toast on the second and the fifth only.
  - `TestTheMovePickerAt80x24`, a golden.
  - `TestMoveUpdateDoesNoIO`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota/tui/sessions_move.go cmd/rota/tui/sessions_move_test.go cmd/rota/tui/testdata/golden/move-80x24.txt && git -C <worktree> commit -m "m in the Sessions tab moves a chat to another account after saying what it costs, and an account over its limit offers it"`

### Task 15: The Services tab

**Files:**
- Create:
  - `cmd/rota/tui/services.go`, `cmd/rota/tui/services_view.go`, `cmd/rota/tui/services_test.go`
  - goldens `services-60x20.txt`, `services-80x24.txt` and `services-120x40.txt`
  - the stub `cmd/rota/tui/invite_qr.go`, `func qrRows(text string) []string { return nil }`, which Task 18 replaces

**Interfaces:**
- Consumes Task 8's `ServiceBackend`, its types, `shownMsg` and `sessionsMsg`, and phase 3's `handOff`, `execTail`, `copyText`, `toast`, `newConfirm`, `clean`, `fit` and `wrap`.
- Produces:
  ```go
  type servicesTab struct
  func newServices(e *env) tab // place 40; registered by Task 19
  type servicesKeys struct { Up, Down, Daemon, Unit, Edit, Invite, Copy, Test key.Binding }
  func servicesTabKeys() servicesKeys // ↑/k, ↓/j (sinks), s, i, e, v, c, t
  type serviceMsg struct { Service Service; Err error }
  type actedMsg struct { Act, Said string; Err error }
  type invitedMsg struct { Invite Invite; Err error }
  type testedMsg struct { Sink string; Outcomes []SinkOutcome; Err error }
  ```
- **Polling.** `Service` is read every 5 s while shown and every 30 s otherwise. The viewers come from `sessionsMsg`: for each live session, its viewers and who has the keyboard.
- **The sections, top to bottom:**
  - **Daemon.** Running: `● running · pid 4123 · rota 1.4.0-beta.11 · since 21:20 (2h 3m)`. Otherwise: `○ not running · s starts it`. Then `store`, `control socket`, `log` and `config`. Then `unit <path> (installed)` or `not installed · i installs it`. Then `sessions N running, M stopped`.
  - **Addresses.** Either `web <url>` or `web: none ([server] listen in server.toml opens a port)`. Either `ssh <addr> · <SHA256 fingerprint>` or `ssh: off ([ssh] enabled in server.toml turns it on)`.
  - **Viewers.** `<session>: <name> (<role>)[, has the keyboard]`, one line per viewer.
  - **Sinks.** `<name>  <type>  <state>  sent N · last 21:01`, and `last error <text> (21:05)` when there is one. The selected sink is shown in reverse.
  - **Audit**, local only: `HH:MM <text>`, each line cleaned.
- **Over SSH** (`!Service.Local`), `s`, `i` and `e` are hidden, and the Daemon section ends `on <host>: rota serve start|stop|install|uninstall; $EDITOR <config>` (R19). The Audit section reads `the audit log is on <host> at <log>`.
- **Keys** (all gated):
  - `s` starts a stopped daemon at once. On a running one it asks first: *Stop the rota daemon? Its sessions go on running; the next start takes them up.*
  - `i` installs, or uninstalls after *Remove the rota service unit? The daemon stops; its sessions go on.*
  - `e` hands the terminal over through `handOff(execTail(Edit()))`, then toasts R24's line.
  - `v` asks for a 10-minute invite and shows its link wrapped, with `expires in 9m`, `c copies it`, and `qrRows`' rows when there are any. It is hidden with a reason when `HTTP` is empty.
  - `t` tests the selected sink.
- **Answers.** An action's answer is a toast of its last line. A failure is a toast at `levelErr`. `Busy()` is `rota serve <act> is running` while one runs.

- [ ] **Step 1: Write the failing tests**, with a `fakeServices`.
  - `TestTheServicesAtThreeSizes`: the goldens, with a running daemon, two sinks (one failing), three audit lines and a viewer.
  - `TestLocalShowsActionsAndSSHSaysWhereToRunThem`.
  - `TestTheDaemonStopsOnlyAfterAConfirm`: `Act("stop")` only after `y`. A stopped daemon gets `Act("start")` with no confirm.
  - `TestInstallAndUninstall`.
  - `TestEditHandsTheTerminalToTheEditor`.
  - `TestAnInviteIsShownAndCopied`.
  - `TestNoInviteWithoutAWebPage`.
  - `TestSinksAreSelectedAndTested`.
  - `TestViewersComeFromTheSessions`.
  - `TestTheAuditTailIsCleaned`: a line holding `\x1b[2J` and `sk-ant-api03-…` shows neither.
  - `TestWatchSeesNoActions`.
  - `TestServicesUpdateDoesNoIO`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota/tui && git -C <worktree> commit -m "The Services tab shows the daemon, its addresses, viewers, sinks and audit tail, and starts, stops, installs, edits, invites and tests from one place"`

### Task 16: `ServiceBackend` from the store's files and `rota serve`

**Files:**
- Create: `cmd/rota/tui_services.go`, `cmd/rota/tui_services_test.go`, `cmd/rota/tui_services_unix_test.go`

**Interfaces:**
- Consumes:
  - Task 8's types
  - `holdwire.RunningDaemon`, `Scan` and `Daemon.SSH`/`SSHHostKey` (phase 2 Task 12)
  - `daemonLog`, `serverConfig`, `unitFile`, `oneLine` and `selfCmd` (phase 3 Task 12)
  - `notify.ReadStatus`, `Describe` and `Test` (phase 5 Task 9)
  - `api.Config.Events.Link` (phase 5 Task 3)
  - `client.Client.HTTP`
- Produces:
  ```go
  type tuiServicesOptions struct { Home, Terminals, Host string; Local bool; Dial func() (*client.Client, error); Run func(args ...string) *exec.Cmd; Env []string }
  func newTUIServices(o tuiServicesOptions) tui.ServiceBackend
  func serviceState(o tuiServicesOptions) tui.Service
  func auditTail(path string, n int) []tui.AuditLine                       // R20
  func editorCmd(path string, env []string) *exec.Cmd                       // R24
  func inviteBase(cfg *api.Config, d *holdwire.Daemon, host string) (string, bool) // R22
  ```
- **`serviceState`:**
  - From `daemon.pid`: `Running`, `PID`, `Version`, `Started`, `HTTP`, `SSH` and `SSHKey`.
  - From the records: the counts, where `Locked` is running.
  - `Log` (`daemonLog`), `Config` (`<home>/server.toml`) and `Unit`, with `Installed` from `os.Stat`.
  - `Sinks`: from `notify.ReadStatus` when its pid is the running daemon's, else from `notify.Describe(cfg.Events)`.
  - `Audit`, only when `Local`.
  - A `server.toml` that does not parse sets `Err` to its error, cleaned, and every other field is still filled.
- **`Act`:**
  - Only when `Local`, otherwise `on <host>: rota serve <act>`.
  - It runs `Run("serve", act)` with no stdin and returns the last non-empty line of its combined output, through `oneLine`.
  - A non-zero exit is an error carrying that line.
- **`Invite`:**
  - `inviteBase`, otherwise the error *the rota daemon serves no web page; [server] listen in server.toml gives it one*.
  - Then `POST http://rota/v1/invites {"ttl":"10m"}` through the daemon's client.
  - The URL is `base + "/invite/" + <the code after /invite/ in the answer>`.
- **`TestSink`** is `notify.Test(c, terminals, sink, 15*time.Second)`, mapped to `SinkOutcome`.

- [ ] **Step 1: Write the failing tests.**
  - `TestServiceStateReadsTheDaemonsRecord`: a store whose daemon claim this test holds (`holdwire.ClaimDaemon`, then `RecordHTTP`). The state is `Running` with this pid and the HTTP address.
  - `TestNoDaemonStillSaysWhereEverythingIs`.
  - `TestABrokenServerTomlIsSaidAndTheRestShown`.
  - `TestTheAuditTailReadsOnlyTheEnd`: a 5 MiB log with an audit line at its start and 30 at its end gives the last 20. The first line is not among them.
  - `TestSinksComeFromTheStatusFileOrTheConfig`.
  - `TestActRunsServeWithNoStdin`: `Run` is a fakecli fake that records its argv and stdin. The argv is `serve stop`, stdin is empty, and the last line comes back.
  - `TestOverSSHThereIsNoActEditOrAudit`.
  - `TestTheEditorComesFromVisualThenEditor`: `VISUAL="code -w"` gives `[code -w <path>]`. Neither set gives `vi`.
  - `TestInviteLinksTheAddressAPhoneReaches`:
    - `[events] link` wins.
    - `0.0.0.0:8787` becomes `<host>:8787`.
    - `[tls]` makes it `https`.
    - No HTTP gives no invite.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota && git -C <worktree> commit -m "The TUI reads the daemon's state, sinks and audit tail from the store, and runs rota serve for it"`

### Task 17: Both TUIs reach sessions and services

**Files:**
- Create: `cmd/rota/tui_sessions.go`, `cmd/rota/tui_sessions_test.go`, `cmd/rota/tui_sessions_unix_test.go`
- Modify:
  - `cmd/rota/tui_entry.go`: `openTUI` sets `Options.Sessions` and `Services`
  - `cmd/rota/tui_ssh.go`: `sshTUI` does the same

**Interfaces:**
- Consumes:
  - `tui.NewSessions` and `SessionsConfig` (Task 9)
  - `newTUIServices` (Task 16)
  - `attach.Command`, `Options`, `DefaultKey`, `TaughtFile` and `Result.Line` (phase 2 Task 10)
  - `client.Dial`, `DialNow` and `ErrNoDaemon`
  - `ensureDaemon` and `(*cli).spawnDaemon(true, "")`
  - `absoluteDirs(store.HostEnv())`
  - `sshd.TUISession`
- Produces:
  ```go
  func (c *cli) localTUIBackends(home, terminals string, api *http.Client) (tui.SessionBackend, tui.ServiceBackend)
  func sshTUIBackends(s sshd.TUISession, home, terminals, host string) (tui.SessionBackend, tui.ServiceBackend)
  func tuiAttach(terminals string, s *sshd.TUISession) func(c *client.Client, id string, watch bool) tui.Attachment
  type tuiAttached struct { cmd *attach.Cmd; o attach.Options } // tea.ExecCommand through cmd; Said() is cmd.Result().Line(o)
  ```
- **Locally:**
  - `Dial(true)` is `ensureDaemon` with a control-socket-only start, and `Dial(false)` is `client.DialNow`.
  - `Env` is `absoluteDirs(store.HostEnv())`. On error it is nil, and the daemon's environment applies.
  - `Defaults{Cwd: os.Getwd(), Host, Store: home, OwnEnv: true, RemoteControl: true}`.
  - The attach takes `Options{Key: attach.DefaultKey, Watch, Settle: true, Teach: attach.TaughtFile(terminals), Again: func(string) string { return "enter in the Sessions tab" }, Redial: func() (*client.Client, error) { return client.Dial(terminals) }}`.
- **Over SSH:**
  - `Dial` always gives `s.Client`, and `Env` is nil.
  - `Defaults{Cwd: <the daemon user's home>, OwnEnv: false, RemoteControl: false}`.
  - The attach has no `Redial`, and gets `SetWindow` from the channel's size and `Resize`, as phase 3 Task 20 does.
  - Services is `Local: false`.

- [ ] **Step 1: Write the failing tests.**
  - `TestTheLocalTUIGetsSessionsAndServices`: with `startTUI` recorded, both fields are non-nil, `Defaults().OwnEnv` is true and `Service.Local` is true.
  - `TestSSHGetsThemWithoutEnvOrRemoteControl`.
  - `TestLocalSessionsStartTheDaemonOnce` (unix, process-level: `ROTA_AS_RUN=1`, `shortHome`, the tty fake as claude):
    - No daemon is running.
    - Two `Create`s start one daemon.
    - `daemon.pid` has no `http`, so the daemon is on its control socket only.
    - Both sessions are listed.
  - `TestAttachFromSessionsComesBackToTheList` (unix, `internal/pty`): `Attach(id, false)` is run on a pty and fed `x\r`, then `Ctrl-\ d`. `Run` returns nil, `Said()` starts `rota: detached from`, and the session still runs.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** **race** and **docker**. The local CI run passes.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota && git -C <worktree> commit -m "The local TUI and the SSH TUI both reach held sessions and the daemon's services, and locally a daemon is started only when a session needs one"`

### Task 18: An invite as a QR code (waits on O2)

Nothing here starts until the owner answers O2. The steps below are for options 1 to 3. Under 4 this task is dropped. Under 5 the encoder is an `exec` of `qrencode -t UTF8`, with its output read as rows.

**Files:**
- Modify: `cmd/rota/tui/invite_qr.go`, replacing Task 15's stub
- Modify, under options 1 and 2: `cmd/rota/go.mod` and `go.sum`, through `GOPRIVATE='github.com/professor93/*' sh scripts/module.sh tidy`, and the dependency sentence in `README.md` and `docs/reference.md`
- Create: `cmd/rota/tui/invite_qr_test.go`, and the golden `qr-hello.txt`

**Interfaces:**
- Produces `func qrRows(text string) []string`. Two modules go to a cell, with `▀`, `▄`, `█` and space, and a quiet zone of 2 modules. The rows are empty when the text does not fit version 10.

- [ ] **Step 1: Write the failing tests.**
  - `TestTheCodeHasItsQuietZoneAndSize`: for `https://box:8787/invite/abc`, every row is the symbol's size plus 4 cells wide, and there are (size+4+1)/2 rows.
  - `TestTheCodeIsStable`: the golden for `HELLO`.
  - `TestTooLongIsNoCode`.
- [ ] **Step 2: Run them.** They fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify.** The local CI run passes. `sh scripts/module.sh check` says `module: ok`, and the root `go.mod` is unchanged.
- [ ] **Step 5: Commit.** `git -C <worktree> add cmd/rota README.md docs/reference.md && git -C <worktree> commit -m "An invite from the Services tab is shown as a QR code beside its link"`

### Task 19: The tabs registered, the manual and the notes

**Files:**
- Create:
  - `cmd/rota/tui/phase4_tabs.go`: `func init() { register(20, newSessions); register(40, newServices) }`
  - `cmd/rota/tui/store_golden_p4_test.go`
  - goldens `store-sessions-60x20.txt`, `store-sessions-80x24.txt`, `store-sessions-120x40.txt` and `store-services-80x24.txt`
- Modify: `docs/reference.md` and `docs/releases/1.4.0.md`

**Interfaces:**
- Consumes everything above, and phase 3 Task 21's `TestReferenceDocumentsEveryKey` and store-golden pattern.

- [ ] **Step 1: Register the tabs.** The header now reads `Accounts  Sessions  Ask  Services`, and `2` and `4` reach the new tabs.
- [ ] **Step 2: The store goldens.** The full `Model` is built as phase 3 Task 21 builds it, with `Sessions` and `Services` set to Task 8's fakes holding Task 10's and Task 15's fixtures. The goldens are Sessions at the three sizes and Services at 80×24.
- [ ] **Step 3: The manual**, in its own voice, from these facts:
  - **The TUI, Sessions:**
    - what is listed, and in what order
    - the preview, and that it never resizes
    - every key, from the bindings
    - that `n` and `r` start a daemon when none runs, and say so
    - New's fields and notes
    - what `m` costs, quoting `MoveThinking`
    - an account over its limit offering `m`
    - that `s` and `x` never touch a worktree, and that `c` copies its path
  - **The TUI, Services:** every key, the sections, what SSH shows instead, how an invite's link is built, and the audit tail.
  - ***A terminal on the server***:
    - the `PATCH` bodies, answers and reasons, from *The HTTP contract*
    - the `confirm` round trip
    - the `moved` frame
    - `resume` on `POST`
    - `resumable` as R5
  - **A paragraph, *Moving a chat*:**
    - within one flavour, through `CopyForResume`
    - claude only, and refused across providers by name
    - the folder rule (R4)
    - the arguments carried over (R3), with Remote Control decided again for the target account (sec-rc's rule)
    - a live session keeping its id, viewers and size
    - the costs, and what happens when a move fails half-way
  - **Layout rows** for every new file in `api/`, `internal/holdwire/`, `cmd/rota/tui/` and `cmd/rota/`.
- [ ] **Step 4: The notes.** One bullet each under `### New in the next beta`:
  - the Sessions tab
  - the Services tab
  - Resume of a stopped session under its own id
  - moving a chat, with its costs and the confirm
  - `PATCH /v1/terminals/{id}`, `POST … resume`, forgetting, and `resumable`

  Each is as long as one of beta.10's.
- [ ] **Step 5: Verify.**
  - The local CI run passes, with `TestReferenceDocumentsEveryKey`, `TestReferenceDocumentsEveryRoute` and `docs_test` included. **race**.
  - `git diff b9fd476 -- docs/releases/` touches only the next-beta section.
  - `git log --format=%B origin/main.. | grep -ciE 'co-authored|claude\.ai|anthropic|generated'` prints `0`.
- [ ] **Step 6: Commit and push.**
  ```bash
  git -C <worktree> add cmd/rota/tui docs/reference.md docs/releases/1.4.0.md
  git -C <worktree> commit -m "The TUI has its Sessions and Services tabs, and the manual and the notes describe them, resuming and moving key by key"
  git -C <worktree> push -u origin p4-t19
  ```

## Spec coverage

| Spec | Where |
|---|---|
| §9 Sessions: list at left, live preview at right, `⏎ w n r m s /`, resumable conversations below | Tasks 10–14 (`x` and `c` from §2 and §13) |
| §9 Services: status, start, stop, install, addresses, viewers, audit tail, invites, sinks with a test button, `$EDITOR`, rota never writes `server.toml` | Tasks 15, 16 and 18 |
| §9 rules: `Backend`, routes before paths, `key.Binding`, theme, ladders, 60×14, goldens | Global Constraints; Tasks 8, 10, 15 and 19 |
| §10 Move: stop, then resume under the target in a new holder from the registry's folder; keeps id, name, arguments and viewers; mid-turn warns; flavour and `CopyForResume`; claude only; across providers refused by name | Tasks 1, 4, 6, 7 and 14 |
| §2 Resume after a reboot, from the lock and the record; stopped kept 168h and `x` forgets sooner; no known conversation means disabled with the reason | Tasks 1, 2, 4 and 12 |
| §4 explicit size; sessions separate; the watch role never types | R7, R13; Tasks 10–12 |
| §5 an exclusive account greyed with its reason, and the rotation stepping on | R17; Task 13 |
| §6 the TUI starts the daemon only when a session is first needed, says so, and an auto-started daemon serves `ctl.sock` alone | Tasks 9, 12 and 17 |
| §11 `account.threshold` on a live session offers `m` | R31; Task 14 |
| §13 Resume at the requester's size; Stop and `x` never touch a worktree, which is shown and copied; account and credential kind on every session; invites as QR | R7, R29; Tasks 10, 12, 15 and 18 |
| The coordinator's Remote Control ruling: Resume and Move go through the same launch path, which decides disabling afresh, and never replay saved arguments | Global Constraints; R3; Tasks 1, 4 and 6 |
| Phase 6 Task 11's contract | *What phase 4 takes … and what it gives* |
