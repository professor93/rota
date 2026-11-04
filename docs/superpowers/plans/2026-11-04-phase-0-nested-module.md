# Phase 0: nested cmd/rota module Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `cmd/rota` becomes the module `github.com/professor93/rota/cmd/rota`, so nothing the program later needs can reach a consumer of the SDK, with no behaviour change, released as `v1.4.0-beta.2` and `cmd/rota/v1.4.0-beta.2`.

**Architecture:** The root module keeps every package and requires nothing. `cmd/rota/go.mod` requires the root at a release tag and never replaces it. A committed `go.work` keeps a checkout building the program against the packages beside it. One POSIX script, `scripts/module.sh`, checks the boundary (in CI and in the release gate), tidies the program's module through a temporary modfile, and pins it to a root tag in release order. The release workflow learns the second tag, and the public suite runs the binary the gate built, named by `ROTA_BIN`.

**Tech Stack:** Go 1.27 modules and workspaces, POSIX sh, GitHub Actions, gh.

**Spec:** `docs/superpowers/specs/2026-11-01-tui-daemon-ssh-design.md`. Read section 1 "The SDK boundary", "Delivery, in order" (phase 0 and the release paragraph), "Public surface" and "The owner's answers".

## Global Constraints

- Root module `github.com/professor93/rota`, `go 1.27`: no `require`, no `replace`, no `go.sum`, ever.
- Program module `github.com/professor93/rota/cmd/rota`, `go 1.27`: requires `github.com/professor93/rota` at a release tag (`v1.4.0-beta.1` until the release pins `v1.4.0-beta.2`). Never a `replace` and never a pseudo-version.
- `go.work` is committed: `go 1.27`, `use ( . ./cmd/rota )`, no `replace`.
- Release tag regex: `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`. The program's tag is `cmd/rota/` followed by that tag.
- Release order: tag `vX` on the version commit and push it. Then the pin commit (`cmd/rota/go.mod` requires `vX`, with its `go.sum`). Then tag `cmd/rota/vX` and push it. Only after that, fast-forward `main`. Tags are lightweight, like `v1.4.0-beta.1`. A number is never reused.
- No behaviour change: the only Go files this phase changes are `lib/httpx.go:21` and `wire/wire.go:424`, and the only change is `Version` going to `"1.4.0-beta.2"`.
- `lib` stays standard-library only and reads no environment.
- Branches: implement on `phase-0`, cut from `plan-phase-0`. The suite change goes on `rota-bin` in `~/GolandProjects/rota-sdk-test`. No PR. `main` moves in either repository only by fast-forward, and only on the owner's word.
- Commit subjects: one plain sentence of what is now true. The version commit is just `1.4.0-beta.2`. No body, no trailer, no attribution, nothing about AI.
- `<testenv>` means `env -u ROTA_CLAUDE_HOME ROTA_HOME="$(mktemp -d)" CLAUDE_CONFIG_DIR="$(mktemp -d)"`, and it prefixes every `go test` in this plan. Nothing here starts a real interactive claude.
- **The local CI run** is these commands, from the repository root, in this order: `test -z "$(gofmt -l .)"`; `for os in linux darwin windows; do GOOS=$os go vet ./... && GOOS=$os go -C cmd/rota vet ./... || echo "vet $os FAILED"; done`; `<testenv> go test ./...`; `<testenv> go -C cmd/rota test ./...`; `sh scripts/module.sh check`. It passes when gofmt and vet print nothing, every test line is `ok`, one of them is `ok  	github.com/professor93/rota/cmd/rota`, and the last line is `module: ok`.
- In `ci.yml` every `run:` is a single command, because pwsh on Windows reports only the last command's exit.
- Scripts are POSIX `sh` for Linux and macOS. go.mod files change only through `go mod edit` and `go mod tidy`, never `sed -i`. Temporary files live in a `mktemp -d` directory that a trap removes.
- Linux comes first, then macOS. Windows CI stays green.

## Review Focus

1. **A checkout with no network and no module cache** (an offline laptop, a proxy outage, a cold CI cache) must still build the program from the workspace without fetching the pinned root. Test: rule 5 of `module.sh check` in Task 1, run with `GOPROXY=off` and an empty `GOMODCACHE`.
2. **A tidy that fails partway** must leave `cmd/rota/go.mod` and `go.sum` byte-identical and must never write its temporary `replace` back. Test: Task 2 Step 4, with a probe import that cannot resolve.
3. **Tags out of order or a reused number**: pinning before the root tag is on origin, pinning on a commit other than the one the tag names, or pinning a version whose `cmd/rota/` tag already exists. Test: Task 2 Step 6, which checks all three refusals.
4. **Old versions and hand-run checks**: `go install …/cmd/rota@v1.3.0` must still give the root's 1.3.0. The module job must accept `v1.3.0` from a dispatch and `cmd/rota/v1.4.0-beta.2` from a push. Test: Task 4 Step 4.
5. **A Windows step that runs two commands** passes even when the first one fails. Test: Task 1 Step 8, where `grep -c 'run: |' .github/workflows/ci.yml` must print `0`.

