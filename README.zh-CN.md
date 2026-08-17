# stc-agent

[English](README.md) | **简体中文**

**万物皆插件的最小 CLI 对话 agent**——基于
[stc-go](https://github.com/0xdenny218/stc-go)（时空可组合性范式的 Go 实现）。

> 状态：**v0.1.0 已发布**——`--tools-dir` 里的每个 `*.wasm` 都是 guest
> 工具 fiber，原地重建即在对话进行中热替换。里程碑全部完成（M0–M4）。

## 安装

```sh
go install github.com/0xdenny218/stc-agent/cmd/stc-agent@latest
```

或克隆后 `go run ./cmd/stc-agent`。运行 agent 只需要 Go；开发 guest
工具另需 [TinyGo](https://tinygo.org/)。

## 用法

```sh
export DEEPSEEK_API_KEY=...   # 或 STC_AGENT_API_KEY / OPENAI_API_KEY
go run ./cmd/stc-agent
```

配置优先级：内置默认 < 配置文件 < 环境变量 < 命令行。

| 参数 | 环境变量 | 默认 |
|---|---|---|
| `--base-url` | `STC_AGENT_BASE_URL` | `https://api.deepseek.com` |
| `--api-key` | `STC_AGENT_API_KEY`，再 `DEEPSEEK_API_KEY`，再 `OPENAI_API_KEY` | —（必填） |
| `--model` | `STC_AGENT_MODEL` | `deepseek-chat` |
| `--timeout` | — | `60s` |
| `--transcript PATH` | — | JSONL transcript；文件已存在则 replay 恢复 |
| `--resume PATH` | — | `--transcript` 的别名 |
| `--tools-dir DIR` | — | `tools.d`；其中每个 `*.wasm` 是一个 guest 工具 |
| `--config PATH` | — | `~/.config/stc-agent/config.json`（存在才读） |

REPL 内命令：

- `/model <name>`——对话中途换模型。config 服务被重提供，模型客户端与
  REPL 反应式重载，而 session fiber（不依赖这两者）逐字保留历史。
- `/tools`——列出已注册工具。
- `/help`——列出命令。
- `/quit`——退出。

工具（每个工具是独立 fiber，注册进稳定的 toolset 服务，因此工具增删
不会重载 agent 循环——toolset 与斜杠命令注册表就是
[`stc-go/registry`](https://github.com/0xdenny218/stc-go/tree/main/registry)，
首个完整回流循环：模式在此重复两处 → 提取到上游 → 本仓库删除自己的
副本）：

- `read_file` / `write_file`——文件读写，输出上限 32 KiB。
- `shell`——`sh -c` 执行，30 秒超时，工作目录固定为启动目录。
  **没有权限审批流：模型可以以你的身份执行任意命令。** 请只在你能接受
  的目录里运行。

一轮输入按 `[模型 → 工具]*` 迭代直到模型给出答复（工具调用以
`→ name(args)` 形式打印轨迹），熔断上限 10 次。工具失败作为结果文本
回灌给模型自我纠正，而不是炸掉整轮；轮次中途发生重载时，未应答的
tool_call 会补 aborted 标记，transcript 保持线格式合法。

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

一次真实运行实录（2026-08-17，GLM `glm-4-flash`，走其 OpenAI 兼容
端点——只改了 `STC_AGENT_BASE_URL`/`STC_AGENT_API_KEY`/
`STC_AGENT_MODEL` 三个环境变量，零代码改动）：

```
==> turn 1: asking the model to roll (dice v1 on disk)
    tool result: {"role":"tool","content":"{\"roll\":3,\"sides\":6,\"version\":\"v1\"}", ...}
==> rebuilding dice.wasm in place as v2 (agent pid 79297 keeps running)
    hot-swap landed; agent still pid 79297
==> turn 2: asking again (v2 now serving)
    tool result: {"role":"tool","content":"{\"roll\":3,\"sides\":6,\"version\":\"v2\"}", ...}
```

注意：启动期
装载失败的 guest 会让启动失败（fiber 状态转储，退出码 1）；重载期失败
的 guest 保留旧版本继续服役。描述只在初次装载时读取——换血换的是
行为，不是已注册的名字/描述。

## 是什么

- CLI 对话 agent（stdin/stdout），带工具调用循环。
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

- 不做 UI/TUI/web；不做框架抽象——模式在 agent 里重复出现后再抽取。
- 不是 [dsh](https://github.com/deepseek-ai/deepseek-harness) 竞品：
  不做流式输出、provider 抽象层、MCP/skills/subagent、权限审批流。

## 里程碑

- [x] M0 脚手架（仓库、CI、双语 README、main 骨架）
- [x] M1 最小对话闭环（config/model/session/cli fiber，/model 级联）
- [x] M2 工具系统 + agent 循环（toolset 稳定服务、静态 Go 工具）
- [x] M3 WASM guest 工具 + 热重载（hmr）：对话中途换工具
- [x] M4 发布 + 回流评审（v0.1.0 已上 GitHub；评审产出为 stc-go issues）

## 开发

标准对齐 stc-go：`gofmt`、`go vet`、`go test -race` 全绿。

```sh
go test -race ./...
```

guest 工具测试会真实构建 guest，找不到 TinyGo 时 Skip（设 `TINYGO`、
放进 `PATH`，或装到 `~/.local/opt/tinygo`）；CI 安装 TinyGo 真实运行。

## 许可证

[MIT](LICENSE)
