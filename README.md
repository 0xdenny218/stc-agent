# stc-agent

**English** | [简体中文](README.zh-CN.md)

**A minimal CLI chat agent where every capability is a fiber** — built on
[stc-go](https://github.com/0xdenny218/stc-go), the Go implementation of the
spatiotemporal composability paradigm.

> Status: **M3 done** — every `*.wasm` in `--tools-dir` is a guest tool
> fiber, hot-swapped in place when you rebuild it, mid-conversation. See
> milestones below.

## Usage

```sh
export DEEPSEEK_API_KEY=...   # or STC_AGENT_API_KEY / OPENAI_API_KEY
go run ./cmd/stc-agent
```

Config precedence: built-in defaults < config file < environment < flags.

| Flag | Environment | Default |
|---|---|---|
| `--base-url` | `STC_AGENT_BASE_URL` | `https://api.deepseek.com` |
| `--api-key` | `STC_AGENT_API_KEY`, then `DEEPSEEK_API_KEY`, then `OPENAI_API_KEY` | — (required) |
| `--model` | `STC_AGENT_MODEL` | `deepseek-chat` |
| `--timeout` | — | `60s` |
| `--transcript PATH` | — | JSONL transcript; an existing file is replayed |
| `--resume PATH` | — | alias of `--transcript` |
| `--tools-dir DIR` | — | `tools.d`; every `*.wasm` in it is a guest tool |
| `--config PATH` | — | `~/.config/stc-agent/config.json` if present |

Commands inside the REPL:

- `/model <name>` — switch models mid-conversation. This re-provides the
  config service; the model client and REPL reload reactively, while the
  session fiber (which depends on neither) keeps the history verbatim.
- `/tools` — list registered tools.
- `/help` — list commands.
- `/quit` — exit.

Tools (each is its own fiber registering into a stable toolset service, so
tool churn never reloads the agent loop):

- `read_file` / `write_file` — filesystem access, 32 KiB output cap.
- `shell` — `sh -c` with a 30s timeout, working directory pinned to the
  launch directory. **There is no permission pipeline: the model can run
  arbitrary commands as your user.** Run it only in directories you are
  comfortable with.

A turn runs `[model → tool]*` until the model answers (tool calls are traced
as `→ name(args)`), with a circuit breaker of 10 tool iterations. Tool
failures are fed back to the model as result text, not turn-fatal errors; a
mid-turn reload fills unanswered tool calls with an aborted marker so the
transcript stays wire-valid.

## Guest tools (WASM, hot-swapped mid-conversation)

Every `*.wasm` file in `--tools-dir` (default `tools.d`) is loaded as a tool
fiber: the tool name is the file name (`dice.wasm` → `dice`), and the wire
description comes from the guest itself — it provides a `tool.<name>` service
in its `start`. Invocations cross the boundary as JSON strings
(`Handle.Call("invoke", args)`). Each file is watched: **rebuild it in place
and the next turn uses the new version — same process, same session, history
verbatim.** An in-flight call finishes on the old version before the swap
lands; a broken rebuild reports an error and keeps the old version serving.

Guest tools are plain Go compiled with [TinyGo](https://tinygo.org/) against
`stc-go/guest` — the SDK exports the call ABI (`stc_alloc`/`stc_free`/
`invoke`), so a guest is just a descriptor plus one handler:

```go
//go:build wasm

package main

import "github.com/0xdenny218/stc-go/guest"

func init() {
	guest.OnInvoke(func(args string) string { return `{"roll":4}` })
}

//export start
func start() {
	_ = guest.Provide("tool.dice", `{"name":"dice","description":"roll a six-sided die","parameters":{"type":"object","properties":{}}}`)
}

func main() {}
```

A runnable one lives in [`examples/guests/dice`](examples/guests/dice):

```sh
mkdir -p tools.d
tinygo build -target wasip1 -buildmode=c-shared -o tools.d/dice.wasm ./examples/guests/dice
stc-agent &                       # ask it to roll
tinygo build ... -tags v2 -o tools.d/dice.wasm ./examples/guests/dice
#                                 ask again — v2 answers, session intact
```

`scripts/demo-hotswap.sh` drives the whole scenario against the real model
(build v1 → roll → rebuild v2 in place → roll again, asserting the version
markers in the transcript). Notes: a guest that fails to load at boot fails
the boot (fiber dump, exit 1); a guest that fails to *reload* keeps the old
version serving. The descriptor is read once at load — a swap changes
behavior, not the registered name/description.

## What it is

- A CLI chat agent (stdin/stdout) with a tool-calling loop.
- Every capability is a fiber: the model client, tools, slash commands, the
  session, the REPL itself. Assembly is entirely paradigm machinery
  (`Load`/`Provide`/`Inject`/`Effect`); `main` only builds the list.
- The differentiating demo: **hot-swap a WASM guest tool mid-conversation —
  no restart, no lost session.** Changing the model cascades through
  re-provision and reactive reloads, while chat history survives — survival
  falls out of the dependency graph itself.
- stc-agent is stc-go's first real consumer: API friction points and
  satellite packages (event dispatch, config schema, host→guest calls) are
  pulled by real needs here, not pushed by speculation.

## Non-goals

- No UI/TUI/web. No framework abstractions — patterns get extracted only
  after they repeat in the agent.
- Not a [dsh](https://github.com/deepseek-ai/deepseek-harness) competitor:
  no streaming, no provider abstraction layer, no MCP/skills/subagents, no
  permission pipeline.

## Milestones

- [x] M0 scaffold (repo, CI, bilingual README, main skeleton)
- [x] M1 minimal chat loop (config/model/session/cli fibers, `/model` cascade)
- [x] M2 tool system + agent loop (toolset as stable service, static Go tools)
- [x] M3 WASM guest tools + hot reload (hmr): mid-conversation tool swap
- [ ] M4 release + satellite-package review (what flows back into stc-go)

## Development

Same bar as stc-go: `gofmt`, `go vet`, `go test -race` all green.

```sh
go test -race ./...
```

The guest-tool tests build real guests and skip when TinyGo is absent (set
`TINYGO`, put it in `PATH`, or install to `~/.local/opt/tinygo`); CI installs
TinyGo and runs them for real.

## License

[MIT](LICENSE)