## File Structure

| Path | Responsibility |
|---|---|
| `go.work` (create) | Puts both modules in one workspace for a checkout; `go install @version` ignores it |
| `cmd/rota/go.mod` (create) | The program's module and its one requirement: the root at a tag |
| `cmd/rota/go.sum` (written by `pin` at release) | The root's two hash lines |
| `scripts/module.sh` (create) | `check` the boundary, `tidy` the program's module, `pin` it to a root tag |
| `.github/workflows/ci.yml` | Vet and test both modules, then run `check` |
| `.github/workflows/release.yml` | Second tag pattern; the gate tests the program and hands the suite its binary; the module job runs on `cmd/rota/v*` |
| `README.md`, `docs/reference.md` | Dependency wording that stays true, the program as a module of its own, `@latest` during the betas, Layout rows |
| `docs/releases/1.4.0.md` | For beta.2: the module split, the panic fixes on main since beta.1, the binary size |
| `lib/httpx.go`, `wire/wire.go` | `Version = "1.4.0-beta.2"` |
| suite: `serve/helpers_test.go`, `Makefile`, `.github/workflows/test.yml` | Honour `ROTA_BIN`; `make local` and the suite's CI name a binary |

---

### Task 1: The program is a module of its own

**Files:**
- Create: `scripts/module.sh` (the `check` subcommand), `cmd/rota/go.mod`, `go.work`
- Modify: `.github/workflows/ci.yml`, `README.md:11`, `docs/reference.md:27-31,42-43,103` and its Layout table
- Test: `sh scripts/module.sh check`

**Interfaces:**
- Produces: `sh scripts/module.sh check`. It works from any directory, because it `cd`s to `$(dirname "$0")/..`. On success it prints `module: ok` and exits 0. On failure it prints one stderr line `module: <the rule broken>` and exits 1. With no subcommand or an unknown one it prints `usage: sh scripts/module.sh check | tidy | pin vX.Y.Z[-pre]` to stderr and exits 2. It defines two shell functions that Task 2 uses: `pinned`, which prints the root version that `cmd/rota/go.mod` requires (read through `go mod edit -print`), and `version`, which prints `wire.Version` (read from `wire/wire.go`).

- [ ] **Step 1: Write the check.** `check` tests these rules in order and reports the first one that fails:
  1. `go mod edit -json go.mod` has no `"Require"` key and no `"Replace"` key (the command leaves out empty lists). Failure: `module: go.mod requires or replaces a module; the root requires nothing`.
  2. In `go mod edit -json cmd/rota/go.mod`, `Module.Path` is `github.com/professor93/rota/cmd/rota`, there is no `"Replace"`, and `pinned` matches the release regex. Failure: `module: cmd/rota/go.mod …`, saying which of the three is wrong.
  3. `go work edit -json go.work` has no `"Replace"`. Failure: `module: go.work replaces a module`.
  4. A throwaway consumer in a `mktemp -d` directory. Its `main.go` blank-imports every package that `GOWORK=off go list -e ./...` prints at the root, except those under `/internal/`. Set it up with `go mod init example.com/consumer` and `go mod edit -require=github.com/professor93/rota@v0.0.0 -replace=github.com/professor93/rota=<root>`. Then run `go mod tidy` and `go build -o /dev/null .`, both with `GOWORK=off GOPROXY=off GOFLAGS= GOTOOLCHAIN=local`. Both must succeed, and `go.sum` must be absent or empty. Failure: `module: a program importing the root's packages needs other modules`, followed by go's output.
  5. From the root, run `GOWORK=<root>/go.work GOPROXY=off GOMODCACHE=<empty tmp> GOFLAGS="-modcacherw -buildvcs=false" go build -o <tmp>/rota ./cmd/rota`, then `<tmp>/rota --version`, which must print exactly `rota $(version)`. Failure: `module: the workspace does not build the program offline`.

- [ ] **Step 2: Run it before the split.** `sh scripts/module.sh check; echo "exit=$?"` prints a `module: cmd/rota/go.mod …` line and then `exit=1`.

