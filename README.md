# stc-agent

**English** | [简体中文](README.zh-CN.md)

**A minimal CLI chat agent where every capability is a fiber** — built on
[stc-go](https://github.com/0xdenny218/stc-go), the Go implementation of the
spatiotemporal composability paradigm.

> Status: **M1 done** — minimal chat loop works: config/model/session/cli
> fibers, `/model` cascade, JSONL transcript with resume. See milestones below.

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
| `--config PATH` | — | `~/.config/stc-agent/config.json` if present |

Commands inside the REPL:

- `/model <name>` — switch models mid-conversation. This re-provides the
  config service; the model client and REPL reload reactively, while the
  session fiber (which depends on neither) keeps the history verbatim.
- `/quit` — exit.

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
- [ ] M2 tool system + agent loop (toolset as stable service, static Go tools)
- [ ] M3 WASM guest tools + hot reload (hmr): mid-conversation tool swap
- [ ] M4 release + satellite-package review (what flows back into stc-go)

## Development

Same bar as stc-go: `gofmt`, `go vet`, `go test -race` all green.

```sh
go test -race ./...
```

## License

[MIT](LICENSE)
