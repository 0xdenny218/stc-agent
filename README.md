# stc-agent

**English** | [简体中文](README.zh-CN.md)

**A minimal CLI chat agent where every capability is a fiber** — built on
[stc-go](https://github.com/0xdenny218/stc-go), the Go implementation of the
spatiotemporal composability paradigm.

> Status: **v0.2.0 released — M0–M10 done.** Two flagship tricks, both
> mid-conversation with no restart, no lost session: rebuild a `*.wasm` in
> `--tools-dir` and the next turn uses the new version; or let the model
> write its own tool (`define_guest`) — the host compiles the Go source
> with TinyGo and loads it as a live tool, same turn.

## Install

```sh
go install github.com/0xdenny218/stc-agent/cmd/stc-agent@latest
```

or clone and `go run ./cmd/stc-agent`. Running the agent only needs Go;
developing guest tools additionally needs [TinyGo](https://tinygo.org/).

## Usage

```sh
export DEEPSEEK_API_KEY=...   # or STC_AGENT_API_KEY / OPENAI_API_KEY
go run ./cmd/stc-agent                          # interactive REPL
go run ./cmd/stc-agent -p "explain this repo"   # one-shot: print the answer and exit
```

Config precedence: built-in defaults < config file < environment < flags.

| Flag | Environment | Default |
|---|---|---|
| `--base-url` | `STC_AGENT_BASE_URL` | `https://api.deepseek.com` |
| `--api-key` | `STC_AGENT_API_KEY`, then `DEEPSEEK_API_KEY`, then `OPENAI_API_KEY` | — (required) |
| `--model` | `STC_AGENT_MODEL` | `deepseek-chat` |
| `--timeout` | — | `60s` |
| `--transcript PATH` | — | JSONL event log (messages + token usage); an existing file is replayed |
| `--resume PATH` | — | alias of `--transcript` |
| `--tools-dir DIR` | — | `tools.d`; every `*.wasm` in it is a guest tool |
| `--skills-dir DIR` | — | `skills.d`; every `<name>/SKILL.md` hot-loads as a skill fiber |
| `--spill-dir DIR` | — | `spill`; where the `spill` tool writes scratch files |
| `--authored-dir DIR` | — | `<tools-dir>/authored`; sources and builds of model-authored guest tools |
| `--tinygo PATH` | — | `tinygo` (from PATH); the compiler `define_guest` uses |
| `--mcp SPEC` | — | MCP stdio server as `name=command args...` (repeatable) |
| `--config PATH` | — | `~/.config/stc-agent/config.json` if present |
| `-p, --print TEXT` | — | run a single turn non-interactively, print the answer, exit 0 |
| `--allow LIST` | — | comma-separated tool names to auto-approve (`*` = all); appended to the policy |
| `--compact-threshold N` | — | `100000`; compact history when a turn's prompt tokens exceed N (0 disables) |
| — | `STC_AGENT_WEB_SEARCH_URL` | DuckDuckGo Instant Answer template (`{q}` = query); swap in any search backend |

On a terminal the REPL has readline line editing with history (plain line
reads when stdin is piped). Model answers stream in as they arrive.
Ctrl-C interrupts the current turn without killing the session (at the
prompt it discards the current line); Ctrl-D on an empty line exits.

Commands inside the REPL:

- `/model <name>` — switch models mid-conversation. This re-provides the
  config service; the model client and REPL reload reactively, while the
  session fiber (which depends on neither) keeps the history verbatim.
- `/tools` — list registered tools.
- `/plan` — toggle plan mode (block non-read-only tools until the plan is
  approved via `exit_plan_mode`).
- `/help` — list commands.
- `/quit` — exit.

Tools (each is its own fiber registering into a stable toolset service, so
tool churn never reloads the agent loop — the toolset and the slash-command
registry are [`stc-go/registry`](https://github.com/0xdenny218/stc-go/tree/main/registry),
the first completed reflux cycle: the pattern repeated here twice, was
extracted upstream, and this repo deleted its own copies):

- `read_file` / `write_file` — filesystem access, 32 KiB output cap.
- `edit` / `glob` / `grep` — exact-string edit, glob with `**` recursion,
  regex search with `path:line:` output (binary files skipped) (M10).
- `shell` — `sh -c` with a 30s timeout, working directory pinned to the
  launch directory.
- `inspect_agent` — self-description: every fiber's live state plus the
  current tool catalog, as JSON. Read-only, auto-approved by the default
  policy.
- `spill` — write a scratch file (drafts, notes, artifacts) into
  `--spill-dir`; single-segment names only (M10).
- `session_title` — label the session; lands as a `title` event in the
  log, replay restores the latest (M10).
- `web_fetch` / `web_search` — fetch a URL / search the web behind one
  SSRF-guarded core (M10).
- `define_guest` — write a Go guest tool; the host compiles it with
  TinyGo and loads it as a regular tool (M10).
- `task` — spawn a sub-agent on a self-contained prompt; its final answer
  flows back as the tool result (M9).
- `todo_write` — maintain a task list rendered into the system prompt (M9).
- `exit_plan_mode` — propose a plan and exit plan mode on approval (M9).
- `job_start` / `job_list` / `job_kill` — background shell commands or
  sub-agents, enumerable and killable (M9).

Every tool call passes an approval gate before it runs. The default policy
auto-approves the read-only set (`read_file`, `inspect_agent`, `glob`,
`grep`) and asks about everything else; configure it in
the config file (`{"approval": {"allow": [...], "deny": [...]}}` — a file
policy replaces the default) or with `--allow`. The deny list wins over the
allow list; a tool matched by neither asks. A question suspends the turn
mid-loop:

```
! allow "shell" to run?
  {"command": "echo hi"}
  [y] allow once  [n] deny  [a] always allow shell
```

`y` runs this one call; `n` feeds `error: denied by user` back to the model
as the tool result (the turn continues); `a` auto-approves the tool for the
rest of the session. With no interactive answer possible (`-p` headless
mode) the gate is fail-closed: having to ask means denying. Every decision
reached by asking — allow or deny — is appended to the transcript as an
`approval` event with its source (`user` / `policy` / `fail-closed`);
policy-allowed calls stay silent.

Hooks bracket the pipeline. Before the gate, a call passes any registered
**intercept hooks** (`tools/pre-execute`): the first hook to object bails
the call and its reason is fed back like a denial. After a real execution,
`tools/post-execute` fires as a notification; turns are bracketed by
`agent/turn-start` / `agent/turn-end`. A hook is just a fiber — registering
a listener or an interceptor is a reversible effect, so unloading the fiber
retracts the hook (notify dispatch is stc-go's core `On`/`Emit`; intercept
is the registry satellite — no third dispatch machinery, evaluation filed
as [stc-go#5](https://github.com/0xdenny218/stc-go/issues/5)).

The system prompt is assembled from fiber-registered **segments**,
name-sorted and joined with blank lines: the agent ships one identity
segment, and any fiber can add or retract its own — the next request sees
the new prompt.

A turn runs `[model → tool]*` until the model answers (the answer streams
in chunk by chunk; tool calls are traced as `→ name(args)`), with a circuit
breaker of 10 tool iterations. Tool
failures and denials are fed back to the model as result text, not
turn-fatal errors; a
mid-turn reload fills unanswered tool calls with an aborted marker so the
transcript stays wire-valid. The transcript is an append-only event log:
messages, token usage per model call, approval decisions — the in-memory
history is just a
projection of it, and resume replays the projection.

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
markers in the transcript). The same scenario runs headless in CI as
`TestE2EHotSwapKeepsSession` (scripted mock model), asserting a
byte-identical tool list and a verbatim transcript across the swap.

A real run (2026-08-17, GLM `glm-4.5` via their Coding Plan endpoint — only the `STC_AGENT_BASE_URL`/`STC_AGENT_API_KEY`/
`STC_AGENT_MODEL` env vars changed, no code):

```
==> turn 1: asking the model to roll (dice v1 on disk)
    tool result: {"role":"tool","content":"{\"roll\":3,\"sides\":6,\"version\":\"v1\"}", ...}
==> rebuilding dice.wasm in place as v2 (agent pid 88227 keeps running)
    hot-swap landed; agent still pid 88227
==> turn 2: asking again (v2 now serving)
    tool result: {"role":"tool","content":"{\"roll\":3,\"sides\":6,\"version\":\"v2\"}", ...}
```

Notes: a guest that fails to load at boot fails
the boot (fiber dump, exit 1); a guest that fails to *reload* keeps the old
version serving. The descriptor is read once at load — a swap changes
behavior, not the registered name/description.

## Skills (hot-loaded directories)

Every subdirectory of `--skills-dir` (default `skills.d`) containing a
`SKILL.md` is a skill — and a fiber. The file is an optional frontmatter
(`name` / `description`) plus a body; the body registers as a system-prompt
segment (`skill:<name>`, name-sorted with the rest), and any `*.wasm` in
the directory loads as the skill's own guest tools via the same machinery
as `--tools-dir` (hmr included). A supervisor fiber watches the directory:
**drop a skill in and its prompt segment and tools are live for the next
turn — delete it and they retract, mid-conversation, no restart.** Editing
`SKILL.md` in place hot-updates the segment (the registry's same-name
overwrite). A bad skill at boot fails the boot; a bad skill dropped later
is reported (`[skill] <name>: <err>`) without disturbing the rest.

## MCP servers (stdio)

`--mcp "name=command args..."` (repeatable, or the `"mcp"` key in the
config file) spawns each MCP stdio server as its own fiber: handshake,
`tools/list`, and every tool registers into the toolset as
`mcp__<server>__<tool>` (description prefixed `[mcp:<server>]`), so the
agent loop, approval gate, hooks and `/tools` all treat them like any
other tool. **Disconnect = tools vanish**: when a server process exits,
its tools unregister immediately (`[mcp] <name> disconnected; N tools
removed`) and the next request simply doesn't advertise them. A minimal
example server lives in [`examples/mcp/echo`](examples/mcp/echo). Stdio
only — MCP over HTTP/SSE is a non-goal.

## Sub-agents, compaction, todos, plan mode, background jobs (M9)

**Sub-agents** (`task` tool): the model hands a self-contained subtask to a
child agent that runs its own turns in a child scope (stc-go
`Context.Child` + `Isolate`, spec D17). The child shares the model and
approval gate but gets a fresh session, toolset and turn-runner in
freshly-created realms — its provides never collide with the parent's and
its event log never touches the parent's. The child's final answer flows
back as an ordinary tool message into the parent's transcript. The default
tool subset is everything except `task` (no recursion); `{"tools": [...]}`
narrows it.

**Compaction** (`--compact-threshold`): when a turn's prompt tokens exceed
the threshold, the loop asks the model to summarize the history into a
single `compaction` event; the projection folds the pre-summary messages
away, so the next request starts from the summary. The summary's own token
usage is logged too.

**Todos** (`todo_write` tool): the current task list is a stream of session
events, rendered into the system prompt as a live segment (the registry's
same-name overwrite retracts/rewrites it). Emptying the list removes the
segment.

**Plan mode** (`/plan`, `exit_plan_mode`): `/plan` toggles a mode that
blocks every non-read-only tool at the gate — research first, then call
`exit_plan_mode` with the plan. The agent asks you to approve; yes turns
the mode off and feeds the plan back, no keeps you in plan mode. Every
decision is logged like an approval event.

**Background jobs** (`job_start` / `job_list` / `job_kill`): start a shell
command or a sub-agent on a self-contained prompt and return immediately;
completion lands as a user message at the next model call, while
`job_list` enumerates and `job_kill` cancels. Shell and sub-agent
background work converge on one lifecycle (the `task` path).

## Tool pack, web tools, agent-authored guest tools (M10)

**Tool pack**: `edit` does exact-string replacement — a missing `old_string`
or an ambiguous one (multiple hits without `replace_all`) is an error fed
back to the model to correct, not a silent partial write. `glob` supports
`**` across directory levels; `grep` prints `path:line: content`, skips
binary files, and requires `recursive=true` to walk a directory. `spill`
writes scratch files into `--spill-dir` (single-segment names only — no
path traversal). `session_title` labels the session as a `title` event in
the log; replay restores the latest.

**Web**: `web_fetch` and `web_search` share one fetch core — http/https
only, SSRF-guarded (private/loopback/link-local targets are blocked, and a
failed lookup counts as blocked), 1 MiB body cap with truncation, binary
bodies rejected. `web_search` hits the key-free DuckDuckGo Instant Answer
endpoint by default; `STC_AGENT_WEB_SEARCH_URL` swaps in any `{q}`
endpoint template. Both are remote tools, so the default policy asks.

**`define_guest` — the model writes its own tools.** The model passes a
name and a complete Go source; the host compiles it with TinyGo
(`--tinygo`) and loads it through the same `guest.Load` path as
`--tools-dir` — hot reload, the approval gate, and rollback all come
along. Sources and builds land in `--authored-dir` (default
`<tools-dir>/authored`, kept apart from hand-dropped wasms so boot
scanning never double-loads them). Failure rolls back cleanly: the wasm is
deleted, nothing leaks into the toolset, and the source stays on disk so
the model can retry. Re-defining a name swaps the tool in place.

A real run (2026-08-18, GLM `glm-4.5` via their Coding Plan endpoint —
asked to define a `shout` tool that upper-cases `{"text": ...}`):

```
→ define_guest({"name": "shout", "source": "package main\n
    import (\"encoding/json\"; \"github.com/0xdenny218/stc-go/guest\"; \"strings\")\n
    init(): guest.OnInvoke(unmarshal {\"text\"} → strings.ToUpper)\n
    start(): guest.Provide(\"tool.shout\", {...})..."})
tool result: guest tool "shout" defined and loaded (source kept at tools.d/authored/shout.go)
```

The model wrote a correct guest in one shot (JSON parse, `OnInvoke`,
`Provide` with the descriptor); the host compiled and loaded it, and the
next request advertised `shout` like any built-in. The E2E
(`TestE2EAuthoredGuestTool`, real TinyGo) drives the whole loop against a
scripted model: define → invoke → wasm-sourced result, plus the rollback
contracts (compile failure leaves no residue, source kept for retry).

## What it is

- A CLI chat agent (stdin/stdout) with a streaming tool-calling loop.
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

- No UI/TUI/web, no provider abstraction layer, no sandboxed execution, no
  MCP over HTTP/SSE, no skill marketplace/distribution.
- No framework ambitions of its own: no plugin distribution, no profile
  composition, no second-development platform. stc-agent is to
  [stc-go](https://github.com/0xdenny218/stc-go) what dsh is to Cordis —
  the agent that exercises the framework to its full requirements, so that
  framework capabilities grow upstream via reflux. Its **agent** capability
  set takes [dsh](https://github.com/deepseek-ai/deepseek-harness) as the
  reference: hooks, skills and MCP are done (M7–M8); subagents, compaction,
  todos, plan mode and background jobs are done (M9); the tool pack and
  agent-authored guest tools are done (M10). The harness roadmap is
  complete.

## Milestones

- [x] M0 scaffold (repo, CI, bilingual README, main skeleton)
- [x] M1 minimal chat loop (config/model/session/cli fibers, `/model` cascade)
- [x] M2 tool system + agent loop (toolset as stable service, static Go tools)
- [x] M3 WASM guest tools + hot reload (hmr): mid-conversation tool swap
- [x] M4 release + satellite-package review (v0.1.0 on GitHub; the review is filed as stc-go issues)
- [x] M5 session spine (event log) + streaming + terminal interaction
  (readline, `-p` headless)
- [x] M6 tool pipeline + approval gate (policy + mid-turn question loop,
  decisions logged as events)
- [x] M7 hooks (notify + intercept) + system-prompt segments +
  `inspect_agent` self-description
- [x] M8 skills (SKILL.md directories hot-load as fibers: prompt segment +
  guest tools) + MCP stdio servers as tool fibers (disconnect = tools
  vanish)
- [x] M9 subagents (child scopes) + compaction + todos + plan mode +
  background jobs (shell and sub-agent, one lifecycle)
- [x] M10 tool pack (edit/glob/grep, spill, session_title) + web tools
  (SSRF-guarded `web_fetch`/`web_search`) + agent-authored guest tools
  (`define_guest`: model-written source → TinyGo compile → load; v0.2.0)

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