- [ ] **Step 3: Split.** `cmd/rota/go.mod`:
  ```
  module github.com/professor93/rota/cmd/rota

  go 1.27

  require github.com/professor93/rota v1.4.0-beta.1
  ```
  `go.work`:
  ```
  go 1.27

  use (
  	.
  	./cmd/rota
  )
  ```
  Do not create `cmd/rota/go.sum` yet; under `go.work` nothing reads it.

- [ ] **Step 4: Run it, then break each rule once.** `sh scripts/module.sh check` prints `module: ok`. Back up both files with `mkdir -p /tmp/p0 && cp go.mod /tmp/p0/root.mod && cp cmd/rota/go.mod /tmp/p0/app.mod`, then make each change below, run `check`, and restore with the reverse `cp` before the next one (never `git stash`):
  - `go mod edit -require=golang.org/x/crypto@v0.58.0 go.mod`: exits 1 with the rule 1 line.
  - `go mod edit -replace=github.com/professor93/rota=../.. cmd/rota/go.mod`: exits 1 with rule 2, naming the replace.
  - `go mod edit -require=github.com/professor93/rota@v0.0.0-20261104000000-000000000000 cmd/rota/go.mod`: exits 1 with rule 2, saying it is not a release tag.
  - `printf 'package lib\n\nimport _ "golang.org/x/crypto/ssh"\n' > lib/zz_probe.go`: exits 1 with rule 4. Then `rm lib/zz_probe.go`.

  After restoring, `git status --porcelain` lists only `?? cmd/rota/go.mod`, `?? go.work` and `?? scripts/module.sh`. On Linux, `docker run --rm -v "$PWD":/src -w /src golang:1.27 sh scripts/module.sh check` prints `module: ok`.

- [ ] **Step 5: Show what CI would miss.** `<testenv> go test ./... 2>&1 | grep -c 'rota/cmd/rota'` prints `0`, because the root's `./...` leaves the program out.

- [ ] **Step 6: CI.** In the `test` job of `ci.yml`, add a step `vet the program` (`run: go -C cmd/rota vet ./...`) after `vet`, and after `test` add `test the program` (`run: go -C cmd/rota test ./...`) followed by `module boundary` (`run: sh scripts/module.sh check`). In the `windows` job, add `build the program`, `vet the program` and `test the program` after `build`, `vet` and `test` respectively, each running `go -C cmd/rota build ./...`, `vet ./...` or `test ./...`. Put one comment above the first new step: `# The program is a module of its own, which ./... at the root leaves out.`

- [ ] **Step 7: Docs.** Make these exact replacements:
  - `README.md:11`: `and the whole project has **zero third-party dependencies**.` becomes `and the SDK and the server packages have **zero third-party dependencies**.`
  - `docs/reference.md:27`: `` | the command | `./cmd/rota` | all of the above | `` becomes `` | the command, a module of its own | `./cmd/rota` | all of the above | ``. The paragraph that ends `the one line an outside program needs.` gains a final sentence: `` The command is the module `github.com/professor93/rota/cmd/rota`, which requires the other at a released version. ``
  - `docs/reference.md:42-43`, the text across the line break: ``**The whole project has no third-party dependencies.** There is no `go.sum`, nothing to audit, and nothing to update.`` becomes ``**The SDK and the server packages have no third-party dependencies.** Their module, `github.com/professor93/rota`, has no `go.sum`, nothing to audit, and nothing to update. The `rota` program is a module of its own, so whatever it comes to need never reaches a program that imports them.``
  - `docs/reference.md:103`: `` `encoding/json/v2`, and nothing else outside the standard library.`` becomes `` `encoding/json/v2`.``
  - In the Layout table, before the `cmd/rota/main.go` row, add `` | `go.work`, `cmd/rota/go.mod` | The command as a module of its own, and the workspace that builds it against the packages beside it | `` and `` | `scripts/module.sh` | Checks the line between the two modules | ``.

- [ ] **Step 8: Verify.**
  - The local CI run passes.
  - `<testenv> go -C cmd/rota test -count=1 -run TestReferenceDocuments -v .` prints `--- PASS: TestReferenceDocumentsEveryCommand` and `--- PASS: TestReferenceDocumentsEveryFlag`. No root test reads `../cmd/rota`.
  - `go run ./cmd/rota --version` prints `rota 1.4.0-beta.1`.
  - `sh scripts/release.sh` writes six binaries and `checksums.txt` to `dist/`.
  - `grep -c 'run: |' .github/workflows/ci.yml` prints `0`.
  - `ruby -ryaml -e 'YAML.load_file(ARGV[0]); puts "yaml ok"' .github/workflows/ci.yml` prints `yaml ok`.

