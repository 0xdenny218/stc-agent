# stc-agent

[English](README.md) | **简体中文**

**万物皆插件的最小 CLI 对话 agent**——基于
[stc-go](https://github.com/0xdenny218/stc-go)（时空可组合性范式的 Go 实现）。

> 状态：**M2 完成**——agent 循环驱动多轮工具调用，静态 Go 工具 fiber
>（`read_file`、`write_file`、`shell`）已接入。里程碑见下。

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
| `--config PATH` | — | `~/.config/stc-agent/config.json`（存在才读） |

REPL 内命令：

- `/model <name>`——对话中途换模型。config 服务被重提供，模型客户端与
  REPL 反应式重载，而 session fiber（不依赖这两者）逐字保留历史。
- `/tools`——列出已注册工具。
- `/help`——列出命令。
- `/quit`——退出。

工具（每个工具是独立 fiber，注册进稳定的 toolset 服务，因此工具增删
不会重载 agent 循环）：

- `read_file` / `write_file`——文件读写，输出上限 32 KiB。
- `shell`——`sh -c` 执行，30 秒超时，工作目录固定为启动目录。
  **没有权限审批流：模型可以以你的身份执行任意命令。** 请只在你能接受
  的目录里运行。

一轮输入按 `[模型 → 工具]*` 迭代直到模型给出答复（工具调用以
`→ name(args)` 形式打印轨迹），熔断上限 10 次。工具失败作为结果文本
回灌给模型自我纠正，而不是炸掉整轮；轮次中途发生重载时，未应答的
tool_call 会补 aborted 标记，transcript 保持线格式合法。

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
- [ ] M3 WASM guest 工具 + 热重载（hmr）：对话中途换工具
- [ ] M4 发布 + 回流评审（哪些模式回流入 stc-go）

## 开发

标准对齐 stc-go：`gofmt`、`go vet`、`go test -race` 全绿。

```sh
go test -race ./...
```

## 许可证

[MIT](LICENSE)
