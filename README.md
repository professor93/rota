# rota

Run several AI coding CLIs — **claude**, **codex**, **grok** —
across several accounts, without ever switching the account you are logged
into.

Register each account once, give it a place in the queue, and ask. rota
launches each vendor's own CLI with a credential you own, takes the first
account still under its usage threshold, and moves on to the next when one
is spent. It is not a proxy: nothing is intercepted, nothing impersonated,
and the whole project has **zero third-party dependencies**.

**The name.** A rota is a roster: the list that says whose turn it is.
This one is a rota of accounts — each takes its turn until it is spent,
and the next steps in.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/professor93/rota/main/install.sh | bash
```

macOS and Linux, arm64 and amd64. The script verifies the release checksum
and installs `rota` into `/usr/local/bin` (or `~/.local/bin` without sudo).
On Windows, take `rota-windows-amd64.exe` or `rota-windows-arm64.exe` from
the [releases page](https://github.com/professor93/rota/releases/latest) and
put it on your PATH.

From source instead (Go 1.27+):

```sh
go install github.com/professor93/rota/cmd/rota@latest
```

## Use

```sh
rota login                     # sign a Claude account in (prints a URL)
rota login codex               # ...or another provider: codex, grok
rota login <login-id> <code>   # finish with the code from the browser
rota login --long              # ...a token for an account here that lasts a year
rota list                      # every account, in rotation order, with usage

rota "explain this repo"       # ask — the rotation picks the account
rota 2 "explain this repo"     # ...or name account 2 yourself
rota run 1 --stateless "2+2?"  # no session saved, no settings or memory read
rota run 1 "explain it" --with deltas  # stream each fragment as the model writes it
rota run 1 "explain it" --with blocks,ask  # JSON, with fences split and the question read
rota run 2 "start here" --input   # keep it open; type more lines, /interrupt, /close
rota send 7f3c1a5d "and the tests?"  # ...or send into that run from another terminal
rota run 2                     # open account 2's CLI interactively
rota run 2 --share             # ...and let a rota server's page watch it too
rota set 2 --order first       # put account 2 first; the rest move down
rota set 2 --order before:5    # ...or right before account 5, or up, down, last, out
rota set 2 --threshold 80      # move on to the next account at 80% usage

rota serve 8787 --token=SECRET # the same over HTTP, with a web playground
```

Every answer prints to stdout; add `--json` anywhere for machine-readable
output. `rota list --sessions` shows what is running right now and which
conversations `--resume` could pick up.

An access token lasts eight hours, and a window left open overnight outlives
it. `rota login --long` stores a second, year-long credential for an account
already registered, and every launch prefers it from then on;
`rota set <id> --long forget` throws it away.

## The HTTP API

`rota serve` exposes accounts at `/v1/accounts`, one-call runs at
`POST /v1/run` (the rotation picks) or `POST /v1/accounts/{id}/run`, and a
playground UI at `/playground`. Streaming is SSE, or NDJSON with
`Accept: application/x-ndjson`. A run started with `"input": true` stays open
for more messages: `/v1/runs/{id}/messages`, `interrupt` and `close` reach it
while it runs, and `/v1/runs/{id}/events` reattaches a reader that dropped.
Paths a request names can be confined with
`--root`; everything risky is refused unless the operator allows it.

Everything `rota serve` takes can be written down once in
`~/.rota/server.toml` — the address, TLS, which groups of routes to answer,
and the people and tokens that may sign in, each `control` (may run things)
or `watch` (may only look). `rota serve --print-config` prints what it would
serve with and where each value came from.

```sh
rota serve --config ~/.rota/server.toml
rota serve passwd alice --role control   # prints a [[users]] block to paste
```

With `[routes] terminal = true` the server also serves `/terminal`: an
account's CLI running on the server itself, kept when the browser tab
closes, watched by several people and typed into by one at a time. It is off
by default, and refuses an address off the loopback without TLS, because a
`control` sign-in on it runs commands as the user the server runs as.

The terminal — both the server's own and `rota run --share` — is Linux and
macOS only. A Windows build has everything else.

## Use it as a library

The SDK lives at `github.com/professor93/rota/lib` (import name `rota`) and
depends on the Go standard library alone. It authenticates accounts,
refreshes tokens, reads quotas, and runs agents — values in, values out. It
stores nothing, reads no environment, and makes no decision an application
could make: where files and tokens live is yours.

```go
import rota "github.com/professor93/rota/lib"

l, _ := rota.Begin(ctx, "claude")
tok, _ := l.Complete(ctx, code)
a := rota.NewAccount(1, "claude", tok)
res, _ := rota.Run(ctx, a, home, cmd, rota.Spec{Prompt: "hi"}, nil, os.Stdout)
```

The full manual — every command, field, endpoint, and design note — is in
[docs/reference.md](docs/reference.md).

## License

[MIT](LICENSE)