- [ ] **Step 9: Commit.**
  ```bash
  git add go.work cmd/rota/go.mod scripts/module.sh .github/workflows/ci.yml README.md docs/reference.md
  git commit -m "The rota program is a module of its own beside the root, which still requires nothing, and CI vets and tests both and checks the line between them"
  ```

### Task 2: Tidy and pin

**Files:**
- Modify: `scripts/module.sh` (add `tidy` and `pin`), `docs/reference.md` (the `scripts/module.sh` Layout row)
- Test: the commands below

**Interfaces:**
- Consumes: `pinned`, `version` and `check` from Task 1.
- Produces:
  - `sh scripts/module.sh tidy`.
  - `sh scripts/module.sh pin <tag>`, where `<tag>` is `vX.Y.Z[-pre]`. On success it makes a commit whose subject is `cmd/rota requires rota <tag>` and which touches only `cmd/rota/go.mod` and `cmd/rota/go.sum`, puts a lightweight local tag `cmd/rota/<tag>` on it, and prints `module: tagged cmd/rota/<tag>; push it with: git push origin <branch> cmd/rota/<tag>`. It pushes nothing.

- [ ] **Step 1: Watch plain tidy fail.** `GOWORK=off go -C cmd/rota mod tidy` fails with `ambiguous import: found package github.com/professor93/rota/cmd/rota in multiple modules`, because root `v1.4.0-beta.1` still contains the program. `sh scripts/module.sh tidy` prints the usage and exits 2.

- [ ] **Step 2: Implement `tidy`.**
  - Copy `cmd/rota/go.mod`, and `go.sum` if it exists, into a `mktemp -d` directory.
  - Run `go mod edit -replace=github.com/professor93/rota=<absolute root> <tmp>/go.mod`.
  - In `cmd/rota`, run `GOWORK=off GOFLAGS= go mod tidy -modfile=<tmp>/go.mod`.
  - Run `go mod edit -dropreplace=github.com/professor93/rota -require=github.com/professor93/rota@$(pinned) <tmp>/go.mod`.
  - The new go.sum is the temporary go.sum plus the original's lines starting `github.com/professor93/rota `, put through `LC_ALL=C sort -u`.
  - Copy each file back only if it differs (`cmp -s`), and write go.sum only if it is non-empty.
  - Any failure before the copy-back exits 1 with `module: tidy failed` and go's output, and writes nothing.

- [ ] **Step 3: Run it.** `GOPROXY=off sh scripts/module.sh tidy` exits 0, and `git status --porcelain cmd/rota` prints nothing. On Linux, `docker run --rm -v "$PWD":/src -w /src golang:1.27 sh -c 'cp cmd/rota/go.mod /tmp/m && GOPROXY=off sh scripts/module.sh tidy && cmp /tmp/m cmd/rota/go.mod && test ! -e cmd/rota/go.sum && echo linux ok'` prints `linux ok`.

- [ ] **Step 4: A failed tidy writes nothing (Review Focus 2).** `printf 'package main\n\nimport _ "example.invalid/probe"\n' > cmd/rota/zz_probe.go; GOPROXY=off sh scripts/module.sh tidy; echo "exit=$?"` prints `module: tidy failed`, mentions `example.invalid/probe`, and ends with `exit=1`. `git status --porcelain cmd/rota` then shows only `?? cmd/rota/zz_probe.go`. Finish with `rm cmd/rota/zz_probe.go`.

- [ ] **Step 5: Implement `pin <tag>`.** It refuses in this order, exiting 1 unless noted:
  1. The argument does not match the release regex: usage, exit 2.
  2. `cmd/rota/<tag>` exists locally, or on origin (`git ls-remote --exit-code --tags origin refs/tags/cmd/rota/<tag>`): `module: cmd/rota/<tag> exists already; a version is never reused`.
  3. `<tag>` is not on origin: `module: <tag> is not a tag on origin; push the root's tag first`.
  4. `HEAD` is not `git rev-parse "<tag>^{commit}"`: `module: HEAD is not the commit tagged <tag>`.
  5. `git status --porcelain` is not empty: `module: the tree has changes`.
  6. `go run ./cmd/rota --version` is not `rota <tag without the v>`: `module: the program says …, the tag says <tag>`.

  If none of these apply, it runs `go mod edit -require=github.com/professor93/rota@<tag> cmd/rota/go.mod`, and then, in `cmd/rota`, `GOWORK=off go mod tidy`, `GOWORK=off GOFLAGS=-mod=readonly go build -o /dev/null .` and `GOWORK=off go vet ./...`. If any of those fail, it restores both files from a backup and exits 1 with `module: the program does not build against <tag>`. Then it runs `check`, `git add cmd/rota/go.mod cmd/rota/go.sum`, `git commit -m "cmd/rota requires rota <tag>"` and `git tag cmd/rota/<tag>`.

