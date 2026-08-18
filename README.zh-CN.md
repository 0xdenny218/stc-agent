# stc-agent

[English](README.md) | **简体中文**

**万物皆插件的最小 CLI 对话 agent**——基于
[stc-go](https://github.com/0xdenny218/stc-go)（时空可组合性范式的 Go 实现）。

> 状态：**v0.1.1 已发布**——`--tools-dir` 里的每个 `*.wasm` 都是 guest
> 工具 fiber，原地重建即在对话进行中热替换。里程碑 M0–M9 完成（M9：
> subagent + compaction + todos/plan/jobs）；harness 化路线 M10 见下。

## 安装

```sh
go install github.com/0xdenny218/stc-agent/cmd/stc-agent@latest
```

或克隆后 `go run ./cmd/stc-agent`。运行 agent 只需要 Go；开发 guest
工具另需 [TinyGo](https://tinygo.org/)。

## 用法

```sh
export DEEPSEEK_API_KEY=...   # 或 STC_AGENT_API_KEY / OPENAI_API_KEY
go run ./cmd/stc-agent                          # 交互式 REPL
go run ./cmd/stc-agent -p "explain this repo"   # 一次性：打印答复后退出
```

配置优先级：内置默认 < 配置文件 < 环境变量 < 命令行。

| 参数 | 环境变量 | 默认 |
|---|---|---|
| `--base-url` | `STC_AGENT_BASE_URL` | `https://api.deepseek.com` |
| `--api-key` | `STC_AGENT_API_KEY`，再 `DEEPSEEK_API_KEY`，再 `OPENAI_API_KEY` | —（必填） |
| `--model` | `STC_AGENT_MODEL` | `deepseek-chat` |
| `--timeout` | — | `60s` |
| `--transcript PATH` | — | JSONL 事件日志（消息 + token 用量）；文件已存在则 replay 恢复 |
| `--resume PATH` | — | `--transcript` 的别名 |
| `--tools-dir DIR` | — | `tools.d`；其中每个 `*.wasm` 是一个 guest 工具 |
| `--skills-dir DIR` | — | `skills.d`；其中每个 `<name>/SKILL.md` 热装载为一个 skill fiber |
| `--mcp SPEC` | — | MCP stdio server，形如 `name=command args...`（可重复） |
| `--config PATH` | — | `~/.config/stc-agent/config.json`（存在才读） |
| `-p, --print TEXT` | — | 非交互跑单轮：打印答复，exit 0 |
| `--allow LIST` | — | 逗号分隔的免审批工具名（`*` = 全部）；追加进策略 |
| `--compact-threshold N` | — | `100000`；一轮 prompt tokens 超过 N 即压缩历史（0 关闭） |

在终端里 REPL 有 readline 行编辑与历史（stdin 是管道时退回普通逐行
读取）。模型答复逐块流式呈现。Ctrl-C 中断当前轮而不杀会话（在提示符
处则是丢弃当前行）；空行上 Ctrl-D 退出。

REPL 内命令：

- `/model <name>`——对话中途换模型。config 服务被重提供，模型客户端与
  REPL 反应式重载，而 session fiber（不依赖这两者）逐字保留历史。
- `/tools`——列出已注册工具。
- `/plan`——切换 plan 模式（在计划经 `exit_plan_mode` 批准前阻断非只读
  工具）。
- `/help`——列出命令。
- `/quit`——退出。

工具（每个工具是独立 fiber，注册进稳定的 toolset 服务，因此工具增删
不会重载 agent 循环——toolset 与斜杠命令注册表就是
[`stc-go/registry`](https://github.com/0xdenny218/stc-go/tree/main/registry)，
首个完整回流循环：模式在此重复两处 → 提取到上游 → 本仓库删除自己的
副本）：

- `read_file` / `write_file`——文件读写，输出上限 32 KiB。
- `shell`——`sh -c` 执行，30 秒超时，工作目录固定为启动目录。
- `inspect_agent`——自描述：每个 fiber 的实时状态加上当前工具目录，
  JSON 格式。只读，默认策略放行。
- `task`——派生子 agent 处理一条自包含指令，终答作为工具结果回流（M9）。
- `todo_write`——维护任务清单，渲染进 system prompt（M9）。
- `exit_plan_mode`——提出计划，获批后退出 plan 模式（M9）。
- `job_start` / `job_list` / `job_kill`——后台 shell 命令或子 agent，
  可列可杀（M9）。

每次工具调用先过审批门再执行。默认策略自动放行 `read_file`，其余一律
询问；策略可在配置文件里配（`{"approval": {"allow": [...], "deny": [...]}}`——
文件策略整体替换默认）或用 `--allow` 追加。deny 名单优先于 allow
名单；两边都不命中的工具走询问。询问会把轮次挂起在工具循环中途：

```
! allow "shell" to run?
  {"command": "echo hi"}
  [y] allow once  [n] deny  [a] always allow shell
```

`y` 只放行这一次；`n` 把 `error: denied by user` 作为工具结果回灌给
模型（轮次继续）；`a` 在本会话内对该工具免审批。无法交互作答时
（`-p` headless 模式）门禁 fail-closed：问不了就拒绝。每个经询问得出
的决定——批准或拒绝——都以 `approval` 事件追加进 transcript，并记录
来源（`user` / `policy` / `fail-closed`）；策略放行的调用不产生事件。

hooks 包围整条管线。过审批门之前，调用先过已注册的**拦截 hook**
（`tools/pre-execute`）：第一个提出异议的 hook 阻断调用，理由像拒绝
一样回灌。真实执行之后，`tools/post-execute` 作为通知触发；轮次由
`agent/turn-start` / `agent/turn-end` 夹住。hook 就是普通 fiber——
注册监听或拦截器都是可逆效应，卸载 fiber 即撤销 hook（通知派发用的
是 stc-go 核心的 `On`/`Emit`，拦截用的是 registry 卫星——不需要第三
套派发机制，评估记录见
[stc-go#5](https://github.com/0xdenny218/stc-go/issues/5)）。

system prompt 由 fiber 注册的**段落**组装：按段落名排序、空行拼接。
agent 自带一个身份段，任何 fiber 都可以增删自己的段落——下一个请求
就看到新 prompt。

一轮输入按 `[模型 → 工具]*` 迭代直到模型给出答复（答复逐块流式呈现；
工具调用以 `→ name(args)` 形式打印轨迹），熔断上限 10 次。工具失败
与审批拒绝作为结果文本回灌给模型自我纠正，而不是炸掉整轮；轮次中途
发生重载时，
未应答的 tool_call 会补 aborted 标记，transcript 保持线格式合法。
transcript 是 append-only 事件日志：消息、每次模型调用的 token
用量、审批决定——内存里的历史只是它的一个投影，resume 即重放投影。

## Guest 工具（WASM，对话进行中热替换）

`--tools-dir`（默认 `tools.d`）里的每个 `*.wasm` 都装载为一个工具
fiber：工具名取文件名（`dice.wasm` → `dice`），线格式描述由 guest
自己提供——它在 `start` 里公布 `tool.<name>` 服务。调用以 JSON 字符串
跨边界（`Handle.Call("invoke", args)`）。每个文件都被监听：**原地重建，
下一轮即走新版本——同一进程、同一会话、历史逐字保留。** 进行中的调用
在旧版本上完整跑完后换血才落定；坏构建报错且旧版本继续服役。

guest 工具就是普通的 Go，用 [TinyGo](https://tinygo.org/) 对着
`stc-go/guest` 编译——调用 ABI（`stc_alloc`/`stc_free`/`invoke`）由
SDK 导出，guest 只需一段描述加一个处理器：

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

可运行的示例在 [`examples/guests/dice`](examples/guests/dice)：

```sh
mkdir -p tools.d
tinygo build -target wasip1 -buildmode=c-shared -o tools.d/dice.wasm ./examples/guests/dice
stc-agent &                       # 让它掷一次骰子
tinygo build ... -tags v2 -o tools.d/dice.wasm ./examples/guests/dice
#                                 再问一次——v2 应答，会话原样
```

`scripts/demo-hotswap.sh` 对着真实模型驱动整个场景（构建 v1 → 掷骰 →
原地重建 v2 → 再掷，并断言 transcript 里的版本标记）。同一场景在 CI
里以 `TestE2EHotSwapKeepsSession` 无头运行（脚本化 mock 模型），断言
换血前后工具表逐字节一致、transcript 逐字保留。

一次真实运行实录（2026-08-17，GLM `glm-4.5`，走其 Coding Plan
端点——只改了 `STC_AGENT_BASE_URL`/`STC_AGENT_API_KEY`/
`STC_AGENT_MODEL` 三个环境变量，零代码改动）：

```
==> turn 1: asking the model to roll (dice v1 on disk)
    tool result: {"role":"tool","content":"{\"roll\":3,\"sides\":6,\"version\":\"v1\"}", ...}
==> rebuilding dice.wasm in place as v2 (agent pid 88227 keeps running)
    hot-swap landed; agent still pid 88227
==> turn 2: asking again (v2 now serving)
    tool result: {"role":"tool","content":"{\"roll\":3,\"sides\":6,\"version\":\"v2\"}", ...}
```

注意：启动期
装载失败的 guest 会让启动失败（fiber 状态转储，退出码 1）；重载期失败
的 guest 保留旧版本继续服役。描述只在初次装载时读取——换血换的是
行为，不是已注册的名字/描述。

## Skills（目录即热装载 fiber）

`--skills-dir`（默认 `skills.d`）下每个含 `SKILL.md` 的子目录是一个
skill——也是一个 fiber。文件由可选 frontmatter（`name` /
`description`）加正文组成：正文注册为 system-prompt 段落
（`skill:<name>`，与其余段落按名排序拼接），目录里的每个 `*.wasm`
经与 `--tools-dir` 完全相同的机制（含 hmr）装成该 skill 自己的 guest
工具。supervisor fiber 监听目录：**落盘一个 skill，它的 prompt 段落与
工具下一轮就生效；删掉它，段落与工具即时撤销——对话进行中，不重启。**
原地编辑 `SKILL.md` 热更新段落（registry 的同名覆盖语义）。启动期的
坏 skill 让启动失败；运行期落盘的坏 skill 只上报
（`[skill] <name>: <err>`），不波及其余。

## MCP servers（stdio）

`--mcp "name=command args..."`（可重复，或配置文件里的 `"mcp"` 键）
把每个 MCP stdio server 装成独立 fiber：握手、`tools/list`，然后每个
工具以 `mcp__<server>__<tool>` 注册进 toolset（描述前缀
`[mcp:<server>]`）——agent 循环、审批门、hooks、`/tools` 对它们一视
同仁。**断开 = 工具失效**：server 进程退出时其工具立即注销
（`[mcp] <name> disconnected; N tools removed`），下一个请求的工具表
里自然不再有它们。最小示例 server 在
[`examples/mcp/echo`](examples/mcp/echo)。仅 stdio——MCP over
HTTP/SSE 是不做项。

## Subagent、compaction、todos、plan 模式、后台任务（M9）

**Subagent**（`task` 工具）：模型把一条自包含子任务交给子 agent，子
agent 在子作用域里独立跑轮（stc-go `Context.Child` + `Isolate`，spec
D17）。子域共享模型与审批门，但在全新 realm 里拿到独立会话、工具表
与轮次 runner——它的 provide 不与父域同键冲突，事件日志也不触碰父
会话。子 agent 终答以普通工具消息回流进父 transcript。默认工具子集是
全部去掉 `task`（不许递归）；`{"tools": [...]}` 可收窄。

**Compaction**（`--compact-threshold`）：一轮的 prompt tokens 超过阈值
时，loop 让模型把历史摘要成一条 `compaction` 事件；投影把被摘要段
折叠，下一个请求从摘要开始。摘要自身的 token 用量也入账。

**Todos**（`todo_write` 工具）：当前任务清单是一串 session 事件，渲染
为 system prompt 里的实时段落（registry 同名覆盖即撤销/改写）。清空
清单即摘除段落。

**Plan 模式**（`/plan`、`exit_plan_mode`）：`/plan` 切换一个模式，在
门禁处阻断所有非只读工具——先调研，再调 `exit_plan_mode` 提交计划。
agent 请你批准：yes 关模式并把计划回灌，no 留在模式。每个决定像审批
事件一样落日志。

**后台任务**（`job_start` / `job_list` / `job_kill`）：启动一条 shell
命令或子 agent 并立即返回；完成时以 user 消息进入下一次模型调用，
`job_list` 枚举、`job_kill` 取消。shell 与子 agent 后台工作收敛到同
一条生命周期（`task` 通路）。

## 是什么

- CLI 对话 agent（stdin/stdout），带流式工具调用循环。
- 每个能力都是 fiber：模型客户端、工具、斜杠命令、会话、REPL 本身。
  装配完全由范式机制（`Load`/`Provide`/`Inject`/`Effect`）承载，
  `main` 只负责列出组件清单。
- 差异化演示：**对话进行中热替换 WASM guest 工具——不重启、不丢会话。**
  换模型经"重提供 → 级联重载"生效，而会话历史存活——存活是依赖图
  本身的推论，不需要专门机制。
- stc-agent 是 stc-go 的第一个真实消费者：API 摩擦点与卫星包
  （事件派发、config schema、host→guest 带参调用）由这里的真实需求
  拉出，不靠推测。

## 不做

- 不做 UI/TUI/web；不做 provider 抽象层；不做 sandbox 隔离执行；不做
  MCP over HTTP/SSE；不做 skill 市场/分发。
- 自身不做框架：不做插件分发、profile 组合与二次开发平台。stc-agent
  之于 [stc-go](https://github.com/0xdenny218/stc-go) 正如 dsh 之于
  Cordis——把框架用透到全部要求的那个 agent，框架能力只经回流在上游
  生长。**agent 能力面**以
  [dsh](https://github.com/deepseek-ai/deepseek-harness) 为参照：
  hooks、skills、MCP 已完成（M7–M8）；subagent、compaction、todos、
  plan 模式与后台任务已完成（M9）；工具包与 agent 自创作 guest 工具在
  路线图上（M10）。

## 里程碑

- [x] M0 脚手架（仓库、CI、双语 README、main 骨架）
- [x] M1 最小对话闭环（config/model/session/cli fiber，/model 级联）
- [x] M2 工具系统 + agent 循环（toolset 稳定服务、静态 Go 工具）
- [x] M3 WASM guest 工具 + 热重载（hmr）：对话中途换工具
- [x] M4 发布 + 回流评审（v0.1.0 已上 GitHub；评审产出为 stc-go issues）
- [x] M5 会话脊柱（事件日志）+ 流式 + 终端交互（readline、`-p` headless）
- [x] M6 工具管线 + 审批门（策略 + 途中提问回路，决定落事件日志）
- [x] M7 hooks（通知 + 拦截）+ system-prompt 段落 + `inspect_agent` 自描述
- [x] M8 skills（SKILL.md 目录热装载为 fiber：prompt 段落 + guest 工具）
  + MCP stdio server 工具 fiber（断开 = 工具失效）
- [x] M9 subagent（子作用域）+ compaction + todos + plan 模式 + 后台任务
  （shell 与子 agent，同一条生命周期）
- [ ] M10 工具包 + agent 自创作 guest 工具（评估）

## 开发

标准对齐 stc-go：`gofmt`、`go vet`、`go test -race` 全绿。

```sh
go test -race ./...
```

guest 工具测试会真实构建 guest，找不到 TinyGo 时 Skip（设 `TINYGO`、
放进 `PATH`，或装到 `~/.local/opt/tinygo`）；CI 安装 TinyGo 真实运行。

## 许可证

[MIT](LICENSE)