- [ ] **Step 6: Refusals (Review Focus 3).**
  - `sh scripts/module.sh pin 1.4.0-beta.2; echo "exit=$?"` prints the usage and `exit=2`.
  - `git tag cmd/rota/v0.0.0-pintest && sh scripts/module.sh pin v0.0.0-pintest; echo "exit=$?"; git tag -d cmd/rota/v0.0.0-pintest` prints `exists already` and `exit=1`.
  - `sh scripts/module.sh pin v0.0.0-pintest` prints `is not a tag on origin` and exits 1.
  - `sh scripts/module.sh pin v1.4.0-beta.1` prints `HEAD is not the commit tagged v1.4.0-beta.1` and exits 1.

  After these, `git tag -l 'cmd/rota/*'` prints nothing. The success path can only run at release time, in Task 6.

- [ ] **Step 7: Docs.** Change the Layout row to `` | `scripts/module.sh` | Checks the line between the two modules, tidies the command's against this checkout, and pins it to a root tag at a release | ``.

- [ ] **Step 8: Verify and commit.** The local CI run passes. Then:
  ```bash
  git add scripts/module.sh docs/reference.md
  git commit -m "scripts/module.sh tidies the program's module against this checkout and pins it to a pushed root tag, refusing a reused or out-of-order version"
  ```

### Task 3: The suite runs the binary it is given (in rota-sdk-test)

**Files** (in `~/GolandProjects/rota-sdk-test`, branch `rota-bin` from `main` at `1ab22ea`):
- Modify: `serve/helpers_test.go:23-52` (TestMain), `Makefile` (`local`), `.github/workflows/test.yml`
- Test: the commands below

**Interfaces:**
- Produces the `ROTA_BIN` contract:
  - Unset or empty: the serve tests behave as they do today. They build `github.com/professor93/rota/cmd/rota` by import path and skip on failure.
  - Set: the serve tests use that file and build nothing.
  - Set to a path that is not absolute, or to a binary whose `version` does not print `rota <version>`: the serve package prints `ROTA_BIN=<value>: <reason>` to stderr and exits 1 before running any test.
- The suite's `go.mod` keeps `require github.com/professor93/rota v1.3.0` and nothing else.

- [ ] **Step 1: Failing case.** `ROTA_BIN=/nonexistent <testenv> go test -count=1 ./serve/; echo "exit=$?"` prints `ok` and `exit=0`, because the variable is ignored today.
- [ ] **Step 2: Implement** the contract in `TestMain`, and say it in the comment above `rotaBin`.
- [ ] **Step 3: Verify.**
  - `ROTA_BIN=/nonexistent …` prints `ROTA_BIN=/nonexistent:` and `exit=1`.
  - `ROTA_BIN=bin/rota …` prints `ROTA_BIN=bin/rota:` with `absolute`, and `exit=1`.
  - Build a recording wrapper and run through it:
    ```
    mkdir -p /tmp/p0 && go -C <phase-0 worktree> build -o /tmp/p0/rota ./cmd/rota
    printf '#!/bin/sh\necho "$1" >> /tmp/p0/ran\nexec /tmp/p0/rota "$@"\n' > /tmp/p0/wrap && chmod +x /tmp/p0/wrap
    ```
    `ROTA_BIN=/tmp/p0/wrap <testenv> go test -count=1 -v -run TestRoot_UnauthenticatedSaysVersion ./serve/` prints `--- PASS`, and `head -1 /tmp/p0/ran` prints `version`.
  - With `ROTA_BIN` unset, `<testenv> go test -count=1 ./...` prints `ok` for every package, as on `main`.
  - `git diff --exit-code go.mod go.sum` prints nothing.
- [ ] **Step 4: `make local`.** The `local` recipe becomes `go -C ../cswapgo build -o $(CURDIR)/bin/rota ./cmd/rota`, then `ROTA_BIN=$(CURDIR)/bin/rota GOWORK=$(CURDIR)/go.work.local go test -tags local -count=1 ./...`. Add `, its binary included` to the header comment for `local`. `make local` then passes. Without this change it would skip the serve tests once `../cswapgo` splits.
- [ ] **Step 5: Suite CI.** In `test.yml`, before `go test ./...`, add:
  ```yaml
      - name: the program at the pinned version
        run: |
          v=$(go list -m -f '{{.Version}}' github.com/professor93/rota)
          GOBIN="$RUNNER_TEMP/bin" go install "github.com/professor93/rota/cmd/rota@$v"
          echo "ROTA_BIN=$RUNNER_TEMP/bin/rota" >> "$GITHUB_ENV"
  ```
  To check it locally, run the same three lines with `/tmp/p0/sbin` in place of `$RUNNER_TEMP/bin`; `/tmp/p0/sbin/rota version` prints `rota 1.3.0`. The `ruby -ryaml` parse of the file prints `yaml ok`.
- [ ] **Step 6: Commit and push the branch.**
  ```bash
  git add serve/helpers_test.go Makefile .github/workflows/test.yml
  git commit -m "The serve tests run the binary ROTA_BIN names and fail when it does not run"
  git push -u origin rota-bin
  ```

### Task 4: The release workflow knows the second tag

**Files:**
- Modify: `.github/workflows/release.yml`
- Test: the commands below

**Interfaces:**
- Consumes: the `ROTA_BIN` contract from Task 3 and `sh scripts/module.sh check` from Task 1.
- Produces the workflow's behaviour for each trigger:
  - A `v*` push runs gate, publish and smoke.
  - A `cmd/rota/v*` push runs only the module job.
  - A dispatch runs smoke and module.

- [ ] **Step 1: The gap.** Build a scratch gate: `G=$(mktemp -d); ln -s "$PWD" "$G/rota"; git clone -q -b rota-bin ~/GolandProjects/rota-sdk-test "$G/rota-sdk-test"`. Then run today's gate lines: `cd "$G/rota-sdk-test" && go work init . ../rota && <testenv> go test -count=1 -v -run TestRoot_UnauthenticatedSaysVersion ./serve/`. It prints `--- SKIP` with `building the pinned rota binary`: the split would pass the gate with `serve` untested.
- [ ] **Step 2: Edit `release.yml`.**
  - `on.push.tags` becomes `['v*', 'cmd/rota/v*']`, with the comment `# vX.Y.Z is the root and the binaries; cmd/rota/vX.Y.Z, pushed after it, is the program's module.`
  - `gate.if` and `publish.if` become `github.event_name == 'push' && !startsWith(github.ref_name, 'cmd/rota/')`.
  - The `run` of the gate's `rota's own tests` becomes five lines: `go vet ./...`, `go -C cmd/rota vet ./...`, `go test ./...`, `go -C cmd/rota test ./...`, `sh scripts/module.sh check`. This is bash with `-e`.
  - Add a gate step after it: `name: the program the suite runs`, `working-directory: rota`, `run: go build -o "$RUNNER_TEMP/rota" ./cmd/rota`. A failed build fails the gate.
  - The suite step's last line becomes `ROTA_BIN="$RUNNER_TEMP/rota" go test ./...`.
  - In the `module` job, delete `needs: publish` and set `if: github.event_name == 'workflow_dispatch' || startsWith(github.ref_name, 'cmd/rota/')`. Strip the prefix wherever the tag is read, with `v="${TAG#cmd/rota/}"`: the regex step checks `"$v"`, and the install step runs `go install "github.com/professor93/rota/cmd/rota@$v"` and checks `grep -qx "rota ${v#v}"`. Retitle its comment: `# The program's module as the world fetches it, by its own tag; by hand, the root's tag, and versions before the split resolve to the root.`
- [ ] **Step 3: Run the new gate lines.** `(cd "$G/rota" && sh scripts/module.sh check && go build -o "$G/rota-bin" ./cmd/rota)`, then `cd "$G/rota-sdk-test" && ROTA_BIN="$G/rota-bin" <testenv> go test -count=1 ./...` prints `ok` for every package, and with `-v -run TestRoot_UnauthenticatedSaysVersion ./serve/` it prints `--- PASS`.
- [ ] **Step 4: Old tags and both trigger forms (Review Focus 4).**
  - `for TAG in v1.3.0 v1.4.0-beta.2 cmd/rota/v1.4.0-beta.2 cmd/rota/x; do v="${TAG#cmd/rota/}"; echo "$v" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' && echo "$TAG -> $v" || echo "$TAG refused"; done` prints `v1.3.0 -> v1.3.0`, `v1.4.0-beta.2 -> v1.4.0-beta.2`, `cmd/rota/v1.4.0-beta.2 -> v1.4.0-beta.2` and `cmd/rota/x refused`.
  - `GOBIN="$G/old" go install github.com/professor93/rota/cmd/rota@v1.3.0 && "$G/old/rota" --version` prints `rota 1.3.0`.
- [ ] **Step 5: Parse.** `ruby -ryaml -e 'YAML.load_file(ARGV[0]); puts "yaml ok"' .github/workflows/release.yml` prints `yaml ok`. The `if:` expressions can only be checked by a real tag push, which Task 6 does.
- [ ] **Step 6: Commit.**
  ```bash
  git add .github/workflows/release.yml
  git commit -m "A release tags the program's module after the root, the module check runs on that tag, and the gate tests the program and hands its binary to the suite"
  ```

### Task 5: Notes, README and 1.4.0-beta.2

**Files:**
- Modify: `docs/releases/1.4.0.md`, `README.md` (after the `go install …@latest` block, line 32), `lib/httpx.go:21`, `wire/wire.go:424`
- Test: the commands below

**Interfaces:**
- Produces: version `1.4.0-beta.2`, and the notes that Task 6 publishes.

- [ ] **Step 1: Failing state.** `go run ./cmd/rota --version` prints `rota 1.4.0-beta.1`, and `grep -c 'cmd/rota/v' docs/releases/1.4.0.md` prints `0`.
- [ ] **Step 2: Size.** `sh scripts/release.sh 2>/dev/null && wc -c < dist/rota-linux-amd64` gives the baseline. It measured 10,350,752 bytes on d66824c with the split. Beta.1's asset, read with `gh release view v1.4.0-beta.1 --json assets -q '.assets[]|select(.name=="rota-linux-amd64").size'`, is 10,342,560. The two must differ by less than 65,536 bytes, because the split adds no code. Write the new size to one decimal in MB.
- [ ] **Step 3: Notes.** In `docs/releases/1.4.0.md`, after the paragraph under `### Changed for people coming from 1.3.2`, add:

  > **The `rota` program is a module of its own** from 1.4.0-beta.2: `github.com/professor93/rota/cmd/rota`, which requires `github.com/professor93/rota`, the SDK and the server, at the same version. The root still requires nothing, so a program that imports `rota/lib` or any other of its packages sees no change, and its `go.mod` and `go.sum` never gain what the `rota` program comes to need. Each version is two tags, `vX.Y.Z` and `cmd/rota/vX.Y.Z`. `go install github.com/professor93/rota/cmd/rota@<version>` works as before, for this version and every older one. While 1.4.0 is in beta, `@latest` gives the newest beta, because the program's module has no other versions yet; `install.sh` stays on the last stable release. A checkout builds as it did: `go.work` puts both modules in one workspace. The linux-amd64 binary is <N.N> MB, the size later betas are measured against.

  Before `### Tests`, add a section `### After a panic` with one bullet for each commit on main since beta.1, written in the notes' voice from these subjects:
  - 4bae8d7: a panicking launch or sweep lets go of the account's claim, and a panicking write leaves no temporary file.
  - 4d68ce7: a terminal or open run whose start panics ends what it started and frees the account and its slot.
  - a9ea8c1: a handover that panics or fails before the CLI frees the claim, and one that reaches the CLI passes it on.
  - d66824c: a watcher stopped by a signal logs that before it frees its lock.

  In each case the panic still reaches the caller unchanged.

  In `README.md`, after the `go install …@latest` block, add this paragraph:

  > While 1.4.0 is in beta, `@latest` gives the newest beta: the program became a module of its own in 1.4.0-beta.2, and that module has only betas so far. The installer above stays on the last stable release; `@v1.3.0` builds that one from source.
- [ ] **Step 4: Commit the notes.**
  ```bash
  git add docs/releases/1.4.0.md README.md
  git commit -m "The 1.4.0 notes and the README say the program is a module of its own and what go install gives during the betas"
  ```
- [ ] **Step 5: Bump.** Set `const Version = "1.4.0-beta.2"` in `lib/httpx.go:21` and `wire/wire.go:424`.
- [ ] **Step 6: Verify.**
  - `go run ./cmd/rota --version` prints `rota 1.4.0-beta.2`.
  - The local CI run passes.
  - `git diff --name-only origin/main -- '*.go'` prints exactly `lib/httpx.go` and `wire/wire.go`.
  - `git log --format=%B origin/main.. | grep -ciE 'co-authored|claude|anthropic|generated'` prints `0`.
- [ ] **Step 7: Commit and push.**
  ```bash
  git add lib/httpx.go wire/wire.go
  git commit -m "1.4.0-beta.2"
  git push -u origin phase-0
  ```

### Task 6: Release v1.4.0-beta.2 (only on the owner's word)

Everything in this task can only be verified by real pushes. Nothing here runs until the owner says so.

**Interfaces:**
- Consumes everything above. `HEAD` of `phase-0` is the `1.4.0-beta.2` commit.

- [ ] **Step 1: Suite first.** In rota-sdk-test, `git checkout main && git merge --ff-only rota-bin && git push origin main`, then `gh run watch` until the suite's CI passes. The gate checks out the suite's `main`.
- [ ] **Step 2: The numbers are free.** `git ls-remote --tags origin refs/tags/v1.4.0-beta.2 refs/tags/cmd/rota/v1.4.0-beta.2` prints nothing. Do not probe the proxy, which would cache a miss.
- [ ] **Step 3: Root tag.** Run `git tag v1.4.0-beta.2 && git push origin v1.4.0-beta.2`, then `gh run watch` on the newest `release.yml` run.
  - Gate, publish and all three smoke jobs succeed, and module is skipped.
  - `gh release view v1.4.0-beta.2 --json isPrerelease -q .isPrerelease` prints `true`.
  - `gh api repos/professor93/rota/releases/latest -q .tag_name` prints `v1.3.0`.
- [ ] **Step 4: Release notes.** Write this head to `$N/head.md`, where `N=$(mktemp -d)`:

  > A pre-release for testing before 1.4.0. `install.sh` gives the last stable version. `go install github.com/professor93/rota/cmd/rota@latest` now gives this beta, because the program became a module of its own in this version and that module has only betas so far; `@v1.3.0` gives the last stable one. To try this one:
  >
  > ```
  > curl -fsSL https://raw.githubusercontent.com/professor93/rota/main/install.sh | ROTA_VERSION=1.4.0-beta.2 bash
  > go install github.com/professor93/rota/cmd/rota@v1.4.0-beta.2
  > ```
  >
  > It carries everything since 1.3.0: 1.3.1 and 1.3.2 were never published on their own.

  Then run `for f in "$N/head.md" docs/releases/1.4.0.md docs/releases/1.3.2.md docs/releases/1.3.1.md; do cat "$f"; echo; done > "$N/notes.md" && gh release edit v1.4.0-beta.2 --notes-file "$N/notes.md"`.
- [ ] **Step 5: Pin.** `sh scripts/module.sh pin v1.4.0-beta.2` prints `module: tagged cmd/rota/v1.4.0-beta.2`. `git show --stat --format=%s HEAD` shows `cmd/rota requires rota v1.4.0-beta.2`, `cmd/rota/go.mod` and `cmd/rota/go.sum`. `cmd/rota/go.sum` has exactly two lines, both starting `github.com/professor93/rota v1.4.0-beta.2`.
- [ ] **Step 6: Program tag.** Run `git push origin phase-0 cmd/rota/v1.4.0-beta.2`, then watch the new `release.yml` run: only `module` runs, and it succeeds. `gh release view cmd/rota/v1.4.0-beta.2` prints `release not found`.
- [ ] **Step 7: As the world sees it.** In `T=$(mktemp -d)`, outside any module:
  - `GOBIN=$T go install github.com/professor93/rota/cmd/rota@v1.4.0-beta.2 && $T/rota --version` prints `rota 1.4.0-beta.2`.
  - `GOBIN=$T/old go install github.com/professor93/rota/cmd/rota@v1.3.0 && $T/old/rota --version` prints `rota 1.3.0`.
  - `go list -m github.com/professor93/rota/cmd/rota@latest` prints `github.com/professor93/rota/cmd/rota v1.4.0-beta.2`.
  - `go list -m github.com/professor93/rota@latest` prints `github.com/professor93/rota v1.3.0`.
- [ ] **Step 8: Main.** `git push origin phase-0:main` (a fast-forward). Then `gh run watch` on the `ci.yml` run for `main`: test on ubuntu and on macos, and windows, all succeed. This is the first Windows run of the split, because CI runs only on `main` and on PRs.
- [ ] **Step 9: Rebuild.** In the main checkout, `git pull --ff-only && go build -trimpath -ldflags='-s -w' -o ~/.local/bin/rota ./cmd/rota && rota --version` prints `rota 1.4.0-beta.2`. Tell the owner the linux-amd64 size from Task 5 Step 2.
